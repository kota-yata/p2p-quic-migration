package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/quic-go/quic-go"
)

type VideoReceiver struct {
	stream *quic.Stream
}

func NewVideoReceiver(stream *quic.Stream) *VideoReceiver {
	return &VideoReceiver{
		stream: stream,
	}
}

func (vr *VideoReceiver) ReceiveVideo() error {
	outputFile, err := os.Create("received_video.mp4")
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer outputFile.Close()

	log.Printf("Created output file 'received_video.mp4' for incoming video stream")

	buffer := make([]byte, 32768)
	totalBytes := int64(0)
	
	for {
		n, err := vr.stream.Read(buffer)
		if err != nil {
			if err == io.EOF {
				log.Printf("Video stream reception completed. Total bytes received: %d", totalBytes)
				break
			}
			return fmt.Errorf("failed to read from stream: %v", err)
		}

		if n > 0 {
			written, err := outputFile.Write(buffer[:n])
			if err != nil {
				return fmt.Errorf("failed to write to output file: %v", err)
			}
			totalBytes += int64(written)
			
			if totalBytes%1048576 == 0 {
				log.Printf("Received %d MB of video data", totalBytes/1048576)
			}
		}
	}

	log.Printf("Video stream saved to 'received_video.mp4' (%d bytes total)", totalBytes)
	return nil
}