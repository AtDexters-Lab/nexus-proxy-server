package proxy

import (
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/bandwidth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/google/uuid"
)

// stubHub returns a fixed backend (or error) for SelectBackend; used by the
// release-at-peek-complete test below. Other interface methods are inert.
type stubHub struct {
	backend iface.Backend
	err     error
}

func (h *stubHub) GetLocalRoutes() []string                     { return nil }
func (h *stubHub) SelectBackend(string) (iface.Backend, error)  { return h.backend, h.err }
func (h *stubHub) UDPFlowIdleTimeout(int) (time.Duration, bool) { return 0, false }
func (h *stubHub) GetBandwidthScheduler() *bandwidth.Scheduler  { return nil }

// stubBackend's AddClient signals the test and blocks until released, so the
// test can sample the peek-semaphore state at the exact moment the dispatch
// path has begun.
type stubBackend struct {
	id              string
	addClientCalled chan struct{}
	addClientHold   chan struct{}
}

func (b *stubBackend) ID() string { return b.id }
func (b *stubBackend) AddClient(_ net.Conn, _ uuid.UUID, _ string, _ bool) error {
	close(b.addClientCalled)
	<-b.addClientHold
	// Returning an error short-circuits Client.Start cleanly so the test
	// goroutines wind down once the slot-state assertion has been made.
	return errors.New("stub: test complete")
}
func (b *stubBackend) RemoveClient(uuid.UUID)                      {}
func (b *stubBackend) SendData(uuid.UUID, []byte) error            { return nil }
func (b *stubBackend) HasRecentActivity(uuid.UUID, time.Time) bool { return false }

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

// TestListener_PeekSlotReleasedBeforeDispatch asserts the load-bearing
// invariant of the peek-only bound: the slot is released when peek
// completes, BEFORE the long-lived dispatch begins. Without this, the
// "MaxConcurrentPeeks" name is a lie and the cap silently bounds total
// established connections instead of just peek-phase concurrency.
//
// Setup: cap=1, real TLS ClientHello with SNI fed through a net.Pipe, stub
// backend whose AddClient blocks. When AddClient fires the dispatch path
// has begun; at that instant the slot must be free.
func TestListener_PeekSlotReleasedBeforeDispatch(t *testing.T) {
	bk := &stubBackend{
		id:              "stub",
		addClientCalled: make(chan struct{}),
		addClientHold:   make(chan struct{}),
	}
	cfg := &config.Config{MaxConcurrentPeeks: 1}
	l := NewListener(cfg, &stubHub{backend: bk}, nil, nil, nil)

	server, client := net.Pipe()
	defer client.Close()

	// Drive a real TLS handshake so PeekSNIAndPrelude extracts SNI; the
	// handshake will abort intentionally inside the peek, but the SNI is
	// captured first.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		tlsClient := tls.Client(client, &tls.Config{
			ServerName:         "example.com",
			InsecureSkipVerify: true,
		})
		_ = tlsClient.Handshake()
	}()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		l.gatedHandle(server)
	}()

	// Wait for the dispatch path to reach AddClient.
	select {
	case <-bk.addClientCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("AddClient never invoked; dispatch did not run")
	}

	if got := len(l.peekSem); got != 0 {
		t.Fatalf("expected peek slot released before AddClient, len(peekSem)=%d", got)
	}

	// Allow AddClient to return (with stub error) so handleConnection can
	// finish; the stub's error path closes the conn, ending the goroutines.
	close(bk.addClientHold)
	_ = client.Close()
	<-handlerDone
	<-clientDone
}
