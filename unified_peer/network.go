package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func (p *UnifiedPeer) setupTLS(certPath, keyPath string) error {
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return fmt.Errorf("failed to load TLS certificate: %v", err)
		}
		p.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"quic-p2p"},
		}
	} else {
		// Insecure mode for development
		p.tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-p2p"},
		}
	}
	return nil
}

func (p *UnifiedPeer) setupQUIC() {
	p.quicConfig = &quic.Config{
		MaxIdleTimeout:             30 * time.Second,
		MaxIncomingStreams:         1000,
		MaxIncomingUniStreams:      1000,
		HandshakeIdleTimeout:       10 * time.Second,
		MaxStreamReceiveWindow:     6 * 1024 * 1024,
		MaxConnectionReceiveWindow: 15 * 1024 * 1024,
		EnableDatagrams:            true,
	}
}

func (p *UnifiedPeer) runListener(ctx context.Context) {
	listener, err := quic.ListenAddr(p.listenAddr, p.tlsConfig, p.quicConfig)
	if err != nil {
		log.Printf("Failed to start listener: %v", err)
		return
	}
	defer listener.Close()

	log.Printf("Listening on %s", p.listenAddr)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn, err := listener.Accept(ctx)
			if err != nil {
				log.Printf("Failed to accept connection: %v", err)
				continue
			}

			log.Printf("Accepted connection from %s", conn.RemoteAddr())
			
			// Store connection
			p.connMutex.Lock()
			p.connections[conn.RemoteAddr().String()] = conn
			p.connMutex.Unlock()

			// Handle connection in goroutine
			go p.handleConnection(ctx, conn)
		}
	}
}

func (p *UnifiedPeer) connectToServer(ctx context.Context) error {
	log.Printf("Connecting to server at %s", p.serverAddr)
	
	conn, err := quic.DialAddr(ctx, p.serverAddr, p.tlsConfig, p.quicConfig)
	if err != nil {
		return fmt.Errorf("failed to dial server: %v", err)
	}

	log.Printf("Connected to server at %s", p.serverAddr)
	
	// Store connection
	p.connMutex.Lock()
	p.connections[conn.RemoteAddr().String()] = conn
	p.connMutex.Unlock()

	// Handle connection
	go p.handleConnection(ctx, conn)
	
	return nil
}

func (p *UnifiedPeer) handleConnection(ctx context.Context, conn quic.Connection) {
	defer conn.CloseWithError(0, "peer disconnected")

	// Handle incoming streams
	go func() {
		for {
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				log.Printf("Failed to accept stream: %v", err)
				return
			}
			
			select {
			case p.incomingStreams <- stream:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Keep connection alive
	<-ctx.Done()
}

func (p *UnifiedPeer) handleNetworkChange(oldAddr, newAddr string) {
	log.Printf("Handling network change from %s to %s", oldAddr, newAddr)
	
	// Migrate all existing connections
	p.connMutex.RLock()
	connections := make([]quic.Connection, 0, len(p.connections))
	for _, conn := range p.connections {
		connections = append(connections, conn)
	}
	p.connMutex.RUnlock()

	for _, conn := range connections {
		if err := p.migrateConnection(conn, newAddr); err != nil {
			log.Printf("Failed to migrate connection to %s: %v", conn.RemoteAddr(), err)
		}
	}
}

func (p *UnifiedPeer) migrateConnection(conn quic.Connection, newAddr string) error {
	// Parse new address
	udpAddr, err := net.ResolveUDPAddr("udp", newAddr+":0")
	if err != nil {
		return fmt.Errorf("failed to resolve new address: %v", err)
	}

	// Create new UDP connection
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection: %v", err)
	}

	// Create new transport with the new UDP connection
	newTransport := &quic.Transport{
		Conn: udpConn,
	}

	// Update transport reference
	p.transport = newTransport

	log.Printf("Successfully migrated connection to %s", newAddr)
	return nil
}

func (p *UnifiedPeer) openStream(ctx context.Context, remoteAddr string) (quic.Stream, error) {
	p.connMutex.RLock()
	conn, exists := p.connections[remoteAddr]
	p.connMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no connection to %s", remoteAddr)
	}

	return conn.OpenStreamSync(ctx)
}

func (p *UnifiedPeer) closeConnection(remoteAddr string) {
	p.connMutex.Lock()
	defer p.connMutex.Unlock()

	if conn, exists := p.connections[remoteAddr]; exists {
		conn.CloseWithError(0, "connection closed")
		delete(p.connections, remoteAddr)
	}
}

func (p *UnifiedPeer) getConnections() []string {
	p.connMutex.RLock()
	defer p.connMutex.RUnlock()

	addrs := make([]string, 0, len(p.connections))
	for addr := range p.connections {
		addrs = append(addrs, addr)
	}
	return addrs
}