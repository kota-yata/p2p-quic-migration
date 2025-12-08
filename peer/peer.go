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
    config           *ServerConfig
    tlsConfig        *tls.Config
    quicConfig       *quic.Config
    transport        *quic.Transport
    udpConn          *net.UDPConn
    intermediateConn *quic.Conn
    // dedicated transport for intermediate server traffic
    serverTransport  *quic.Transport
    serverUDPConn    *net.UDPConn
    networkMonitor   *network_monitor.NetworkMonitor
    knownPeers       map[string]shared.PeerInfo
    audioRelayStop   func()
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
    if err := p.setupServerTransport(); err != nil {
        return fmt.Errorf("failed to setup server transport: %v", err)
    }
    defer p.cleanup()

    // Connect to the intermediate server using the dedicated server transport
    intermediateConn, err := ConnectToServer(p.config.serverAddr, p.tlsConfig, p.quicConfig, p.serverTransport)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")
	p.intermediateConn = intermediateConn
	WaitForObservedAddress(intermediateConn)

	// init peer discovery and handling
	p.knownPeers = make(map[string]shared.PeerInfo)
	go ManagePeerDiscovery(intermediateConn, p)

	// monitor established connections and coordinate cancellation/handling
	go p.monitorConnections()

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
		AddressDiscoveryMode: 1,
		KeepAlivePeriod:      30 * time.Second,
		MaxIdleTimeout:       5 * time.Minute,
	}

	return nil
}

func (p *Peer) setupTransport() error {
	var err error
	// Bind to all interfaces on a fixed port so the socket remains valid
	// across interface/IP changes. The kernel selects the correct source IP
	// per route, avoiding send failures when the primary interface changes.
	log.Printf("Binding UDP transport to local address: 0.0.0.0")
	p.udpConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: serverPort, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %v", err)
	}

	p.transport = &quic.Transport{
		Conn: p.udpConn,
	}

	return nil
}

func (p *Peer) setupServerTransport() error {
    // Use a dedicated UDP socket for the intermediate server traffic.
    // Bind to any address with an ephemeral port.
    udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
    if err != nil {
        return fmt.Errorf("failed to listen on UDP for server transport: %v", err)
    }
    p.serverUDPConn = udp
    p.serverTransport = &quic.Transport{Conn: udp}
    return nil
}

func (p *Peer) cleanup() {
    if p.transport != nil {
        p.transport.Close()
    }
    if p.udpConn != nil {
        p.udpConn.Close()
    }
    if p.serverTransport != nil {
        p.serverTransport.Close()
    }
    if p.serverUDPConn != nil {
        p.serverUDPConn.Close()
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
			log.Printf("Accept error: %v", err)
			continue
		}
		// notify accept event; monitor will cancel dialers and handle
		select {
		case connectionEstablished <- connchan{conn: conn, isAcceptor: true}:
		default:
			// if buffer is full, close the new conn to avoid leaks
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

func (s *Peer) onAddrChange(oldAddr, newAddr net.IP) {
	log.Printf("Handling network change from %s to %s", oldAddr, newAddr)

	if s.intermediateConn == nil {
		log.Println("No intermediate connection available for network change")
		return
	}
	// Migrate the server connection to a fresh UDP socket bound to the new
	// local address to avoid per-packet control data pointing at a stale IP.
	if err := s.migrateIntermediateConnection(newAddr); err != nil {
		log.Printf("Failed to migrate server connection: %v", err)
		return
	}
	log.Printf("Successfully migrated server connection to new address: %s", newAddr)

	if err := s.sendNetworkChangeNotification(oldAddr); err != nil {
		log.Printf("Failed to send server network change notification after migration: %v", err)
	}

	s.StartHolePunchingToAllPeers()
}

// monitorConnections waits for either acceptor or initiator connection events,
// cancels outstanding hole punching attempts, and hands off to the proper handler.
func (s *Peer) monitorConnections() {
	for evt := range connectionEstablished {
		// cancel any in-flight hole punching attempts
		for _, c := range s.hpCancels {
			c()
		}
		s.hpCancels = nil

		if evt.isAcceptor {
			s.handleIncomingConnection(evt.conn)
		} else {
			// Use remote addr for logging context
			peerAddr := evt.conn.RemoteAddr().String()
			handleCommunicationAsInitiator(evt.conn, peerAddr, s.config.role)
		}
	}
}

func (s *Peer) migrateIntermediateConnection(newAddr net.IP) error {
	if s.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

	// Bind a fresh UDP socket to the new local IP with an ephemeral port.
	// Using a different socket avoids stale ancillary data on the old path.
	newUDPConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: newAddr, Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection: %v", err)
	}

	newTransport := &quic.Transport{
		Conn: newUDPConn,
	}

	path, err := s.intermediateConn.AddPath(newTransport)
	if err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to add new path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

    // Wait for the connection to report the new local address, then retire
    // the old server transport/socket to avoid further sends on it.
    target := newUDPConn.LocalAddr().String()
    deadline := time.Now().Add(2 * time.Second)
    for time.Now().Before(deadline) {
        if s.intermediateConn.LocalAddr().String() == target {
            break
        }
        time.Sleep(50 * time.Millisecond)
    }

    // Close the old server transport/socket and replace references so cleanup works.
    if s.serverTransport != nil {
        s.serverTransport.Close()
    }
    if s.serverUDPConn != nil {
        s.serverUDPConn.Close()
    }
    s.serverTransport = newTransport
    s.serverUDPConn = newUDPConn

    return nil
}

// Send network change notification through the intermediate server
func (s *Peer) sendNetworkChangeNotification(oldAddr net.IP) error {
	if s.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is closed")
	}

    oldFullAddr := oldAddr.String() + ":0"
    // Report our P2P listener address (local socket bound for peer transport),
    // so logs remain coherent even though the server uses the observed host.
    newFullAddr := s.udpConn.LocalAddr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := s.intermediateConn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("failed to open stream: %v", err)
	}

	notification := fmt.Sprintf("NETWORK_CHANGE|%s|%s", oldFullAddr, newFullAddr)
	_, err = stream.Write([]byte(notification))
	if err != nil {
		return fmt.Errorf("failed to write notification: %v", err)
	}

	log.Printf("Sent server network change notification to intermediate server: %s -> %s", oldFullAddr, newFullAddr)
	return nil
}
