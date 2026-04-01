package client

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

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
	c.connected.Store(true)
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
