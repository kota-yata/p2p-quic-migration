p2p-quic-migration is a project to seek the smallest interruption time when a peer's IP address has changed during p2p connection.

# Development
This project uses a [modified version of quic-go](https://github.com/kota-yata/quic-go). Place this repository and `quic-go` in the same directory hierarchy and it will work (if not, let me know).

## Prerequisites

### GStreamer Installation
This project requires GStreamer for audio streaming. Install it and its plugins.

**Note for macOS:** If you see GLib warnings like "Failed to load shared library 'libgobject-2.0.0.dylib'", install additional dependencies and set variables:
```bash
brew install glib gobject-introspection
export DYLD_LIBRARY_PATH="/opt/homebrew/opt/glib/lib:$DYLD_LIBRARY_PATH"
export GI_TYPELIB_PATH="/opt/homebrew/lib/girepository-1.0:$GI_TYPELIB_PATH"
```

**Verify Installation:**
```bash
gst-launch-1.0 --version # or just gstreamer --version
```

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
