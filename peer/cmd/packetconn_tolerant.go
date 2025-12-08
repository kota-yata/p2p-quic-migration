package main

import (
    "net"
    "os"
    "strings"
    "syscall"
)

type tolerantPacketConn struct {
    net.PacketConn
    swallowENetUnreach bool
}

func (t *tolerantPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
    n, err := t.PacketConn.WriteTo(b, addr)
    if err == nil || !t.swallowENetUnreach {
        return n, err
    }
    if isNetUnreachable(err) {
        return len(b), nil
    }
    return n, err
}

func isNetUnreachable(err error) bool {
    if err == nil {
        return false
    }
    if op, ok := err.(*net.OpError); ok && op != nil {
        if se, ok := op.Err.(*os.SyscallError); ok && se != nil {
            if errno, ok := se.Err.(syscall.Errno); ok {
                return errno == syscall.ENETUNREACH
            }
        }
    }
    return strings.Contains(err.Error(), "network is unreachable")
}

