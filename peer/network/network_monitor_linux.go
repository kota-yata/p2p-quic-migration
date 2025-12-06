//go:build linux || android
// +build linux android

package network_monitor

import (
    "fmt"
    "log"
    "net"

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
    // Get initial address
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
                    nm.onChange(oldAddr, newAddr)
                }
            }
        case <-nm.stopChan:
            return
        }
    }
}

