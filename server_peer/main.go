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

	// Open outgoing streams for sending audio/video
	audioSendStream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open outgoing audio stream: %v", err)
		return
	}

	videoSendStream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open outgoing video stream: %v", err)
		audioSendStream.Close()
		return
	}

	log.Print("Outgoing streams opened, starting bidirectional communication with peer")
	handleBidirectionalCommunication(audioSendStream, videoSendStream, conn)
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

	go func() {
		defer conn.CloseWithError(0, "Hole punch connection closed")
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

func handleBidirectionalCommunication(audioSendStream, videoSendStream *quic.Stream, conn *quic.Conn) {
	defer audioSendStream.Close()
	defer videoSendStream.Close()
	log.Printf("Starting bidirectional peer communication session")

	communicator := &BidirectionalPeerCommunicator{
		audioSendStream:  audioSendStream,
		videoSendStream:  videoSendStream,
		conn:            conn,
	}

	communicator.handleBidirectionalStreams()
}

type BidirectionalPeerCommunicator struct {
	audioSendStream  *quic.Stream
	videoSendStream  *quic.Stream
	conn            *quic.Conn
}

func (bpc *BidirectionalPeerCommunicator) handleBidirectionalStreams() {
	log.Printf("Starting bidirectional audio and video streaming")

	var wg sync.WaitGroup
	
	// Start sending audio and video
	audioStreamer := NewAudioStreamer(bpc.audioSendStream)
	videoStreamer := NewVideoStreamer(bpc.videoSendStream)

	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := audioStreamer.StreamAudio(); err != nil {
			log.Printf("Audio streaming failed: %v", err)
		} else {
			log.Printf("Audio streaming completed successfully")
		}
	}()

	go func() {
		defer wg.Done()
		if err := videoStreamer.StreamVideo(); err != nil {
			log.Printf("Video streaming failed: %v", err)
		} else {
			log.Printf("Video streaming completed successfully")
		}
	}()

	// Accept incoming streams for receiving audio and video
	go bpc.acceptIncomingStreams()

	wg.Wait()
	log.Printf("Bidirectional streaming completed")
}

func (bpc *BidirectionalPeerCommunicator) acceptIncomingStreams() {
	log.Printf("Waiting for incoming audio and video streams from peer...")
	
	streamCount := 0
	for {
		stream, err := bpc.conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Error accepting incoming stream: %v", err)
			break
		}

		streamCount++
		log.Printf("Accepted incoming stream #%d", streamCount)

		// For simplicity, assume first stream is audio, second is video
		// In a production system, you'd want a proper protocol to identify stream types
		if streamCount == 1 {
			go bpc.handleIncomingAudio(stream)
		} else if streamCount == 2 {
			go bpc.handleIncomingVideo(stream)
		} else {
			log.Printf("Unexpected additional stream #%d, closing", streamCount)
			stream.Close()
		}
	}
}

func (bpc *BidirectionalPeerCommunicator) handleIncomingAudio(stream *quic.Stream) {
	defer stream.Close()
	log.Printf("Starting to receive and play incoming audio stream")
	
	audioReceiver := NewAudioReceiver(stream)
	if err := audioReceiver.ReceiveAudio(); err != nil {
		log.Printf("Audio receiving failed: %v", err)
	} else {
		log.Printf("Audio receiving completed successfully")
	}
}

func (bpc *BidirectionalPeerCommunicator) handleIncomingVideo(stream *quic.Stream) {
	defer stream.Close()
	log.Printf("Starting to receive and play incoming video stream")
	
	videoReceiver := NewVideoReceiver(stream)
	if err := videoReceiver.ReceiveVideo(); err != nil {
		log.Printf("Video receiving failed: %v", err)
	} else {
		log.Printf("Video receiving completed successfully")
	}
}
