//go:build intermediate
// +build intermediate

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/kota-yata/p2p-quic-migration/src/shared"
)

type PeerRegistry struct {
	mu    sync.RWMutex
	peers map[string]*shared.PeerInfo
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		peers: make(map[string]*shared.PeerInfo),
	}
}

func (pr *PeerRegistry) AddPeer(id string, addr net.Addr) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	
	now := time.Now()
	pr.peers[id] = &shared.PeerInfo{
		ID:          id,
		Address:     addr.String(),
		ConnectedAt: now,
		LastSeen:    now,
	}
	log.Printf("Added peer %s with address %s", id, addr.String())
}

func (pr *PeerRegistry) RemovePeer(id string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	
	if _, exists := pr.peers[id]; exists {
		delete(pr.peers, id)
		log.Printf("Removed peer %s", id)
	}
}

func (pr *PeerRegistry) UpdateLastSeen(id string) {
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

	// Configure QUIC to support address discovery
	quicConf := &quic.Config{
		// Set to mode 0: willing to provide address observations but not requesting them
		AddressDiscoveryMode: 0,
	}

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
	peerID := conn.RemoteAddr().String()
	registry.AddPeer(peerID, conn.RemoteAddr())
	
	defer func() {
		registry.RemovePeer(peerID)
		conn.CloseWithError(0, "")
	}()

	log.Printf("New Connection from: %s", conn.RemoteAddr())

	// Handle incoming streams
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Accept stream error: %v", err)
			return
		}

		go handleStream(stream, conn, peerID)
	}
}

func handleStream(stream *quic.Stream, conn *quic.Conn, peerID string) {
	defer stream.Close()

	registry.UpdateLastSeen(peerID)

	buffer := make([]byte, 1024)
	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Stream read error: %v", err)
			return
		}

		message := string(buffer[:n])
		log.Printf("Received data from %s: %s", conn.RemoteAddr(), message)

		// Handle different types of requests
		switch message {
		case "GET_PEERS":
			peers := registry.GetPeers()
			response, err := json.Marshal(peers)
			if err != nil {
				log.Printf("Failed to marshal peers: %v", err)
				continue
			}
			
			_, err = stream.Write(response)
			if err != nil {
				log.Printf("Failed to write peers response: %v", err)
				return
			}
			log.Printf("Sent peer list to %s: %d peers", conn.RemoteAddr(), len(peers))
		
		case "HEARTBEAT":
			registry.UpdateLastSeen(peerID)
			_, err = stream.Write([]byte("HEARTBEAT_ACK"))
			if err != nil {
				log.Printf("Failed to write heartbeat response: %v", err)
				return
			}
			
		default:
			// For any other data, respond with observed address and peer count
			response := fmt.Sprintf("Observed address: %s, Connected peers: %d", 
				conn.RemoteAddr().String(), registry.GetPeerCount())
			_, err = stream.Write([]byte(response))
			if err != nil {
				log.Printf("Failed to write response: %v", err)
				return
			}
		}
	}
}
