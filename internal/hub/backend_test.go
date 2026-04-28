package hub_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestBackendStartPumpsTerminatesWhenConnCloses(t *testing.T) {
	t.Parallel()

	// Inline scaffolding: this test observes pumps' natural exit when the WS
	// closes, which is incompatible with newTestBackend's startPumps helper
	// (whose returned cleanup forces pump shutdown via b.Close).
	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		serverConnCh <- conn
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer clientConn.Close()

	backendConn := <-serverConnCh
	cfg := &config.Config{BackendsJWTSecret: "secret"}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(backendConn, meta, cfg, stubValidator{}, &http.Client{})

	done := make(chan struct{})
	go func() {
		b.StartPumps()
		close(done)
	}()

	require.NoError(t, b.SendControlMessage(protocol.ControlMessage{Event: protocol.EventPingClient}))
	require.NoError(t, clientConn.Close())

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("backend pumps did not terminate after connection close")
	}
}

// SendData must return iface.ErrClientGone when the target client has already
// been removed from the per-backend map. This is the post-disconnect-race
// short-circuit that prevents user→backend forward bytes from being shipped
// during the user-side TCP grace window after EventDisconnect.
func TestSendDataReturnsErrClientGoneWhenClientUnknown(t *testing.T) {
	t.Parallel()

	b, _, cleanup := startBackend(t)
	defer cleanup()

	err := b.SendData(uuid.New(), []byte("payload"))
	require.Error(t, err)
	require.True(t, errors.Is(err, iface.ErrClientGone), "expected ErrClientGone, got %v", err)
}
