package client

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestSocks5Handshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- socks5Handshake(server) }()

	// Send method selection: version 5, 1 method, no-auth
	_, _ = client.Write([]byte{0x05, 0x01, 0x00})

	// Read server response first (net.Pipe is synchronous).
	var resp [2]byte
	if _, err := client.Read(resp[:]); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("unexpected response: %x", resp)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
}

func TestSocks5HandshakeRejectsNoAuth(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- socks5Handshake(server) }()

	// Only offer username/password auth (0x02), not no-auth
	_, _ = client.Write([]byte{0x05, 0x01, 0x02})

	// Read the rejection reply so the write doesn't block.
	var resp [2]byte
	_, _ = client.Read(resp[:])

	err := <-errCh
	if err == nil {
		t.Fatal("expected error for unsupported auth method")
	}
}

func TestSocks5ReadConnectDomain(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	type result struct {
		host string
		port int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		h, p, e := socks5ReadConnect(server)
		resCh <- result{h, p, e}
	}()

	// CONNECT to example.com:443
	domain := []byte("example.com")
	var buf bytes.Buffer
	buf.Write([]byte{0x05, 0x01, 0x00, 0x03}) // ver, cmd=connect, rsv, atyp=domain
	buf.WriteByte(byte(len(domain)))
	buf.Write(domain)
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], 443)
	buf.Write(portBuf[:])
	_, _ = client.Write(buf.Bytes())

	res := <-resCh
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.host != "example.com" {
		t.Fatalf("expected host example.com, got %s", res.host)
	}
	if res.port != 443 {
		t.Fatalf("expected port 443, got %d", res.port)
	}
}

func TestSocks5ReadConnectIPv4(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	type result struct {
		host string
		port int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		h, p, e := socks5ReadConnect(server)
		resCh <- result{h, p, e}
	}()

	// CONNECT to 1.2.3.4:80
	var buf bytes.Buffer
	buf.Write([]byte{0x05, 0x01, 0x00, 0x01}) // ver, cmd=connect, rsv, atyp=ipv4
	buf.Write([]byte{1, 2, 3, 4})
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], 80)
	buf.Write(portBuf[:])
	_, _ = client.Write(buf.Bytes())

	res := <-resCh
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.host != "1.2.3.4" {
		t.Fatalf("expected host 1.2.3.4, got %s", res.host)
	}
	if res.port != 80 {
		t.Fatalf("expected port 80, got %d", res.port)
	}
}

func TestSocks5ReadConnectIPv6(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	type result struct {
		host string
		port int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		h, p, e := socks5ReadConnect(server)
		resCh <- result{h, p, e}
	}()

	// CONNECT to [::1]:25
	var buf bytes.Buffer
	buf.Write([]byte{0x05, 0x01, 0x00, 0x04}) // ver, cmd=connect, rsv, atyp=ipv6
	ipv6 := net.ParseIP("::1").To16()
	buf.Write(ipv6)
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], 25)
	buf.Write(portBuf[:])
	_, _ = client.Write(buf.Bytes())

	res := <-resCh
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.host != "::1" {
		t.Fatalf("expected host ::1, got %s", res.host)
	}
	if res.port != 25 {
		t.Fatalf("expected port 25, got %d", res.port)
	}
}

func TestSocks5ReadConnectUnsupportedCmd(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	type result struct {
		host string
		port int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		h, p, e := socks5ReadConnect(server)
		resCh <- result{h, p, e}
	}()

	// BIND command (0x02) instead of CONNECT — send only the header
	// (4 bytes) since socks5ReadConnect reads exactly 4 bytes before
	// checking the command and sending the error reply.
	go func() { _, _ = client.Write([]byte{0x05, 0x02, 0x00, 0x01, 1, 2, 3, 4, 0, 80}) }()

	// Read the error reply (command not supported).
	var resp [10]byte
	_, _ = client.Read(resp[:])

	res := <-resCh
	if res.err == nil {
		t.Fatal("expected error for unsupported command")
	}
	if resp[1] != socks5RepCmdNotSupported {
		t.Fatalf("expected rep=0x07, got 0x%02x", resp[1])
	}
}

func TestSocks5SendReply(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- socks5SendReply(server, socks5RepSuccess) }()

	var buf [10]byte
	n, err := client.Read(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("expected 10 bytes, got %d", n)
	}
	if buf[0] != 0x05 {
		t.Fatalf("expected version 5, got %d", buf[0])
	}
	if buf[1] != socks5RepSuccess {
		t.Fatalf("expected success reply, got %d", buf[1])
	}

	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestSocks5TargetAddr(t *testing.T) {
	tests := []struct {
		host string
		port int
		want string
	}{
		{"example.com", 443, "example.com:443"},
		{"1.2.3.4", 80, "1.2.3.4:80"},
		{"::1", 25, "[::1]:25"},
	}
	for _, tt := range tests {
		got := socks5TargetAddr(tt.host, tt.port)
		if got != tt.want {
			t.Errorf("socks5TargetAddr(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
		}
	}
}
