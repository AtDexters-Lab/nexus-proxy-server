package protocol

import (
	"strconv"

	"github.com/google/uuid"
)

type Transport string

const (
	TransportTCP Transport = "tcp"
	TransportUDP Transport = "udp"
)

const (
	TokenIssuer   = "authorizer"
	TokenAudience = "nexus"
)

const (
	RouteKeyPrefixTCP = "tcp:"
	RouteKeyPrefixUDP = "udp:"
)

// RouteKey returns the route key for a port-claimed transport and port.
func RouteKey(transport Transport, port int) string {
	switch transport {
	case TransportUDP:
		return RouteKeyPrefixUDP + strconv.Itoa(port)
	default:
		return RouteKeyPrefixTCP + strconv.Itoa(port)
	}
}

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
	// EventOutboundConnect is sent from a backend to request the proxy to
	// open an outbound TCP connection to an external target on its behalf.
	EventOutboundConnect EventType = "outbound_connect"
	// EventOutboundResult is sent from the proxy back to the backend with
	// the result of an outbound connection request.
	EventOutboundResult EventType = "outbound_result"
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
	// TargetAddr is the host:port that the backend wants to connect to
	// (used with EventOutboundConnect).
	TargetAddr string `json:"target_addr,omitempty"`
	// Success indicates whether the outbound connection was established
	// (used with EventOutboundResult).
	Success bool `json:"success,omitempty"`
	// Credits carries flow control credits for per-client backpressure.
	// Sent with EventConnect/EventOutboundResult to grant initial credits,
	// and with EventResumeStream to replenish. Credits=0 (or absent via
	// omitempty) means the sender does not support credit-based flow control.
	Credits int64 `json:"credits,omitempty"`
}

const (
	// DefaultCreditCapacity is the initial credit grant per client connection.
	// Equals the receiver's buffer capacity — the sender cannot exceed this
	// without receiving replenishment.
	DefaultCreditCapacity int64 = 64

	// CreditReplenishBatch is the number of consumed messages before the
	// receiver sends a credit replenishment. Smaller batches reduce stall
	// time but increase control message overhead.
	CreditReplenishBatch int64 = 8
)

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

// BackendClaims represents the custom attestation fields shared by both the
// client (token producer) and the server (token consumer). Each side embeds
// this struct alongside jwt.RegisteredClaims locally.
type BackendClaims struct {
	Hostnames                  []string        `json:"hostnames,omitempty"`
	TCPPorts                   []int           `json:"tcp_ports,omitempty"`
	UDPRoutes                  []UDPRouteClaim `json:"udp_routes,omitempty"`
	Weight                     int             `json:"weight"`
	SessionNonce               string          `json:"session_nonce,omitempty"`
	HandshakeMaxAgeSeconds     *int            `json:"handshake_max_age_seconds,omitempty"`
	ReauthIntervalSeconds      *int            `json:"reauth_interval_seconds,omitempty"`
	ReauthGraceSeconds         *int            `json:"reauth_grace_seconds,omitempty"`
	MaintenanceGraceCapSeconds *int            `json:"maintenance_grace_cap_seconds,omitempty"`
	AuthorizerStatusURI        string          `json:"authorizer_status_uri,omitempty"`
	PolicyVersion              string          `json:"policy_version,omitempty"`
	OutboundAllowed            bool            `json:"outbound_allowed,omitempty"`
	AllowedOutboundPorts       []int           `json:"allowed_outbound_ports,omitempty"`
}

// UDPRouteClaim represents a UDP route within attestation claims.
type UDPRouteClaim struct {
	Port                   int  `json:"port"`
	FlowIdleTimeoutSeconds *int `json:"flow_idle_timeout_seconds,omitempty"`
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
