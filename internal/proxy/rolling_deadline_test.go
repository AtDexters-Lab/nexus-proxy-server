package proxy_test

import (
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	proxy "github.com/AtDexters-Lab/nexus-proxy/internal/proxy"
	"github.com/stretchr/testify/require"
)

// TestPeekSNIAndPrelude_SilentClientFailsAtFirstByte asserts the first-byte
// deadline fires cleanly for a client that connects but never writes.
func TestPeekSNIAndPrelude_SilentClientFailsAtFirstByte(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	firstByte := 200 * time.Millisecond
	start := time.Now()
	sni, _, err := proxy.PeekSNIAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     firstByte,
		IdleExtension: 50 * time.Millisecond,
		AbsDeadline:   time.Now().Add(2 * time.Second),
	}, 32<<10)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Empty(t, sni)
	// Must fire near firstByte, not absDeadline.
	require.Less(t, elapsed, firstByte+200*time.Millisecond)
	// And must actually wait for firstByte rather than returning instantly.
	require.GreaterOrEqual(t, elapsed, firstByte-50*time.Millisecond)
}

// TestPeekSNIAndPrelude_SlowFirstByteStillSucceeds asserts that a legitimate
// client which pauses for several seconds before emitting the ClientHello
// (e.g., ESP32 doing ECDHE keygen) is still routed correctly as long as the
// first-byte window covers the pause.
func TestPeekSNIAndPrelude_SlowFirstByteStillSucceeds(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer client.Close()

	pause := 800 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(pause)
		tlsClient := tls.Client(client, &tls.Config{
			ServerName:         "slow.example.com",
			InsecureSkipVerify: true,
		})
		_ = tlsClient.Handshake()
	}()

	sni, prelude, err := proxy.PeekSNIAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     2 * time.Second,
		IdleExtension: 500 * time.Millisecond,
		AbsDeadline:   time.Now().Add(3 * time.Second),
	}, 64<<10)
	_ = server.Close()
	<-done

	require.NoError(t, err)
	require.Equal(t, "slow.example.com", sni)
	require.NotEmpty(t, prelude)
}

// TestPeekSNIAndPrelude_DripFeedCappedAtAbsDeadline asserts that a client
// dripping one byte at a time cannot hold the peek open past AbsDeadline,
// even though every byte resets the idle extension.
func TestPeekSNIAndPrelude_DripFeedCappedAtAbsDeadline(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()

	// Send a valid TLS record header claiming a large body (16384 bytes),
	// then drip body bytes slowly. The parser will keep reading the body
	// until AbsDeadline fires.
	dripInterval := 20 * time.Millisecond
	go func() {
		defer client.Close()
		// Record: type=0x16 (handshake), version=0x0303 (TLS 1.2), length=0x4000 (16384).
		_, _ = client.Write([]byte{0x16, 0x03, 0x03, 0x40, 0x00})
		for i := 0; i < 10000; i++ {
			time.Sleep(dripInterval)
			if _, err := client.Write([]byte{0x00}); err != nil {
				return
			}
		}
	}()

	absMax := 1500 * time.Millisecond
	start := time.Now()
	_, _, err := proxy.PeekSNIAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     absMax,
		IdleExtension: 1 * time.Second, // generous; cap should win
		AbsDeadline:   time.Now().Add(absMax),
	}, 32<<10)
	elapsed := time.Since(start)

	require.Error(t, err)
	// Must terminate at or near the absolute ceiling, not be stretched.
	require.Less(t, elapsed, absMax+400*time.Millisecond)
	require.GreaterOrEqual(t, elapsed, absMax-200*time.Millisecond)
}

// TestPeekSNIAndPrelude_FastClientBurst asserts the happy path — a fast client
// sending the entire ClientHello in one burst completes quickly.
func TestPeekSNIAndPrelude_FastClientBurst(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		tlsClient := tls.Client(client, &tls.Config{
			ServerName:         "burst.example.com",
			InsecureSkipVerify: true,
		})
		_ = tlsClient.Handshake()
	}()

	start := time.Now()
	sni, _, err := proxy.PeekSNIAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     5 * time.Second,
		IdleExtension: 1 * time.Second,
		AbsDeadline:   time.Now().Add(10 * time.Second),
	}, 64<<10)
	elapsed := time.Since(start)
	_ = server.Close()
	<-done

	require.NoError(t, err)
	require.Equal(t, "burst.example.com", sni)
	require.Less(t, elapsed, 500*time.Millisecond)
}

// TestPeekHTTPHostAndPrelude_BufioBoundaryPause asserts that a legitimate
// HTTP client sending the request line in one packet, pausing briefly, then
// completing the headers still succeeds. This covers the bufio/TeeReader
// composition flagged by the red-team review: if IdleExtension is too tight,
// a pause between the request line and headers causes the second bufio fill
// to time out.
func TestPeekHTTPHostAndPrelude_BufioBoundaryPause(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer client.Close()

	pause := 300 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = client.Write([]byte("GET /path HTTP/1.1\r\n"))
		time.Sleep(pause)
		_, _ = client.Write([]byte("Host: bufio.example.com\r\n\r\n"))
	}()

	host, path, prelude, err := proxy.PeekHTTPHostAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     2 * time.Second,
		IdleExtension: 1 * time.Second, // must exceed pause
		AbsDeadline:   time.Now().Add(5 * time.Second),
	}, 64<<10)
	_ = server.Close()
	<-done

	require.NoError(t, err)
	require.Equal(t, "bufio.example.com", host)
	require.Equal(t, "/path", path)
	require.NotEmpty(t, prelude)
}

