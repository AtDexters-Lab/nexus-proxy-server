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

// ErrMissingSNI indicates a valid TLS ClientHello without an SNI value.
var ErrMissingSNI = errors.New("missing sni in clienthello")

// PeekTimeouts controls the deadline regime used by the protocol-sniff peek
// functions. The three-knob design accommodates slow-start clients (e.g.,
// embedded devices doing ECDHE keygen between TCP accept and ClientHello
// emission) while bounding total peek duration against slowloris-style
// holdtime attacks.
//
// Semantics:
//   - FirstByte sets the initial read deadline. A truly silent client fails
//     here. MUST be > 0; a zero-value PeekTimeouts{} will time out every
//     peek immediately. Callers should obtain a populated struct via the
//     config accessors (Config.PeekFirstByteTimeout etc.), never construct
//     directly with zero values.
//   - IdleExtension is added to the deadline on every successful kernel-level
//     Read (n > 0). Once bytes are flowing, the caller stays patient as long
//     as the client keeps producing bytes.
//   - AbsDeadline is a hard wall-clock ceiling. The deadline never extends
//     past it, regardless of activity. Callers share AbsDeadline across
//     multiple sequential peek phases (TLS then HTTP) so a single
//     connection's total peek time is bounded by one AbsDeadline, not two.
type PeekTimeouts struct {
	FirstByte     time.Duration
	IdleExtension time.Duration
	AbsDeadline   time.Time
}

// rollingDeadlineConn wraps a net.Conn and extends the read deadline on each
// successful (n > 0) Read, bounded by absDeadline. It is the single
// deadline-rolling primitive used by both the TLS peek and HTTP sniff paths.
// Capture of read bytes is intentionally a separate concern (readRecorder for
// TLS, limitedCapture/TeeReader for HTTP).
type rollingDeadlineConn struct {
	net.Conn
	idleExt     time.Duration
	absDeadline time.Time
}

func (r *rollingDeadlineConn) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	if n > 0 && r.idleExt > 0 {
		next := time.Now().Add(r.idleExt)
		if !r.absDeadline.IsZero() && next.After(r.absDeadline) {
			next = r.absDeadline
		}
		_ = r.Conn.SetReadDeadline(next)
	}
	return n, err
}

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

// Write discards any bytes the TLS stack attempts to emit while we are in
// peek mode. When GetConfigForClient returns an error, crypto/tls will try to
// send an alert to the client. We do not want that alert to leak onto the wire
// because the real backend handshake will continue once we finish sniffing.
// Pretending the write succeeded keeps the handshake state machine happy
// without letting any bytes escape.
func (r *readRecorder) Write(p []byte) (int, error) {
	return len(p), nil
}

// initialDeadline returns now+FirstByte clamped to AbsDeadline (if set).
func (t PeekTimeouts) initialDeadline() time.Time {
	d := time.Now().Add(t.FirstByte)
	if !t.AbsDeadline.IsZero() && d.After(t.AbsDeadline) {
		return t.AbsDeadline
	}
	return d
}

// PeekSNIAndPrelude reads only as much as needed to obtain the SNI from the
// incoming TLS ClientHello using crypto/tls, capturing the bytes that were read
// so they can be replayed to the chosen backend. It returns the server name
// (lowercased) and the captured prelude. If the connection is not TLS or the
// handshake is malformed, an error is returned.
func PeekSNIAndPrelude(conn net.Conn, timeouts PeekTimeouts, maxPrelude int) (string, []byte, error) {
	rolling := &rollingDeadlineConn{
		Conn:        conn,
		idleExt:     timeouts.IdleExtension,
		absDeadline: timeouts.AbsDeadline,
	}
	rc := &readRecorder{Conn: rolling, max: maxPrelude}

	_ = conn.SetReadDeadline(timeouts.initialDeadline())
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

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
		return "", prelude, ErrMissingSNI
	}
	return sni, prelude, nil
}
