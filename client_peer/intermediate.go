package main

import (
	"crypto/tls"
	"log"

	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

type ClientPeerHandler struct {
	transport    *quic.Transport
	tlsConfig    *tls.Config
	quicConfig   *quic.Config
	hasConnected bool
	knownPeers   map[string]shared.PeerInfo
}

func NewClientPeerHandler(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) *ClientPeerHandler {
	return &ClientPeerHandler{
		transport:    transport,
		tlsConfig:    tlsConfig,
		quicConfig:   quicConfig,
		hasConnected: false,
		knownPeers:   make(map[string]shared.PeerInfo),
	}
}

func (cph *ClientPeerHandler) HandleInitialPeers(peers []shared.PeerInfo) {
	for _, peer := range peers {
		cph.knownPeers[peer.ID] = peer
		go attemptNATHolePunch(cph.transport, peer.Address, cph.tlsConfig, cph.quicConfig, holePunchCompleted)
	}

	if len(peers) > 0 && !cph.hasConnected {
		cph.connectToPeerWithDelay(peers[0].Address)
	}
}

func (cph *ClientPeerHandler) HandleNewPeer(peer shared.PeerInfo) {
	cph.knownPeers[peer.ID] = peer
	go attemptNATHolePunch(cph.transport, peer.Address, cph.tlsConfig, cph.quicConfig, holePunchCompleted)

	if !cph.hasConnected {
		cph.connectToPeerWithDelay(peer.Address)
	}
}

func (cph *ClientPeerHandler) connectToPeerWithDelay(peerAddr string) {
	log.Printf("Connection will be established during hole punching to %s", peerAddr)
	cph.hasConnected = true
}

func (cph *ClientPeerHandler) StartHolePunchingToAllPeers(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	log.Printf("Client starting hole punching to all known peers after network change")

	if len(cph.knownPeers) == 0 {
		log.Printf("No known peers to hole punch to")
		return
	}

	// Update transport references for new network interface
	cph.transport = transport
	cph.tlsConfig = tlsConfig
	cph.quicConfig = quicConfig

	for peerID, peer := range cph.knownPeers {
		log.Printf("Client starting hole punch to peer %s at %s", peerID, peer.Address)
		go attemptNATHolePunch(transport, peer.Address, tlsConfig, quicConfig, holePunchCompleted)
	}
}
