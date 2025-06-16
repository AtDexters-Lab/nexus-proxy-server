package protocol

import "github.com/google/uuid"

// Control bytes for peer binary messages for tunneled data.
const (
	PeerTunnelData  byte = 0x11
	PeerTunnelClose byte = 0x12
)

// PeerMessageType defines the type of a JSON control message sent between peers.
type PeerMessageType string

const (
	PeerAnnounce      PeerMessageType = "announce"
	PeerTunnelRequest PeerMessageType = "tunnel_request"
)

// PeerMessage is the structure for JSON control messages exchanged between peers.
// Note: Payload is not used for JSON messages, it's for conceptual clarity.
// Actual tunneled data is sent via binary messages for efficiency.
type PeerMessage struct {
	Version   uint64          `json:"version,omitempty"`
	Type      PeerMessageType `json:"type"`
	Hostnames []string        `json:"hostnames,omitempty"`
	// Fields for tunneling request
	ClientID uuid.UUID `json:"client_id,omitempty"`
	Hostname string    `json:"hostname,omitempty"`
}
