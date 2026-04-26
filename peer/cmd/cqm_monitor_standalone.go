//go:build cqm_monitor_standalone

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
)

func main() {
	log.Println("Starting nl80211 CQM monitor test...")

	monitor := network_monitor.NewQualityMonitor(func(evt network_monitor.QualityEvent) {
		log.Printf("*** CQM EVENT DETECTED ***")
		log.Printf("Type: %v", evt.Type)
		log.Printf("Interface: %s", evt.Current.Interface)
		log.Printf("IP: %s", evt.Current.IP)
		log.Printf("RSSI: %d dBm", evt.Current.RSSIDBm)
		log.Printf("Reason: %s", evt.Reason)
		log.Printf("*** END CQM EVENT ***")
	})

	if err := monitor.Start(); err != nil {
		log.Fatalf("Failed to start CQM monitor: %v", err)
	}
	defer monitor.Stop()

	log.Println("CQM monitor is running. Wait for nl80211 notify-cqm events.")
	log.Println("Press Ctrl+C to stop...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Stopping CQM monitor...")
}
