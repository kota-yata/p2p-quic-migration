package cmp9protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

type MessageType uint8

const (
    TypeGetPeersReq        MessageType = 0x01
    TypePeerListResp       MessageType = 0x02
    TypeNewPeerNotif       MessageType = 0x03
    TypeNetworkChangeReq   MessageType = 0x04
    TypeNetworkChangeNotif MessageType = 0x05
    TypeAudioRelayReq      MessageType = 0x06
    TypeRelayAllowlistSet  MessageType = 0x07
    // v2 automatic local/global addressing
    TypeObservedAddr           MessageType = 0x08
    TypeSelfAddrsSet           MessageType = 0x09
    TypeGetPeerEndpointsReq    MessageType = 0x0A
    TypePeerEndpointsResp      MessageType = 0x0B
    TypeNewPeerEndpointNotif   MessageType = 0x0C
)

// Message is the common interface for all control messages.
type Message interface {
	Type() MessageType
	MarshalBinaryPayload() ([]byte, error)
}

// Address is a compact socket address encoding.
// AF: 0x04 (IPv4) or 0x06 (IPv6)
type Address struct {
    AF   uint8
    IP   net.IP
    Port uint16
}

func (a Address) MarshalBinary() ([]byte, error) {
	var ipBytes []byte
	switch a.AF {
	case 0x04:
		ip4 := a.IP.To4()
		if ip4 == nil {
			return nil, fmt.Errorf("invalid IPv4: %v", a.IP)
		}
		ipBytes = ip4
	case 0x06:
		ip16 := a.IP.To16()
		if ip16 == nil || ip16.To4() != nil {
			return nil, fmt.Errorf("invalid IPv6: %v", a.IP)
		}
		ipBytes = ip16
	default:
		return nil, fmt.Errorf("invalid AF: %x", a.AF)
	}

	out := make([]byte, 0, 1+len(ipBytes)+2)
	out = append(out, a.AF)
	out = append(out, ipBytes...)
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], a.Port)
	out = append(out, port[:]...)
	return out, nil
}

func (a *Address) UnmarshalBinary(b []byte) (int, error) {
	if len(b) < 1 {
		return 0, io.ErrUnexpectedEOF
	}
	af := b[0]
	switch af {
	case 0x04:
		if len(b) < 1+4+2 {
			return 0, io.ErrUnexpectedEOF
		}
		a.AF = af
		a.IP = net.IP(b[1 : 1+4]).To4()
		a.Port = binary.BigEndian.Uint16(b[1+4 : 1+4+2])
		return 1 + 4 + 2, nil
	case 0x06:
		if len(b) < 1+16+2 {
			return 0, io.ErrUnexpectedEOF
		}
		a.AF = af
		a.IP = net.IP(b[1 : 1+16])
		a.Port = binary.BigEndian.Uint16(b[1+16 : 1+16+2])
		return 1 + 16 + 2, nil
	default:
		return 0, fmt.Errorf("invalid AF: %x", af)
	}
}

type GetPeersReq struct{}

func (GetPeersReq) Type() MessageType                     { return TypeGetPeersReq }
func (GetPeersReq) MarshalBinaryPayload() ([]byte, error) { return nil, nil }

type PeerEntry struct {
	PeerID  uint32
	Address Address
}

type PeerListResp struct {
	Peers []PeerEntry
}

func (PeerListResp) Type() MessageType { return TypePeerListResp }
func (m PeerListResp) MarshalBinaryPayload() ([]byte, error) {
	// Count (2) + N*(PeerID 4 + Address 7/19)
	// Build into a buffer
	// First, ensure each address serializes
	buf := make([]byte, 2)
	if len(m.Peers) > 0xFFFF {
		return nil, errors.New("too many peers to encode in one message")
	}
	binary.BigEndian.PutUint16(buf[:2], uint16(len(m.Peers)))
	for _, p := range m.Peers {
		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], p.PeerID)
		buf = append(buf, tmp[:]...)
		ab, err := p.Address.MarshalBinary()
		if err != nil {
			return nil, err
		}
		buf = append(buf, ab...)
	}
	return buf, nil
}

type NewPeerNotif struct {
	PeerID  uint32
	Address Address
}

