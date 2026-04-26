package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
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

func (p *Peer) sendRelayAllowlistUpdate() error {
	if err := p.openRelayAllowStream(); err != nil {
		return err
	}
	addrs := make([]qswitch.Address, 0, len(p.endpoints)*2)
	for _, ep := range p.endpoints {
		if a, ok := toProtoAddr(ep.observed); ok {
			addrs = append(addrs, a)
		}
		if ep.hasLocal && p.ownObservedIP != nil {
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
	msg := qswitch.RelayAllowlistSet{Addresses: addrs}
	if err := qswitch.WriteMessage(p.relayAllowStream, msg); err != nil {
		return err
	}
	return nil
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

func (p *Peer) switchToAudioRelay(targetPeerID uint32) error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection available for relay")
	}

	stream, err := p.relayConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open audio relay stream: %v", err)
	}

	if err := qswitch.WriteMessage(stream, qswitch.AudioRelayReq{TargetPeerID: targetPeerID}); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send audio relay request: %v", err)
	}

	log.Printf("Switched to audio relay mode for peer %d", targetPeerID)

	p.audioRelayStop = startAudioRelay(stream, targetPeerID)
	return nil
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

func (p *Peer) ensureCandidateManagers() {
	if p.candidateManager == nil {
		p.candidateManager = newCandidatePairManager()
	}
	if p.pathCandidateManager == nil {
		p.pathCandidateManager = newCandidatePairManager()
	}
	if p.quicCandidatePaths == nil {
		p.quicCandidatePaths = make(map[string]*quicCandidatePath)
	}
}

func (p *Peer) startCandidatePairEvaluator() {
	p.ensureCandidateManagers()
	p.registerQUICRemoteCandidate("intermediate", p.config.serverAddr, candidateTypeHost)
	p.registerQUICRemoteCandidate("relay", p.config.relayAddr, candidateTypeRelay)
	p.refreshLocalCandidates()

	p.candidateProbeStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(candidatePairProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.evaluateCandidatePairs()
			case <-p.candidateProbeStop:
				return
			}
		}
	}()
}

func (p *Peer) registerQUICRemoteCandidate(label, addr string, typ candidateType) {
	candidate, ok := remoteCandidateFromEndpoint(0, addr, typ, false)
	if !ok {
		return
	}
	candidate.ID = "quic/" + label + "/" + addr
	p.pathCandidateManager.upsertRemoteCandidate(candidate)
}

func (p *Peer) registerPeerEndpointCandidates(e qswitch.PeerEndpoint) {
	p.ensureCandidateManagers()
	preferLocal := false
	if p.ownObservedIP != nil && e.Flags&0x01 != 0 {
		preferLocal = sameIPFamily(e.Observed.IP, p.ownObservedIP) && ipEqual(e.Observed.IP, p.ownObservedIP)
	}
	for _, candidate := range remoteCandidatesFromPeerEndpoint(e, preferLocal) {
		p.candidateManager.upsertRemoteCandidate(candidate)
	}
	p.refreshLocalCandidates()
}

func (p *Peer) registerRemoteEndpointCandidate(addr string, typ candidateType, peerID uint32, isLocal bool) {
	p.ensureCandidateManagers()
	candidate, ok := remoteCandidateFromEndpoint(peerID, addr, typ, isLocal)
	if !ok {
		return
	}
	p.candidateManager.upsertRemoteCandidate(candidate)
	p.refreshLocalCandidates()
}

func (p *Peer) refreshLocalCandidates() {
	candidates, err := discoverLocalCandidates()
	if err != nil {
		log.Printf("Failed to discover local candidates: %v", err)
		return
	}
	p.candidateManager.setLocalCandidates(candidates)
	p.pathCandidateManager.setLocalCandidates(candidates)
}

func (p *Peer) evaluateCandidatePairs() {
	p.migrationMu.Lock()
	defer p.migrationMu.Unlock()

	if p.intermediateConn == nil || p.intermediateConn.Context().Err() != nil {
		return
	}
	p.refreshLocalCandidates()

	now := time.Now()
	for _, pair := range p.pathCandidateManager.pairs {
		if pair.State == candidatePairFailed || pair.Remote.Type == candidateTypeRelay && p.relayConn == nil {
			continue
		}
		p.probeQUICCandidatePath(pair)
	}

	best := p.bestIntermediatePathCandidate(now)
	if best == nil {
		return
	}
	if p.pathCandidateManager.selected == nil {
		p.pathCandidateManager.selectPair(best)
		return
	}
	if !shouldRenominate(p.pathCandidateManager.selected, best, now) {
		return
	}
	if err := p.switchToQUICCandidatePair(best); err != nil {
		log.Printf("Failed to switch selected candidate pair %s: %v", best.ID, err)
		return
	}
}

func (p *Peer) bestIntermediatePathCandidate(now time.Time) *candidatePair {
	var best *candidatePair
	for _, pair := range p.pathCandidateManager.pairs {
		if pair.State != candidatePairSucceeded || !strings.HasPrefix(pair.Remote.ID, "quic/intermediate/") {
			continue
		}
		if best == nil || pair.qualityScore(now) > best.qualityScore(now) {
			best = pair
		}
	}
	return best
}

