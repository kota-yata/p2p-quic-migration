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
	// optional local address per peer (string host:port)
	localAddrs map[uint32]string
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		nextID:              1,
		peers:               make(map[uint32]*shared.PeerInfo),
		connections:         make(map[uint32]*quic.Conn),
		connIndex:           make(map[string]uint32),
		notificationStreams: make(map[uint32]*quic.Stream),
		relayAllowList:      make(map[uint32][]string),
		localAddrs:          make(map[uint32]string),
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
		if _, ok := pr.localAddrs[id]; ok {
			delete(pr.localAddrs, id)
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

func (pr *PeerRegistry) AddNotificationStream(id uint32, stream *quic.Stream) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.notificationStreams[id] = stream
	log.Printf("Added notification stream for peer %s", id)
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

// SetLocalAddr records a peer's local address (host:port). Empty string clears it.
func (pr *PeerRegistry) SetLocalAddr(peerID uint32, local string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if local == "" {
		delete(pr.localAddrs, peerID)
		return
	}
	pr.localAddrs[peerID] = local
}

// BuildPeerEndpoint builds a PeerEndpoint for the given id.
func (pr *PeerRegistry) BuildPeerEndpoint(id uint32) (proto.PeerEndpoint, bool) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	info, ok := pr.peers[id]
	if !ok {
		return proto.PeerEndpoint{}, false
	}
	obs, err := parseAddrString(info.Address)
	if err != nil {
		return proto.PeerEndpoint{}, false
	}
	ep := proto.PeerEndpoint{PeerID: id, Flags: 0, Observed: obs}
	if l, ok := pr.localAddrs[id]; ok && l != "" {
		if la, err := parseAddrString(l); err == nil {
			ep.Flags |= 0x01
			ep.Local = la
		}
	}
	return ep, true
}

// BuildAllEndpoints creates endpoint entries for all peers except excludeID (if non-zero).
func (pr *PeerRegistry) BuildAllEndpoints(excludeID uint32) []proto.PeerEndpoint {
	pr.mu.RLock()
	ids := make([]uint32, 0, len(pr.peers))
	for id := range pr.peers {
		if id == excludeID {
			continue
		}
		ids = append(ids, id)
	}
	pr.mu.RUnlock()

	out := make([]proto.PeerEndpoint, 0, len(ids))
	for _, id := range ids {
		if ep, ok := pr.BuildPeerEndpoint(id); ok {
			out = append(out, ep)
		}
	}
	return out
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

	// Open control stream toward the client and send ObservedAddr
	ctrl, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open control stream to peer %d: %v", peerID, err)
		return
	}
	registry.AddNotificationStream(peerID, ctrl)

	if addr, err := parseAddrString(conn.RemoteAddr().String()); err == nil {
		_ = proto.WriteMessage(ctrl, proto.ObservedAddr{Observed: addr})
		log.Printf("Sent ObservedAddr to peer %d: %s", peerID, conn.RemoteAddr().String())
	} else {
		log.Printf("Failed to encode ObservedAddr for %d: %v", peerID, err)
	}

	// Handle control messages on the control stream
	go controlLoop(ctrl, conn, peerID)

	// Accept additional non-control streams from the client
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Accept stream error: %v", err)
			return
		}
		go handleNonControlStream(stream, conn, peerID)
	}
}

func controlLoop(stream *quic.Stream, conn *quic.Conn, peerID uint32) {
	defer stream.Close()

	for {
		msg, err := proto.ReadMessage(stream)
		if err != nil {
			if err.Error() == "EOF" || strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("Control stream closed by peer %d", peerID)
			} else {
				log.Printf("Control stream read error from %d: %v", peerID, err)
			}
			return
		}
		switch m := msg.(type) {
		case proto.SelfAddrsSet:
			if m.HasLocal {
				localStr := addrToString(m.Local)
				registry.SetLocalAddr(peerID, localStr)
				log.Printf("Peer %d set local address %s", peerID, localStr)
			} else {
				registry.SetLocalAddr(peerID, "")
				log.Printf("Peer %d has no local address", peerID)
			}
			// Broadcast NewPeerEndpointNotif to others
			if ep, ok := registry.BuildPeerEndpoint(peerID); ok {
				registry.mu.RLock()
				for otherID, s := range registry.notificationStreams {
					if otherID == peerID {
						continue
					}
					_ = proto.WriteMessage(s, proto.NewPeerEndpointNotif{Entry: ep})
				}
				registry.mu.RUnlock()
			}
		case proto.GetPeerEndpointsReq:
			entries := registry.BuildAllEndpoints(peerID)
			if err := proto.WriteMessage(stream, proto.PeerEndpointsResp{Entries: entries}); err != nil {
				log.Printf("Failed to write PeerEndpointsResp: %v", err)
				return
			}
			log.Printf("Sent %d endpoints to peer %d", len(entries), peerID)
		case proto.NetworkChangeReq:
			registry.handleNetworkChange(m, peerID, conn)
		default:
			log.Printf("Unhandled message on control stream from %d: %T", peerID, m)
		}
	}
}

// Non-control streams: first message determines stream intent
func handleNonControlStream(stream *quic.Stream, conn *quic.Conn, peerID uint32) {
	defer stream.Close()

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

	case proto.NetworkChangeReq:
		// Allow network change notifications on non-control streams as well
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
