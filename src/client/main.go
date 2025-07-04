package main

import (
	"context"
	"crypto/tls"
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

	serverAddr := "127.0.0.1:12345"
	udpAddr, err := net.ResolveUDPAddr("udp", serverAddr)

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 1234, IP: net.IPv4zero})
	tr := quic.Transport{
		Conn: udpConn,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, udpAddr, tlsConfig, quicConfig)

	defer conn.CloseWithError(0, "")

	fmt.Printf("Connected to QUIC server at %s\n", serverAddr)

	// wait for few seconds to ensure the connection is established
	time.Sleep(2 * time.Second)

	// Check for observed address from the connection
	if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
		fmt.Printf("Observed address received: %s\n", observedAddr.String())
	} else {
		fmt.Printf("No observed address received yet\n")
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
