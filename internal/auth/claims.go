package auth

import (
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/golang-jwt/jwt/v5"
)

// Claims represents the JWT payload expected from attested backends.
type Claims struct {
	protocol.BackendClaims
	// IssuedAtQuote is a server-side concern for attestation freshness.
	IssuedAtQuote string `json:"issued_at_quote"`
	jwt.RegisteredClaims
}

// Copy returns a deep copy of claims to avoid sharing state across goroutines.
func (c *Claims) Copy() *Claims {
	if c == nil {
		return nil
	}
	cp := *c
	if len(c.Hostnames) > 0 {
		cp.Hostnames = append([]string{}, c.Hostnames...)
	}
	if len(c.TCPPorts) > 0 {
		cp.TCPPorts = append([]int{}, c.TCPPorts...)
	}
	if len(c.UDPRoutes) > 0 {
		cp.UDPRoutes = append([]protocol.UDPRouteClaim{}, c.UDPRoutes...)
	}
	if len(c.AllowedOutboundPorts) > 0 {
		cp.AllowedOutboundPorts = append([]int{}, c.AllowedOutboundPorts...)
	}
	return &cp
}
