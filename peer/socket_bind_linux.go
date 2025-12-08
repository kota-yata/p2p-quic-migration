//go:build linux || android

package main

import (
    "fmt"
    "log"
    "net"

    "golang.org/x/sys/unix"
)

// ifaceNameForIP returns the name of the network interface that owns the given IP.
func ifaceNameForIP(ip net.IP) (string, error) {
    if ip == nil {
        return "", fmt.Errorf("nil ip")
    }
    ifaces, err := net.Interfaces()
    if err != nil {
        return "", err
    }
    for _, iface := range ifaces {
        addrs, err := iface.Addrs()
        if err != nil {
            continue
        }
        for _, addr := range addrs {
            var aip net.IP
            switch v := addr.(type) {
            case *net.IPNet:
                aip = v.IP
            case *net.IPAddr:
                aip = v.IP
            }
            if aip != nil && aip.Equal(ip) {
                return iface.Name, nil
            }
        }
    }
    return "", fmt.Errorf("no interface found for %s", ip.String())
}

// bindToDevice binds a UDP socket to the specified interface using SO_BINDTODEVICE.
func bindToDevice(conn *net.UDPConn, ifName string) error {
    rawConn, err := conn.SyscallConn()
    if err != nil {
        return err
    }
    var bindErr error
    controlErr := rawConn.Control(func(fd uintptr) {
        // SO_BINDTODEVICE requires CAP_NET_RAW; on Android app UIDs typically have it via APIs.
        if err := unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifName); err != nil {
            bindErr = fmt.Errorf("SO_BINDTODEVICE %s failed: %w", ifName, err)
        }
    })
    if controlErr != nil {
        return controlErr
    }
    if bindErr != nil {
        return bindErr
    }
    log.Printf("Bound socket to device %s", ifName)
    return nil
}

