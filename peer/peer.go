package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
	"github.com/kota-yata/p2p-quic-migration/shared"
	"github.com/quic-go/quic-go"
)

type ServerConfig struct {
	keyFile    string
	certFile   string
	serverAddr string
	role       string
}

type Peer struct {
	config           *ServerConfig
	tlsConfig        *tls.Config
	quicConfig       *quic.Config
	transport        *quic.Transport
	udpConn          *net.UDPConn
	intermediateConn *quic.Conn
	networkMonitor   *network_monitor.NetworkMonitor
	knownPeers       map[string]shared.PeerInfo
	audioRelayStop   func()
	// hole punch cancellation management
	hpCancels []context.CancelFunc
}

func (s *Peer) Run() error {
	if err := s.setupTLS(); err != nil {
		return fmt.Errorf("failed to setup TLS: %v", err)
	}

	if err := s.setupTransport(); err != nil {
		return fmt.Errorf("failed to setup transport: %v", err)
	}
	defer s.cleanup()

	// Connect to the intermediate server
	intermediateConn, err := ConnectToServer(s.config.serverAddr, s.tlsConfig, s.quicConfig, s.transport)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	defer intermediateConn.CloseWithError(0, "")
	s.intermediateConn = intermediateConn
	WaitForObservedAddress(intermediateConn)

    // init peer discovery and handling
    s.knownPeers = make(map[string]shared.PeerInfo)
    go ManagePeerDiscovery(intermediateConn, s)

    // listen for incoming relay streams from the intermediate server (for receivers)
    go s.acceptRelayStreams()

	// monitor established connections and coordinate cancellation/handling
	go s.monitorConnections()

	s.networkMonitor = network_monitor.NewNetworkMonitor(s.onAddrChange)
	if err := s.networkMonitor.Start(); err != nil {
		return fmt.Errorf("failed to start network monitor: %v", err)
	}
	defer s.networkMonitor.Stop()

	return s.runPeerListener()
}

func (s *Peer) setupTLS() error {
	cer, err := tls.LoadX509KeyPair(s.config.certFile, s.config.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load certificate: %v", err)
	}

	s.tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cer},
		NextProtos:         []string{"p2p-quic"},
	}

	s.quicConfig = &quic.Config{
		AddressDiscoveryMode: 1,
		KeepAlivePeriod:      30 * time.Second,
		MaxIdleTimeout:       5 * time.Minute,
	}

	return nil
}

func (s *Peer) setupTransport() error {
    // Prefer binding to the primary local IPv4 that would be used to reach the server
    // so the QUIC connection originates from a concrete interface, not 0.0.0.0.
    localIP := determineLocalIPv4ForRemote(s.config.serverAddr)
    laddr := &net.UDPAddr{IP: localIP, Port: serverPort}

    udp, err := net.ListenUDP("udp4", laddr)
    if err != nil {
        return fmt.Errorf("failed to listen on UDP %s: %v", laddr.String(), err)
    }
    // On Linux/Android, bind the socket to the specific interface that has this IP
    ifName, _ := ifaceNameForIP(localIP)
    if ifName != "" {
        if err := bindToDevice(udp, ifName); err != nil {
            log.Printf("Warning: failed to bind UDP socket to interface %s: %v", ifName, err)
        } else {
            log.Printf("UDP socket bound to interface %s", ifName)
        }
    }

    s.udpConn = udp
    s.transport = &quic.Transport{Conn: udp}
    log.Printf("UDP transport bound to %s", udp.LocalAddr())
    return nil
}

