package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	maxHolePunchAttempts = 5
	holePunchTimeout     = 2 * time.Second
	attemptInterval      = 200 * time.Millisecond
)

func attemptNATHolePunch(ctx context.Context, tr *quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan connchan) {
	log.Printf("Starting NAT hole punching to peer: %s (up to %d attempts)", peerAddr, maxHolePunchAttempts)

	peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		log.Printf("Failed to resolve peer address %s: %v", peerAddr, err)
		return
	}

	for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
		select {
		case <-ctx.Done():
			log.Printf("Hole punching canceled before attempt %d to %s", attempt, peerAddr)
			return
		default:
		}

		conn, err := performHolePunchAttempt(ctx, tr, peerAddrResolved, tlsConfig, quicConfig)
		if err != nil {
			log.Printf("NAT hole punch attempt %d to %s failed: %v", attempt, peerAddr, err)
		}
		if conn != nil {
			// Notify monitor; do not block if channel is full
			select {
			case stopChan <- connchan{conn: conn, isAcceptor: false}:
				break
			default:
				log.Printf("connectionEstablished channel full; cannot deliver initiator connection")
				// Close the connection to avoid leaking resources
				conn.CloseWithError(0, "dropped: channel full")
			}
			return
		}

		// Wait briefly before next attempt
		select {
		case <-ctx.Done():
			log.Printf("Hole punching canceled after attempt %d to %s", attempt, peerAddr)
			return
		case <-time.After(attemptInterval):
		}
	}
}

func performHolePunchAttempt(ctx context.Context, tr *quic.Transport, peerAddrResolved *net.UDPAddr, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
    log.Printf("NAT hole punch attempt to peer %s", peerAddrResolved.String())
    conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

	if err != nil {
		return nil, err
	}

	log.Printf("NAT hole punch succeeded - acting as connection initiator")

	return conn, nil
}

func attemptPrioritizedHolePunch(ctx context.Context, tr *quic.Transport, candidates []string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan connchan) {
    for idx, addr := range candidates {
        select {
        case <-ctx.Done():
            return
        default:
        }
        log.Printf("Hole punching candidate %d/%d: %s", idx+1, len(candidates), addr)
        cctx, cancel := context.WithCancel(ctx)
        attemptNATHolePunch(cctx, tr, addr, tlsConfig, quicConfig, stopChan)
        cancel()
        if ctx.Err() != nil {
            return
        }
    }
}
