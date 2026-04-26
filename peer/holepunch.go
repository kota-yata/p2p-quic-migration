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
	holePunchTimeout = 2 * time.Second
)

func attemptCandidatePairHolePunch(ctx context.Context, pairs []*candidatePair, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan connchan, onSuccess func(pairID string, rtt time.Duration)) {
	for idx, pair := range pairs {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Printf("Hole punching candidate pair %d/%d: local=%s remote=%s", idx+1, len(pairs), pair.Local.ID, pair.Remote.Addr)
		tr, udp, err := transportForLocalCandidate(pair.Local)
		if err != nil {
			log.Printf("Failed to create transport for candidate pair %s: %v", pair.ID, err)
			continue
		}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, holePunchTimeout)
		conn, err := dialCandidatePair(cctx, tr, pair.Remote.Addr, tlsConfig, quicConfig)
		cancel()
		rtt := time.Since(start)
		if err != nil {
			log.Printf("NAT hole punch candidate pair %s failed: %v", pair.ID, err)
			tr.Close()
			udp.Close()
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
