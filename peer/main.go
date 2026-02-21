package main

import (
	"flag"
	"log"

	"github.com/quic-go/quic-go"
)

const (
	peerPort = 1234
)

// conn and role
type connchan struct {
	conn       *quic.Conn
	isAcceptor bool
}

var connectionEstablished = make(chan connchan, 1)

func main() {
	config := parseFlags()

	server := &Peer{
		config: config,
	}

	if err := server.Run(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func parseFlags() *ServerConfig {
	key := flag.String("key", "server.key", "TLS key (requires -cert option)")
	cert := flag.String("cert", "server.crt", "TLS certificate (requires -key option)")
	serverAddr := flag.String("serverAddr", "203.178.143.72:12345", "Address to intermediary server")
	relayAddr := flag.String("relayAddr", "203.178.143.72:12345", "Address to relay server")
	role := flag.String("role", "both", "Peer role: sender, receiver, or both")
	flag.Parse()

	cfg := &ServerConfig{
		keyFile:    *key,
		certFile:   *cert,
		serverAddr: *serverAddr,
		relayAddr:  *relayAddr,
		role:       *role,
	}

	if *role != "sender" && *role != "receiver" && *role != "both" {
		log.Fatalf("Invalid role specified: %s. Must be 'sender', 'receiver', or 'both'.", *role)
	}

	return cfg
}
