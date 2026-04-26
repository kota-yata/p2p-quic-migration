package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	maxHolePunchAttempts = 5
	holePunchTimeout     = 2 * time.Second
	holePunchInterval    = 200 * time.Millisecond
)

func attemptCandidatePairHolePunch(ctx context.Context, activeTransport *quic.Transport, activeLocalIP net.IP, pairs []*candidatePair, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan connchan, onSuccess func(pairID string, rtt time.Duration)) {
	for idx, pair := range pairs {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Printf("Hole punching candidate pair %d/%d: local=%s remote=%s", idx+1, len(pairs), pair.Local.ID, pair.Remote.Addr)
		conn, rtt, err := attemptCandidatePairWithRetries(ctx, activeTransport, activeLocalIP, pair, tlsConfig, quicConfig)
		if err != nil {
			log.Printf("NAT hole punch candidate pair %s failed: %v", pair.ID, err)
			continue
		}
		if onSuccess != nil {
			onSuccess(pair.ID, rtt)
		}
		select {
		case stopChan <- connchan{conn: conn, isAcceptor: false}:
		default:
			log.Printf("connectionEstablished channel full; cannot deliver initiator connection")
			conn.CloseWithError(0, "dropped: channel full")
		}
		return
	}
}

func attemptCandidatePairWithRetries(ctx context.Context, activeTransport *quic.Transport, activeLocalIP net.IP, pair *candidatePair, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, time.Duration, error) {
	var lastErr error
	for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		default:
		}

		tr, closeTransport, err := transportForCandidatePair(activeTransport, activeLocalIP, pair.Local)
		if err != nil {
			return nil, 0, err
		}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, holePunchTimeout)
		conn, err := dialCandidatePair(cctx, tr, pair.Remote.Addr, tlsConfig, quicConfig)
		cancel()
		rtt := time.Since(start)
		if err == nil {
			return conn, rtt, nil
		}
		lastErr = err
		closeTransport()
		log.Printf("NAT hole punch attempt %d/%d for candidate pair %s failed: %v", attempt, maxHolePunchAttempts, pair.ID, err)

		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(holePunchInterval):
		}
	}
	return nil, 0, lastErr
}

func transportForCandidatePair(activeTransport *quic.Transport, activeLocalIP net.IP, local localCandidate) (*quic.Transport, func(), error) {
	if activeTransport != nil && ipEqual(local.IP, activeLocalIP) {
		return activeTransport, func() {}, nil
	}
	tr, udp, err := transportForLocalCandidate(local)
	if err != nil {
		return nil, nil, err
	}
	return tr, func() {
		tr.Close()
		udp.Close()
	}, nil
}

func transportForLocalCandidate(local localCandidate) (*quic.Transport, *net.UDPConn, error) {
	if local.Iface == "" || local.IP == nil {
		return nil, nil, fmt.Errorf("missing local candidate binding")
	}
	udp, err := listenUDPOnInterface(local.Iface, local.IP)
	if err != nil {
		return nil, nil, err
	}
	return &quic.Transport{Conn: udp}, udp, nil
}

func dialCandidatePair(ctx context.Context, tr *quic.Transport, remoteAddr string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
	peerAddrResolved, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return nil, err
	}
	return tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)
}