func (p *Peer) probeQUICCandidatePath(pair *candidatePair) {
	label := "intermediate"
	conn := p.intermediateConn
	if strings.HasPrefix(pair.Remote.ID, "quic/relay/") {
		label = "relay"
		conn = p.relayConn
	}
	if conn == nil || conn.Context().Err() != nil {
		return
	}

	qp := p.quicCandidatePaths[pair.ID]
	if qp == nil {
		udp, err := listenUDPOnInterface(pair.Local.Iface, pair.Local.IP)
		if err != nil {
			log.Printf("Failed to create %s candidate path via %s ip=%s: %v", label, pair.Local.Iface, pair.Local.IP, err)
			p.pathCandidateManager.recordFailure(pair.ID)
			return
		}
		transport := &quic.Transport{Conn: udp}
		path, err := conn.AddPath(transport)
		if err != nil {
			transport.Close()
			udp.Close()
			log.Printf("Failed to add %s candidate path via %s ip=%s: %v", label, pair.Local.Iface, pair.Local.IP, err)
			p.pathCandidateManager.recordFailure(pair.ID)
			return
		}
		oldIP := net.IP(nil)
		if p.pathCandidateManager.selected != nil {
			oldIP = p.pathCandidateManager.selected.Local.IP
		}
		qp = &quicCandidatePath{path: path, udp: udp, transport: transport, oldIP: oldIP}
		p.quicCandidatePaths[pair.ID] = qp
	}

	previousState := pair.State
	pair.State = candidatePairInProgress
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	start := time.Now()
	err := qp.path.Probe(ctx)
	cancel()
	if err != nil {
		log.Printf("Probe failed for %s candidate pair %s: %v", label, pair.ID, err)
		p.pathCandidateManager.recordFailure(pair.ID)
		if pair.ResponseCnt > 0 {
			pair.State = previousState
		}
		return
	}
	p.pathCandidateManager.recordSuccess(pair.ID, time.Since(start), time.Now())
}

func (p *Peer) switchToQUICCandidatePair(best *candidatePair) error {
	qp := p.quicCandidatePaths[best.ID]
	if qp == nil {
		return fmt.Errorf("candidate path %s is not available", best.ID)
	}
	if err := qp.path.Switch(); err != nil {
		return err
	}

	p.intermediateTransport = qp.transport
	p.intermediateUdpConn = qp.udp
	oldIP := qp.oldIP
	if oldIP == nil && p.pathCandidateManager.selected != nil {
		oldIP = p.pathCandidateManager.selected.Local.IP
	}
	p.pathCandidateManager.selectPair(best)

	if relayPair := p.relayPairForLocal(best.Local.ID); relayPair != nil && relayPair.State == candidatePairSucceeded {
		if relayPath := p.quicCandidatePaths[relayPair.ID]; relayPath != nil {
			if err := relayPath.path.Switch(); err != nil {
				log.Printf("Failed to switch relay path for selected local candidate %s: %v", best.Local.ID, err)
			} else {
				p.relayTransport = relayPath.transport
				p.relayUdpConn = relayPath.udp
			}
		}
	}

	if oldIP != nil && !oldIP.Equal(best.Local.IP) {
		if err := p.sendNetworkChangeNotification(oldIP); err != nil {
			return fmt.Errorf("failed to notify network change after candidate switch: %w", err)
		}
	}
	if err := p.sendRelayAllowlistUpdate(); err != nil {
		log.Printf("Failed to refresh relay allowlist after candidate switch: %v", err)
	}
	log.Printf("Switched QUIC candidate pair to %s via %s ip=%s rtt=%s", best.ID, best.Local.Iface, best.Local.IP, best.RTT)
	return nil
}

func (p *Peer) relayPairForLocal(localID string) *candidatePair {
	for _, pair := range p.pathCandidateManager.pairs {
		if pair.Local.ID == localID && strings.HasPrefix(pair.Remote.ID, "quic/relay/") {
			return pair
		}
	}
	return nil
}

func interfacePriority(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "rmnet"),
		strings.HasPrefix(lower, "ccmni"),
		strings.HasPrefix(lower, "wwan"),
		strings.HasPrefix(lower, "pdp"),
		strings.HasPrefix(lower, "usb"):
		return 0
	case strings.HasPrefix(lower, "eth"),
		strings.HasPrefix(lower, "en"):
		return 1
	case strings.HasPrefix(lower, "wlan"),
		strings.HasPrefix(lower, "wl"):
		return 2
	default:
		return 3
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

// acceptRelayStreams accepts additional audio streams initiated by the relay server
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
		log.Printf("Accepted incoming relay stream from server (relay)")
		go handleIncomingAudioStream(stream, "relay")
	}
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

func toProtoAddr(addrStr string) (qswitch.Address, bool) {
	var zero qswitch.Address
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
		return qswitch.Address{AF: 0x04, IP: ip.To4(), Port: portNum}, true
	}
	return qswitch.Address{AF: 0x06, IP: ip.To16(), Port: portNum}, true
}

func (p *Peer) startHolePunchingCandidates() {
	p.ensureCandidateManagers()
	p.refreshLocalCandidates()
	pairs := p.candidateManager.orderedDialPairs(time.Now())
	if len(pairs) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.hpCancels = append(p.hpCancels, cancel)
	go attemptCandidatePairHolePunch(ctx, pairs, p.tlsConfig, p.quicConfig, connectionEstablished, func(pairID string, rtt time.Duration) {
		p.migrationMu.Lock()
		defer p.migrationMu.Unlock()
		if p.candidateManager != nil {
			p.candidateManager.recordSuccess(pairID, rtt, time.Now())
			p.candidateManager.selectPair(p.candidateManager.pairs[pairID])
		}
	})
}
