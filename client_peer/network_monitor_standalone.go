//go:build standalone

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.Println("Starting network monitor test...")

	// Create a network monitor with a simple callback
	monitor := NewNetworkMonitor(func(oldAddr, newAddr string) {
		log.Printf("*** NETWORK CHANGE DETECTED ***")
		log.Printf("Old address: %s", oldAddr)
		log.Printf("New address: %s", newAddr)
		log.Printf("*** END NETWORK CHANGE ***")
	})

	// Start monitoring
	if err := monitor.Start(); err != nil {
		log.Fatalf("Failed to start network monitor: %v", err)
	}
	defer monitor.Stop()

	log.Println("Network monitor is running. Turn WiFi on/off to test address change detection.")
	log.Println("Press Ctrl+C to stop...")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Stopping network monitor...")
}