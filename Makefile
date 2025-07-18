# P2P QUIC Migration Makefile

SERVER_ADDR ?= 0.0.0.0:1234
INTERMEDIATE_ADDR ?= 203.178.143.72:12345
CERT_FILE ?= server.crt
KEY_FILE ?= server.key

.PHONY: help client server intermediate clean all deps cert network-monitor-test

help:
	@echo "P2P QUIC Migration - Available targets:"
	@echo ""
	@echo "Basic Commands:"
	@echo "  make client        - Run the client"
	@echo "  make server        - Run the server"
	@echo "  make intermediate  - Run the intermediate server"
	@echo ""
	@echo "Utility Commands:"
	@echo "  make cert         - Generate certificates"
	@echo "  make clean        - Clean up binaries"
	@echo "  make deps         - Download dependencies"
	@echo "  make all          - Run all components in tmux"
	@echo "  make mp4-test     - Test MP4 video and audio pipelines"
	@echo "  make network-monitor-test - Run network monitor test"
	@echo ""
	@echo "Configuration:"
	@echo "  SERVER_ADDR       - Server address (default: $(SERVER_ADDR))"
	@echo "  INTERMEDIATE_ADDR - Intermediate address (default: $(INTERMEDIATE_ADDR))"
	@echo "  CERT_FILE         - Certificate file (default: $(CERT_FILE))"
	@echo "  KEY_FILE          - Key file (default: $(KEY_FILE))"

client: deps
	cd client_peer && go run .

server: deps cert
	cd server_peer && go run . -cert="../$(CERT_FILE)" -key="../$(KEY_FILE)"

intermediate: deps cert
	cd intermediate && go run . -cert="../$(CERT_FILE)" -key="../$(KEY_FILE)"

cert:
	@if [ ! -f "$(CERT_FILE)" ] || [ ! -f "$(KEY_FILE)" ]; then \
		openssl req -x509 -newkey rsa:2048 -keyout "$(KEY_FILE)" -out "$(CERT_FILE)" -days 365 -nodes \
			-subj "/C=US/ST=Test/L=Test/O=Test/CN=localhost"; \
	fi

deps:
	@go mod download
	@go mod verify

clean:
	@go clean
	@rm -f client server intermediate_server

all: deps cert
	@tmux new-session -d -s p2p-quic \; \
		split-window -h \; \
		split-window -v \; \
		select-pane -t 0 \; \
		send-keys 'make intermediate' Enter \; \
		select-pane -t 1 \; \
		send-keys 'sleep 2 && make server' Enter \; \
		select-pane -t 2 \; \
		send-keys 'sleep 4 && make client' Enter \; \
		attach-session

build: deps
	@cd client_peer && go build -o ../client .
	@cd server_peer && go build -o ../server .
	@cd intermediate && go build -o ../intermediate_server .

mp4-test:
	@echo "Testing MP4 video and audio pipelines..."
	@echo "Testing MP4 video extraction and H.264 parsing..."
	@gst-launch-1.0 -v filesrc location=./static/output.mp4 ! qtdemux name=demux demux.video_0 ! queue ! h264parse ! video/x-h264,stream-format=byte-stream ! fakesink 2>/dev/null && echo "Video pipeline test passed" || echo "Video pipeline test failed - check if output.mp4 exists and has video track"
	@echo "Testing MP4 audio extraction and AAC decoding..."
	@gst-launch-1.0 -v filesrc location=./static/output.mp4 ! qtdemux name=demux demux.audio_0 ! queue ! aacparse ! avdec_aac ! audioconvert ! fakesink 2>/dev/null && echo "Audio pipeline test passed" || echo "Audio pipeline test failed - check if output.mp4 exists and has audio track"
	@echo "MP4 pipeline tests completed"

gs-test:
	@echo "Testing GStreamer audio pipeline..."
	@gst-launch-1.0 -v filesrc location=./static/output.mp3 ! decodebin ! audioconvert ! audioresample ! audio/x-raw,rate=44100,channels=2,format=S16LE,layout=interleaved ! fakesink 2>/dev/null || echo "Audio test failed"
	@echo "GStreamer audio test completed"

network-monitor-test: deps
	@echo "Running network monitor test..."
	@echo "Turn WiFi on/off to test network change detection"
	@echo "Press Ctrl+C to stop"
	cd client_peer && go run -tags=standalone network_monitor_standalone.go network_monitor.go
