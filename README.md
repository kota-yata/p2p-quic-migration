# p2p-quic-migration
P2P QUIC with seamless connection migration.

This project uses [modified quic-go](https://github.com/kota-yata/quic-go).

## Building individual binaries
```bash
# Build client
go build -tags client -o bin/client ./src

# Build server
go build -tags server -o bin/server ./src

# Build intermediate server
go build -tags intermediate -o bin/intermediate-server ./src
```

You can also run them directly with `go run` using the corresponding tag. For example:

```bash
go run -tags intermediate ./src -cert server.crt -key server.key
go run -tags client ./src
```

