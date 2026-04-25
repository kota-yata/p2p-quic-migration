package network_monitor

import (
	"fmt"
	"net"
	"runtime"
	"strings"
)

// GetCurrentAddress retrieves the current preferred IPv4 address.
func GetCurrentAddress() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %w", err)
	}

	var availableAddrs []net.IP

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
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
			default:
				continue
			}

			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			if isWiFiInterface(iface.Name) || isEthernetInterface(iface.Name) {
				availableAddrs = append([]net.IP{ip4}, availableAddrs...)
				continue
			}
			availableAddrs = append(availableAddrs, ip4)
		}
	}

	if len(availableAddrs) > 0 {
		return availableAddrs[0], nil
	}
	return nil, fmt.Errorf("no suitable network interface found")
}

// Heuristic approach by checking interface name prefix.
func isWiFiInterface(ifaceName string) bool {
	if strings.HasPrefix(ifaceName, "wlan") || strings.HasPrefix(ifaceName, "wl") {
		return true
	}
	return runtime.GOOS == "darwin" && strings.HasPrefix(ifaceName, "en")
}

func isEthernetInterface(ifaceName string) bool {
	return strings.HasPrefix(ifaceName, "eth") || strings.HasPrefix(ifaceName, "en")
}

func firstIPv4(iface net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
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
		if ip4 := ip.To4(); ip4 != nil {
			return ip4
		}
	}
	return nil
}
