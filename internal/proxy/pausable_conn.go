package proxy

import (
	"net"
	"sync"
)

// PausableConn wraps a net.Conn with pause/resume capability.
// When paused, Read() blocks until Resume() is called or Close() is called.
type PausableConn struct {
	net.Conn
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
	closed bool
}

// NewPausableConn wraps an existing connection.
func NewPausableConn(conn net.Conn) *PausableConn {
	pc := &PausableConn{Conn: conn}
	pc.cond = sync.NewCond(&pc.mu)
	return pc
}

// Read blocks if paused, then delegates to underlying conn.
func (pc *PausableConn) Read(b []byte) (int, error) {
	pc.mu.Lock()
	for pc.paused && !pc.closed {
		pc.cond.Wait()
	}
	if pc.closed {
		pc.mu.Unlock()
		return 0, net.ErrClosed
	}
	pc.mu.Unlock()
	return pc.Conn.Read(b)
}

// Pause signals reads should block. Idempotent. No-op if already closed.
func (pc *PausableConn) Pause() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if !pc.closed {
		pc.paused = true
	}
}

// Resume signals reads can proceed. Idempotent. No-op if already closed.
func (pc *PausableConn) Resume() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if !pc.closed {
		pc.paused = false
		pc.cond.Broadcast()
	}
}

// IsPaused returns the current pause state.
func (pc *PausableConn) IsPaused() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.paused
}

// Close unblocks waiting readers and closes the underlying connection.
func (pc *PausableConn) Close() error {
	pc.mu.Lock()
	pc.closed = true
	pc.cond.Broadcast()
	pc.mu.Unlock()
	return pc.Conn.Close()
}
