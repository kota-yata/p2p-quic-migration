package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	byrd "github.com/kota-yata/byrd-mp3"
	"github.com/quic-go/quic-go"
)

const (
	audioFilePath       = "../static/output.mp3"
	audioSampleRate     = 48000
	audioChannelCount   = 2
	audioBytesPerSample = 2
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
	log.Printf("Starting audio from beginning")

	input, err := os.Open(audioFilePath)
	if err != nil {
		return fmt.Errorf("failed to open MP3 file: %w", err)
	}
	defer input.Close()

	decoder, err := byrd.NewDecoder(input)
	if err != nil {
		return fmt.Errorf("failed to create Byrd MP3 decoder: %w", err)
	}

	buffer := make([]byte, 4096)
	totalBytesSent := int64(0)
	readCount := 0
	startedAt := time.Now()
	formatChecked := false

	for {
		n, err := decoder.Read(buffer)
		readCount++

		if !formatChecked && n > 0 {
			if decoder.SampleRate() != audioSampleRate || decoder.Channels() != audioChannelCount {
				return fmt.Errorf(
					"unsupported MP3 format: got %d Hz with %d channels, want %d Hz with %d channels",
					decoder.SampleRate(), decoder.Channels(), audioSampleRate, audioChannelCount,
				)
			}
			formatChecked = true
			log.Printf("Byrd MP3 decoder started: %d Hz, %d channels, S16LE", decoder.SampleRate(), decoder.Channels())
		}

		if n > 0 {
			for offset := 0; offset < n; {
				written, writeErr := as.stream.Write(buffer[offset:n])
				if written > 0 {
					offset += written
					totalBytesSent += int64(written)
				}
				if writeErr != nil {
					return fmt.Errorf("failed to write audio data to stream after %d bytes: %w", totalBytesSent, writeErr)
				}
				if written == 0 {
					return fmt.Errorf("failed to write audio data to stream after %d bytes: %w", totalBytesSent, io.ErrShortWrite)
				}
			}

			// Match the real-time pacing previously provided by GStreamer's sync=true.
			targetElapsed := time.Duration(totalBytesSent) * time.Second /
				time.Duration(audioSampleRate*audioChannelCount*audioBytesPerSample)
			if wait := targetElapsed - time.Since(startedAt); wait > 0 {
				time.Sleep(wait)
			}

			if totalBytesSent%262144 == 0 {
				log.Printf("Sent %.1f MB of audio data", float64(totalBytesSent)/1048576)
			}
		}

		if err != nil {
			if err == io.EOF {
				log.Printf("Audio stream completed. Total bytes sent: %d, read attempts: %d", totalBytesSent, readCount)
				break
			}
			return fmt.Errorf("failed to decode MP3 with Byrd after %d reads: %w", readCount, err)
		}
	}

	if !formatChecked {
		return fmt.Errorf("MP3 file contained no decodable audio frames")
	}

	if err := as.stream.Close(); err != nil {
		log.Printf("Error closing stream: %v", err)
	}

	log.Printf("Audio streaming completed successfully. Total bytes sent: %d", totalBytesSent)
	return nil
}

type AudioReceiver struct {
	stream *quic.Stream
}

func NewAudioReceiver(stream *quic.Stream) *AudioReceiver {
	return &AudioReceiver{
		stream: stream,
	}
}

func (ar *AudioReceiver) ReceiveAudio() error {
	log.Printf("Starting real-time audio playback from stream")

	args := []string{
		"fdsrc", "fd=0", "!",
		"rawaudioparse", "use-sink-caps=false", "sample-rate=" + strconv.Itoa(audioSampleRate), "num-channels=" + strconv.Itoa(audioChannelCount), "format=pcm", "pcm-format=s16le", "!",
		"audioconvert", "!",
		"audioresample", "!",
		"queue", "max-size-time=50000000", "leaky=downstream", "!",
		"autoaudiosink", "sync=false",
	}

	cmd := exec.Command("gst-launch-1.0", args...)

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
