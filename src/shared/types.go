package shared

import "time"

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