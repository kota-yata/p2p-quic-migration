SERVER_ADDR ?= 0.0.0.0:1234
INTERMEDIATE_ADDR ?= 162.43.38.7:12345
CERT_FILE ?= server.crt
KEY_FILE ?= server.key
ROLE ?= both
RECORD ?= false
# Unix time is evaluated at make parse time
RECORD_PATH ?= ../record/p2prec$(shell date +%s).mp3

PCAP_WIFI ?= ./pcap/wifi_$(shell date +%s).pcap
PCAP_CELL ?= ./pcap/cell_$(shell date +%s).pcap
LOG_FILE  ?= ./log/wifi_event_$(shell date +%s).log

IF_WIFI ?= wlan0
IF_CELL ?= rmnet_data2

ifeq ($(RECORD),true)
RECORD_FLAGS := --record --rpath="$(RECORD_PATH)"
else
RECORD_FLAGS :=
endif

.PHONY: peer ps pr prrec exp intermediate clean deps cert

peer: deps cert
	cd peer && go run . -cert="../$(CERT_FILE)" -key="../$(KEY_FILE)" -serverAddr "$(INTERMEDIATE_ADDR)" -role "$(ROLE)" $(RECORD_FLAGS)

ps: deps cert
	$(MAKE) peer ROLE=sender

pr: deps cert
	$(MAKE) peer ROLE=receiver

prrec: deps cert
	$(MAKE) peer ROLE=receiver RECORD=true RECORD_PATH="$(RECORD_PATH)"

monitor: # 2>/dev/null squashes stdout. If you want to see errors, remove it.
	@echo "Starting monitoring... (Wi-Fi Log & Dual-Interface tcpdump)"
	@(logcat -v time | grep --line-buffered -E "setWifiEnabled" > $(LOG_FILE) & echo $$! > .logcat.pid)
	@(tcpdump -i $(IF_WIFI) -w $(PCAP_WIFI) 2>/dev/null & echo $$! > .tcpdump_wifi.pid)
	@(tcpdump -i $(IF_CELL) -w $(PCAP_CELL) 2>/dev/null & echo $$! > .tcpdump_cell.pid)
	@# waiting for user input
	@read _
	@$(MAKE) stop-monitor

stop-monitor:
	@echo "Stopping processes..."
	@for pid_file in .logcat.pid .tcpdump_wifi.pid .tcpdump_cell.pid; do \
		if [ -f $$pid_file ]; then \
			kill `cat $$pid_file` 2>/dev/null || true; \
			rm $$pid_file; \
		fi; \
	done
	@echo "Done."

exp: deps cert
	@echo "Starting monitoring..."
	@mkdir -p ./record ./pcap ./log
	@trap 'echo "Cleaning up..."; kill $$PID1 $$PID2 $$PID3 2>/dev/null; rm -f .*.pid' EXIT; \
	logcat -v time | grep --line-buffered -E "setWifiEnabled" > $(LOG_FILE) & PID1=$$!; echo $$PID1 > .logcat.pid; \
	echo "Starting tcpdump on interfaces $(IF_WIFI) and $(IF_CELL)..."; \
	tcpdump -i $(IF_WIFI) -w $(PCAP_WIFI) 2>/dev/null & PID2=$$!; echo $$PID2 > .tcpdump_wifi.pid; \
	tcpdump -i $(IF_CELL) -w $(PCAP_CELL) 2>/dev/null & PID3=$$!; echo $$PID3 > .tcpdump_cell.pid; \
	echo "Processes started. Running prrec..."; \
	$(MAKE) prrec RECORD_PATH="$(RECORD_PATH)"

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

build: deps
	@cd peer && go build -o ../peer .
	@cd intermediate && go build -o ../intermediate_server .
