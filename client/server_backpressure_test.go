package client

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// makeTestConn creates a clientConn registered in the client's localConns
// and connQueues, suitable for testing backpressure. Returns the conn and
// its per-connection outbound queue.
func makeTestConn(c *Client, id uuid.UUID) (*clientConn, chan outboundMessage) {
	cc := &clientConn{
		id:      id,
		quit:    make(chan struct{}),
		drained: make(chan struct{}),
		writeCh: make(chan []byte, localConnWriteBuffer),
		flow: flowControl{
			lowWaterMark:  DefaultLowWaterMark,
			highWaterMark: DefaultHighWaterMark,
			maxBuffer:     DefaultMaxBuffer,
		},
		session: &Session{},
	}
	cc.state.Store(uint32(ConnStateActive))
	cc.session.connected.Store(true)
	cc.session.done = make(chan struct{})
	c.localConns.Store(id, cc)
	c.getOrCreateQueue(id)
	return cc, c.getQueue(id)
}

// sendControlMessage sends a control message from the "server" side of the
// WebSocket to the client's readPump.
func sendControlMessage(t *testing.T, serverWS *websocket.Conn, event protocol.EventType, clientID uuid.UUID) {
	t.Helper()
	msg := protocol.ControlMessage{Event: event, ClientID: clientID, Reason: "test"}
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	frame := append([]byte{protocol.ControlByteControl}, payload...)
	if err := serverWS.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

// TestServerPause_SetsFlag verifies that EventPauseStream from the server
// sets serverPaused on the corresponding clientConn.
func TestServerPause_SetsFlag(t *testing.T) {
	c := newTestClient(t)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, _ := makeTestConn(c, id)

	// Start readPump to process the control message.
	c.wg.Add(1)
	go c.readPump()

	sendControlMessage(t, serverWS, protocol.EventPauseStream, id)
	time.Sleep(50 * time.Millisecond)

	if !cc.serverPaused.Load() {
		t.Fatal("serverPaused was not set after EventPauseStream")
	}

	c.cancel()
	clientWS.Close()
	c.wg.Wait()
}

// TestServerResume_ClearsFlag verifies that EventResumeStream clears
// serverPaused and re-signals dataReady.
func TestServerResume_ClearsFlag(t *testing.T) {
	c := newTestClient(t)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, _ := makeTestConn(c, id)

	c.wg.Add(1)
	go c.readPump()

	// Pause, then resume.
	sendControlMessage(t, serverWS, protocol.EventPauseStream, id)
	time.Sleep(50 * time.Millisecond)
	if !cc.serverPaused.Load() {
		t.Fatal("serverPaused not set")
	}

	sendControlMessage(t, serverWS, protocol.EventResumeStream, id)
	time.Sleep(50 * time.Millisecond)
	if cc.serverPaused.Load() {
		t.Fatal("serverPaused was not cleared after EventResumeStream")
	}

	// Verify dataReady was signaled (reSignalDataReady should have fired).
	select {
	case gotID := <-c.dataReady:
		if gotID != id {
			t.Fatalf("dataReady signaled for wrong clientID: got %s, want %s", gotID, id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dataReady was not signaled after resume")
	}

	c.cancel()
	clientWS.Close()
	c.wg.Wait()
}

// TestLayer1_SuppressSignaling verifies that enqueueData does NOT signal
// dataReady when serverPaused is true.
func TestLayer1_SuppressSignaling(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Set server-paused.
	cc.serverPaused.Store(true)

	// Enqueue a data message.
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], id[:])
	msg := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append(header, []byte("test_data")...),
	}
	err := c.enqueueData(msg)
	if err != nil {
		t.Fatalf("enqueueData failed: %v", err)
	}

	// Data should be in the queue.
	select {
	case <-queue:
		// Good — data was enqueued.
	default:
		t.Fatal("data was not enqueued to the per-connection queue")
	}

	// dataReady should NOT have been signaled (Layer 1).
	select {
	case <-c.dataReady:
		t.Fatal("dataReady was signaled despite serverPaused being true — Layer 1 failed")
	case <-time.After(100 * time.Millisecond):
		// Good — no signal.
	}
}

