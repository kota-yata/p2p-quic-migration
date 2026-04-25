//go:build linux

package network_monitor

import (
	"testing"

	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"golang.org/x/sys/unix"
)

func TestParseNotifyCQMThresholdLow(t *testing.T) {
	cqm, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NL80211_ATTR_CQM_RSSI_THRESHOLD_EVENT, Data: nlenc.Uint32Bytes(unix.NL80211_CQM_RSSI_THRESHOLD_EVENT_LOW)},
		{Type: unix.NL80211_ATTR_CQM_RSSI_LEVEL, Data: nlenc.Int32Bytes(-78)},
	})
	if err != nil {
		t.Fatalf("failed to marshal nested attrs: %v", err)
	}
	data, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NL80211_ATTR_IFINDEX, Data: nlenc.Uint32Bytes(7)},
		{Type: unix.NL80211_ATTR_CQM, Data: cqm},
	})
	if err != nil {
		t.Fatalf("failed to marshal attrs: %v", err)
	}

	ifindex, eventType, rssiLevel, beaconLoss, err := parseNotifyCQM(data)
	if err != nil {
		t.Fatalf("parseNotifyCQM failed: %v", err)
	}
	if ifindex != 7 {
		t.Fatalf("unexpected ifindex: %d", ifindex)
	}
	if eventType != unix.NL80211_CQM_RSSI_THRESHOLD_EVENT_LOW {
		t.Fatalf("unexpected threshold event: %d", eventType)
	}
	if rssiLevel != -78 {
		t.Fatalf("unexpected rssi level: %d", rssiLevel)
	}
	if beaconLoss {
		t.Fatal("unexpected beacon loss")
	}
}

func TestParseNotifyCQMBeaconLoss(t *testing.T) {
	cqm, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NL80211_ATTR_CQM_BEACON_LOSS_EVENT, Data: []byte{}},
	})
	if err != nil {
		t.Fatalf("failed to marshal nested attrs: %v", err)
	}
	data, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NL80211_ATTR_IFINDEX, Data: nlenc.Uint32Bytes(3)},
		{Type: unix.NL80211_ATTR_CQM, Data: cqm},
	})
	if err != nil {
		t.Fatalf("failed to marshal attrs: %v", err)
	}

	ifindex, eventType, rssiLevel, beaconLoss, err := parseNotifyCQM(data)
	if err != nil {
		t.Fatalf("parseNotifyCQM failed: %v", err)
	}
	if ifindex != 3 {
		t.Fatalf("unexpected ifindex: %d", ifindex)
	}
	if eventType != 0 {
		t.Fatalf("unexpected threshold event: %d", eventType)
	}
	if rssiLevel != 0 {
		t.Fatalf("unexpected rssi level: %d", rssiLevel)
	}
	if !beaconLoss {
		t.Fatal("expected beacon loss")
	}
}
