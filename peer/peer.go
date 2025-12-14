package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

type ServerConfig struct {
	keyFile    string
	certFile   string
	serverAddr string
	role       string
}

type Peer struct {
	config             *ServerConfig
	tlsConfig          *tls.Config
	quicConfig         *quic.Config
	transport          *quic.Transport
	udpConn            *net.UDPConn
	intermediateConn   *quic.Conn
	intermediateStream *quic.Stream
	networkMonitor     *network_monitor.NetworkMonitor
	knownPeers         map[string]shared.PeerInfo
	audioRelayStop     func()
	// hole punch cancellation management
	hpCancels []context.CancelFunc
}

func (p *Peer) Run() error {
	if err := p.setupTLS(); err != nil {
		return fmt.Errorf("failed to setup TLS: %v", err)
	}
	p.networkMonitor = network_monitor.NewNetworkMonitor(p.onAddrChange)
	if err := p.networkMonitor.Start(); err != nil {
		return fmt.Errorf("failed to start network monitor: %v", err)
	}
	defer p.networkMonitor.Stop()

	if err := p.setupTransport(); err != nil {
		return fmt.Errorf("failed to setup transport: %v", err)
	}
	defer p.cleanup()

	// Connect to the intermediate server
	intermediateConn, err := ConnectToServer(p.config.serverAddr, p.tlsConfig, p.quicConfig, p.transport)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")
	p.intermediateConn = intermediateConn

	// init peer discovery and handling
	p.knownPeers = make(map[string]shared.PeerInfo)
	stream, err := intermediateConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open peer discovery stream: %v", err)
	}
	defer stream.Close()
	p.intermediateStream = stream
	go IntermediateReadLoop(intermediateConn, p, stream)

	// monitor established connections and coordinate cancellation/handling
	go p.monitorHolepunch()

	return p.runPeerListener()
}

func (p *Peer) setupTLS() error {
	cer, err := tls.LoadX509KeyPair(p.config.certFile, p.config.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load certificate: %v", err)
	}

	p.tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cer},
		NextProtos:         []string{"p2p-quic"},
	}

	p.quicConfig = &quic.Config{
		KeepAlivePeriod: 30 * time.Second,
		MaxIdleTimeout:  5 * time.Minute,
	}

	return nil
}

func (p *Peer) setupTransport() error {
	var err error
	currentAddr, err := p.networkMonitor.GetCurrentAddress()
	if err != nil {
		return fmt.Errorf("failed to get current network address: %v", err)
	}

	log.Printf("Binding UDP transport to local address: %s", currentAddr.String())
	p.udpConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: peerPort, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %v", err)
	}

	p.transport = &quic.Transport{
		Conn: p.udpConn,
	}

	return nil
}

func (p *Peer) cleanup() {
	log.Printf("Cleaning up resources...")
	if p.transport != nil {
		p.transport.Close()
	}
	if p.udpConn != nil {
		p.udpConn.Close()
	}
	if p.intermediateConn != nil {
		p.intermediateConn.CloseWithError(0, "")
	}
	if p.networkMonitor != nil {
		p.networkMonitor.Stop()
	}
}

func (p *Peer) runPeerListener() error {
	ln, err := p.transport.Listen(p.tlsConfig, p.quicConfig)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	defer ln.Close()

	log.Printf("Start peer listener on %s", ln.Addr().String())

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Printf("Accept error possibly due to connection migration: %v", err)
			// try to re-listen on the new transport
			ln, err = p.transport.Listen(p.tlsConfig, p.quicConfig)
			if err != nil {
				return fmt.Errorf("failed to re-listen after accept error: %v", err)
			}
			log.Printf("Re-listened on new transport at %s", ln.Addr().String())
			continue
		}
		// notify accept event; monitor will cancel dialers and handle
		select {
		case connectionEstablished <- connchan{conn: conn, isAcceptor: true}:
		default:
			// if buffer is full, close the new conn
			log.Printf("connectionEstablished channel full; dropping accept notification")
			conn.CloseWithError(0, "dropped: channel full")
		}
	}
}

