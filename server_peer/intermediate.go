package main

import (
	"crypto/tls"

	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

// ServerPeerHandler implements the PeerHandler interface for server-specific behavior
type ServerPeerHandler struct {
	transport  *quic.Transport
	tlsConfig  *tls.Config
	quicConfig *quic.Config
}

func NewServerPeerHandler(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) *ServerPeerHandler {
	return &ServerPeerHandler{
		transport:  transport,
		tlsConfig:  tlsConfig,
		quicConfig: quicConfig,
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