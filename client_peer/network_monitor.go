package main

import (
	"fmt"
	"log"
	"net"
	"time"
)

type NetworkMonitor struct {
	currentAddress string
	onChange       func(oldAddr, newAddr string)
	stopChan       chan bool
}

func NewNetworkMonitor(onChange func(oldAddr, newAddr string)) *NetworkMonitor {
	return &NetworkMonitor{
		onChange: onChange,
		stopChan: make(chan bool),
	}
}

func (nm *NetworkMonitor) Start() error {
	initialAddr, err := nm.getCurrentAddress()
	if err != nil {
		return fmt.Errorf("failed to get initial address: %v", err)
	}
	
	nm.currentAddress = initialAddr
	log.Printf("Network monitor started with initial address: %s", initialAddr)
	
	go nm.monitorLoop()
	return nil
}

func (nm *NetworkMonitor) Stop() {
	close(nm.stopChan)
}

func (nm *NetworkMonitor) getCurrentAddress() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
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
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			
			// Return the first non-loopback IPv4 address
			if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
				return ip.String(), nil
			}
		}
	}
	
	return "", fmt.Errorf("no suitable network interface found")
}

func (nm *NetworkMonitor) monitorLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-nm.stopChan:
			log.Println("Network monitor stopped")
			return
		case <-ticker.C:
			newAddr, err := nm.getCurrentAddress()
			if err != nil {
				log.Printf("Failed to get current address: %v", err)
				continue
			}
			
			if newAddr != nm.currentAddress {
				log.Printf("Network change detected: %s -> %s", nm.currentAddress, newAddr)
				oldAddr := nm.currentAddress
				nm.currentAddress = newAddr
				
				if nm.onChange != nil {
					nm.onChange(oldAddr, newAddr)
				}
			}
		}
	}
}