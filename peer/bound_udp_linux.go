//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func listenUDPOnInterface(iface string, ip net.IP) (*net.UDPConn, error) {
	addr := (&net.UDPAddr{IP: ip, Port: 0}).String()
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var bindErr error
			if err := c.Control(func(fd uintptr) {
				bindErr = unix.BindToDevice(int(fd), iface)
			}); err != nil {
				return err
			}
			return bindErr
		},
	}

	pc, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to bind socket to %s and listen on %s: %w", iface, addr, err)
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("unexpected packet connection type %T", pc)
	}
	return udp, nil
}
