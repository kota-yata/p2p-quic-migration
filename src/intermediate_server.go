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
)

type PeerInfo struct {
	ID          string    `json:"id"`
	Address     string    `json:"address"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
}

type PeerNotification struct {
	Type string    `json:"type"`
	Peer *PeerInfo `json:"peer"`
}

type PeerRegistry struct {
	mu          sync.RWMutex
	peers       map[string]*PeerInfo
	connections map[string]*quic.Conn
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		peers:       make(map[string]*PeerInfo),
		connections: make(map[string]*quic.Conn),
	}
}

func (pr *PeerRegistry) AddPeer(id string, addr net.Addr, conn *quic.Conn) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	
	now := time.Now()
	peerInfo := &PeerInfo{
		ID:          id,
		Address:     addr.String(),
		ConnectedAt: now,
		LastSeen:    now,
	}
	pr.peers[id] = peerInfo
	pr.connections[id] = conn
	log.Printf("Added peer %s with address %s", id, addr.String())
	
	// Notify existing peers about the new peer
	go pr.notifyPeersAboutNewPeer(id, peerInfo)
}

func (pr *PeerRegistry) RemovePeer(id string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	
	if _, exists := pr.peers[id]; exists {
		delete(pr.peers, id)
		delete(pr.connections, id)
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

func (pr *PeerRegistry) GetPeers() []*PeerInfo {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	
	peers := make([]*PeerInfo, 0, len(pr.peers))
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

func (pr *PeerRegistry) notifyPeersAboutNewPeer(newPeerID string, newPeerInfo *PeerInfo) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	
	notification := PeerNotification{
		Type: "NEW_PEER",
		Peer: newPeerInfo,
	}
	
	notificationData, err := json.Marshal(notification)
	if err != nil {
		log.Printf("Failed to marshal peer notification: %v", err)
		return
	}
	
	for peerID, conn := range pr.connections {
		if peerID == newPeerID {
			continue // Don't notify the new peer about itself
		}
		
		go func(peerID string, conn *quic.Conn) {
			stream, err := conn.OpenStreamSync(context.Background())
			if err != nil {
				log.Printf("Failed to open stream for peer notification to %s: %v", peerID, err)
				return
			}
			defer stream.Close()
			
			_, err = stream.Write(notificationData)
			if err != nil {
				log.Printf("Failed to send peer notification to %s: %v", peerID, err)
				return
			}
			
			log.Printf("Sent new peer notification to %s about %s", peerID, newPeerID)
		}(peerID, conn)
	}
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
	registry.AddPeer(peerID, conn.RemoteAddr(), conn)
	
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
			allPeers := registry.GetPeers()
			// Filter out the requesting peer from the response
			peers := make([]*PeerInfo, 0, len(allPeers))
			for _, peer := range allPeers {
				if peer.ID != peerID {
					peers = append(peers, peer)
				}
			}
			
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
			log.Printf("Sent peer list to %s: %d peers (excluding self)", conn.RemoteAddr(), len(peers))
		
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
