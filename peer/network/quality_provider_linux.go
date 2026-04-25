//go:build linux

package network_monitor

import (
	"fmt"
	"net"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"github.com/mdlayher/wifi"
	"golang.org/x/sys/unix"
)

const (
	defaultCQMDegradedRSSI = -72
	defaultCQMRSSIHyst     = 4
)

type linuxCQMProvider struct {
	conn          *genetlink.Conn
	wifiClient    *wifi.Client
	family        genetlink.Family
	iface         *wifi.Interface
	cqmThreshold  int32
	cqmHysteresis uint32
}

func NewDefaultQualityProvider() QualityProvider {
	return &linuxCQMProvider{
		cqmThreshold:  defaultCQMDegradedRSSI,
		cqmHysteresis: defaultCQMRSSIHyst,
	}
}

func (p *linuxCQMProvider) Start() error {
	conn, err := genetlink.Dial(nil)
	if err != nil {
		return fmt.Errorf("failed to dial generic netlink: %w", err)
	}
	for _, o := range []netlink.ConnOption{
		netlink.ExtendedAcknowledge,
		netlink.GetStrictCheck,
	} {
		_ = conn.SetOption(o, true)
	}

	family, err := conn.GetFamily(unix.NL80211_GENL_NAME)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to resolve nl80211 family: %w", err)
	}

	groupID, ok := multicastGroupID(family, unix.NL80211_MULTICAST_GROUP_MLME)
	if !ok {
		_ = conn.Close()
		return fmt.Errorf("nl80211 multicast group %q unavailable", unix.NL80211_MULTICAST_GROUP_MLME)
	}
	if err := conn.JoinGroup(groupID); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to join nl80211 multicast group %q: %w", unix.NL80211_MULTICAST_GROUP_MLME, err)
	}

	wc, err := wifi.New()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to create wifi client: %w", err)
	}

	ifi, err := activeStationInterface(wc)
	if err != nil {
		_ = wc.Close()
		_ = conn.Close()
		return err
	}

	if err := setCQMThreshold(conn, family, ifi, p.cqmThreshold, p.cqmHysteresis); err != nil {
		_ = wc.Close()
		_ = conn.Close()
		return err
	}

	p.conn = conn
	p.wifiClient = wc
	p.family = family
	p.iface = ifi
	return nil
}

func (p *linuxCQMProvider) Close() error {
	if p.wifiClient != nil {
		_ = p.wifiClient.Close()
		p.wifiClient = nil
	}
	if p.conn != nil {
		err := p.conn.Close()
		p.conn = nil
		return err
	}
	return nil
}

func (p *linuxCQMProvider) ReceiveEvent() (QualityEvent, error) {
	if p.conn == nil || p.iface == nil {
		return QualityEvent{}, fmt.Errorf("provider not started")
	}

	for {
		msgs, _, err := p.conn.Receive()
		if err != nil {
			return QualityEvent{}, err
		}
		for _, msg := range msgs {
			if msg.Header.Command != unix.NL80211_CMD_NOTIFY_CQM {
				continue
			}
			evt, ok, err := p.parseNotifyCQM(msg)
			if err != nil {
				return QualityEvent{}, err
			}
			if ok {
				return evt, nil
			}
		}
	}
}

func (p *linuxCQMProvider) Snapshot() (QualitySnapshot, error) {
	if p.iface == nil || p.wifiClient == nil {
		return QualitySnapshot{}, fmt.Errorf("provider not started")
	}

	iface, err := net.InterfaceByIndex(p.iface.Index)
	if err != nil {
		return QualitySnapshot{}, fmt.Errorf("failed to resolve net.Interface for %s: %w", p.iface.Name, err)
	}
	ip := firstIPv4(*iface)

	stations, err := p.wifiClient.StationInfo(p.iface)
	if err != nil {
		return QualitySnapshot{
			Interface: p.iface.Name,
			IP:        ip,
			Available: ip != nil,
			Time:      time.Now(),
		}, fmt.Errorf("failed to get station info for %s: %w", p.iface.Name, err)
	}

	snap := QualitySnapshot{
		Interface: p.iface.Name,
		IP:        ip,
		Available: ip != nil,
		Time:      time.Now(),
	}
	if len(stations) > 0 {
		snap.RSSIDBm = stations[0].SignalAverage
		if snap.RSSIDBm == 0 {
			snap.RSSIDBm = stations[0].Signal
		}
	}
	return snap, nil
}

