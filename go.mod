module github.com/kota-yata/p2p-quic-migration

go 1.23

require github.com/quic-go/quic-go v0.0.0

require (
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.uber.org/mock v0.5.0 // indirect
	golang.org/x/crypto v0.26.0 // indirect
	golang.org/x/mod v0.18.0 // indirect
	golang.org/x/net v0.28.0 // indirect
	golang.org/x/sync v0.8.0 // indirect
	golang.org/x/sys v0.23.0 // indirect
	golang.org/x/tools v0.22.0 // indirect
)

replace github.com/quic-go/quic-go => ../quic-go