func (NewPeerNotif) Type() MessageType { return TypeNewPeerNotif }
func (m NewPeerNotif) MarshalBinaryPayload() ([]byte, error) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], m.PeerID)
	out := tmp[:]
	ab, err := m.Address.MarshalBinary()
	if err != nil {
		return nil, err
	}
	out = append(out, ab...)
	return out, nil
}

type NetworkChangeReq struct {
	OldAddress Address
}

func (NetworkChangeReq) Type() MessageType { return TypeNetworkChangeReq }
func (m NetworkChangeReq) MarshalBinaryPayload() ([]byte, error) {
	return m.OldAddress.MarshalBinary()
}

type NetworkChangeNotif struct {
	PeerID     uint32
	OldAddress Address
	NewAddress Address
}

func (NetworkChangeNotif) Type() MessageType { return TypeNetworkChangeNotif }
func (m NetworkChangeNotif) MarshalBinaryPayload() ([]byte, error) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], m.PeerID)
	out := tmp[:]
	abOld, err := m.OldAddress.MarshalBinary()
	if err != nil {
		return nil, err
	}
	out = append(out, abOld...)
	abNew, err := m.NewAddress.MarshalBinary()
	if err != nil {
		return nil, err
	}
	out = append(out, abNew...)
	return out, nil
}

type AudioRelayReq struct {
    TargetPeerID uint32
}

func (AudioRelayReq) Type() MessageType { return TypeAudioRelayReq }
func (m AudioRelayReq) MarshalBinaryPayload() ([]byte, error) {
    var tmp [4]byte
    binary.BigEndian.PutUint32(tmp[:], m.TargetPeerID)
    return tmp[:], nil
}

// RelayAllowlistSet replaces the relay allow list for the sending peer connection.
type RelayAllowlistSet struct {
    Addresses []Address // up to 255 entries
}

func (RelayAllowlistSet) Type() MessageType { return TypeRelayAllowlistSet }
func (m RelayAllowlistSet) MarshalBinaryPayload() ([]byte, error) {
    if len(m.Addresses) > 255 {
        return nil, errors.New("too many addresses for RELAY_ALLOWLIST_SET")
    }
    out := []byte{byte(len(m.Addresses))}
    for _, a := range m.Addresses {
        ab, err := a.MarshalBinary()
        if err != nil {
            return nil, err
        }
        out = append(out, ab...)
    }
    return out, nil
}

