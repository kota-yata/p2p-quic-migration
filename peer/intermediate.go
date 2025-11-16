// Communication with intermediate server

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
    knownPeers       map[string]shared.PeerInfo
}

func NewServerPeerHandler(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config, intermediateConn *quic.Conn) *ServerPeerHandler {
    return &ServerPeerHandler{
        transport:        transport,
        tlsConfig:        tlsConfig,
        quicConfig:       quicConfig,
        intermediateConn: intermediateConn,
        knownPeers:       make(map[string]shared.PeerInfo),
    }
}

func (sph *ServerPeerHandler) HandleInitialPeers(peers []shared.PeerInfo) {
    for _, peer := range peers {
        sph.knownPeers[peer.ID] = peer
        sph.startHolePunching(peer.Address)
    }
}

func (sph *ServerPeerHandler) HandleNewPeer(peer shared.PeerInfo) {
    sph.knownPeers[peer.ID] = peer
    sph.startHolePunching(peer.Address)
}

func (sph *ServerPeerHandler) startHolePunching(peerAddr string) {
    go attemptNATHolePunch(sph.transport, peerAddr, sph.tlsConfig, sph.quicConfig, connectionEstablished)
}

func (sph *ServerPeerHandler) StopAudioRelay() {
    if sph.audioRelay != nil {
        log.Printf("Stopping audio relay due to P2P reconnection")
        sph.audioRelay.Stop()
        sph.audioRelay = nil
    }
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

func (sph *ServerPeerHandler) StartHolePunchingToAllPeers(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
    log.Printf("Server starting hole punching to all known peers after network change")

    if len(sph.knownPeers) == 0 {
        log.Printf("No known peers to hole punch to")
        return
    }

    // Update transport references for new network interface
    sph.transport = transport
    sph.tlsConfig = tlsConfig
    sph.quicConfig = quicConfig

    for peerID, peer := range sph.knownPeers {
        log.Printf("Server starting hole punch to peer %s at %s", peerID, peer.Address)
        go attemptNATHolePunch(transport, peer.Address, tlsConfig, quicConfig, connectionEstablished)
    }
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
        stopChan:     make(chan bool, 1),
    }

    go sph.audioRelay.StartRelaying()

    return nil
}

type AudioRelay struct {
    stream       *quic.Stream
    targetPeerID string
    stopChan     chan bool
}

func (ar *AudioRelay) StartRelaying() {
    defer ar.stream.Close()

    currentPosition := getCurrentAudioPosition()
    log.Printf("Starting audio relay for peer %s from position %d bytes", ar.targetPeerID, currentPosition)

    audioStreamer := NewAudioStreamerFromPosition(ar.stream, currentPosition)

    // Run audio streaming in a goroutine so we can monitor for stop signal
    done := make(chan error, 1)
    go func() {
        done <- audioStreamer.StreamAudio()
    }()

    // Wait for either completion or stop signal
    select {
    case err := <-done:
        if err != nil {
            log.Printf("Audio relay streaming failed: %v", err)
        } else {
            log.Printf("Audio relay to peer %s completed normally", ar.targetPeerID)
        }
    case <-ar.stopChan:
        log.Printf("Audio relay to peer %s stopped due to P2P reconnection", ar.targetPeerID)
        // Close the stream to interrupt the audio streaming
        ar.stream.Close()
        return
    }
}

func (ar *AudioRelay) Stop() {
    select {
    case ar.stopChan <- true:
        log.Printf("Sent stop signal to audio relay for peer %s", ar.targetPeerID)
    default:
        // Channel might be full or relay already stopped
    }
}

