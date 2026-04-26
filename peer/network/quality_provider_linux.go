//go:build linux

package network_monitor

import (
	"fmt"
	"net"
	"time"

	"github.com/mdlayher/wifi"
)

type linuxRSSIProvider struct {
	wifiClient *wifi.Client
	iface      *wifi.Interface
}

func NewDefaultQualityProvider() QualityProvider {
	return &linuxRSSIProvider{}
}

func (p *linuxRSSIProvider) Snapshot() (QualitySnapshot, error) {
	if p.wifiClient == nil || p.iface == nil {
		wc, err := wifi.New()
		if err != nil {
			return QualitySnapshot{}, fmt.Errorf("failed to create wifi client: %w", err)
		}

		ifi, err := activeStationInterface(wc)
		if err != nil {
			_ = wc.Close()
			return QualitySnapshot{}, err
		}

		p.wifiClient = wc
		p.iface = ifi
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

func (p *linuxRSSIProvider) Close() error {
	if p.wifiClient != nil {
		err := p.wifiClient.Close()
		p.wifiClient = nil
		p.iface = nil
		return err
	}
	return nil
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
