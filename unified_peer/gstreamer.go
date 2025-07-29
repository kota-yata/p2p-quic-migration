package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"

	"github.com/quic-go/quic-go"
)

type GStreamerConfig struct {
	EnableAudio bool
	EnableVideo bool
	AudioSource string
	VideoSource string
}

type GStreamerManager struct {
	config      GStreamerConfig
	audioSender *AudioSender
	videoSender *VideoSender
	audioReceiver *AudioReceiver
	videoReceiver *VideoReceiver
	
	// Stream management
	activeStreams map[string]*StreamHandler
	streamMutex   sync.RWMutex
}

type StreamHandler struct {
	stream     quic.Stream
	streamType string // "audio-send", "audio-recv", "video-send", "video-recv"
	cmd        *exec.Cmd
	cancel     context.CancelFunc
}

func NewGStreamerManager(config GStreamerConfig) *GStreamerManager {
	return &GStreamerManager{
		config:        config,
		activeStreams: make(map[string]*StreamHandler),
	}
}

func (gm *GStreamerManager) StartAudioSender(ctx context.Context, stream quic.Stream) error {
	if !gm.config.EnableAudio {
		return fmt.Errorf("audio is disabled")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	
	cmd := exec.CommandContext(streamCtx,
		"gst-launch-1.0",
		"filesrc", fmt.Sprintf("location=%s", gm.config.AudioSource), "!",
		"qtdemux", "name=demux", "demux.audio_0", "!",
		"queue", "!",
		"aacparse", "!",
		"faad", "!",
		"audioconvert", "!",
		"audioresample", "!",
		"audio/x-raw,rate=44100,channels=2,format=S16LE,layout=interleaved", "!",
		"queue", "max-size-time=1000000000", "!",
		"fdsink", "fd=1", "sync=true",
	)

	cmd.Stdout = stream

	handler := &StreamHandler{
		stream:     stream,
		streamType: "audio-send",
		cmd:        cmd,
		cancel:     cancel,
	}

	gm.streamMutex.Lock()
	gm.activeStreams[fmt.Sprintf("audio-send-%s", stream.StreamID())] = handler
	gm.streamMutex.Unlock()

	log.Printf("Starting audio sender for stream %d", stream.StreamID())
	return cmd.Run()
}

func (gm *GStreamerManager) StartVideoSender(ctx context.Context, stream quic.Stream) error {
	if !gm.config.EnableVideo {
		return fmt.Errorf("video is disabled")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	
	cmd := exec.CommandContext(streamCtx,
		"gst-launch-1.0",
		"filesrc", fmt.Sprintf("location=%s", gm.config.VideoSource), "!",
		"qtdemux", "name=demux", "demux.video_0", "!",
		"queue", "!",
		"h264parse", "!",
		"queue", "max-size-time=1000000000", "!",
		"fdsink", "fd=1", "sync=true",
	)

	cmd.Stdout = stream

	handler := &StreamHandler{
		stream:     stream,
		streamType: "video-send",
		cmd:        cmd,
		cancel:     cancel,
	}

	gm.streamMutex.Lock()
	gm.activeStreams[fmt.Sprintf("video-send-%s", stream.StreamID())] = handler
	gm.streamMutex.Unlock()

	log.Printf("Starting video sender for stream %d", stream.StreamID())
	return cmd.Run()
}

func (gm *GStreamerManager) StartAudioReceiver(ctx context.Context, stream quic.Stream) error {
	if !gm.config.EnableAudio {
		return fmt.Errorf("audio is disabled")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	
	cmd := exec.CommandContext(streamCtx,
		"gst-launch-1.0",
		"fdsrc", "fd=0", "!",
		"rawaudioparse", "use-sink-caps=false", "sample-rate=44100", "num-channels=2", "format=pcm", "pcm-format=s16le", "!",
		"audioconvert", "!",
		"audioresample", "!",
		"queue", "max-size-time=50000000", "leaky=downstream", "!",
		"autoaudiosink", "sync=false",
	)

	cmd.Stdin = stream

	handler := &StreamHandler{
		stream:     stream,
		streamType: "audio-recv",
		cmd:        cmd,
		cancel:     cancel,
	}

	gm.streamMutex.Lock()
	gm.activeStreams[fmt.Sprintf("audio-recv-%s", stream.StreamID())] = handler
	gm.streamMutex.Unlock()

	log.Printf("Starting audio receiver for stream %d", stream.StreamID())
	return cmd.Run()
}

func (gm *GStreamerManager) StartVideoReceiver(ctx context.Context, stream quic.Stream) error {
	if !gm.config.EnableVideo {
		return fmt.Errorf("video is disabled")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	
	cmd := exec.CommandContext(streamCtx,
		"gst-launch-1.0",
		"fdsrc", "fd=0", "!",
		"h264parse", "!",
		"avdec_h264", "!",
		"videoconvert", "!",
		"videoscale", "!",
		"autovideosink", "sync=true",
	)

	cmd.Stdin = stream

	handler := &StreamHandler{
		stream:     stream,
		streamType: "video-recv",
		cmd:        cmd,
		cancel:     cancel,
	}

	gm.streamMutex.Lock()
	gm.activeStreams[fmt.Sprintf("video-recv-%s", stream.StreamID())] = handler
	gm.streamMutex.Unlock()

	log.Printf("Starting video receiver for stream %d", stream.StreamID())
	return cmd.Run()
}

func (gm *GStreamerManager) StopStream(streamID string) {
	gm.streamMutex.Lock()
	defer gm.streamMutex.Unlock()

	if handler, exists := gm.activeStreams[streamID]; exists {
		log.Printf("Stopping stream %s", streamID)
		handler.cancel()
		if handler.cmd.Process != nil {
			handler.cmd.Process.Kill()
		}
		handler.stream.Close()
		delete(gm.activeStreams, streamID)
	}
}

func (gm *GStreamerManager) StopAllStreams() {
	gm.streamMutex.Lock()
	defer gm.streamMutex.Unlock()

	log.Printf("Stopping all streams")
	for streamID, handler := range gm.activeStreams {
		handler.cancel()
		if handler.cmd.Process != nil {
			handler.cmd.Process.Kill()
		}
		handler.stream.Close()
		delete(gm.activeStreams, streamID)
	}
}

func (gm *GStreamerManager) GetActiveStreams() []string {
	gm.streamMutex.RLock()
	defer gm.streamMutex.RUnlock()

	streams := make([]string, 0, len(gm.activeStreams))
	for streamID := range gm.activeStreams {
		streams = append(streams, streamID)
	}
	return streams
}

// Legacy components for compatibility
type AudioSender struct {
	manager *GStreamerManager
}

type VideoSender struct {
	manager *GStreamerManager
}

type AudioReceiver struct {
	manager *GStreamerManager
}

type VideoReceiver struct {
	manager *GStreamerManager
}

func (as *AudioSender) StreamAudio(writer io.Writer) error {
	// This is a simplified version - in practice, you'd want to adapt the stream
	return fmt.Errorf("use GStreamerManager.StartAudioSender instead")
}

func (vs *VideoSender) StreamVideo(writer io.Writer) error {
	return fmt.Errorf("use GStreamerManager.StartVideoSender instead")
}

func (ar *AudioReceiver) ReceiveAudio(reader io.Reader) error {
	return fmt.Errorf("use GStreamerManager.StartAudioReceiver instead")
}

func (vr *VideoReceiver) ReceiveVideo(reader io.Reader) error {
	return fmt.Errorf("use GStreamerManager.StartVideoReceiver instead")
}