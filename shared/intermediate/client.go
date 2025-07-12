package intermediate

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

// Client handles communication with the intermediate server for peer discovery
type Client struct {
	serverAddr string
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	transport  *quic.Transport
}

func NewClient(serverAddr string, tlsConfig *tls.Config, quicConfig *quic.Config, transport *quic.Transport) *Client {
	return &Client{
		serverAddr: serverAddr,
		tlsConfig:  tlsConfig,
		quicConfig: quicConfig,
		transport:  transport,
	}
}

func (c *Client) ConnectToServer() (*quic.Conn, error) {
	serverAddrResolved, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	conn, err := c.transport.Dial(ctx, serverAddrResolved, c.tlsConfig, c.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to intermediate server: %v", err)
	}

	log.Printf("Connected to intermediate server at %s", c.serverAddr)
	return conn, nil
}

func (c *Client) WaitForObservedAddress(conn *quic.Conn) {
	for i := 0; i < observedAddressMaxRetries; i++ {
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address received: %s", observedAddr.String())
			break
		}
	}
}

func (c *Client) ManagePeerDiscovery(conn *quic.Conn, peerHandler PeerHandler) {
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open communication stream: %v", err)
		return
	}
	defer stream.Close()

	if err := c.sendPeerRequest(stream); err != nil {
		log.Printf("Failed to send peer request: %v", err)
		return
	}

	peerManager := &PeerManager{
		peerHandler:    peerHandler,
		isFirstMessage: true,
	}

	peerManager.handlePeerCommunication(stream)
}

func (c *Client) sendPeerRequest(stream *quic.Stream) error {
	if _, err := stream.Write([]byte("GET_PEERS")); err != nil {
		return fmt.Errorf("failed to send GET_PEERS request: %v", err)
	}
	return nil
}

// PeerHandler interface defines how peers are handled differently by client and server
type PeerHandler interface {
	HandleInitialPeers(peers []shared.PeerInfo)
	HandleNewPeer(peer shared.PeerInfo)
}

type PeerManager struct {
	peerHandler    PeerHandler
	isFirstMessage bool
}

func (pm *PeerManager) handlePeerCommunication(stream *quic.Stream) {
	buffer := make([]byte, 4096)

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			log.Printf("Failed to read from intermediate server: %v", err)
			return
		}

		if pm.isFirstMessage {
			pm.handleInitialPeerList(buffer[:n])
			pm.isFirstMessage = false
		} else {
			pm.handlePeerNotification(buffer[:n])
		}
	}
}

func (pm *PeerManager) handleInitialPeerList(data []byte) {
	var peers []shared.PeerInfo
	if err := json.Unmarshal(data, &peers); err != nil {
		log.Printf("Failed to unmarshal peer list: %v", err)
		return
	}

	log.Printf("Received %d peers from intermediate server:", len(peers))
	for _, peer := range peers {
		log.Printf("  Peer: %s (Address: %s)", peer.ID, peer.Address)
	}

	pm.peerHandler.HandleInitialPeers(peers)
}

func (pm *PeerManager) handlePeerNotification(data []byte) {
	var notification shared.PeerNotification
	if err := json.Unmarshal(data, &notification); err != nil {
		log.Printf("Failed to unmarshal peer notification: %v", err)
		return
	}

	log.Printf("Received peer notification - Type: %s, Peer: %s (Address: %s)",
		notification.Type, notification.Peer.ID, notification.Peer.Address)

	if notification.Type == "NEW_PEER" {
		pm.peerHandler.HandleNewPeer(*notification.Peer)
	}
}