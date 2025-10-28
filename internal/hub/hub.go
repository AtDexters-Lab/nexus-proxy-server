package hub

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	hostn "github.com/AtDexters-Lab/nexus-proxy-server/internal/hostnames"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
	"github.com/gorilla/websocket"
)

const (
	authTimeout     = 10 * time.Second
	shutdownTimeout = 5 * time.Second
)

// hubImpl manages the lifecycle of backend and peer connections.
type hubImpl struct {
	config        *config.Config
	backendServer *http.Server // Listens for backend connections
	peerServer    *http.Server // Listens for peer connections (mTLS)
	upgrader      websocket.Upgrader
	pools         sync.Map // key: exact hostname (string) -> *LoadBalancerPool
	wildcardPools sync.Map // key: suffix like ".example.com" -> *LoadBalancerPool
	peerManager   iface.PeerManager
	hubTlsConfig  *tls.Config
	validator     auth.Validator
}

// New creates and returns a new Hub instance.
func New(cfg *config.Config, tlsConfig *tls.Config, validator auth.Validator) *hubImpl {
	return &hubImpl{
		config:       cfg,
		hubTlsConfig: tlsConfig,
		validator:    validator,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// SetPeerManager sets the peer manager for the hub. This is done after
// initialization to break the circular dependency.
func (h *hubImpl) SetPeerManager(pm iface.PeerManager) {
	h.peerManager = pm
}

// Run starts the Hub's HTTP servers for both backends and peers.
func (h *hubImpl) Run() {
	// --- Backend Server Setup (JWT Auth) ---
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/connect", h.handleBackendConnect)
	h.backendServer = &http.Server{
		Addr:      h.config.BackendListenAddress,
		Handler:   backendMux,
		TLSConfig: h.hubTlsConfig,
	}

	log.Printf("INFO: Backend Hub (JWT) listening on %s", h.config.BackendListenAddress)
	go func() {
		if err := h.backendServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("FATAL: Backend Hub failed to start: %v", err)
		}
	}()

	// --- Peer Server Setup (mTLS Auth) ---
	if len(h.config.Peers) > 0 {
		peerMux := http.NewServeMux()
		peerMux.HandleFunc("/mesh", h.handlePeerConnect)

		// Clone the base TLS config and add peer-specific mTLS settings.
		peerTlsConfig := h.hubTlsConfig.Clone()
		peerTlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		peerTlsConfig.VerifyPeerCertificate = h.buildVerifyPeerCertificateFunc()

		h.peerServer = &http.Server{
			Addr:      h.config.PeerListenAddress,
			Handler:   peerMux,
			TLSConfig: peerTlsConfig,
		}

		log.Printf("INFO: Peer Hub (mTLS) listening on %s", h.config.PeerListenAddress)
		go func() {
			if err := h.peerServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("FATAL: Peer Hub failed to start: %v", err)
			}
		}()
	}
}

// buildVerifyPeerCertificateFunc is a closure that creates the verification function.
// This function checks if a peer's certificate FQDN belongs to a trusted domain.
func (h *hubImpl) buildVerifyPeerCertificateFunc() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
			return fmt.Errorf("no verified certificate chain presented by peer")
		}

		// The first certificate in the first verified chain is the client's cert.
		cert := verifiedChains[0][0]

		// Check against all trusted domain suffixes
		for _, suffix := range h.config.PeerAuthentication.TrustedDomainSuffixes {
			// Check Subject Alternative Names first (preferred)
			for _, san := range cert.DNSNames {
				if strings.HasSuffix(san, suffix) {
					log.Printf("INFO: Peer certificate validated via SAN for: %s", san)
					return nil // Success
				}
			}
			// Fallback to checking Common Name
			if strings.HasSuffix(cert.Subject.CommonName, suffix) {
				log.Printf("INFO: Peer certificate validated via CN for: %s", cert.Subject.CommonName)
				return nil // Success
			}
		}

		// If no match was found
		return fmt.Errorf("peer certificate FQDN ('%s' / SANs: %v) is not in any trusted domain", cert.Subject.CommonName, cert.DNSNames)
	}
}

