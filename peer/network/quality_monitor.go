package network_monitor

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

const (
	defaultQualitySampleInterval = 500 * time.Millisecond
	defaultDegradedRSSI          = -75
	defaultRecoveredRSSI         = -67
	defaultRapidDropWindow       = 5 * time.Second
	defaultRapidDropDB           = 10
	defaultRecoveryDuration      = 5 * time.Second
	defaultSwitchCooldown        = 10 * time.Second
)

type QualityEventType int

const (
	QualityEventLinkDegraded QualityEventType = iota + 1
	QualityEventLinkRecovered
)

type QualitySnapshot struct {
	Interface string
	IP        net.IP
	RSSIDBm   int
	Link      int
	Available bool
	Time      time.Time
}

type QualityEvent struct {
	Type     QualityEventType
	Current  QualitySnapshot
	Previous QualitySnapshot
	Reason   string
}

type QualityProvider interface {
	Snapshot() (QualitySnapshot, error)
}

type QualityEventSource interface {
	QualityProvider
	Start() error
	Close() error
	ReceiveEvent() (QualityEvent, error)
}

type QualityMonitorConfig struct {
	SampleInterval   time.Duration
	DegradedRSSI     int
	RecoveredRSSI    int
	RapidDropWindow  time.Duration
	RapidDropDB      int
	RecoveryDuration time.Duration
	SwitchCooldown   time.Duration
}

type QualityMonitor struct {
	provider QualityProvider
	onEvent  func(QualityEvent)
	cfg      QualityMonitorConfig

	stopChan chan struct{}
	doneChan chan struct{}
	started  bool

	mu              sync.Mutex
	lastSnapshot    QualitySnapshot
	degraded        bool
	degradedCount   int
	recoveredSince  time.Time
	lastSwitchEvent time.Time
}

func DefaultQualityMonitorConfig() QualityMonitorConfig {
	return QualityMonitorConfig{
		SampleInterval:   defaultQualitySampleInterval,
		DegradedRSSI:     defaultDegradedRSSI,
		RecoveredRSSI:    defaultRecoveredRSSI,
		RapidDropWindow:  defaultRapidDropWindow,
		RapidDropDB:      defaultRapidDropDB,
		RecoveryDuration: defaultRecoveryDuration,
		SwitchCooldown:   defaultSwitchCooldown,
	}
}

func NewQualityMonitor(onEvent func(QualityEvent)) *QualityMonitor {
	return NewQualityMonitorWithProvider(NewDefaultQualityProvider(), onEvent, DefaultQualityMonitorConfig())
}

func NewQualityMonitorWithProvider(provider QualityProvider, onEvent func(QualityEvent), cfg QualityMonitorConfig) *QualityMonitor {
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = defaultQualitySampleInterval
	}
	if cfg.RapidDropWindow <= 0 {
		cfg.RapidDropWindow = defaultRapidDropWindow
	}
	if cfg.RecoveryDuration <= 0 {
		cfg.RecoveryDuration = defaultRecoveryDuration
	}
	if cfg.SwitchCooldown <= 0 {
		cfg.SwitchCooldown = defaultSwitchCooldown
	}
	return &QualityMonitor{
		provider: provider,
		onEvent:  onEvent,
		cfg:      cfg,
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

func (qm *QualityMonitor) Start() error {
	if qm.provider == nil {
		return fmt.Errorf("no quality provider configured")
	}
	if source, ok := qm.provider.(QualityEventSource); ok {
		if err := source.Start(); err != nil {
			return err
		}
	}
	initial, err := qm.provider.Snapshot()
	if err != nil {
		if source, ok := qm.provider.(QualityEventSource); ok {
			_ = source.Close()
		}
		return fmt.Errorf("failed to get initial quality snapshot: %w", err)
	}
	qm.mu.Lock()
	qm.lastSnapshot = initial
	qm.started = true
	qm.mu.Unlock()

	log.Printf("Quality monitor started on %s ip=%s rssi=%ddBm link=%d available=%t",
		initial.Interface, initial.IP, initial.RSSIDBm, initial.Link, initial.Available)

	if source, ok := qm.provider.(QualityEventSource); ok {
		go qm.monitorEventLoop(source)
		return nil
	}

	go qm.monitorLoop()
	return nil
}

func (qm *QualityMonitor) Stop() {
	qm.mu.Lock()
	started := qm.started
	qm.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-qm.stopChan:
	default:
		close(qm.stopChan)
	}
	if source, ok := qm.provider.(QualityEventSource); ok {
		_ = source.Close()
	}
	<-qm.doneChan
}

