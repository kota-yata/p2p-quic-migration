package main

import (
	"fmt"
	"math"
	"net"
	"sort"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared/qswitch"
)

const (
	candidatePairProbeInterval = 200 * time.Millisecond
	candidatePathProbeTimeout  = 200 * time.Millisecond
	candidateStabilityWindow   = 5 * time.Second
	candidateRTTThreshold      = 10 * time.Millisecond
	candidateQualityThreshold  = 1.15
)

type candidateType string

const (
	candidateTypeHost  candidateType = "host"
	candidateTypeSrflx candidateType = "srflx"
	candidateTypePrflx candidateType = "prflx"
	candidateTypeRelay candidateType = "relay"
)

type candidatePairState string

const (
	candidatePairWaiting    candidatePairState = "waiting"
	candidatePairInProgress candidatePairState = "in-progress"
	candidatePairSucceeded  candidatePairState = "succeeded"
	candidatePairFailed     candidatePairState = "failed"
)

type localCandidate struct {
	ID    string
	Iface string
	IP    net.IP
	Type  candidateType
}

type remoteCandidate struct {
	ID      string
	Addr    string
	Type    candidateType
	PeerID  uint32
	IsLocal bool
}

type candidatePair struct {
	ID           string
	Local        localCandidate
	Remote       remoteCandidate
	State        candidatePairState
	RTT          time.Duration
	ResponseCnt  int
	LastResponse time.Time
	Selected     bool
}

func newCandidatePair(local localCandidate, remote remoteCandidate) *candidatePair {
	return &candidatePair{
		ID:     candidatePairID(local, remote),
		Local:  local,
		Remote: remote,
		State:  candidatePairWaiting,
	}
}

func candidatePairID(local localCandidate, remote remoteCandidate) string {
	return local.ID + "->" + remote.ID
}

func (p *candidatePair) qualityScore(now time.Time) float64 {
	score := float64(candidateTypeScore(p.Local.Type) + candidateTypeScore(p.Remote.Type))
	if p.RTT > 0 {
		rttMS := float64(p.RTT) / float64(time.Millisecond)
		if rttMS < 1 {
			rttMS = 1
		}
		score += -math.Log10(rttMS) * 10
	} else {
		score -= 30
	}
	if !p.LastResponse.IsZero() && now.Sub(p.LastResponse) <= candidateStabilityWindow {
		score += 20
	}
	return score
}

func candidateTypeScore(t candidateType) int {
	switch t {
	case candidateTypeHost:
		return 100
	case candidateTypeSrflx:
		return 50
	case candidateTypePrflx:
		return 30
	case candidateTypeRelay:
		return 10
	default:
		return 0
	}
}

func shouldRenominate(current, best *candidatePair, now time.Time) bool {
	if current == nil || best == nil {
		return false
	}
	if current.ID == best.ID || best.State != candidatePairSucceeded {
		return false
	}
	if current.Remote.Type == candidateTypeRelay &&
		current.Local.Type == candidateTypeHost &&
		best.Local.Type == candidateTypeHost &&
		best.Remote.Type == candidateTypeHost {
		return true
	}
	if current.RTT > 0 && best.RTT > 0 && current.RTT-best.RTT > candidateRTTThreshold {
		return true
	}
	currentScore := current.qualityScore(now)
	bestScore := best.qualityScore(now)
	if currentScore <= 0 {
		return bestScore > currentScore
	}
	return bestScore/currentScore > candidateQualityThreshold
}

type candidatePairManager struct {
	localCandidates  map[string]localCandidate
	remoteCandidates map[string]remoteCandidate
	pairs            map[string]*candidatePair
	selected         *candidatePair
}

func newCandidatePairManager() *candidatePairManager {
	return &candidatePairManager{
		localCandidates:  make(map[string]localCandidate),
		remoteCandidates: make(map[string]remoteCandidate),
		pairs:            make(map[string]*candidatePair),
	}
}

func (m *candidatePairManager) setLocalCandidates(candidates []localCandidate) {
	next := make(map[string]localCandidate, len(candidates))
	for _, c := range candidates {
		next[c.ID] = c
	}
	m.localCandidates = next
	m.rebuildPairs()
}

func (m *candidatePairManager) upsertRemoteCandidate(candidate remoteCandidate) {
	m.remoteCandidates[candidate.ID] = candidate
	m.rebuildPairs()
}

func (m *candidatePairManager) removeDuplicateRemoteAddrs() {
	seen := make(map[string]remoteCandidate)
	for _, candidate := range m.remoteCandidates {
		current, ok := seen[candidate.Addr]
		if !ok || candidatePreference(candidate) > candidatePreference(current) {
			seen[candidate.Addr] = candidate
		}
	}
	if len(seen) == len(m.remoteCandidates) {
		return
	}
	m.remoteCandidates = make(map[string]remoteCandidate, len(seen))
	for _, candidate := range seen {
		m.remoteCandidates[candidate.ID] = candidate
	}
	m.rebuildPairs()
}

func (m *candidatePairManager) rebuildPairs() {
	for _, local := range m.localCandidates {
		for _, remote := range m.remoteCandidates {
			id := candidatePairID(local, remote)
			if _, ok := m.pairs[id]; !ok {
				m.pairs[id] = newCandidatePair(local, remote)
			}
		}
	}
	for id, pair := range m.pairs {
		if _, ok := m.localCandidates[pair.Local.ID]; !ok {
			delete(m.pairs, id)
			continue
		}
		if _, ok := m.remoteCandidates[pair.Remote.ID]; !ok {
			delete(m.pairs, id)
		}
	}
	if m.selected != nil {
		if _, ok := m.pairs[m.selected.ID]; !ok {
			m.selected.Selected = false
			m.selected = nil
		}
	}
}

