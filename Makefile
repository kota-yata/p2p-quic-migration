# P2P QUIC Migration Makefile
# This Makefile provides convenient targets to run client, server, and intermediate server

# Default variables
SERVER_ADDR ?= 0.0.0.0:1234
INTERMEDIATE_ADDR ?= 203.178.143.72:12345
CERT_FILE ?= server.crt
KEY_FILE ?= server.key

# Colors for output
GREEN = \033[0;32m
YELLOW = \033[0;33m
BLUE = \033[0;34m
NC = \033[0m # No Color

.PHONY: help client server intermediate clean all deps cert

# Default target
help:
	@echo "$(GREEN)P2P QUIC Migration - Available targets:$(NC)"
	@echo ""
	@echo "$(YELLOW)Basic Commands:$(NC)"
	@echo "  make client        - Run the client (automatically connects to available peers)"
	@echo "  make server        - Run the server (acts as a peer)"
	@echo "  make intermediate  - Run the intermediate server (peer discovery service)"
	@echo ""
	@echo "$(YELLOW)Utility Commands:$(NC)"
	@echo "  make cert         - Generate self-signed certificates for testing"
	@echo "  make clean        - Clean up built binaries"
	@echo "  make deps         - Download and verify dependencies"
	@echo "  make all          - Run all three components in parallel (requires tmux)"
	@echo ""
	@echo "$(YELLOW)Configuration:$(NC)"
	@echo "  SERVER_ADDR       - Intermediate server address (default: $(SERVER_ADDR))"
	@echo "  INTERMEDIATE_ADDR - Address for intermediate server to bind (default: $(INTERMEDIATE_ADDR))"
	@echo "  CERT_FILE         - TLS certificate file (default: $(CERT_FILE))"
	@echo "  KEY_FILE          - TLS key file (default: $(KEY_FILE))"
	@echo ""
	@echo "$(YELLOW)Examples:$(NC)"
	@echo "  make client SERVER_ADDR=localhost:12345"
	@echo "  make server CERT_FILE=my.crt KEY_FILE=my.key"
	@echo "  make intermediate

# Run the client
client: deps
	@echo "$(BLUE)Starting P2P Client...$(NC)"
	@echo "$(YELLOW)Connecting to intermediate server: $(SERVER_ADDR)$(NC)"
	@echo "$(YELLOW)Client will automatically discover and connect to available peers$(NC)"
	go run -tags client ./src

# Run the server (peer)
server: deps cert
	@echo "$(BLUE)Starting P2P Server...$(NC)"
	@echo "$(YELLOW)Using certificates: $(CERT_FILE), $(KEY_FILE)$(NC)"
	@echo "$(YELLOW)Connecting to intermediate server: $(SERVER_ADDR)$(NC)"
	go run -tags server ./src -cert="$(CERT_FILE)" -key="$(KEY_FILE)"

# Run the intermediate server
intermediate: deps cert
	@echo "$(BLUE)Starting Intermediate Server (Peer Discovery Service)...$(NC)"
	@echo "$(YELLOW)Binding to: $(INTERMEDIATE_ADDR)$(NC)"
	@echo "$(YELLOW)Using certificates: $(CERT_FILE), $(KEY_FILE)$(NC)"
	go run -tags intermediate ./src -cert="$(CERT_FILE)" -key="$(KEY_FILE)"

# Generate self-signed certificates for testing
cert:
	@if [ ! -f "$(CERT_FILE)" ] || [ ! -f "$(KEY_FILE)" ]; then \
		echo "$(YELLOW)Generating self-signed certificates...$(NC)"; \
		openssl req -x509 -newkey rsa:2048 -keyout "$(KEY_FILE)" -out "$(CERT_FILE)" -days 365 -nodes \
			-subj "/C=US/ST=Test/L=Test/O=Test/CN=localhost"; \
		echo "$(GREEN)Certificates generated: $(CERT_FILE), $(KEY_FILE)$(NC)"; \
	else \
		echo "$(GREEN)Certificates already exist: $(CERT_FILE), $(KEY_FILE)$(NC)"; \
	fi

# Download and verify dependencies
deps:
	@echo "$(YELLOW)Checking dependencies...$(NC)"
	@go mod download
	@go mod verify

# Clean up
clean:
	@echo "$(YELLOW)Cleaning up...$(NC)"
	@go clean
	@rm -f client server intermediate_server
	@echo "$(GREEN)Clean complete$(NC)"

# Run all components in parallel using tmux
all: deps cert
	@echo "$(BLUE)Starting all components in tmux session...$(NC)"
	@echo "$(YELLOW)This will open 3 tmux panes: intermediate server, server, and client$(NC)"
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
	@echo "$(GREEN)To kill the session: tmux kill-session -t p2p-quic$(NC)"

# Build binaries (optional)
build: deps
	@echo "$(YELLOW)Building binaries...$(NC)"
	@go build -tags client -o client ./src
	@go build -tags server -o server ./src  
	@go build -tags intermediate -o intermediate_server ./src
	@echo "$(GREEN)Binaries built: client, server, intermediate_server$(NC)"
