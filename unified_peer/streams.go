package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/quic-go/quic-go"
)

func (p *UnifiedPeer) handleIncomingStreams(ctx context.Context) {
	for {
		select {
		case stream := <-p.incomingStreams:
			go p.processIncomingStream(ctx, stream)
		case <-ctx.Done():
			return
		}
	}
}

func (p *UnifiedPeer) processIncomingStream(ctx context.Context, stream quic.Stream) {
	defer stream.Close()

	// Read stream type header
	reader := bufio.NewReader(stream)
	header, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("Failed to read stream header: %v", err)
		return
	}

	streamType := strings.TrimSpace(header)
	log.Printf("Processing incoming stream of type: %s", streamType)

	switch streamType {
	case "audio":
		p.handleAudioStream(ctx, stream, false) // false = receiving
	case "video":
		p.handleVideoStream(ctx, stream, false) // false = receiving
	case "control":
		p.handleControlStream(ctx, stream)
	case "peer-discovery":
		p.handlePeerDiscoveryStream(ctx, stream)
	default:
		log.Printf("Unknown stream type: %s", streamType)
	}
}

func (p *UnifiedPeer) handleAudioStream(ctx context.Context, stream quic.Stream, sending bool) {
	if sending {
		// Send audio to this stream
		if err := p.gstreamer.StartAudioSender(ctx, stream); err != nil {
			log.Printf("Failed to start audio sender: %v", err)
		}
	} else {
		// Receive audio from this stream
		if err := p.gstreamer.StartAudioReceiver(ctx, stream); err != nil {
			log.Printf("Failed to start audio receiver: %v", err)
		}
	}
}

func (p *UnifiedPeer) handleVideoStream(ctx context.Context, stream quic.Stream, sending bool) {
	if sending {
		// Send video to this stream
		if err := p.gstreamer.StartVideoSender(ctx, stream); err != nil {
			log.Printf("Failed to start video sender: %v", err)
		}
	} else {
		// Receive video from this stream
		if err := p.gstreamer.StartVideoReceiver(ctx, stream); err != nil {
			log.Printf("Failed to start video receiver: %v", err)
		}
	}
}

func (p *UnifiedPeer) handleControlStream(ctx context.Context, stream quic.Stream) {
	reader := bufio.NewReader(stream)
	
	for {
		select {
		case <-ctx.Done():
			return
		default:
			line, err := reader.ReadString('\n')
			if err != nil {
				log.Printf("Control stream ended: %v", err)
				return
			}
			
			command := strings.TrimSpace(line)
			p.processControlCommand(ctx, stream, command)
		}
	}
}

func (p *UnifiedPeer) processControlCommand(ctx context.Context, stream quic.Stream, command string) {
	parts := strings.Split(command, ":")
	if len(parts) < 2 {
		log.Printf("Invalid control command: %s", command)
		return
	}

	action := parts[0]
	target := parts[1]

	switch action {
	case "start":
		p.startMediaStream(ctx, stream, target)
	case "stop":
		p.stopMediaStream(target)
	case "status":
		p.sendStatus(stream)
	default:
		log.Printf("Unknown control action: %s", action)
	}
}

func (p *UnifiedPeer) startMediaStream(ctx context.Context, controlStream quic.Stream, mediaType string) {
	// Find a connection to start the stream on
	connections := p.getConnections()
	if len(connections) == 0 {
		log.Printf("No connections available to start %s stream", mediaType)
		return
	}

	// Use the first available connection
	remoteAddr := connections[0]
	
	// Open a new stream for the media
	stream, err := p.openStream(ctx, remoteAddr)
	if err != nil {
		log.Printf("Failed to open %s stream: %v", mediaType, err)
		return
	}

	// Send stream type header
	if _, err := stream.Write([]byte(mediaType + "\n")); err != nil {
		log.Printf("Failed to write stream header: %v", err)
		stream.Close()
		return
	}

	// Start the appropriate stream handler
	switch mediaType {
	case "audio":
		go p.handleAudioStream(ctx, stream, true) // true = sending
	case "video":
		go p.handleVideoStream(ctx, stream, true) // true = sending
	default:
		log.Printf("Unknown media type: %s", mediaType)
		stream.Close()
	}
}

func (p *UnifiedPeer) stopMediaStream(streamType string) {
	activeStreams := p.gstreamer.GetActiveStreams()
	for _, streamID := range activeStreams {
		if strings.Contains(streamID, streamType) {
			p.gstreamer.StopStream(streamID)
		}
	}
}

func (p *UnifiedPeer) sendStatus(stream quic.Stream) {
	activeStreams := p.gstreamer.GetActiveStreams()
	connections := p.getConnections()
	
	status := fmt.Sprintf("STATUS:streams=%d,connections=%d\n", 
		len(activeStreams), len(connections))
	
	if _, err := stream.Write([]byte(status)); err != nil {
		log.Printf("Failed to send status: %v", err)
	}
}

func (p *UnifiedPeer) handlePeerDiscoveryStream(ctx context.Context, stream quic.Stream) {
	// Handle peer discovery messages
	reader := bufio.NewReader(stream)
	
	for {
		select {
		case <-ctx.Done():
			return
		default:
			line, err := reader.ReadString('\n')
			if err != nil {
				log.Printf("Peer discovery stream ended: %v", err)
				return
			}
			
			message := strings.TrimSpace(line)
			log.Printf("Received peer discovery message: %s", message)
			
			// Process peer discovery message
			// This would integrate with the existing peer handler logic
		}
	}
}

// Utility functions for initiating streams
func (p *UnifiedPeer) StartAudioStreamTo(ctx context.Context, remoteAddr string) error {
	stream, err := p.openStream(ctx, remoteAddr)
	if err != nil {
		return fmt.Errorf("failed to open audio stream: %v", err)
	}

	// Send stream type header
	if _, err := stream.Write([]byte("audio\n")); err != nil {
		stream.Close()
		return fmt.Errorf("failed to write stream header: %v", err)
	}

	// Start audio sender
	go p.handleAudioStream(ctx, stream, true)
	return nil
}

func (p *UnifiedPeer) StartVideoStreamTo(ctx context.Context, remoteAddr string) error {
	stream, err := p.openStream(ctx, remoteAddr)
	if err != nil {
		return fmt.Errorf("failed to open video stream: %v", err)
	}

	// Send stream type header
	if _, err := stream.Write([]byte("video\n")); err != nil {
		stream.Close()
		return fmt.Errorf("failed to write stream header: %v", err)
	}

	// Start video sender
	go p.handleVideoStream(ctx, stream, true)
	return nil
}

func (p *UnifiedPeer) SendControlMessage(ctx context.Context, remoteAddr, message string) error {
	stream, err := p.openStream(ctx, remoteAddr)
	if err != nil {
		return fmt.Errorf("failed to open control stream: %v", err)
	}
	defer stream.Close()

	// Send stream type header
	if _, err := stream.Write([]byte("control\n")); err != nil {
		return fmt.Errorf("failed to write stream header: %v", err)
	}

	// Send control message
	if _, err := stream.Write([]byte(message + "\n")); err != nil {
		return fmt.Errorf("failed to send control message: %v", err)
	}

	return nil
}