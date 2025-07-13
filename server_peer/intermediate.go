package main

import (
	"crypto/tls"
	"log"

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

// HandleNetworkChange implements the NetworkChangeHandler interface
func (sph *ServerPeerHandler) HandleNetworkChange(peerID, oldAddr, newAddr string) {
	log.Printf("Server received network change notification: peer %s changed from %s to %s", peerID, oldAddr, newAddr)
	
	// TODO: Implement connection migration logic
	// 1. Switch to relay mode via intermediate server
	// 2. Start new hole punching to the new address
	// 3. Resume direct P2P once connection is established
	
	log.Printf("Starting new hole punching to updated address: %s", newAddr)
	sph.startHolePunching(newAddr)
}