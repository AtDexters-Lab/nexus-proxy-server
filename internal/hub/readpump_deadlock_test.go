package hub_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// pipeTCPConn wraps a net.Conn (from net.Pipe) with fake TCP addresses so
// that backend.AddClient's LocalAddr type assertion to *net.TCPAddr succeeds.
// net.Pipe is synchronous: writes block until the peer reads, which is
// essential for reliably reproducing the readPump deadlock without depending
// on kernel TCP buffer sizes.
type pipeTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (p *pipeTCPConn) LocalAddr() net.Addr  { return p.local }
func (p *pipeTCPConn) RemoteAddr() net.Addr { return p.remote }

// pipeTCPPair creates a synchronous connected pair where each side reports
// *net.TCPAddr addresses. The "local" side (index 0) is for AddClient;
// the "remote" side (index 1) is the far end.
func pipeTCPPair(port int) (*pipeTCPConn, *pipeTCPConn) {
	s, c := net.Pipe()
	local := &pipeTCPConn{
		Conn:   s,
		local:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
		remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 50000},
	}
	remote := &pipeTCPConn{
		Conn:   c,
		local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 50000},
		remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
	}
	return local, remote
}

// TestReadPumpDeadlock_SelfLoop reproduces the deadlock that occurs when
// two client connections on the same backend create a circular dependency:
//
//   - Client B (e.g. HTTPS response) has a slow/blocked TCP write
//   - Client C (e.g. tunnel carrying ACK data for client B) is starved
//     because readPump is blocked on Client B's write
//
// This is the exact scenario from the Mowgli 256K self-loop stall:
// Connection A (WS tunnel) and Connection B (HTTPS) share one backend.
// When readPump blocks writing to Connection B, it can't forward tunnel
// ACK data to the tunnel client, creating a permanent deadlock.
//
// Without the fix, this test times out. With the fix, both clients
// receive their data independently.
func TestReadPumpDeadlock_SelfLoop(t *testing.T) {
	t.Parallel()

	// --- Set up WebSocket pair (simulates Nexus ↔ backend connection) ---
	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
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

	backendWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 10,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(backendWS, meta, cfg, stubValidator{}, &http.Client{})

	// Start backend pumps.
	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()
	defer func() {
		b.Close()
		pumpWg.Wait()
	}()

	// --- Create two client connections ---

	// Client B: "slow client" — uses net.Pipe (synchronous), and we never
	// read from slowRemote. The first Write to slowLocal will block forever.
	clientBID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowRemote.Close()
	defer slowLocal.Close()

	err = b.AddClient(slowLocal, clientBID, "example.com", false)
	require.NoError(t, err)

	// Client C: "fast client" — we actively drain reads so writes complete.
	clientCID := uuid.New()
	fastLocal, fastRemote := pipeTCPPair(443)
	defer fastRemote.Close()
	defer fastLocal.Close()

	err = b.AddClient(fastLocal, clientCID, "example.com", false)
	require.NoError(t, err)

	// Drain fastRemote so writes to fastLocal never block.
	fastReceived := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := fastRemote.Read(buf)
			if err != nil {
				close(fastReceived)
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			fastReceived <- data
		}
	}()

	// --- Send data via the WebSocket (simulates backend sending response) ---

	buildDataMsg := func(clientID uuid.UUID, payload []byte) []byte {
		msg := make([]byte, 0, 1+protocol.ClientIDLength+len(payload))
		msg = append(msg, protocol.ControlByteData)
		msg = append(msg, clientID[:]...)
		msg = append(msg, payload...)
		return msg
	}

	// Send ONE message for Client B. Since net.Pipe is synchronous,
	// the readPump's clientConn.Write() will block on the first write
	// (slowRemote is not reading).
	payload := []byte("block_me")
	err = clientWS.WriteMessage(websocket.BinaryMessage, buildDataMsg(clientBID, payload))
	require.NoError(t, err)

	// Give readPump a moment to pick up the message and block on the write.
	time.Sleep(50 * time.Millisecond)

	// Now send a message for Client C (the tunnel ACK). If readPump is
	// blocked on Client B's synchronous pipe write, this message will
	// never be processed.
	marker := []byte("TUNNEL_ACK_DATA")
	err = clientWS.WriteMessage(websocket.BinaryMessage, buildDataMsg(clientCID, marker))
	require.NoError(t, err)

	// --- Verify: Client C should receive its data within a reasonable time ---
	deadline := time.After(3 * time.Second)
	received := false
	for !received {
		select {
		case data, ok := <-fastReceived:
			if !ok {
				t.Fatal("fast client connection closed unexpectedly")
			}
			if string(data) == string(marker) {
				received = true
			}
		case <-deadline:
			t.Fatal("DEADLOCK REPRODUCED: Client C (tunnel) data was not delivered within 3s — " +
				"readPump is blocked on Client B's slow write")
		}
	}
}