func (p *linuxCQMProvider) parseNotifyCQM(msg genetlink.Message) (QualityEvent, bool, error) {
	if p.iface == nil {
		return QualityEvent{}, false, nil
	}

	ifindex, eventType, rssiLevel, beaconLoss, err := parseNotifyCQM(msg.Data)
	if err != nil {
		return QualityEvent{}, false, err
	}
	if ifindex != p.iface.Index {
		return QualityEvent{}, false, nil
	}

	snap, snapErr := p.Snapshot()
	if snapErr != nil {
		snap = QualitySnapshot{
			Interface: p.iface.Name,
			Available: true,
			Time:      time.Now(),
		}
	}
	if rssiLevel != 0 {
		snap.RSSIDBm = rssiLevel
	}

	switch {
	case beaconLoss:
		return QualityEvent{
			Type:    QualityEventLinkDegraded,
			Current: snap,
			Reason:  "nl80211 CQM beacon loss",
		}, true, nil
	case eventType == unix.NL80211_CQM_RSSI_THRESHOLD_EVENT_LOW:
		return QualityEvent{
			Type:    QualityEventLinkDegraded,
			Current: snap,
			Reason:  "nl80211 CQM RSSI low",
		}, true, nil
	case eventType == unix.NL80211_CQM_RSSI_THRESHOLD_EVENT_HIGH:
		return QualityEvent{
			Type:    QualityEventLinkRecovered,
			Current: snap,
			Reason:  "nl80211 CQM RSSI high",
		}, true, nil
	default:
		return QualityEvent{}, false, nil
	}
}

func activeStationInterface(c *wifi.Client) (*wifi.Interface, error) {
	ifis, err := c.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to list wifi interfaces: %w", err)
	}

	for _, ifi := range ifis {
		if ifi.Type != wifi.InterfaceTypeStation {
			continue
		}
		nif, err := net.InterfaceByIndex(ifi.Index)
		if err != nil {
			continue
		}
		if (nif.Flags&net.FlagUp) == 0 || (nif.Flags&net.FlagLoopback) != 0 {
			continue
		}
		if ip := firstIPv4(*nif); ip == nil {
			continue
		}
		return ifi, nil
	}

	return nil, fmt.Errorf("no active station-mode Wi-Fi interface found")
}

func setCQMThreshold(conn *genetlink.Conn, family genetlink.Family, ifi *wifi.Interface, threshold int32, hysteresis uint32) error {
	cqmAttrs, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NL80211_ATTR_CQM_RSSI_THOLD, Data: nlenc.Int32Bytes(threshold)},
		{Type: unix.NL80211_ATTR_CQM_RSSI_HYST, Data: nlenc.Uint32Bytes(hysteresis)},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal CQM attributes: %w", err)
	}

	attrs, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NL80211_ATTR_IFINDEX, Data: nlenc.Uint32Bytes(uint32(ifi.Index))},
		{Type: unix.NL80211_ATTR_CQM, Data: cqmAttrs},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal nl80211 attributes: %w", err)
	}

	req := genetlink.Message{
		Header: genetlink.Header{
			Command: unix.NL80211_CMD_SET_CQM,
			Version: family.Version,
		},
		Data: attrs,
	}
	if _, err := conn.Execute(req, family.ID, netlink.Request|netlink.Acknowledge); err != nil {
		return fmt.Errorf("failed to configure nl80211 CQM for %s threshold=%ddBm hysteresis=%d: %w",
			ifi.Name, threshold, hysteresis, err)
	}
	return nil
}

func multicastGroupID(family genetlink.Family, name string) (uint32, bool) {
	for _, g := range family.Groups {
		if g.Name == name {
			return g.ID, true
		}
	}
	return 0, false
}

func parseNotifyCQM(data []byte) (ifindex int, thresholdEvent uint32, rssiLevel int, beaconLoss bool, err error) {
	ad, err := netlink.NewAttributeDecoder(data)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("failed to decode nl80211 attrs: %w", err)
	}

	for ad.Next() {
		switch ad.Type() {
		case unix.NL80211_ATTR_IFINDEX:
			ifindex = int(ad.Uint32())
		case unix.NL80211_ATTR_CQM:
			nested, nerr := netlink.NewAttributeDecoder(ad.Bytes())
			if nerr != nil {
				return 0, 0, 0, false, fmt.Errorf("failed to decode CQM attrs: %w", nerr)
			}
			for nested.Next() {
				switch nested.Type() {
				case unix.NL80211_ATTR_CQM_RSSI_THRESHOLD_EVENT:
					thresholdEvent = nested.Uint32()
				case unix.NL80211_ATTR_CQM_RSSI_LEVEL:
					rssiLevel = int(int32(nlenc.Uint32(nested.Bytes())))
				case unix.NL80211_ATTR_CQM_BEACON_LOSS_EVENT:
					beaconLoss = true
				}
			}
			if err := nested.Err(); err != nil {
				return 0, 0, 0, false, fmt.Errorf("failed to parse nested CQM attrs: %w", err)
			}
		}
	}
	if err := ad.Err(); err != nil {
		return 0, 0, 0, false, fmt.Errorf("failed to parse nl80211 attrs: %w", err)
	}
	return ifindex, thresholdEvent, rssiLevel, beaconLoss, nil
}
