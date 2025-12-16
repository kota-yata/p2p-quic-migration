package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"

	"github.com/quic-go/quic-go"
)

var (
	globalAudioPosition int64
	positionMutex       sync.RWMutex
)

type AudioStreamer struct {
	stream     *quic.Stream
	startBytes int64
}

func NewAudioStreamer(stream *quic.Stream) *AudioStreamer {
	return &AudioStreamer{
		stream:     stream,
		startBytes: 0,
	}
}

func NewAudioStreamerFromPosition(stream *quic.Stream, startBytes int64) *AudioStreamer {
	return &AudioStreamer{
		stream:     stream,
		startBytes: startBytes,
	}
}

func (as *AudioStreamer) StreamAudio() error {
	var cmd *exec.Cmd

	log.Printf("Starting audio from beginning")

	// Use output.mp3 as the audio source. decodebin ensures mp3 is decoded to raw PCM.
	cmd = exec.Command("gst-launch-1.0",
		"filesrc", "location=../static/output.mp3", "!",
		"decodebin", "!",
		"audioconvert", "!",
		"audioresample", "!",
		"audio/x-raw,rate=44100,channels=2,format=S16LE,layout=interleaved", "!",
		"queue", "max-size-time=1000000000", "!",
		"fdsink", "fd=1", "sync=true")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gstreamer: %v", err)
	}

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				break
			}
			if n > 0 {
				log.Printf("GStreamer stderr: %s", string(buf[:n]))
			}
		}
	}()

	log.Printf("GStreamer audio pipeline started, streaming audio data...")

	buffer := make([]byte, 4096)
	totalBytesSent := int64(0)
	readCount := 0

	for {
		n, err := stdout.Read(buffer)
		readCount++

		if err != nil {
			if err == io.EOF {
				log.Printf("Audio stream completed. Total bytes sent: %d, read attempts: %d", totalBytesSent, readCount)
				break
			}
			return fmt.Errorf("failed to read from audio pipeline after %d reads: %v", readCount, err)
		}

		if n > 0 {
			written, err := as.stream.Write(buffer[:n])
			if err != nil {
				return fmt.Errorf("failed to write audio data to stream after %d bytes: %v", totalBytesSent, err)
			}
			totalBytesSent += int64(written)
			log.Printf("Streamed %d bytes of audio data", totalBytesSent)

			if totalBytesSent%262144 == 0 {
				log.Printf("Sent %.1f MB of audio data", float64(totalBytesSent)/1048576)
			}
		}
	}

	if err := as.stream.Close(); err != nil {
		log.Printf("Error closing stream: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("GStreamer process ended with error: %v", err)
	}

	log.Printf("Audio streaming completed successfully. Total bytes sent: %d", totalBytesSent)
	return nil
}

type AudioReceiver struct {
	stream   *quic.Stream
	recorder *AudioRecorder
}

func NewAudioReceiver(stream *quic.Stream, recorder *AudioRecorder) *AudioReceiver {
	return &AudioReceiver{
		stream:   stream,
		recorder: recorder,
	}
}

func (ar *AudioReceiver) ReceiveAudio() error {
	log.Printf("Starting real-time audio playback from stream")

	cmd := exec.Command("gst-launch-1.0",
		"fdsrc", "fd=0", "!",
		"rawaudioparse", "use-sink-caps=false", "sample-rate=44100", "num-channels=2", "format=pcm", "pcm-format=s16le", "!",
		"audioconvert", "!",
		"audioresample", "!",
		"queue", "max-size-time=50000000", "leaky=downstream", "!",
		"autoaudiosink", "sync=false")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gstreamer playback: %v", err)
	}

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				break
			}
			if n > 0 {
				log.Printf("GStreamer stderr: %s", string(buf[:n]))
			}
		}
	}()

	log.Printf("GStreamer audio playback pipeline started")

	buffer := make([]byte, 4096)
	totalBytes := int64(0)

	for {
		n, err := ar.stream.Read(buffer)
		if err != nil {
			if err == io.EOF {
				log.Printf("Audio stream reception completed. Total bytes received: %d", totalBytes)
				break
			}
			return fmt.Errorf("failed to read from stream: %v", err)
		}

		if n > 0 {
			if err := ar.writeToRecorder(buffer[:n]); err != nil {
				log.Printf("Failed to write incoming audio to recorder: %v", err)
			}

			written, err := stdin.Write(buffer[:n])
			if err != nil {
				return fmt.Errorf("failed to write to gstreamer: %v", err)
			}
			totalBytes += int64(written)

			if totalBytes%262144 == 0 {
				log.Printf("Received and playing %.1f MB of audio data", float64(totalBytes)/1048576)
			}
		}
	}

	stdin.Close()

	if err := cmd.Wait(); err != nil {
		log.Printf("GStreamer playback process ended with error: %v", err)
	}

	log.Printf("Audio playback completed successfully. Total bytes received: %d", totalBytes)
	return nil
}

func (ar *AudioReceiver) writeToRecorder(data []byte) error {
	if ar.recorder == nil {
		return nil
	}
	return ar.recorder.Write(data)
}
