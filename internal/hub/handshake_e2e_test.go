package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// jsonClaimsValidator decodes the "token" string as a JSON-encoded *auth.Claims.
// Used for end-to-end tests that drive performHandshake — the test crafts the
// token string itself and the validator round-trips it. Production validators
// verify JWT signatures; this stub bypasses crypto entirely so test fixtures
// stay readable.
type jsonClaimsValidator struct{}

func (jsonClaimsValidator) Validate(ctx context.Context, token string) (*auth.Claims, error) {
	var c auth.Claims
	if err := json.Unmarshal([]byte(token), &c); err != nil {
		return nil, fmt.Errorf("jsonClaimsValidator: decode: %w", err)
	}
	return &c, nil
}

// marshalClaims serializes auth.Claims to the JSON token format expected by
// jsonClaimsValidator. Helper for test readability.
func marshalClaims(t *testing.T, c auth.Claims) string {
	t.Helper()
	b, err := json.Marshal(&c)
	require.NoError(t, err)
	return string(b)
}

// driveHandshake performs the backend-side handshake protocol against a
// websocket dialed to a hub's handleBackendConnect endpoint. Returns the
// AttestationResultMessage on success, or the *websocket.CloseError on
// terminal close. Exactly one of the two return values is non-nil.
//
// This mirrors what client/client.go:connectAndAuthenticate does end-to-end:
//
//  1. send stage0 token
//  2. await ChallengeHandshake frame
//  3. send stage1 token (with the challenge nonce)
//  4. await handshake_result frame OR a close frame
func driveHandshake(t *testing.T, wsURL string, stage0Claims func() auth.Claims, stage1ClaimsBuilder func(nonce string) auth.Claims) (*protocol.AttestationResultMessage, *websocket.CloseError) {
	t.Helper()

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()

	// Stage 0.
	stage0Token := marshalClaims(t, stage0Claims())
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(stage0Token)))

	// Read challenge OR close. Close at stage0 is a terminal stage0 path —
	// the relay rejected the claims before sending the challenge.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	msgType, payload, err := ws.ReadMessage()
	if err != nil {
		var ce *websocket.CloseError
		if cce, ok := err.(*websocket.CloseError); ok {
			ce = cce
		}
		return nil, ce
	}
	require.Equal(t, websocket.TextMessage, msgType)

	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))
	require.Equal(t, protocol.ChallengeHandshake, challenge.Type)
	require.NotEmpty(t, challenge.Nonce)

	// Stage 1.
	stage1Token := marshalClaims(t, stage1ClaimsBuilder(challenge.Nonce))
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(stage1Token)))

	// Await result frame OR close.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	msgType, payload, err = ws.ReadMessage()
	if err != nil {
		var ce *websocket.CloseError
		if cce, ok := err.(*websocket.CloseError); ok {
			ce = cce
		}
		return nil, ce
	}
	require.Equal(t, websocket.TextMessage, msgType)

	var result protocol.AttestationResultMessage
	require.NoError(t, json.Unmarshal(payload, &result))
	return &result, nil
}

// newE2EHub spins up a hub with handleBackendConnect served on httptest,
// returning the ws:// URL and a cleanup func.
func newE2EHub(t *testing.T, cfg *config.Config) (string, func()) {
	t.Helper()
	h := New(cfg, nil, jsonClaimsValidator{}, &http.Client{})

	ts := httptest.NewServer(http.HandlerFunc(h.handleBackendConnect))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return wsURL, ts.Close
}

