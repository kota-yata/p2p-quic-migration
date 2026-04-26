//go:build !linux && !android

package main

import (
	"net"
)

func listenUDPOnInterface(_ string, ip net.IP) (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
}
