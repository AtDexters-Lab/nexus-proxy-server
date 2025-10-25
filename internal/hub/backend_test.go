package hub_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestBackendStartPumpsTerminatesWhenConnCloses(t *testing.T) {
	t.Parallel()

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
	cfg := &config.Config{}
	b := hub.NewBackend(backendConn, []string{"example.com"}, 1, cfg)

	done := make(chan struct{})
	go func() {
		b.StartPumps()
		close(done)
	}()

	// Queue a control message so writePump attempts a write against the closed conn.
	err = b.SendControlMessage(protocol.ControlMessage{Event: protocol.EventPingClient})
	require.NoError(t, err)

	// Close the client side to force writePump/readPump errors.
	require.NoError(t, clientConn.Close())

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("backend pumps did not terminate after connection close")
	}
}