// TestHandshakeE2E_PartialAccept_RegistersFilteredSetAndSendsResult is the
// load-bearing gitea-scenario test: backend claims [443, 2222]; relay's
// AllowedTCPPortClaims=[443]; session must survive with port 443 registered
// and port 2222 rejected via the success-path handshake_result frame.
func TestHandshakeE2E_PartialAccept_RegistersFilteredSetAndSendsResult(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	stage0Claims := func() auth.Claims {
		return auth.Claims{BackendClaims: protocol.BackendClaims{
			Hostnames: []string{"example.com"},
			TCPPorts:  []int{443, 2222},
			Weight:    1,
		}}
	}
	stage1Builder := func(nonce string) auth.Claims {
		c := stage0Claims()
		c.SessionNonce = nonce
		return c
	}

	result, closeErr := driveHandshake(t, wsURL, stage0Claims, stage1Builder)
	require.Nil(t, closeErr, "expected success path, got close: %+v", closeErr)
	require.NotNil(t, result)

	require.Equal(t, protocol.AttestationResultHandshake, result.Type)
	require.Equal(t, []int{443}, result.Accepted.TCPPorts, "only 443 should be accepted")
	require.Equal(t, []string{"example.com"}, result.Accepted.Hostnames)

	require.Len(t, result.Rejected, 1)
	require.Equal(t, protocol.RejectedKindTCPPort, result.Rejected[0].Kind)
	require.Equal(t, protocol.RejectedCodeTCPPortNotAllowed, result.Rejected[0].Code)
	require.Equal(t, "2222", result.Rejected[0].Value)
	require.False(t, result.Truncated)
}

// TestHandshakeE2E_EmptyAfterFilter_EncodesCodesInCloseReason verifies the
// terminal stage0 path: backend claims ONLY [2222], all rejected, no usable
// claims left → 1008 close with "policy:tcp_port_not_allowed:2222" reason.
// This is the corner case where the device's catalog dropped only disallowed
// ports and the session correctly cannot proceed — but the device must learn
// WHY so it can recover.
func TestHandshakeE2E_EmptyAfterFilter_EncodesCodesInCloseReason(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	stage0Claims := func() auth.Claims {
		return auth.Claims{BackendClaims: protocol.BackendClaims{
			TCPPorts: []int{2222},
			Weight:   1,
		}}
	}
	stage1Builder := func(nonce string) auth.Claims { return stage0Claims() }

	result, closeErr := driveHandshake(t, wsURL, stage0Claims, stage1Builder)
	require.Nil(t, result)
	require.NotNil(t, closeErr)
	require.Equal(t, websocket.ClosePolicyViolation, closeErr.Code)
	require.Contains(t, closeErr.Text, "policy:")
	require.Contains(t, closeErr.Text, "tcp_port_not_allowed:2222")
}

// TestHandshakeE2E_InputMalformed_TerminalUsesPlainFallback verifies the
// codes-vs-fallback discrimination: a port out of range is an integrity
// failure (not policy drift), so the close-reason carries the prose error,
// NOT accumulated soft-rejects from earlier normalizers.
func TestHandshakeE2E_InputMalformed_TerminalUsesPlainFallback(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	stage0Claims := func() auth.Claims {
		return auth.Claims{BackendClaims: protocol.BackendClaims{
			Hostnames: []string{"example.com"},
			// 99999 is out of range → terminalInputMalformed.
			TCPPorts: []int{99999},
			Weight:   1,
		}}
	}
	stage1Builder := func(nonce string) auth.Claims { return stage0Claims() }

	result, closeErr := driveHandshake(t, wsURL, stage0Claims, stage1Builder)
	require.Nil(t, result)
	require.NotNil(t, closeErr)
	require.Equal(t, websocket.ClosePolicyViolation, closeErr.Code)
	// Codes-win-over-fallback rule MUST NOT apply here — input is malformed,
	// not policy-drift. The close-reason carries the raw error string.
	require.NotContains(t, closeErr.Text, "policy:")
	require.Contains(t, closeErr.Text, "invalid tcp port claim")
}

