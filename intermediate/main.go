package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared"
	proto "github.com/kota-yata/p2p-quic-migration/shared/cmp9protocol"
	"github.com/quic-go/quic-go"
)

type PeerRegistry struct {
    mu     sync.RWMutex
    nextID uint32
    // peer information, keyed by uint32 ID
    peers map[uint32]*shared.PeerInfo
    // connection by peer ID
    connections map[uint32]*quic.Conn
    // reverse lookup: connection remote addr string -> peer ID
    connIndex map[string]uint32
    // control stream for notifications by peer ID
    notificationStreams map[uint32]*quic.Stream
    // relay allow list per peer (string host:port entries)
    relayAllowList map[uint32][]string
}

func NewPeerRegistry() *PeerRegistry {
    return &PeerRegistry{
        nextID:              1,
        peers:               make(map[uint32]*shared.PeerInfo),
        connections:         make(map[uint32]*quic.Conn),
        connIndex:           make(map[string]uint32),
        notificationStreams: make(map[uint32]*quic.Stream),
        relayAllowList:      make(map[uint32][]string),
    }
}

func (pr *PeerRegistry) AddPeer(addr net.Addr, conn *quic.Conn) uint32 {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	now := time.Now()
	id := pr.nextID
	pr.nextID++
	peerInfo := &shared.PeerInfo{
		ID:          fmt.Sprintf("%d", id),
		Address:     addr.String(),
		ConnectedAt: now,
		LastSeen:    now,
	}
	pr.peers[id] = peerInfo
	pr.connections[id] = conn
	pr.connIndex[conn.RemoteAddr().String()] = id
	log.Printf("Added peer %s with address %s", id, addr.String())

	go pr.notifyPeersAboutNewPeer(id, peerInfo)
	return id
}

func (pr *PeerRegistry) RemovePeer(id uint32) {
    pr.mu.Lock()
    defer pr.mu.Unlock()

	if _, exists := pr.peers[id]; exists {
		delete(pr.peers, id)
		delete(pr.connections, id)
        if stream, exists := pr.notificationStreams[id]; exists {
            stream.Close()
            delete(pr.notificationStreams, id)
        }
        if _, ok := pr.relayAllowList[id]; ok {
            delete(pr.relayAllowList, id)
        }
        // remove from reverse index if present
        for k, v := range pr.connIndex {
            if v == id {
                delete(pr.connIndex, k)
                break
            }
        }
        log.Printf("Removed peer %s", id)
    }
}

func (pr *PeerRegistry) UpdateLastSeen(id uint32) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if peer, exists := pr.peers[id]; exists {
		peer.LastSeen = time.Now()
	}
}

func (pr *PeerRegistry) GetPeers() []*shared.PeerInfo {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	peers := make([]*shared.PeerInfo, 0, len(pr.peers))
	for _, peer := range pr.peers {
		peers = append(peers, peer)
	}
	return peers
}

func (pr *PeerRegistry) GetPeerCount() int {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return len(pr.peers)
}

func (pr *PeerRegistry) AddNotificationStream(id uint32, stream *quic.Stream) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.notificationStreams[id] = stream
	log.Printf("Added notification stream for peer %s", id)
}

func (pr *PeerRegistry) notifyPeersAboutNewPeer(newPeerID uint32, newPeerInfo *shared.PeerInfo) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	for peerID, stream := range pr.notificationStreams {
		if peerID == newPeerID {
			continue
		}

		go func(peerID string, stream *quic.Stream) {
			// build binary notification
			// newPeerInfo.ID is string, but message uses uint32; use newPeerID
			addr, err := parseAddrString(newPeerInfo.Address)
			if err != nil {
				log.Printf("Failed to encode address for NEW_PEER: %v", err)
				return
			}
			msg := proto.NewPeerNotif{PeerID: newPeerID, Address: addr}
			if err := proto.WriteMessage(stream, msg); err != nil {
				log.Printf("Failed to send peer notification to %s: %v", peerID, err)
				return
			}
			log.Printf("Sent new peer notification to %s about %d", peerID, newPeerID)
		}(fmt.Sprintf("%d", peerID), stream)
	}
}

