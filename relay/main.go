package main

import (
    "context"
    "crypto/tls"
    "flag"
    "fmt"
    "io"
    "log"
    "net"

    proto "github.com/kota-yata/p2p-quic-migration/shared/cmp9protocol"
    "github.com/quic-go/quic-go"
)

var registry *PeerRegistry

func main() {
	key := flag.String("key", "", "TLS key (requires -cert option)")
	cert := flag.String("cert", "", "TLS certificate (requires -key option)")
	addr := flag.String("addr", "0.0.0.0:12345", "Address to bind to")
	flag.Parse()

	registry = NewPeerRegistry()

	cer, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Fatal("load cert: ", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cer},
		NextProtos:   []string{"p2p-quic"},
	}

	quicConf := &quic.Config{}

	ln, err := quic.ListenAddr(*addr, tlsConf, quicConf)
	if err != nil {
		log.Fatal("listen addr: ", err)
	}
	defer ln.Close()

	log.Printf("Start Relay Server: %s", *addr)

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Fatal("accept: ", err)
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn *quic.Conn) {
	registry.AddPeer(conn)

	defer func() {
		registry.RemovePeer(conn)
		conn.CloseWithError(0, "")
	}()

	log.Printf("New Connection from: %s", conn.RemoteAddr())

	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("Accept stream error: %v", err)
			return
		}

		go handleStream(conn, stream)
	}
}

func handleStream(conn *quic.Conn, stream *quic.Stream) {
    defer stream.Close()
    log.Printf("Received stream from %s", conn.RemoteAddr())

    // Read the first framed message to determine stream intent.
    msg, err := proto.ReadMessage(stream)
    if err != nil {
        log.Printf("Failed to read first message from %s: %v", conn.RemoteAddr(), err)
        return
    }

    switch m := msg.(type) {
    case proto.RelayAllowlistSet:
        // Convert to string addresses and set for this connection.
        addrs := make([]string, 0, len(m.Addresses))
        for _, a := range m.Addresses {
            addrs = append(addrs, net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port)))
        }
        registry.SetRelayAllowList(conn, addrs)
        log.Printf("Updated allow list for %s with %d entries", conn.RemoteAddr(), len(addrs))
        // Allow multiple updates on the same stream until EOF.
        for {
            next, err := proto.ReadMessage(stream)
            if err != nil {
                if err == io.EOF {
                    return
                }
                log.Printf("Error reading allowlist update from %s: %v", conn.RemoteAddr(), err)
                return
            }
            if upd, ok := next.(proto.RelayAllowlistSet); ok {
                addrs = addrs[:0]
                for _, a := range upd.Addresses {
                    addrs = append(addrs, net.JoinHostPort(a.IP.String(), fmt.Sprintf("%d", a.Port)))
                }
                registry.SetRelayAllowList(conn, addrs)
                log.Printf("Updated allow list for %s with %d entries", conn.RemoteAddr(), len(addrs))
            } else {
                log.Printf("Unexpected message on allowlist stream from %s: %T", conn.RemoteAddr(), next)
            }
        }
    case proto.AudioRelayReq:
        // Find a target connection that explicitly allows this source address.
        sourceAddr := conn.RemoteAddr().String()
        targetConn, _, ok := registry.FindTargetByAllowedSource(sourceAddr)
        if !ok {
            log.Printf("No target peer found whose allow list permits source %s; dropping relay request", sourceAddr)
            return
        }
        targetStream, err := targetConn.OpenStreamSync(context.Background())
        if err != nil {
            log.Printf("Failed to open stream to target for %s: %v", sourceAddr, err)
            return
        }
        defer targetStream.Close()

        // Relay raw bytes remaining on this stream to the target stream.
        buf := make([]byte, 4096)
        nbytes, err := io.CopyBuffer(targetStream, stream, buf)
        if err != nil {
            log.Printf("Relay copy error from %s: %v", sourceAddr, err)
            return
        }
        log.Printf("Relayed %d bytes from %s", nbytes, sourceAddr)
    default:
        log.Printf("Unexpected first message on stream from %s: %T", conn.RemoteAddr(), m)
    }
}
