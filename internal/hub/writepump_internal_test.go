package hub

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// internalPipeTCPConn wraps a net.Pipe end with synthetic *net.TCPAddr
// addresses so AddClient's LocalAddr type assertion succeeds.
type internalPipeTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (p *internalPipeTCPConn) LocalAddr() net.Addr  { return p.local }
func (p *internalPipeTCPConn) RemoteAddr() net.Addr { return p.remote }

// newSlowPipeConn returns a synchronous pipe pair where the local side
// presents *net.TCPAddr addresses. The remote end is returned for the
// caller to close at test cleanup; nothing reads from it, so any Conn.Write
// on the local end blocks indefinitely — perfect for forcing writeCh
// overflow in bufferedConn.
func newSlowPipeConn() (local, remote net.Conn) {
	s, c := net.Pipe()
	local = &internalPipeTCPConn{
		Conn:   s,
		local:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443},
		remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 50000},
	}
	remote = c
	return
}

func mustNewUUID(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.New()
}

// newInternalTestBackend creates a Backend for internal-package tests, with
// the WS handshake already established. Returns the Backend, the client-side
// WS, and a function that starts the pumps. Caller is responsible for
// invoking startPumps when ready and calling the returned cleanup.
func newInternalTestBackend(t *testing.T) (b *Backend, clientWS *websocket.Conn, startPumps func() func()) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		serverConnCh <- conn
	}))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	backendWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 10,
	}
	meta := &AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}

	// validator is nil — these tests do not exercise auth paths.
	b = NewBackend(backendWS, meta, cfg, nil, &http.Client{})

	startPumps = func() func() {
		var pumpWg sync.WaitGroup
		pumpWg.Add(1)
		go func() {
			defer pumpWg.Done()
			b.StartPumps()
		}()
		return func() {
			b.Close()
			pumpWg.Wait()
			clientWS.Close()
			ts.Close()
		}
	}
	return
}

// TestWritePump_DispatchesFrameTypeFromOutbound is the internal regression
// test for the RF-3 finding: writePump must use the per-message
// outbound.messageType, not a hardcoded BinaryMessage. A careless rewrite
// would silently break performReauth (which uses TextMessage frames).
//
// We exercise this by injecting a synthetic outboundMessage{messageType:
// TextMessage} directly onto outgoingControl, then asserting that the
// receiving WebSocket peer sees a TextMessage frame with the expected payload.
func TestWritePump_DispatchesFrameTypeFromOutbound(t *testing.T) {
	t.Parallel()

	b, clientWS, startPumps := newInternalTestBackend(t)

	// Enqueue a TextMessage directly. This bypasses SendControlMessage's
	// JSON+ControlByteControl wrapping — we want a raw TextMessage frame
	// to verify the messageType field is preserved end-to-end.
	const marker = "reauth-challenge-marker-text-frame"
	b.outgoingControl <- outboundMessage{
		messageType: websocket.TextMessage,
		data:        []byte(marker),
	}

	cleanup := startPumps()
	defer cleanup()

	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		msgType, msg, err := clientWS.ReadMessage()
		require.NoError(t, err, "timed out waiting for TextMessage frame")
		if msgType == websocket.TextMessage {
			require.Equal(t, marker, string(msg),
				"writePump must preserve TextMessage payload verbatim")
			return
		}
		// Otherwise it's a BinaryMessage from some other source (none expected
		// since we registered no clients) — keep reading.
	}
}

