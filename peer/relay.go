package main

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/kota-yata/p2p-quic-migration/shared/qswitch"
)

func (p *Peer) openRelayAllowStream() error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection available")
	}
	if p.relayAllowStream != nil {
		return nil
	}
	s, err := p.relayConn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	p.relayAllowStream = s
	return nil
}

func (p *Peer) sendRelayAllowlistUpdate() error {
	if err := p.openRelayAllowStream(); err != nil {
		return err
	}
	addrs := make([]qswitch.Address, 0, len(p.endpoints)*2)
	for _, ep := range p.endpoints {
		if a, ok := toProtoAddr(ep.observed); ok {
			addrs = append(addrs, a)
		}
		if ep.hasLocal && p.ownObservedIP != nil {
			if host, _, err := net.SplitHostPort(ep.observed); err == nil {
				peerObsIP := net.ParseIP(host)
				if sameIPFamily(peerObsIP, p.ownObservedIP) && ipEqual(peerObsIP, p.ownObservedIP) {
					if a, ok := toProtoAddr(ep.local); ok {
						addrs = append(addrs, a)
					}
				}
			}
		}
	}
	msg := qswitch.RelayAllowlistSet{Addresses: addrs}
	if err := qswitch.WriteMessage(p.relayAllowStream, msg); err != nil {
		return err
	}
	return nil
}

func (p *Peer) switchToAudioRelay(targetPeerID uint32) error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection available for relay")
	}

	stream, err := p.relayConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open audio relay stream: %v", err)
	}

	if err := qswitch.WriteMessage(stream, qswitch.AudioRelayReq{TargetPeerID: targetPeerID}); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send audio relay request: %v", err)
	}

	log.Printf("Switched to audio relay mode for peer %d", targetPeerID)

	p.audioRelayStop = startAudioRelay(stream, targetPeerID)
	return nil
}

// acceptRelayStreams accepts additional audio streams initiated by the relay server.
func (p *Peer) acceptRelayStreams() {
	if p.relayConn == nil {
		return
	}
	for {
		stream, err := p.relayConn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Error accepting stream from relay: %v", err)
			return
		}
		log.Printf("Accepted incoming relay stream from server (relay)")
		go handleIncomingAudioStream(stream, "relay")
	}
}
