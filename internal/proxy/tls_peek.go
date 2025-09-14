package proxy

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"time"
)

// errAbortHandshake is returned from the tls.Config callback to stop the
// handshake as soon as the ClientHello (and thus SNI) is available.
var errAbortHandshake = errors.New("abort tls handshake after clienthello")

// readRecorder wraps a net.Conn and records up to max bytes that are read
// through it. This is used to capture the exact ClientHello bytes so they can
// be forwarded to the selected backend before proxying the remainder.
type readRecorder struct {
	net.Conn
	max    int
	buf    bytes.Buffer
	capped bool
}

func (r *readRecorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	if n > 0 && !r.capped {
		// Only record up to r.max bytes to avoid unbounded memory usage.
		remain := r.max - r.buf.Len()
		if remain <= 0 {
			r.capped = true
		} else {
			toWrite := n
			if toWrite > remain {
				toWrite = remain
				r.capped = true
			}
			if toWrite > 0 {
				r.buf.Write(p[:toWrite])
			}
		}
	}
	return n, err
}

// PeekSNIAndPrelude reads only as much as needed to obtain the SNI from the
// incoming TLS ClientHello using crypto/tls, capturing the bytes that were read
// so they can be replayed to the chosen backend. It returns the server name
// (lowercased) and the captured prelude. If the connection is not TLS or the
// handshake is malformed, an error is returned.
func PeekSNIAndPrelude(conn net.Conn, timeout time.Duration, maxPrelude int) (string, []byte, error) {
	rc := &readRecorder{Conn: conn, max: maxPrelude}
	// Apply a short deadline to protect against slowloris-style handshakes.
	_ = rc.SetReadDeadline(time.Now().Add(timeout))
	defer rc.SetReadDeadline(time.Time{})

	var sni string
	tlsConn := tls.Server(rc, &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			if chi != nil && chi.ServerName != "" {
				sni = strings.ToLower(chi.ServerName)
			}
			// Abort before any server writes occur.
			return nil, errAbortHandshake
		},
	})

	// Trigger the handshake enough to read the ClientHello.
	if err := tlsConn.Handshake(); err != nil {
		prelude := make([]byte, rc.buf.Len())
		copy(prelude, rc.buf.Bytes())
		if !errors.Is(err, errAbortHandshake) {
			// Not a TLS handshake or malformed; return captured bytes so caller can
			// reinsert for other protocol sniffing (e.g., HTTP).
			return "", prelude, err
		}
		// errAbortHandshake is expected when we intentionally stop after ClientHello.
	}
	prelude := make([]byte, rc.buf.Len())
	copy(prelude, rc.buf.Bytes())
	if sni == "" {
		return "", prelude, errors.New("missing sni in clienthello")
	}
	return sni, prelude, nil
}
