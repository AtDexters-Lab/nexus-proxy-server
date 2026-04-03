package peer

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mockPeer implements iface.Peer for testing TunneledConn
type mockPeer struct {
	addr        string
	sendSuccess atomic.Bool
	sendCalls   atomic.Int32
	lastMessage []byte
	mu          sync.Mutex
}

func newMockPeer(sendSuccess bool) *mockPeer {
	p := &mockPeer{addr: "test-peer"}
	p.sendSuccess.Store(sendSuccess)
	return p
}

func (p *mockPeer) Addr() string { return p.addr }

func (p *mockPeer) Send(message []byte) bool {
	p.sendCalls.Add(1)
	p.mu.Lock()
	p.lastMessage = make([]byte, len(message))
	copy(p.lastMessage, message)
	p.mu.Unlock()
	return p.sendSuccess.Load()
}

func (p *mockPeer) StartTunnel(net.Conn, string, bool) {}

func (p *mockPeer) getSendCalls() int32 {
	return p.sendCalls.Load()
}

func (p *mockPeer) getLastMessage() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastMessage
}

func (p *mockPeer) setSendSuccess(success bool) {
	p.sendSuccess.Store(success)
}

func TestTunneledConnPauseSuccess(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Pause should succeed and update local state
	tc.Pause()

	if !tc.IsPaused() {
		t.Fatal("expected connection to be paused after successful Pause()")
	}
	if peer.getSendCalls() != 1 {
		t.Fatalf("expected 1 send call, got %d", peer.getSendCalls())
	}
}

func TestTunneledConnPauseFailsWhenSendQueueFull(t *testing.T) {
	peer := newMockPeer(false) // Send will fail
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Pause should fail and NOT update local state
	tc.Pause()

	if tc.IsPaused() {
		t.Fatal("expected connection to NOT be paused when send fails")
	}
	if peer.getSendCalls() != 1 {
		t.Fatalf("expected 1 send call, got %d", peer.getSendCalls())
	}
}

func TestTunneledConnResumeSuccess(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// First pause
	tc.Pause()
	if !tc.IsPaused() {
		t.Fatal("expected connection to be paused")
	}

	// Resume should succeed and update local state
	tc.Resume()

	if tc.IsPaused() {
		t.Fatal("expected connection to NOT be paused after successful Resume()")
	}
	if peer.getSendCalls() != 2 {
		t.Fatalf("expected 2 send calls (pause + resume), got %d", peer.getSendCalls())
	}
}

func TestTunneledConnResumeFailsWhenSendQueueFull(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// First pause successfully
	tc.Pause()
	if !tc.IsPaused() {
		t.Fatal("expected connection to be paused")
	}

	// Now make send fail
	peer.setSendSuccess(false)

	// Resume should fail and NOT update local state
	tc.Resume()

	if !tc.IsPaused() {
		t.Fatal("expected connection to remain paused when resume send fails")
	}
	if peer.getSendCalls() != 2 {
		t.Fatalf("expected 2 send calls (pause + failed resume), got %d", peer.getSendCalls())
	}
}

func TestTunneledConnPauseIdempotent(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Multiple pauses should be idempotent
	tc.Pause()
	tc.Pause()
	tc.Pause()

	if !tc.IsPaused() {
		t.Fatal("expected connection to be paused")
	}
	// Only 1 send call since subsequent pauses are no-ops
	if peer.getSendCalls() != 1 {
		t.Fatalf("expected 1 send call (idempotent), got %d", peer.getSendCalls())
	}
}

func TestTunneledConnResumeIdempotent(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Resume when not paused should be no-op
	tc.Resume()
	tc.Resume()

	if tc.IsPaused() {
		t.Fatal("expected connection to not be paused")
	}
	// No send calls since we're not paused
	if peer.getSendCalls() != 0 {
		t.Fatalf("expected 0 send calls (not paused), got %d", peer.getSendCalls())
	}
}

func TestTunneledConnPauseAfterClose(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)

	tc.Close()
	sendCallsAfterClose := peer.getSendCalls()

	// Pause after close should be no-op
	tc.Pause()

	if tc.IsPaused() {
		t.Fatal("expected pause to be no-op after close")
	}
	// No additional send calls after close
	if peer.getSendCalls() != sendCallsAfterClose {
		t.Fatalf("expected no additional send calls after close, got %d", peer.getSendCalls()-sendCallsAfterClose)
	}
}

func TestTunneledConnResumeAfterClose(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)

	// Pause first
	tc.Pause()
	tc.Close()
	sendCallsAfterClose := peer.getSendCalls()

	// Resume after close should be no-op
	tc.Resume()

	// No additional send calls after close
	if peer.getSendCalls() != sendCallsAfterClose {
		t.Fatalf("expected no additional send calls after close, got %d", peer.getSendCalls()-sendCallsAfterClose)
	}
}

