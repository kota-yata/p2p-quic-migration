package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	network_monitor "github.com/kota-yata/p2p-quic-migration/peer/network"
	"github.com/quic-go/quic-go"
)

const (
	serverPort               = 1234
	connectionTimeout        = 10 * time.Second
	observedAddressMaxChecks = 10
)

type config struct {
	keyFile    string
	certFile   string
	serverAddr string
}

type app struct {
	cfg        config
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	udpConn    *net.UDPConn
	transport  *quic.Transport
	conn       *quic.Conn
	monitor    *network_monitor.NetworkMonitor
}

func main() {
	cfg := parseFlags()

	a := &app{cfg: cfg}
	if err := a.run(); err != nil {
		log.Fatalf("connection migration demo failed: %v", err)
	}
}

func parseFlags() config {
	key := flag.String("key", "server.key", "TLS key (requires -cert option)")
	cert := flag.String("cert", "server.crt", "TLS certificate (requires -key option)")
	serverAddr := flag.String("serverAddr", "203.178.143.72:12345", "Address to intermediate server")
	flag.Parse()
	return config{keyFile: *key, certFile: *cert, serverAddr: *serverAddr}
}

func (a *app) run() error {
	if err := a.setupTLS(); err != nil {
		return err
	}
	if err := a.setupTransport(); err != nil {
		return err
	}
	defer a.cleanup()

	if err := a.connectToServer(); err != nil {
		return err
	}
	defer a.conn.CloseWithError(0, "")

	waitForObservedAddress(a.conn)

	a.monitor = network_monitor.NewNetworkMonitor(a.onAddrChange)
	if err := a.monitor.Start(); err != nil {
		return fmt.Errorf("failed to start network monitor: %v", err)
	}
	defer a.monitor.Stop()

	log.Println("Connected to intermediate server. Waiting for primary IP changes...")
	log.Println("Press Ctrl+C to exit.")

	// Start sending a small heartbeat message periodically over a stream.
	stopHeartbeat := a.startHeartbeat()
	defer stopHeartbeat()

	// Wait for termination signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down...")
	return nil
}

func (a *app) setupTLS() error {
	cer, err := tls.LoadX509KeyPair(a.cfg.certFile, a.cfg.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load certificate: %v", err)
	}
	a.tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cer},
		NextProtos:         []string{"p2p-quic"},
	}
	a.quicConfig = &quic.Config{
		AddressDiscoveryMode: 1,
		KeepAlivePeriod:      30 * time.Second,
		MaxIdleTimeout:       5 * time.Minute,
	}
	return nil
}

func (a *app) setupTransport() error {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: serverPort, IP: net.IPv4zero})
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %v", err)
	}
	a.udpConn = udp
	a.transport = &quic.Transport{Conn: udp}
	return nil
}

func (a *app) cleanup() {
	if a.transport != nil {
		a.transport.Close()
	}
	if a.udpConn != nil {
		a.udpConn.Close()
	}
}

func (a *app) connectToServer() error {
	serverAddr, err := net.ResolveUDPAddr("udp", a.cfg.serverAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve server address: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()
	conn, err := a.transport.Dial(ctx, serverAddr, a.tlsConfig, a.quicConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to intermediate server: %v", err)
	}
	a.conn = conn
	log.Printf("Connected to intermediate server at %s", a.cfg.serverAddr)
	return nil
}

func waitForObservedAddress(conn *quic.Conn) {
	for i := 0; i < observedAddressMaxChecks; i++ {
		if observedAddr := conn.GetObservedAddress(); observedAddr != nil {
			log.Printf("Observed address: %s", observedAddr.String())
			break
		}
	}
}

// startHeartbeat opens a stream and periodically writes a small text
// message to keep traffic flowing. Returns a stopper function.
func (a *app) startHeartbeat() func() {
	if a.conn == nil {
		return func() {}
	}

	stream, err := a.conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Printf("Failed to open heartbeat stream: %v", err)
		return func() {}
	}

	stop := make(chan struct{}, 1)

	go func() {
		defer stream.Close()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		payload := []byte("HB\n")
		for {
			select {
			case <-ticker.C:
				// Avoid blocking forever if the peer isn't reading.
				_ = stream.SetWriteDeadline(time.Now().Add(1 * time.Second))
				if _, err := stream.Write(payload); err != nil {
					log.Printf("Heartbeat write failed: %v", err)
					return
				}
				log.Printf("Heartbeat sent")
			case <-stop:
				return
			}
		}
	}()

	return func() {
		select {
		case stop <- struct{}{}:
		default:
		}
	}
}

// onAddrChange performs QUIC connection migration to the intermediate server
// only when the primary IP address has changed.
func (a *app) onAddrChange(oldAddr, newAddr net.IP) {
	log.Printf("Primary IP changed: %s -> %s", oldAddr, newAddr)
	// if a.conn == nil || a.conn.Context().Err() != nil {
	// 	log.Printf("Connection closed; skipping migration")
	// 	return
	// }

	if err := a.migrateIntermediateConnection(newAddr); err != nil {
		log.Printf("Migration failed: %v", err)
		return
	}
	log.Printf("Migration to new local address %s successful", newAddr)
}

func (a *app) migrateIntermediateConnection(newAddr net.IP) error {
	if a.conn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

	newUDP, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return fmt.Errorf("failed to create new UDP socket: %v", err)
	}
	newTransport := &quic.Transport{Conn: newUDP}

	path, err := a.conn.AddPath(newTransport)
	if err != nil {
		newUDP.Close()
		return fmt.Errorf("failed to add new path: %v")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Printf("Probing new path from %s to server", newAddr)
	if err := path.Probe(ctx); err != nil {
		newUDP.Close()
		return fmt.Errorf("failed to probe new path: %v", err)
	}

	log.Printf("Switching to new path")
	if err := path.Switch(); err != nil {
		_ = path.Close()
		newUDP.Close()
		return fmt.Errorf("failed to switch to new path: %v", err)
	}

	// Update our active transport and socket references for any future operations
	a.transport = newTransport
	a.udpConn = newUDP
	return nil
}
