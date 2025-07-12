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
	maxRetries            = 10
	maxHolePunchAttempts  = 10
	sessionTimeout        = 60 * time.Second
	holePunchDelay        = 2 * time.Second
	connectionTimeout     = 5 * time.Second
	holePunchTimeout      = 2 * time.Second
	communicationDuration = 50 * time.Second
	maxMessageExchanges   = 10
)

var clientConnectionEstablished = make(chan bool, 1)
var holePunchCompleted = make(chan bool, 1)

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

	intermediateClient := intermediate.NewClient(c.serverAddr, c.tlsConfig, c.quicConfig, c.transport)

	intermediateConn, err := intermediateClient.ConnectToServer()
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")

	intermediateClient.WaitForObservedAddress(intermediateConn)

	peerHandler := NewClientPeerHandler(c.transport, c.tlsConfig, c.quicConfig)
	go intermediateClient.ManagePeerDiscovery(intermediateConn, peerHandler)

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

func (c *Client) waitForSession() {
	log.Println("Waiting for peer discovery...")
	select {
	case <-time.After(sessionTimeout):
		log.Println("Client session timeout after 60 seconds")
	case <-context.Background().Done():
		log.Println("Context cancelled")
	}
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

	log.Printf("Starting to receive video stream from peer %s", peerAddr)

	videoReceiver := NewVideoReceiver(stream)
	if err := videoReceiver.ReceiveVideo(); err != nil {
		return fmt.Errorf("failed to receive video stream: %v", err)
	}

	log.Printf("Video reception from %s completed successfully!", peerAddr)
	return nil
}

func attemptNATHolePunch(tr *quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan bool) {
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

func performHolePunchAttempt(tr *quic.Transport, peerAddrResolved *net.UDPAddr, tlsConfig *tls.Config, quicConfig *quic.Config, peerAddr string, attempt int) {
	log.Printf("Client NAT hole punch attempt %d/%d to peer %s", attempt, maxHolePunchAttempts, peerAddr)

	ctx, cancel := context.WithTimeout(context.Background(), holePunchTimeout)
	defer cancel()

	conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

	if err != nil {
		log.Printf("Client NAT hole punch attempt %d/%d to %s completed (connection failed, which is normal): %v", attempt, maxHolePunchAttempts, peerAddr, err)
	} else {
		log.Printf("Client NAT hole punch attempt %d/%d to %s succeeded - opening stream to server!", attempt, maxHolePunchAttempts, peerAddr)

		// Use this successful connection for video receiving - open stream to server
		go func() {
			defer conn.CloseWithError(0, "")

			stream, err := conn.OpenStreamSync(context.Background())
			if err != nil {
				log.Printf("Failed to open stream to server: %v", err)
				return
			}

			log.Printf("Opened stream to server, starting to receive video stream from peer %s", peerAddr)

			videoReceiver := NewVideoReceiver(stream)
			if err := videoReceiver.ReceiveVideo(); err != nil {
				log.Printf("Failed to receive video stream: %v", err)
				return
			}

			log.Printf("Video reception from %s completed successfully!", peerAddr)
		}()

		// Signal that hole punching completed successfully
		select {
		case holePunchCompleted <- true:
			log.Printf("Signaled hole punch completion for %s", peerAddr)
		default:
			// Channel already has a value, which is fine
		}
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
