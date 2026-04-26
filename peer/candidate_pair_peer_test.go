package main

import (
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestSeedActivePathCandidateSelectsCurrentLocalPair(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	manager := newCandidatePairManager()
	local := localCandidate{ID: "lo0/127.0.0.1", Iface: "lo0", IP: net.ParseIP("127.0.0.1").To4(), Type: candidateTypeHost}
	manager.setLocalCandidates([]localCandidate{local})
	remote, ok := remoteCandidateFromEndpoint(0, "203.0.113.1:12345", candidateTypeHost, false)
	if !ok {
		t.Fatal("failed to create remote candidate")
	}
	remote.ID = "quic/intermediate/203.0.113.1:12345"
	manager.upsertRemoteCandidate(remote)

	p := &Peer{
		intermediateUdpConn:  udp,
		pathCandidateManager: manager,
	}
	p.seedActivePathCandidate(time.Now())

	if manager.selected == nil {
		t.Fatal("expected active path candidate to be selected")
	}
	if manager.selected.Local.ID != local.ID {
		t.Fatalf("selected wrong local candidate: %s", manager.selected.Local.ID)
	}
	if manager.selected.State != candidatePairSucceeded {
		t.Fatalf("expected selected active path to be succeeded, got %s", manager.selected.State)
	}
	if !p.isActiveLocalCandidate(local) {
		t.Fatal("expected local candidate to match active UDP address")
	}
}

func TestTransportForCandidatePairUsesActiveTransportForCurrentLocal(t *testing.T) {
	active := &quic.Transport{}
	local := localCandidate{ID: "en0/10.0.0.2", IP: net.ParseIP("10.0.0.2").To4(), Type: candidateTypeHost}

	got, closeTransport, err := transportForCandidatePair(active, net.ParseIP("10.0.0.2").To4(), local)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()

	if got != active {
		t.Fatal("expected current local candidate to reuse active transport")
	}
}
