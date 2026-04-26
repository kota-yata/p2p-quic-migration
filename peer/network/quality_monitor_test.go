package network_monitor

import (
	"net"
	"testing"
	"time"
)

func TestQualityMonitorDetectsWeakSignal(t *testing.T) {
	qm := testQualityMonitor()
	base := time.Now()

	if _, ok := qm.evaluate(testSnapshot(-74, base)); ok {
		t.Fatal("unexpected event before degraded threshold")
	}
	evt, ok := qm.evaluate(testSnapshot(-76, base.Add(500*time.Millisecond)))
	if !ok {
		t.Fatal("expected degraded event")
	}
	if evt.Reason != "weak signal" {
		t.Fatalf("expected weak signal reason, got %q", evt.Reason)
	}
}

func TestQualityMonitorDetectsRapidDrop(t *testing.T) {
	qm := testQualityMonitor()
	base := time.Now()

	if _, ok := qm.evaluate(testSnapshot(-55, base)); ok {
		t.Fatal("unexpected event for baseline")
	}
	evt, ok := qm.evaluate(testSnapshot(-66, base.Add(time.Second)))
	if !ok {
		t.Fatal("expected rapid-drop degraded event")
	}
	if evt.Reason != "rapid signal drop" {
		t.Fatalf("expected rapid drop reason, got %q", evt.Reason)
	}
}

func testQualityMonitor() *QualityMonitor {
	return NewQualityMonitorWithProvider(nil, nil, QualityMonitorConfig{
		SampleInterval:  time.Second,
		DegradedRSSI:    -75,
		RapidDropWindow: 5 * time.Second,
		RapidDropDB:     10,
	})
}

func testSnapshot(rssi int, at time.Time) QualitySnapshot {
	return QualitySnapshot{
		Interface: "wlan0",
		IP:        net.IPv4(192, 0, 2, 1),
		RSSIDBm:   rssi,
		Link:      50,
		Available: true,
		Time:      at,
	}
}