// TestHandshakeE2E_AdversarialClaimList_TruncatedAtBound stresses the 32/Kind
// cap on rejected entries: backend claims 50 disallowed ports; the resulting
// success-path frame caps at 32 with Truncated=true.
func TestHandshakeE2E_AdversarialClaimList_TruncatedAtBound(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	claimedPorts := []int{443}
	for p := 1000; p < 1050; p++ {
		claimedPorts = append(claimedPorts, p)
	}

	stage0Claims := func() auth.Claims {
		return auth.Claims{BackendClaims: protocol.BackendClaims{
			Hostnames: []string{"example.com"},
			TCPPorts:  claimedPorts,
			Weight:    1,
		}}
	}
	stage1Builder := func(nonce string) auth.Claims {
		c := stage0Claims()
		c.SessionNonce = nonce
		return c
	}

	result, closeErr := driveHandshake(t, wsURL, stage0Claims, stage1Builder)
	require.Nil(t, closeErr)
	require.NotNil(t, result)
	require.Len(t, result.Rejected, protocol.RejectedListBound)
	require.True(t, result.Truncated)
	require.Equal(t, []int{443}, result.Accepted.TCPPorts)
}

// TestHandshakeE2E_OutboundExplicitButAllSoftRejected_BlocksOutbound is the
// regression guard for the codex-found P1: a backend that claims
// outbound_allowed=true with an explicit allowed_outbound_ports list whose
// every entry gets soft-rejected (server allowlist disagrees) MUST NOT be
// able to dial any port server-side. Without the OutboundPortsExplicit bit,
// empty-after-filter would be indistinguishable from "no restriction" and
// the device would gain privilege it explicitly tried to renounce.
func TestHandshakeE2E_OutboundExplicitButAllSoftRejected_BlocksOutbound(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
		AllowOutbound:        true,
		AllowedOutboundPorts: []int{443}, // server allows 443
	}
	h := New(cfg, nil, jsonClaimsValidator{}, &http.Client{})
	ts := httptest.NewServer(http.HandlerFunc(h.handleBackendConnect))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()

	stage0 := auth.Claims{BackendClaims: protocol.BackendClaims{
		Hostnames:            []string{"example.com"},
		TCPPorts:             []int{443},
		Weight:               1,
		OutboundAllowed:      true,
		AllowedOutboundPorts: []int{25}, // device requests [25]; server allows [443] → all rejected
	}}
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage0))))

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, payload, err := ws.ReadMessage()
	require.NoError(t, err)
	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))

	stage1 := stage0
	stage1.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage1))))

	// Result frame arrives — session survived with [25] in rejected.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, payload, err = ws.ReadMessage()
	require.NoError(t, err)
	var result protocol.AttestationResultMessage
	require.NoError(t, json.Unmarshal(payload, &result))
	require.Empty(t, result.Accepted.AllowedOutboundPorts, "all explicit outbound ports were soft-rejected")
	require.True(t, result.Accepted.OutboundAllowed, "outbound enable bit still set; restriction is via the explicit list")
	require.True(t, result.Accepted.OutboundPortsExplicit,
		"explicit-bit must be true on the wire so consumers can distinguish 'all rejected' from 'unspecified'")

	// Now query the backend's outbound check via SelectBackend → b.outboundPortsExplicit:
	// the only way to verify the runtime check from a test is to inspect the
	// backend instance. Pool: tcp:443 routes to this backend.
	be, err := h.SelectBackend(protocol.RouteKey(protocol.TransportTCP, 443))
	require.NoError(t, err)
	b, ok := be.(*Backend)
	require.True(t, ok, "expected *Backend")
	require.True(t, b.outboundPortsExplicit, "OutboundPortsExplicit must be true even with empty filtered set")
	require.Empty(t, b.allowedOutboundPorts, "the filtered set is empty")

	// Simulate the outbound check predicate: with explicit=true and empty list,
	// NO port should be allowed via the backend-level check.
	require.False(t, portAllowed(b.allowedOutboundPorts, 443),
		"backend-level check must deny 443 because the explicit list filtered to empty")
}

