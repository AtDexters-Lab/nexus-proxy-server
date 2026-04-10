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
//
// If conn implements StopWrites(), it is called first so that any buffered
// write path (e.g. bufferedConn) can flush remaining data before the TCP
// FIN is sent. This prevents data loss when EventDisconnect arrives shortly
// after a data message.
func GracefulCloseConn(conn net.Conn, abort <-chan struct{}, gracePeriod time.Duration) {
	inner := conn
	for {
		u, ok := inner.(interface{ Unwrap() net.Conn })
		if !ok {
			break
		}
		inner = u.Unwrap()
	}

	if tc, ok := inner.(*net.TCPConn); ok {
		go func() {
			// Let any buffered write layer flush before TCP half-close.
			if sw, ok := conn.(interface{ StopWrites() }); ok {
				sw.StopWrites()
			}
			_ = tc.CloseWrite()
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
