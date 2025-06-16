package proxy

import (
	"bufio"
	"fmt"
	"net"
	"strings"
)

// PeekHost reads from the PeekableConn to parse the Host header from an HTTP request.
func PeekHost(conn *PeekableConn) (string, error) {
	reader := bufio.NewReader(conn)

	// Read request line
	_, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read http request line: %w", err)
	}

	// Read headers
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("failed to read http headers: %w", err)
		}

		if line == "\r\n" {
			break
		}

		if strings.HasPrefix(strings.ToLower(line), "host:") {
			host := strings.TrimSpace(line[5:])
			// In case the host includes a port, strip it.
			if strings.Contains(host, ":") {
				host, _, err = net.SplitHostPort(host)
				if err != nil {
					return "", fmt.Errorf("invalid host header format: %s", host)
				}
			}
			// Return the full hostname.
			return host, nil
		}
	}

	return "", fmt.Errorf("host header not found")
}