// TestBufferedConn_OverflowTriggersRemoveClient verifies that the drive-by
// fix at handleBinaryMessage's data error path actually goes through
// RemoveClient (not bare Close) so all per-client state — clients map entry,
// forwardCredits, EventDisconnect notification — is cleaned up. Without
// this, an overflow leaks state and leaves the backend dispatching to a
// closed conn until the backend itself disconnects.
func TestBufferedConn_OverflowTriggersRemoveClient(t *testing.T) {
	t.Parallel()

	b, clientWS, startPumps := newInternalTestBackend(t)
	cleanup := startPumps()
	defer cleanup()

	// Register a client with a synchronous (net.Pipe) conn whose remote
	// side is never read. The drain goroutine will block on the very first
	// Conn.Write, so writeCh fills as the test pushes data frames in.
	slow, slowRemote := newSlowPipeConn()
	defer slowRemote.Close()
	clientID := mustNewUUID(t)
	require.NoError(t, b.AddClient(slow, clientID, "example.com", false))

	// Send enough data frames to overflow writeCh. The handleBinaryMessage
	// error branch should fire and call RemoveClient, which sends EventDisconnect
	// back via outgoingControl.
	dataFrame := make([]byte, 0, 1+16+16)
	dataFrame = append(dataFrame, 0x01) // ControlByteData
	dataFrame = append(dataFrame, clientID[:]...)
	dataFrame = append(dataFrame, []byte("payload")...)
	for i := 0; i < 2*clientWriteBufferSize+10; i++ {
		_ = clientWS.WriteMessage(websocket.BinaryMessage, dataFrame)
	}

	// Read frames serially (not from a goroutine) to avoid gorilla's
	// repeated-read-on-failed-connection panic.
	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	sawDisconnect := false
	for i := 0; i < 50; i++ {
		_, msg, err := clientWS.ReadMessage()
		if err != nil {
			break // read error — stop polling to avoid gorilla panic
		}
		if len(msg) < 2 {
			continue
		}
		if strings.Contains(string(msg), "disconnect") {
			sawDisconnect = true
			break
		}
	}
	require.True(t, sawDisconnect, "EventDisconnect not received — RemoveClient was not called on writeCh overflow")

	// Sanity: the client should not still be in b.clients.
	_, stillPresent := b.clients.Load(clientID)
	require.False(t, stillPresent, "client should have been removed from b.clients on writeCh overflow")
}

// TestWritePump_ReauthSucceedsUnderLoad is the regression test for RF-3.
// It simulates the conditions under which the original bug would manifest:
// the data lane is saturated with backlog, then a reauth-equivalent
// TextMessage is enqueued onto outgoingControl.
//
// The original bug: with a single shared `outgoing` channel (cap 256), a
// data backlog could fill the channel and cause the next reauth challenge
// (also routed via the same channel) to be dropped — backend would never
// receive the challenge and would fail reauth, cascading to a backend close.
//
// The fix is the channel split: outgoingControl is a separate buffer from
// outgoingData, so the reauth challenge can always be enqueued. writePump
// uses fair selection (NOT strict priority — strict priority would starve
// data and pings), so the reauth marker is delivered within bounded time
// regardless of data backlog.
//
// We assert: the reauth marker IS delivered within the 3-second window,
// alongside the data backlog. We do NOT assert strict ordering.
func TestWritePump_ReauthSucceedsUnderLoad(t *testing.T) {
	t.Parallel()

	b, clientWS, startPumps := newInternalTestBackend(t)

	// Saturate the data lane with 256 messages (cap is 256).
	dummyData := make([]byte, 256)
	for i := 0; i < 256; i++ {
		select {
		case b.outgoingData <- outboundMessage{messageType: websocket.BinaryMessage, data: dummyData}:
		default:
			t.Fatalf("data lane filled before reaching cap; only enqueued %d", i)
		}
	}

	// Now enqueue the reauth-equivalent TextMessage onto the control lane.
	const reauthMarker = "REAUTH-CHALLENGE-PAYLOAD"
	select {
	case b.outgoingControl <- outboundMessage{
		messageType: websocket.TextMessage,
		data:        []byte(reauthMarker),
	}:
	default:
		t.Fatal("control lane unexpectedly full")
	}

	cleanup := startPumps()
	defer cleanup()

	// Read frames until we see the TextMessage marker. With fair select,
	// the reauth marker is interleaved with data — we don't care about the
	// position, only that it arrives within the deadline (i.e., it wasn't
	// dropped).
	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		msgType, msg, err := clientWS.ReadMessage()
		require.NoError(t, err, "timed out waiting for reauth TextMessage — channel split is broken")
		if msgType == websocket.TextMessage {
			require.Equal(t, reauthMarker, string(msg),
				"writePump must preserve TextMessage payload verbatim")
			return
		}
	}
}
