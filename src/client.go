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

	serverAddr := flag.String("serverAddr", "127.0.0.1:12345", "Address to bind to")
	flag.Parse()

	udpAddr, err := net.ResolveUDPAddr("udp", *serverAddr)

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 1234, IP: net.IPv4zero})
	tr := quic.Transport{
		Conn: udpConn,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, udpAddr, tlsConfig, quicConfig)

	defer conn.CloseWithError(0, "")

	fmt.Printf("Connected to QUIC server at %s\n", serverAddr)

	for {
		// Check for observed address from the connection and break if received
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			fmt.Printf("Observed address received: %s\n", observedAddr.String())
			break
		}
	}

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	// Read a limited amount instead of all data
	response := make([]byte, 1024)
	n, err := stream.Read(response)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Received: %s\n", string(response[:n]))
}
