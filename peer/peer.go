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
	relayAllowStream      *quic.Stream
	networkMonitor        *network_monitor.NetworkMonitor
	endpoints             map[uint32]endpointInfo
	ownObservedIP         net.IP
	audioRelayStop        func()
	// hole punch cancellation management
	hpCancels []context.CancelFunc
}

type endpointInfo struct {
	observed string
	local    string
	hasLocal bool
}

// openRelayAllowStream opens (or reopens) the control stream used to send allowlist updates.
func (p *Peer) openRelayAllowStream() error {
	if p.intermediateConn == nil {
		return fmt.Errorf("no server connection available")
	}
	if p.relayAllowStream != nil {
		return nil
	}
	s, err := p.intermediateConn.OpenStreamSync(context.Background())
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
	// Build allowlist based on initial phase strategy: include local+observed when in same segment
	addrs := make([]proto.Address, 0, len(p.endpoints)*2)
	for _, ep := range p.endpoints {
		// Always include observed
		if a, ok := toProtoAddr(ep.observed); ok {
			addrs = append(addrs, a)
		}
		// Include local if present and same segment (compare observed IPs)
		if ep.hasLocal && p.ownObservedIP != nil {
			// Determine peer observed IP
			if host, _, err := net.SplitHostPort(ep.observed); err == nil {
				peerObsIP := net.ParseIP(host)
				if sameIPFamily(peerObsIP, p.ownObservedIP) && ipEqual(peerObsIP, p.ownObservedIP) {
					if a, ok := toProtoAddr(ep.local); ok {
						addrs = append(addrs, a)
					}
				}
			}
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

	// init peer discovery and handling
	p.endpoints = make(map[uint32]endpointInfo)
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
	p.intermediateUdpConn, err = net.ListenUDP("udp", &net.UDPAddr{Port: peerPort, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP for intermediate: %v", err)
	}

	p.intermediateTransport = &quic.Transport{
		Conn: p.intermediateUdpConn,
	}
	// No separate relay transport; relay uses intermediate transport

	return nil
}

func (p *Peer) cleanup() {
	log.Printf("Cleaning up resources...")
	if p.intermediateTransport != nil {
		p.intermediateTransport.Close()
	}
	// No separate relay transport to close
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

// initial endpoints received from server
func (p *Peer) handleInitialEndpoints(entries []proto.PeerEndpoint) {
	for _, e := range entries {
		peerObs := net.JoinHostPort(e.Observed.IP.String(), fmt.Sprintf("%d", e.Observed.Port))
		var peerLoc string
		if e.Flags&0x01 != 0 {
			peerLoc = net.JoinHostPort(e.Local.IP.String(), fmt.Sprintf("%d", e.Local.Port))
		}
		p.endpoints[e.PeerID] = endpointInfo{observed: peerObs, local: peerLoc, hasLocal: e.Flags&0x01 != 0}
		candidates := p.buildCandidates(peerObs, peerLoc, e.Flags&0x01 != 0)
		p.startHolePunchingCandidates(candidates)
	}
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send relay allowlist after initial endpoints: %v", err)
	}
}

func (p *Peer) handleNewEndpoint(e proto.PeerEndpoint) {
	peerObs := net.JoinHostPort(e.Observed.IP.String(), fmt.Sprintf("%d", e.Observed.Port))
	var peerLoc string
	if e.Flags&0x01 != 0 {
		peerLoc = net.JoinHostPort(e.Local.IP.String(), fmt.Sprintf("%d", e.Local.Port))
	}
	p.endpoints[e.PeerID] = endpointInfo{observed: peerObs, local: peerLoc, hasLocal: e.Flags&0x01 != 0}
	candidates := p.buildCandidates(peerObs, peerLoc, e.Flags&0x01 != 0)
	p.startHolePunchingCandidates(candidates)
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send relay allowlist after new endpoint: %v", err)
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

	// Update peer observed address; clear local for post-initial behavior
	ei := p.endpoints[peerID]
	ei.observed = newAddr
	ei.local = ""
	ei.hasLocal = false
	p.endpoints[peerID] = ei

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

	log.Printf("Starting new hole punching to updated address (observed): %s", newAddr)
	p.startHolePunchingCandidates([]string{newAddr})
}

func (p *Peer) StartHolePunchingToAllPeers() {
	log.Printf("Server starting hole punching to all known peers after network change")

	if len(p.endpoints) == 0 {
		log.Printf("No known peers to hole punch to")
		return
	}

	for peerID, ep := range p.endpoints {
		log.Printf("Server starting hole punch to peer %d", peerID)
		candidates := p.buildCandidates(ep.observed, ep.local, ep.hasLocal)
		ctx, cancel := context.WithCancel(context.Background())
		p.hpCancels = append(p.hpCancels, cancel)
		go attemptPrioritizedHolePunch(ctx, p.intermediateTransport, candidates, p.tlsConfig, p.quicConfig, connectionEstablished)
	}
}

func (p *Peer) switchToAudioRelay(targetPeerID uint32) error {
	if p.intermediateConn == nil {
		return fmt.Errorf("no server connection available for relay")
	}

	stream, err := p.intermediateConn.OpenStreamSync(context.Background())
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

	newUDPConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
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

	log.Printf("Probing new path to intermediate server (local %s)", newUDPConn.LocalAddr().String())
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

// Relay connection migration removed: relay is handled by the intermediate server connection.

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
	if err := proto.WriteMessage(p.intermediateStream, proto.NetworkChangeReq{OldAddress: addr}); err != nil {
		return fmt.Errorf("write attempt failed: %w", err)
	}
	log.Printf("Sent server network change notification to intermediate server")
	return nil
}

// acceptIntermediateStreams accepts additional audio streams initiated by the
func (p *Peer) acceptRelayStreams() {
	if p.intermediateConn == nil {
		return
	}
	for {
		stream, err := p.intermediateConn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Error accepting stream from relay: %v", err)
			return
		}
		log.Printf("Accepted incoming relay stream from server (relay)")
		go handleIncomingAudioStream(stream, "relay")
	}
}

// Helpers for endpoint handling
func (p *Peer) buildCandidates(peerObserved, peerLocal string, hasLocal bool) []string {
	// Decide if we are on the same global segment (observed IP match)
	if p.ownObservedIP != nil && hasLocal {
		if host, _, err := net.SplitHostPort(peerObserved); err == nil {
			peerObsIP := net.ParseIP(host)
			if sameIPFamily(peerObsIP, p.ownObservedIP) && ipEqual(peerObsIP, p.ownObservedIP) && peerLocal != "" {
				return []string{peerLocal, peerObserved}
			}
		}
	}
	return []string{peerObserved}
}

func sameIPFamily(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	return (a.To4() != nil) == (b.To4() != nil)
}

func ipEqual(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	if a.To4() != nil && b.To4() != nil {
		return a.To4().Equal(b.To4())
	}
	return a.Equal(b)
}

func toProtoAddr(addrStr string) (proto.Address, bool) {
	var zero proto.Address
	host, portStr, err := net.SplitHostPort(addrStr)
	if err != nil {
		return zero, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return zero, false
	}
	var portNum uint16
	if pn, err := strconv.Atoi(portStr); err == nil {
		portNum = uint16(pn)
	} else if p2, err := net.LookupPort("udp", portStr); err == nil {
		portNum = uint16(p2)
	} else {
		return zero, false
	}
	if ip.To4() != nil {
		return proto.Address{AF: 0x04, IP: ip.To4(), Port: portNum}, true
	}
	return proto.Address{AF: 0x06, IP: ip.To16(), Port: portNum}, true
}

func (p *Peer) startHolePunchingCandidates(candidates []string) {
	if len(candidates) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.hpCancels = append(p.hpCancels, cancel)
	go attemptPrioritizedHolePunch(ctx, p.intermediateTransport, candidates, p.tlsConfig, p.quicConfig, connectionEstablished)
}