// determineLocalIPv4ForRemote returns the local IPv4 the OS would use to
// reach the given remote UDP address. Falls back to 0.0.0.0 on failure.
func determineLocalIPv4ForRemote(remote string) net.IP {
    raddr, err := net.ResolveUDPAddr("udp4", remote)
    if err != nil || raddr == nil {
        log.Printf("Warning: failed to resolve remote '%s': %v; using 0.0.0.0", remote, err)
        return net.IPv4zero
    }
    // Create a temporary UDP connection to learn the chosen local address
    c, err := net.DialUDP("udp4", nil, raddr)
    if err != nil {
        log.Printf("Warning: failed to dial remote '%s' to determine local IP: %v; using 0.0.0.0", remote, err)
        return net.IPv4zero
    }
    defer c.Close()
    if la, ok := c.LocalAddr().(*net.UDPAddr); ok && la.IP != nil && la.IP.To4() != nil {
        return la.IP
    }
    return net.IPv4zero
}

func (s *Peer) cleanup() {
	if s.transport != nil {
		s.transport.Close()
	}
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}

func (s *Peer) runPeerListener() error {
	ln, err := s.transport.Listen(s.tlsConfig, s.quicConfig)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	defer ln.Close()

	log.Printf("Start Server: 0.0.0.0:%d", serverPort)

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		// notify accept event; monitor will cancel dialers and handle
		select {
		case connectionEstablished <- connchan{conn: conn, isAcceptor: true}:
		default:
			// if buffer is full, close the new conn to avoid leaks
			log.Printf("connectionEstablished channel full; dropping accept notification")
			conn.CloseWithError(0, "dropped: channel full")
		}
	}
}

func (s *Peer) handleIncomingConnection(conn *quic.Conn) {
	log.Print("New Peer Connection Accepted. Setting up audio streaming...")

	// stop any relay now that direct P2P is up
	s.StopAudioRelay()

	// Since we received the connection, we act as the "acceptor"
	log.Printf("Acting as connection acceptor with role=%s", s.config.role)
	handleCommunicationAsAcceptor(conn, s.config.role)
}

func (p *Peer) handleInitialPeers(peers []shared.PeerInfo) {
	for _, peer := range peers {
		if p.knownPeers != nil {
			p.knownPeers[peer.ID] = peer
		}
		p.startHolePunching(peer.Address)
	}
}

func (p *Peer) handleNewPeer(peer shared.PeerInfo) {
	if p.knownPeers != nil {
		p.knownPeers[peer.ID] = peer
	}
	p.startHolePunching(peer.Address)
}

func (p *Peer) startHolePunching(peerAddr string) {
	// create a cancelable context for this punching attempt
	ctx, cancel := context.WithCancel(context.Background())
	// record cancel so an acceptor success can stop dial attempts
	p.hpCancels = append(p.hpCancels, cancel)
	go attemptNATHolePunch(ctx, p.transport, peerAddr, p.tlsConfig, p.quicConfig, connectionEstablished)
}

func (p *Peer) StopAudioRelay() {
    if p.audioRelayStop != nil {
        log.Printf("Stopping audio relay due to P2P reconnection")
        p.audioRelayStop()
        p.audioRelayStop = nil
    }
}

// acceptRelayStreams listens on the intermediate server connection for incoming
// audio relay streams and starts playback when received.
func (p *Peer) acceptRelayStreams() {
    if p.intermediateConn == nil {
        return
    }
    for {
        stream, err := p.intermediateConn.AcceptStream(context.Background())
        if err != nil {
            log.Printf("Error accepting relay stream from intermediate: %v", err)
            return
        }

        // Only play audio if our role includes receiving
        if p.config != nil && (p.config.role == "receiver" || p.config.role == "both") {
            log.Printf("Received incoming audio relay stream from intermediate server; starting playback")
            // Stop any existing relay playback first
            p.StopAudioRelay()
            p.audioRelayStop = startAudioRelayPlayback(stream)
        } else {
            log.Printf("Role=%s is sender-only; closing incoming relay stream", p.config.role)
            stream.Close()
        }
    }
}

