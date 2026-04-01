package hub_test

import (
	"context"

	"github.com/AtDexters-Lab/nexus-proxy/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
)

type stubValidator struct {
	claims *auth.Claims
	err    error
}

func (s stubValidator) Validate(ctx context.Context, token string) (*auth.Claims, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.claims != nil {
		return s.claims.Copy(), nil
	}
	return (&auth.Claims{BackendClaims: protocol.BackendClaims{Hostnames: []string{"example.com"}}}).Copy(), nil
}
