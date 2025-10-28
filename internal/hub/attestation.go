package hub

import "time"

// AttestationMetadata captures the policy directives extracted from the
// attested token that Nexus must enforce for a backend connection.
type AttestationMetadata struct {
	Hostnames           []string
	Weight              int
	ReauthInterval      time.Duration
	ReauthGrace         time.Duration
	MaintenanceCap      time.Duration
	HasMaintenanceCap   bool
	AuthorizerStatusURI string
	PolicyVersion       string
}

func (m *AttestationMetadata) cloneHostnames() []string {
	if m == nil || len(m.Hostnames) == 0 {
		return nil
	}
	dup := make([]string, len(m.Hostnames))
	copy(dup, m.Hostnames)
	return dup
}
