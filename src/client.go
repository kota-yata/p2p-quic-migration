//go:build client
// +build client

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"log"
	"net"
	"time"

	"github.com/kota-yata/p2p-quic-migration/src/shared"
	"github.com/quic-go/quic-go"
)

func main() {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-quic"},
	}

	quicConfig := &quic.Config{
		AddressDiscoveryMode: 1, // Request address observations
	}

	serverAddr := flag.String("serverAddr", "203.178.143.72:12345", "Address to the intermediary server")
	flag.Parse()

	serverAddrResolved, err := net.ResolveUDPAddr("udp", *serverAddr)
	if err != nil {
		log.Fatalf("Failed to resolve server address: %v", err)
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 1235, IP: net.IPv4zero})
	if err != nil {
		log.Fatalf("Failed to listen on UDP: %v", err)
	}
	defer udpConn.Close()

	tr := quic.Transport{
		Conn: udpConn,
	}
	// defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, serverAddrResolved, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("Failed to connect to intermediate server: %v", err)
	}
	// defer conn.CloseWithError(0, "")

	log.Printf("Connected to intermediate server at %s\n", *serverAddr)

	// Check for observed address from the connection
	for i := 0; i < 10; i++ { // Add a maximum retry limit
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address received: %s\n", observedAddr.String())
			break
		}
	}

	// Create a channel to communicate discovered peers
	peerChannel := make(chan string, 1)

	// Start background goroutine for bidirectional communication with intermediate server
	go manageIntermediateServerCommunication(conn, peerChannel, &tr, tlsConfig, quicConfig)

	// Wait for a peer to be discovered and automatically connect
	log.Println("Waiting for peer discovery...")
	
	select {
	case <-time.After(30 * time.Second):
		log.Fatal("Timeout waiting for peer discovery")
	case <-context.Background().Done():
		log.Println("Context cancelled")
	}
}

func manageIntermediateServerCommunication(conn *quic.Conn, peerChannel chan string, tr *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	// Open a single bidirectional stream for all communication
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open communication stream: %v", err)
		return
	}
	defer stream.Close()

	// Send GET_PEERS request first
	if _, err = stream.Write([]byte("GET_PEERS")); err != nil {
		log.Printf("Failed to send GET_PEERS request: %v", err)
		return
	}

	// Continuously read from the stream (peer list first, then notifications)
	buffer := make([]byte, 4096)
	isFirstMessage := true
	hasConnectedToPeer := false

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Failed to read from intermediate server: %v", err)
			return
		}

		if isFirstMessage {
			// First message should be the peer list
			var peers []shared.PeerInfo
			if err := json.Unmarshal(buffer[:n], &peers); err != nil {
				log.Printf("Failed to unmarshal peer list: %v", err)
				return
			}

			log.Printf("Received %d peers from intermediate server:", len(peers))
			for _, peer := range peers {
				log.Printf("  Peer: %s (Address: %s)", peer.ID, peer.Address)
			}

			// If we have at least one peer and haven't connected yet, connect to the first one
			if len(peers) > 0 && !hasConnectedToPeer {
				go connectToPeer(peers[0].Address, tr, tlsConfig, quicConfig)
				hasConnectedToPeer = true
			}
			isFirstMessage = false
		} else {
			// Subsequent messages should be notifications
			var notification shared.PeerNotification
			if err := json.Unmarshal(buffer[:n], &notification); err != nil {
				log.Printf("Failed to unmarshal peer notification: %v", err)
				continue
			}

			log.Printf("Received peer notification - Type: %s, Peer: %s (Address: %s)",
				notification.Type, notification.Peer.ID, notification.Peer.Address)

			// If this is a new peer notification and we haven't connected yet, connect to it
			if notification.Type == "NEW_PEER" && !hasConnectedToPeer {
				go connectToPeer(notification.Peer.Address, tr, tlsConfig, quicConfig)
				hasConnectedToPeer = true
			}
		}
	}
}

func connectToPeer(peerAddr string, tr *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	log.Printf("Attempting to connect to peer at %s", peerAddr)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s: %v", peerAddr, err)
		return
	}

	// Create a new context for peer connection
	peerCtx, peerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer peerCancel()

	peerConn, err := tr.Dial(peerCtx, peerAddrResolved, tlsConfig, quicConfig)
	if err != nil {
		log.Printf("Failed to connect to peer %s: %v", peerAddr, err)
		return
	}
	defer peerConn.CloseWithError(0, "")

	log.Printf("Successfully connected to peer at %s", peerAddr)

	peerStream, err := peerConn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open stream to peer %s: %v", peerAddr, err)
		return
	}
	defer peerStream.Close()

	if _, err = peerStream.Write([]byte("Hello from client\n")); err != nil {
		log.Printf("Failed to write to peer %s: %v", peerAddr, err)
		return
	}

	response := make([]byte, 1024)
	n, err := peerStream.Read(response)
	if err != nil {
		log.Printf("Failed to read from peer %s: %v", peerAddr, err)
		return
	}

	log.Printf("Received from peer %s: %s", peerAddr, string(response[:n]))
}
