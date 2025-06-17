package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
)

var ErrNotHTTPRequest = errors.New("stream does not contain a valid HTTP request")

func PeekHost(conn *PeekableConn) (host string, finalStream io.Reader, err error) {

	var capturedBytes bytes.Buffer
	teeReader := io.TeeReader(conn.reader, &capturedBytes)

	bufferedStream := bufio.NewReader(teeReader)

	req, err := http.ReadRequest(bufferedStream)
	if err != nil {
		return "", io.MultiReader(&capturedBytes, conn.reader), ErrNotHTTPRequest
	}

	targetHost := req.Host
	if targetHost == "" {
		targetHost = req.URL.Host
	}

	if h, _, err := net.SplitHostPort(targetHost); err == nil {
		targetHost = h
	}

	return targetHost, io.MultiReader(&capturedBytes, conn.reader), nil
}