// TestHandshakeE2E_OutboundExplicitFlipBetweenStages_RejectsHandshake is the
// regression guard for the second codex P1: a stage0 token with an explicit
// allowed_outbound_ports list that filters to empty, paired with a stage1
// token that OMITS the list entirely. Without the explicit-bit cross-check,
// stage0 captures explicit=true (filters to empty), stage1 captures
// explicit=false (omitted), filtered sets match (both empty), and the
// runtime would otherwise see explicit=false → unrestricted. Must terminate.
func TestHandshakeE2E_OutboundExplicitFlipBetweenStages_RejectsHandshake(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
		AllowOutbound:        true,
		AllowedOutboundPorts: []int{443},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()

	stage0 := auth.Claims{BackendClaims: protocol.BackendClaims{
		Hostnames:            []string{"example.com"},
		TCPPorts:             []int{443},
		Weight:               1,
		OutboundAllowed:      true,
		AllowedOutboundPorts: []int{25}, // explicit, all soft-rejected
	}}
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage0))))

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, payload, err := ws.ReadMessage()
	require.NoError(t, err)
	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))

	// Stage 1 OMITS the explicit list — attempting to flip explicit=true→false
	// while keeping the filtered set the same (both empty).
	stage1 := stage0
	stage1.AllowedOutboundPorts = nil
	stage1.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage1))))

	// Expect 1008 close — cross-check must catch the flip.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err = ws.ReadMessage()
	require.Error(t, err)
	ce, ok := err.(*websocket.CloseError)
	require.True(t, ok, "expected *websocket.CloseError")
	require.Equal(t, websocket.ClosePolicyViolation, ce.Code)
	require.Contains(t, ce.Text, "outbound_ports_explicit")
}

// TestHandshakeE2E_AllAllowed_NoRejectedEntries verifies the success path
// produces an empty rejected list when all claims are allowed.
func TestHandshakeE2E_AllAllowed_NoRejectedEntries(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443, 80},
		RelayPorts:           []int{443, 80},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	stage0Claims := func() auth.Claims {
		return auth.Claims{BackendClaims: protocol.BackendClaims{
			Hostnames: []string{"example.com"},
			TCPPorts:  []int{443, 80},
			Weight:    1,
		}}
	}
	stage1Builder := func(nonce string) auth.Claims {
		c := stage0Claims()
		c.SessionNonce = nonce
		return c
	}

	result, closeErr := driveHandshake(t, wsURL, stage0Claims, stage1Builder)
	require.Nil(t, closeErr)
	require.NotNil(t, result)
	require.Empty(t, result.Rejected)
	require.False(t, result.Truncated)
	require.Equal(t, []int{80, 443}, result.Accepted.TCPPorts)
}

// TestHandshakeE2E_RegisteredSetMatchesAcceptedOnly verifies the routing
// invariant: only accepted ports register into the hub's pools. The test
// queries SelectBackend for both the accepted and the rejected port and
// expects the accepted one to resolve and the rejected one to fail.
//
// This test inlines the handshake protocol (instead of using driveHandshake)
// so we can keep the websocket open during pool inspection — driveHandshake's
// deferred ws.Close would tear down the registration before SelectBackend runs.
func TestHandshakeE2E_RegisteredSetMatchesAcceptedOnly(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
	}
	h := New(cfg, nil, jsonClaimsValidator{}, &http.Client{})

	ts := httptest.NewServer(http.HandlerFunc(h.handleBackendConnect))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()

	stage0 := auth.Claims{BackendClaims: protocol.BackendClaims{
		Hostnames: []string{"example.com"},
		TCPPorts:  []int{443, 2222},
		Weight:    1,
	}}
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage0))))

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, payload, err := ws.ReadMessage()
	require.NoError(t, err)
	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))

	stage1 := stage0
	stage1.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage1))))

	// Read result frame to ensure register() has completed.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err = ws.ReadMessage()
	require.NoError(t, err)

	// Now inspect pools with the connection still open.
	_, err = h.SelectBackend(protocol.RouteKey(protocol.TransportTCP, 443))
	require.NoError(t, err, "port 443 should be in pools")

	_, err = h.SelectBackend(protocol.RouteKey(protocol.TransportTCP, 2222))
	require.Error(t, err, "port 2222 was rejected and must not be in pools")

	_, err = h.SelectBackend("example.com")
	require.NoError(t, err, "hostname should be in pools")
}