func (p *Peer) handleIncomingConnection(conn *quic.Conn) {
	log.Print("New Peer Connection Accepted. Setting up audio streaming...")

	// stop any relay now that direct P2P is up
	p.StopAudioRelay()

	// Since we received the connection, we act as the "acceptor"
	log.Printf("Acting as connection acceptor with role=%s", p.config.role)
	handleCommunicationAsAcceptor(conn, p.config.role)
}

func (p *Peer) handleInitialPeers(peers []shared.PeerInfo) {
	for _, peer := range peers {
		if p.knownPeers != nil {
			p.knownPeers[peer.ID] = peer
		}
		p.startHolePunching(peer.Address)
	}
}

func (p *Peer) handleNewPeer(peer shared.PeerInfo) {
	if p.knownPeers != nil {
		p.knownPeers[peer.ID] = peer
	}
	p.startHolePunching(peer.Address)
}

func (p *Peer) startHolePunching(peerAddr string) {
	// create a cancelable context for this punching attempt
	ctx, cancel := context.WithCancel(context.Background())
	// record cancel so an acceptor success can stop dial attempts
	p.hpCancels = append(p.hpCancels, cancel)
	go attemptNATHolePunch(ctx, p.transport, peerAddr, p.tlsConfig, p.quicConfig, connectionEstablished)
}

func (p *Peer) StopAudioRelay() {
	if p.audioRelayStop != nil {
		log.Printf("Stopping audio relay due to P2P reconnection")
		p.audioRelayStop()
		p.audioRelayStop = nil
	}
}

func (p *Peer) HandleNetworkChange(peerID, oldAddr, newAddr string) {
	log.Printf("Network change notification from server: peer %s changed from %s to %s", peerID, oldAddr, newAddr)

	// Only sender/both should stream via relay while reconnecting
	if p.config != nil && (p.config.role == "sender" || p.config.role == "both") {
		if err := p.switchToAudioRelay(peerID); err != nil {
			log.Printf("Failed to switch to audio relay: %v", err)
			return
		}
	} else {
		log.Printf("Role=%s; skipping audio relay during migration", p.config.role)
	}

	log.Printf("Starting new hole punching to updated address: %s", newAddr)
	p.startHolePunching(newAddr)
}

func (p *Peer) StartHolePunchingToAllPeers() {
	log.Printf("Server starting hole punching to all known peers after network change")

	if len(p.knownPeers) == 0 {
		log.Printf("No known peers to hole punch to")
		return
	}

	for peerID, peer := range p.knownPeers {
		log.Printf("Server starting hole punch to peer %s at %s", peerID, peer.Address)
		ctx, cancel := context.WithCancel(context.Background())
		p.hpCancels = append(p.hpCancels, cancel)
		go attemptNATHolePunch(ctx, p.transport, peer.Address, p.tlsConfig, p.quicConfig, connectionEstablished)
	}
}

func (p *Peer) switchToAudioRelay(targetPeerID string) error {
	if p.intermediateConn == nil {
		return fmt.Errorf("no intermediate connection available")
	}

	stream, err := p.intermediateConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open audio relay stream: %v", err)
	}

	relayRequest := fmt.Sprintf("AUDIO_RELAY|%s", targetPeerID)
	if _, err := stream.Write([]byte(relayRequest)); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send audio relay request: %v", err)
	}

	log.Printf("Switched to audio relay mode for peer %s", targetPeerID)

	// start relay and store stopper
	p.audioRelayStop = startAudioRelay(stream, targetPeerID)
	return nil
}

