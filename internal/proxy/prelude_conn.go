package proxy

import (
	"bytes"
	"io"
	"net"
)

// preludeConn wraps a net.Conn so that reads first consume the provided
// prelude bytes before reading from the underlying connection. All other
// methods delegate to the wrapped connection.
type preludeConn struct {
	net.Conn
	r io.Reader
}

// WithPrelude returns a net.Conn whose Read will yield prelude first, then the
// remaining bytes from conn. Useful when an earlier sniff consumed bytes that
// should still be visible to a protocol handler (e.g., serving ACME HTTP).
func WithPrelude(conn net.Conn, prelude []byte) net.Conn {
	if len(prelude) == 0 {
		return conn
	}
	return &preludeConn{Conn: conn, r: io.MultiReader(bytes.NewReader(prelude), conn)}
}

func (pc *preludeConn) Read(p []byte) (int, error) { return pc.r.Read(p) }

// Optional: preserve CloseRead/CloseWrite if the underlying conn supports it.
type halfCloser interface {
	CloseRead() error
	CloseWrite() error
}

func (pc *preludeConn) CloseRead() error {
	if hc, ok := pc.Conn.(halfCloser); ok {
		return hc.CloseRead()
	}
	return nil
}

func (pc *preludeConn) CloseWrite() error {
	if hc, ok := pc.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return nil
}
