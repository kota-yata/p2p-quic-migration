#!/bin/bash

# Example usage script for unified peer

echo "Unified Peer Usage Examples"
echo "=========================="

echo ""
echo "1. Two-way audio/video call between peers:"
echo "   Terminal 1: ./unified_peer -mode bidirectional -listen :4433"
echo "   Terminal 2: ./unified_peer -mode bidirectional -server localhost:4433 -listen :4434"

echo ""
echo "2. Audio-only streaming:"
echo "   Sender:   ./unified_peer -mode server -video=false -listen :4433"
echo "   Receiver: ./unified_peer -mode client -video=false -server localhost:4433"

echo ""
echo "3. Server with network migration support:"
echo "   ./unified_peer -mode bidirectional -network-monitor=true -listen :4433"

echo ""
echo "4. Client connecting to remote server:"
echo "   ./unified_peer -mode client -server remote.example.com:4433 -network-monitor=true"

echo ""
echo "5. Custom media sources:"
echo "   ./unified_peer -audio-source /path/to/music.mp4 -video-source /path/to/video.mp4"

echo ""
echo "Available command line options:"
echo "  -mode           : server, client, or bidirectional (default: bidirectional)"
echo "  -server         : Server address to connect to (default: localhost:4433)"
echo "  -listen         : Address to listen on (default: :4433)"
echo "  -cert           : Path to TLS certificate"
echo "  -key            : Path to TLS private key"
echo "  -video          : Enable video streaming (default: true)"
echo "  -audio          : Enable audio streaming (default: true)"
echo "  -audio-source   : Audio source file (default: ../static/output.mp4)"
echo "  -video-source   : Video source file (default: ../static/output.mp4)"
echo "  -network-monitor: Enable network monitoring (default: true)"

echo ""
echo "Requirements:"
echo "  - Go 1.21+"
echo "  - GStreamer 1.0+"
echo "  - Media files in ../static/ directory"