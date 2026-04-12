package hub_test

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newTestBackend wires a backend WS pair and returns the hub-side Backend
// (without starting pumps), the client-side WS, and a cleanup function.
// Caller is responsible for calling startPumps() when ready.
func newTestBackend(t *testing.T) (*hub.Backend, *websocket.Conn, func() func()) {
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
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(backendWS, meta, cfg, stubValidator{}, &http.Client{})

	startPumps := func() func() {
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

	return b, clientWS, startPumps
}

// startBackend is the convenience wrapper that starts pumps immediately.
// Used by tests that don't need to enqueue messages before pumps run.
func startBackend(t *testing.T) (*hub.Backend, *websocket.Conn, func()) {
	t.Helper()
	b, clientWS, startPumps := newTestBackend(t)
	cleanup := startPumps()
	return b, clientWS, cleanup
}

// readUntilControl reads from clientWS, returning the first control message
// matching the predicate, skipping data and non-matching control frames.
// Fails the test if deadline elapses.
func readUntilControl(t *testing.T, clientWS *websocket.Conn, deadline time.Duration, match func(protocol.ControlMessage) bool) protocol.ControlMessage {
	t.Helper()
	clientWS.SetReadDeadline(time.Now().Add(deadline))
	for {
		_, msg, err := clientWS.ReadMessage()
		require.NoError(t, err)
		if len(msg) < 2 {
			continue
		}
		if msg[0] != protocol.ControlByteControl {
			continue
		}
		var ctrl protocol.ControlMessage
		if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
			continue
		}
		if match(ctrl) {
			return ctrl
		}
	}
}

// TestWritePump_ControlAndDataMixedDelivery verifies that when both lanes
// have messages, both kinds are delivered without one starving the other.
// Control gets fair (not strict-priority) treatment because the channel
// split alone solves the silent credit-loss bug; strict priority would
// starve data and ping frames, causing upload stalls and pongWait
// disconnects under sustained credit-replenishment load.
//
// The original strict-priority design was reverted after Codex flagged the
// starvation regression — see plan section "Commit 1" notes.
func TestWritePump_ControlAndDataMixedDelivery(t *testing.T) {
	t.Parallel()

	b, clientWS, startPumps := newTestBackend(t)

	clientID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowLocal.Close()
	defer slowRemote.Close()
	require.NoError(t, b.AddClient(slowLocal, clientID, "example.com", false))

	// Fill the data lane.
	payload := make([]byte, 64)
	for i := 0; i < 100; i++ {
		_ = b.SendData(clientID, payload)
	}

	// Add a control message after the data backlog.
	pingMsg := protocol.ControlMessage{
		Event:    protocol.EventPongClient,
		ClientID: clientID,
	}
	require.NoError(t, b.SendControlMessage(pingMsg))

	cleanup := startPumps()
	defer cleanup()

	// Both the data frames AND the pong must drain within a reasonable
	// window. We don't assert strict ordering — the fair select can
	// interleave them. We just verify both arrive.
	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	gotData := 0
	gotPong := false
	for !gotPong || gotData < 100 {
		_, msg, err := clientWS.ReadMessage()
		require.NoError(t, err)
		if len(msg) < 1 {
			continue
		}
		if msg[0] == protocol.ControlByteData {
			gotData++
			continue
		}
		if msg[0] == protocol.ControlByteControl {
			var ctrl protocol.ControlMessage
			if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
				continue
			}
			if ctrl.Event == protocol.EventPongClient && ctrl.ClientID == clientID {
				gotPong = true
			}
		}
	}
	require.Equal(t, 100, gotData, "all 100 data frames must be delivered")
	require.True(t, gotPong, "pong must be delivered")
}