func (p *Peer) HandleNetworkChange(peerID, oldAddr, newAddr string) {
	log.Printf("Network change notification from server: peer %s changed from %s to %s", peerID, oldAddr, newAddr)

	// Only sender/both should stream via relay while reconnecting
	if p.config != nil && (p.config.role == "sender" || p.config.role == "both") {
		if err := p.switchToAudioRelay(peerID); err != nil {
			log.Printf("Failed to switch to audio relay: %v", err)
			return
		}
	} else {
		log.Printf("Role=%s; skipping audio relay during migration", p.config.role)
	}

	log.Printf("Starting new hole punching to updated address: %s", newAddr)
	p.startHolePunching(newAddr)
}

func (p *Peer) StartHolePunchingToAllPeers(transport *quic.Transport, tlsConfig *tls.Config, quicConfig *quic.Config) {
	log.Printf("Server starting hole punching to all known peers after network change")

	if len(p.knownPeers) == 0 {
		log.Printf("No known peers to hole punch to")
		return
	}

	// Update references for new network interface
	p.transport = transport
	p.tlsConfig = tlsConfig
	p.quicConfig = quicConfig

	for peerID, peer := range p.knownPeers {
		log.Printf("Server starting hole punch to peer %s at %s", peerID, peer.Address)
		ctx, cancel := context.WithCancel(context.Background())
		p.hpCancels = append(p.hpCancels, cancel)
		go attemptNATHolePunch(ctx, transport, peer.Address, tlsConfig, quicConfig, connectionEstablished)
	}
}

func (p *Peer) switchToAudioRelay(targetPeerID string) error {
	if p.intermediateConn == nil {
		return fmt.Errorf("no intermediate connection available")
	}

	stream, err := p.intermediateConn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open audio relay stream: %v", err)
	}

	relayRequest := fmt.Sprintf("AUDIO_RELAY|%s", targetPeerID)
	if _, err := stream.Write([]byte(relayRequest)); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send audio relay request: %v", err)
	}

	log.Printf("Switched to audio relay mode for peer %s", targetPeerID)

	// start relay and store stopper
	p.audioRelayStop = startAudioRelay(stream, targetPeerID)
	return nil
}

func (s *Peer) onAddrChange(oldAddr, newAddr string) {
    log.Printf("Handling network change from %s to %s", oldAddr, newAddr)

    if s.intermediateConn == nil {
        log.Println("No intermediate connection available for network change")
        return
    }

    if err := s.migrateIntermediateConnection(newAddr); err != nil {
        log.Printf("Failed to migrate server connection: %v", err)
        return
    }

    log.Printf("Successfully migrated server connection to new address: %s", newAddr)

    if err := s.sendNetworkChangeNotification(oldAddr); err != nil {
        log.Printf("Failed to send server network change notification after migration: %v", err)
    }

    s.StartHolePunchingToAllPeers(s.transport, s.tlsConfig, s.quicConfig)
}

// monitorConnections waits for either acceptor or initiator connection events,
// cancels outstanding hole punching attempts, and hands off to the proper handler.
func (s *Peer) monitorConnections() {
	for evt := range connectionEstablished {
		// cancel any in-flight hole punching attempts
		for _, c := range s.hpCancels {
			c()
		}
		s.hpCancels = nil

		if evt.isAcceptor {
			s.handleIncomingConnection(evt.conn)
		} else {
			// Use remote addr for logging context
			peerAddr := evt.conn.RemoteAddr().String()
			// Stop any server-based audio relay now that direct P2P is up
			s.StopAudioRelay()
			handleCommunicationAsInitiator(evt.conn, peerAddr, s.config.role)
		}
	}
}

