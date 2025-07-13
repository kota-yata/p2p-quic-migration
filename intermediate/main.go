package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

type PeerRegistry struct {
	mu                  sync.RWMutex
	peers               map[string]*shared.PeerInfo
	connections         map[string]*quic.Conn
	notificationStreams map[string]*quic.Stream
	audioRelays         map[string]*AudioRelaySession
}

// AudioRelaySession represents an active audio relay between two peers
type AudioRelaySession struct {
	sourcePeerID string
	targetPeerID string
	sourceStream *quic.Stream
	targetStream *quic.Stream
	stopChan     chan bool
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		peers:               make(map[string]*shared.PeerInfo),
		connections:         make(map[string]*quic.Conn),
		notificationStreams: make(map[string]*quic.Stream),
		audioRelays:         make(map[string]*AudioRelaySession),
	}
}

func (pr *PeerRegistry) AddPeer(id string, addr net.Addr, conn *quic.Conn) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	now := time.Now()
	peerInfo := &shared.PeerInfo{
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
		if stream, exists := pr.notificationStreams[id]; exists {
			stream.Close()
			delete(pr.notificationStreams, id)
		}
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

func (pr *PeerRegistry) AddNotificationStream(id string, stream *quic.Stream) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.notificationStreams[id] = stream
	log.Printf("Added notification stream for peer %s", id)
}

func (pr *PeerRegistry) notifyPeersAboutNewPeer(newPeerID string, newPeerInfo *shared.PeerInfo) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	notification := shared.PeerNotification{
		Type: "NEW_PEER",
		Peer: newPeerInfo,
	}

	notificationData, err := json.Marshal(notification)
	if err != nil {
		log.Printf("Failed to marshal peer notification: %v", err)
		return
	}

	for peerID, stream := range pr.notificationStreams {
		if peerID == newPeerID {
			continue // Don't notify the new peer about itself
		}

		go func(peerID string, stream *quic.Stream) {
			_, err := stream.Write(notificationData)
			if err != nil {
				log.Printf("Failed to send peer notification to %s: %v", peerID, err)
				return
			}

			log.Printf("Sent new peer notification to %s about %s", peerID, newPeerID)
		}(peerID, stream)
	}
}

func (pr *PeerRegistry) handleNetworkChange(message, peerID string) {
	parts := strings.Split(message, "|")
	if len(parts) != 3 {
		log.Printf("Invalid network change message format from %s: %s", peerID, message)
		return
	}

	oldAddr := parts[1]
	newAddr := parts[2]

	log.Printf("Network change detected for peer %s: %s -> %s", peerID, oldAddr, newAddr)

	// Update peer's address in registry
	pr.mu.Lock()
	if peer, exists := pr.peers[peerID]; exists {
		peer.Address = newAddr
		peer.LastSeen = time.Now()
	}
	pr.mu.Unlock()

	// Notify other peers about the network change
	pr.notifyPeersAboutNetworkChange(peerID, oldAddr, newAddr)
}

func (pr *PeerRegistry) handleAudioRelay(message, sourcePeerID string, sourceStream *quic.Stream) {
	parts := strings.Split(message, "|")
	if len(parts) != 2 {
		log.Printf("Invalid audio relay message format from %s: %s", sourcePeerID, message)
		return
	}

	targetPeerID := parts[1]
	log.Printf("Setting up audio relay from %s to %s", sourcePeerID, targetPeerID)

	pr.mu.Lock()
	defer pr.mu.Unlock()

	// Check if target peer exists and has a connection
	targetConn, exists := pr.connections[targetPeerID]
	if !exists {
		log.Printf("Target peer %s not found for audio relay", targetPeerID)
		sourceStream.Write([]byte("ERROR: Target peer not found"))
		return
	}

	// Open a stream to the target peer for audio relay
	targetStream, err := targetConn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open stream to target peer %s: %v", targetPeerID, err)
		sourceStream.Write([]byte("ERROR: Failed to connect to target"))
		return
	}

	// Create audio relay session
	relayID := fmt.Sprintf("%s->%s", sourcePeerID, targetPeerID)
	relay := &AudioRelaySession{
		sourcePeerID: sourcePeerID,
		targetPeerID: targetPeerID,
		sourceStream: sourceStream,
		targetStream: targetStream,
		stopChan:     make(chan bool, 1),
	}

	pr.audioRelays[relayID] = relay

	// Start the relay in a goroutine
	go relay.StartRelay()

	log.Printf("Audio relay session started: %s", relayID)
}

func (ars *AudioRelaySession) StartRelay() {
	defer func() {
		ars.targetStream.Close()
		log.Printf("Audio relay session ended: %s->%s", ars.sourcePeerID, ars.targetPeerID)
	}()

	// Relay audio data from source to target
	buffer := make([]byte, 4096)
	for {
		select {
		case <-ars.stopChan:
			return
		default:
			n, err := ars.sourceStream.Read(buffer)
			if err != nil {
				log.Printf("Audio relay read error: %v", err)
				return
			}

			if n > 0 {
				_, err = ars.targetStream.Write(buffer[:n])
				if err != nil {
					log.Printf("Audio relay write error: %v", err)
					return
				}
			}
		}
	}
}

func (pr *PeerRegistry) notifyPeersAboutNetworkChange(changedPeerID, oldAddr, newAddr string) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	notification := shared.NetworkChangeNotification{
		Type:       "NETWORK_CHANGE",
		PeerID:     changedPeerID,
		OldAddress: oldAddr,
		NewAddress: newAddr,
	}

	notificationData, err := json.Marshal(notification)
	if err != nil {
		log.Printf("Failed to marshal network change notification: %v", err)
		return
	}

	for peerID, stream := range pr.notificationStreams {
		if peerID == changedPeerID {
			continue // Don't notify the peer about its own change
		}

		go func(peerID string, stream *quic.Stream) {
			_, err := stream.Write(notificationData)
			if err != nil {
				log.Printf("Failed to send network change notification to %s: %v", peerID, err)
				return
			}

			log.Printf("Sent network change notification to %s about %s", peerID, changedPeerID)
		}(peerID, stream)
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
		log.Print("Waiting for data on stream...")
		n, err := stream.Read(buffer)
		if err != nil {
			if err.Error() == "EOF" {
				log.Printf("Stream closed by peer %s", peerID)
			} else {
				log.Printf("Stream read error: %v", err)
			}
			return
		}

		message := string(buffer[:n])

		// Handle different types of requests
		if strings.HasPrefix(message, "NETWORK_CHANGE|") {
			registry.handleNetworkChange(message, peerID)
			continue
		}

		if strings.HasPrefix(message, "AUDIO_RELAY|") {
			registry.handleAudioRelay(message, peerID, stream)
			continue
		}

		switch message {
		case "GET_PEERS":
			allPeers := registry.GetPeers()
			// Filter out the requesting peer from the response
			peers := make([]*shared.PeerInfo, 0, len(allPeers))
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

			// After sending peer list, register this stream for notifications
			registry.AddNotificationStream(peerID, stream)
			log.Printf("Peer %s registered for notifications on the same stream", peerID)

			// Keep the stream open by blocking here
			// The stream will be used for sending notifications
			select {} // Block forever until connection closes
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