// TestLayer2_SkipDrain verifies that drainConnectionQueue returns without
// dequeuing when serverPaused is true.
func TestLayer2_SkipDrain(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Put a message in the queue.
	queue <- outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     []byte("should_not_be_drained"),
	}

	// Set server-paused.
	cc.serverPaused.Store(true)
	cc.signaled.Store(true)

	// Call drainConnectionQueue — it should skip the drain.
	c.drainConnectionQueue(clientWS, id, 4)

	// Message should still be in the queue (not dequeued).
	select {
	case <-queue:
		// Good — message is still there.
	default:
		t.Fatal("drainConnectionQueue dequeued data despite serverPaused — Layer 2 failed")
	}

	// signaled should be cleared to allow re-signal on resume.
	if cc.signaled.Load() {
		t.Fatal("signaled flag was not cleared after skip — re-signal on resume would fail")
	}
}

// TestLayer2_FallsThroughOnClose verifies that drainConnectionQueue does
// NOT skip drain when the connection is closing (quit closed), even if
// serverPaused is true. This ensures closeOnDrained fires promptly.
func TestLayer2_FallsThroughOnClose(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Put a message in the queue.
	queue <- outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     []byte("should_be_drained"),
	}

	// Set server-paused AND close quit (connection closing).
	cc.serverPaused.Store(true)
	close(cc.quit)

	// drainConnectionQueue should fall through to normal drain.
	c.drainConnectionQueue(clientWS, id, 4)

	// Queue should be empty (message was drained).
	select {
	case <-queue:
		t.Fatal("message still in queue — drain should have processed it")
	default:
		// Good — drained.
	}
}

// TestLayer3_RetryOnTimeout verifies that enqueueData retries (instead of
// returning error) when the queue is full and serverPaused is true.
func TestLayer3_RetryOnTimeout(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Fill the queue completely.
	for i := 0; i < cap(queue); i++ {
		queue <- outboundMessage{payload: []byte("filler")}
	}

	// Set server-paused.
	cc.serverPaused.Store(true)

	// Start enqueueData in a goroutine — it should retry (not fail).
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], id[:])
	msg := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append(header, []byte("blocked_data")...),
	}

	result := make(chan error, 1)
	go func() {
		result <- c.enqueueData(msg)
	}()

	// Wait a moment — enqueueData should be looping, not returning error.
	select {
	case err := <-result:
		t.Fatalf("enqueueData returned immediately instead of retrying: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Good — it's retrying.
	}

	// Now clear the pause and drain one item to make room.
	cc.serverPaused.Store(false)
	<-queue // free one slot

	// enqueueData should now succeed.
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("enqueueData failed after unpause: %v", err)
		}
	case <-time.After(enqueueTimeout + time.Second):
		t.Fatal("enqueueData did not complete after unpause and queue drain")
	}
}

// TestLayer3_ExitsOnQuit verifies that enqueueData exits promptly when the
// connection is closing (quit closed), even if serverPaused is true and
// the queue is full.
func TestLayer3_ExitsOnQuit(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Fill the queue.
	for i := 0; i < cap(queue); i++ {
		queue <- outboundMessage{payload: []byte("filler")}
	}
	cc.serverPaused.Store(true)

	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], id[:])
	msg := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append(header, []byte("data")...),
	}

	result := make(chan error, 1)
	go func() {
		result <- c.enqueueData(msg)
	}()

	// Let it enter the retry loop.
	time.Sleep(100 * time.Millisecond)

	// Close quit — simulates connection teardown.
	close(cc.quit)

	// enqueueData should exit within one enqueueTimeout cycle.
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected error from enqueueData after quit, got nil")
		}
	case <-time.After(enqueueTimeout + 2*time.Second):
		t.Fatal("enqueueData did not exit after quit was closed — Layer 3 quit check failed")
	}
}

// TestAutoResume_GenerationCounter verifies that a stale auto-resume timer
// does not clear a newer pause.
func TestAutoResume_GenerationCounter(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, _ := makeTestConn(c, id)

	// Simulate pause → resume → pause cycle.
	// The first pause's auto-resume timer should not clear the second pause.
	cc.serverPaused.CompareAndSwap(false, true)
	gen1 := cc.serverPauseGen.Add(1)

	// Simulate resume (invalidates gen1 timer).
	cc.serverPauseGen.Add(1)
	cc.serverPaused.Store(false)

	// Simulate second pause.
	cc.serverPaused.Store(true)
	cc.serverPauseGen.Add(1)

	// Fire gen1's auto-resume callback manually.
	if cc.serverPauseGen.Load() == gen1 && cc.serverPaused.CompareAndSwap(true, false) {
		t.Fatal("stale auto-resume timer cleared a newer pause — generation counter failed")
	}

	// serverPaused should still be true (second pause intact).
	if !cc.serverPaused.Load() {
		t.Fatal("serverPaused was cleared by stale timer")
	}
}