// TestTunneledConnReadDoesNotBlockWhenPaused verifies that Read() does NOT block
// when paused. This is critical because blocking would deadlock the shared readPump
// since io.Pipe is unbuffered.
func TestTunneledConnReadDoesNotBlockWhenPaused(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Pause the connection
	tc.Pause()
	if !tc.IsPaused() {
		t.Fatal("expected connection to be paused")
	}

	// Write data via WriteToPipe (simulating readPump)
	// This should NOT block even though we're paused
	writeDone := make(chan struct{})
	go func() {
		tc.WriteToPipe([]byte("hello"))
		close(writeDone)
	}()

	// Read should still work - it must not block when paused
	// The actual backpressure happens at the origin peer, not locally
	readDone := make(chan struct{})
	var readData []byte
	go func() {
		buf := make([]byte, 10)
		n, _ := tc.Read(buf)
		readData = buf[:n]
		close(readDone)
	}()

	// Both operations should complete quickly
	select {
	case <-writeDone:
		// Good - WriteToPipe didn't block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WriteToPipe blocked - this would deadlock the readPump!")
	}

	select {
	case <-readDone:
		if string(readData) != "hello" {
			t.Fatalf("expected 'hello', got %q", string(readData))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read blocked when paused - this is incorrect behavior")
	}
}

func TestTunneledConnConcurrentPauseResume(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Rapid concurrent pause/resume calls should not cause races
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tc.Pause()
		}()
		go func() {
			defer wg.Done()
			tc.Resume()
		}()
	}
	wg.Wait()

	// Verify we can still use the connection
	peer.setSendSuccess(true)

	// Write and read should work regardless of pause state
	// (Read never blocks locally for TunneledConn)
	go func() {
		tc.WriteToPipe([]byte("test"))
	}()

	buf := make([]byte, 10)
	done := make(chan struct{})
	go func() {
		tc.Read(buf)
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read timed out after concurrent pause/resume")
	}
}

// TestTunneledConnWriteToPipeNeverBlocksReadPump verifies that WriteToPipe
// completes even when the Read side is slow, preventing readPump deadlock.
func TestTunneledConnWriteToPipeNeverBlocksReadPump(t *testing.T) {
	peer := newMockPeer(true)
	clientID := uuid.New()
	tc := NewTunneledConn(clientID, peer, "192.168.1.1:8080", 8080)
	defer tc.Close()

	// Simulate a slow reader by pausing before reading
	tc.Pause()

	// WriteToPipe should still complete because Read doesn't block locally
	// Start the write
	writeDone := make(chan struct{})
	go func() {
		tc.WriteToPipe([]byte("data"))
		close(writeDone)
	}()

	// Start the read (which should NOT be blocked by pause)
	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, 10)
		tc.Read(buf)
		close(readDone)
	}()

	// Both should complete
	select {
	case <-writeDone:
		// Good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WriteToPipe blocked - readPump would deadlock!")
	}

	select {
	case <-readDone:
		// Good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read blocked unexpectedly")
	}
}

func TestTunneledConnRemoteAddr(t *testing.T) {
	peer := newMockPeer(true)

	tests := []struct {
		name     string
		clientIp string
		want     string
	}{
		{"IPv4 with port", "203.0.113.45:54321", "203.0.113.45:54321"},
		{"IPv6 with port", "[::1]:54321", "[::1]:54321"},
		{"bare IPv4 no port", "192.168.1.1", "192.168.1.1:0"},
		{"malformed string", "garbage", ":0"},
		{"empty string", "", ":0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := NewTunneledConn(uuid.New(), peer, tt.clientIp, 443)
			defer tc.Close()

			got := tc.RemoteAddr().String()
			if got != tt.want {
				t.Errorf("RemoteAddr().String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseUDPAddr(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // expected String() output, or "" if nil expected
	}{
		{"IPv4 with port", "192.168.1.1:8080", "192.168.1.1:8080"},
		{"IPv6 with port", "[::1]:8080", "[::1]:8080"},
		{"bare IPv4 no port", "10.0.0.1", "10.0.0.1:0"},
		{"hostname must not resolve", "hostname:80", ""},
		{"empty string", "", ""},
		{"malformed", "garbage", ""},
		{"hostname:port ParseIP fails", "notanip:1234", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseUDPAddr(tt.input)
			if tt.want == "" {
				if got != nil {
					t.Errorf("parseUDPAddr(%q) = %v, want nil", tt.input, got)
				}
			} else {
				if got == nil {
					t.Fatalf("parseUDPAddr(%q) = nil, want %q", tt.input, tt.want)
				}
				if got.String() != tt.want {
					t.Errorf("parseUDPAddr(%q).String() = %q, want %q", tt.input, got.String(), tt.want)
				}
			}
		})
	}
}