func (pr *PeerRegistry) handleNetworkChange(req proto.NetworkChangeReq, peerID uint32, conn *quic.Conn) {
	oldAddrStr := addrToString(req.OldAddress)
	observedAddr := conn.RemoteAddr().String()

	log.Printf("Network change detected for peer %d: old %s, observed new %s",
		peerID, oldAddrStr, observedAddr)

	pr.mu.Lock()
	if peer, exists := pr.peers[peerID]; exists {
		peer.Address = observedAddr
		peer.LastSeen = time.Now()
	}
	pr.mu.Unlock()

	pr.notifyPeersAboutNetworkChange(peerID, oldAddrStr, observedAddr)
}

func (pr *PeerRegistry) notifyPeersAboutNetworkChange(changedPeerID uint32, oldAddr, newAddr string) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	for peerID, stream := range pr.notificationStreams {
		if peerID == changedPeerID {
			continue
		}

		go func(peerID string, stream *quic.Stream) {
			oldA, err := parseAddrString(oldAddr)
			if err != nil {
				log.Printf("encode old addr: %v", err)
				return
			}
			newA, err := parseAddrString(newAddr)
			if err != nil {
				log.Printf("encode new addr: %v", err)
				return
			}
			msg := proto.NetworkChangeNotif{PeerID: changedPeerID, OldAddress: oldA, NewAddress: newA}
			if err := proto.WriteMessage(stream, msg); err != nil {
				log.Printf("Failed to send network change notification to %s: %v", peerID, err)
				return
			}
			log.Printf("Sent network change notification to %s about %d", peerID, changedPeerID)
		}(fmt.Sprintf("%d", peerID), stream)
	}
}

// SetRelayAllowList replaces the relay allow list for a given peer.
func (pr *PeerRegistry) SetRelayAllowList(peerID uint32, allow []string) {
    pr.mu.Lock()
    defer pr.mu.Unlock()
    pr.relayAllowList[peerID] = append([]string(nil), allow...)
}

// FindTargetByAllowedSource finds a connection whose allow list contains the given source address string.
func (pr *PeerRegistry) FindTargetByAllowedSource(sourceAddr string) (*quic.Conn, uint32, bool) {
    pr.mu.RLock()
    defer pr.mu.RUnlock()
    for id, list := range pr.relayAllowList {
        for _, v := range list {
            if v == sourceAddr {
                if c, ok := pr.connections[id]; ok {
                    return c, id, true
                }
            }
        }
    }
    return nil, 0, false
}

var registry *PeerRegistry

func main() {
	key := flag.String("key", "", "TLS key (requires -cert option)")
	cert := flag.String("cert", "", "TLS certificate (requires -key option)")
	addr := flag.String("addr", "0.0.0.0:12345", "Address to bind to")
	flag.Parse()

	registry = NewPeerRegistry()

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
		log.Fatal("listen addr: ", err)
	}
	defer ln.Close()

	log.Printf("Start Intermediate Server: %s", *addr)

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Fatal("accept: ", err)
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn *quic.Conn) {
	peerID := registry.AddPeer(conn.RemoteAddr(), conn)

	defer func() {
		registry.RemovePeer(peerID)
		conn.CloseWithError(0, "")
	}()

	log.Printf("New Connection from: %s (assigned ID %d)", conn.RemoteAddr(), peerID)

	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Accept stream error: %v", err)
			return
		}

		go handleStream(stream, conn, peerID)
	}
}

