package main

import (
	"context"
	"log"
	"net"
	"time"
)

type NetworkMonitor struct {
	lastAddr      string
	changeHandler func(oldAddr, newAddr string)
	pollInterval  time.Duration
}

func NewNetworkMonitor(changeHandler func(oldAddr, newAddr string)) *NetworkMonitor {
	return &NetworkMonitor{
		changeHandler: changeHandler,
		pollInterval:  3 * time.Second,
	}
}

func (nm *NetworkMonitor) Start(ctx context.Context) {
	log.Println("Starting network monitor")
	
	// Get initial address
	nm.lastAddr = nm.getCurrentAddress()
	log.Printf("Initial network address: %s", nm.lastAddr)

	ticker := time.NewTicker(nm.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			currentAddr := nm.getCurrentAddress()
			if currentAddr != nm.lastAddr && currentAddr != "" {
				log.Printf("Network change detected: %s -> %s", nm.lastAddr, currentAddr)
				if nm.changeHandler != nil {
					nm.changeHandler(nm.lastAddr, currentAddr)
				}
				nm.lastAddr = currentAddr
			}
		case <-ctx.Done():
			log.Println("Network monitor stopped")
			return
		}
	}
}

func (nm *NetworkMonitor) getCurrentAddress() string {
	// Get all network interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Printf("Failed to get network interfaces: %v", err)
		return ""
	}

	for _, iface := range interfaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			// Skip loopback addresses
			if ipnet.IP.IsLoopback() {
				continue
			}

			// Prefer IPv4 addresses
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}

	return ""
}

func (nm *NetworkMonitor) GetCurrentAddress() string {
	return nm.getCurrentAddress()
}

func (nm *NetworkMonitor) Stop() {
	// Stop is handled by context cancellation in Start()
}