package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
	proto "github.com/kota-yata/p2p-quic-migration/shared/cmp9protocol"
	"github.com/quic-go/quic-go"
)

type ServerConfig struct {
	keyFile    string
	certFile   string
	serverAddr string
	relayAddr  string
	role       string
}

type Peer struct {
	config                *ServerConfig
	tlsConfig             *tls.Config
	quicConfig            *quic.Config
	intermediateTransport *quic.Transport
	intermediateUdpConn   *net.UDPConn
	intermediateConn      *quic.Conn
	intermediateStream    *quic.Stream
	relayTransport        *quic.Transport
	relayUdpConn          *net.UDPConn
	relayConn             *quic.Conn
	relayAllowStream      *quic.Stream
	networkMonitor        *network_monitor.NetworkMonitor
	knownPeers            map[uint32]string
	audioRelayStop        func()
	// hole punch cancellation management
	hpCancels []context.CancelFunc
}

// openRelayAllowStream opens (or reopens) the control stream used to send allowlist updates.
func (p *Peer) openRelayAllowStream() error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection available")
	}
	if p.relayAllowStream != nil {
		return nil
	}
	s, err := p.relayConn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	p.relayAllowStream = s
	return nil
}

// sendRelayAllowlistUpdate sends the current known peers as allowlist to the relay.
func (p *Peer) sendRelayAllowlistUpdate() error {
	if err := p.openRelayAllowStream(); err != nil {
		return err
	}
	addrs := make([]proto.Address, 0, len(p.knownPeers))
	for _, addrStr := range p.knownPeers {
		host, portStr, err := net.SplitHostPort(addrStr)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			continue
		}
		var portNum uint16
		if pn, err := strconv.Atoi(portStr); err == nil {
			portNum = uint16(pn)
		} else if p2, err := net.LookupPort("udp", portStr); err == nil {
			portNum = uint16(p2)
		} else {
			continue
		}
		if ip.To4() != nil {
			addrs = append(addrs, proto.Address{AF: 0x04, IP: ip.To4(), Port: portNum})
		} else {
			addrs = append(addrs, proto.Address{AF: 0x06, IP: ip.To16(), Port: portNum})
		}
	}
	msg := proto.RelayAllowlistSet{Addresses: addrs}
	if err := proto.WriteMessage(p.relayAllowStream, msg); err != nil {
		return err
	}
	return nil
}

func (p *Peer) Run() error {
	if err := p.setupTLS(); err != nil {
		return fmt.Errorf("failed to setup TLS: %v", err)
	}

	p.networkMonitor = network_monitor.NewNetworkMonitor(p.onAddrChange)
	if err := p.networkMonitor.Start(); err != nil {
		return fmt.Errorf("failed to start network monitor: %v", err)
	}

	if err := p.setupTransport(); err != nil {
		return fmt.Errorf("failed to setup transport: %v", err)
	}
	defer p.cleanup()

	// Connect to the intermediate (signaling) server
	intermediateConn, err := ConnectToServer(p.config.serverAddr, p.tlsConfig, p.quicConfig, p.intermediateTransport)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")
	p.intermediateConn = intermediateConn

	// Connect to the relay server
	relayConn, err := ConnectToServer(p.config.relayAddr, p.tlsConfig, p.quicConfig, p.relayTransport)
	if err != nil {
		return fmt.Errorf("failed to connect to relay server: %v", err)
	}
	defer relayConn.CloseWithError(0, "")
	p.relayConn = relayConn

	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("warning: failed to send initial relay allowlist: %v", err)
	}

	// init peer discovery and handling
	p.knownPeers = make(map[uint32]string)
	stream, err := intermediateConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open peer discovery stream: %v", err)
	}
	defer stream.Close()
	p.intermediateStream = stream
	go IntermediateControlReadLoop(intermediateConn, p, stream)
	// Accept additional streams from the intermediate (e.g., audio relay)
	go p.acceptRelayStreams()

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
	p.intermediateUdpConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: peerPort, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP for intermediate: %v", err)
	}
	p.relayUdpConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 0, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP for relay: %v", err)
	}

	p.intermediateTransport = &quic.Transport{
		Conn: p.intermediateUdpConn,
	}
	p.relayTransport = &quic.Transport{
		Conn: p.relayUdpConn,
	}

	return nil
}

func (p *Peer) cleanup() {
	log.Printf("Cleaning up resources...")
	if p.intermediateTransport != nil {
		p.intermediateTransport.Close()
	}
	if p.relayTransport != nil {
		p.relayTransport.Close()
	}
	if p.relayUdpConn != nil {
		p.relayUdpConn.Close()
	}
	if p.intermediateUdpConn != nil {
		p.intermediateUdpConn.Close()
	}
	if p.intermediateConn != nil {
		p.intermediateConn.CloseWithError(0, "")
	}
	if p.networkMonitor != nil {
		p.networkMonitor.Stop()
	}
}

func (p *Peer) runPeerListener() error {
	ln, err := p.intermediateTransport.Listen(p.tlsConfig, p.quicConfig)
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
			ln, err = p.intermediateTransport.Listen(p.tlsConfig, p.quicConfig)
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

func (p *Peer) handleInitialPeers(entries []proto.PeerEntry) {
	for _, e := range entries {
		addr := net.JoinHostPort(e.Address.IP.String(), fmt.Sprintf("%d", e.Address.Port))
		p.knownPeers[e.PeerID] = addr
		p.startHolePunching(addr)
	}
	// Update relay allow list with current known peers
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send relay allowlist after initial peers: %v", err)
	}
}

