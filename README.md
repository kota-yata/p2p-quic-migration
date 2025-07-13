# p2p-quic-migration  
P2P QUIC with seamless connection migration.

This project uses a [modified version of quic-go](https://github.com/kota-yata/quic-go). Place this repository and `quic-go` in the same directory hierarchy and it will work (if not, let me know).

## Development  
My man Claude wrote some nice `make` commands:

```bash
# Run client (peer)
make client
# Run server (peer)
make server
# Run intermediate server
make intermediate
# Generate certs for servers. Running any component with make will automatically run this beforehand
make certs
# Build binaries
make build
# Test if GStreamer pipeline works
make gs-test
```

1. Run `make intermediate` somewhere  
2. Run `make server` on a machine behind NAT with your mp3 file at ./static/output.mp3
3. Run `make client` on a machine behind NAT  

You must run the client and server on different networks unless your router supports [NAT loopback](https://nordvpn.com/cybersecurity/glossary/nat-loopback/) (a.k.a. NAT hairpinning).

If the connection succeeds, you should hear the sound from `static/output.mp3`.

## How P2P QUIC Connection Migration Works

### P2P Connection Establishment  
<img src="./static/conn-establish.png" width="500"/>

The steps are similar to WebRTC’s approach, with some simplifications.

#### Address Exchange  
The client peer and server peer first connect to the intermediate server. The intermediate server retrieves each peer’s external address and exchanges them. Once both peers have each other's address, they can begin hole punching.

In WebRTC, this is typically done using a STUN server and a signaling server. A peer asks the STUN server for its reflexive address, and then sends it to the signaling server, which relays it to the other peer. WebRTC peers often gather multiple address-port pairs (called "candidates"), including local addresses, to allow direct connections even within the same LAN. This project currently does **not** handle local candidate exchange.

Previously, this project used [QUIC Address Discovery](https://www.ietf.org/archive/id/draft-seemann-quic-address-discovery-00.html) to perform a STUN-like function over QUIC. The draft defines an `OBSERVED_ADDRESS` frame, equivalent to STUN’s `MAPPED_ADDRESS` attribute, allowing a peer to learn its reflexive address as quick as possible via a probe packet.

While that approach may work better in real-world scenarios (e.g., when gathering addresses from multiple sources for security or NAT type detection), this project now uses the intermediate server to directly exchange peer addresses for simplicity.

#### NAT Hole Punching  
Once peers have each other’s address, they begin NAT hole punching.

NAT hole punching is a standard process in P2P connections. Both peers send UDP packets to create NAT table mappings. These initial packets might not reach the destination if the NAT mapping isn't yet established, but that’s expected. Eventually, both routers will have valid mappings and a direct connection will be possible.

In this project, peers repeatedly attempt to initiate a QUIC connection from both sides until one succeeds. By contrast, WebRTC uses a more robust method with ICE (Interactive Connectivity Establishment), sending STUN Binding Requests for every combination of local and remote candidates. ICE then selects the final pair based on connectivity, RTT, user policy, and other criteria. If forced to use a relay (TURN) server, ICE will choose that path regardless of other available connections.

### Handling Address Changes  
Sometimes a peer's address changes and the connection breaks — e.g., when switching from Wi-Fi to 5G.

In a client-server model over QUIC, this is handled seamlessly using the “connection migration” feature. QUIC identifies connections using Connection IDs rather than the typical 4-tuple, allowing servers to associate packets from a new IP/port with the same connection.

However, this doesn't work as-is in P2P scenarios because a new NAT hole punch is required whenever a peer’s address changes. Packets from the new address get blocked by NAT before the mapping exists.

This project handles that problem simply: when a client peer's address changes, it signals the server peer through the intermediate server.

<img src="./static/network-change.png" width="500"/>

The server peer then immediately switches packet transmission from the P2P connection to the intermediate server. During the new NAT hole punching process, application data is sent via the intermediate server.

<img src="./static/conn-switch.png" width="500"/>

Once the new NAT mappings are in place, the peers resume direct P2P communication.

<img src="./static/new-p2p.png" width="500"/>

This approach is quite general and could be applied to WebRTC or other systems. WebRTC handles address changes by triggering an ICE restart — essentially re-running the connection establishment step. From my testing, ICE restarts take about 5 seconds or more, and this project’s approach performs faster in comparison.
