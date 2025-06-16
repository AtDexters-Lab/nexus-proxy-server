package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
)

var ErrNotHTTPRequest = errors.New("stream does not contain a valid HTTP request")

func PeekHost(conn *PeekableConn) (host string, finalStream io.Reader, err error) {
	if conn == nil || conn.reader == nil {
		return "", nil, errors.New("PeekableConn or its reader is nil")
	}

	var capturedBytes bytes.Buffer
	teeReader := io.TeeReader(conn.reader, &capturedBytes)

	bufferedStream := bufio.NewReader(teeReader)

	req, err := http.ReadRequest(bufferedStream)
	if err != nil {
		log.Printf("DEBUG: Failed to read HTTP request: %v | %s", err, capturedBytes.String())
		return "", io.MultiReader(&capturedBytes, conn.reader), ErrNotHTTPRequest
	}

	targetHost := req.Host
	if targetHost == "" && req.URL != nil {
		targetHost = req.URL.Host
	}

	if targetHost == "" {
		return "", io.MultiReader(&capturedBytes, conn.reader), errors.New("could not determine target host")
	}

	if h, _, err := net.SplitHostPort(targetHost); err == nil {
		targetHost = h
	}

	return targetHost, io.MultiReader(&capturedBytes, conn.reader), nil
}
