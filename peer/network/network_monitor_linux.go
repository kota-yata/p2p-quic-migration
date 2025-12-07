//go:build linux || android
// +build linux android

package network_monitor

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/vishvananda/netlink"
)

type NetworkMonitor struct {
	currentAddress string
	onChange       func(oldAddr, newAddr string)
	updateChan     chan netlink.AddrUpdate
	stopChan       chan struct{}
}

func NewNetworkMonitor(onChange func(oldAddr, newAddr string)) *NetworkMonitor {
	return &NetworkMonitor{
		onChange:   onChange,
		stopChan:   make(chan struct{}),
		updateChan: make(chan netlink.AddrUpdate),
	}
}

func (nm *NetworkMonitor) Start() error {
	log.Printf("Netlink monitor initiated")
	initialAddr, err := nm.getCurrentAddress()
	if err != nil {
		return fmt.Errorf("failed to get initial address: %w", err)
	}
	nm.currentAddress = initialAddr
	log.Printf("Monitor started with initial address: %s", initialAddr)

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

func (nm *NetworkMonitor) getCurrentAddress() (string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("failed to list routes: %w", err)
	}

	defaultIfaceIndex := -1
	for _, route := range routes {
		// If the route has no destination, it should be the default route
		if route.Dst == nil || route.Dst.IP.IsUnspecified() || route.Dst.String() == "0.0.0.0/0" {
			defaultIfaceIndex = route.LinkIndex
			break
		}
	}
	if defaultIfaceIndex == -1 {
		return "", fmt.Errorf("no default route found")
	}

	iface, err := net.InterfaceByIndex(defaultIfaceIndex)
	if err != nil {
		return "", fmt.Errorf("failed to get interface by index: %w", err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("failed to get addresses for interface: %w", err)
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

		if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("no suitable network interface found")
}

func (nm *NetworkMonitor) monitorLoop() {
	defer log.Println("Network monitor goroutine stopped.")

	for {
		select {
		case <-nm.updateChan:
			newAddr, err := nm.getCurrentAddress()
			if err != nil {
				continue
			}
			if newAddr != nm.currentAddress {
				oldAddr := nm.currentAddress
				nm.currentAddress = newAddr
				if nm.onChange != nil {
					time.Sleep(2 * time.Second) // brief delay to allow network to stabilize
					nm.onChange(oldAddr, newAddr)
				}
			}
		case <-nm.stopChan:
			return
		}
	}
}
