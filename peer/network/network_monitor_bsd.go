//go:build darwin || freebsd || netbsd || openbsd
// +build darwin freebsd netbsd openbsd

package network_monitor

import (
	"fmt"
	"log"
	"net"

	"golang.org/x/sys/unix"
)

const readBufferSize = 2048

type NetworkMonitor struct {
	currentAddress net.IP
	onChange       func(oldAddr, newAddr net.IP)
	fd             int
	stopChan       chan bool
}

func NewNetworkMonitor(onChange func(oldAddr, newAddr net.IP)) *NetworkMonitor {
	return &NetworkMonitor{
		onChange: onChange,
		stopChan: make(chan bool),
		fd:       -1, // File descriptor for AF_ROUTE socket
	}
}

func (nm *NetworkMonitor) Start() error {
	var err error

	nm.fd, err = unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, 0)
	if err != nil {
		return fmt.Errorf("failed to create AF_ROUTE socket: %v", err)
	}

	initialAddr, err := nm.getCurrentAddress()
	if err != nil {
		unix.Close(nm.fd)
		return fmt.Errorf("failed to get initial address: %v", err)
	}

	nm.currentAddress = initialAddr
	log.Printf("Monitor started with initial address: %s", initialAddr)

	go nm.monitorLoop()
	return nil
}

func (nm *NetworkMonitor) Stop() {
	close(nm.stopChan)

	if nm.fd != -1 {
		unix.Close(nm.fd)
	}
}

// Return the first non-loopback IPv4 address found on the system
func (nm *NetworkMonitor) getCurrentAddress() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
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
			return ip, nil
		}
	}

	return nil, fmt.Errorf("no suitable network interface found")
}

func (nm *NetworkMonitor) monitorLoop() {
	defer log.Println("Network monitor goroutine stopped.")

	buffer := make([]byte, readBufferSize)

	for {
		n, err := unix.Read(nm.fd, buffer)

		if err != nil {
			select {
			case <-nm.stopChan:
				return
			default:
				log.Printf("AF_ROUTE Read error: %v", err)
				continue
			}
		}

		if n == 0 {
			continue
		}

		newAddr, err := nm.getCurrentAddress()
		if err != nil {
			continue
		}

		if !newAddr.Equal(nm.currentAddress) {
			oldAddr := nm.currentAddress
			nm.currentAddress = newAddr

			if nm.onChange != nil {
				nm.onChange(oldAddr, newAddr)
			}
		}
	}
}