// Stop gracefully shuts down the Hub's HTTP servers.
func (h *hubImpl) Stop() {
	log.Println("INFO: Shutting down hub servers...")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if h.backendServer != nil {
			if err := h.backendServer.Shutdown(ctx); err != nil {
				log.Printf("WARN: Backend hub graceful shutdown failed: %v", err)
			} else {
				log.Println("INFO: Backend hub shut down gracefully.")
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if h.peerServer != nil {
			if err := h.peerServer.Shutdown(ctx); err != nil {
				log.Printf("WARN: Peer hub graceful shutdown failed: %v", err)
			} else {
				log.Println("INFO: Peer hub shut down gracefully.")
			}
		}
	}()

	wg.Wait()
}

// SelectBackend finds the load balancer pool for the given hostname.
func (h *hubImpl) SelectBackend(hostname string) (iface.Backend, error) {
	// 1) Exact match first
	if rawPool, ok := h.pools.Load(hostname); ok {
		return rawPool.(*LoadBalancerPool).Select()
	}
	// 2) Single-label wildcard fallback based on first-dot suffix
	if suffix, ok := hostn.FirstDotSuffix(hostname); ok {
		if rawPool, ok := h.wildcardPools.Load(suffix); ok {
			return rawPool.(*LoadBalancerPool).Select()
		}
	}
	return nil, fmt.Errorf("no backend pool available for hostname: %s", hostname)
}

// handleBackendConnect handles a connection from a backend service (JWT Auth).
func (h *hubImpl) handleBackendConnect(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ERROR: Failed to upgrade backend connection: %v", err)
		return
	}

	backend, err := h.performHandshake(conn)
	if err != nil {
		log.Printf("WARN: Backend authentication failed for %s: %v", conn.RemoteAddr(), err)
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()))
		conn.Close()
		return
	}

	log.Printf("INFO: Backend for hostnames %v authenticated successfully from %s", backend.hostnames, conn.RemoteAddr())

	h.register(backend)
	defer h.unregister(backend)

	backend.StartPumps()
}

// handlePeerConnect handles a connection from another Nexus node (mTLS Auth).
func (h *hubImpl) handlePeerConnect(w http.ResponseWriter, r *http.Request) {
	if h.peerManager == nil {
		log.Printf("ERROR: Peer manager not initialized, cannot handle peer connection.")
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	// The mTLS verification is handled by the server's TLSConfig before this handler is called.
	// If we've reached this point, the peer's certificate is already validated.
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ERROR: Failed to upgrade peer connection: %v", err)
		return
	}

	log.Printf("INFO: Inbound mTLS-authenticated peer connection established from %s", conn.RemoteAddr())
	h.peerManager.HandleInboundPeer(conn)
}

type challengeMessage struct {
	Type  string `json:"type"`
	Nonce string `json:"nonce"`
}

func (h *hubImpl) performHandshake(conn *websocket.Conn) (*Backend, error) {
	stage0Token, err := h.readTokenMessage(conn, "stage0")
	if err != nil {
		return nil, err
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), authTimeout)
	claims0, err := h.validator.Validate(ctx0, stage0Token)
	cancel0()
	if err != nil {
		return nil, fmt.Errorf("stage0 token validation failed: %w", err)
	}

	hostnames, weight, err := h.validateStage0Claims(claims0)
	if err != nil {
		return nil, err
	}

	nonce, err := generateNonce()
	if err != nil {
		return nil, fmt.Errorf("generate challenge nonce: %w", err)
	}

	if err := sendChallenge(conn, challengeMessage{Type: "handshake_challenge", Nonce: nonce}); err != nil {
		return nil, fmt.Errorf("send challenge: %w", err)
	}

	stage1Token, err := h.readTokenMessage(conn, "stage1")
	if err != nil {
		return nil, err
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), authTimeout)
	claims1, err := h.validator.Validate(ctx1, stage1Token)
	cancel1()
	if err != nil {
		return nil, fmt.Errorf("stage1 token validation failed: %w", err)
	}

	meta, err := h.buildMetadataFromClaims(claims1, hostnames, weight, nonce)
	if err != nil {
		return nil, err
	}

	return NewBackend(conn, meta, h.config, h.validator), nil
}

