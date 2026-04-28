package proxy_test

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	proxy "github.com/AtDexters-Lab/nexus-proxy/internal/proxy"
	"github.com/stretchr/testify/require"
)

// TestPeekSNIAndPrelude_TruncationFlagSet feeds a real ClientHello against a
// recorder cap small enough to be hit during the handshake. Asserts that the
// truncation flag is set so the listener can act on it (close + WARN). SNI
// may or may not be extracted depending on how far the parser advanced
// before the cap; both outcomes carry the same operator-actionable signal.
func TestPeekSNIAndPrelude_TruncationFlagSet(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		tlsClient := tls.Client(client, &tls.Config{
			ServerName:         "example.com",
			InsecureSkipVerify: true,
		})
		_ = tlsClient.Handshake()
	}()

	// 64 bytes is well below a typical ClientHello (~250+ bytes) but large
	// enough for the parser to read the record header and begin processing.
	const tinyCap = 64
	_, _, truncated, _ := proxy.PeekSNIAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     2 * time.Second,
		IdleExtension: 500 * time.Millisecond,
		AbsDeadline:   time.Now().Add(2 * time.Second),
	}, tinyCap)
	_ = server.Close()
	<-done

	require.True(t, truncated, "expected truncated=true with cap %d below a typical ClientHello", tinyCap)
}

// TestPeekSNIAndPrelude_NoTruncationOnFitting asserts the negative: a cap
// large enough to hold the entire ClientHello does not set the truncation
// flag. Guards against a regression that would always-set the flag.
func TestPeekSNIAndPrelude_NoTruncationOnFitting(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		tlsClient := tls.Client(client, &tls.Config{
			ServerName:         "example.com",
			InsecureSkipVerify: true,
		})
		_ = tlsClient.Handshake()
	}()

	sni, _, truncated, err := proxy.PeekSNIAndPrelude(server, proxy.PeekTimeouts{
		FirstByte:     2 * time.Second,
		IdleExtension: 500 * time.Millisecond,
		AbsDeadline:   time.Now().Add(2 * time.Second),
	}, 64<<10)
	_ = server.Close()
	<-done

	require.NoError(t, err)
	require.Equal(t, "example.com", sni)
	require.False(t, truncated, "expected truncated=false with 64KB cap fitting a typical ClientHello")
}
