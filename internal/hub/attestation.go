package hub

import "time"

type UDPRoutePolicy struct {
	Port            int
	FlowIdleTimeout time.Duration
}

// AttestationMetadata captures the policy directives extracted from the
// attested token that Nexus must enforce for a backend connection.
type AttestationMetadata struct {
	Hostnames           []string
	TCPPorts            []int
	UDPRoutes           []UDPRoutePolicy
	Weight              int
	ReauthInterval      time.Duration
	ReauthGrace         time.Duration
	MaintenanceCap      time.Duration
	HasMaintenanceCap   bool
	AuthorizerStatusURI  string
	PolicyVersion        string
	OutboundAllowed      bool
	AllowedOutboundPorts []int
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