// TestPeekHTTPHostAndPrelude_DripFeedCappedAtAbsDeadline asserts that
// drip-feeding HTTP headers is terminated by AbsDeadline regardless of idle
// extension resets.
func TestPeekHTTPHostAndPrelude_DripFeedCappedAtAbsDeadline(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()

	go func() {
		defer client.Close()
		_, _ = client.Write([]byte("GET / HTTP/1.1\r\n"))
		// Drip junk header bytes forever. Never emit the terminating CRLF.
		junk := []byte("X")
		for i := 0; i < 200; i++ {
			time.Sleep(100 * time.Millisecond)
			if _, err := client.Write(junk); err != nil {
				return
			}
		}
	}()

	absMax := 1500 * time.Millisecond
	start := time.Now()
	_, _, _, err := proxy.PeekHTTPHostAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     absMax,
		IdleExtension: 1 * time.Second,
		AbsDeadline:   time.Now().Add(absMax),
	}, 64<<10)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, absMax+400*time.Millisecond)
	require.GreaterOrEqual(t, elapsed, absMax-200*time.Millisecond)
}

// TestPeekSNIAndPrelude_WithPreludeFallthrough asserts that the TLS-peek →
// HTTP-sniff fallthrough (malformed TLS, HTTP retry with WithPrelude replay)
// respects the shared AbsDeadline — the caller cannot extract two independent
// peek budgets from one connection.
func TestPeekSNIAndPrelude_WithPreludeFallthrough(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()

	httpReq := []byte("GET /fall HTTP/1.1\r\nHost: fall.example.com\r\n\r\n")
	go func() {
		defer client.Close()
		_, _ = client.Write(httpReq)
	}()

	absDeadline := time.Now().Add(1 * time.Second)
	peek := proxy.PeekTimeouts{
		FirstByte:     500 * time.Millisecond,
		IdleExtension: 200 * time.Millisecond,
		AbsDeadline:   absDeadline,
	}

	// TLS peek fails fast because first byte is 'G', not 0x16.
	_, tlsPrelude, tlsErr := proxy.PeekSNIAndPrelude(server, peek, 32<<10)
	require.Error(t, tlsErr)

	// Restore captured bytes and re-peek as HTTP with the SAME peek config
	// (sharing AbsDeadline). This is what listener.go does.
	conn := proxy.WithPrelude(server, tlsPrelude)
	host, _, _, httpErr := proxy.PeekHTTPHostAndPrelude(conn, peek, 64<<10)

	require.NoError(t, httpErr)
	require.Equal(t, "fall.example.com", host)
	require.True(t, time.Now().Before(absDeadline.Add(500*time.Millisecond)),
		"fallthrough must not extend past shared AbsDeadline")
}

// TestRollingDeadlineConn_DrainFromBufferDoesNotLeakPastAbsDeadline uses a
// mock conn to verify the absolute ceiling is a hard cap independent of how
// many n>0 reads occur.
func TestRollingDeadlineConn_DrainFromBufferDoesNotLeakPastAbsDeadline(t *testing.T) {
	t.Parallel()

	// 100 bytes of payload followed by a blocking reader. Each Read returns 1
	// byte. Over AbsMax = 500ms the rolling wrapper should read as much as it
	// can but the deadline cannot be extended past AbsMax.
	payload := strings.Repeat("A", 100)
	slow := &byteAtATimeReader{data: []byte(payload)}

	absMax := 500 * time.Millisecond
	server, client := net.Pipe()
	defer client.Close()

	go func() {
		defer server.Close()
		_, _ = io.Copy(server, slow)
	}()

	start := time.Now()
	// Apply a peek; we don't care about the result, just that it bounds.
	_, _, _ = proxy.PeekSNIAndPrelude(client, proxy.PeekTimeouts{
		FirstByte:     absMax,
		IdleExtension: 10 * time.Second,
		AbsDeadline:   time.Now().Add(absMax),
	}, 64<<10)
	elapsed := time.Since(start)

	require.Less(t, elapsed, absMax+400*time.Millisecond,
		"AbsDeadline must bound peek duration regardless of read activity")
}

// byteAtATimeReader is a test helper returning one byte per Read.
type byteAtATimeReader struct {
	data []byte
	pos  int
}

func (b *byteAtATimeReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		// Block "forever" (caller will close the pipe on timeout).
		time.Sleep(10 * time.Second)
		return 0, io.EOF
	}
	p[0] = b.data[b.pos]
	b.pos++
	time.Sleep(50 * time.Millisecond)
	return 1, nil
}
