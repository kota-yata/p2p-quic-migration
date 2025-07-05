//go:build client
// +build client

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
	maxRetries             = 10
	maxHolePunchAttempts   = 10
	sessionTimeout         = 60 * time.Second
	holePunchDelay         = 2 * time.Second
	connectionTimeout      = 5 * time.Second
	holePunchTimeout       = 2 * time.Second
	communicationDuration  = 50 * time.Second
	maxMessageExchanges    = 10
)

var clientConnectionEstablished = make(chan bool, 1)

func main() {
	serverAddr := flag.String("serverAddr", "203.178.143.72:12345", "Address to the intermediary server")
	flag.Parse()

	client := &Client{
		serverAddr: *serverAddr,
		tlsConfig:  createTLSConfig(),
		quicConfig: createQUICConfig(),
	}

	if err := client.Run(); err != nil {
		log.Fatalf("Client failed: %v", err)
	}
}

type Client struct {
	serverAddr string
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	transport  *quic.Transport
	udpConn    *net.UDPConn
}

func createTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-quic"},
	}
}

func createQUICConfig() *quic.Config {
	return &quic.Config{
		AddressDiscoveryMode: 1,
	}
}

func (c *Client) Run() error {
	if err := c.setupTransport(); err != nil {
		return fmt.Errorf("failed to setup transport: %v", err)
	}
	defer c.cleanup()

	intermediateConn, err := c.connectToIntermediateServer()
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")

	c.waitForObservedAddress(intermediateConn)

	go c.managePeerDiscovery(intermediateConn)

	c.waitForSession()
	return nil
}

func (c *Client) setupTransport() error {
	var err error
	c.udpConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 1235, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %v", err)
	}

	c.transport = &quic.Transport{
		Conn: c.udpConn,
	}
	return nil
}

func (c *Client) cleanup() {
	if c.udpConn != nil {
		c.udpConn.Close()
	}
}

func (c *Client) connectToIntermediateServer() (*quic.Conn, error) {
	serverAddrResolved, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := c.transport.Dial(ctx, serverAddrResolved, c.tlsConfig, c.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to intermediate server: %v", err)
	}

	log.Printf("Connected to intermediate server at %s", c.serverAddr)
	return conn, nil
}

func (c *Client) waitForObservedAddress(conn *quic.Conn) {
	for i := 0; i < 10; i++ {
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address received: %s", observedAddr.String())
			break
		}
	}
}

func (c *Client) waitForSession() {
	log.Println("Waiting for peer discovery...")
	select {
	case <-time.After(sessionTimeout):
		log.Println("Client session timeout after 60 seconds")
	case <-context.Background().Done():
		log.Println("Context cancelled")
	}
}

func (c *Client) managePeerDiscovery(conn *quic.Conn) {
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open communication stream: %v", err)
		return
	}
	defer stream.Close()

	if err := c.sendPeerRequest(stream); err != nil {
		log.Printf("Failed to send peer request: %v", err)
		return
	}

	peerManager := &PeerManager{
		client:        c,
		hasConnected:  false,
		isFirstMessage: true,
	}

	peerManager.handlePeerCommunication(stream)
}

func (c *Client) sendPeerRequest(stream *quic.Stream) error {
	if _, err := stream.Write([]byte("GET_PEERS")); err != nil {
		return fmt.Errorf("failed to send GET_PEERS request: %v", err)
	}
	return nil
}

type PeerManager struct {
	client         *Client
	hasConnected   bool
	isFirstMessage bool
}

func (pm *PeerManager) handlePeerCommunication(stream *quic.Stream) {
	buffer := make([]byte, 4096)

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Failed to read from intermediate server: %v", err)
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
		go attemptNATHolePunch(*pm.client.transport, peer.Address, pm.client.tlsConfig, pm.client.quicConfig, clientConnectionEstablished)
	}

	if len(peers) > 0 && !pm.hasConnected {
		pm.connectToPeerWithDelay(peers[0].Address)
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
		go attemptNATHolePunch(*pm.client.transport, notification.Peer.Address, pm.client.tlsConfig, pm.client.quicConfig, clientConnectionEstablished)

		if !pm.hasConnected {
			pm.connectToPeerWithDelay(notification.Peer.Address)
		}
	}
}

func (pm *PeerManager) connectToPeerWithDelay(peerAddr string) {
	go func() {
		time.Sleep(holePunchDelay)
		connectToPeer(peerAddr, pm.client.transport, pm.client.tlsConfig, pm.client.quicConfig)
	}()
	pm.hasConnected = true
}

func connectToPeer(peerAddr string, tr *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	conn, err := establishConnection(peerAddr, tr, tlsConfig, quicConfig)
	if err != nil {
		log.Printf("Failed to establish connection to peer %s: %v", peerAddr, err)
		return
	}
	defer conn.CloseWithError(0, "")

	signalConnectionEstablished()

	if err := handlePeerCommunication(conn, peerAddr); err != nil {
		log.Printf("Peer communication failed: %v", err)
	}
}