func (s *Peer) migrateIntermediateConnection(newAddr string) error {
	if s.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

    // Bind the new UDP socket to the detected local IP to ensure the probe
    // actually uses the intended interface (e.g., Wi‑Fi vs Cellular).
    var laddr *net.UDPAddr
    var network string
    if ip := net.ParseIP(newAddr); ip != nil {
        laddr = &net.UDPAddr{IP: ip, Port: 0}
        if ip.To4() != nil {
            network = "udp4"
        } else {
            network = "udp6"
        }
    } else {
        // Fallback: let OS pick if parsing failed
        laddr = &net.UDPAddr{Port: 0}
        network = "udp4"
        log.Printf("Warning: failed to parse newAddr '%s'; falling back to default bind", newAddr)
    }

    newUDPConn, err := net.ListenUDP(network, laddr)
    if err != nil {
        return fmt.Errorf("failed to create new UDP connection: %v", err)
    }

    // Bind to the specific interface that owns newAddr (Linux/Android)
    ifName, _ := ifaceNameForIP(laddr.IP)
    if ifName != "" {
        if err := bindToDevice(newUDPConn, ifName); err != nil {
            log.Printf("Warning: failed to bind new UDP socket to interface %s: %v", ifName, err)
        } else {
            log.Printf("New UDP socket bound to interface %s", ifName)
        }
    }

    // New path transport (no need to swallow; but harmless if kept default)
    newTransport := &quic.Transport{Conn: newUDPConn}

    // Pre-warm NAT/routing for the new socket by sending a dummy UDP packet
    if srv, err := net.ResolveUDPAddr("udp4", s.config.serverAddr); err == nil {
        _, _ = newUDPConn.WriteToUDP([]byte("WARMUP"), srv)
        time.Sleep(300 * time.Millisecond)
    } else {
        log.Printf("Warning: failed to resolve server addr for warmup: %v", err)
    }

    path, err := s.intermediateConn.AddPath(newTransport)
	if err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to add new path: %v", err)
	}

    // Prefer immediate switch when old path is gone; then validate
    if err := path.Switch(); err != nil {
        log.Printf("Immediate switch to new path failed: %v; attempting probe/then switch", err)
        // Try probing with a couple of short retries to let routes stabilize
        var probeErr error
        for i := 1; i <= 3; i++ {
            ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            log.Printf("Probing new path from %s (bind %s) to intermediate server [attempt %d]", newAddr, newUDPConn.LocalAddr().String(), i)
            probeErr = path.Probe(ctx)
            cancel()
            if probeErr == nil {
                log.Printf("Path probing succeeded on attempt %d", i)
                break
            }
            time.Sleep(400 * time.Millisecond)
        }
        if probeErr != nil {
            newUDPConn.Close()
            return fmt.Errorf("failed to probe new path: %v", probeErr)
        }
        log.Printf("Switching to new path after successful probe")
        if err := path.Switch(); err != nil {
            if closeErr := path.Close(); closeErr != nil {
                log.Printf("Warning: failed to close path after switch failure: %v", closeErr)
            }
            newUDPConn.Close()
            return fmt.Errorf("failed to switch to new path: %v", err)
        }
    } else {
        log.Printf("Switched to new path without prior probe (old path likely down)")
    }

    // Update the server's transport and UDP connection used for outgoing connections
    s.transport = newTransport
	s.udpConn = newUDPConn

    return nil
}

// Send network change notification through the intermediate server
func (s *Peer) sendNetworkChangeNotification(oldAddr string) error {
	if s.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is closed")
	}

	oldFullAddr := oldAddr + ":0"
	// TODO: Remove new address report because the intermediate server won't use this anyway
	// The server looks at the source address of the incoming connection, not newAddr in the payload
	newFullAddr := s.intermediateConn.LocalAddr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := s.intermediateConn.OpenStreamSync(ctx)
	log.Printf("server addr %s, oldAddr %s, newAddr %s", s.intermediateConn.RemoteAddr().String(), oldFullAddr, newFullAddr)
	if err != nil {
		return fmt.Errorf("failed to open stream: %v", err)
	}

	notification := fmt.Sprintf("NETWORK_CHANGE|%s|%s", oldFullAddr, newFullAddr)
	_, err = stream.Write([]byte(notification))
	if err != nil {
		return fmt.Errorf("failed to write notification: %v", err)
	}

	log.Printf("Sent server network change notification to intermediate server: %s -> %s", oldFullAddr, newFullAddr)
	return nil
}
