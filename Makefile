SERVER_ADDR ?= 0.0.0.0:1234
INTERMEDIATE_ADDR ?= 203.178.143.72:12345
CERT_FILE ?= server.crt
KEY_FILE ?= server.key

.PHONY: peer intermediate unified-peer unified-server unified-client unified-bidirectional clean all deps cert

peer: deps cert
	cd peer && go run . -cert="../$(CERT_FILE)" -key="../$(KEY_FILE)"

address-detection:
	cd peer/cmd && go run network_monitor_standalone.go

cm-test:
	cd peer/cmd && go run connection_migration.go -cert ../../$(CERT_FILE) -key ../../$(KEY_FILE) -serverAddr "$(INTERMEDIATE_ADDR)"

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

all: deps cert
	@tmux new-session -d -s p2p-quic \; \
		split-window -h \; \
		split-window -v \; \
		select-pane -t 0 \; \
		send-keys 'make intermediate' Enter \; \
		select-pane -t 1 \; \
		send-keys 'sleep 2 && make peer' Enter \; \
		select-pane -t 2 \; \
		send-keys 'sleep 4 && make peer' Enter \; \
		attach-session

build: deps
	@cd peer && go build -o ../peer .
	@cd intermediate && go build -o ../intermediate_server .
