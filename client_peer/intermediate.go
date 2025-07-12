package main

import (
	"crypto/tls"
	"log"

	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

// ClientPeerHandler implements the PeerHandler interface for client-specific behavior
type ClientPeerHandler struct {
	transport    *quic.Transport
	tlsConfig    *tls.Config
	quicConfig   *quic.Config
	hasConnected bool
}

func NewClientPeerHandler(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) *ClientPeerHandler {
	return &ClientPeerHandler{
		transport:    transport,
		tlsConfig:    tlsConfig,
		quicConfig:   quicConfig,
		hasConnected: false,
	}
}

func (cph *ClientPeerHandler) HandleInitialPeers(peers []shared.PeerInfo) {
	for _, peer := range peers {
		go attemptNATHolePunch(*cph.transport, peer.Address, cph.tlsConfig, cph.quicConfig, holePunchCompleted)
	}

	if len(peers) > 0 && !cph.hasConnected {
		cph.connectToPeerWithDelay(peers[0].Address)
	}
}

func (cph *ClientPeerHandler) HandleNewPeer(peer shared.PeerInfo) {
	go attemptNATHolePunch(*cph.transport, peer.Address, cph.tlsConfig, cph.quicConfig, holePunchCompleted)

	if !cph.hasConnected {
		cph.connectToPeerWithDelay(peer.Address)
	}
}

func (cph *ClientPeerHandler) connectToPeerWithDelay(peerAddr string) {
	// No longer needed - hole punching will handle the connection directly
	log.Printf("Connection will be established during hole punching to %s", peerAddr)
	cph.hasConnected = true
}