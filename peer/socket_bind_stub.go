//go:build !linux && !android

package main

import (
    "net"
)

func ifaceNameForIP(ip net.IP) (string, error) {
    return "", nil
}

func bindToDevice(conn *net.UDPConn, ifName string) error {
    return nil
}