func (m *candidatePairManager) recordSuccess(pairID string, rtt time.Duration, now time.Time) {
	pair := m.pairs[pairID]
	if pair == nil {
		return
	}
	pair.State = candidatePairSucceeded
	pair.RTT = rtt
	pair.ResponseCnt++
	pair.LastResponse = now
}

func (m *candidatePairManager) recordFailure(pairID string) {
	pair := m.pairs[pairID]
	if pair != nil && pair.ResponseCnt == 0 {
		pair.State = candidatePairFailed
	}
}

func (m *candidatePairManager) bestSucceeded(now time.Time) *candidatePair {
	var best *candidatePair
	for _, pair := range m.pairs {
		if pair.State != candidatePairSucceeded {
			continue
		}
		if best == nil || pair.qualityScore(now) > best.qualityScore(now) {
			best = pair
		}
	}
	return best
}

func (m *candidatePairManager) selectPair(pair *candidatePair) {
	if m.selected != nil {
		m.selected.Selected = false
	}
	m.selected = pair
	if pair != nil {
		pair.Selected = true
	}
}

func (m *candidatePairManager) orderedDialPairs(now time.Time) []*candidatePair {
	pairs := make([]*candidatePair, 0, len(m.pairs))
	for _, pair := range m.pairs {
		if pair.State == candidatePairFailed {
			continue
		}
		pairs = append(pairs, pair)
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return pairs[i].qualityScore(now) > pairs[j].qualityScore(now)
	})
	return pairs
}

func candidatePreference(candidate remoteCandidate) int {
	score := candidateTypeScore(candidate.Type)
	if candidate.IsLocal {
		score += 1000
	}
	return score
}

func discoverLocalCandidates() ([]localCandidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to list interfaces: %w", err)
	}
	input := make([]interfaceAddrs, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		input = append(input, interfaceAddrs{name: iface.Name, flags: iface.Flags, addrs: addrs})
	}
	return discoverLocalCandidatesFromInterfaceAddrs(input), nil
}

type interfaceAddrs struct {
	name  string
	flags net.Flags
	addrs []net.Addr
}

func discoverLocalCandidatesFromInterfaceAddrs(ifaces []interfaceAddrs) []localCandidate {
	var candidates []localCandidate
	for _, iface := range ifaces {
		if (iface.flags&net.FlagUp) == 0 || (iface.flags&net.FlagLoopback) != 0 {
			continue
		}
		for _, addr := range iface.addrs {
			ip := addrIP(addr)
			ip4 := ip.To4()
			if ip4 == nil || !ip4.IsGlobalUnicast() {
				continue
			}
			candidates = append(candidates, localCandidate{
				ID:    iface.name + "/" + ip4.String(),
				Iface: iface.name,
				IP:    ip4,
				Type:  candidateTypeHost,
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Iface == candidates[j].Iface {
			return bytesCompareIP(candidates[i].IP, candidates[j].IP) < 0
		}
		return interfacePriority(candidates[i].Iface) < interfacePriority(candidates[j].Iface)
	})
	return candidates
}

func addrIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func bytesCompareIP(a, b net.IP) int {
	as, bs := a.String(), b.String()
	switch {
	case as < bs:
		return -1
	case as > bs:
		return 1
	default:
		return 0
	}
}

func remoteCandidateFromEndpoint(peerID uint32, addr string, typ candidateType, isLocal bool) (remoteCandidate, bool) {
	if addr == "" {
		return remoteCandidate{}, false
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return remoteCandidate{}, false
	}
	scope := "observed"
	if isLocal {
		scope = "local"
	}
	return remoteCandidate{
		ID:      fmt.Sprintf("%d/%s/%s", peerID, scope, addr),
		Addr:    addr,
		Type:    typ,
		PeerID:  peerID,
		IsLocal: isLocal,
	}, true
}

func remoteCandidatesFromPeerEndpoint(e qswitch.PeerEndpoint, preferLocal bool) []remoteCandidate {
	observed := net.JoinHostPort(e.Observed.IP.String(), fmt.Sprintf("%d", e.Observed.Port))
	var out []remoteCandidate
	if e.Flags&0x01 != 0 {
		local := net.JoinHostPort(e.Local.IP.String(), fmt.Sprintf("%d", e.Local.Port))
		if c, ok := remoteCandidateFromEndpoint(e.PeerID, local, candidateTypeHost, true); ok {
			out = append(out, c)
		}
	}
	if c, ok := remoteCandidateFromEndpoint(e.PeerID, observed, candidateTypeSrflx, false); ok {
		if preferLocal && len(out) > 0 {
			out = append(out, c)
		} else {
			out = append([]remoteCandidate{c}, out...)
		}
	}
	return dedupeRemoteCandidatesByAddr(out)
}

func dedupeRemoteCandidatesByAddr(candidates []remoteCandidate) []remoteCandidate {
	out := make([]remoteCandidate, 0, len(candidates))
	seen := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		idx, ok := seen[candidate.Addr]
		if !ok {
			seen[candidate.Addr] = len(out)
			out = append(out, candidate)
			continue
		}
		if candidatePreference(candidate) > candidatePreference(out[idx]) {
			out[idx] = candidate
		}
	}
	return out
}
