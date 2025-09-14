package proxy

import (
	"net"
	"sync"
)

// SingleConnListener is a net.Listener that exposes exactly one pre-existing
// connection to an http.Server. It returns the connection once from Accept.
// Subsequent Accept calls block until the connection is closed, then return
// net.ErrClosed. This prevents premature server shutdown races.
type SingleConnListener struct {
	conn      net.Conn
	handedOut bool
	mu        sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

// closeSignalConn wraps a net.Conn and signals on Close so the listener can
// know when it is safe to have Accept stop blocking and return ErrClosed.
type closeSignalConn struct {
	net.Conn
	notify func()
	once   sync.Once
}

func (c *closeSignalConn) Close() error {
	c.once.Do(c.notify)
	return c.Conn.Close()
}

// NewSingleConnListener creates a new SingleConnListener.
func NewSingleConnListener(conn net.Conn) net.Listener {
	l := &SingleConnListener{closed: make(chan struct{})}
	// Wrap provided conn to notify when it is closed by http.Server.
	l.conn = &closeSignalConn{Conn: conn, notify: func() { l.closeOnce.Do(func() { close(l.closed) }) }}
	return l
}

// Accept returns the connection on the first call. On subsequent calls it
// blocks until the connection is closed, then returns net.ErrClosed.
func (l *SingleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.handedOut {
		l.handedOut = true
		c := l.conn
		l.mu.Unlock()
		return c, nil
	}
	ch := l.closed
	l.mu.Unlock()
	<-ch
	return nil, net.ErrClosed
}

// Close closes the underlying connection and unblocks any Accept waiters.
func (l *SingleConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.conn.Close()
}

// Addr returns the single connection's local address.
func (l *SingleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}
