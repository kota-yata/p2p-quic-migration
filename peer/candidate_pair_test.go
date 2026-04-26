package main

import (
	"net"
	"testing"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared/qswitch"
)

func TestCandidateQualityScoreRTT(t *testing.T) {
	now := time.Now()
	fast := testPair(candidateTypeHost, candidateTypeHost, 20*time.Millisecond)
	slow := testPair(candidateTypeHost, candidateTypeHost, 200*time.Millisecond)
	fast.LastResponse = now
	slow.LastResponse = now

	if fast.qualityScore(now) <= slow.qualityScore(now) {
		t.Fatalf("expected lower RTT to improve score: fast=%f slow=%f", fast.qualityScore(now), slow.qualityScore(now))
	}
}

func TestCandidateQualityScoreStabilityBonus(t *testing.T) {
	now := time.Now()
	recent := testPair(candidateTypeHost, candidateTypeHost, 50*time.Millisecond)
	stale := testPair(candidateTypeHost, candidateTypeHost, 50*time.Millisecond)
	recent.LastResponse = now.Add(-candidateStabilityWindow)
	stale.LastResponse = now.Add(-candidateStabilityWindow - time.Millisecond)

	got := recent.qualityScore(now) - stale.qualityScore(now)
	if got != 20 {
		t.Fatalf("expected 20 point stability bonus, got %f", got)
	}
}

func TestCandidateQualityScoreMissingRTTPenalty(t *testing.T) {
	now := time.Now()
	withRTT := testPair(candidateTypeHost, candidateTypeHost, time.Millisecond)
	missingRTT := testPair(candidateTypeHost, candidateTypeHost, 0)

	got := withRTT.qualityScore(now) - missingRTT.qualityScore(now)
	if got != 30 {
		t.Fatalf("expected missing RTT penalty of 30, got %f", got)
	}
}

func TestShouldRenominateRelayToDirectHost(t *testing.T) {
	now := time.Now()
	current := testPair(candidateTypeHost, candidateTypeRelay, 10*time.Millisecond)
	best := testPair(candidateTypeHost, candidateTypeHost, 100*time.Millisecond)

	if !shouldRenominate(current, best, now) {
		t.Fatal("expected relay to direct host-host switch")
	}
}

func TestShouldRenominateRTTImprovementGreaterThanThreshold(t *testing.T) {
	now := time.Now()
	current := testPair(candidateTypeHost, candidateTypeHost, 30*time.Millisecond)
	best := testPair(candidateTypeHost, candidateTypeHost, 19*time.Millisecond)
	best.ID = "better-rtt"

	if !shouldRenominate(current, best, now) {
		t.Fatal("expected RTT improvement greater than 10ms to switch")
	}
}

func TestShouldRenominateRTTImprovementAtThresholdDoesNotSwitch(t *testing.T) {
	now := time.Now()
	current := testPair(candidateTypeHost, candidateTypeHost, 30*time.Millisecond)
	best := testPair(candidateTypeHost, candidateTypeHost, 20*time.Millisecond)
	best.ID = "threshold-rtt"

	if shouldRenominate(current, best, now) {
		t.Fatal("did not expect RTT improvement of 10ms to switch")
	}
}

func TestShouldRenominateQualityImprovement(t *testing.T) {
	now := time.Now()
	current := testPair(candidateTypeRelay, candidateTypeRelay, time.Millisecond)
	best := testPair(candidateTypeSrflx, candidateTypeSrflx, time.Millisecond)

	if !shouldRenominate(current, best, now) {
		t.Fatal("expected quality improvement greater than 15% to switch")
	}
}

func TestShouldRenominateRejectsInvalidPairs(t *testing.T) {
	now := time.Now()
	current := testPair(candidateTypeHost, candidateTypeHost, 20*time.Millisecond)
	same := *current
	failed := testPair(candidateTypeHost, candidateTypeHost, time.Millisecond)
	failed.ID = "failed"
	failed.State = candidatePairFailed

	if shouldRenominate(nil, current, now) {
		t.Fatal("nil current should not switch")
	}
	if shouldRenominate(current, nil, now) {
		t.Fatal("nil best should not switch")
	}
	if shouldRenominate(current, &same, now) {
		t.Fatal("same pair should not switch")
	}
	if shouldRenominate(current, failed, now) {
		t.Fatal("failed best pair should not switch")
	}
}

func TestDiscoverLocalCandidatesFiltersInterfaces(t *testing.T) {
	candidates := discoverLocalCandidatesFromInterfaceAddrs([]interfaceAddrs{
		{name: "down0", flags: 0, addrs: []net.Addr{ipNet("198.51.100.10")}},
		{name: "lo0", flags: net.FlagUp | net.FlagLoopback, addrs: []net.Addr{ipNet("198.51.100.11")}},
		{name: "v6", flags: net.FlagUp, addrs: []net.Addr{ipNet("2001:db8::1")}},
		{name: "multicast", flags: net.FlagUp, addrs: []net.Addr{ipNet("224.0.0.1")}},
		{name: "eth0", flags: net.FlagUp, addrs: []net.Addr{ipNet("198.51.100.12")}},
	})

	if len(candidates) != 1 {
		t.Fatalf("expected one usable candidate, got %d: %#v", len(candidates), candidates)
	}
	if candidates[0].Iface != "eth0" || !candidates[0].IP.Equal(net.ParseIP("198.51.100.12")) || candidates[0].Type != candidateTypeHost {
		t.Fatalf("unexpected candidate: %#v", candidates[0])
	}
}

func TestRemoteCandidatesFromPeerEndpointTypes(t *testing.T) {
	endpoint := qswitch.PeerEndpoint{
		PeerID:   7,
		Observed: qswitch.Address{AF: 0x04, IP: net.ParseIP("203.0.113.7").To4(), Port: 5000},
		Local:    qswitch.Address{AF: 0x04, IP: net.ParseIP("10.0.0.7").To4(), Port: 5001},
		Flags:    0x01,
	}

	candidates := remoteCandidatesFromPeerEndpoint(endpoint, true)
	if len(candidates) != 2 {
		t.Fatalf("expected local and observed candidates, got %d", len(candidates))
	}
	if candidates[0].Type != candidateTypeHost || !candidates[0].IsLocal {
		t.Fatalf("expected preferred local host candidate first: %#v", candidates[0])
	}
	if candidates[1].Type != candidateTypeSrflx || candidates[1].IsLocal {
		t.Fatalf("expected observed srflx candidate second: %#v", candidates[1])
	}
}

func testPair(localType, remoteType candidateType, rtt time.Duration) *candidatePair {
	pair := newCandidatePair(
		localCandidate{ID: "local/" + string(localType), Type: localType},
		remoteCandidate{ID: "remote/" + string(remoteType), Type: remoteType},
	)
	pair.State = candidatePairSucceeded
	pair.RTT = rtt
	return pair
}

func ipNet(ip string) *net.IPNet {
	parsed := net.ParseIP(ip)
	if parsed.To4() != nil {
		return &net.IPNet{IP: parsed, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: parsed, Mask: net.CIDRMask(128, 128)}
}
