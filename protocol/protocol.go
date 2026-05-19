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

// AttestationResultType identifies a hub→backend disposition frame indicating
// which claims the relay accepted vs. soft-rejected during attestation.
type AttestationResultType string

const (
	AttestationResultHandshake AttestationResultType = "handshake_result"
	AttestationResultReauth    AttestationResultType = "reauth_result"
)

// Discriminator contract: all hub→backend text frames carry a top-level
// "type" field as JSON discriminator (ChallengeMessage and AttestationResultMessage).
// Future text-frame types MUST follow the same shape; protocol_test.go enforces it.

// Closed-set Kind values for RejectedClaim.
const (
	RejectedKindHostname     = "hostname"
	RejectedKindTCPPort      = "tcp_port"
	RejectedKindUDPRoute     = "udp_route"
	RejectedKindOutboundPort = "outbound_port"
)

// Closed-set Code values for RejectedClaim (snake_case <resource>_<reason>).
const (
	RejectedCodeHostnameReservedRouteKey = "hostname_reserved_route_key"
	RejectedCodeTCPPortNotAllowed        = "tcp_port_not_allowed"
	RejectedCodeUDPRouteNotAllowed       = "udp_route_not_allowed"
	RejectedCodeOutboundPortNotAllowed   = "outbound_port_not_allowed"
)

// RejectedClaim describes a single claim entry the relay soft-rejected.
//
// INFORMATIONAL ONLY. Downstream consumers MUST NOT branch policy decisions
// on rejected entries; future schema changes are explicitly permitted to
// break consumers that do. See deferred_result_frame_integrity for the
// signing work that will lift this constraint.
//
// Value is ATTACKER-CONTROLLED (a backend's claim string — hostnames can
// survive IDNA-fallback with embedded control bytes; future Kinds may carry
// other backend-supplied strings). Log/wire sinks must escape it: the relay's
// log uses %q formatting; the close-reason path filters to an ASCII charset
// with <elided> sentinel; the wire frame is JSON-encoded (escapes control
// bytes by spec). Any new sink reading Value MUST apply a comparable escape.
//
// Code is the stable machine-readable string consumers may read for telemetry.
// Reason is human prose for debug aid; never used for equality or branching.
type RejectedClaim struct {
	Kind   string `json:"kind"`
	Code   string `json:"code"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// AcceptedClaims echoes the final accepted set after policy filtering.
//
// OutboundPortsExplicit disambiguates two states that would otherwise look
// identical on the wire (both produce an empty AllowedOutboundPorts):
//   - false: the device's attestation claim omitted the port list entirely;
//            outbound is unrestricted at the backend level (the server-side
//            allowlist still applies).
//   - true:  the device's claim listed ports but every entry was soft-rejected
//            by the server allowlist. Backend-level restriction is in effect
//            and the device can dial no port (consistent with runtime behavior).
type AcceptedClaims struct {
	Hostnames             []string        `json:"hostnames,omitempty"`
	TCPPorts              []int           `json:"tcp_ports,omitempty"`
	UDPRoutes             []UDPRouteClaim `json:"udp_routes,omitempty"`
	OutboundAllowed       bool            `json:"outbound_allowed"`
	AllowedOutboundPorts  []int           `json:"allowed_outbound_ports,omitempty"`
	OutboundPortsExplicit bool            `json:"outbound_ports_explicit,omitempty"`
}

// AttestationResultMessage is a JSON text-frame the hub sends to the backend
// post-success (handshake or reauth) carrying the policy disposition.
// On terminal-close paths, rejected codes ride in the 1008 close-frame
// reason string instead (see hub.encodeRejectedReason).
type AttestationResultMessage struct {
	Type      AttestationResultType `json:"type"`
	Accepted  AcceptedClaims        `json:"accepted"`
	Rejected  []RejectedClaim       `json:"rejected,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"`
}

// RejectedListBound caps the number of rejected entries per Kind in the
// success-path text frame. Prevents DoS from a hostile backend claiming
// thousands of disallowed entries.
const RejectedListBound = 32

// ClosePolicyReasonPrefix is the wire-format prefix for the 1008 close-frame
// reason when the relay encodes rejected codes (instead of a separate text
// frame) on terminal close paths. Encoder (hub) and parser (client) share
// this constant so framing-format drift is a compile error.
const ClosePolicyReasonPrefix = "policy:"

// ClosePolicyReasonTruncTail is the suffix appended when the encoder dropped
// trailing entries to fit the 123-byte budget. Client parsers detect this
// suffix to set Event.Truncated on EventDisconnected.
const ClosePolicyReasonTruncTail = ",..."

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
