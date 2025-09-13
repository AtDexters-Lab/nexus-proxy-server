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

func PeekHost(conn *PeekableConn) (host string, err error) {
	if conn == nil || conn.reader == nil {
		return "", errors.New("PeekableConn or its reader is nil")
	}

	var capturedBytes bytes.Buffer
	defer func() {
		conn.reader = io.MultiReader(&capturedBytes, conn.reader)
	}()

	teeReader := io.TeeReader(conn.reader, &capturedBytes)

	bufferedStream := bufio.NewReader(teeReader)

	req, err := http.ReadRequest(bufferedStream)
	if err != nil {
		log.Printf("DEBUG: Failed to read HTTP request: %v | %s", err, capturedBytes.String())
		return "", ErrNotHTTPRequest
	}

	targetHost := req.Host
	if targetHost == "" && req.URL != nil {
		targetHost = req.URL.Host
	}

	if targetHost == "" {
		return "", errors.New("could not determine target host")
	}

	if h, _, err := net.SplitHostPort(targetHost); err == nil {
		targetHost = h
	}

	return targetHost, nil
}
