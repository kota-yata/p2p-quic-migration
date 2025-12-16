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
	role := flag.String("role", "both", "Peer role: sender, receiver, or both")
	record := flag.Bool("record", false, "Record incoming audio to a file")
	recordPath := flag.String("rpath", "recordings/received_audio.raw", "Path to store incoming audio when --record is set")
	flag.Parse()

	cfg := &ServerConfig{
		keyFile:    *key,
		certFile:   *cert,
		serverAddr: *serverAddr,
		role:       *role,
		record:     *record,
		recordPath: *recordPath,
	}

	if *role != "sender" && *role != "receiver" && *role != "both" {
		log.Fatalf("Invalid role specified: %s. Must be 'sender', 'receiver', or 'both'.", *role)
	}

	return cfg
}
