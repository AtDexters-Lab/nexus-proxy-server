package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Mock Helper for categorization
func TestErrorCategorization(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		want     ErrorCategory
		wantCode string
	}{
		{"nil error", nil, ErrorTransient, "none"},
		{"context canceled", context.Canceled, ErrorTransient, "timeout"},
		{"connection refused", syscall.ECONNREFUSED, ErrorTransient, "connection_refused"},
		{"rate limit", errors.New("429 rate limit exceeded"), ErrorRateLimit, "rate_limited"},
		{"unauthorized", errors.New("401 Unauthorized"), ErrorPermanent, "auth_failed"},
		{"invalid token", errors.New("invalid token provided"), ErrorPermanent, "auth_failed"},
		{"forbidden", errors.New("403 Forbidden"), ErrorPermanent, "auth_failed"},
		{"auth failed", errors.New("authentication failed"), ErrorPermanent, "auth_failed"},
		{"unknown error", errors.New("something went wrong"), ErrorTransient, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := categorizeError(tt.err)
			if got.Category != tt.want {
				t.Errorf("categorizeError() category = %v, want %v", got.Category, tt.want)
			}
			if got.Reason != tt.wantCode {
				t.Errorf("categorizeError() reason = %v, want %v", got.Reason, tt.wantCode)
			}
		})
	}
}

func TestFlowControlLogic(t *testing.T) {
	// Setup a client conn with flow control
	conn := &clientConn{
		id: uuid.New(),
		flow: flowControl{
			lowWaterMark:  2,
			highWaterMark: 5,
			maxBuffer:     10,
		},
		writeCh: make(chan []byte, 10),
	}
	conn.state.Store(uint32(ConnStateActive))

	c := &Client{
		controlSend: make(chan outboundMessage, 10),
	}
	s := &Session{done: make(chan struct{})}
	s.connected.Store(true)
	c.activeSession.Store(s)
	c.ctx = context.Background()

	// Helper to consume control messages
	consumeControl := func() string {
		select {
		case msg := <-c.controlSend:
			// Parse/decoding skipped for simplicity, just checking existence
			// In real test we would parse JSON.
			// msg.payload contains [controlByte + JSON]
			if len(msg.payload) > 1 {
				s := string(msg.payload[1:])
				if strings.Contains(s, "pause_stream") {
					return "pause"
				}
				if strings.Contains(s, "resume_stream") {
					return "resume"
				}
			}
			return "unknown"
		default:
			return "none"
		}
	}

	// 1. Fill buffer up to high water mark
	for i := 0; i < 5; i++ {
		// Simulate handleDataMessage logic
		currentLevel := int(conn.flow.level.Add(1))
		if currentLevel >= conn.flow.highWaterMark && !conn.flow.paused.Load() {
			conn.flow.paused.Store(true)
			c.enqueueControl(pauseStreamMessage(conn.id, "buffer_full"))
		}
	}

	if !conn.flow.paused.Load() {
		t.Error("expected connection to be paused at high water mark")
	}
	if got := consumeControl(); got != "pause" {
		t.Errorf("expected pause message, got %s", got)
	}

	// 2. Drain buffer down to low water mark
	// Simulate writeToLocal logic
	for i := 0; i < 3; i++ { // 5 -> 4 -> 3 -> 2
		newLevel := int(conn.flow.level.Add(-1))
		if conn.flow.paused.Load() && newLevel <= conn.flow.lowWaterMark {
			conn.flow.paused.Store(false)
			c.enqueueControl(resumeStreamMessage(conn.id))
		}
	}

	if conn.flow.paused.Load() {
		t.Error("expected connection to be resumed at low water mark")
	}
	if got := consumeControl(); got != "resume" {
		t.Errorf("expected resume message, got %s", got)
	}
}

// TestFlushForwardCredits_PreservesCreditsOnInactiveSession is the
// regression test for the symmetric client-side credit-loss bug AND for
// the stale-session leak: when the owning session is no longer connected,
// credits must be restored to forwardConsumed (preventing loss) AND must
// NOT be enqueued onto the shared control channel (preventing leak onto
// a successor session after reconnect).
func TestFlushForwardCredits_PreservesCreditsOnInactiveSession(t *testing.T) {
	// Owning session is constructed but immediately marked inactive,
	// simulating a torn-down connection whose writeToLocal goroutine is
	// still finishing.
	staleSession := &Session{done: make(chan struct{})}
	staleSession.connected.Store(false)

	conn := &clientConn{
		id:      uuid.New(),
		session: staleSession,
	}

	// Build a Client with a fresh "active" session to verify the stale
	// retry does NOT leak onto it.
	c := &Client{
		controlSend: make(chan outboundMessage, 10),
	}
	c.ctx = context.Background()
	freshSession := &Session{done: make(chan struct{})}
	freshSession.connected.Store(true)
	c.activeSession.Store(freshSession)

	conn.forwardConsumed.Store(5)

	c.flushForwardCredits(conn)

	// Credits must be restored.
	if got := conn.forwardConsumed.Load(); got != 5 {
		t.Errorf("forwardConsumed not restored after inactive session: got %d, want 5", got)
	}

	// Crucially: nothing should have leaked onto controlSend.
	select {
	case msg := <-c.controlSend:
		t.Errorf("stale forward credit leaked onto control channel: %x", msg.payload)
	default:
	}
}

