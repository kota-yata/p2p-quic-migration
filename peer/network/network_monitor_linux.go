//go:build linux || android

package network_monitor

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/vishvananda/netlink"
)

type NetworkMonitor struct {
	currentAddress net.IP
	onChange       func(oldAddr, newAddr net.IP)
	updateChan     chan netlink.AddrUpdate
	stopChan       chan struct{}
}

func NewNetworkMonitor(onChange func(oldAddr, newAddr net.IP)) *NetworkMonitor {
	return &NetworkMonitor{
		onChange:   onChange,
		stopChan:   make(chan struct{}),
		updateChan: make(chan netlink.AddrUpdate),
	}
}

func (nm *NetworkMonitor) Start() error {
	log.Printf("Netlink monitor initiated")
	initialAddr, err := GetCurrentAddress()
	if err != nil {
		return fmt.Errorf("failed to get initial address: %w", err)
	}
	nm.currentAddress = initialAddr
	log.Printf("Monitor started with initial address: %s", initialAddr.String())

	// Subscribe to netlink address updates
	if err := netlink.AddrSubscribe(nm.updateChan, nm.stopChan); err != nil {
		return fmt.Errorf("failed to subscribe to netlink updates: %w", err)
	}

	go nm.monitorLoop()
	return nil
}

func (nm *NetworkMonitor) Stop() {
	select {
	case <-nm.stopChan:
		// already closed
	default:
		close(nm.stopChan)
	}
}

func (nm *NetworkMonitor) monitorLoop() {
	defer log.Println("Network monitor goroutine stopped.")

	for {
		select {
		case <-nm.updateChan:
			newAddr, err := GetCurrentAddress()
			if err != nil {
				continue
			}
			if !newAddr.Equal(nm.currentAddress) {
				oldAddr := nm.currentAddress
				nm.currentAddress = newAddr
				if nm.onChange != nil {
					time.Sleep(200 * time.Millisecond)
					nm.onChange(oldAddr, newAddr)
				}
			}
		case <-nm.stopChan:
			return
		}
	}
}