func handleStream(stream *quic.Stream, conn *quic.Conn, peerID uint32) {
    defer stream.Close()

    registry.UpdateLastSeen(peerID)

    // Read first message to determine stream intent
    msg, err := proto.ReadMessage(stream)
    if err != nil {
        if err.Error() == "EOF" || strings.Contains(err.Error(), "use of closed network connection") {
            log.Printf("Stream closed by peer %d before first message", peerID)
        } else {
            log.Printf("Stream read error (first message): %v", err)
        }
        return
    }

    switch m := msg.(type) {
    case proto.AudioRelayReq:
        // Relay: this stream will carry raw audio to be forwarded to the target
        sourceAddr := conn.RemoteAddr().String()
        targetConn, targetID, ok := registry.FindTargetByAllowedSource(sourceAddr)
        if !ok {
            log.Printf("No target peer found whose allow list permits source %s; dropping relay request", sourceAddr)
            return
        }
        targetStream, err := targetConn.OpenStreamSync(context.Background())
        if err != nil {
            log.Printf("Failed to open stream to target %d for %s: %v", targetID, sourceAddr, err)
            return
        }
        defer targetStream.Close()

        buf := make([]byte, 4096)
        nbytes, err := io.CopyBuffer(targetStream, stream, buf)
        if err != nil {
            log.Printf("Relay copy error from %s to %d: %v", sourceAddr, targetID, err)
            return
        }
        log.Printf("Relayed %d bytes from %s to %d", nbytes, sourceAddr, targetID)
        return

    case proto.RelayAllowlistSet:
        // Update allow list for this peer and continue accepting updates
        addrs := make([]string, 0, len(m.Addresses))
        for _, a := range m.Addresses {
            addrs = append(addrs, net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port)))
        }
        registry.SetRelayAllowList(peerID, addrs)
        log.Printf("Updated allow list for peer %d with %d entries", peerID, len(addrs))
        // Continue to accept further updates on this stream
        for {
            next, err := proto.ReadMessage(stream)
            if err != nil {
                if err.Error() == "EOF" || strings.Contains(err.Error(), "use of closed network connection") {
                    return
                }
                log.Printf("Error reading allowlist update from %d: %v", peerID, err)
                return
            }
            if upd, ok := next.(proto.RelayAllowlistSet); ok {
                addrs = addrs[:0]
                for _, a := range upd.Addresses {
                    addrs = append(addrs, net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port)))
                }
                registry.SetRelayAllowList(peerID, addrs)
                log.Printf("Updated allow list for peer %d with %d entries", peerID, len(addrs))
            } else {
                log.Printf("Unexpected message on allowlist stream from %d: %T", peerID, next)
            }
        }

    case proto.GetPeersReq:
        // Original signaling/control stream; keep previous loop behavior
        // Immediately handle this first request, then register and continue
        registry.mu.RLock()
        entries := make([]proto.PeerEntry, 0, len(registry.peers))
        for id, info := range registry.peers {
            if id == peerID {
                continue
            }
            addr, err := parseAddrString(info.Address)
            if err != nil {
                continue
            }
            entries = append(entries, proto.PeerEntry{PeerID: id, Address: addr})
        }
        registry.mu.RUnlock()
        resp := proto.PeerListResp{Peers: entries}
        if err := proto.WriteMessage(stream, resp); err != nil {
            log.Printf("Failed to write peer list resp: %v", err)
            return
        }
        log.Printf("Sent peer list to %s: %d peers (excluding self)", conn.RemoteAddr(), len(entries))
        registry.AddNotificationStream(peerID, stream)
        log.Printf("Peer %d registered for notifications on the same stream", peerID)

        for {
            msg, err := proto.ReadMessage(stream)
            if err != nil {
                if err.Error() == "EOF" || strings.Contains(err.Error(), "use of closed network connection") {
                    log.Printf("Stream closed by peer %d", peerID)
                } else {
                    log.Printf("Stream read error: %v", err)
                }
                return
            }
            switch m := msg.(type) {
            case proto.NetworkChangeReq:
                registry.handleNetworkChange(m, peerID, conn)
            case proto.GetPeersReq:
                // respond again if requested
                registry.mu.RLock()
                entries = entries[:0]
                for id, info := range registry.peers {
                    if id == peerID {
                        continue
                    }
                    addr, err := parseAddrString(info.Address)
                    if err != nil {
                        continue
                    }
                    entries = append(entries, proto.PeerEntry{PeerID: id, Address: addr})
                }
                registry.mu.RUnlock()
                resp := proto.PeerListResp{Peers: entries}
                if err := proto.WriteMessage(stream, resp); err != nil {
                    log.Printf("Failed to write peer list resp: %v", err)
                    return
                }
            default:
                log.Printf("Unhandled message type on control stream: %T", m)
            }
        }

    case proto.NetworkChangeReq:
        // If a client sends NetworkChangeReq first on a fresh stream, handle and then return
        registry.handleNetworkChange(m, peerID, conn)
        return

    default:
        log.Printf("Unexpected first message on stream from %d: %T", peerID, m)
        return
    }
}

// helper: parse "host:port" into protocol Address
func parseAddrString(s string) (proto.Address, error) {
	var out proto.Address
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return out, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return out, fmt.Errorf("invalid IP: %s", host)
	}
	portU16 := uint16(0)
	if p, err := net.LookupPort("udp", portStr); err == nil {
		portU16 = uint16(p)
	} else {
		return out, err
	}
	if ip.To4() != nil {
		out = proto.Address{AF: 0x04, IP: ip.To4(), Port: portU16}
	} else {
		out = proto.Address{AF: 0x06, IP: ip.To16(), Port: portU16}
	}
	return out, nil
}

func addrToString(a proto.Address) string {
	return net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port))
}
