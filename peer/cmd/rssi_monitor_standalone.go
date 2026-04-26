//go:build rssi_monitor_standalone

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
)

func main() {
	log.Println("Starting RSSI polling monitor test...")

	monitor := network_monitor.NewQualityMonitor(func(evt network_monitor.QualityEvent) {
		log.Printf("*** RSSI EVENT DETECTED ***")
		log.Printf("Type: %v", evt.Type)
		log.Printf("Interface: %s", evt.Current.Interface)
		log.Printf("IP: %s", evt.Current.IP)
		log.Printf("RSSI: %d dBm", evt.Current.RSSIDBm)
		log.Printf("Reason: %s", evt.Reason)
		log.Printf("*** END RSSI EVENT ***")
	})

	if err := monitor.Start(); err != nil {
		log.Fatalf("Failed to start RSSI monitor: %v", err)
	}
	defer monitor.Stop()

	log.Println("RSSI monitor is running with 200ms polling.")
	log.Println("Press Ctrl+C to stop...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Stopping RSSI monitor...")
}