func establishConnection(peerAddr string, tr *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
	log.Printf("Attempting to connect to peer at %s (will retry up to %d times)", peerAddr, maxRetries)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve peer address %s: %v", peerAddr, err)
	}

	var conn *quic.Conn
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("Connection attempt %d/%d to peer %s", attempt, maxRetries, peerAddr)

		ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
		conn, err = tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)
		cancel()

		if err == nil {
			log.Printf("Successfully connected to peer %s on attempt %d", peerAddr, attempt)
			return conn, nil
		}

		lastErr = err
		log.Printf("Connection attempt %d/%d to peer %s failed: %v", attempt, maxRetries, peerAddr, err)

		if attempt < maxRetries {
			backoffDuration := calculateBackoff(attempt)
			log.Printf("Waiting %v before retry attempt %d", backoffDuration, attempt+1)
			time.Sleep(backoffDuration)
		}
	}

	return nil, fmt.Errorf("all %d connection attempts failed. Last error: %v", maxRetries, lastErr)
}

func calculateBackoff(attempt int) time.Duration {
	backoffDuration := time.Duration(1<<uint(attempt-1)) * time.Second
	if backoffDuration > 30*time.Second {
		backoffDuration = 30 * time.Second
	}
	return backoffDuration
}

func signalConnectionEstablished() {
	select {
	case clientConnectionEstablished <- true:
		log.Printf("Connection established - signaling to stop hole punching")
	default:
		// Channel already has a value, which is fine
	}
}

func handlePeerCommunication(conn *quic.Conn, peerAddr string) error {
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open stream to peer %s: %v", peerAddr, err)
	}
	defer stream.Close()

	if err := sendInitialMessage(stream, peerAddr); err != nil {
		return err
	}

	if err := readInitialResponse(stream, peerAddr); err != nil {
		return err
	}

	go maintainPeerCommunication(stream, peerAddr)

	log.Printf("Starting continuous communication with peer %s", peerAddr)
	time.Sleep(communicationDuration)
	log.Printf("Peer connection to %s completed successfully!", peerAddr)

	return nil
}

func sendInitialMessage(stream *quic.Stream, peerAddr string) error {
	if _, err := stream.Write([]byte("Hello from client\n")); err != nil {
		return fmt.Errorf("failed to write to peer %s: %v", peerAddr, err)
	}
	return nil
}

func readInitialResponse(stream *quic.Stream, peerAddr string) error {
	response := make([]byte, 1024)
	n, err := stream.Read(response)
	if err != nil {
		return fmt.Errorf("failed to read from peer %s: %v", peerAddr, err)
	}

	log.Printf("Received from peer %s: %s", peerAddr, string(response[:n]))
	return nil
}

func attemptNATHolePunch(tr quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan bool) {
	log.Printf("Client starting NAT hole punching to peer: %s (will attempt %d times)", peerAddr, maxHolePunchAttempts)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s for NAT hole punch: %v", peerAddr, err)
		return
	}

	for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
		select {
		case <-stopChan:
			log.Printf("Stopping client NAT hole punch to %s - connection established", peerAddr)
			return
		default:
		}

		performHolePunchAttempt(tr, peerAddrResolved, tlsConfig, quicConfig, peerAddr, attempt)

		if attempt < maxHolePunchAttempts {
			waitBeforeNextAttempt(attempt)
		}
	}

	log.Printf("Client completed all %d NAT hole punch attempts to peer %s", maxHolePunchAttempts, peerAddr)
}

func performHolePunchAttempt(tr quic.Transport, peerAddrResolved *net.UDPAddr, tlsConfig *tls.Config, quicConfig *quic.Config, peerAddr string, attempt int) {
	log.Printf("Client NAT hole punch attempt %d/%d to peer %s", attempt, maxHolePunchAttempts, peerAddr)

	ctx, cancel := context.WithTimeout(context.Background(), holePunchTimeout)
	defer cancel()

	conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

	if err != nil {
		log.Printf("Client NAT hole punch attempt %d/%d to %s completed (connection failed, which is normal): %v", attempt, maxHolePunchAttempts, peerAddr, err)
	} else {
		conn.CloseWithError(0, "NAT hole punch complete")
		log.Printf("Client NAT hole punch attempt %d/%d to %s succeeded (unexpected but good!)", attempt, maxHolePunchAttempts, peerAddr)
	}
}

func waitBeforeNextAttempt(attempt int) {
	backoffDuration := time.Duration(attempt) * time.Second
	if backoffDuration > 5*time.Second {
		backoffDuration = 5 * time.Second
	}
	log.Printf("Client waiting %v before next hole punch attempt %d", backoffDuration, attempt+1)
	time.Sleep(backoffDuration)
}

func maintainPeerCommunication(stream *quic.Stream, peerAddr string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	counter := 0

	for range ticker.C {
		counter++
		message := fmt.Sprintf("Client message #%d to peer\n", counter)

		if _, err := stream.Write([]byte(message)); err != nil {
			log.Printf("Failed to send message to peer %s: %v", peerAddr, err)
			return
		}

		response := make([]byte, 1024)
		n, err := stream.Read(response)
		if err != nil {
			log.Printf("Failed to read response from peer %s: %v", peerAddr, err)
			return
		}

		log.Printf("Peer %s responded: %s", peerAddr, string(response[:n]))

		if counter >= maxMessageExchanges {
			log.Printf("Completed %d message exchanges with peer %s", counter, peerAddr)
			return
		}
	}
}