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
    ip := determineLocalIPv4ForRemote(a.cfg.serverAddr)
    laddr := &net.UDPAddr{IP: ip, Port: serverPort}
    udp, err := net.ListenUDP("udp4", laddr)
    if err != nil {
        return fmt.Errorf("failed to listen on UDP %s: %v", laddr.String(), err)
    }
    // Try to bind to interface that owns the IP (Linux/Android)
    ifName, _ := ifaceNameForIP(ip)
    if ifName != "" {
        if err := bindToDevice(udp, ifName); err != nil {
            log.Printf("Warning: failed to bind UDP socket to interface %s: %v", ifName, err)
        } else {
            log.Printf("UDP socket bound to interface %s", ifName)
        }
    }
    a.udpConn = udp
    a.transport = &quic.Transport{Conn: udp}
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

// onAddrChange performs QUIC connection migration to the intermediate server
// only when the primary IP address has changed.
func (a *app) onAddrChange(oldAddr, newAddr string) {
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

func (a *app) migrateIntermediateConnection(newAddr string) error {
	if a.conn.Context().Err() != nil {
		return fmt.Errorf("connection is already closed, cannot migrate")
	}

    // Bind the new UDP socket to the provided local IP so the probe
    // originates from the intended interface.
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
        // Fallback: let OS decide if parsing failed
        laddr = &net.UDPAddr{Port: 0}
        network = "udp4"
        log.Printf("Warning: failed to parse newAddr '%s'; falling back to default bind", newAddr)
    }

    newUDP, err := net.ListenUDP(network, laddr)
    if err != nil {
        return fmt.Errorf("failed to create new UDP socket: %v", err)
    }
    ifName, _ := ifaceNameForIP(laddr.IP)
    if ifName != "" {
        if err := bindToDevice(newUDP, ifName); err != nil {
            log.Printf("Warning: failed to bind new UDP socket to interface %s: %v", ifName, err)
        } else {
            log.Printf("New UDP socket bound to interface %s", ifName)
        }
    }
	newTransport := &quic.Transport{Conn: newUDP}

    // Pre-warm NAT/routing for the new socket by sending a dummy UDP packet
    if srv, err := net.ResolveUDPAddr("udp4", a.cfg.serverAddr); err == nil {
        _, _ = newUDP.WriteToUDP([]byte("WARMUP"), srv)
        time.Sleep(300 * time.Millisecond)
    } else {
        log.Printf("Warning: failed to resolve server addr for warmup: %v", err)
    }

    path, err := a.conn.AddPath(newTransport)
	if err != nil {
		newUDP.Close()
		return fmt.Errorf("failed to add new path: %v", err)
	}

    // Try immediate switch if old path is likely gone; otherwise fallback to probe
    if err := path.Switch(); err != nil {
        log.Printf("Immediate switch to new path failed: %v; attempting probe/then switch", err)
        var probeErr error
        for i := 1; i <= 3; i++ {
            ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            log.Printf("Probing new path from %s (bind %s) to server %s [attempt %d]", newAddr, newUDP.LocalAddr().String(), a.conn.RemoteAddr(), i)
            probeErr = path.Probe(ctx)
            cancel()
            if probeErr == nil {
                break
            }
            time.Sleep(400 * time.Millisecond)
        }
        if probeErr != nil {
            newUDP.Close()
            return fmt.Errorf("failed to probe new path: %v", probeErr)
        }
        log.Printf("Switching to new path after successful probe")
        if err := path.Switch(); err != nil {
            _ = path.Close()
            newUDP.Close()
            return fmt.Errorf("failed to switch to new path: %v", err)
        }
    } else {
        log.Printf("Switched to new path without prior probe (old path likely down)")
    }

	// Update our active transport and socket references for any future operations
	a.transport = newTransport
	a.udpConn = newUDP
	return nil
}
