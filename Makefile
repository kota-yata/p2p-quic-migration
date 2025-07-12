# P2P QUIC Migration Makefile

SERVER_ADDR ?= 0.0.0.0:1234
INTERMEDIATE_ADDR ?= 203.178.143.72:12345
CERT_FILE ?= server.crt
KEY_FILE ?= server.key

.PHONY: help client server intermediate clean all deps cert

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
	@echo ""
	@echo "Configuration:"
	@echo "  SERVER_ADDR       - Server address (default: $(SERVER_ADDR))"
	@echo "  INTERMEDIATE_ADDR - Intermediate address (default: $(INTERMEDIATE_ADDR))"
	@echo "  CERT_FILE         - Certificate file (default: $(CERT_FILE))"
	@echo "  KEY_FILE          - Key file (default: $(KEY_FILE))"

client: deps
	go run -tags client ./src

server: deps cert
	go run -tags server ./src -cert="$(CERT_FILE)" -key="$(KEY_FILE)"

intermediate: deps cert
	go run -tags intermediate ./src -cert="$(CERT_FILE)" -key="$(KEY_FILE)"

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
	@go build -tags client -o client ./src
	@go build -tags server -o server ./src  
	@go build -tags intermediate -o intermediate_server ./src
