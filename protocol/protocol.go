package protocol

import "github.com/google/uuid"

type Transport string

const (
	TransportTCP Transport = "tcp"
	TransportUDP Transport = "udp"
)

const (
	// ClientIDLength is the expected length of a client's unique identifier (UUID).
	ClientIDLength = 16
	// ControlByteData indicates a standard data message.
	ControlByteData byte = 0x01
	// ControlByteControl indicates a JSON control message.
	ControlByteControl byte = 0x02

	MessageHeaderLength = 1 + ClientIDLength // Length of the control message header, including control byte and client ID
)

// EventType defines the type of a control message event.
type EventType string

const (
	// EventConnect is sent to a backend when a new client connects.
	EventConnect EventType = "connect"
	// EventDisconnect is sent to a backend when a client disconnects.
	EventDisconnect EventType = "disconnect"
	// EventPingClient is sent from a backend to the proxy to check liveness.
	EventPingClient EventType = "ping_client"
	// EventPongClient is sent from the proxy to a backend in response to a ping.
	EventPongClient EventType = "pong_client"
	// EventPauseStream is sent from a backend to pause reading from a client.
	EventPauseStream EventType = "pause_stream"
	// EventResumeStream is sent from a backend to resume reading from a client.
	EventResumeStream EventType = "resume_stream"
)

// ControlMessage defines the structure for out-of-band communication
// between the proxy and the backend.
type ControlMessage struct {
	Event     EventType `json:"event"`
	ClientID  uuid.UUID `json:"client_id"`
	ConnPort  int       `json:"conn_port,omitempty"`
	ClientIP  string    `json:"client_ip,omitempty"`
	Transport Transport `json:"transport,omitempty"`
	// Hostname is the virtual host this client connected for. Included on connect.
	Hostname string `json:"hostname,omitempty"`
	// IsTLS indicates whether the original connection was negotiated over TLS.
	IsTLS bool `json:"is_tls,omitempty"`
	// Reason provides context for disconnect or pause events.
	Reason string `json:"reason,omitempty"`
}

// ChallengeType identifies a WebSocket text-frame challenge during authentication.
type ChallengeType string

const (
	ChallengeHandshake ChallengeType = "handshake_challenge"
	ChallengeReauth    ChallengeType = "reauth_challenge"
)

// ChallengeMessage is a JSON text-frame exchanged during the
// handshake and re-authentication flows.
type ChallengeMessage struct {
	Type  ChallengeType `json:"type"`
	Nonce string        `json:"nonce"`
}

// DisconnectReason identifies why a backend disconnected a client.
type DisconnectReason string

const (
	DisconnectNormal        DisconnectReason = "normal"
	DisconnectBufferFull    DisconnectReason = "buffer_full"
	DisconnectDialFailed    DisconnectReason = "dial_failed"
	DisconnectTimeout       DisconnectReason = "timeout"
	DisconnectLocalError    DisconnectReason = "local_error"
	DisconnectShutdown      DisconnectReason = "shutdown"
	DisconnectSessionEnded  DisconnectReason = "session_ended"
	DisconnectPauseViolated DisconnectReason = "pause_violated"
	DisconnectUnknown       DisconnectReason = "unknown"
)
