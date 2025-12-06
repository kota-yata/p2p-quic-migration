package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func (s *Server) handleNetworkChange(oldAddr, newAddr string) {
	log.Printf("Handling network change from %s to %s", oldAddr, newAddr)

	if s.intermediateConn == nil {
		log.Println("No intermediate connection available for network change")
		return
	}

	if err := s.migrateConnection(newAddr); err != nil {
		log.Printf("Failed to migrate server connection: %v", err)
		return
	}

	log.Printf("Successfully migrated server connection to new address: %s", newAddr)

	if s.peerHandler != nil {
		s.peerHandler.StartHolePunchingToAllPeers(s.transport, s.tlsConfig, s.quicConfig)
	}

	if err := s.sendNetworkChangeNotification(oldAddr, newAddr); err != nil {
		log.Printf("Failed to send server network change notification after migration: %v", err)
	}
}

func (s *Server) migrateConnection(newAddr string) error {
	if s.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

	newUDPConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(newAddr), Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP connection: %v", err)
	}

	newTransport := &quic.Transport{
		Conn: newUDPConn,
	}

	path, err := s.intermediateConn.AddPath(newTransport)
	if err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to add new path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Printf("Probing new path from %s to intermediate server", newAddr)
	if err := path.Probe(ctx); err != nil {
		newUDPConn.Close()
		return fmt.Errorf("failed to probe new path: %v", err)
	} else {
		log.Printf("Path probing succeeded")
	}

	log.Printf("Switching to new path")
	if err := path.Switch(); err != nil {
		if closeErr := path.Close(); closeErr != nil {
			log.Printf("Warning: failed to close path after switch failure: %v", closeErr)
		}
		newUDPConn.Close()
		return fmt.Errorf("failed to switch to new path: %v", err)
	}

	// Update the server's transport and UDP connection used for outgoing connections
	s.transport = newTransport
	s.udpConn = newUDPConn

	return nil
}

func (s *Server) sendNetworkChangeNotification(oldAddr, newAddr string) error {
	if s.intermediateConn.Context().Err() != nil {
		return fmt.Errorf("connection is closed")
	}

	oldFullAddr := oldAddr + ":0"
	newFullAddr := s.intermediateConn.LocalAddr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := s.intermediateConn.OpenStreamSync(ctx)
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
