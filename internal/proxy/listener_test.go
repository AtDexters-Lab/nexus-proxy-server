package proxy

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
)

// fakeConn implements net.Conn just enough to satisfy gatedHandle's reject
// path: Close is observable, the rest are inert. The reject path closes the
// conn and returns without ever calling Read/Write.
type fakeConn struct {
	closed atomic.Bool
}

func (c *fakeConn) Read(_ []byte) (int, error)         { return 0, net.ErrClosed }
func (c *fakeConn) Write(_ []byte) (int, error)        { return 0, nil }
func (c *fakeConn) Close() error                       { c.closed.Store(true); return nil }
func (c *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443} }
func (c *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }
func (c *fakeConn) SetDeadline(_ time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

// TestListener_PeekSemaphoreBounds asserts that when the peek semaphore is
// full, a fresh connection is rejected (closed) and the slot count is
// unchanged; releasing a slot lets the next acquire succeed. The semaphore
// is pre-filled directly so the test is fully deterministic — no goroutines,
// no polling — and gatedHandle only exercises its reject branch.
func TestListener_PeekSemaphoreBounds(t *testing.T) {
	cfg := &config.Config{MaxConcurrentPeeks: 2}
	l := NewListener(cfg, nil, nil, nil, nil)

	if cap(l.peekSem) != 2 {
		t.Fatalf("expected peekSem capacity 2, got %d", cap(l.peekSem))
	}

	// Saturate.
	l.peekSem <- struct{}{}
	l.peekSem <- struct{}{}

	conn := &fakeConn{}
	l.gatedHandle(conn)
	if !conn.closed.Load() {
		t.Fatal("expected connection to be closed when semaphore full")
	}
	if len(l.peekSem) != 2 {
		t.Fatalf("rejected attempt should not change slot count, got %d", len(l.peekSem))
	}

	// Release a slot; capacity recovers.
	<-l.peekSem
	if len(l.peekSem) != 1 {
		t.Fatalf("expected one slot freed, got %d held", len(l.peekSem))
	}
}

// TestListener_PeekSemaphoreUnbounded asserts that MaxConcurrentPeeks=-1
// leaves peekSem nil so gatedHandle's nil-check skips the bound entirely.
// Guards against a regression where a non-positive cap accidentally
// allocated a zero-capacity channel that always rejects.
func TestListener_PeekSemaphoreUnbounded(t *testing.T) {
	cfg := &config.Config{MaxConcurrentPeeks: -1}
	l := NewListener(cfg, nil, nil, nil, nil)
	if l.peekSem != nil {
		t.Fatalf("expected nil peekSem when unbounded, got %p (cap=%d)", l.peekSem, cap(l.peekSem))
	}
}
