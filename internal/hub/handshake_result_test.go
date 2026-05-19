package hub

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type stubVal struct{}

func (stubVal) Validate(ctx context.Context, token string) (*auth.Claims, error) {
	return (&auth.Claims{BackendClaims: protocol.BackendClaims{Hostnames: []string{"example.com"}}}).Copy(), nil
}

func TestEncodeRejectedReason_PolicyShaped_EmitsCodes(t *testing.T) {
	t.Parallel()
	rejected := []protocol.RejectedClaim{
		{Kind: protocol.RejectedKindTCPPort, Code: protocol.RejectedCodeTCPPortNotAllowed, Value: "2222"},
		{Kind: protocol.RejectedKindTCPPort, Code: protocol.RejectedCodeTCPPortNotAllowed, Value: "8080"},
	}
	reason := encodeRejectedReason(terminalPolicyShaped, rejected, "should be ignored")
	require.True(t, strings.HasPrefix(reason, "policy:"))
	require.Contains(t, reason, "tcp_port_not_allowed:2222")
	require.Contains(t, reason, "tcp_port_not_allowed:8080")
	require.NotContains(t, reason, "should be ignored", "codes-win-over-fallback when policy-shaped")
}

func TestEncodeRejectedReason_InputMalformed_EmitsFallback(t *testing.T) {
	t.Parallel()
	// Even when rejected list is non-empty, input-malformed terminal must emit
	// fallback prose — the rejected list is not the cause.
	rejected := []protocol.RejectedClaim{
		{Kind: protocol.RejectedKindHostname, Code: protocol.RejectedCodeHostnameReservedRouteKey, Value: "tcp:53"},
	}
	reason := encodeRejectedReason(terminalInputMalformed, rejected, "invalid tcp port claim: 99999")
	require.Equal(t, "invalid tcp port claim: 99999", reason)
	require.NotContains(t, reason, "policy:")
}

func TestEncodeRejectedReason_EmptyRejected_FallsBackToProse(t *testing.T) {
	t.Parallel()
	reason := encodeRejectedReason(terminalPolicyShaped, nil, "stage0 token missing hostnames and port claims")
	require.Equal(t, "stage0 token missing hostnames and port claims", reason)
}

func TestEncodeRejectedReason_BudgetTruncation_CodeBoundary(t *testing.T) {
	t.Parallel()
	// Pack enough entries to exceed the 123-byte budget. Each entry is
	// "tcp_port_not_allowed:NNNN" (~24 bytes) plus the leading "policy:" (7) and
	// inter-entry commas.
	rejected := make([]protocol.RejectedClaim, 32)
	for i := range rejected {
		rejected[i] = protocol.RejectedClaim{
			Kind:  protocol.RejectedKindTCPPort,
			Code:  protocol.RejectedCodeTCPPortNotAllowed,
			Value: "1000",
		}
	}
	reason := encodeRejectedReason(terminalPolicyShaped, rejected, "")
	require.LessOrEqual(t, len(reason), closeReasonByteBudget, "reason exceeds 123-byte budget")
	require.True(t, strings.HasSuffix(reason, ",..."), "truncated reason should end with ,...")
}

func TestEncodeRejectedReason_NonASCIIValue_Elided(t *testing.T) {
	t.Parallel()
	rejected := []protocol.RejectedClaim{
		{Kind: protocol.RejectedKindHostname, Code: protocol.RejectedCodeHostnameReservedRouteKey, Value: "пример"},
	}
	reason := encodeRejectedReason(terminalPolicyShaped, rejected, "")
	require.Contains(t, reason, "<elided>")
	require.NotContains(t, reason, "пример")
}

func TestEncodeRejectedReason_ValueWithSeparator_Elided(t *testing.T) {
	t.Parallel()
	// A value containing ',' or ':' must be elided to keep the parser safe.
	rejected := []protocol.RejectedClaim{
		{Kind: "future", Code: "future_code", Value: "https://example.com:8443/x"},
	}
	reason := encodeRejectedReason(terminalPolicyShaped, rejected, "")
	require.Equal(t, "policy:future_code:<elided>", reason)
}

func TestEncodeRejectedReason_FallbackOversize_ByteTruncated(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	reason := encodeRejectedReason(terminalInputMalformed, nil, long)
	require.Equal(t, closeReasonByteBudget, len(reason))
}

// TestLogRejectedCodes_EscapesAttackerControlledValue is the regression guard
// for the log-injection vulnerability surfaced by the security review (F1):
// a backend submitting a hostname with embedded control bytes (newlines,
// ANSI escapes, etc.) flows into RejectedClaim.Value and was previously
// printed via %v over a []string, surviving control bytes verbatim. The
// fix uses %q formatting on the Value half — verify no raw control byte
// makes it into the log output.
func TestLogRejectedCodes_EscapesAttackerControlledValue(t *testing.T) {
	// Not t.Parallel — mutates package-global log output destination.
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})

	hostile := "tcp:53\nINFO: forged log line\x1b[31m"
	rejected := []protocol.RejectedClaim{
		{Kind: protocol.RejectedKindHostname, Code: protocol.RejectedCodeHostnameReservedRouteKey, Value: hostile},
	}
	logRejectedCodes("test-backend", rejected, false)

	output := buf.String()
	require.NotContains(t, output, "\nINFO: forged log line", "raw newline survived — log injection vector still open")
	require.NotContains(t, output, "\x1b[31m", "raw ANSI escape survived")
	// %q escapes newlines as \n; verify the escaped form is present.
	require.Contains(t, output, `\nINFO: forged log line`)
	require.Contains(t, output, "hostname_reserved_route_key")
}

// TestSendPolicyClose_NoWriteRaceWithWritePump exercises concurrent writes
// between sendPolicyClose (called from the reauth-loop goroutine in production)
// and writePump's data/ping writes. The connWriteMu mutex must serialize these
// or gorilla/websocket framing corrupts on the wire.
//
// Run with `-race` for the actual concurrency guarantee.
func TestSendPolicyClose_NoWriteRaceWithWritePump(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	serverConnCh := make(chan *websocket.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		serverConnCh <- conn
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer clientWS.Close()
	hubSideWS := <-serverConnCh

	cfg := &config.Config{IdleTimeoutSeconds: 30}
	meta := &AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := NewBackend(hubSideWS, meta, cfg, stubVal{}, &http.Client{})

	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()

	// Reader pump on client side to drain frames so writePump doesn't block.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		clientWS.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := clientWS.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Hammer outgoingControl from another goroutine while sendPolicyClose runs.
	// SendControlMessage routes through outgoingControl with the mutex acquired
	// at the actual write site in writePump.
	hammerDone := make(chan struct{})
	go func() {
		defer close(hammerDone)
		for i := 0; i < 100; i++ {
			_ = b.SendControlMessage(protocol.ControlMessage{Event: protocol.EventPongClient})
			time.Sleep(time.Microsecond)
		}
	}()

	// Brief warmup so writePump has work to do.
	time.Sleep(5 * time.Millisecond)

	// Race the policy-close against the hammer. The connWriteMu should serialize.
	rejected := []protocol.RejectedClaim{
		{Kind: protocol.RejectedKindTCPPort, Code: protocol.RejectedCodeTCPPortNotAllowed, Value: "2222"},
	}
	b.sendPolicyClose(terminalPolicyShaped, rejected, nil)

	<-hammerDone
	pumpWg.Wait()
	<-clientDone
}
