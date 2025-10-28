package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

// Validator validates backend attestation tokens and returns parsed claims.
type Validator interface {
	Validate(ctx context.Context, token string) (*Claims, error)
}

// NewValidator returns a Validator that uses a remote verifier when configured
// and falls back to local HMAC validation with backendsJWTSecret.
func NewValidator(cfg *config.Config) (Validator, error) {
	var local Validator
	if cfg.BackendsJWTSecret != "" {
		local = &localValidator{secret: []byte(cfg.BackendsJWTSecret)}
	}

	if cfg.RemoteVerifierURL == "" {
		if local == nil {
			return nil, errors.New("backendsJWTSecret must be set when remoteVerifierURL is not configured")
		}
		return local, nil
	}

	timeout := cfg.RemoteVerifierTimeout()
	client := &http.Client{Timeout: timeout}
	remote := &remoteValidator{
		url:      cfg.RemoteVerifierURL,
		client:   client,
		fallback: local,
	}
	return remote, nil
}

type localValidator struct {
	secret []byte
}

func (v *localValidator) Validate(ctx context.Context, token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.secret, nil
	}, jwt.WithAudience("nexus"), jwt.WithIssuer("authorizer"))
	if err != nil {
		return nil, fmt.Errorf("jwt validation failed: %w", err)
	}
	if !parsed.Valid {
		return nil, errors.New("invalid jwt token")
	}
	return claims.Copy(), nil
}

type remoteValidator struct {
	url      string
	client   *http.Client
	fallback Validator
}

type remoteRequest struct {
	Token string `json:"token"`
}

type remoteResponse struct {
	Valid  bool    `json:"valid"`
	Claims *Claims `json:"claims"`
	Error  string  `json:"error"`
}

func (v *remoteValidator) Validate(ctx context.Context, token string) (*Claims, error) {
	payload, err := json.Marshal(remoteRequest{Token: token})
	if err != nil {
		return nil, fmt.Errorf("marshal remote verifier request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create remote verifier request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		if v.fallback != nil {
			return v.fallback.Validate(ctx, token)
		}
		return nil, fmt.Errorf("remote verifier request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		if v.fallback != nil {
			return v.fallback.Validate(ctx, token)
		}
		return nil, fmt.Errorf("remote verifier returned status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote verifier rejected token with status %d", resp.StatusCode)
	}

	var body remoteResponse
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode remote verifier response: %w", err)
	}

	if !body.Valid {
		if body.Error != "" {
			return nil, fmt.Errorf("remote verifier invalid token: %s", body.Error)
		}
		return nil, errors.New("remote verifier rejected token")
	}
	if body.Claims == nil {
		return nil, errors.New("remote verifier returned no claims")
	}
	return body.Claims.Copy(), nil
}
