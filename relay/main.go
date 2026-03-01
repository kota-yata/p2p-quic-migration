package main

import (
    "context"
    "crypto/tls"
    "flag"
    "fmt"
    "io"
    "log"
    "net"
    "sync"

    proto "github.com/kota-yata/p2p-quic-migration/shared/cmp9protocol"
    "github.com/quic-go/quic-go"
)

type RelayRegistry struct {
    mu       sync.RWMutex
    nextID   uint32
    conns    map[uint32]*quic.Conn
    allowmap map[uint32][]string // peerID -> allowed source addresses (host:port)
}

func NewRelayRegistry() *RelayRegistry {
    return &RelayRegistry{
        nextID:   1,
        conns:    make(map[uint32]*quic.Conn),
        allowmap: make(map[uint32][]string),
    }
}

func (rr *RelayRegistry) Add(conn *quic.Conn) uint32 {
    rr.mu.Lock()
    defer rr.mu.Unlock()
    id := rr.nextID
    rr.nextID++
    rr.conns[id] = conn
    log.Printf("Relay: added peer %d from %s", id, conn.RemoteAddr().String())
    return id
}

func (rr *RelayRegistry) Remove(id uint32) {
    rr.mu.Lock()
    defer rr.mu.Unlock()
    delete(rr.conns, id)
    delete(rr.allowmap, id)
    log.Printf("Relay: removed peer %d", id)
}

func (rr *RelayRegistry) SetAllowlist(id uint32, allow []string) {
    rr.mu.Lock()
    rr.allowmap[id] = append([]string(nil), allow...)
    rr.mu.Unlock()
}

// Find a target peer whose allowlist contains the given sourceAddr (host:port)
func (rr *RelayRegistry) FindTargetByAllowedSource(sourceAddr string) (*quic.Conn, uint32, bool) {
    rr.mu.RLock()
    defer rr.mu.RUnlock()
    for id, list := range rr.allowmap {
        for _, v := range list {
            if v == sourceAddr {
                if c, ok := rr.conns[id]; ok {
                    return c, id, true
                }
            }
        }
    }
    return nil, 0, false
}

func main() {
    key := flag.String("key", "", "TLS key (requires -cert option)")
    cert := flag.String("cert", "", "TLS certificate (requires -key option)")
    addr := flag.String("addr", "0.0.0.0:23456", "Relay server address to bind")
    flag.Parse()

    cer, err := tls.LoadX509KeyPair(*cert, *key)
    if err != nil {
        log.Fatal("load cert: ", err)
    }

    tlsConf := &tls.Config{
        Certificates: []tls.Certificate{cer},
        NextProtos:   []string{"p2p-quic"},
    }

    quicConf := &quic.Config{}

    ln, err := quic.ListenAddr(*addr, tlsConf, quicConf)
    if err != nil {
        log.Fatal("relay listen: ", err)
    }
    defer ln.Close()

    reg := NewRelayRegistry()
    log.Printf("Audio Relay Server listening on %s", *addr)

    for {
        conn, err := ln.Accept(context.Background())
        if err != nil {
            log.Printf("relay accept err: %v", err)
            continue
        }
        go handleRelayConn(reg, conn)
    }
}

func handleRelayConn(reg *RelayRegistry, conn *quic.Conn) {
    id := reg.Add(conn)
    defer func() {
        reg.Remove(id)
        conn.CloseWithError(0, "")
    }()

    log.Printf("Relay: new connection %d from %s", id, conn.RemoteAddr().String())

    for {
        stream, err := conn.AcceptStream(context.Background())
        if err != nil {
            log.Printf("relay stream accept err from %d: %v", id, err)
            return
        }
        go handleRelayStream(reg, id, conn, stream)
    }
}

func handleRelayStream(reg *RelayRegistry, id uint32, conn *quic.Conn, stream *quic.Stream) {
    defer stream.Close()

    msg, err := proto.ReadMessage(stream)
    if err != nil {
        log.Printf("relay read first msg err from %d: %v", id, err)
        return
    }
    switch m := msg.(type) {
    case proto.RelayAllowlistSet:
        addrs := make([]string, 0, len(m.Addresses))
        for _, a := range m.Addresses {
            addrs = append(addrs, net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port)))
        }
        reg.SetAllowlist(id, addrs)
        log.Printf("Relay: set allowlist for %d with %d entries", id, len(addrs))
        // keep reading updates on same stream
        for {
            next, err := proto.ReadMessage(stream)
            if err != nil {
                return
            }
            if upd, ok := next.(proto.RelayAllowlistSet); ok {
                addrs = addrs[:0]
                for _, a := range upd.Addresses {
                    addrs = append(addrs, net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port)))
                }
                reg.SetAllowlist(id, addrs)
                log.Printf("Relay: updated allowlist for %d with %d entries", id, len(addrs))
            } else {
                log.Printf("Relay: unexpected message on allowlist stream from %d: %T", id, next)
            }
        }
    case proto.AudioRelayReq:
        // Find target whose allowlist allows this source
        sourceAddr := conn.RemoteAddr().String()
        targetConn, targetID, ok := reg.FindTargetByAllowedSource(sourceAddr)
        if !ok {
            log.Printf("Relay: no target found allowing source %s; dropping", sourceAddr)
            return
        }
        targetStream, err := targetConn.OpenStreamSync(context.Background())
        if err != nil {
            log.Printf("Relay: open stream to target %d failed: %v", targetID, err)
            return
        }
        defer targetStream.Close()

        buf := make([]byte, 4096)
        n, err := io.CopyBuffer(targetStream, stream, buf)
        if err != nil {
            log.Printf("Relay: copy error from %d to %d: %v", id, targetID, err)
            return
        }
        log.Printf("Relay: relayed %d bytes from %d to %d", n, id, targetID)
    default:
        log.Printf("Relay: unexpected first message from %d: %T", id, m)
    }
}

