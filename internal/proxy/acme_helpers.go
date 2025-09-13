package proxy

import (
	"net"
	"sync"
)

// SingleConnListener is a net.Listener that returns a single connection.
// This is used to serve a single HTTP request from an existing connection.
type SingleConnListener struct {
	conn net.Conn
	once sync.Once
}

// NewSingleConnListener creates a new SingleConnListener.
func NewSingleConnListener(conn net.Conn) net.Listener {
	return &SingleConnListener{conn: conn}
}

// Accept returns the single connection.
func (l *SingleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() {
		c = l.conn
	})
	if c == nil {
		return nil, net.ErrClosed
	}
	return c, nil
}

// Close closes the single connection.
func (l *SingleConnListener) Close() error {
	return l.conn.Close()
}

// Addr returns the single connection's local address.
func (l *SingleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}
