package hub

import (
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
)

type UDPRoutePolicy struct {
	Port            int
	FlowIdleTimeout time.Duration
}

// AttestationMetadata captures the policy directives extracted from the
// attested token that Nexus must enforce for a backend connection.
//
// Rejected and Truncated are informational only — they describe which
// claims the relay soft-rejected during policy normalization. They are
// NEVER branched on inside the data path; their sole consumer is the
// AttestationResultMessage sent to the backend post-success.
//
// OutboundPortsExplicit captures whether the backend's attestation token
// included a non-empty allowed_outbound_ports list, BEFORE soft-filtering.
// Required to preserve privilege boundaries: a claim of [25] against a
// server allowlist of [443] filters to empty, which would otherwise be
// indistinguishable from "no restriction" (the existing semantic for an
// empty claim). Read at outbound-dial time so a device that restricted
// itself remains restricted even if zero of its claimed ports survived.
type AttestationMetadata struct {
	Hostnames             []string
	TCPPorts              []int
	UDPRoutes             []UDPRoutePolicy
	Weight                int
	ReauthInterval        time.Duration
	ReauthGrace           time.Duration
	MaintenanceCap        time.Duration
	HasMaintenanceCap     bool
	AuthorizerStatusURI   string
	PolicyVersion         string
	OutboundAllowed       bool
	AllowedOutboundPorts  []int
	OutboundPortsExplicit bool
	Rejected              []protocol.RejectedClaim
	Truncated             bool
}

func (m *AttestationMetadata) cloneHostnames() []string {
	if m == nil || len(m.Hostnames) == 0 {
		return nil
	}
	dup := make([]string, len(m.Hostnames))
	copy(dup, m.Hostnames)
	return dup
}

func (m *AttestationMetadata) cloneTCPPorts() []int {
	if m == nil || len(m.TCPPorts) == 0 {
		return nil
	}
	dup := make([]int, len(m.TCPPorts))
	copy(dup, m.TCPPorts)
	return dup
}

func (m *AttestationMetadata) cloneUDPRoutes() []UDPRoutePolicy {
	if m == nil || len(m.UDPRoutes) == 0 {
		return nil
	}
	dup := make([]UDPRoutePolicy, len(m.UDPRoutes))
	copy(dup, m.UDPRoutes)
	return dup
}

func (m *AttestationMetadata) cloneAllowedOutboundPorts() []int {
	if m == nil || len(m.AllowedOutboundPorts) == 0 {
		return nil
	}
	dup := make([]int, len(m.AllowedOutboundPorts))
	copy(dup, m.AllowedOutboundPorts)
	return dup
}

func (m *AttestationMetadata) cloneRejected() []protocol.RejectedClaim {
	if m == nil || len(m.Rejected) == 0 {
		return nil
	}
	dup := make([]protocol.RejectedClaim, len(m.Rejected))
	copy(dup, m.Rejected)
	return dup
}
