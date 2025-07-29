package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type PeerHandler struct {
	intermediateAddr string
	peers            map[string]*PeerInfo
	peersMutex       sync.RWMutex
}

type PeerInfo struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	LastSeen time.Time
}

type PeerNotification struct {
	Type    string   `json:"type"`
	PeerID  string   `json:"peer_id"`
	Address string   `json:"address"`
	Peers   []string `json:"peers,omitempty"`
}

func NewPeerHandler(intermediateAddr string) *PeerHandler {
	return &PeerHandler{
		intermediateAddr: intermediateAddr,
		peers:           make(map[string]*PeerInfo),
	}
}

func (ph *PeerHandler) RegisterWithIntermediate(ctx context.Context, peerID string, localAddr string) error {
	log.Printf("Registering with intermediate server at %s", ph.intermediateAddr)
	
	// Connect to intermediate server
	conn, err := net.Dial("tcp", ph.intermediateAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer conn.Close()

	// Send registration message
	notification := PeerNotification{
		Type:    "register",
		PeerID:  peerID,
		Address: localAddr,
	}

	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal registration: %v", err)
	}

	if _, err := conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to send registration: %v", err)
	}

	// Read response
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read registration response: %v", err)
	}

	var responseNotification PeerNotification
	if err := json.Unmarshal([]byte(strings.TrimSpace(response)), &responseNotification); err != nil {
		return fmt.Errorf("failed to unmarshal response: %v", err)
	}

	if responseNotification.Type != "registered" {
		return fmt.Errorf("registration failed: %s", response)
	}

	log.Printf("Successfully registered with intermediate server")
	
	// Update peer list with known peers
	ph.updatePeerList(responseNotification.Peers)
	
	return nil
}

func (ph *PeerHandler) StartHolePunchingToAllPeers(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	ph.peersMutex.RLock()
	peers := make([]*PeerInfo, 0, len(ph.peers))
	for _, peer := range ph.peers {
		peers = append(peers, peer)
	}
	ph.peersMutex.RUnlock()

	for _, peer := range peers {
		go ph.attemptHolePunching(transport, tlsConfig, quicConfig, peer)
	}
}

func (ph *PeerHandler) attemptHolePunching(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config, peer *PeerInfo) {
	log.Printf("Attempting hole punching to peer %s at %s", peer.ID, peer.Address)
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try to establish QUIC connection
	conn, err := quic.DialAddr(ctx, peer.Address, tlsConfig, quicConfig)
	if err != nil {
		log.Printf("Failed to connect to peer %s: %v", peer.ID, err)
		return
	}
	defer conn.CloseWithError(0, "hole punching complete")

	log.Printf("Successfully established connection to peer %s", peer.ID)
	
	// Open a test stream
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		log.Printf("Failed to open stream to peer %s: %v", peer.ID, err)
		return
	}
	defer stream.Close()

	// Send a hello message
	helloMsg := fmt.Sprintf("peer-discovery\nhello from peer\n")
	if _, err := stream.Write([]byte(helloMsg)); err != nil {
		log.Printf("Failed to send hello to peer %s: %v", peer.ID, err)
		return
	}

	log.Printf("Hole punching to peer %s completed successfully", peer.ID)
}

func (ph *PeerHandler) updatePeerList(peerAddresses []string) {
	ph.peersMutex.Lock()
	defer ph.peersMutex.Unlock()

	// Clear existing peers
	ph.peers = make(map[string]*PeerInfo)

	// Add new peers
	for i, addr := range peerAddresses {
		peerID := fmt.Sprintf("peer-%d", i)
		ph.peers[peerID] = &PeerInfo{
			ID:       peerID,
			Address:  addr,
			LastSeen: time.Now(),
		}
		log.Printf("Added peer: %s at %s", peerID, addr)
	}
}

func (ph *PeerHandler) GetPeers() []*PeerInfo {
	ph.peersMutex.RLock()
	defer ph.peersMutex.RUnlock()

	peers := make([]*PeerInfo, 0, len(ph.peers))
	for _, peer := range ph.peers {
		peers = append(peers, peer)
	}
	return peers
}

func (ph *PeerHandler) AddPeer(peerID, address string) {
	ph.peersMutex.Lock()
	defer ph.peersMutex.Unlock()

	ph.peers[peerID] = &PeerInfo{
		ID:       peerID,
		Address:  address,
		LastSeen: time.Now(),
	}
	log.Printf("Added peer: %s at %s", peerID, address)
}

func (ph *PeerHandler) RemovePeer(peerID string) {
	ph.peersMutex.Lock()
	defer ph.peersMutex.Unlock()

	if _, exists := ph.peers[peerID]; exists {
		delete(ph.peers, peerID)
		log.Printf("Removed peer: %s", peerID)
	}
}

func (ph *PeerHandler) GetPeer(peerID string) (*PeerInfo, bool) {
	ph.peersMutex.RLock()
	defer ph.peersMutex.RUnlock()

	peer, exists := ph.peers[peerID]
	return peer, exists
}

func (ph *PeerHandler) NotifyNetworkChange(oldAddr, newAddr string) error {
	log.Printf("Notifying intermediate server of network change: %s -> %s", oldAddr, newAddr)
	
	conn, err := net.Dial("tcp", ph.intermediateAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer conn.Close()

	notification := PeerNotification{
		Type:    "network-change",
		Address: newAddr,
	}

	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal network change notification: %v", err)
	}

	if _, err := conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to send network change notification: %v", err)
	}

	log.Printf("Network change notification sent successfully")
	return nil
}

func (ph *PeerHandler) StartPeerDiscovery(ctx context.Context, peerID string, localAddr string) {
	// Register with intermediate server
	if err := ph.RegisterWithIntermediate(ctx, peerID, localAddr); err != nil {
		log.Printf("Failed to register with intermediate server: %v", err)
		return
	}

	// Periodically refresh peer list
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := ph.RegisterWithIntermediate(ctx, peerID, localAddr); err != nil {
				log.Printf("Failed to refresh peer registration: %v", err)
			}
		case <-ctx.Done():
			log.Println("Peer discovery stopped")
			return
		}
	}
}