// TestReadPumpBackpressure_LargePayload verifies that a large response
// (>64 messages) to a slow client does NOT cause the client to be
// disconnected. With proper backpressure, the proxy should signal the
// backend to pause (EventPauseStream) rather than overflowing the buffer.
//
// The test simulates the client-side behavior: a writer goroutine sends
// messages until EventPauseStream is received on the WebSocket, then stops.
// This mirrors what the real client library does.
func TestReadPumpBackpressure_LargePayload(t *testing.T) {
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
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer clientWS.Close()

	backendWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 10,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(backendWS, meta, cfg, stubValidator{}, &http.Client{})

	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()
	defer func() {
		b.Close()
		pumpWg.Wait()
	}()

	// Client B: "slow client" — net.Pipe is synchronous, we never read
	// from slowRemote. The drain goroutine blocks on the first write,
	// so the bufferedConn's channel fills up.
	clientBID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowRemote.Close()
	defer slowLocal.Close()

	err = b.AddClient(slowLocal, clientBID, "example.com", false)
	require.NoError(t, err)

	// Client C: "fast client" — actively drained.
	clientCID := uuid.New()
	fastLocal, fastRemote := pipeTCPPair(443)
	defer fastRemote.Close()
	defer fastLocal.Close()

	err = b.AddClient(fastLocal, clientCID, "example.com", false)
	require.NoError(t, err)

	fastReceived := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := fastRemote.Read(buf)
			if err != nil {
				close(fastReceived)
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			fastReceived <- data
		}
	}()

	buildDataMsg := func(clientID uuid.UUID, payload []byte) []byte {
		msg := make([]byte, 0, 1+protocol.ClientIDLength+len(payload))
		msg = append(msg, protocol.ControlByteData)
		msg = append(msg, clientID[:]...)
		msg = append(msg, payload...)
		return msg
	}

	// Reader goroutine: watches for EventPauseStream from Nexus.
	// This simulates the client library receiving the pause signal.
	pauseReceived := make(chan struct{}, 1)
	go func() {
		for {
			_, msg, err := clientWS.ReadMessage()
			if err != nil {
				return
			}
			if len(msg) < 2 || msg[0] != protocol.ControlByteControl {
				continue
			}
			var ctrl protocol.ControlMessage
			if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
				continue
			}
			if ctrl.Event == protocol.EventPauseStream && ctrl.ClientID == clientBID {
				select {
				case pauseReceived <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Send 55 messages for Client B — enough to trigger the pause at
	// high watermark (48) with headroom to spare. Total 55 < 128
	// (buffer size), so no overflow. The drain goroutine is stuck
	// (net.Pipe, slowRemote not reading), so all 55 stay in the channel.
	payload := []byte("response_chunk")
	for i := 0; i < 55; i++ {
		err = clientWS.WriteMessage(websocket.BinaryMessage, buildDataMsg(clientBID, payload))
		require.NoError(t, err)
	}

	// Wait for the EventPauseStream to arrive — this proves Nexus detected
	// the buffer filling and signaled the backend to stop.
	select {
	case <-pauseReceived:
		// Backpressure signal received — Nexus told us to stop.
	case <-time.After(3 * time.Second):
		t.Fatal("BACKPRESSURE MISSING: EventPauseStream was never received " +
			"(expected at high watermark ~48)")
	}

	// Now send a message for Client C (the tunnel ACK). readPump must not
	// be blocked — it should process this immediately.
	marker := []byte("TUNNEL_ACK_DATA")
	err = clientWS.WriteMessage(websocket.BinaryMessage, buildDataMsg(clientCID, marker))
	require.NoError(t, err)

	// Verify Client C receives its data (readPump not blocked).
	deadline := time.After(3 * time.Second)
	received := false
	for !received {
		select {
		case data, ok := <-fastReceived:
			if !ok {
				t.Fatal("fast client connection closed unexpectedly")
			}
			if string(data) == string(marker) {
				received = true
			}
		case <-deadline:
			t.Fatal("Client C (tunnel) data was not delivered within 3s")
		}
	}

	// Verify Client B was NOT disconnected.
	time.Sleep(100 * time.Millisecond)
	slowRemote.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, readErr := slowRemote.Read(make([]byte, 1))
	if readErr != nil {
		if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			// Timeout = pipe still open = Client B alive. Good.
		} else {
			t.Fatalf("BACKPRESSURE MISSING: Client B was disconnected after buffer overflow: %v", readErr)
		}
	}
}

// TestReadPumpDeadlock_ControlMessages verifies that control messages
// (like EventPingClient) are still processed while a client write is blocked.
func TestReadPumpDeadlock_ControlMessages(t *testing.T) {
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
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer clientWS.Close()

	backendWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 10,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(backendWS, meta, cfg, stubValidator{}, &http.Client{})

	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()
	defer func() {
		b.Close()
		pumpWg.Wait()
	}()

	// Block readPump with a slow client.
	clientBID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowRemote.Close()
	defer slowLocal.Close()

	err = b.AddClient(slowLocal, clientBID, "example.com", false)
	require.NoError(t, err)

	// Send one data message for the slow client to block readPump.
	dataMsg := make([]byte, 0, 1+protocol.ClientIDLength+8)
	dataMsg = append(dataMsg, protocol.ControlByteData)
	dataMsg = append(dataMsg, clientBID[:]...)
	dataMsg = append(dataMsg, []byte("block_me")...)
	err = clientWS.WriteMessage(websocket.BinaryMessage, dataMsg)
	require.NoError(t, err)

	// Give readPump time to block.
	time.Sleep(50 * time.Millisecond)

	// Register another client and send PingClient for it.
	anotherClientID := uuid.New()
	pingLocal, _ := pipeTCPPair(443)
	defer pingLocal.Close()
	err = b.AddClient(pingLocal, anotherClientID, "example.com", false)
	require.NoError(t, err)

	pingMsg := protocol.ControlMessage{
		Event:    protocol.EventPingClient,
		ClientID: anotherClientID,
	}
	pingPayload, err := json.Marshal(pingMsg)
	require.NoError(t, err)
	controlFrame := append([]byte{protocol.ControlByteControl}, pingPayload...)
	err = clientWS.WriteMessage(websocket.BinaryMessage, controlFrame)
	require.NoError(t, err)

	// Read responses. We need to skip EventConnect messages from AddClient calls.
	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, msg, err := clientWS.ReadMessage()
		if err != nil {
			t.Fatalf("DEADLOCK REPRODUCED: timed out waiting for PongClient — "+
				"readPump is blocked on Client B's slow write: %v", err)
		}
		if len(msg) < 2 || msg[0] != protocol.ControlByteControl {
			continue
		}
		var ctrl protocol.ControlMessage
		if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
			continue
		}
		if ctrl.Event == protocol.EventPongClient && ctrl.ClientID == anotherClientID {
			// Success — readPump processed the control message despite Client B blocking.
			return
		}
		// Otherwise it's an EventConnect or something else; keep reading.
	}
}