func (p *Peer) onAddrChange(oldAddr, newAddr net.IP) {
	log.Printf("Handling network change from %s to %s", oldAddr, newAddr)

	if p.intermediateConn == nil {
		log.Println("No intermediate connection available for network change")
		return
	}

	if err := p.migrateIntermediateConnection(newAddr); err != nil {
		log.Printf("Failed to migrate server connection: %v", err)
		return
	}

	log.Printf("Successfully migrated server connection to new address: %s", newAddr)

	if err := p.sendNetworkChangeNotification(oldAddr); err != nil {
		log.Printf("Failed to send server network change notification after migration: %v", err)
		return
	}

	p.StartHolePunchingToAllPeers()
}

// monitorHolepunch waits for either acceptor or initiator connection events,
// cancels outstanding hole punching attempts, and hands off to the proper handler.
func (p *Peer) monitorHolepunch() {
	for evt := range connectionEstablished {
		// cancel any in-flight hole punching attempts
		for _, c := range p.hpCancels {
			c()
		}
		p.hpCancels = nil

		if evt.isAcceptor {
			p.handleIncomingConnection(evt.conn)
		} else {
			// Use remote addr for logging context
			peerAddr := evt.conn.RemoteAddr().String()
			handleCommunicationAsInitiator(evt.conn, peerAddr, p.config.role)
		}
	}
}

func (p *Peer) migrateIntermediateConnection(newAddr net.IP) error {
	if p.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

	// Bind to an ephemeral port on the new address to avoid reusing the
	// original fixed port, which can confuse NATs after interface changes.
	newUDPConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: newAddr, Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection: %v", err)
	}

	newTransport := &quic.Transport{
		Conn: newUDPConn,
	}

	// Keep references to old transport/socket (do not close immediately).
	// Closing the original Transport can cancel the connection's context
	// even after a successful path switch. We intentionally keep it alive.
	// These may be cleaned up on process shutdown.
	// oldTransport := p.transport
	// oldUDPConn := p.udpConn

	path, err := p.intermediateConn.AddPath(newTransport)
	if err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to add new path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Printf("Probing new path from %s to intermediate server", newAddr)
	if err := path.Probe(ctx); err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to probe new path: %v", err)
	} else {
		log.Printf("Path probing succeeded")
	}

	log.Printf("Switching to new path")
	if err := path.Switch(); err != nil {
		if closeErr := path.Close(); closeErr != nil {
			log.Printf("Warning: failed to close path after switch failure: %v", closeErr)
		}
		newUDPConn.Close()
		return fmt.Errorf("failed to switch to new path: %v", err)
	}

	// Update the server's transport and UDP connection used for outgoing connections
	p.transport = newTransport
	p.udpConn = newUDPConn

	// Note: Do NOT close the old transport or UDP socket here. The connection
	// is still owned by the original transport, and closing it may cancel the
	// connection's context. We prefer to leak these resources until shutdown
	// rather than break the migrated connection.

	return nil
}

// Send network change notification through the intermediate server
func (p *Peer) sendNetworkChangeNotification(oldAddr net.IP) error {
	if p.intermediateConn.Context().Err() != nil {
		return p.intermediateConn.Context().Err()
	}

	oldFullAddr := oldAddr.String() + ":0"
	// TODO: Remove new address report because the intermediate server won't use this anyway
	// The server looks at the source address of the incoming connection, not newAddr in the payload
	newFullAddr := p.intermediateConn.LocalAddr().String()

	// Try a few times with short backoff in case the new path isn't
	// immediately ready for app data after switching.
	notification := fmt.Sprintf("NETWORK_CHANGE|%s|%s", oldFullAddr, newFullAddr)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		log.Printf("Attempting to send network change notification (attempt %d)...", attempt)
		if _, err := p.intermediateStream.Write([]byte(notification)); err != nil {
			lastErr = fmt.Errorf("write attempt %d failed: %w", attempt, err)
			_ = p.intermediateStream.Close()
		} else {
			log.Printf("Sent server network change notification to intermediate server: %s -> %s (attempt %d)", oldFullAddr, newFullAddr, attempt)
			// _ = p.intermediateStream.Close()
			return nil
		}

		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}

	return lastErr
}