func (qm *QualityMonitor) monitorLoop() {
	defer close(qm.doneChan)

	ticker := time.NewTicker(qm.cfg.SampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			snap, err := qm.provider.Snapshot()
			if err != nil {
				log.Printf("Quality monitor snapshot failed: %v", err)
				continue
			}
			if evt, ok := qm.evaluate(snap); ok && qm.onEvent != nil {
				qm.onEvent(evt)
			}
		case <-qm.stopChan:
			return
		}
	}
}

func (qm *QualityMonitor) monitorEventLoop(source QualityEventSource) {
	defer close(qm.doneChan)

	for {
		select {
		case <-qm.stopChan:
			return
		default:
		}

		evt, err := source.ReceiveEvent()
		if err != nil {
			select {
			case <-qm.stopChan:
				return
			default:
				log.Printf("Quality monitor event receive failed: %v", err)
				time.Sleep(250 * time.Millisecond)
				continue
			}
		}
		if qm.onEvent != nil {
			qm.onEvent(evt)
		}
	}
}

func (qm *QualityMonitor) evaluate(snap QualitySnapshot) (QualityEvent, bool) {
	now := snap.Time
	if now.IsZero() {
		now = time.Now()
		snap.Time = now
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	prev := qm.lastSnapshot
	qm.lastSnapshot = snap

	if !snap.Available {
		return QualityEvent{}, false
	}

	rapidDrop := prev.Available &&
		!prev.Time.IsZero() &&
		now.Sub(prev.Time) <= qm.cfg.RapidDropWindow &&
		prev.RSSIDBm-snap.RSSIDBm >= qm.cfg.RapidDropDB

	weakSignal := snap.RSSIDBm <= qm.cfg.DegradedRSSI
	if weakSignal {
		qm.degradedCount++
	} else {
		qm.degradedCount = 0
	}

	if !qm.degraded && (qm.degradedCount >= 2 || rapidDrop) {
		if !qm.lastSwitchEvent.IsZero() && now.Sub(qm.lastSwitchEvent) < qm.cfg.SwitchCooldown {
			return QualityEvent{}, false
		}
		qm.degraded = true
		qm.lastSwitchEvent = now
		reason := "weak signal"
		if rapidDrop {
			reason = "rapid signal drop"
		}
		return QualityEvent{Type: QualityEventLinkDegraded, Current: snap, Previous: prev, Reason: reason}, true
	}

	if qm.degraded && snap.RSSIDBm >= qm.cfg.RecoveredRSSI {
		if qm.recoveredSince.IsZero() {
			qm.recoveredSince = now
			return QualityEvent{}, false
		}
		if now.Sub(qm.recoveredSince) >= qm.cfg.RecoveryDuration {
			qm.degraded = false
			qm.degradedCount = 0
			qm.recoveredSince = time.Time{}
			return QualityEvent{Type: QualityEventLinkRecovered, Current: snap, Previous: prev, Reason: "signal recovered"}, true
		}
		return QualityEvent{}, false
	}

	if snap.RSSIDBm < qm.cfg.RecoveredRSSI {
		qm.recoveredSince = time.Time{}
	}
	return QualityEvent{}, false
}
