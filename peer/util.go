package main

import (
	"net"
	"strconv"
	"strings"

	"github.com/kota-yata/p2p-quic-migration/shared/qswitch"
)

func interfacePriority(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "rmnet"),
		strings.HasPrefix(lower, "ccmni"),
		strings.HasPrefix(lower, "wwan"),
		strings.HasPrefix(lower, "pdp"),
		strings.HasPrefix(lower, "usb"):
		return 0
	case strings.HasPrefix(lower, "eth"),
		strings.HasPrefix(lower, "en"):
		return 1
	case strings.HasPrefix(lower, "wlan"),
		strings.HasPrefix(lower, "wl"):
		return 2
	default:
		return 3
	}
}

func sameIPFamily(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	return (a.To4() != nil) == (b.To4() != nil)
}

func ipEqual(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	if a.To4() != nil && b.To4() != nil {
		return a.To4().Equal(b.To4())
	}
	return a.Equal(b)
}

func toProtoAddr(addrStr string) (qswitch.Address, bool) {
	var zero qswitch.Address
	host, portStr, err := net.SplitHostPort(addrStr)
	if err != nil {
		return zero, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return zero, false
	}
	var portNum uint16
	if pn, err := strconv.Atoi(portStr); err == nil {
		portNum = uint16(pn)
	} else if p2, err := net.LookupPort("udp", portStr); err == nil {
		portNum = uint16(p2)
	} else {
		return zero, false
	}
	if ip.To4() != nil {
		return qswitch.Address{AF: 0x04, IP: ip.To4(), Port: portNum}, true
	}
	return qswitch.Address{AF: 0x06, IP: ip.To16(), Port: portNum}, true
}
