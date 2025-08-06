package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
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

	peerHandler := NewServerPeerHandler(s.transport, s.tlsConfig, s.quicConfig, intermediateConn)
	go intermediateClient.ManagePeerDiscovery(intermediateConn, peerHandler)

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
	log.Print("New Peer Connection Accepted. Setting up bidirectional audio and video streaming...")

	peerHandler.StopAudioRelay()

	// Since we received the connection, we act as the "acceptor"
	// We accept incoming streams from the initiator and then open our own
	log.Print("Acting as connection acceptor - waiting for peer streams first")
	handleBidirectionalCommunicationAsAcceptor(conn)
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

	log.Printf("NAT hole punch attempt %d/%d to %s succeeded - acting as connection initiator", attempt, maxHolePunchAttempts, peerAddr)

	// We open streams first and then accept return streams
	go handleBidirectionalCommunicationAsInitiator(conn, peerAddr)

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

// Initiator: Opens streams first, then accepts return streams
func handleBidirectionalCommunicationAsInitiator(conn *quic.Conn, peerAddr string) {
	defer conn.CloseWithError(0, "Initiator session completed")
	log.Printf("Starting bidirectional communication as initiator with %s", peerAddr)

	// First: Open our outgoing streams
	audioSendStream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open outgoing audio stream as initiator: %v", err)
		return
	}
	defer audioSendStream.Close()

	videoSendStream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open outgoing video stream as initiator: %v", err)
		return
	}
	defer videoSendStream.Close()

	log.Printf("Initiator opened outgoing streams, starting to send audio/video")

	var wg sync.WaitGroup

	// Start sending our audio and video
	wg.Add(2)
	go func() {
		defer wg.Done()
		audioStreamer := NewAudioStreamer(audioSendStream)
		if err := audioStreamer.StreamAudio(); err != nil {
			log.Printf("Initiator audio streaming failed: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		videoStreamer := NewVideoStreamer(videoSendStream)
		if err := videoStreamer.StreamVideo(); err != nil {
			log.Printf("Initiator video streaming failed: %v", err)
		}
	}()

	// Then: Accept return streams from the acceptor
	go func() {
		log.Printf("Initiator waiting for return streams from acceptor...")
		acceptStreamsFromPeer(conn, "initiator")
	}()

	wg.Wait()
	log.Printf("Initiator bidirectional communication completed")
}

// Acceptor: Accepts streams first, then opens return streams
func handleBidirectionalCommunicationAsAcceptor(conn *quic.Conn) {
	log.Printf("Starting bidirectional communication as acceptor")

	// First: Accept incoming streams from initiator
	go func() {
		log.Printf("Acceptor waiting for incoming streams from initiator...")
		acceptStreamsFromPeer(conn, "acceptor")
	}()

	// Give the acceptor goroutine a moment to start, then open our return streams
	time.Sleep(100 * time.Millisecond)

	audioSendStream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open return audio stream as acceptor: %v", err)
		return
	}
	defer audioSendStream.Close()

	videoSendStream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open return video stream as acceptor: %v", err)
		return
	}
	defer videoSendStream.Close()

	log.Printf("Acceptor opened return streams, starting to send audio/video back")

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		audioStreamer := NewAudioStreamer(audioSendStream)
		if err := audioStreamer.StreamAudio(); err != nil {
			log.Printf("Acceptor audio streaming failed: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		videoStreamer := NewVideoStreamer(videoSendStream)
		if err := videoStreamer.StreamVideo(); err != nil {
			log.Printf("Acceptor video streaming failed: %v", err)
		}
	}()

	wg.Wait()
	log.Printf("Acceptor bidirectional communication completed")
}

// Common function to accept and handle incoming streams
func acceptStreamsFromPeer(conn *quic.Conn, role string) {
	streamCount := 0
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("%s error accepting incoming stream: %v", role, err)
			break
		}

		streamCount++
		log.Printf("%s accepted incoming stream #%d", role, streamCount)

		if streamCount == 1 {
			go handleIncomingAudioStream(stream, role)
		} else if streamCount == 2 {
			go handleIncomingVideoStream(stream, role)
		} else {
			log.Printf("%s unexpected additional stream #%d, closing", role, streamCount)
			stream.Close()
		}
	}
}

func handleIncomingAudioStream(stream *quic.Stream, role string) {
	defer stream.Close()
	log.Printf("%s starting to receive and play incoming audio stream", role)

	audioReceiver := NewAudioReceiver(stream)
	if err := audioReceiver.ReceiveAudio(); err != nil {
		log.Printf("%s audio receiving failed: %v", role, err)
	} else {
		log.Printf("%s audio receiving completed successfully", role)
	}
}

func handleIncomingVideoStream(stream *quic.Stream, role string) {
	defer stream.Close()
	log.Printf("%s starting to receive and play incoming video stream", role)

	videoReceiver := NewVideoReceiver(stream)
	if err := videoReceiver.ReceiveVideo(); err != nil {
		log.Printf("%s video receiving failed: %v", role, err)
	} else {
		log.Printf("%s video receiving completed successfully", role)
	}
}
