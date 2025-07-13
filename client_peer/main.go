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
	sessionTimeout        = 300 * time.Second
	holePunchDelay        = 2 * time.Second
	connectionTimeout     = 5 * time.Second
	holePunchTimeout      = 2 * time.Second
	communicationDuration = 300 * time.Second
	maxMessageExchanges   = 10
)

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
	serverAddr       string
	tlsConfig        *tls.Config
	quicConfig       *quic.Config
	transport        *quic.Transport
	udpConn          *net.UDPConn
	networkMonitor   *NetworkMonitor
	intermediateConn *quic.Conn
	peerHandler      *ClientPeerHandler
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
	c.intermediateConn = intermediateConn

	intermediateClient.WaitForObservedAddress(intermediateConn)

	c.networkMonitor = NewNetworkMonitor(c.handleNetworkChange)
	if err := c.networkMonitor.Start(); err != nil {
		return fmt.Errorf("failed to start network monitor: %v", err)
	}
	defer c.networkMonitor.Stop()

	peerHandler := NewClientPeerHandler(c.transport, c.tlsConfig, c.quicConfig)
	c.peerHandler = peerHandler
	go intermediateClient.ManagePeerDiscovery(intermediateConn, peerHandler)

	go c.handleIntermediateStreams(intermediateConn)

	c.waitForSession()
	return nil
}

func (c *Client) handleIntermediateStreams(conn *quic.Conn) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Failed to accept stream from intermediate server: %v", err)
			return
		}

		log.Printf("Accepted stream from intermediate server for audio relay")

		go func(s *quic.Stream) {
			defer func() {
				s.Close()
				log.Printf("Closed relayed audio stream from intermediate server")
			}()

			log.Printf("Starting to receive relayed audio from intermediate server")
			audioReceiver := NewAudioReceiver(s)
			if err := audioReceiver.ReceiveAudio(); err != nil {
				log.Printf("Failed to receive relayed audio stream: %v", err)
			} else {
				log.Printf("Relayed audio stream completed successfully")
			}
		}(stream)
	}
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
		log.Println("Client session timeout after 5 minutes")
	case <-context.Background().Done():
		log.Println("Context cancelled")
	}
}

func (c *Client) handleNetworkChange(oldAddr, newAddr string) {
	log.Printf("Handling network change from %s to %s", oldAddr, newAddr)

	if c.intermediateConn == nil {
		log.Println("No intermediate connection available for network change")
		return
	}

	if err := c.migrateConnection(newAddr); err != nil {
		log.Printf("Failed to migrate connection: %v", err)
		return
	}

	log.Printf("Successfully migrated connection to new address: %s", newAddr)

	if c.peerHandler != nil {
		c.peerHandler.StartHolePunchingToAllPeers(c.transport, c.tlsConfig, c.quicConfig)
	}

	if err := c.sendNetworkChangeNotification(oldAddr, newAddr); err != nil {
		log.Printf("Failed to send network change notification after migration: %v", err)
	}
}

func (c *Client) migrateConnection(newAddr string) error {
	if c.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

	newUDPConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(newAddr), Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection: %v", err)
	}

	newTransport := &quic.Transport{
		Conn: newUDPConn,
	}

	path, err := c.intermediateConn.AddPath(newTransport)
	if err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to add new path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	log.Printf("Probing new path from %s to intermediate server", newAddr)

	var probeErr error
	for attempt := 1; attempt <= 3; attempt++ {
		probeErr = path.Probe(ctx)
		if probeErr == nil {
			log.Printf("Path probing succeeded on attempt %d", attempt)
			break
		}

		log.Printf("Path probing attempt %d failed: %v", attempt, probeErr)
		if attempt < 3 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	if probeErr != nil {
		newUDPConn.Close()
		return fmt.Errorf("path probing failed after 3 attempts: %v", probeErr)
	}

	log.Printf("Switching to new path")
	if err := path.Switch(); err != nil {
		if closeErr := path.Close(); closeErr != nil {
			log.Printf("Warning: failed to close path after switch failure: %v", closeErr)
		}
		newUDPConn.Close()
		return fmt.Errorf("failed to switch to new path: %v", err)
	}

	// Store old transport and UDP connection for cleanup after successful migration
	// oldTransport := c.transport
	// oldUDPConn := c.udpConn

	// Update the client's transport and UDP connection
	c.transport = newTransport
	c.udpConn = newUDPConn

	// Clean up old transport and connection after a delay to allow migration to complete
	// go func() {
	// 	time.Sleep(1 * time.Second)
	// 	if oldTransport != nil {
	// 		oldTransport.Close()
	// 	}
	// 	if oldUDPConn != nil {
	// 		oldUDPConn.Close()
	// 	}
	// }()

	return nil
}

func (c *Client) sendNetworkChangeNotification(oldAddr, newAddr string) error {
	if c.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is closed")
	}

	oldFullAddr := oldAddr + ":0"
	newFullAddr := c.intermediateConn.LocalAddr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := c.intermediateConn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("failed to open stream: %v", err)
	}

	notification := fmt.Sprintf("NETWORK_CHANGE|%s|%s", oldFullAddr, newFullAddr)
	_, err = stream.Write([]byte(notification))
	if err != nil {
		return fmt.Errorf("failed to write notification: %v", err)
	}

	log.Printf("Sent network change notification to intermediate server: %s -> %s", oldFullAddr, newFullAddr)
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
		log.Printf("Client NAT hole punch attempt %d/%d to %s succeeded - waiting for server to open stream!", attempt, maxHolePunchAttempts, peerAddr)

		go func() {
			defer conn.CloseWithError(0, "")

			stream, err := conn.AcceptStream(context.Background())
			if err != nil {
				log.Printf("Failed to accept stream from server: %v", err)
				return
			}

			log.Printf("Accepted stream from server, starting to receive audio stream from peer %s", peerAddr)

			audioReceiver := NewAudioReceiver(stream)
			if err := audioReceiver.ReceiveAudio(); err != nil {
				log.Printf("Failed to receive audio stream: %v", err)
				return
			}

			log.Printf("Audio reception from %s completed successfully!", peerAddr)
		}()

		select {
		case holePunchCompleted <- true:
			log.Printf("Signaled hole punch completion for %s", peerAddr)
		default:
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
