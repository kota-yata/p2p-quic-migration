// Communication with intermediate server (simplified)

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

const (
	connectionTimeout         = 10 * time.Second
	observedAddressMaxRetries = 10
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

	log.Printf("Connected to intermediate server at %s", serverAddr)
	return conn, nil
}

// WaitForObservedAddress polls the QUIC connection for the observed (server-seen) address.
func WaitForObservedAddress(conn *quic.Conn) {
	for i := 0; i < observedAddressMaxRetries; i++ {
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address received: %s", observedAddr.String())
			break
		}
	}
}

// IntermediateReadLoop exchanges peer info and handles ongoing notifications.
func IntermediateReadLoop(conn *quic.Conn, p *Peer, stream *quic.Stream) {
	if err := sendPeerRequest(stream); err != nil {
		log.Printf("Failed to send peer request: %v", err)
		return
	}

	buffer := make([]byte, 4096)
	isFirst := true

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Failed to read from intermediate server: %v", err)
			return
		}

		data := buffer[:n]
		if isFirst {
			handleInitialPeerList(p, data)
			isFirst = false
			continue
		}
		handlePeerNotification(p, data)
	}
}

func sendPeerRequest(stream *quic.Stream) error {
	if _, err := stream.Write([]byte("GET_PEERS")); err != nil {
		return fmt.Errorf("failed to send GET_PEERS request: %v", err)
	}
	return nil
}

func handleInitialPeerList(p *Peer, data []byte) {
	var peers []shared.PeerInfo
	if err := json.Unmarshal(data, &peers); err != nil {
		log.Printf("Failed to unmarshal peer list: %v", err)
		return
	}

	log.Printf("Received %d peers from intermediate server:", len(peers))
	for _, peer := range peers {
		log.Printf("  Peer: %s (Address: %s)", peer.ID, peer.Address)
	}

	p.handleInitialPeers(peers)
}

func handlePeerNotification(p *Peer, data []byte) {
	var pn shared.PeerNotification
	if err := json.Unmarshal(data, &pn); err == nil && pn.Type == "NEW_PEER" {
		log.Printf("Received peer notification - Type: %s, Peer: %s (Address: %s)", pn.Type, pn.Peer.ID, pn.Peer.Address)
		p.handleNewPeer(*pn.Peer)
		return
	}

	var nn shared.NetworkChangeNotification
	if err := json.Unmarshal(data, &nn); err == nil && nn.Type == "NETWORK_CHANGE" {
		log.Printf("Received network change notification - Peer: %s, %s -> %s", nn.PeerID, nn.OldAddress, nn.NewAddress)
		p.HandleNetworkChange(nn.PeerID, nn.OldAddress, nn.NewAddress)
		return
	}

	log.Printf("Failed to unmarshal notification data: %s", string(data))
}

// startAudioRelay starts streaming audio over the provided stream and returns a stopper.
func startAudioRelay(stream *quic.Stream, targetPeerID string) func() {
	stop := make(chan struct{}, 1)

	go func() {
		defer stream.Close()

		currentPosition := getCurrentAudioPosition()
		log.Printf("Starting audio relay for peer %s from position %d bytes", targetPeerID, currentPosition)

		audioStreamer := NewAudioStreamerFromPosition(stream, currentPosition)

		done := make(chan error, 1)
		go func() {
			done <- audioStreamer.StreamAudio()
		}()

		select {
		case err := <-done:
			if err != nil {
				log.Printf("Audio relay streaming failed: %v", err)
			} else {
				log.Printf("Audio relay to peer %s completed normally", targetPeerID)
			}
		case <-stop:
			log.Printf("Audio relay to peer %s stopped due to P2P reconnection", targetPeerID)
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
