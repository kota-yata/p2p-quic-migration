package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
	"github.com/quic-go/quic-go"
)

const (
	serverPort = 1234
)

var connectionEstablished = make(chan bool, 1)

func main() {
	config := parseFlags()

	server := &Server{
		config: config,
	}

	if err := server.Run(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

type ServerConfig struct {
	keyFile    string
	certFile   string
	serverAddr string
}

type Server struct {
	config           *ServerConfig
	tlsConfig        *tls.Config
	quicConfig       *quic.Config
	transport        *quic.Transport
	udpConn          *net.UDPConn
	intermediateConn *quic.Conn
	networkMonitor   *network_monitor.NetworkMonitor
	peerHandler      *ServerPeerHandler
}

func parseFlags() *ServerConfig {
	key := flag.String("key", "server.key", "TLS key (requires -cert option)")
	cert := flag.String("cert", "server.crt", "TLS certificate (requires -key option)")
	serverAddr := flag.String("serverAddr", "203.178.143.72:12345", "Address to intermediary server")
	flag.Parse()

	return &ServerConfig{
		keyFile:    *key,
		certFile:   *cert,
		serverAddr: *serverAddr,
	}
}

func (s *Server) Run() error {
	if err := s.setupTLS(); err != nil {
		return fmt.Errorf("failed to setup TLS: %v", err)
	}

	if err := s.setupTransport(); err != nil {
		return fmt.Errorf("failed to setup transport: %v", err)
	}
	defer s.cleanup()

	intermediateClient := NewClient(s.config.serverAddr, s.tlsConfig, s.quicConfig, s.transport)

	intermediateConn, err := intermediateClient.ConnectToServer()
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")
	s.intermediateConn = intermediateConn
	intermediateClient.WaitForObservedAddress(intermediateConn)

	peerHandler := NewServerPeerHandler(s.transport, s.tlsConfig, s.quicConfig, intermediateConn)
	s.peerHandler = peerHandler
	go intermediateClient.ManagePeerDiscovery(intermediateConn, peerHandler)

	s.networkMonitor = network_monitor.NewNetworkMonitor(s.handleNetworkChange)
	if err := s.networkMonitor.Start(); err != nil {
		return fmt.Errorf("failed to start network monitor: %v", err)
	}
	defer s.networkMonitor.Stop()

	return s.runPeerListener(peerHandler)
}

func (s *Server) setupTLS() error {
	cer, err := tls.LoadX509KeyPair(s.config.certFile, s.config.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load certificate: %v", err)
	}

	s.tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cer},
		NextProtos:         []string{"p2p-quic"},
	}

	s.quicConfig = &quic.Config{
		AddressDiscoveryMode: 1,
		KeepAlivePeriod:      30 * time.Second,
		MaxIdleTimeout:       5 * time.Minute,
	}

	return nil
}

func (s *Server) setupTransport() error {
	var err error
	s.udpConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: serverPort, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %v", err)
	}

	s.transport = &quic.Transport{
		Conn: s.udpConn,
	}

	return nil
}

func (s *Server) cleanup() {
	if s.transport != nil {
		s.transport.Close()
	}
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}

func (s *Server) runPeerListener(peerHandler *ServerPeerHandler) error {
	ln, err := s.transport.Listen(s.tlsConfig, s.quicConfig)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	defer ln.Close()

	log.Printf("Start Server: 0.0.0.0:%d", serverPort)

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		connectionEstablished <- true

		go s.handleIncomingConnection(conn, peerHandler)
	}
}

func (s *Server) handleIncomingConnection(conn *quic.Conn, peerHandler *ServerPeerHandler) {
	log.Print("New Peer Connection Accepted. Setting up bidirectional audio-only streaming...")

	peerHandler.StopAudioRelay()

	// Since we received the connection, we act as the "acceptor"
	// We accept incoming streams from the initiator and then open our own
	log.Print("Acting as connection acceptor - waiting for peer streams first")
	handleBidirectionalCommunicationAsAcceptor(conn)
}
