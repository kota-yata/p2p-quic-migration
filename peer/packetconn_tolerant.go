package main

import (
    "net"
    "os"
    "strings"
    "syscall"
)

// tolerantPacketConn wraps a net.PacketConn and swallows transient routing
// errors (like ENETUNREACH) on WriteTo. This helps keep a QUIC connection alive
// while the old interface disappears during path migration.
type tolerantPacketConn struct {
    net.PacketConn
    swallowENetUnreach bool
}

func (t *tolerantPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
    n, err := t.PacketConn.WriteTo(b, addr)
    if err == nil || !t.swallowENetUnreach {
        return n, err
    }
    // Detect ENETUNREACH in the error chain and pretend success
    if isNetUnreachable(err) {
        return len(b), nil
    }
    return n, err
}

func isNetUnreachable(err error) bool {
    if err == nil {
        return false
    }
    // net.OpError -> os.SyscallError -> errno
    if op, ok := err.(*net.OpError); ok && op != nil {
        if se, ok := op.Err.(*os.SyscallError); ok && se != nil {
            if errno, ok := se.Err.(syscall.Errno); ok {
                return errno == syscall.ENETUNREACH
            }
        }
    }
    // Fallback string check
    return strings.Contains(err.Error(), "network is unreachable")
}

