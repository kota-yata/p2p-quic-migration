//go:build server
// +build server

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/kota-yata/p2p-quic-migration/src/shared"
	"github.com/quic-go/quic-go"
)

const (
	serverPort                = 1234
	maxHolePunchAttempts      = 10
	observedAddressMaxRetries = 10
	intermediateConnTimeout   = 10 * time.Second
	holePunchTimeout          = 2 * time.Second
	periodicMessageInterval   = 5 * time.Second
	maxPeriodicMessages       = 10
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

	intermediateConn, err := s.connectToIntermediateServer()
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")

	s.waitForObservedAddress(intermediateConn)

	go s.managePeerDiscovery(intermediateConn)

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

func (s *Server) connectToIntermediateServer() (*quic.Conn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", s.config.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), intermediateConnTimeout)
	defer cancel()

	conn, err := s.transport.Dial(ctx, udpAddr, s.tlsConfig, s.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to intermediate server: %v", err)
	}

	log.Printf("Connected to intermediate server at %s", s.config.serverAddr)
	return conn, nil
}

func (s *Server) waitForObservedAddress(conn *quic.Conn) {
	for i := 0; i < observedAddressMaxRetries; i++ {
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address received: %s", observedAddr.String())
			break
		}
	}
}

func (s *Server) managePeerDiscovery(conn *quic.Conn) {
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open communication stream: %v", err)
		return
	}
	defer stream.Close()

	if err := s.sendPeerRequest(stream); err != nil {
		log.Printf("Failed to send peer request: %v", err)
		return
	}

	peerManager := &PeerManager{
		server:         s,
		isFirstMessage: true,
	}

	peerManager.handlePeerCommunication(stream)
}

func (s *Server) sendPeerRequest(stream *quic.Stream) error {
	if _, err := stream.Write([]byte("GET_PEERS")); err != nil {
		return fmt.Errorf("failed to send GET_PEERS request: %v", err)
	}
	return nil
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

		go s.handleIncomingConnection(conn)
	}
}

func (s *Server) handleIncomingConnection(conn *quic.Conn) {
	stream, err := conn.AcceptStream(context.Background())
	if err != nil {
		log.Printf("Accept stream error: %v", err)
		return
	}

	log.Print("New Client Connection Accepted")
	handlePeerCommunication(stream, conn)
}

type PeerManager struct {
	server         *Server
	isFirstMessage bool
}

func (pm *PeerManager) handlePeerCommunication(stream *quic.Stream) {
	buffer := make([]byte, 4096)

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Error reading from intermediate server: %v", err)
			return
		}

		if pm.isFirstMessage {
			pm.handleInitialPeerList(buffer[:n])
			pm.isFirstMessage = false
		} else {
			pm.handlePeerNotification(buffer[:n])
		}
	}
}

func (pm *PeerManager) handleInitialPeerList(data []byte) {
	var peers []shared.PeerInfo
	if err := json.Unmarshal(data, &peers); err != nil {
		log.Printf("Failed to unmarshal peer list: %v", err)
		return
	}

	log.Printf("Received %d peers from intermediate server:", len(peers))
	for _, peer := range peers {
		log.Printf("  Peer: %s (Address: %s)", peer.ID, peer.Address)
		pm.startHolePunching(peer.Address)
	}
}

func (pm *PeerManager) handlePeerNotification(data []byte) {
	var notification shared.PeerNotification
	if err := json.Unmarshal(data, &notification); err != nil {
		log.Printf("Failed to unmarshal peer notification: %v", err)
		return
	}

	log.Printf("Received peer notification - Type: %s, Peer: %s (Address: %s)",
		notification.Type, notification.Peer.ID, notification.Peer.Address)

	if notification.Type == "NEW_PEER" {
		pm.startHolePunching(notification.Peer.Address)
	}
}

func (pm *PeerManager) startHolePunching(peerAddr string) {
	go attemptNATHolePunch(pm.server.transport, peerAddr, pm.server.tlsConfig, pm.server.quicConfig, connectionEstablished)
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
			waitBeforeNextHolePunch(attempt)
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

	conn.CloseWithError(0, "NAT hole punch complete")
	log.Printf("NAT hole punch attempt %d/%d to %s succeeded (unexpected but good!)", attempt, maxHolePunchAttempts, peerAddr)
	return nil
}

func waitBeforeNextHolePunch(attempt int) {
	backoffDuration := time.Duration(attempt) * time.Second
	if backoffDuration > 5*time.Second {
		backoffDuration = 5 * time.Second
	}
	log.Printf("Waiting %v before next hole punch attempt %d", backoffDuration, attempt+1)
	time.Sleep(backoffDuration)
}

func handlePeerCommunication(stream *quic.Stream, conn *quic.Conn) {
	defer stream.Close()
	log.Printf("Starting peer communication session")

	signalConnectionEstablished()

	communicator := &PeerCommunicator{
		stream: stream,
		conn:   conn,
	}

	communicator.handleMessages()
}

func signalConnectionEstablished() {
	select {
	case connectionEstablished <- true:
		log.Printf("Connection established - signaling to stop hole punching")
	default:
		// Channel already has a value, which is fine
	}
}

type PeerCommunicator struct {
	stream *quic.Stream
	conn   *quic.Conn
}

func (pc *PeerCommunicator) handleMessages() {
	buffer := make([]byte, 4096)
	periodicSender := pc.startPeriodicSender()
	defer periodicSender.stop()

	for {
		n, err := pc.stream.Read(buffer)
		if err != nil {
			log.Printf("Peer communication ended: %v", err)
			return
		}

		msg := string(buffer[:n])
		log.Printf("Received from peer: %s", msg)

		if err := pc.sendResponse(msg); err != nil {
			log.Printf("Failed to send response: %v", err)
			return
		}
	}
}

func (pc *PeerCommunicator) sendResponse(msg string) error {
	response := "Server echo: " + msg
	if _, err := pc.stream.Write([]byte(response)); err != nil {
		return fmt.Errorf("failed to write response: %v", err)
	}
	return nil
}

func (pc *PeerCommunicator) startPeriodicSender() *PeriodicSender {
	sender := &PeriodicSender{
		stream: pc.stream,
		ticker: time.NewTicker(periodicMessageInterval),
		stopCh: make(chan bool),
	}

	go sender.run()
	return sender
}

type PeriodicSender struct {
	stream  *quic.Stream
	ticker  *time.Ticker
	stopCh  chan bool
	counter int
}

func (ps *PeriodicSender) run() {
	defer ps.ticker.Stop()

	for {
		select {
		case <-ps.ticker.C:
			ps.counter++
			if ps.counter > maxPeriodicMessages {
				return
			}

			periodic := fmt.Sprintf("Server periodic message #%d\n", ps.counter)
			if _, err := ps.stream.Write([]byte(periodic)); err != nil {
				log.Printf("Failed to send periodic message: %v", err)
				return
			}

		case <-ps.stopCh:
			return
		}
	}
}

func (ps *PeriodicSender) stop() {
	select {
	case ps.stopCh <- true:
	default:
	}
}
