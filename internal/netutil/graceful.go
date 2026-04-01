package netutil

import (
	"net"
	"time"
)

// GracefulCloseConn performs a TCP half-close (CloseWrite), giving the remote
// end time to read any buffered data, then fully closes after gracePeriod.
// The abort channel can be used to skip the grace period; if nil or never
// closed, the full grace period is used. Falls back to immediate close for
// non-TCP connections.
func GracefulCloseConn(conn net.Conn, abort <-chan struct{}, gracePeriod time.Duration) {
	inner := conn
	if u, ok := conn.(interface{ Unwrap() net.Conn }); ok {
		inner = u.Unwrap()
	}

	if tc, ok := inner.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		go func() {
			if abort != nil {
				select {
				case <-time.After(gracePeriod):
				case <-abort:
				}
			} else {
				time.Sleep(gracePeriod)
			}
			_ = conn.Close()
		}()
		return
	}

	_ = conn.Close()
}
