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
		startBytes: getCurrentAudioPosition(),
	}
}

func NewAudioStreamerFromPosition(stream *quic.Stream, startBytes int64) *AudioStreamer {
	return &AudioStreamer{
		stream:     stream,
		startBytes: startBytes,
	}
}

func getCurrentAudioPosition() int64 {
	positionMutex.RLock()
	defer positionMutex.RUnlock()
	return globalAudioPosition
}

func updateAudioPosition(position int64) {
	positionMutex.Lock()
	defer positionMutex.Unlock()
	globalAudioPosition = position
}

func (as *AudioStreamer) StreamAudio() error {
	var cmd *exec.Cmd

	log.Printf("Starting audio from beginning (position tracking will resume from current transmission)")

	cmd = exec.Command("gst-launch-1.0",
		"filesrc", "location=../static/output.mp3", "!",
		"decodebin", "!",
		"audioconvert", "!",
		"audioresample", "!",
		"audio/x-raw,rate=44100,channels=2,format=S16LE,layout=interleaved", "!",
		"queue", "max-size-time=1000000000", "!",
		"fdsink", "fd=1", "sync=false")

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
	totalBytesRead := int64(0)
	readCount := 0

	log.Printf("Will skip %d bytes to resume from correct position", as.startBytes)

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
			totalBytesRead += int64(n)

			if totalBytesRead <= as.startBytes {
				continue
			}

			var dataToSend []byte
			if totalBytesRead-int64(n) < as.startBytes {
				skipInThisBuffer := as.startBytes - (totalBytesRead - int64(n))
				dataToSend = buffer[skipInThisBuffer:n]
				log.Printf("Partial skip: skipping %d bytes from this %d byte buffer", skipInThisBuffer, n)
			} else {
				dataToSend = buffer[:n]
			}

			if len(dataToSend) > 0 {
				written, err := as.stream.Write(dataToSend)
				if err != nil {
					return fmt.Errorf("failed to write audio data to stream after %d bytes: %v", totalBytesSent, err)
				}
				totalBytesSent += int64(written)

				updateAudioPosition(as.startBytes + totalBytesSent)

				if totalBytesSent%262144 == 0 {
					log.Printf("Sent %.1f MB of audio data", float64(totalBytesSent)/1048576)
				}
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
