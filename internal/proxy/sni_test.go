package proxy_test

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	proxy "github.com/AtDexters-Lab/nexus-proxy-server/internal/proxy"
	"github.com/stretchr/testify/require"
)

func TestPeekSNIAndPrelude_Success(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Initiate a TLS client handshake that will fail once server aborts,
		// but will send a ClientHello including SNI first.
		tlsClient := tls.Client(client, &tls.Config{
			ServerName:         "example.com",
			InsecureSkipVerify: true,
		})
		_ = tlsClient.Handshake()
	}()

	sni, prelude, err := proxy.PeekSNIAndPrelude(server, 2*time.Second, 64<<10)
	// Close the server side to unblock client goroutine.
	_ = server.Close()
	<-done

	require.NoError(t, err)
	require.Equal(t, "example.com", sni)
	require.NotEmpty(t, prelude)
	require.Equal(t, byte(0x16), prelude[0]) // TLS record type: handshake
}

func TestPeekSNIAndPrelude_MissingSNI(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tlsClient := tls.Client(client, &tls.Config{
			InsecureSkipVerify: true,
			// No ServerName -> no SNI extension
		})
		_ = tlsClient.Handshake()
	}()

	sni, prelude, err := proxy.PeekSNIAndPrelude(server, 2*time.Second, 64<<10)
	_ = server.Close()
	<-done

	require.Error(t, err)
	require.Empty(t, sni)
	require.NotEmpty(t, prelude) // We still captured the ClientHello
	require.Equal(t, byte(0x16), prelude[0])
}

func TestPeekSNIAndPrelude_NotTLS(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	// Write a plain HTTP request from the client side.
	httpData := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	go func() {
		client.Write(httpData)
		client.Close()
	}()

	sni, prelude, err := proxy.PeekSNIAndPrelude(server, 2*time.Second, 64<<10)
	_ = server.Close()

	require.Error(t, err)
	require.Empty(t, sni)
	require.NotEmpty(t, prelude)
	require.Equal(t, httpData[0], prelude[0])
}
