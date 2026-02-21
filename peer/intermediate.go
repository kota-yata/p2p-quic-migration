// Communication with intermediate server (simplified)

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	proto "github.com/kota-yata/p2p-quic-migration/shared/cmp9protocol"
	"github.com/quic-go/quic-go"
)

const (
	connectionTimeout = 10 * time.Second
)

// ConnectToServer dials the intermediate server using the provided transport/configs.
func ConnectToServer(serverAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, transport *quic.Transport) (*quic.Conn, error) {
	serverAddrResolved, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	conn, err := transport.Dial(ctx, serverAddrResolved, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to intermediate server: %v", err)
	}

	log.Printf("Connected to a server at %s", serverAddr)
	return conn, nil
}

// Relay role is handled by the intermediate server; no separate dial helper.

// IntermediateControlReadLoop handles the v2 flow: ObservedAddr -> SelfAddrsSet -> GetPeerEndpointsReq
func IntermediateControlReadLoop(conn *quic.Conn, p *Peer, stream *quic.Stream) {
    // Expect ObservedAddr first
    msg, err := proto.ReadMessage(stream)
    if err != nil {
        log.Printf("Failed to read ObservedAddr: %v", err)
        return
    }
    oa, ok := msg.(proto.ObservedAddr)
    if !ok {
        log.Printf("Unexpected first message on control stream: %T", msg)
        return
    }
    p.ownObservedIP = oa.Observed.IP
    log.Printf("Server observed our address: %s:%d", oa.Observed.IP.String(), oa.Observed.Port)

    // Determine local address (from network monitor + UDP local port)
    localIP, err := p.networkMonitor.GetCurrentAddress()
    if err != nil {
        log.Printf("Failed to get local IP: %v", err)
        return
    }
    // Read the actual bound local port from UDPConn if available
    localPort := peerPort
    if p.intermediateUdpConn != nil {
        if la, ok := p.intermediateUdpConn.LocalAddr().(*net.UDPAddr); ok && la != nil {
            if la.Port != 0 {
                localPort = la.Port
            }
        }
    }
    var localAddr proto.Address
    if localIP.To4() != nil {
        localAddr = proto.Address{AF: 0x04, IP: localIP.To4(), Port: uint16(localPort)}
    } else {
        localAddr = proto.Address{AF: 0x06, IP: localIP.To16(), Port: uint16(localPort)}
    }

    // Send SelfAddrsSet with our observed (from server) and local
    if err := proto.WriteMessage(stream, proto.SelfAddrsSet{Observed: oa.Observed, HasLocal: true, Local: localAddr}); err != nil {
        log.Printf("Failed to send SelfAddrsSet: %v", err)
        return
    }

    // Request endpoints directory
    if err := proto.WriteMessage(stream, proto.GetPeerEndpointsReq{}); err != nil {
        log.Printf("Failed to send GetPeerEndpointsReq: %v", err)
        return
    }

    for {
        msg, err := proto.ReadMessage(stream)
        if err != nil {
            log.Printf("Failed to read from intermediate server: %v", err)
            return
        }
        switch m := msg.(type) {
        case proto.PeerEndpointsResp:
            log.Printf("Received %d peer endpoints from server", len(m.Entries))
            p.handleInitialEndpoints(m.Entries)
        case proto.NewPeerEndpointNotif:
            log.Printf("New peer endpoint: id=%d", m.Entry.PeerID)
            p.handleNewEndpoint(m.Entry)
        case proto.NetworkChangeNotif:
            handleNetworkChangeNotification(p, m)
        default:
            log.Printf("Unexpected message on control stream: %T", m)
        }
    }
}

func handleNetworkChangeNotification(p *Peer, n proto.NetworkChangeNotif) {
    oldA := net.JoinHostPort(n.OldAddress.IP.String(), fmt.Sprintf("%d", n.OldAddress.Port))
    newA := net.JoinHostPort(n.NewAddress.IP.String(), fmt.Sprintf("%d", n.NewAddress.Port))
    log.Printf("Received network change notification - Peer: %d, %s -> %s", n.PeerID, oldA, newA)
    p.HandleNetworkChange(n.PeerID, oldA, newA)
}

// startAudioRelay starts streaming audio over the provided stream and returns a stopper.
func startAudioRelay(stream *quic.Stream, targetPeerID uint32) func() {
	stop := make(chan struct{}, 1)

	go func() {
		defer stream.Close()

		log.Printf("Starting audio relay for peer %d from position %d bytes", targetPeerID, 0)

		audioStreamer := NewAudioStreamerFromPosition(stream, 0)

		done := make(chan error, 1)
		go func() {
			done <- audioStreamer.StreamAudio()
		}()

		select {
		case err := <-done:
			if err != nil {
				log.Printf("Audio relay streaming failed: %v", err)
			} else {
				log.Printf("Audio relay to peer %d completed normally", targetPeerID)
			}
		case <-stop:
			log.Printf("Audio relay to peer %d stopped due to P2P reconnection", targetPeerID)
			stream.Close()
			return
		}
	}()

	return func() {
		select {
		case stop <- struct{}{}:
			// sent stop signal
		default:
			// already stopped
		}
	}
}
