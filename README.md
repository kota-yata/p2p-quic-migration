# p2p-quic-migration
P2P QUIC with seamless connection migration.

This project uses [modified quic-go](https://github.com/kota-yata/quic-go). Put this repository and quic-go in the same directory hierarchy and it will work (If not, let me know).

## Development
My boy Claude Code made a nice makefile.
```bash
# Run client (peer)
make client

# Run server (peer)
make server

# Run intermediate server
make intermediate

# Generate certs for servers. Running each componenet with make will automatically run this beforehand
make certs

# Build binaries
make build
```

You can also run them directly with `go run` using the corresponding tag. For example:

```bash
go run -tags intermediate ./src -cert server.crt -key server.key
go run -tags client ./src
```