// TestSendControlMessage_ConnectPrecedesFirstData is the regression test
// for the second Codex P1 finding: EventConnect must precede the first data
// frame on the same client, otherwise the backend's handleDataMessage sees
// an unknown clientID and tears down the new connection (drops the prelude
// or first UDP packet).
//
// AddClient sends EventConnect via SendControlMessage; proxy.Client.Start
// then immediately sends prelude bytes via SendData. After the channel
// split, EventConnect must route through outgoingData (same FIFO lane)
// rather than outgoingControl, otherwise the fair select can transmit
// data before the connect.
func TestSendControlMessage_ConnectPrecedesFirstData(t *testing.T) {
	t.Parallel()

	b, clientWS, startPumps := newTestBackend(t)

	clientID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowLocal.Close()
	defer slowRemote.Close()

	// AddClient enqueues EventConnect. proxy.Client.Start would normally
	// then call SendData immediately. Replicate that pattern here BEFORE
	// starting pumps, so both lanes are populated when writePump's first
	// select runs.
	require.NoError(t, b.AddClient(slowLocal, clientID, "example.com", false))
	payload := make([]byte, 64)
	for i := 0; i < 50; i++ {
		_ = b.SendData(clientID, payload)
	}

	cleanup := startPumps()
	defer cleanup()

	// First control frame on the wire MUST be EventConnect for this clientID,
	// not data.
	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, msg, err := clientWS.ReadMessage()
		require.NoError(t, err)
		if len(msg) < 1 {
			continue
		}
		if msg[0] == protocol.ControlByteData {
			t.Fatalf("data frame arrived before EventConnect — first-data ordering broken")
		}
		if msg[0] == protocol.ControlByteControl {
			var ctrl protocol.ControlMessage
			if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
				continue
			}
			if ctrl.Event == protocol.EventConnect && ctrl.ClientID == clientID {
				return // good — connect arrived first
			}
		}
	}
}

// TestSendControlMessage_DisconnectPreservesFIFO is the regression test
// for the Codex P1 finding: EventDisconnect must not overtake queued data
// on the same backend, otherwise the backend closes the local socket
// before the tail of the stream is delivered.
func TestSendControlMessage_DisconnectPreservesFIFO(t *testing.T) {
	t.Parallel()

	b, clientWS, startPumps := newTestBackend(t)

	clientID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowLocal.Close()
	defer slowRemote.Close()
	require.NoError(t, b.AddClient(slowLocal, clientID, "example.com", false))

	// Enqueue 50 data frames followed by an EventDisconnect — exactly the
	// pattern proxy.Client.Start uses on its defer path.
	payload := make([]byte, 64)
	for i := 0; i < 50; i++ {
		_ = b.SendData(clientID, payload)
	}
	disconnectMsg := protocol.ControlMessage{
		Event:    protocol.EventDisconnect,
		ClientID: clientID,
	}
	require.NoError(t, b.SendControlMessage(disconnectMsg))

	// Start pumps. We must observe ALL 50 data frames BEFORE the disconnect.
	cleanup := startPumps()
	defer cleanup()

	clientWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	dataCount := 0
	for {
		_, msg, err := clientWS.ReadMessage()
		require.NoError(t, err)
		if len(msg) < 1 {
			continue
		}
		if msg[0] == protocol.ControlByteData {
			dataCount++
			continue
		}
		if msg[0] == protocol.ControlByteControl {
			var ctrl protocol.ControlMessage
			if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
				continue
			}
			if ctrl.Event == protocol.EventDisconnect && ctrl.ClientID == clientID {
				require.Equal(t, 50, dataCount,
					"EventDisconnect overtook queued data (only %d/50 data frames before disconnect)", dataCount)
				return
			}
		}
	}
}

