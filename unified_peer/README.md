# Unified Peer

A unified P2P streaming component that merges the functionality of both client_peer and server_peer, enabling bidirectional audio/video streaming with QUIC connection migration support.

## Features

- **Bidirectional Streaming**: Can both send and receive audio/video streams
- **Multiple Modes**: Server-only, client-only, or full bidirectional mode
- **Network Migration**: Automatic QUIC connection migration when network changes
- **P2P Discovery**: Peer discovery through intermediate server
- **Stream Management**: Dynamic stream creation and management
- **GStreamer Integration**: Audio/video processing with GStreamer

## Usage

### Basic Usage

```bash
# Run in bidirectional mode (default)
./unified_peer

# Run as server only
./unified_peer -mode server -listen :4433

# Run as client only
./unified_peer -mode client -server localhost:4433

# Enable network monitoring (client/bidirectional modes)
./unified_peer -mode client -network-monitor=true

# Specify media sources
./unified_peer -audio-source /path/to/audio.mp4 -video-source /path/to/video.mp4
```

### Command Line Options

- `-mode`: Peer mode (server, client, bidirectional) [default: bidirectional]
- `-server`: Server address to connect to [default: localhost:4433]
- `-listen`: Address to listen on [default: :4433]
- `-cert`: Path to TLS certificate
- `-key`: Path to TLS private key
- `-video`: Enable video streaming [default: true]
- `-audio`: Enable audio streaming [default: true]
- `-audio-source`: Audio source file [default: ../static/output.mp4]
- `-video-source`: Video source file [default: ../static/output.mp4]
- `-network-monitor`: Enable network monitoring [default: true]

## Architecture

### Core Components

1. **UnifiedPeer**: Main peer component handling connections and coordination
2. **GStreamerManager**: Manages audio/video streaming with GStreamer
3. **NetworkMonitor**: Monitors network changes and triggers migration
4. **PeerHandler**: Handles P2P discovery and hole punching
5. **Stream Management**: Handles different types of QUIC streams

### Stream Types

- **audio**: Audio streaming (bidirectional)
- **video**: Video streaming (bidirectional)
- **control**: Control messages for stream management
- **peer-discovery**: Peer discovery and coordination

### Network Migration

The unified peer supports QUIC connection migration:

1. Network monitor detects IP address changes
2. Creates new UDP connection on new interface
3. Migrates existing QUIC connections
4. Notifies peers of network change
5. Re-establishes P2P connections if needed

## Building

```bash
cd unified_peer
go mod tidy
go build -o unified_peer
```

## Dependencies

- Go 1.21+
- GStreamer 1.0+
- QUIC-GO library

## Migration from Separate Components

This unified peer replaces both `client_peer` and `server_peer` with a single component that can:

1. **Send audio/video** (previously server_peer functionality)
2. **Receive audio/video** (previously client_peer functionality)
3. **Handle network changes** (enhanced from client_peer)
4. **Manage P2P connections** (combined from both components)

### Key Improvements

- **Bidirectional streaming**: Both peers can send and receive
- **Unified codebase**: Single component reduces maintenance
- **Enhanced flexibility**: Can switch modes at runtime
- **Better stream management**: Centralized stream handling
- **Improved network handling**: Enhanced migration capabilities

## Examples

### Two-way Audio/Video Call

```bash
# Peer A (server + client)
./unified_peer -mode bidirectional -listen :4433

# Peer B (client + server)  
./unified_peer -mode bidirectional -server peer-a:4433 -listen :4434
```

### Audio-only Streaming

```bash
# Audio sender
./unified_peer -mode server -video=false -listen :4433

# Audio receiver
./unified_peer -mode client -video=false -server sender:4433
```

### With Network Migration

```bash
# Mobile peer with network monitoring
./unified_peer -mode bidirectional -network-monitor=true -server remote-peer:4433
```