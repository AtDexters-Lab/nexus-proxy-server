package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

var ErrNotHTTP = errors.New("not an http/1.x request")

// PeekHTTPHostAndPrelude uses net/http to parse the request line and headers
// to extract the Host and Path, while teeing all bytes read so they can be
// replayed to the backend as a prelude. It limits time via deadline; maximum
// header size is enforced by http.ReadRequest and the outer deadline.
func PeekHTTPHostAndPrelude(conn net.Conn, timeout time.Duration, _ int) (host string, path string, prelude []byte, err error) {
	// Apply deadline to protect against slow headers.
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})

	var captured bytes.Buffer
	tee := io.TeeReader(conn, &captured)
	br := bufio.NewReader(tee)

	req, rerr := http.ReadRequest(br)
	if rerr != nil {
		// If nothing captured, it likely isn't HTTP.
		if captured.Len() == 0 {
			return "", "", nil, ErrNotHTTP
		}
		prelude = make([]byte, captured.Len())
		copy(prelude, captured.Bytes())
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
	host = normalizeHostname(host)

	if req.URL != nil {
		path = req.URL.Path
	}
	if path == "" {
		path = "/"
	}

	prelude = make([]byte, captured.Len())
	copy(prelude, captured.Bytes())
	return host, path, prelude, nil
}
