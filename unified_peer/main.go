package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/quic-go/quic-go"
)

type PeerMode int

const (
	ModeServer PeerMode = iota
	ModeClient
	ModeBidirectional
)

type UnifiedPeer struct {
	mode           PeerMode
	serverAddr     string
	listenAddr     string
	transport      *quic.Transport
	tlsConfig      *tls.Config
	quicConfig     *quic.Config
	networkMonitor *NetworkMonitor
	gstreamer      *GStreamerManager
	peerHandler    *PeerHandler
	
	// Connection management
	connections    map[string]quic.Connection
	connMutex      sync.RWMutex
	
	// Stream management
	incomingStreams chan quic.Stream
	outgoingStreams chan quic.Stream
	
	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type Config struct {
	Mode           string
	ServerAddr     string
	ListenAddr     string
	TLSCertPath    string
	TLSKeyPath     string
	EnableVideo    bool
	EnableAudio    bool
	AudioSource    string
	VideoSource    string
	NetworkMonitor bool
}

func main() {
	var config Config
	
	flag.StringVar(&config.Mode, "mode", "bidirectional", "Peer mode: server, client, or bidirectional")
	flag.StringVar(&config.ServerAddr, "server", "localhost:4433", "Server address to connect to")
	flag.StringVar(&config.ListenAddr, "listen", ":4433", "Address to listen on")
	flag.StringVar(&config.TLSCertPath, "cert", "", "Path to TLS certificate")
	flag.StringVar(&config.TLSKeyPath, "key", "", "Path to TLS private key")
	flag.BoolVar(&config.EnableVideo, "video", true, "Enable video streaming")
	flag.BoolVar(&config.EnableAudio, "audio", true, "Enable audio streaming")
	flag.StringVar(&config.AudioSource, "audio-source", "../static/output.mp4", "Audio source file")
	flag.StringVar(&config.VideoSource, "video-source", "../static/output.mp4", "Video source file")
	flag.BoolVar(&config.NetworkMonitor, "network-monitor", true, "Enable network monitoring")
	flag.Parse()

	peer, err := NewUnifiedPeer(config)
	if err != nil {
		log.Fatalf("Failed to create unified peer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-signalChan
		log.Println("Received shutdown signal, shutting down gracefully...")
		cancel()
	}()

	if err := peer.Run(ctx); err != nil {
		log.Fatalf("Peer failed to run: %v", err)
	}
}

func NewUnifiedPeer(config Config) (*UnifiedPeer, error) {
	var mode PeerMode
	switch config.Mode {
	case "server":
		mode = ModeServer
	case "client":
		mode = ModeClient
	case "bidirectional":
		mode = ModeBidirectional
	default:
		return nil, fmt.Errorf("invalid mode: %s", config.Mode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	
	peer := &UnifiedPeer{
		mode:            mode,
		serverAddr:      config.ServerAddr,
		listenAddr:      config.ListenAddr,
		connections:     make(map[string]quic.Connection),
		incomingStreams: make(chan quic.Stream, 100),
		outgoingStreams: make(chan quic.Stream, 100),
		ctx:             ctx,
		cancel:          cancel,
	}

	// Setup TLS configuration
	if err := peer.setupTLS(config.TLSCertPath, config.TLSKeyPath); err != nil {
		return nil, fmt.Errorf("failed to setup TLS: %v", err)
	}

	// Setup QUIC configuration
	peer.setupQUIC()

	// Setup GStreamer manager
	gstreamerConfig := GStreamerConfig{
		EnableAudio:  config.EnableAudio,
		EnableVideo:  config.EnableVideo,
		AudioSource:  config.AudioSource,
		VideoSource:  config.VideoSource,
	}
	peer.gstreamer = NewGStreamerManager(gstreamerConfig)

	// Setup network monitor for client/bidirectional modes
	if config.NetworkMonitor && (mode == ModeClient || mode == ModeBidirectional) {
		peer.networkMonitor = NewNetworkMonitor(peer.handleNetworkChange)
	}

	// Setup peer handler for P2P discovery
	peer.peerHandler = NewPeerHandler(config.ServerAddr)

	return peer, nil
}

func (p *UnifiedPeer) Run(ctx context.Context) error {
	p.wg.Add(1)
	defer p.wg.Done()

	log.Printf("Starting unified peer in %v mode", p.mode)

	// Start network monitoring if enabled
	if p.networkMonitor != nil {
		go p.networkMonitor.Start(ctx)
	}

	// Start appropriate services based on mode
	switch p.mode {
	case ModeServer:
		return p.runServerMode(ctx)
	case ModeClient:
		return p.runClientMode(ctx)
	case ModeBidirectional:
		return p.runBidirectionalMode(ctx)
	}

	return nil
}

func (p *UnifiedPeer) runServerMode(ctx context.Context) error {
	// Start listening for incoming connections
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runListener(ctx)
	}()

	// Start stream handlers
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.handleIncomingStreams(ctx)
	}()

	// Wait for context cancellation
	<-ctx.Done()
	p.wg.Wait()
	return nil
}

func (p *UnifiedPeer) runClientMode(ctx context.Context) error {
	// Connect to server
	if err := p.connectToServer(ctx); err != nil {
		return fmt.Errorf("failed to connect to server: %v", err)
	}

	// Start stream handlers
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.handleIncomingStreams(ctx)
	}()

	// Wait for context cancellation
	<-ctx.Done()
	p.wg.Wait()
	return nil
}

func (p *UnifiedPeer) runBidirectionalMode(ctx context.Context) error {
	// Start both server and client functionality
	p.wg.Add(2)
	
	go func() {
		defer p.wg.Done()
		p.runListener(ctx)
	}()
	
	go func() {
		defer p.wg.Done()
		if err := p.connectToServer(ctx); err != nil {
			log.Printf("Failed to connect to server in bidirectional mode: %v", err)
		}
	}()

	// Start stream handlers
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.handleIncomingStreams(ctx)
	}()

	// Wait for context cancellation
	<-ctx.Done()
	p.wg.Wait()
	return nil
}