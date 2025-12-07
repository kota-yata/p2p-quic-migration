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
)

func attemptNATHolePunch(tr *quic.Transport, peerAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, stopChan chan bool, role string) {
    log.Printf("Starting NAT hole punching to peer: %s (will attempt %d times)", peerAddr, maxHolePunchAttempts)

    peerAddrResolved, err := net.ResolveUDPAddr("udp", peerAddr)
    if err != nil {
        log.Printf("Failed to resolve peer address %s for NAT hole punch: %v", peerAddr, err)
        return
    }

    for attempt := 1; attempt <= maxHolePunchAttempts; attempt++ {
        select {
        case <-stopChan:
            log.Printf("Stopping server NAT hole punch to %s - connection established", peerAddr)
            return
        default:
        }

        if err := performHolePunchAttempt(tr, peerAddrResolved, tlsConfig, quicConfig, peerAddr, attempt, role); err != nil {
            log.Printf("NAT hole punch attempt %d/%d to %s failed: %v", attempt, maxHolePunchAttempts, peerAddr, err)
        }

        if attempt < maxHolePunchAttempts {
            if waitBeforeNextHolePunch(attempt, stopChan) {
                log.Printf("Stopping server NAT hole punch to %s during wait - connection established", peerAddr)
                return
            }
        }
    }

    log.Printf("Completed all %d NAT hole punch attempts to peer %s", maxHolePunchAttempts, peerAddr)
}

func performHolePunchAttempt(tr *quic.Transport, peerAddrResolved *net.UDPAddr, tlsConfig *tls.Config, quicConfig *quic.Config, peerAddr string, attempt int, role string) error {
    log.Printf("NAT hole punch attempt %d/%d to peer %s", attempt, maxHolePunchAttempts, peerAddr)

    udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
    if err != nil {
        return fmt.Errorf("failed to create UDP connection: %v", err)
    }
    defer udpConn.Close()

    ctx, cancel := context.WithTimeout(context.Background(), holePunchTimeout)
    defer cancel()

    conn, err := tr.Dial(ctx, peerAddrResolved, tlsConfig, quicConfig)

    if err != nil {
        log.Printf("NAT hole punch attempt %d/%d to %s completed (connection failed, which is normal): %v", attempt, maxHolePunchAttempts, peerAddr, err)
        return nil
    }

    log.Printf("NAT hole punch attempt %d/%d to %s succeeded - acting as connection initiator", attempt, maxHolePunchAttempts, peerAddr)

    // Initiator behavior depends on role
    go handleCommunicationAsInitiator(conn, peerAddr, role)

    return nil
}

func waitBeforeNextHolePunch(attempt int, stopChan chan bool) bool {
    backoffDuration := time.Duration(attempt) * time.Second
    if backoffDuration > 5*time.Second {
        backoffDuration = 5 * time.Second
    }
    log.Printf("Waiting %v before next hole punch attempt %d", backoffDuration, attempt+1)

    select {
    case <-stopChan:
        return true // Signal to stop
    case <-time.After(backoffDuration):
        return false // Continue with next attempt
    }
}
