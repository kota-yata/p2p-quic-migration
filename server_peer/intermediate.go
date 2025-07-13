package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"

	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

type ServerPeerHandler struct {
	transport        *quic.Transport
	tlsConfig        *tls.Config
	quicConfig       *quic.Config
	intermediateConn *quic.Conn
	audioRelay       *AudioRelay
}

func NewServerPeerHandler(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config, intermediateConn *quic.Conn) *ServerPeerHandler {
	return &ServerPeerHandler{
		transport:        transport,
		tlsConfig:        tlsConfig,
		quicConfig:       quicConfig,
		intermediateConn: intermediateConn,
	}
}

func (sph *ServerPeerHandler) HandleInitialPeers(peers []shared.PeerInfo) {
	for _, peer := range peers {
		sph.startHolePunching(peer.Address)
	}
}

func (sph *ServerPeerHandler) HandleNewPeer(peer shared.PeerInfo) {
	sph.startHolePunching(peer.Address)
}

func (sph *ServerPeerHandler) startHolePunching(peerAddr string) {
	go attemptNATHolePunch(sph.transport, peerAddr, sph.tlsConfig, sph.quicConfig, connectionEstablished)
}

func (sph *ServerPeerHandler) HandleNetworkChange(peerID, oldAddr, newAddr string) {
	log.Printf("Server received network change notification: peer %s changed from %s to %s", peerID, oldAddr, newAddr)

	if err := sph.switchToAudioRelay(peerID); err != nil {
		log.Printf("Failed to switch to audio relay: %v", err)
		return
	}

	log.Printf("Starting new hole punching to updated address: %s", newAddr)
	sph.startHolePunching(newAddr)
}

func (sph *ServerPeerHandler) switchToAudioRelay(targetPeerID string) error {
	if sph.intermediateConn == nil {
		return fmt.Errorf("no intermediate connection available")
	}

	stream, err := sph.intermediateConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open audio relay stream: %v", err)
	}

	relayRequest := fmt.Sprintf("AUDIO_RELAY|%s", targetPeerID)
	_, err = stream.Write([]byte(relayRequest))
	if err != nil {
		stream.Close()
		return fmt.Errorf("failed to send audio relay request: %v", err)
	}

	log.Printf("Switched to audio relay mode for peer %s", targetPeerID)

	sph.audioRelay = &AudioRelay{
		stream:       stream,
		targetPeerID: targetPeerID,
	}

	go sph.audioRelay.StartRelaying()

	return nil
}

type AudioRelay struct {
	stream       *quic.Stream
	targetPeerID string
}

func (ar *AudioRelay) StartRelaying() {
	defer ar.stream.Close()

	audioStreamer := NewAudioStreamer(ar.stream)
	if err := audioStreamer.StreamAudio(); err != nil {
		log.Printf("Audio relay streaming failed: %v", err)
	}

	log.Printf("Audio relay to peer %s completed", ar.targetPeerID)
}
