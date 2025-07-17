package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"

	"github.com/quic-go/quic-go"
)

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

	cmd := exec.Command("gst-launch-1.0",
		"fdsrc", "fd=0", "!",
		"rawaudioparse", "use-sink-caps=false", "sample-rate=44100", "num-channels=2", "format=pcm", "pcm-format=s16le", "!",
		"audioconvert", "!",
		"audioresample", "!",
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

type VideoReceiver struct {
	stream *quic.Stream
}

func NewVideoReceiver(stream *quic.Stream) *VideoReceiver {
	return &VideoReceiver{
		stream: stream,
	}
}

func (vr *VideoReceiver) ReceiveVideo() error {
	log.Printf("Starting real-time video playback from stream")

	cmd := exec.Command("gst-launch-1.0",
		"fdsrc", "fd=0", "!",
		"h264parse", "!",
		"avdec_h264", "!",
		"videoconvert", "!",
		"videoscale", "!",
		"autovideosink", "sync=false")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gstreamer video playback: %v", err)
	}

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				break
			}
			if n > 0 {
				log.Printf("GStreamer video stderr: %s", string(buf[:n]))
			}
		}
	}()

	log.Printf("GStreamer video playback pipeline started")

	buffer := make([]byte, 8192)
	totalBytes := int64(0)

	for {
		n, err := vr.stream.Read(buffer)
		if err != nil {
			if err == io.EOF {
				log.Printf("Video stream reception completed. Total bytes received: %d", totalBytes)
				break
			}
			return fmt.Errorf("failed to read from video stream: %v", err)
		}

		if n > 0 {
			written, err := stdin.Write(buffer[:n])
			if err != nil {
				return fmt.Errorf("failed to write to gstreamer video: %v", err)
			}
			totalBytes += int64(written)

			if totalBytes%1048576 == 0 {
				log.Printf("Received and playing %.1f MB of video data", float64(totalBytes)/1048576)
			}
		}
	}

	stdin.Close()

	if err := cmd.Wait(); err != nil {
		log.Printf("GStreamer video playback process ended with error: %v", err)
	}

	log.Printf("Video playback completed successfully. Total bytes received: %d", totalBytes)
	return nil
}
