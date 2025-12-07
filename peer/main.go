package main

import (
	"flag"
	"log"
)

const (
	serverPort = 1234
)

var connectionEstablished = make(chan bool, 1)

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
    flag.Parse()

    cfg := &ServerConfig{
        keyFile:    *key,
        certFile:   *cert,
        serverAddr: *serverAddr,
        role:       *role,
    }

    switch cfg.role {
    case "sender", "receiver", "both":
        // ok
    default:
        log.Fatalf("invalid -role value: %s (allowed: sender, receiver, both)", cfg.role)
    }

    return cfg
}
