//go:build server
// +build server

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/kota-yata/p2p-quic-migration/src/shared"
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

	// Request peer list from intermediate server
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("Failed to open stream to intermediate server: %v", err)
	}
	defer stream.Close()

	// Send GET_PEERS request
	if _, err = stream.Write([]byte("GET_PEERS")); err != nil {
		log.Fatalf("Failed to send GET_PEERS request: %v", err)
	}

	// Read peer list response
	buffer := make([]byte, 4096)
	n, err := stream.Read(buffer)
	if err != nil {
		log.Fatalf("Failed to read peer list: %v", err)
	}

	var peers []shared.PeerInfo
	if err := json.Unmarshal(buffer[:n], &peers); err != nil {
		log.Fatalf("Failed to unmarshal peer list: %v", err)
	}

	log.Printf("Received %d peers from intermediate server:", len(peers))
	for _, peer := range peers {
		log.Printf("  Peer: %s (Address: %s)", peer.ID, peer.Address)
	}

	ln, err := tr.Listen(tlsConfig, quicConfig)
	if err != nil {
		log.Fatal("listen addr: ", err)
	}
	defer ln.Close()

	log.Print("Start Server: 127.0.0.1:1234")

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Fatal("accept: ", err)
		}

		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Fatal("accept stream: ", err)
		}

		log.Print("New Client Connection Accepted")
		go func(stream *quic.Stream) {
			s := bufio.NewScanner(stream)
			for s.Scan() {
				msg := s.Text()
				log.Printf("Accept Message: `%s`", msg)
				_, err := stream.Write([]byte("Hello from server\n"))
				if err != nil {
					log.Fatal("write: ", err)
				}
			}
		}(stream)
	}
}
