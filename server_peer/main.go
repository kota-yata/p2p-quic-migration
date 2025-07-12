package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared/intermediate"
	"github.com/quic-go/quic-go"
)

const (
	serverPort           = 1234
	maxHolePunchAttempts = 5
	holePunchTimeout     = 2 * time.Second
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
	config     *ServerConfig
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	transport  *quic.Transport
	udpConn    *net.UDPConn
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

	intermediateClient := intermediate.NewClient(s.config.serverAddr, s.tlsConfig, s.quicConfig, s.transport)

	intermediateConn, err := intermediateClient.ConnectToServer()
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")

	intermediateClient.WaitForObservedAddress(intermediateConn)

	peerHandler := NewServerPeerHandler(s.transport, s.tlsConfig, s.quicConfig)
	go intermediateClient.ManagePeerDiscovery(intermediateConn, peerHandler)

	return s.runPeerListener()
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

func (s *Server) runPeerListener() error {
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
		// stop the hole punching attempts here
		connectionEstablished <- true

		go s.handleIncomingConnection(conn)
	}
}

func (s *Server) handleIncomingConnection(conn *quic.Conn) {
	log.Print("New Client Connection Accepted. Opening stream for video streaming...")
	
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open stream for video streaming: %v", err)
		return
	}

	log.Print("Stream opened, starting video streaming to client")
	handlePeerCommunication(stream, conn)
}

func attemptNATHolePunch(tr *quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan bool) {
	log.Printf("Starting NAT hole punching to peer: %s (will attempt %d times)", peerAddr, maxHolePunchAttempts)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s for NAT hole punch: %v", peerAddr, err)
		return
	}

	for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
		select {
		case <-stopChan:
			log.Printf("Stopping server NAT hole punch to %s - connection established", peerAddr)
			return
		default:
		}

		if err := performHolePunchAttempt(tr, peerAddrResolved, tlsConfig, quicConfig, peerAddr, attempt); err != nil {
			log.Printf("NAT hole punch attempt %d/%d to %s failed: %v", attempt, maxHolePunchAttempts, peerAddr, err)
		}

		if attempt < maxHolePunchAttempts {
			if waitBeforeNextHolePunch(attempt, stopChan) {
				log.Printf("Stopping server NAT hole punch to %s during wait - connection established", peerAddr)
				return
			}
		}
	}

	log.Printf("Completed all %d NAT hole punch attempts to peer %s", maxHolePunchAttempts, peerAddr)
}

func performHolePunchAttempt(tr *quic.Transport, peerAddrResolved *net.UDPAddr, tlsConfig *tls.Config, quicConfig *quic.Config, peerAddr string, attempt int) error {
	log.Printf("NAT hole punch attempt %d/%d to peer %s", attempt, maxHolePunchAttempts, peerAddr)

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to create UDP connection: %v", err)
	}
	defer udpConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), holePunchTimeout)
	defer cancel()

	conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

	if err != nil {
		log.Printf("NAT hole punch attempt %d/%d to %s completed (connection failed, which is normal): %v", attempt, maxHolePunchAttempts, peerAddr, err)
		return nil
	}

	log.Printf("NAT hole punch attempt %d/%d to %s succeeded - keeping connection alive for potential incoming streams", attempt, maxHolePunchAttempts, peerAddr)

	// Keep this connection alive but don't use it for streaming
	// The client will connect to our listener instead
	go func() {
		defer conn.CloseWithError(0, "Hole punch connection closed")
		// Keep the connection alive for a while
		time.Sleep(30 * time.Second)
	}()

	return nil
}

func waitBeforeNextHolePunch(attempt int, stopChan chan bool) bool {
	backoffDuration := time.Duration(attempt) * time.Second
	if backoffDuration > 5*time.Second {
		backoffDuration = 5 * time.Second
	}
	log.Printf("Waiting %v before next hole punch attempt %d", backoffDuration, attempt+1)

	select {
	case <-stopChan:
		return true // Signal to stop
	case <-time.After(backoffDuration):
		return false // Continue with next attempt
	}
}

func handlePeerCommunication(stream *quic.Stream, conn *quic.Conn) {
	defer stream.Close()
	log.Printf("Starting peer communication session")

	communicator := &PeerCommunicator{
		stream: stream,
		conn:   conn,
	}

	communicator.handleMessages()
}

type PeerCommunicator struct {
	stream *quic.Stream
	conn   *quic.Conn
}

func (pc *PeerCommunicator) handleMessages() {
	log.Printf("Starting audio stream to peer")

	audioStreamer := NewAudioStreamer(pc.stream)
	if err := audioStreamer.StreamAudio(); err != nil {
		log.Printf("Audio streaming failed: %v", err)
		return
	}
}