// TestFlushForwardCredits_DeliversAndResetsOnSuccess verifies the happy path:
// when enqueueControl succeeds, forwardConsumed is reset to 0 and a credit
// message lands on controlSend.
func TestFlushForwardCredits_DeliversAndResetsOnSuccess(t *testing.T) {
	s := &Session{done: make(chan struct{})}
	s.connected.Store(true)

	conn := &clientConn{
		id:      uuid.New(),
		session: s,
	}
	c := &Client{
		controlSend: make(chan outboundMessage, 10),
	}
	c.ctx = context.Background()
	c.activeSession.Store(s)

	conn.forwardConsumed.Store(7)

	c.flushForwardCredits(conn)

	if got := conn.forwardConsumed.Load(); got != 0 {
		t.Errorf("forwardConsumed not reset after successful flush: got %d, want 0", got)
	}

	select {
	case msg := <-c.controlSend:
		// Verify it's a credit message (EventResumeStream with Credits>0).
		if len(msg.payload) < 2 {
			t.Fatal("control message too short")
		}
		body := string(msg.payload[1:])
		if !strings.Contains(body, "resume_stream") {
			t.Errorf("expected resume_stream credit message, got: %s", body)
		}
	default:
		t.Error("controlSend did not receive credit message after successful flush")
	}
}

// TestWriteToLocal_BatchFlushDeliversCredits verifies that writeToLocal
// flushes forward credits at the batch boundary (8 frames). Sub-batch
// credits are intentionally NOT flushed opportunistically — that would
// create a control-plane storm at fan-out.
func TestWriteToLocal_BatchFlushDeliversCredits(t *testing.T) {
	localServer, localClient := net.Pipe()
	defer localServer.Close()
	defer localClient.Close()

	conn := &clientConn{
		id:           uuid.New(),
		conn:         localClient,
		quit:         make(chan struct{}),
		drained:      make(chan struct{}),
		localFlushed: make(chan struct{}),
		writeCh:      make(chan []byte, 64),
	}
	conn.state.Store(uint32(ConnStateActive))

	c := &Client{
		controlSend: make(chan outboundMessage, 16),
		config:      ClientBackendConfig{Name: "test"},
	}
	c.ctx = context.Background()
	s := &Session{done: make(chan struct{})}
	s.connected.Store(true)
	c.activeSession.Store(s)
	conn.session = s

	// Drain whatever writeToLocal pushes to localClient.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := localServer.Read(buf); err != nil {
				return
			}
		}
	}()

	// Run writeToLocal in a goroutine.
	go c.writeToLocal(conn)

	// Push 8 writes — exactly at the batch threshold, so the flush
	// fires synchronously from the data path.
	for i := 0; i < 8; i++ {
		conn.writeCh <- []byte("x")
	}

	// Wait for the credit message on controlSend.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case msg := <-c.controlSend:
			if len(msg.payload) < 2 {
				continue
			}
			body := string(msg.payload[1:])
			if !strings.Contains(body, "resume_stream") {
				continue
			}
			return
		case <-deadline:
			t.Fatal("batch boundary did not deliver forward credits within 500ms")
		}
	}
}

// TestFlushForwardCredits_NoOpOnZeroPending verifies that flushForwardCredits
// is a no-op when forwardConsumed is 0 (no enqueue, no state change).
func TestFlushForwardCredits_NoOpOnZeroPending(t *testing.T) {
	s := &Session{done: make(chan struct{})}
	s.connected.Store(true)

	conn := &clientConn{
		id:      uuid.New(),
		session: s,
	}
	c := &Client{
		controlSend: make(chan outboundMessage, 10),
	}
	c.ctx = context.Background()
	c.activeSession.Store(s)

	c.flushForwardCredits(conn)

	if got := conn.forwardConsumed.Load(); got != 0 {
		t.Errorf("forwardConsumed should remain 0: got %d", got)
	}
	select {
	case <-c.controlSend:
		t.Error("controlSend received a message when forwardConsumed was 0")
	default:
	}
}

func TestStateTransitions(t *testing.T) {
	conn := &clientConn{}
	conn.state.Store(uint32(ConnStatePending))

	if !conn.transition(ConnStatePending, ConnStateActive) {
		t.Error("failed transition Pending->Active")
	}
	if conn.state.Load() != uint32(ConnStateActive) {
		t.Error("state not Active")
	}

	if conn.transition(ConnStatePending, ConnStateClosed) {
		t.Error("allowed invalid transition Pending->Closed from Active")
	}

	if !conn.transition(ConnStateActive, ConnStateDraining) {
		t.Error("failed transition Active->Draining")
	}
}