// completeHandshake drives the stage0/stage1 exchange and reads the
// handshake_result frame, leaving the websocket open for subsequent reauth
// drives. Returns the open ws (caller must close) and the result frame.
func completeHandshake(t *testing.T, wsURL string, stage1Claims auth.Claims) (*websocket.Conn, *protocol.AttestationResultMessage) {
	t.Helper()

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	// Stage 0 = stage 1 claims minus the session nonce.
	stage0 := stage1Claims
	stage0.SessionNonce = ""
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage0))))

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, payload, err := ws.ReadMessage()
	require.NoError(t, err)
	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))
	require.Equal(t, protocol.ChallengeHandshake, challenge.Type)

	stage1 := stage1Claims
	stage1.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, stage1))))

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, payload, err = ws.ReadMessage()
	require.NoError(t, err)

	var result protocol.AttestationResultMessage
	require.NoError(t, json.Unmarshal(payload, &result))
	require.Equal(t, protocol.AttestationResultHandshake, result.Type)
	return ws, &result
}

// TestReauthE2E_PartialAccept_SendsResultFrame drives the full reauth loop end
// to end: handshake with reauth_interval_seconds=1, wait for the reauth
// challenge from the relay, respond with the same claims (partial-accept set),
// and verify the relay emits a reauth_result frame on outgoingControl carrying
// the same rejected disposition.
//
// This is the test that catches regressions in:
//   - reauthLoop firing
//   - performReauth → applyClaims → sendReauthResult path
//   - reauth-result text frame routing through outgoingControl (under connWriteMu)
//   - the client side of the reauth-challenge / reauth-token exchange
func TestReauthE2E_PartialAccept_SendsResultFrame(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443},
		RelayPorts:           []int{443},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	claims := auth.Claims{BackendClaims: protocol.BackendClaims{
		Hostnames:             []string{"example.com"},
		TCPPorts:              []int{443, 2222}, // 2222 is soft-rejected
		Weight:                1,
		ReauthIntervalSeconds: ptrInt(1),
		ReauthGraceSeconds:    ptrInt(3),
	}}

	ws, result := completeHandshake(t, wsURL, claims)
	defer ws.Close()

	// Handshake-result has the 2222 rejection.
	require.Equal(t, []int{443}, result.Accepted.TCPPorts)
	require.Len(t, result.Rejected, 1)

	// Wait for reauth challenge — fires after reauthInterval (~1s with jitter).
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	msgType, payload, err := ws.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, msgType)

	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))
	require.Equal(t, protocol.ChallengeReauth, challenge.Type)
	require.NotEmpty(t, challenge.Nonce)

	// Send reauth token with the same claim set + the new nonce.
	reauthClaims := claims
	reauthClaims.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, reauthClaims))))

	// Expect reauth_result frame.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, payload, err = ws.ReadMessage()
	require.NoError(t, err)

	var reauthResult protocol.AttestationResultMessage
	require.NoError(t, json.Unmarshal(payload, &reauthResult))
	require.Equal(t, protocol.AttestationResultReauth, reauthResult.Type)
	require.Equal(t, []int{443}, reauthResult.Accepted.TCPPorts)
	require.Len(t, reauthResult.Rejected, 1)
	require.Equal(t, protocol.RejectedCodeTCPPortNotAllowed, reauthResult.Rejected[0].Code)
	require.Equal(t, "2222", reauthResult.Rejected[0].Value)
}

