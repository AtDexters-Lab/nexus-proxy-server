package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	hn "github.com/AtDexters-Lab/nexus-proxy-server/internal/hostnames"
)

var (
	ErrNotHTTP             = errors.New("not an http/1.x request")
	ErrHTTPPreludeTooLarge = errors.New("http prelude exceeds limit")
)

type limitedCapture struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func newLimitedCapture(limit int) *limitedCapture {
	return &limitedCapture{limit: limit}
}

func (c *limitedCapture) Write(p []byte) (int, error) {
	if c.limit <= 0 {
		return c.buf.Write(p)
	}
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.overflow = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *limitedCapture) copyBytes() []byte {
	if c.buf.Len() == 0 {
		return nil
	}
	out := make([]byte, c.buf.Len())
	copy(out, c.buf.Bytes())
	return out
}

// PeekHTTPHostAndPrelude uses net/http to parse the request line and headers
// to extract the Host and Path, while teeing all bytes read so they can be
// replayed to the backend as a prelude. It limits time via deadline; maximum
// header size is enforced by http.ReadRequest and the outer deadline.
func PeekHTTPHostAndPrelude(conn net.Conn, timeout time.Duration, maxPrelude int) (host string, path string, prelude []byte, err error) {
	// Apply deadline to protect against slow headers.
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})

	captured := newLimitedCapture(maxPrelude)
	tee := io.TeeReader(conn, captured)
	br := bufio.NewReader(tee)

	req, rerr := http.ReadRequest(br)
	prelude = captured.copyBytes()
	if captured.overflow {
		return "", "", prelude, ErrHTTPPreludeTooLarge
	}
	if rerr != nil {
		// If nothing captured, it likely isn't HTTP.
		if len(prelude) == 0 {
			return "", "", nil, ErrNotHTTP
		}
		return "", "", prelude, rerr
	}

	host = req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	if host == "" {
		return "", "", nil, errors.New("host header not found")
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = hn.Normalize(host)

	if req.URL != nil {
		path = req.URL.Path
	}
	if path == "" {
		path = "/"
	}

	return host, path, prelude, nil
}