// TestWritePump_ControlNotStarvedByData asserts that under sustained data
// load, control messages still get through. We continuously feed the data
// lane and intermittently send control messages, then verify all control
// messages arrived within a reasonable window.
func TestWritePump_ControlNotStarvedByData(t *testing.T) {
	t.Parallel()

	b, clientWS, cleanup := startBackend(t)
	defer cleanup()

	clientID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowLocal.Close()
	defer slowRemote.Close()
	require.NoError(t, b.AddClient(slowLocal, clientID, "example.com", false))

	// Drain EventConnect.
	readUntilControl(t, clientWS, 2*time.Second, func(c protocol.ControlMessage) bool {
		return c.Event == protocol.EventConnect && c.ClientID == clientID
	})

	// Drain WS continuously, counting control messages.
	var controlReceived atomic.Int64
	stopRead := make(chan struct{})
	doneRead := make(chan struct{})
	go func() {
		defer close(doneRead)
		for {
			select {
			case <-stopRead:
				return
			default:
			}
			clientWS.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			_, msg, err := clientWS.ReadMessage()
			if err != nil {
				continue
			}
			if len(msg) < 2 || msg[0] != protocol.ControlByteControl {
				continue
			}
			var ctrl protocol.ControlMessage
			if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
				continue
			}
			if ctrl.Event == protocol.EventPongClient {
				controlReceived.Add(1)
			}
		}
	}()

	// Sustain data load + interleaved control messages.
	payload := make([]byte, 64)
	const targetControl = 50
	for i := 0; i < targetControl; i++ {
		// Burst data
		for j := 0; j < 10; j++ {
			_ = b.SendData(clientID, payload)
		}
		// One control
		_ = b.SendControlMessage(protocol.ControlMessage{
			Event:    protocol.EventPongClient,
			ClientID: clientID,
		})
	}

	// Wait for all control messages to be drained.
	require.Eventually(t, func() bool {
		return controlReceived.Load() >= targetControl
	}, 5*time.Second, 25*time.Millisecond,
		"only %d/%d control messages received under sustained data load",
		controlReceived.Load(), targetControl)

	close(stopRead)
	<-doneRead
}

// (TextMessage frame-type preservation is exercised by the internal-package
// tests TestWritePump_DispatchesFrameTypeFromOutbound and
// TestWritePump_ReauthSucceedsUnderLoad in writepump_internal_test.go, which
// can directly enqueue synthetic TextMessage outboundMessages onto the
// unexported outgoingControl lane.)

// TestHandleResumeStream_CapsHugeCredit verifies that the relay does not
// panic on adversarial Credits values (e.g., MaxInt64 from a buggy or
// malicious backend). The cap is enforced in handleResumeStream.
func TestHandleResumeStream_CapsHugeCredit(t *testing.T) {
	t.Parallel()

	b, clientWS, cleanup := startBackend(t)
	defer cleanup()

	clientID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowLocal.Close()
	defer slowRemote.Close()
	require.NoError(t, b.AddClient(slowLocal, clientID, "example.com", false))

	// Drain EventConnect.
	readUntilControl(t, clientWS, 2*time.Second, func(c protocol.ControlMessage) bool {
		return c.Event == protocol.EventConnect && c.ClientID == clientID
	})

	// Send adversarial EventResumeStream from the "backend" (clientWS) to the
	// hub. Without the cap, the hub would call make(chan struct{}, MaxInt64)
	// and panic.
	resumeMsg := protocol.ControlMessage{
		Event:    protocol.EventResumeStream,
		ClientID: clientID,
		Credits:  math.MaxInt64,
	}
	payload, err := json.Marshal(resumeMsg)
	require.NoError(t, err)
	frame := append([]byte{protocol.ControlByteControl}, payload...)
	require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage, frame))

	// Give the hub a moment to process. If it panics, the test crashes.
	// If it survives, the cap is working.
	time.Sleep(100 * time.Millisecond)

	// Sanity check: send a normal data message and verify the hub is still
	// alive (not deadlocked or crashed).
	dataMsg := append([]byte{protocol.ControlByteData}, clientID[:]...)
	dataMsg = append(dataMsg, []byte("alive")...)
	require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage, dataMsg))

	// Read from the slow client to confirm the hub processed the data.
	buf := make([]byte, 256)
	slowRemote.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := slowRemote.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "alive", string(buf[:n]))
}
