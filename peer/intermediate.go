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

// ConnectToRelay dials the relay server. Alias to ConnectToServer for now.
func ConnectToRelay(relayAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, transport *quic.Transport) (*quic.Conn, error) {
	return ConnectToServer(relayAddr, tlsConfig, quicConfig, transport)
}

// IntermediateControlReadLoop exchanges peer info and handles ongoing notifications.
func IntermediateControlReadLoop(conn *quic.Conn, p *Peer, stream *quic.Stream) {
	if err := sendPeerRequest(stream); err != nil {
		log.Printf("Failed to send peer request: %v", err)
		return
	}

	isFirst := true
	for {
		msg, err := proto.ReadMessage(stream)
		if err != nil {
			log.Printf("Failed to read from intermediate server: %v", err)
			return
		}
		switch m := msg.(type) {
		case proto.PeerListResp:
			if !isFirst {
				log.Printf("Unexpected PEER_LIST_RESP after initial exchange; ignoring")
				continue
			}
			handleInitialPeerList(p, m)
			isFirst = false
		case proto.NewPeerNotif:
			handleNewPeerNotification(p, m)
		case proto.NetworkChangeNotif:
			handleNetworkChangeNotification(p, m)
		default:
			log.Printf("Unexpected message on control stream: %T", m)
		}
	}
}

func sendPeerRequest(stream *quic.Stream) error {
	return proto.WriteMessage(stream, proto.GetPeersReq{})
}

func handleInitialPeerList(p *Peer, resp proto.PeerListResp) {
	log.Printf("Received %d peers from intermediate server:", len(resp.Peers))
	for _, e := range resp.Peers {
		addr := net.JoinHostPort(e.Address.IP.String(), fmt.Sprintf("%d", e.Address.Port))
		log.Printf("  Peer: %d (Address: %s)", e.PeerID, addr)
	}
	p.handleInitialPeers(resp.Peers)
}

func handleNewPeerNotification(p *Peer, n proto.NewPeerNotif) {
	addr := net.JoinHostPort(n.Address.IP.String(), fmt.Sprintf("%d", n.Address.Port))
	log.Printf("Received peer notification - NEW_PEER: id=%d addr=%s", n.PeerID, addr)
	p.handleNewPeer(proto.PeerEntry{PeerID: n.PeerID, Address: n.Address})
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
