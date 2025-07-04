//go:build intermediate
// +build intermediate

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"

	"github.com/quic-go/quic-go"
)

func main() {
	key := flag.String("key", "", "TLS key (requires -cert option)")
	cert := flag.String("cert", "", "TLS certificate (requires -key option)")
	addr := flag.String("addr", "127.0.0.1:12345", "Address to bind to")
	flag.Parse()

	cer, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Fatal("load cert: ", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cer},
		NextProtos:   []string{"p2p-quic"},
	}

	// Configure QUIC to support address discovery
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
	defer conn.CloseWithError(0, "")

	log.Printf("New Connection from: %s", conn.RemoteAddr())

	// Handle incoming streams
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Accept stream error: %v", err)
			return
		}

		go handleStream(stream, conn)
	}
}

func handleStream(stream *quic.Stream, conn *quic.Conn) {
	defer stream.Close()

	// For any data received, respond with the observed address information
	buffer := make([]byte, 1024)
	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Stream read error: %v", err)
			return
		}

		log.Printf("Received data from %s: %s", conn.RemoteAddr(), string(buffer[:n]))
	}
}
