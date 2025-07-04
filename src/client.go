//go:build client
// +build client

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

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
	peerAddr := flag.String("peerAddr", "127.0.0.1:1234", "Address of the peer to connect to")
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
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, serverAddrResolved, tlsConfig, quicConfig)
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

	peerAddrResolved, err := net.ResolveUDPAddr("udp", *peerAddr)
	if err != nil {
		log.Fatalf("Failed to resolve peer address: %v", err)
	}

	// Create a new context for peer connection
	peerCtx, peerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer peerCancel()

	peerConn, err := tr.Dial(peerCtx, peerAddrResolved, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("Failed to connect to peer: %v", err)
	}
	log.Printf("Connected to peer at %s\n", *peerAddr)

	stream, err := peerConn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("Failed to open stream to peer: %v", err)
	}
	defer stream.Close()

	if _, err = stream.Write([]byte("Hello from client\n")); err != nil {
		log.Fatal("write: ", err)
	}

	response := make([]byte, 1024)
	n, err := stream.Read(response)
	if err != nil {
		log.Fatalf("Failed to read from stream: %v", err)
	}

	fmt.Printf("Received: %s\n", string(response[:n]))
}
