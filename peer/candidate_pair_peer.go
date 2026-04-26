package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared/qswitch"
	"github.com/quic-go/quic-go"
)

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
