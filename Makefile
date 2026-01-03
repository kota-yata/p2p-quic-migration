SERVER_ADDR ?= 0.0.0.0:1234
INTERMEDIATE_ADDR ?= 162.43.47.102:12345
CERT_FILE ?= server.crt
KEY_FILE ?= server.key
ROLE ?= both

PCAP_RCV_WIFI ?= ./pcap/rcv_wifi_$(shell date +%s).pcap
PCAP_RCV_CELL ?= ./pcap/rcv_cell_$(shell date +%s).pcap
PCAP_SND_WIFI ?= ./pcap/snd_$(shell date +%s).pcap
LOG_FILE  ?= ./log/wifi_event_$(shell date +%s).log

RCV_IF_WIFI ?= wlan0
RCV_IF_CELL ?= rmnet_data2
SND_IF_WIFI ?= eth0

.PHONY: peer ps pr exp intermediate clean deps cert

peer: deps cert
	cd peer && go run . -cert="../$(CERT_FILE)" -key="../$(KEY_FILE)" -serverAddr "$(INTERMEDIATE_ADDR)" -role "$(ROLE)"

ps: deps cert
	$(MAKE) peer ROLE=sender

pr: deps cert
	$(MAKE) peer ROLE=receiver

## removed prrec target (recording deprecated)

monitor:
	@echo "Starting monitoring... (Wi-Fi Log & Dual-Interface tcpdump)"
	@mkdir -p ./pcap ./log
	@(logcat -v time | grep --line-buffered -E "setWifiEnabled" > $(LOG_FILE) & echo $$! > .logcat.pid)
	@(tcpdump -i $(RCV_IF_WIFI) -w $(PCAP_RCV_WIFI) & echo $$! > .tcpdump_wifi.pid)
	@(tcpdump -i $(RCV_IF_CELL) -w $(PCAP_RCV_CELL) & echo $$! > .tcpdump_cell.pid)
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

clearm:
	@echo "Removing pcap and log files..."
	@rm -f ./pcap/*.pcap ./log/*.log
	@echo "Done."

exp-pr: deps cert # storing pids just in case of unexpected termination
	@echo "Starting monitoring..."
	@mkdir -p ./pcap ./log
	@trap 'echo "Cleaning up..."; kill $$PID1 $$PID2 $$PID3 2>/dev/null; rm -f .*.pid' EXIT; \
	logcat -v time | grep --line-buffered -E "setWifiEnabled" > $(LOG_FILE) & PID1=$$!; echo $$PID1 > .logcat.pid; \
	echo "Starting tcpdump on interfaces $(RCV_IF_WIFI) and $(RCV_IF_CELL)..."; \
	tcpdump -i $(RCV_IF_WIFI) -w $(PCAP_RCV_WIFI) 2>/dev/null & PID2=$$!; echo $$PID2 > .tcpdump_wifi.pid; \
	tcpdump -i $(RCV_IF_CELL) -w $(PCAP_RCV_CELL) 2>/dev/null & PID3=$$!; echo $$PID3 > .tcpdump_cell.pid; \
	echo "Processes started. Running pr..."; \
	$(MAKE) pr

exp-ps: deps cert # storing pids just in case of unexpected termination
	@echo "Starting monitoring..."
	@mkdir -p ./pcap ./log
	@trap 'echo "Cleaning up..."; kill $$PID1 $$PID2 $$PID3 2>/dev/null; rm -f .*.pid' EXIT; \
	echo "Starting tcpdump on interfaces $(SND_IF_WIFI)..."; \
	tcpdump -i $(SND_IF_WIFI) -w $(PCAP_SND_WIFI) & PID2=$$!; echo $$PID2 > .tcpdump_wifi.pid; \
	echo "Processes started. Running ps..."; \
	$(MAKE) ps

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
