package registration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generateTestCerts creates a self-signed CA and leaf cert for testing.
func generateTestCerts(t *testing.T) (tlsCert tls.Certificate, caPool *x509.CertPool) {
	t.Helper()

	// Generate CA key and cert.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	caCert, err := x509.ParseCertificate(caCertDER)
	require.NoError(t, err)

	caPool = x509.NewCertPool()
	caPool.AddCert(caCert)

	// Generate leaf key and cert.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "nexus-test.example.com"},
		DNSNames:     []string{"nexus-test.example.com", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafCertDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	leafCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafCertDER})
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	require.NoError(t, err)
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})

	tlsCert, err = tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	require.NoError(t, err)

	return tlsCert, caPool
}

// newMTLSTestServer creates an httptest server requiring mTLS.
func newMTLSTestServer(t *testing.T, caPool *x509.CertPool, serverCert tls.Certificate, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// newTestClient creates a Client with a test HTTP client configured for mTLS.
func newTestClient(t *testing.T, cfg *config.Config, cert tls.Certificate, caPool *x509.CertPool) *Client {
	t.Helper()
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caPool,
			},
		},
	}
	client, err := NewClient(cfg, httpClient)
	require.NoError(t, err)
	return client
}

func TestRegister_Success(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	var requestCount atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var body registerRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "us-west-2", body.Region)
		assert.Equal(t, 8443, body.BackendPort)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(registerResponse{HeartbeatInterval: 30})
	})

	server := newMTLSTestServer(t, caPool, cert, handler)

	cfg := &config.Config{
		BackendListenAddress: ":8443",
		RegistrationURL:      server.URL,
		Region:               "us-west-2",
	}
	client := newTestClient(t, cfg, cert, caPool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interval, err := client.register(ctx)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, interval)
	assert.Equal(t, int32(1), requestCount.Load())
}

func TestRegister_PermanentErrors(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	tests := []struct {
		name   string
		status int
	}{
		{"400 Bad Request", http.StatusBadRequest},
		{"401 Unauthorized", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "error", tt.status)
			})
			server := newMTLSTestServer(t, caPool, cert, handler)

			cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
			client := newTestClient(t, cfg, cert, caPool)

			_, err := client.register(context.Background())
			require.Error(t, err)
			assert.True(t, isPermanent(err), "expected permanent error for %d", tt.status)
		})
	}
}

func TestRegister_RetryableErrors(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	t.Run("500 Internal Server Error", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		})
		server := newMTLSTestServer(t, caPool, cert, handler)

		cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
		client := newTestClient(t, cfg, cert, caPool)

		_, err := client.register(context.Background())
		require.Error(t, err)
		assert.False(t, isPermanent(err))
	})

	t.Run("429 with Retry-After", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		})
		server := newMTLSTestServer(t, caPool, cert, handler)

		cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
		client := newTestClient(t, cfg, cert, caPool)

		_, err := client.register(context.Background())
		require.Error(t, err)
		assert.False(t, isPermanent(err))
		re, ok := err.(*retryableError)
		require.True(t, ok)
		assert.Equal(t, 10*time.Second, re.retryAfter)
	})

	t.Run("503 Service Unavailable", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nexus auth not configured", http.StatusServiceUnavailable)
		})
		server := newMTLSTestServer(t, caPool, cert, handler)

		cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
		client := newTestClient(t, cfg, cert, caPool)

		_, err := client.register(context.Background())
		require.Error(t, err)
		assert.False(t, isPermanent(err), "503 should be retryable")
	})

	t.Run("200 with malformed body", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		})
		server := newMTLSTestServer(t, caPool, cert, handler)

		cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
		client := newTestClient(t, cfg, cert, caPool)

		_, err := client.register(context.Background())
		require.Error(t, err)
		assert.False(t, isPermanent(err))
	})
}

func TestRegister_HeartbeatClamp(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(registerResponse{HeartbeatInterval: 1})
	})
	server := newMTLSTestServer(t, caPool, cert, handler)

	cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
	client := newTestClient(t, cfg, cert, caPool)

	interval, err := client.register(context.Background())
	require.NoError(t, err)
	assert.Equal(t, minHeartbeat, interval)
}

func TestRegister_EmptyRegion(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, hasRegion := body["region"]
		assert.False(t, hasRegion, "empty region should be omitted via omitempty")
		assert.Equal(t, float64(8443), body["backendPort"], "backendPort must always be present")

		json.NewEncoder(w).Encode(registerResponse{HeartbeatInterval: 30})
	})
	server := newMTLSTestServer(t, caPool, cert, handler)

	cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
	client := newTestClient(t, cfg, cert, caPool)

	_, err := client.register(context.Background())
	require.NoError(t, err)
}

func TestRunAndStop(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	var heartbeats atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heartbeats.Add(1)
		// Heartbeat interval will be clamped to minHeartbeat (5s).
		json.NewEncoder(w).Encode(registerResponse{HeartbeatInterval: 1})
	})
	server := newMTLSTestServer(t, caPool, cert, handler)

	cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
	client := newTestClient(t, cfg, cert, caPool)

	ctx := context.Background()
	go client.Run(ctx)

	// Wait enough for initial registration + at least one heartbeat (clamped to 5s).
	time.Sleep(minHeartbeat + 3*time.Second)

	client.Stop()

	count := heartbeats.Load()
	assert.GreaterOrEqual(t, count, int32(2), "expected at least initial registration + 1 heartbeat")
}

func TestRunPermanentError(t *testing.T) {
	cert, caPool := generateTestCerts(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	server := newMTLSTestServer(t, caPool, cert, handler)

	cfg := &config.Config{BackendListenAddress: ":8443", RegistrationURL: server.URL}
	client := newTestClient(t, cfg, cert, caPool)

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(done)
	}()

	// Run should exit quickly on permanent error.
	select {
	case <-done:
		// OK — exited due to permanent error.
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit on permanent error")
	}
}

func TestNewClient_InvalidBackendPort(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr string
	}{
		{"missing port", "no-port", "failed to parse port"},
		{"non-numeric port", ":abc", "invalid port"},
		{"port zero", ":0", "out of valid range"},
		{"port too large", ":99999", "out of valid range"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				BackendListenAddress: tt.addr,
				RegistrationURL:     "https://example.com/register",
			}
			_, err := NewClient(cfg, &http.Client{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	assert.Equal(t, 10*time.Second, parseRetryAfter("10"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("not-a-number"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-5"))
}
