package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

// NetworkMonitor is responsible for detecting IP address changes using Netlink.
type NetworkMonitor struct {
	currentAddress string
	onChange       func(oldAddr, newAddr string)
	updateChan     chan netlink.AddrUpdate
	stopChan       chan struct{}
}

// NewNetworkMonitor creates a new monitor instance.
func NewNetworkMonitor(onChange func(oldAddr, newAddr string)) *NetworkMonitor {
	return &NetworkMonitor{
		onChange:   onChange,
		stopChan:   make(chan struct{}),
		updateChan: make(chan netlink.AddrUpdate),
	}
}

// Start subscribes to Netlink events and runs the monitoring loop.
func (nm *NetworkMonitor) Start() error {
	// 1. Get initial address
	initialAddr, err := nm.getCurrentAddress()
	if err != nil {
		return fmt.Errorf("failed to get initial address: %w", err)
	}
	nm.currentAddress = initialAddr
	log.Printf("✅ Monitor started. Initial Address: %s", initialAddr)

	// 2. Subscribe to Netlink Address updates
	// Netlink.AddrSubscribe internally creates and manages the AF_NETLINK socket.
	// We use the stopChan to signal the netlink subscription to stop when we exit.
	if err := netlink.AddrSubscribe(nm.updateChan, nm.stopChan); err != nil {
		return fmt.Errorf("failed to subscribe to Netlink updates: %w", err)
	}

	go nm.monitorLoop()
	return nil
}

// Stop closes the stop channel to signal all goroutines to exit.
func (nm *NetworkMonitor) Stop() {
	log.Println("🛑 Stopping monitor...")
	close(nm.stopChan)
}

// getCurrentAddress scans all interfaces for the first non-loopback IPv4 address.
func (nm *NetworkMonitor) getCurrentAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}

		// Look for a non-loopback IPv4 address
		if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			return ip.String(), nil
		}
	}

	return "<No-Address>", nil // Return a placeholder if no suitable address is found
}

// monitorLoop processes updates received from the Netlink socket.
func (nm *NetworkMonitor) monitorLoop() {
	defer log.Println("🔌 Network monitor goroutine stopped.")

	for {
		select {
		case _ = <-nm.updateChan:
			// A Netlink event occurred, check the current state
			newAddr, err := nm.getCurrentAddress()
			if err != nil {
				log.Printf("⚠️ Error getting current address after update: %v", err)
				continue
			}

			// Check if the perceived address has actually changed
			if newAddr != nm.currentAddress {
				oldAddr := nm.currentAddress
				nm.currentAddress = newAddr

				if nm.onChange != nil {
					nm.onChange(oldAddr, newAddr)
				}
			}

		case <-nm.stopChan:
			return // Exit the loop when stop is signaled
		}
	}
}

// main function to run the executable.
func main() {
	// Setup logging
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// Create the monitor with a logging callback
	monitor := NewNetworkMonitor(func(oldAddr, newAddr string) {
		log.Printf("🚨 ADDRESS CHANGE DETECTED: %s -> %s", oldAddr, newAddr)
	})

	// Start the monitor
	if err := monitor.Start(); err != nil {
		log.Fatalf("Fatal error during monitor startup: %v", err)
	}

	// Wait for an interrupt signal (e.g., Ctrl+C or kill)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	log.Println("Monitor running. Press Ctrl+C or send SIGTERM to exit.")
	<-sigChan // Blocks until a signal is received

	// Cleanly stop the monitor
	monitor.Stop()
	// Give a moment for goroutine to exit
	time.Sleep(100 * time.Millisecond)
	log.Println("Application shutdown complete.")
}
