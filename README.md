# p2p-quic-migration
P2P QUIC with seamless connection migration.

This project uses [a modified quic-go](https://github.com/kota-yata/quic-go).

## Building individual binaries

All source files now live in `src/` and use build tags. Build each component with:

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
go run -tags client ./src
```