// WriteMessage writes a single framed message (Type, Length, Payload) to w.
func WriteMessage(w io.Writer, m Message) error {
	payload, err := m.MarshalBinaryPayload()
	if err != nil {
		return err
	}
	if len(payload) > 0xFFFF {
		return errors.New("payload too large")
	}
	header := []byte{byte(m.Type()), 0, 0}
	binary.BigEndian.PutUint16(header[1:3], uint16(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err = w.Write(payload)
	return err
}

// ReadMessage reads exactly one framed message from r.
func ReadMessage(r io.Reader) (Message, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	mtype := MessageType(hdr[0])
	length := binary.BigEndian.Uint16(hdr[1:3])
	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return decodePayload(mtype, payload)
}

func decodePayload(t MessageType, p []byte) (Message, error) {
    switch t {
	case TypeGetPeersReq:
		if len(p) != 0 {
			return nil, errors.New("GET_PEERS_REQ must have empty payload")
		}
		return GetPeersReq{}, nil
	case TypePeerListResp:
		if len(p) < 2 {
			return nil, io.ErrUnexpectedEOF
		}
		count := binary.BigEndian.Uint16(p[:2])
		off := 2
		peers := make([]PeerEntry, 0, count)
		for i := 0; i < int(count); i++ {
			if len(p[off:]) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			pid := binary.BigEndian.Uint32(p[off : off+4])
			off += 4
			var addr Address
			n, err := addr.UnmarshalBinary(p[off:])
			if err != nil {
				return nil, err
			}
			off += n
			peers = append(peers, PeerEntry{PeerID: pid, Address: addr})
		}
		if off != len(p) {
			return nil, errors.New("extra bytes in PEER_LIST_RESP payload")
		}
		return PeerListResp{Peers: peers}, nil
	case TypeNewPeerNotif:
		if len(p) < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		pid := binary.BigEndian.Uint32(p[:4])
		var addr Address
		n, err := addr.UnmarshalBinary(p[4:])
		if err != nil {
			return nil, err
		}
		if 4+n != len(p) {
			return nil, errors.New("extra bytes in NEW_PEER_NOTIF payload")
		}
		return NewPeerNotif{PeerID: pid, Address: addr}, nil
	case TypeNetworkChangeReq:
		var addr Address
		n, err := addr.UnmarshalBinary(p)
		if err != nil {
			return nil, err
		}
		if n != len(p) {
			return nil, errors.New("extra bytes in NETWORK_CHANGE_REQ payload")
		}
		return NetworkChangeReq{OldAddress: addr}, nil
	case TypeNetworkChangeNotif:
		if len(p) < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		pid := binary.BigEndian.Uint32(p[:4])
		off := 4
		var oldA Address
		n1, err := oldA.UnmarshalBinary(p[off:])
		if err != nil {
			return nil, err
		}
		off += n1
		var newA Address
		n2, err := newA.UnmarshalBinary(p[off:])
		if err != nil {
			return nil, err
		}
		off += n2
		if off != len(p) {
			return nil, errors.New("extra bytes in NETWORK_CHANGE_NOTIF payload")
		}
		return NetworkChangeNotif{PeerID: pid, OldAddress: oldA, NewAddress: newA}, nil
    case TypeAudioRelayReq:
        if len(p) != 4 {
            return nil, errors.New("AUDIO_RELAY_REQ payload must be 4 bytes")
        }
        pid := binary.BigEndian.Uint32(p[:4])
        return AudioRelayReq{TargetPeerID: pid}, nil
    case TypeRelayAllowlistSet:
        if len(p) < 1 {
            return nil, io.ErrUnexpectedEOF
        }
        count := int(p[0])
        off := 1
        addrs := make([]Address, 0, count)
        for i := 0; i < count; i++ {
            var a Address
            n, err := a.UnmarshalBinary(p[off:])
            if err != nil {
                return nil, err
            }
            off += n
            addrs = append(addrs, a)
        }
        if off != len(p) {
            return nil, errors.New("extra bytes in RELAY_ALLOWLIST_SET payload")
        }
        return RelayAllowlistSet{Addresses: addrs}, nil
    case TypeObservedAddr:
        var a Address
        n, err := a.UnmarshalBinary(p)
        if err != nil {
            return nil, err
        }
        if n != len(p) {
            return nil, errors.New("extra bytes in OBSERVED_ADDR payload")
        }
        return ObservedAddr{Observed: a}, nil
    case TypeSelfAddrsSet:
        // Observed Address + Flags (1 byte) [+ Local Address if flag set]
        var obs Address
        off, err := obs.UnmarshalBinary(p)
        if err != nil {
            return nil, err
        }
        if len(p[off:]) < 1 {
            return nil, io.ErrUnexpectedEOF
        }
        flags := p[off]
        off++
        var local Address
        if flags&0x01 != 0 {
            n, err := local.UnmarshalBinary(p[off:])
            if err != nil {
                return nil, err
            }
            off += n
        }
        if off != len(p) {
            return nil, errors.New("extra bytes in SELF_ADDRS_SET payload")
        }
        return SelfAddrsSet{Observed: obs, HasLocal: flags&0x01 != 0, Local: local}, nil
    case TypeGetPeerEndpointsReq:
        if len(p) != 0 {
            return nil, errors.New("GET_PEER_ENDPOINTS_REQ must have empty payload")
        }
        return GetPeerEndpointsReq{}, nil
    case TypePeerEndpointsResp:
        if len(p) < 2 {
            return nil, io.ErrUnexpectedEOF
        }
        count := int(binary.BigEndian.Uint16(p[:2]))
        off := 2
        entries := make([]PeerEndpoint, 0, count)
        for i := 0; i < count; i++ {
            ep, n, err := unmarshalPeerEndpoint(p[off:])
            if err != nil {
                return nil, err
            }
            off += n
            entries = append(entries, ep)
        }
        if off != len(p) {
            return nil, errors.New("extra bytes in PEER_ENDPOINTS_RESP payload")
        }
        return PeerEndpointsResp{Entries: entries}, nil
    case TypeNewPeerEndpointNotif:
        ep, n, err := unmarshalPeerEndpoint(p)
        if err != nil {
            return nil, err
        }
        if n != len(p) {
            return nil, errors.New("extra bytes in NEW_PEER_ENDPOINT_NOTIF payload")
        }
        return NewPeerEndpointNotif{Entry: ep}, nil
    default:
        return nil, fmt.Errorf("unknown message type: 0x%02x", uint8(t))
    }
}

// v2: endpoint directory

// ObservedAddr is pushed by the server immediately on control stream start.
type ObservedAddr struct {
    Observed Address
}

func (ObservedAddr) Type() MessageType { return TypeObservedAddr }
func (m ObservedAddr) MarshalBinaryPayload() ([]byte, error) {
    return m.Observed.MarshalBinary()
}

// SelfAddrsSet is sent by the peer once after receiving ObservedAddr.
type SelfAddrsSet struct {
    Observed Address
    HasLocal bool
    Local    Address
}

func (SelfAddrsSet) Type() MessageType { return TypeSelfAddrsSet }
func (m SelfAddrsSet) MarshalBinaryPayload() ([]byte, error) {
    ob, err := m.Observed.MarshalBinary()
    if err != nil {
        return nil, err
    }
    flags := byte(0)
    if m.HasLocal {
        flags |= 0x01
    }
    out := append([]byte{}, ob...)
    out = append(out, flags)
    if m.HasLocal {
        lb, err := m.Local.MarshalBinary()
        if err != nil {
            return nil, err
        }
        out = append(out, lb...)
    }
    return out, nil
}

type GetPeerEndpointsReq struct{}

func (GetPeerEndpointsReq) Type() MessageType                     { return TypeGetPeerEndpointsReq }
func (GetPeerEndpointsReq) MarshalBinaryPayload() ([]byte, error) { return nil, nil }

// PeerEndpoint describes a peer's observed and optional local address.
type PeerEndpoint struct {
    PeerID  uint32
    Flags   uint8 // bit0: HasLocal
    Observed Address
    Local    Address // valid only if Flags&1
}

func (ep PeerEndpoint) MarshalBinary() ([]byte, error) {
    var out []byte
    var idb [4]byte
    idb[0] = byte(ep.PeerID >> 24)
    idb[1] = byte(ep.PeerID >> 16)
    idb[2] = byte(ep.PeerID >> 8)
    idb[3] = byte(ep.PeerID)
    out = append(out, idb[:]...)
    out = append(out, ep.Flags)
    ob, err := ep.Observed.MarshalBinary()
    if err != nil {
        return nil, err
    }
    out = append(out, ob...)
    if ep.Flags&0x01 != 0 {
        lb, err := ep.Local.MarshalBinary()
        if err != nil {
            return nil, err
        }
        out = append(out, lb...)
    }
    return out, nil
}

func unmarshalPeerEndpoint(p []byte) (PeerEndpoint, int, error) {
    var ep PeerEndpoint
    if len(p) < 5 {
        return ep, 0, io.ErrUnexpectedEOF
    }
    ep.PeerID = binary.BigEndian.Uint32(p[:4])
    ep.Flags = p[4]
    off := 5
    var obs Address
    n, err := obs.UnmarshalBinary(p[off:])
    if err != nil {
        return ep, 0, err
    }
    ep.Observed = obs
    off += n
    if ep.Flags&0x01 != 0 {
        var loc Address
        n2, err := loc.UnmarshalBinary(p[off:])
        if err != nil {
            return ep, 0, err
        }
        ep.Local = loc
        off += n2
    }
    return ep, off, nil
}

type PeerEndpointsResp struct {
    Entries []PeerEndpoint
}

func (PeerEndpointsResp) Type() MessageType { return TypePeerEndpointsResp }
func (m PeerEndpointsResp) MarshalBinaryPayload() ([]byte, error) {
    if len(m.Entries) > 0xFFFF {
        return nil, errors.New("too many endpoints")
    }
    out := make([]byte, 2)
    binary.BigEndian.PutUint16(out[:2], uint16(len(m.Entries)))
    for _, e := range m.Entries {
        b, err := e.MarshalBinary()
        if err != nil {
            return nil, err
        }
        out = append(out, b...)
    }
    return out, nil
}

type NewPeerEndpointNotif struct {
    Entry PeerEndpoint
}

func (NewPeerEndpointNotif) Type() MessageType { return TypeNewPeerEndpointNotif }
func (m NewPeerEndpointNotif) MarshalBinaryPayload() ([]byte, error) {
    return m.Entry.MarshalBinary()
}
