p2p-quic-migration is a project to seek the smallest interruption time when a peer's IP address has changed during p2p connection.

# Development
## Prerequisites
Gstreamer with the "good" plugin is required.

## Running programs 
```bash
# Run peer
make peer
# Run intermediate server (handles signaling and relay roles)
make intermediate
# Generate certs for servers. Running any component with make will automatically run this beforehand
make cert
# Build binaries
make build
```

1. Run `make intermediate` somewhere  
2. Run `make ps` on a machine behind NAT with your media files at ./static/output.mp3
3. Run `make pr` on another machine behind NAT  

If the connection succeeds, you should hear the audio. Then try switching the client's network (WiFi to cellular for example). The sound will be interrupted for less than a second and recovered.

## Roles: sender vs receiver
Peers can run with explicit roles via flags:

```bash
# sender
make ps
# receiver
make pr
```

- `sender` opens an outgoing audio stream to the peer but does not play incoming audio.
- `receiver` accepts and plays incoming audio but does not open outgoing audio streams.
