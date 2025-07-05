//go:build client
// +build client

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
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

	// Keep the client running to maintain the connection
	select {
	case <-time.After(60 * time.Second):
		log.Println("Client session timeout after 60 seconds")
	case <-context.Background().Done():
		log.Println("Context cancelled")
	}
}

var clientConnectionEstablished = make(chan bool, 1)

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
				// Attempt NAT hole punching to each peer first
				go attemptNATHolePunch(*tr, peer.Address, tlsConfig, quicConfig, clientConnectionEstablished)
			}

			// If we have at least one peer and haven't connected yet, connect to the first one
			if len(peers) > 0 && !hasConnectedToPeer {
				// Wait a moment for hole punching to take effect, then try to connect
				go func() {
					time.Sleep(2 * time.Second) // Give hole punching time to work
					connectToPeer(peers[0].Address, tr, tlsConfig, quicConfig)
				}()
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

			// If this is a new peer, attempt NAT hole punching first
			if notification.Type == "NEW_PEER" {
				go attemptNATHolePunch(*tr, notification.Peer.Address, tlsConfig, quicConfig, clientConnectionEstablished)

				// If we haven't connected yet, try to connect to this new peer
				if !hasConnectedToPeer {
					go func() {
						time.Sleep(2 * time.Second) // Give hole punching time to work
						connectToPeer(notification.Peer.Address, tr, tlsConfig, quicConfig)
					}()
					hasConnectedToPeer = true
				}
			}
		}
	}
}

func connectToPeer(peerAddr string, tr *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	const maxRetries = 10
	log.Printf("Attempting to connect to peer at %s (will retry up to %d times)", peerAddr, maxRetries)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s: %v", peerAddr, err)
		return
	}

	var peerConn *quic.Conn
	var lastErr error

	// Retry connection attempts up to maxRetries times
	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("Connection attempt %d/%d to peer %s", attempt, maxRetries, peerAddr)

		// Create a new context for each attempt with reasonable timeout
		peerCtx, peerCancel := context.WithTimeout(context.Background(), 5*time.Second)

		peerConn, err = tr.Dial(peerCtx, peerAddrResolved, tlsConfig, quicConfig)
		peerCancel() // Clean up context

		if err == nil {
			// Connection successful!
			log.Printf("Successfully connected to peer %s on attempt %d", peerAddr, attempt)
			break
		}

		lastErr = err
		log.Printf("Connection attempt %d/%d to peer %s failed: %v", attempt, maxRetries, peerAddr, err)

		// If this wasn't the last attempt, wait before retrying
		if attempt < maxRetries {
			// Exponential backoff: 1s, 2s, 4s, 8s, 16s, then cap at 30s
			backoffDuration := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoffDuration > 30*time.Second {
				backoffDuration = 30 * time.Second
			}
			log.Printf("Waiting %v before retry attempt %d", backoffDuration, attempt+1)
			time.Sleep(backoffDuration)
		}
	}

	// Check if all attempts failed
	if peerConn == nil {
		log.Printf("All %d connection attempts to peer %s failed. Last error: %v", maxRetries, peerAddr, lastErr)
		return
	}

	// Connection successful - signal to stop hole punching
	select {
	case clientConnectionEstablished <- true:
		log.Printf("Connection established - signaling to stop hole punching")
	default:
		// Channel already has a value, which is fine
	}

	// Connection successful - proceed with communication
	defer peerConn.CloseWithError(0, "")

	peerStream, err := peerConn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open stream to peer %s after successful connection: %v", peerAddr, err)
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

	// Start continuous data exchange
	go maintainPeerCommunication(peerStream, peerAddr)

	// Keep the connection alive for ongoing communication
	log.Printf("Starting continuous communication with peer %s", peerAddr)
	time.Sleep(50 * time.Second) // Keep connection alive

	log.Printf("Peer connection to %s completed successfully!", peerAddr)
}

func attemptNATHolePunch(tr quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan bool) {
	const maxHolePunchAttempts = 10
	log.Printf("Client starting NAT hole punching to peer: %s (will attempt %d times)", peerAddr, maxHolePunchAttempts)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s for NAT hole punch: %v", peerAddr, err)
		return
	}

	// Perform multiple hole punching attempts
	for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
		// Check if connection has been established
		select {
		case <-stopChan:
			log.Printf("Stopping client NAT hole punch to %s - connection established", peerAddr)
			return
		default:
		}

		log.Printf("Client NAT hole punch attempt %d/%d to peer %s", attempt, maxHolePunchAttempts, peerAddr)

		// Use a short timeout for each hole punching attempt
		ctx, _ := context.WithTimeout(context.Background(), 2*time.Second)

		// Attempt to dial the peer - this will initiate NAT hole punching
		// The connection doesn't need to succeed, just the attempt helps with NAT traversal
		conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

		// Clean up resources
		// cancel()

		if err != nil {
			// This is expected to fail in most cases - that's okay for NAT hole punching
			log.Printf("Client NAT hole punch attempt %d/%d to %s completed (connection failed, which is normal): %v", attempt, maxHolePunchAttempts, peerAddr, err)
		} else {
			// If connection somehow succeeds, close it immediately since this is just for hole punching
			conn.CloseWithError(0, "NAT hole punch complete")
			log.Printf("Client NAT hole punch attempt %d/%d to %s succeeded (unexpected but good!)", attempt, maxHolePunchAttempts, peerAddr)
		}

		// If this wasn't the last attempt, wait before retrying
		if attempt < maxHolePunchAttempts {
			// Short backoff between hole punch attempts (1s, 2s, 3s, 4s, 5s, then 5s for remaining)
			backoffDuration := time.Duration(attempt) * time.Second
			if backoffDuration > 5*time.Second {
				backoffDuration = 5 * time.Second
			}
			log.Printf("Client waiting %v before next hole punch attempt %d", backoffDuration, attempt+1)
			time.Sleep(backoffDuration)
		}
	}

	log.Printf("Client completed all %d NAT hole punch attempts to peer %s", maxHolePunchAttempts, peerAddr)
}

func maintainPeerCommunication(stream *quic.Stream, peerAddr string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	counter := 0

	for range ticker.C {
		counter++
		message := fmt.Sprintf("Client message #%d to peer\n", counter)

		if _, err := stream.Write([]byte(message)); err != nil {
			log.Printf("Failed to send message to peer %s: %v", peerAddr, err)
			return
		}

		// Read response
		response := make([]byte, 1024)
		n, err := stream.Read(response)
		if err != nil {
			log.Printf("Failed to read response from peer %s: %v", peerAddr, err)
			return
		}

		log.Printf("Peer %s responded: %s", peerAddr, string(response[:n]))

		if counter >= 10 {
			log.Printf("Completed %d message exchanges with peer %s", counter, peerAddr)
			return
		}
	}
}
