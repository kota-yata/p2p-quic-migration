package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"

	"github.com/quic-go/quic-go"
)

type VideoStreamer struct {
	stream *quic.Stream
}

func NewVideoStreamer(stream *quic.Stream) *VideoStreamer {
	return &VideoStreamer{
		stream: stream,
	}
}

func (vs *VideoStreamer) StreamVideo() error {
	cmd := exec.Command("gst-launch-1.0", 
		"filesrc", "location=output.mp4", "!",
		"qtdemux", "!",
		"h264parse", "!",
		"fdsink", "fd=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gstreamer: %v", err)
	}

	log.Printf("GStreamer pipeline started, streaming video data...")

	buffer := make([]byte, 32768)
	for {
		n, err := stdout.Read(buffer)
		if err != nil {
			if err == io.EOF {
				log.Printf("Video stream completed")
				break
			}
			return fmt.Errorf("failed to read from gstreamer: %v", err)
		}

		if _, err := vs.stream.Write(buffer[:n]); err != nil {
			return fmt.Errorf("failed to write video data to stream: %v", err)
		}
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("GStreamer process ended with error: %v", err)
	}

	log.Printf("Video streaming completed successfully")
	return nil
}