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
	if _, ok := qm.evaluate(testSnapshot(-76, base.Add(500*time.Millisecond))); ok {
		t.Fatal("unexpected event after first weak sample")
	}
	evt, ok := qm.evaluate(testSnapshot(-77, base.Add(time.Second)))
	if !ok {
		t.Fatal("expected degraded event")
	}
	if evt.Type != QualityEventLinkDegraded {
		t.Fatalf("expected degraded event, got %v", evt.Type)
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
	if evt.Type != QualityEventLinkDegraded {
		t.Fatalf("expected degraded event, got %v", evt.Type)
	}
	if evt.Reason != "rapid signal drop" {
		t.Fatalf("expected rapid drop reason, got %q", evt.Reason)
	}
}

func TestQualityMonitorRecoversAfterStableSignal(t *testing.T) {
	qm := testQualityMonitor()
	base := time.Now()

	qm.evaluate(testSnapshot(-80, base))
	if _, ok := qm.evaluate(testSnapshot(-80, base.Add(time.Second))); !ok {
		t.Fatal("expected degraded event")
	}
	if _, ok := qm.evaluate(testSnapshot(-65, base.Add(2*time.Second))); ok {
		t.Fatal("unexpected recovery before stability duration")
	}
	evt, ok := qm.evaluate(testSnapshot(-64, base.Add(4*time.Second)))
	if !ok {
		t.Fatal("expected recovery event")
	}
	if evt.Type != QualityEventLinkRecovered {
		t.Fatalf("expected recovery event, got %v", evt.Type)
	}
}

func TestQualityMonitorSuppressesDegradedDuringCooldown(t *testing.T) {
	qm := testQualityMonitor()
	base := time.Now()

	qm.evaluate(testSnapshot(-80, base))
	if _, ok := qm.evaluate(testSnapshot(-80, base.Add(time.Second))); !ok {
		t.Fatal("expected initial degraded event")
	}
	qm.degraded = false
	qm.degradedCount = 0

	qm.evaluate(testSnapshot(-80, base.Add(2*time.Second)))
	if _, ok := qm.evaluate(testSnapshot(-80, base.Add(3*time.Second))); ok {
		t.Fatal("unexpected degraded event during cooldown")
	}
}

func testQualityMonitor() *QualityMonitor {
	return NewQualityMonitorWithProvider(nil, nil, QualityMonitorConfig{
		SampleInterval:   time.Second,
		DegradedRSSI:     -75,
		RecoveredRSSI:    -67,
		RapidDropWindow:  5 * time.Second,
		RapidDropDB:      10,
		RecoveryDuration: 2 * time.Second,
		SwitchCooldown:   10 * time.Second,
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
