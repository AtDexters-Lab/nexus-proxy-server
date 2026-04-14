package client

import (
	"encoding/json"
	"math"
	"net"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// makeTestConn creates a clientConn registered in the client's localConns
// and connQueues, suitable for testing credit-based flow control.
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

func sendControlMessage(t *testing.T, serverWS *websocket.Conn, msg protocol.ControlMessage) {
	t.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	frame := append([]byte{protocol.ControlByteControl}, payload...)
	if err := serverWS.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

// setupTestListener starts a TCP listener and configures the client's
// port mapping to route to it. Returns the port and a cleanup function.
func setupTestListener(t *testing.T, c *Client) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	// Accept and discard connections so dial succeeds.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						conn.Close()
						return
					}
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	c.config.PortMappings[port] = PortMapping{Default: ln.Addr().String()}
	return port
}

// TestCredits_Reverse_InitialFromConnect verifies that Credits in
// EventConnect are stored as initial availableCredits.
func TestCredits_Reverse_InitialFromConnect(t *testing.T) {
	c := newTestClient(t)
	port := setupTestListener(t, c)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	session := c.beginSession()
	c.wg.Add(1)
	go c.writePump(session)
	c.wg.Add(1)
	go c.readPump()

	id := uuid.New()
	sendControlMessage(t, serverWS, protocol.ControlMessage{
		Event:    protocol.EventConnect,
		ClientID: id,
		ConnPort: port,
		ClientIP: "127.0.0.1",
		Hostname: "example.com",
		Credits:  42,
	})

	time.Sleep(100 * time.Millisecond)

	val, ok := c.localConns.Load(id)
	if !ok {
		t.Fatal("connection not created")
	}
	cc := val.(*clientConn)
	got := cc.availableCredits.Load()
	if got != 42 {
		t.Fatalf("availableCredits = %d, want 42", got)
	}

	c.cancel()
	clientWS.Close()
	c.wg.Wait()
}

// TestCredits_Reverse_UnlimitedWhenZero verifies that Credits=0
// (old server) results in math.MaxInt64 (unlimited).
func TestCredits_Reverse_UnlimitedWhenZero(t *testing.T) {
	c := newTestClient(t)
	port := setupTestListener(t, c)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	session := c.beginSession()
	c.wg.Add(1)
	go c.writePump(session)
	c.wg.Add(1)
	go c.readPump()

	id := uuid.New()
	sendControlMessage(t, serverWS, protocol.ControlMessage{
		Event:    protocol.EventConnect,
		ClientID: id,
		ConnPort: port,
		ClientIP: "127.0.0.1",
		Hostname: "example.com",
		// Credits deliberately omitted (zero value)
	})

	time.Sleep(100 * time.Millisecond)

	val, ok := c.localConns.Load(id)
	if !ok {
		t.Fatal("connection not created")
	}
	cc := val.(*clientConn)
	got := cc.availableCredits.Load()
	if got != math.MaxInt64 {
		t.Fatalf("availableCredits = %d, want MaxInt64", got)
	}

	c.cancel()
	clientWS.Close()
	c.wg.Wait()
}

// TestCredits_Reverse_SuppressSignalingAtZero verifies that enqueueData
// does NOT signal dataReady when availableCredits <= 0.
func TestCredits_Reverse_SuppressSignalingAtZero(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)
	cc.availableCredits.Store(0)

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

	select {
	case <-queue:
	default:
		t.Fatal("data was not enqueued")
	}

	select {
	case <-c.dataReady:
		t.Fatal("dataReady was signaled despite credits=0")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestCredits_Reverse_SkipDrainAtZero verifies drainConnectionQueue
// returns without dequeuing when credits=0.
func TestCredits_Reverse_SkipDrainAtZero(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	queue <- outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     []byte("should_not_be_drained"),
	}

	cc.availableCredits.Store(0)
	cc.signaled.Store(true)

	c.drainConnectionQueue(clientWS, id, 4)

	select {
	case <-queue:
	default:
		t.Fatal("drainConnectionQueue dequeued data despite credits=0")
	}

	if cc.signaled.Load() {
		t.Fatal("signaled flag not cleared")
	}
}

// TestCredits_Reverse_ClosingKickstartFlushesOneBatch verifies the
// closing-mode kickstart that breaks the proxy-sub-batch-deadlock:
// when teardown finds credits=0 with queued data, the client grants
// itself ONE batch (CreditReplenishBatch) of credits and flushes that
// many frames past the official window. The proxy receives them,
// crosses its consumed≥8 threshold, fires a real EventResumeStream,
// and credit-gated drain resumes naturally for the rest of the queue.
//
// Without the kickstart: proxy's writeCh empties before reaching 8
// consumed, no replenish ever fires, client deadlocks until
// connectionDrainTimeout (Codex P1 from round 3).
//
// The kickstart is one-shot per connection-close — repeated drain
// calls with credits=0 and queue non-empty after the first kickstart
// just return waiting (verified by TestCredits_Reverse_ClosingKickstartIsOneShot).
func TestCredits_Reverse_ClosingKickstartFlushesOneBatch(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Seed exactly CreditReplenishBatch+1 frames so the kickstart batch
	// drains and one frame remains in the queue (so we can verify the
	// kickstart is bounded).
	for i := 0; i < int(protocol.CreditReplenishBatch)+1; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}
	cc.availableCredits.Store(0)
	close(cc.quit)

	c.drainConnectionQueue(clientWS, id, 4)
	// drainConnectionQueue only drains maxMessages=4 per call; with the
	// kickstart adding 8 credits, the first call drains 4. Re-invoke
	// twice more to flush all 8 kickstart credits.
	c.drainConnectionQueue(clientWS, id, 4)
	c.drainConnectionQueue(clientWS, id, 4)

	// Exactly CreditReplenishBatch (=8) frames flushed past zero
	// credits; the 9th frame stays in the queue waiting for a real
	// EventResumeStream replenishment.
	if remaining := len(queue); remaining != 1 {
		t.Fatalf("kickstart drained %d frames — expected exactly CreditReplenishBatch=%d (1 frame should remain queued)",
			int(protocol.CreditReplenishBatch)+1-remaining, protocol.CreditReplenishBatch)
	}
}

