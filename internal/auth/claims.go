package auth

import "github.com/golang-jwt/jwt/v5"

// Claims represents the JWT payload expected from attested backends.
type Claims struct {
	Hostnames                  []string `json:"hostnames"`
	Weight                     int      `json:"weight"`
	SessionNonce               string   `json:"session_nonce"`
	HandshakeMaxAgeSeconds     *int     `json:"handshake_max_age_seconds"`
	ReauthIntervalSeconds      *int     `json:"reauth_interval_seconds"`
	ReauthGraceSeconds         *int     `json:"reauth_grace_seconds"`
	MaintenanceGraceCapSeconds *int     `json:"maintenance_grace_cap_seconds"`
	AuthorizerStatusURI        string   `json:"authorizer_status_uri"`
	PolicyVersion              string   `json:"policy_version"`
	IssuedAtQuote              string   `json:"issued_at_quote"`
	jwt.RegisteredClaims
}

// Copy returns a deep copy of claims to avoid sharing state across goroutines.
func (c *Claims) Copy() *Claims {
	if c == nil {
		return nil
	}
	copyClaims := *c
	if len(c.Hostnames) > 0 {
		copyClaims.Hostnames = append([]string{}, c.Hostnames...)
	}
	return &copyClaims
}
