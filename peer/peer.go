package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
	"github.com/kota-yata/p2p-quic-migration/shared/qswitch"
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
	relayConn             *quic.Conn
	relayAllowStream      *quic.Stream
	relayTransport        *quic.Transport
	relayUdpConn          *net.UDPConn
	candidateManager      *candidatePairManager
	pathCandidateManager  *candidatePairManager
	candidateProbeStop    chan struct{}
	quicCandidatePaths    map[string]*quicCandidatePath
	endpoints             map[uint32]endpointInfo
	ownObservedIP         net.IP
	audioRelayStop        func()
	migrationMu           sync.Mutex
	// hole punch cancellation management
	hpCancels []context.CancelFunc
}

type quicCandidatePath struct {
	path      *quic.Path
	udp       *net.UDPConn
	transport *quic.Transport
	oldIP     net.IP
}

type endpointInfo struct {
	observed string
	local    string
	hasLocal bool
}

func (p *Peer) Run() error {
	if err := p.setupTLS(); err != nil {
		return fmt.Errorf("failed to setup TLS: %v", err)
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

	// Connect to the relay server (may be same addr as intermediate)
	relayConn, err := ConnectToServer(p.config.relayAddr, p.tlsConfig, p.quicConfig, p.intermediateTransport)
	if err != nil {
		return fmt.Errorf("failed to connect to relay server: %v", err)
	}
	defer relayConn.CloseWithError(0, "")
	p.relayConn = relayConn

	p.endpoints = make(map[uint32]endpointInfo)
	// Accept control stream initiated by the server
	stream, err := intermediateConn.AcceptStream(context.Background())
	if err != nil {
		return fmt.Errorf("failed to accept control stream: %v", err)
	}
	defer stream.Close()
	p.intermediateStream = stream
	go IntermediateControlReadLoop(intermediateConn, p, stream)
	go p.acceptRelayStreams()
	p.startCandidatePairEvaluator()
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
	currentAddr, err := network_monitor.GetCurrentAddress()
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

	return nil
}

func (p *Peer) cleanup() {
	log.Printf("Cleaning up resources...")
	if p.intermediateTransport != nil {
		p.intermediateTransport.Close()
	}
	if p.intermediateUdpConn != nil {
		p.intermediateUdpConn.Close()
	}
	if p.intermediateConn != nil {
		p.intermediateConn.CloseWithError(0, "")
	}
	if p.relayConn != nil {
		p.relayConn.CloseWithError(0, "")
	}
	if p.relayTransport != nil {
		p.relayTransport.Close()
	}
	if p.relayUdpConn != nil {
		p.relayUdpConn.Close()
	}
	if p.candidateProbeStop != nil {
		close(p.candidateProbeStop)
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
		select {
		case connectionEstablished <- connchan{conn: conn, isAcceptor: true}:
		default:
			log.Printf("connectionEstablished channel full; dropping accept notification")
			conn.CloseWithError(0, "dropped: channel full")
		}
	}
}

func (p *Peer) handleIncomingConnection(conn *quic.Conn) {
	log.Print("New Peer Connection Accepted. Setting up audio streaming...")

	p.StopAudioRelay()

	// Since we received the connection, we act as the "acceptor"
	log.Printf("Acting as connection acceptor with role=%s", p.config.role)
	handleCommunicationAsAcceptor(conn, p.config.role)
}

func (p *Peer) handleInitialEndpoints(entries []qswitch.PeerEndpoint) {
	for _, e := range entries {
		peerObs := net.JoinHostPort(e.Observed.IP.String(), fmt.Sprintf("%d", e.Observed.Port))
		var peerLoc string
		if e.Flags&0x01 != 0 {
			peerLoc = net.JoinHostPort(e.Local.IP.String(), fmt.Sprintf("%d", e.Local.Port))
		}
		p.endpoints[e.PeerID] = endpointInfo{observed: peerObs, local: peerLoc, hasLocal: e.Flags&0x01 != 0}
		p.registerPeerEndpointCandidates(e)
		p.startHolePunchingCandidates()
	}
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send relay allowlist after initial endpoints: %v", err)
	}
}

func (p *Peer) handleNewEndpoint(e qswitch.PeerEndpoint) {
	peerObs := net.JoinHostPort(e.Observed.IP.String(), fmt.Sprintf("%d", e.Observed.Port))
	var peerLoc string
	if e.Flags&0x01 != 0 {
		peerLoc = net.JoinHostPort(e.Local.IP.String(), fmt.Sprintf("%d", e.Local.Port))
	}
	p.endpoints[e.PeerID] = endpointInfo{observed: peerObs, local: peerLoc, hasLocal: e.Flags&0x01 != 0}
	p.registerPeerEndpointCandidates(e)
	p.startHolePunchingCandidates()
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to send relay allowlist after new endpoint: %v", err)
	}
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
	p.registerRemoteEndpointCandidate(newAddr, candidateTypeSrflx, peerID, false)
	p.startHolePunchingCandidates()
}

// monitorHolepunch waits for either acceptor or initiator connection events,
// cancels outstanding hole punching attempts, and hands off to the proper handler.
func (p *Peer) monitorHolepunch() {
	for evt := range connectionEstablished {
		for _, c := range p.hpCancels {
			c()
		}
		p.hpCancels = nil

		if evt.isAcceptor {
			p.handleIncomingConnection(evt.conn)
		} else {
			peerAddr := evt.conn.RemoteAddr().String()
			handleCommunicationAsInitiator(evt.conn, peerAddr, p.config.role)
		}
	}
}

func (p *Peer) sendNetworkChangeNotification(oldAddr net.IP) error {
	if p.intermediateConn.Context().Err() != nil {
		return p.intermediateConn.Context().Err()
	}

	addr := qswitch.Address{}
	if oldAddr.To4() != nil {
		addr.AF = 0x04
		addr.IP = oldAddr.To4()
		addr.Port = 0
	} else {
		addr.AF = 0x06
		addr.IP = oldAddr.To16()
		addr.Port = 0
	}
	if err := qswitch.WriteMessage(p.intermediateStream, qswitch.NetworkChangeReq{OldAddress: addr}); err != nil {
		return fmt.Errorf("write attempt failed: %w", err)
	}
	log.Printf("Sent server network change notification to intermediate server")
	return nil
}