func (p *Peer) handleNewPeer(entry proto.PeerEntry) {
	addr := net.JoinHostPort(entry.Address.IP.String(), fmt.Sprintf("%d", entry.Address.Port))
	p.knownPeers[entry.PeerID] = addr
	p.startHolePunching(addr)
	// Update relay allow list to include this peer
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send relay allowlist after new peer: %v", err)
	}
}

func (p *Peer) startHolePunching(peerAddr string) {
	// create a cancelable context for this punching attempt
	ctx, cancel := context.WithCancel(context.Background())
	// record cancel so an acceptor success can stop dial attempts
	p.hpCancels = append(p.hpCancels, cancel)
	go attemptNATHolePunch(ctx, p.intermediateTransport, peerAddr, p.tlsConfig, p.quicConfig, connectionEstablished)
}

func (p *Peer) StopAudioRelay() {
	if p.audioRelayStop != nil {
		log.Printf("Stopping audio relay due to P2P reconnection")
		p.audioRelayStop()
		p.audioRelayStop = nil
	}
}

func (p *Peer) HandleNetworkChange(peerID uint32, oldAddr, newAddr string) {
	log.Printf("Network change notification from server: peer %d changed from %s to %s", peerID, oldAddr, newAddr)

	// Update local cache of peer address for allow listing and hole punching
	p.knownPeers[peerID] = newAddr

	// Send updated allow list to relay so it will accept from the new address
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send updated relay allowlist: %v", err)
	}

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

	for peerID, addr := range p.knownPeers {
		log.Printf("Server starting hole punch to peer %d at %s", peerID, addr)
		ctx, cancel := context.WithCancel(context.Background())
		p.hpCancels = append(p.hpCancels, cancel)
		go attemptNATHolePunch(ctx, p.intermediateTransport, addr, p.tlsConfig, p.quicConfig, connectionEstablished)
	}
}

func (p *Peer) switchToAudioRelay(targetPeerID uint32) error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection available")
	}

	stream, err := p.relayConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open audio relay stream: %v", err)
	}

	// Send AUDIO_RELAY_REQ as the first framed message on this fresh stream
	if err := proto.WriteMessage(stream, proto.AudioRelayReq{TargetPeerID: targetPeerID}); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send audio relay request: %v", err)
	}

	log.Printf("Switched to audio relay mode for peer %d", targetPeerID)

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

	// Migrate relay connection to the new local path as well
	if err := p.migrateRelayConnection(newAddr); err != nil {
		log.Printf("Failed to migrate relay connection: %v", err)
	} else {
		log.Printf("Successfully migrated relay connection to new address: %s", newAddr)
	}

	if err := p.sendNetworkChangeNotification(oldAddr); err != nil {
		log.Printf("Failed to send server network change notification after migration: %v", err)
		return
	}

	// Update relay allow list for our peers as our own source address changed from relay's perspective
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to refresh relay allowlist after our migration: %v", err)
	}
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
	// Force IPv4 UDP to avoid AF mismatch issues during migration
	newUDPConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: newAddr, Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection: %v", err)
	}

	newTransport := &quic.Transport{
		Conn: newUDPConn,
	}

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
	p.intermediateTransport = newTransport
	p.intermediateUdpConn = newUDPConn

	return nil
}

func (p *Peer) migrateRelayConnection(newAddr net.IP) error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection to migrate")
	}
	if p.relayConn.Context().Err() != nil {
		return fmt.Errorf("relay connection is closed, cannot migrate")
	}
	// Create a dedicated UDP socket and transport for the relay path
	newUDPConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: newAddr, Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection for relay: %v", err)
	}
	newTransport := &quic.Transport{Conn: newUDPConn}
	path, err := p.relayConn.AddPath(newTransport)
	if err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to add new path to relay: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Printf("Probing new path from %s to relay server", newAddr)
	if err := path.Probe(ctx); err != nil {
		_ = path.Close()
		newUDPConn.Close()
		return fmt.Errorf("failed to probe new relay path: %v", err)
	}
	if err := path.Switch(); err != nil {
		_ = path.Close()
		newUDPConn.Close()
		return fmt.Errorf("failed to switch to new relay path: %v", err)
	}
	p.relayTransport = newTransport
	p.relayUdpConn = newUDPConn

	return nil
}

// Send network change notification through the intermediate server
func (p *Peer) sendNetworkChangeNotification(oldAddr net.IP) error {
	if p.intermediateConn.Context().Err() != nil {
		return p.intermediateConn.Context().Err()
	}

	// Build payload with old address only; server uses observed remote for new.
	addr := proto.Address{}
	if oldAddr.To4() != nil {
		addr.AF = 0x04
		addr.IP = oldAddr.To4()
		addr.Port = 0
	} else {
		addr.AF = 0x06
		addr.IP = oldAddr.To16()
		addr.Port = 0
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		log.Printf("Attempting to send network change notification (attempt %d)...", attempt)
		if err := proto.WriteMessage(p.intermediateStream, proto.NetworkChangeReq{OldAddress: addr}); err != nil {
			lastErr = fmt.Errorf("write attempt %d failed: %w", attempt, err)
			_ = p.intermediateStream.Close()
		} else {
			log.Printf("Sent server network change notification to intermediate server (attempt %d)", attempt)
			return nil
		}

		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}

	return lastErr
}

// acceptIntermediateStreams accepts additional audio streams initiated by the
func (p *Peer) acceptRelayStreams() {
	if p.relayConn == nil {
		return
	}
	for {
		stream, err := p.relayConn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Error accepting stream from relay: %v", err)
			return
		}
		log.Printf("Accepted incoming relay stream from relay server")
		go handleIncomingAudioStream(stream, "relay")
	}
}
