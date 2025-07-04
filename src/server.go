//go:build server
// +build server

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"log"

	"github.com/quic-go/quic-go"
)

func main() {
	key := flag.String("key", "", "TLS key (requires -cert option)")
	cert := flag.String("cert", "", "TLS certificate (requires -key option)")
	flag.Parse()

	cer, err := tls.LoadX509KeyPair(*cert, *key)
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cer},
		NextProtos:   []string{"p2p-quic"},
	}
	quicConf := &quic.Config{}

	ln, err := quic.ListenAddr("127.0.0.1:12345", tlsConf, quicConf)
	if err != nil {
		log.Fatal("listen addr: ", err)
	}
	defer ln.Close()

	log.Print("Start Server: 127.0.0.1:12345")

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Fatal("accept: ", err)
		}

		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Fatal("accept stream: ", err)
		}

		log.Print("New Client Connection Accepted")
		go func(stream *quic.Stream) {
			s := bufio.NewScanner(stream)
			for s.Scan() {
				msg := s.Text()
				log.Printf("Accept Message: `%s`", msg)
				_, err := stream.Write([]byte(msg))
				if err != nil {
					log.Print("write: ", err)
				}
			}
		}(stream)
	}
}
