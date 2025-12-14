//go:build linux || android

package network_monitor

import (
	"fmt"
	"log"
	"net"
	"strings"

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
	initialAddr, err := nm.GetCurrentAddress()
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

// getCurrentAddress retrieves the current preferred IPv4 address
// The most desirable functionality here is to return the default route interface's IP.
// However, Android devices sometimes lack a default route, so we fall back to selecting
// an available non-loopback IPv4 address, prioritizing Wi-Fi and Ethernet interfaces.
func (nm *NetworkMonitor) GetCurrentAddress() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %w", err)
	}

	var availableAddrs []net.IP

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue // interface down or loopback
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
			default:
				continue
			}

			if ip != nil && ip.To4() != nil {
				if isWiFiInterface(iface.Name) || isEthernetInterface(iface.Name) {
					// Prioritize Wi-Fi and Ethernet interfaces
					availableAddrs = append([]net.IP{ip}, availableAddrs...)
				}
				availableAddrs = append(availableAddrs, ip)
			}
		}
	}

	if len(availableAddrs) >= 2 {
		return availableAddrs[0], nil
	} else if len(availableAddrs) == 1 {
		return availableAddrs[0], nil
	}

	return nil, fmt.Errorf("no suitable network interface found")
}

func (nm *NetworkMonitor) monitorLoop() {
	defer log.Println("Network monitor goroutine stopped.")

	for {
		select {
		case <-nm.updateChan:
			newAddr, err := nm.GetCurrentAddress()
			if err != nil {
				continue
			}
			if !newAddr.Equal(nm.currentAddress) {
				oldAddr := nm.currentAddress
				nm.currentAddress = newAddr
				if nm.onChange != nil {
					// time.Sleep(2 * time.Second)
					nm.onChange(oldAddr, newAddr)
				}
			}
		case <-nm.stopChan:
			return
		}
	}
}

// Heuristic approach by checking interface name prefix
func isWiFiInterface(ifaceName string) bool {
	return strings.HasPrefix(ifaceName, "wlan") || strings.HasPrefix(ifaceName, "en")
}
func isEthernetInterface(ifaceName string) bool {
	return strings.HasPrefix(ifaceName, "eth")
}
