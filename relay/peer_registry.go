package main

import (
	"log"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type PeerRegistry struct {
	mu    sync.RWMutex
	peers map[*quic.Conn]*Peer
}

func (pr *PeerRegistry) AddPeer(conn *quic.Conn) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	now := time.Now()
	peer := &Peer{
		LastSeen: now,
	}
	pr.peers[conn] = peer
}

func (pr *PeerRegistry) RemovePeer(conn *quic.Conn) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if _, exists := pr.peers[conn]; exists {
		delete(pr.peers, conn)
		log.Printf("Removed peer %s", conn.RemoteAddr().String())
	}
}

type Peer struct {
	LastSeen time.Time
	// RelayAllowList contains string forms of allowed source addresses ("ip:port").
	RelayAllowList []string
}

func (p *Peer) UpdateLastSeen() {
	p.LastSeen = time.Now()
}

func (p *Peer) AddToRelayAllowList(addr string) {
	p.RelayAllowList = append(p.RelayAllowList, addr)
}

func (p *Peer) RemoveFromRelayAllowList(addr string) {
	out := p.RelayAllowList[:0]
	for _, v := range p.RelayAllowList {
		if v != addr {
			out = append(out, v)
		}
	}
	p.RelayAllowList = out
}

func (p *Peer) IsInRelayAllowList(addr string) bool {
	for _, v := range p.RelayAllowList {
		if v == addr {
			return true
		}
	}
	return false
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		mu:    sync.RWMutex{},
		peers: make(map[*quic.Conn]*Peer),
	}
}

// SetRelayAllowList replaces the allow list for a given connection.
func (pr *PeerRegistry) SetRelayAllowList(conn *quic.Conn, allow []string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if p, ok := pr.peers[conn]; ok {
		p.RelayAllowList = append([]string(nil), allow...)
		p.UpdateLastSeen()
	}
}

// FindTargetByAllowedSource finds a connection whose allow list contains the given source address string.
func (pr *PeerRegistry) FindTargetByAllowedSource(sourceAddr string) (*quic.Conn, *Peer, bool) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	for c, p := range pr.peers {
		if p.IsInRelayAllowList(sourceAddr) {
			return c, p, true
		}
	}
	return nil, nil, false
}
