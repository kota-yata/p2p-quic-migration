//go:build server
// +build server

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
	key := flag.String("key", "server.key", "TLS key (requires -cert option)")
	cert := flag.String("cert", "server.crt", "TLS certificate (requires -key option)")
	serverAddr := flag.String("serverAddr", "203.178.143.72:12345", "Address to intermediary server")
	flag.Parse()

	cer, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Fatalf("Failed to load certificate: %v", err)
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cer},
		NextProtos:         []string{"p2p-quic"},
	}
	quicConfig := &quic.Config{
		AddressDiscoveryMode: 1, // Request address observations
	}

	udpAddr, err := net.ResolveUDPAddr("udp", *serverAddr)
	if err != nil {
		log.Fatalf("Failed to resolve server address: %v", err)
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 1234, IP: net.IPv4zero})
	if err != nil {
		log.Fatalf("Failed to listen on UDP: %v", err)
	}
	defer udpConn.Close()

	tr := quic.Transport{
		Conn: udpConn,
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, udpAddr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("Failed to connect to intermediate server: %v", err)
	}
	defer conn.CloseWithError(0, "")

	log.Printf("Connected to intermediate server at %s\n", *serverAddr)

	// Check for observed address from the connection
	for i := 0; i < 10; i++ { // Add a maximum retry limit
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address received: %s\n", observedAddr.String())
			break
		}
	}

	// Start background goroutine for bidirectional communication with intermediate server
	go manageIntermediateServerCommunication(&tr, conn, tlsConfig, quicConfig)

	ln, err := tr.Listen(tlsConfig, quicConfig)
	if err != nil {
		log.Fatal("listen addr: ", err)
	}
	defer ln.Close()

	log.Print("Start Server: 0.0.0.0:1234")

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Fatal("accept: ", err)
		}

		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Accept stream error: %v", err)
			continue
		}

		log.Print("New Client Connection Accepted")
		go handlePeerCommunication(stream, conn)
	}
}

func manageIntermediateServerCommunication(tr *quic.Transport, conn *quic.Conn, tlsConfig *tls.Config, quicConfig *quic.Config) {
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

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Error reading from intermediate server: %v", err)
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
				// Attempt NAT hole punching by dialing each peer
				go attemptNATHolePunch(tr, peer.Address, tlsConfig, quicConfig)
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

			// If this is a new peer, attempt NAT hole punching
			if notification.Type == "NEW_PEER" {
				go attemptNATHolePunch(tr, notification.Peer.Address, tlsConfig, quicConfig)
			}
		}
	}
}

func attemptNATHolePunch(tr *quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config) {
	const maxHolePunchAttempts = 10
	log.Printf("Starting NAT hole punching to peer: %s (will attempt %d times)", peerAddr, maxHolePunchAttempts)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s for NAT hole punch: %v", peerAddr, err)
		return
	}

	// Perform multiple hole punching attempts
	for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
		log.Printf("NAT hole punch attempt %d/%d to peer %s", attempt, maxHolePunchAttempts, peerAddr)

		// Create a short-lived UDP connection for each hole punching attempt
		udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
		if err != nil {
			log.Printf("Failed to create UDP connection for NAT hole punch attempt %d to %s: %v", attempt, peerAddr, err)
			continue
		}

		// Use a short timeout for each hole punching attempt
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		// Attempt to dial the peer - this will initiate NAT hole punching
		// The connection doesn't need to succeed, just the attempt helps with NAT traversal
		conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

		// Clean up resources
		cancel()
		udpConn.Close()

		if err != nil {
			// This is expected to fail in most cases - that's okay for NAT hole punching
			log.Printf("NAT hole punch attempt %d/%d to %s completed (connection failed, which is normal): %v", attempt, maxHolePunchAttempts, peerAddr, err)
		} else {
			// If connection somehow succeeds, close it immediately since this is just for hole punching
			conn.CloseWithError(0, "NAT hole punch complete")
			log.Printf("NAT hole punch attempt %d/%d to %s succeeded (unexpected but good!)", attempt, maxHolePunchAttempts, peerAddr)
		}

		// If this wasn't the last attempt, wait before retrying
		if attempt < maxHolePunchAttempts {
			// Short backoff between hole punch attempts (1s, 2s, 3s, 4s, 5s, then 5s for remaining)
			backoffDuration := time.Duration(attempt) * time.Second
			if backoffDuration > 5*time.Second {
				backoffDuration = 5 * time.Second
			}
			log.Printf("Waiting %v before next hole punch attempt %d", backoffDuration, attempt+1)
			time.Sleep(backoffDuration)
		}
	}

	log.Printf("Completed all %d NAT hole punch attempts to peer %s", maxHolePunchAttempts, peerAddr)
}

func handlePeerCommunication(stream *quic.Stream, conn *quic.Conn) {
	defer stream.Close()
	log.Printf("Starting peer communication session")
	
	buffer := make([]byte, 4096)
	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Peer communication ended: %v", err)
			return
		}
		
		msg := string(buffer[:n])
		log.Printf("Received from peer: %s", msg)
		
		// Echo back with server identifier
		response := "Server echo: " + msg
		if _, err := stream.Write([]byte(response)); err != nil {
			log.Printf("Failed to write response: %v", err)
			return
		}
		
		// Send periodic messages to keep connection alive
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			counter := 0
			
			for range ticker.C {
				counter++
				periodic := fmt.Sprintf("Server periodic message #%d\n", counter)
				if _, err := stream.Write([]byte(periodic)); err != nil {
					log.Printf("Failed to send periodic message: %v", err)
					return
				}
				if counter >= 10 { // Stop after 10 messages
					return
				}
			}
		}()
	}
}
