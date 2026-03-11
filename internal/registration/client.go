package registration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
)

const (
	httpTimeout    = 15 * time.Second
	minBackoff     = 5 * time.Second
	maxBackoff     = 2 * time.Minute
	minHeartbeat   = 5 * time.Second
	jitterFraction = 0.2
)

type registerRequest struct {
	Region string `json:"region,omitempty"`
}

type registerResponse struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// Client periodically registers this Nexus instance with an orchestrator.
type Client struct {
	registrationURL string
	region          string
	httpClient      *http.Client

	// internalCtx/cancel provides a cancellation signal owned by this Client.
	// Stop() cancels it; Run() merges it with the parent context.
	internalCtx context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewClient creates a registration client. The hub TLS config is reused for
// mTLS client authentication, following the same pattern as peer connections.
func NewClient(cfg *config.Config, hubTlsConfig *tls.Config) (*Client, error) {
	clientTLS := &tls.Config{}

	// Reuse hub cert for mTLS client auth (same logic as peer.go:52-76).
	if cfg.HubPublicHostname != "" && hubTlsConfig.GetCertificate != nil {
		hostname := cfg.HubPublicHostname
		clientTLS.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return hubTlsConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: hostname})
		}
	} else if len(hubTlsConfig.Certificates) > 0 {
		clientTLS.Certificates = hubTlsConfig.Certificates
	}

	// Custom CA for verifying the orchestrator's server cert.
	if cfg.RegistrationCACertFile != "" {
		caCert, err := os.ReadFile(cfg.RegistrationCACertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read registration CA cert %s: %w", cfg.RegistrationCACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("registration CA cert file contains no valid certificates: %s", cfg.RegistrationCACertFile)
		}
		clientTLS.RootCAs = pool
	}

	internalCtx, cancel := context.WithCancel(context.Background())

	c := &Client{
		registrationURL: cfg.RegistrationURL,
		region:          cfg.Region,
		httpClient: &http.Client{
			Timeout:   httpTimeout,
			Transport: &http.Transport{TLSClientConfig: clientTLS},
		},
		internalCtx: internalCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}

	if cfg.HubPublicHostname != "" {
		log.Printf("INFO: [REGISTRATION] Client configured: url=%s hostname=%s", c.registrationURL, cfg.HubPublicHostname)
	} else {
		log.Printf("INFO: [REGISTRATION] Client configured: url=%s cert=%s", c.registrationURL, cfg.HubTlsCertFile)
	}
	return c, nil
}

// Run starts the registration loop. It blocks until cancelled or a permanent
// error occurs. Must be called before Stop.
func (c *Client) Run(parentCtx context.Context) {
	defer close(c.done)

	// Merge parent context with internal cancel so either can stop the loop.
	ctx, localCancel := context.WithCancel(parentCtx)
	defer localCancel()
	stop := context.AfterFunc(c.internalCtx, localCancel)
	defer stop()

	log.Printf("INFO: [REGISTRATION] Starting registration loop to %s", c.registrationURL)

	// Initial registration with exponential backoff.
	interval, err := c.registerWithBackoff(ctx)
	if err != nil {
		return // permanent error or context cancelled; already logged
	}

	// Steady-state heartbeat loop.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("INFO: [REGISTRATION] Shutting down")
			return
		case <-ticker.C:
			newInterval, err := c.register(ctx)
			if err != nil {
				if isPermanent(err) {
					log.Printf("ERROR: [REGISTRATION] Permanent error during heartbeat: %v", err)
					return
				}
				log.Printf("WARN: [REGISTRATION] Heartbeat failed: %v — entering backoff", err)
				recovered, err := c.registerWithBackoff(ctx)
				if err != nil {
					return
				}
				newInterval = recovered
			}
			if newInterval != interval {
				log.Printf("INFO: [REGISTRATION] Heartbeat interval changed: %s → %s", interval, newInterval)
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

// Stop cancels the registration loop and waits for it to exit.
func (c *Client) Stop() {
	c.cancel()
	<-c.done
	c.httpClient.CloseIdleConnections()
}

// register performs a single registration attempt. Returns the heartbeat
// interval on success or a classified error.
func (c *Client) register(ctx context.Context) (time.Duration, error) {
	body, _ := json.Marshal(registerRequest{Region: c.region})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registrationURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, &retryableError{err: fmt.Errorf("HTTP request failed: %w", err)}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusOK:
		var result registerResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return 0, &retryableError{err: fmt.Errorf("malformed response body: %w", err)}
		}
		interval := time.Duration(result.HeartbeatInterval) * time.Second
		if interval < minHeartbeat {
			log.Printf("WARN: [REGISTRATION] Heartbeat interval %s below minimum, clamping to %s", interval, minHeartbeat)
			interval = minHeartbeat
		}
		log.Printf("INFO: [REGISTRATION] Registered successfully, heartbeat interval: %s", interval)
		return interval, nil

	case http.StatusTooManyRequests:
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return 0, &retryableError{
			err:        fmt.Errorf("429 Too Many Requests: %s", respBody),
			retryAfter: retryAfter,
		}

	case http.StatusBadRequest, http.StatusUnauthorized:
		log.Printf("ERROR: [REGISTRATION] Permanent failure (HTTP %d): %s", resp.StatusCode, respBody)
		return 0, &permanentError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)}

	case http.StatusServiceUnavailable:
		log.Printf("WARN: [REGISTRATION] Orchestrator unavailable (HTTP 503): %s", respBody)
		return 0, &retryableError{err: fmt.Errorf("HTTP 503: %s", respBody)}

	default:
		log.Printf("WARN: [REGISTRATION] Unexpected status %d: %s", resp.StatusCode, respBody)
		return 0, &retryableError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)}
	}
}

// registerWithBackoff retries registration with exponential backoff until
// success, permanent error, or context cancellation.
func (c *Client) registerWithBackoff(ctx context.Context) (time.Duration, error) {
	backoff := minBackoff

	for {
		interval, err := c.register(ctx)
		if err == nil {
			return interval, nil
		}

		if ctx.Err() != nil {
			log.Println("INFO: [REGISTRATION] Context cancelled during backoff")
			return 0, ctx.Err()
		}

		if isPermanent(err) {
			log.Printf("ERROR: [REGISTRATION] Permanent error: %v", err)
			return 0, err
		}

		// Use Retry-After if provided by 429.
		wait := backoff
		if re, ok := err.(*retryableError); ok && re.retryAfter > 0 {
			wait = re.retryAfter
		}
		wait = addJitter(wait)

		log.Printf("WARN: [REGISTRATION] Retrying in %s: %v", wait, err)

		select {
		case <-ctx.Done():
			log.Println("INFO: [REGISTRATION] Context cancelled during backoff")
			return 0, ctx.Err()
		case <-time.After(wait):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}

// Error types for classification.

type permanentError struct{ err error }

func (e *permanentError) Error() string  { return e.err.Error() }
func (e *permanentError) Unwrap() error  { return e.err }

type retryableError struct {
	err        error
	retryAfter time.Duration
}

func (e *retryableError) Error() string  { return e.err.Error() }
func (e *retryableError) Unwrap() error  { return e.err }

func isPermanent(err error) bool {
	_, ok := err.(*permanentError)
	return ok
}

func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func addJitter(d time.Duration) time.Duration {
	jitter := time.Duration(float64(d) * jitterFraction * (2*rand.Float64() - 1))
	return d + jitter
}