func (h *hubImpl) readTokenMessage(conn *websocket.Conn, stage string) (string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(authTimeout)); err != nil {
		return "", fmt.Errorf("set read deadline for %s: %w", stage, err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		return "", fmt.Errorf("read %s token: %w", stage, err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return "", fmt.Errorf("reset read deadline after %s: %w", stage, err)
	}
	if messageType != websocket.TextMessage {
		return "", fmt.Errorf("expected text frame for %s token, got type %d", stage, messageType)
	}
	return string(payload), nil
}

func (h *hubImpl) validateStage0Claims(claims *auth.Claims) ([]string, int, error) {
	if len(claims.Hostnames) == 0 {
		return nil, 0, errors.New("stage0 token missing hostnames")
	}
	normalized, err := normalizeHostnames(claims.Hostnames)
	if err != nil {
		return nil, 0, err
	}

	weight := claims.Weight
	if weight <= 0 {
		weight = 1
	}

	if claims.HandshakeMaxAgeSeconds != nil && *claims.HandshakeMaxAgeSeconds > 0 {
		if claims.IssuedAt == nil || claims.IssuedAt.Time.IsZero() {
			return nil, 0, errors.New("stage0 token missing iat for handshake_max_age_seconds")
		}
		age := time.Since(claims.IssuedAt.Time)
		if age > time.Duration(*claims.HandshakeMaxAgeSeconds)*time.Second {
			return nil, 0, fmt.Errorf("stage0 token exceeded handshake_max_age_seconds: age=%s", age)
		}
	}

	return normalized, weight, nil
}

func (h *hubImpl) buildMetadataFromClaims(claims *auth.Claims, expectedHosts []string, expectedWeight int, nonce string) (*AttestationMetadata, error) {
	if claims.SessionNonce != nonce {
		return nil, fmt.Errorf("session nonce mismatch: expected %s got %s", nonce, claims.SessionNonce)
	}

	if len(claims.Hostnames) == 0 {
		return nil, errors.New("attested token missing hostnames")
	}
	normalized, err := normalizeHostnames(claims.Hostnames)
	if err != nil {
		return nil, err
	}
	if !sameStringSets(normalized, expectedHosts) {
		return nil, errors.New("attested token hostnames differ from handshake token")
	}

	weight := expectedWeight
	if claims.Weight > 0 && claims.Weight != expectedWeight {
		return nil, errors.New("attested token weight differs from handshake token")
	}

	meta := &AttestationMetadata{
		Hostnames:           append([]string{}, expectedHosts...),
		Weight:              weight,
		PolicyVersion:       claims.PolicyVersion,
		AuthorizerStatusURI: claims.AuthorizerStatusURI,
	}

	if claims.ReauthIntervalSeconds != nil && *claims.ReauthIntervalSeconds > 0 {
		meta.ReauthInterval = time.Duration(*claims.ReauthIntervalSeconds) * time.Second
		if claims.ReauthGraceSeconds != nil && *claims.ReauthGraceSeconds > 0 {
			meta.ReauthGrace = time.Duration(*claims.ReauthGraceSeconds) * time.Second
		} else {
			meta.ReauthGrace = defaultReauthGrace
		}
	}

	if claims.MaintenanceGraceCapSeconds != nil {
		if *claims.MaintenanceGraceCapSeconds < 0 {
			return nil, errors.New("maintenance_grace_cap_seconds cannot be negative")
		}
		meta.HasMaintenanceCap = true
		meta.MaintenanceCap = time.Duration(*claims.MaintenanceGraceCapSeconds) * time.Second
	}

	if meta.AuthorizerStatusURI != "" {
		if err := validateAuthorizerStatusURI(meta.AuthorizerStatusURI); err != nil {
			return nil, err
		}
	}

	return meta, nil
}

func sendChallenge(conn *websocket.Conn, msg challengeMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func generateNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func normalizeHostnames(hosts []string) ([]string, error) {
	uniq := make(map[string]struct{}, len(hosts))
	normalized := make([]string, 0, len(hosts))
	for _, hname := range hosts {
		n := hostn.NormalizeOrWildcard(hname)
		if n == "" {
			continue
		}
		if _, ok := uniq[n]; ok {
			continue
		}
		uniq[n] = struct{}{}
		normalized = append(normalized, n)
	}
	if len(normalized) == 0 {
		return nil, errors.New("no valid hostnames after normalization")
	}
	return normalized, nil
}

func validateAuthorizerStatusURI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid authorizer_status_uri: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("authorizer_status_uri must use https scheme")
	}
	if parsed.Host == "" {
		return errors.New("authorizer_status_uri missing host")
	}
	return nil
}

func sameStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		if counts[v] == 0 {
			return false
		}
		counts[v]--
		if counts[v] == 0 {
			delete(counts, v)
		}
	}
	return len(counts) == 0
}

func (h *hubImpl) register(b *Backend) {
	for _, hn := range b.hostnames {
		if suffix, ok := hostn.WildcardSuffix(hn); ok {
			rawPool, _ := h.wildcardPools.LoadOrStore(suffix, NewLoadBalancerPool())
			pool := rawPool.(*LoadBalancerPool)
			pool.AddBackend(b)
			log.Printf("INFO: Backend %s registered to wildcard pool for pattern '%s' (suffix '%s')", b.id, hn, suffix)
		} else {
			rawPool, _ := h.pools.LoadOrStore(hn, NewLoadBalancerPool())
			pool := rawPool.(*LoadBalancerPool)
			pool.AddBackend(b)
			log.Printf("INFO: Backend %s registered to pool for hostname '%s'", b.id, hn)
		}
	}
	h.updateAndAnnounceRoutes()
}

func (h *hubImpl) unregister(b *Backend) {
	b.Close()
	for _, hn := range b.hostnames {
		if suffix, ok := hostn.WildcardSuffix(hn); ok {
			if rawPool, ok := h.wildcardPools.Load(suffix); ok {
				pool := rawPool.(*LoadBalancerPool)
				pool.RemoveBackend(b)
				log.Printf("INFO: Backend %s unregistered from wildcard pool for pattern '%s' (suffix '%s')", b.id, hn, suffix)
			}
		} else {
			if rawPool, ok := h.pools.Load(hn); ok {
				pool := rawPool.(*LoadBalancerPool)
				pool.RemoveBackend(b)
				log.Printf("INFO: Backend %s unregistered from pool for hostname '%s'", b.id, hn)
			}
		}
	}
	h.updateAndAnnounceRoutes()
}

func (h *hubImpl) updateAndAnnounceRoutes() {
	if h.peerManager == nil {
		return
	}
	h.peerManager.AnnounceLocalRoutes()
}

func (h *hubImpl) GetLocalRoutes() []string {
	hostnames := make([]string, 0)
	h.pools.Range(func(key, value interface{}) bool {
		hostname := key.(string)
		pool := value.(*LoadBalancerPool)
		if pool.HasBackends() {
			hostnames = append(hostnames, hostname)
		}
		return true
	})
	h.wildcardPools.Range(func(key, value interface{}) bool {
		suffix := key.(string)
		pool := value.(*LoadBalancerPool)
		if pool.HasBackends() {
			hostnames = append(hostnames, "*"+suffix)
		}
		return true
	})
	return hostnames
}