// TestReauthE2E_SetDrift_ProducesPolicyCloseFrame drives the reauth-terminal
// path: handshake with [443, 80], then send a reauth token with only [443].
// The filtered reauth set [443] differs from the registered set [443, 80] →
// terminalPolicyShaped. The relay must write a 1008 close-frame via
// sendPolicyClose (acquiring connWriteMu to serialize with writePump) before
// closing the connection.
//
// This is the test that catches the iter-3 race-blocker: bypass sendPolicyClose
// (e.g., bare b.Close in reauthLoop) and the device sees CloseAbnormalClosure
// instead of 1008 — operator-debug fidelity regresses.
func TestReauthE2E_SetDrift_ProducesPolicyCloseFrame(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443, 80},
		RelayPorts:           []int{443, 80},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	handshakeClaims := auth.Claims{BackendClaims: protocol.BackendClaims{
		Hostnames:             []string{"example.com"},
		TCPPorts:              []int{443, 80},
		Weight:                1,
		ReauthIntervalSeconds: ptrInt(1),
		ReauthGraceSeconds:    ptrInt(3),
	}}

	ws, _ := completeHandshake(t, wsURL, handshakeClaims)
	defer ws.Close()

	// Wait for reauth challenge.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, payload, err := ws.ReadMessage()
	require.NoError(t, err)

	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))
	require.Equal(t, protocol.ChallengeReauth, challenge.Type)

	// Send reauth token with a SHRUNK claim set [443] — drift from registered [443, 80].
	driftClaims := handshakeClaims
	driftClaims.TCPPorts = []int{443}
	driftClaims.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, driftClaims))))

	// Expect a 1008 close frame.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, _, err = ws.ReadMessage()
	require.Error(t, err)

	ce, ok := err.(*websocket.CloseError)
	require.True(t, ok, "expected *websocket.CloseError, got %T: %v", err, err)
	require.Equal(t, websocket.ClosePolicyViolation, ce.Code,
		"reauth set-drift must send 1008 (not CloseAbnormalClosure) so device learns the cause")
	// Rejected slice was empty (both [443] and [443, 80] are allowed; nothing
	// was soft-rejected), so the reason falls back to prose.
	require.Contains(t, ce.Text, "tcp port claims in token differ from registered set")
}

// TestReauthE2E_SetDriftWithRejectedClaims_CodesInCloseReason exercises the
// codes path on a terminal-policy-shaped reauth: the reauth token carries
// claims that produce a non-empty rejected list AND that disagree with the
// registered set. The relay must encode the rejected codes into the
// close-reason per encodeRejectedReason(terminalPolicyShaped, ...).
func TestReauthE2E_SetDriftWithRejectedClaims_CodesInCloseReason(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		BackendsJWTSecret:    "secret",
		IdleTimeoutSeconds:   30,
		AllowedTCPPortClaims: []int{443, 80},
		RelayPorts:           []int{443, 80},
	}
	wsURL, cleanup := newE2EHub(t, cfg)
	defer cleanup()

	// Handshake: register [443, 80]. 2222 is disallowed but not yet claimed.
	handshakeClaims := auth.Claims{BackendClaims: protocol.BackendClaims{
		Hostnames:             []string{"example.com"},
		TCPPorts:              []int{443, 80},
		Weight:                1,
		ReauthIntervalSeconds: ptrInt(1),
		ReauthGraceSeconds:    ptrInt(3),
	}}

	ws, _ := completeHandshake(t, wsURL, handshakeClaims)
	defer ws.Close()

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, payload, err := ws.ReadMessage()
	require.NoError(t, err)

	var challenge protocol.ChallengeMessage
	require.NoError(t, json.Unmarshal(payload, &challenge))

	// Reauth claims: [80, 2222] — 2222 soft-rejected; filtered [80] differs
	// from registered [443, 80] → terminal policy-shaped with non-empty rejected.
	driftClaims := handshakeClaims
	driftClaims.TCPPorts = []int{80, 2222}
	driftClaims.SessionNonce = challenge.Nonce
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(marshalClaims(t, driftClaims))))

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, _, err = ws.ReadMessage()
	require.Error(t, err)

	ce, ok := err.(*websocket.CloseError)
	require.True(t, ok)
	require.Equal(t, websocket.ClosePolicyViolation, ce.Code)
	require.Contains(t, ce.Text, "policy:")
	require.Contains(t, ce.Text, "tcp_port_not_allowed:2222")
}
