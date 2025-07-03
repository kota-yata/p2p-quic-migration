package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"

	"github.com/quic-go/quic-go"
)

func main() {
	// Configure TLS with certificate verification skipped
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-example"},
	}

	addr := "127.0.0.1:12345"
	fmt.Printf("Connecting to QUIC server at %s\n", addr)

	conn, err := quic.DialAddr(context.Background(), addr, tlsConfig, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.CloseWithError(0, "")

	fmt.Printf("Connected to server\n")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	message := "Hello from QUIC client!"
	fmt.Printf("Sending: %s\n", message)

	_, err = stream.Write([]byte(message))
	if err != nil {
		log.Fatal(err)
	}

	err = stream.Close()
	if err != nil {
		log.Fatal(err)
	}

	response, err := io.ReadAll(stream)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Received: %s\n", string(response))
}
