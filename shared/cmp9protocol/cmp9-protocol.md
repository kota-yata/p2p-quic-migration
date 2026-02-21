Cmp9 defines compact binary control messages exchanged between Peer and Intermediate Server, and between Peer and Relay Server, plus a raw audio relay stream.

## Message Framing
- Header:
  - Type: 1 byte (enum, see below)
  - PayloadLen: 2 bytes, unsigned, big‑endian (0–65535)
- Payload: `PayloadLen` bytes. Content depends on `Type`.
- All multi‑byte integers use network byte order (big‑endian).

Notes
- The control stream is long‑lived. Messages may be sent back‑to‑back.
- The audio relay uses a separate stream whose first frame is a single control message, followed by raw audio bytes for the remainder of that stream.

## Common Encodings
- PeerID: 4 bytes (`uint32`). Assigned by the Intermediate Server at connection time and used in all subsequent references. Stable for the lifetime of the connection.
- Address:
  - `AF`: 1 byte. `0x04` = IPv4, `0x06` = IPv6
  - `IP`: 4 bytes if IPv4, 16 bytes if IPv6
  - `Port`: 2 bytes (`uint16`)
  - Total: 7 bytes (IPv4) or 19 bytes (IPv6)

## Types and Payloads

- 0x01 GET_PEERS_REQ (peer → server)
  - Payload: empty (PayloadLen = 0)
  - Semantics: Request the current list of connected peers and register this stream for subsequent server notifications.

- 0x02 PEER_LIST_RESP (server → peer)
  - Payload:
    - `Count`: 2 bytes (`uint16`)
    - Repeated `Count` times:
      - `PeerID`: 4 bytes
      - `Address`: encoded as Address (AF + IP + Port)
  - Semantics: Responds to GET_PEERS_REQ on the same stream.

- 0x03 NEW_PEER_NOTIF (server → peer)
  - Payload:
    - `PeerID`: 4 bytes
    - `Address`: Address
  - Semantics: Pushed on the same stream used for GET_PEERS_REQ to inform about a newly connected peer.

- 0x04 NETWORK_CHANGE_REQ (peer → server)
  - Payload:
    - `OldAddress`: Address
  - Semantics: Informs server that the peer migrated networks. Server updates peer’s address to the currently observed remote address; the `OldAddress` is advisory for fanout notifications.

- 0x05 NETWORK_CHANGE_NOTIF (server → peer)
  - Payload:
    - `PeerID`: 4 bytes
    - `OldAddress`: Address
    - `NewAddress`: Address (server‑observed)
  - Semantics: Broadcast on the notification stream to all other peers.

- 0x06 AUDIO_RELAY_REQ (peer → relay) [first frame on a fresh stream]
  - Payload:
    - `TargetPeerID`: 4 bytes
  - Semantics: On a newly opened QUIC stream to the Relay, the peer sends this one control message. After the `AUDIO_RELAY_REQ` header+payload, the peer immediately sends raw audio bytes on the same stream. The Relay forwards these bytes to the target peer over another stream it opens toward the target.

- 0x07 RELAY_ALLOWLIST_SET (peer → relay)
  - Payload:
    - `Count`: 1 byte (`uint8`)
    - Repeated `Count` times:
      - `Address`: Address (AF + IP + Port)
  - Semantics: Replaces the relay allow‑list for the sending peer. The Relay MUST forward only packets/streams originating from source addresses present in this list and MUST discard others. Send at initial relay connection establishment; resend whenever the counterparty's address changes.

## Audio Relay Stream
- After sending 0x06 on a fresh stream, the remainder of that stream is raw audio data (codec/format negotiated out‑of‑band for now; current implementation uses MP3 frames). No additional control framing is applied to media bytes.
- The target peer receives a corresponding inbound stream carrying those raw audio bytes.

## Flow Summary
- Peer discovery
  - Peer → Server: 0x01 GET_PEERS_REQ
  - Server → Peer: 0x02 PEER_LIST_RESP (list)
  - Server → Peer: 0x03 NEW_PEER_NOTIF (push)
- Network migration
  - Peer → Server: 0x04 NETWORK_CHANGE_REQ (old address only)
  - Server → Peers: 0x05 NETWORK_CHANGE_NOTIF (peerID, old/new)
- Audio relay
  - Initial setup: Peer sets allow‑list at Relay: 0x07 RELAY_ALLOWLIST_SET (e.g., allow the counterparty’s address)
  - Peer opens new stream to Relay
  - Peer → Relay: 0x06 AUDIO_RELAY_REQ on that stream
  - Then raw audio bytes on same stream until close
  - On counterparty address change (received via 0x05 from intermediate): Peer immediately sends updated 0x07 RELAY_ALLOWLIST_SET to the Relay to reflect the new allowed source address.

## Relay Enforcement Rules
- Default‑deny: If no allow list is present, the Relay MUST drop all forwarding attempts.
- Replace semantics: Each `RELAY_ALLOWLIST_SET` fully replaces the previous list for that peer connection.
- Scope: The allow list is scoped to the authenticated QUIC connection of the sending peer.
- Matching: Relay MUST match the observed source address of inbound traffic it would forward against the allow list (Address struct match: AF + IP + Port). Non‑matching traffic MUST be discarded.

## Size Considerations
- Header is 3 bytes flat.
- IPv4 `Address` is 7 bytes; IPv6 is 19 bytes.
- `PeerID` is 4 bytes to keep lookups compact while supporting large fan‑out.
- `PEER_LIST_RESP` scales as 4+7/19 bytes per entry; if the list would exceed 65535 payload bytes, the server may split across multiple 0x02 messages on the same stream.
 - `RELAY_ALLOWLIST_SET` supports up to 255 addresses per message (1‑byte count). Multiple messages may be sent to overwrite with a new set.
