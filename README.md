p2p-quic-migration is a project to seek the smallest interruption time when a peer's IP address has changed during p2p connection.

# Development
## Prerequisites
Gstreamer with the "good" plugin is required.

## Running programs 
```bash
# Run peer
make peer
# Run intermediate server
make intermediate
# Generate certs for servers. Running any component with make will automatically run this beforehand
make cert
# Build binaries
make build
```

1. Run `make intermediate` somewhere  
2. Run `make peer` on a machine behind NAT with your media files at ./static/output.mp3
3. Run `make peer` on another machine behind NAT  

You must run the peers on different networks unless your router supports [NAT loopback](https://nordvpn.com/cybersecurity/glossary/nat-loopback/) (a.k.a. NAT hairpinning).

If the connection succeeds, you should hear the audio. Then try switching the client's network (WiFi to cellular for example). The sound will be interrupted for like 3 sec and recovered soon.

## Roles: sender vs receiver
Peers can run with explicit roles via flags:

```bash
# sender
make ps INTERMEDIATE_ADDR=<host:port>
# receiver
make pr INTERMEDIATE_ADDR=<host:port>
```

- `sender` opens an outgoing audio stream to the peer but does not play incoming audio.
- `receiver` accepts and plays incoming audio but does not open outgoing audio streams.
- Simply run `make peer` runs peer as both sender and receiver, but I haven't been running peer with "both" mode for a long time so not sure if it still works