// TestCredits_Reverse_ClosingKickstartIsOneShot verifies the
// kickstart fires AT MOST ONCE per connection-close. If drain is
// re-invoked after the kickstart is consumed (credits exhausted
// again), the second call must return waiting — never re-grant
// another batch — so total over-send is bounded at exactly
// CreditReplenishBatch frames per close.
func TestCredits_Reverse_ClosingKickstartIsOneShot(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Seed 100 frames — well over what the kickstart can flush.
	const seeded = 100
	for i := 0; i < seeded; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}
	cc.availableCredits.Store(0)
	close(cc.quit)

	// Loop drain calls: each pass should drain at most maxMessages=4
	// from the kickstart budget, then return waiting once the 8
	// kickstart credits are spent.
	for i := 0; i < 50; i++ {
		c.drainConnectionQueue(clientWS, id, 4)
	}

	drained := seeded - len(queue)
	if int64(drained) > protocol.CreditReplenishBatch {
		t.Fatalf("kickstart fired more than once: drained %d frames past credits — "+
			"expected ≤ CreditReplenishBatch=%d (unbounded kickstart reintroduces writeCh overflow)",
			drained, protocol.CreditReplenishBatch)
	}
}

// TestCredits_Reverse_Replenish verifies EventResumeStream with Credits
// adds to availableCredits and re-signals dataReady.
func TestCredits_Reverse_Replenish(t *testing.T) {
	c := newTestClient(t)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, _ := makeTestConn(c, id)
	cc.availableCredits.Store(0)

	c.wg.Add(1)
	go c.readPump()

	sendControlMessage(t, serverWS, protocol.ControlMessage{
		Event:    protocol.EventResumeStream,
		ClientID: id,
		Credits:  16,
	})

	time.Sleep(50 * time.Millisecond)

	got := cc.availableCredits.Load()
	if got != 16 {
		t.Fatalf("availableCredits = %d, want 16", got)
	}

	select {
	case gotID := <-c.dataReady:
		if gotID != id {
			t.Fatalf("wrong clientID in dataReady")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dataReady not signaled after replenishment")
	}

	c.cancel()
	clientWS.Close()
	c.wg.Wait()
}

// TestCredits_Forward_InitialGrant verifies that the client sends
// forward credits to Nexus after receiving EventConnect.
func TestCredits_Forward_InitialGrant(t *testing.T) {
	c := newTestClient(t)
	port := setupTestListener(t, c)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	session := c.beginSession()
	c.wg.Add(1)
	go c.writePump(session)
	c.wg.Add(1)
	go c.readPump()

	id := uuid.New()
	sendControlMessage(t, serverWS, protocol.ControlMessage{
		Event:    protocol.EventConnect,
		ClientID: id,
		ConnPort: port,
		ClientIP: "127.0.0.1",
		Hostname: "example.com",
		Credits:  protocol.DefaultCreditCapacity,
	})

	// Read from serverWS — should receive forward credit grant.
	serverWS.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, msg, err := serverWS.ReadMessage()
		if err != nil {
			t.Fatalf("timed out waiting for forward credit grant: %v", err)
		}
		if len(msg) < 2 || msg[0] != protocol.ControlByteControl {
			continue
		}
		var ctrl protocol.ControlMessage
		if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
			continue
		}
		if ctrl.Event == protocol.EventResumeStream && ctrl.ClientID == id && ctrl.Credits > 0 {
			if ctrl.Credits != int64(c.config.FlowControl.MaxBuffer) {
				t.Fatalf("forward credits = %d, want %d", ctrl.Credits, c.config.FlowControl.MaxBuffer)
			}
			break
		}
	}

	c.cancel()
	clientWS.Close()
	c.wg.Wait()
}

// TestCredits_Reverse_RetryEnqueueAtZero verifies enqueueData retries
// when the queue is full and credits=0 (doesn't kill the connection).
func TestCredits_Reverse_RetryEnqueueAtZero(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	for i := 0; i < cap(queue); i++ {
		queue <- outboundMessage{payload: []byte("filler")}
	}

	cc.availableCredits.Store(0)

	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], id[:])
	msg := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append(header, []byte("blocked")...),
	}

	result := make(chan error, 1)
	go func() {
		result <- c.enqueueData(msg)
	}()

	select {
	case err := <-result:
		t.Fatalf("enqueueData returned immediately: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	cc.availableCredits.Store(10)
	<-queue

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("enqueueData failed after replenishment: %v", err)
		}
	case <-time.After(enqueueTimeout + time.Second):
		t.Fatal("enqueueData did not complete after replenishment")
	}
}
