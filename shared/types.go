package shared

import "time"

// Types used from both peer and intermediary server

type PeerInfo struct {
	ID          string    `json:"id"`
	Address     string    `json:"address"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
}

type PeerNotification struct {
	Type string    `json:"type"`
	Peer *PeerInfo `json:"peer"`
}

type NetworkChangeNotification struct {
	Type       string `json:"type"`
	PeerID     string `json:"peer_id"`
	OldAddress string `json:"old_address"`
	NewAddress string `json:"new_address"`
}
