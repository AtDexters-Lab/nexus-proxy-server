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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"

	"github.com/AtDexters-Lab/nexus-proxy/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/bandwidth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	hostn "github.com/AtDexters-Lab/nexus-proxy/hostnames"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/gorilla/websocket"
)

const (
	authTimeout     = 10 * time.Second
	shutdownTimeout = 5 * time.Second
)

// hubImpl manages the lifecycle of backend and peer connections.
type hubImpl struct {
	config             *config.Config
	backendServer      *http.Server // Listens for backend connections
	peerServer         *http.Server // Listens for peer connections (mTLS)
	upgrader           websocket.Upgrader
	pools              sync.Map // key: exact hostname (string) -> *LoadBalancerPool
	wildcardPools      sync.Map // key: suffix like ".example.com" -> *LoadBalancerPool
	udpFlowIdleByPort  sync.Map // key: int port -> time.Duration
	udpFlowIdleMu      sync.Mutex
	udpFlowIdleSources map[int]map[string]time.Duration // key: int port -> backend ID -> time.Duration
	peerManager        iface.PeerManager
	hubTlsConfig       *tls.Config
	validator          auth.Validator
	outboundClient     *http.Client
	bandwidthScheduler *bandwidth.Scheduler
}

// New creates and returns a new Hub instance.
func New(cfg *config.Config, tlsConfig *tls.Config, validator auth.Validator, outboundClient *http.Client) *hubImpl {
	h := &hubImpl{
		config:             cfg,
		hubTlsConfig:       tlsConfig,
		validator:          validator,
		outboundClient:     outboundClient,
		udpFlowIdleSources: make(map[int]map[string]time.Duration),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	// Initialize bandwidth scheduler if limit is configured
	if cfg.TotalBandwidthMbps > 0 {
		h.bandwidthScheduler = bandwidth.NewScheduler(cfg.TotalBandwidthMbps)
	}

	return h
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
		// RequireAnyClientCert demands a client cert but skips Go's built-in
		// chain + ExtKeyUsage verification — necessary because ACME certs
		// carry serverAuth only. Chain and domain validation happen in the
		// VerifyPeerCertificate callback instead.
		peerTlsConfig := h.hubTlsConfig.Clone()
		peerTlsConfig.ClientAuth = tls.RequireAnyClientCert
		peerTlsConfig.SessionTicketsDisabled = true
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

// buildVerifyPeerCertificateFunc returns a callback that verifies the peer's
// client certificate chain against the system root pool and checks the leaf
// cert's domain against trustedDomainSuffixes. Because the peer TLS config
// uses RequireAnyClientCert, Go does not build verifiedChains — the callback
// must parse rawCerts and verify the chain itself.
func (h *hubImpl) buildVerifyPeerCertificateFunc() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		// Defensive: unreachable with RequireAnyClientCert (Go aborts the
		// handshake before this callback if no cert is sent), but guards
		// against future ClientAuth changes.
		if len(rawCerts) == 0 {
			return errors.New("peer presented no certificate")
		}

		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("failed to parse peer certificate: %w", err)
		}

		intermediates := x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			inter, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("failed to parse intermediate certificate: %w", err)
			}
			intermediates.AddCert(inter)
		}

		// An empty KeyUsages defaults to serverAuth, which matches ACME
		// certs. This avoids requiring clientAuth, which ACME certs lack.
		if _, err := leaf.Verify(x509.VerifyOptions{
			Intermediates: intermediates,
		}); err != nil {
			return fmt.Errorf("peer certificate chain verification failed: %w", err)
		}

		for _, suffix := range h.config.PeerAuthentication.TrustedDomainSuffixes {
			for _, san := range leaf.DNSNames {
				if strings.HasSuffix(san, suffix) {
					log.Printf("INFO: Peer certificate validated via SAN for: %s", san)
					return nil
				}
			}
			if strings.HasSuffix(leaf.Subject.CommonName, suffix) {
				log.Printf("INFO: Peer certificate validated via CN for: %s", leaf.Subject.CommonName)
				return nil
			}
		}

		return fmt.Errorf("peer certificate FQDN ('%s' / SANs: %v) is not in any trusted domain", leaf.Subject.CommonName, leaf.DNSNames)
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

	// Stop bandwidth scheduler
	if h.bandwidthScheduler != nil {
		h.bandwidthScheduler.Stop()
	}
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

func (h *hubImpl) UDPFlowIdleTimeout(port int) (time.Duration, bool) {
	if raw, ok := h.udpFlowIdleByPort.Load(port); ok {
		if d, ok := raw.(time.Duration); ok {
			return d, true
		}
	}
	return 0, false
}

func (h *hubImpl) trackUDPFlowIdleTimeout(backendID string, routes []UDPRoutePolicy) {
	if len(routes) == 0 {
		return
	}

	portsTouched := make(map[int]struct{}, len(routes))

	h.udpFlowIdleMu.Lock()
	defer h.udpFlowIdleMu.Unlock()

	for _, route := range routes {
		port := route.Port
		portSources := h.udpFlowIdleSources[port]
		if portSources == nil {
			portSources = make(map[string]time.Duration, 1)
			h.udpFlowIdleSources[port] = portSources
		}

		for existingBackendID, existingTimeout := range portSources {
			if existingBackendID == backendID {
				continue
			}
			if existingTimeout != route.FlowIdleTimeout {
				log.Printf("WARN: Conflicting UDP flow idle timeouts for port %d: %s from backend %s vs %s from backend %s; using max", port, existingTimeout, existingBackendID, route.FlowIdleTimeout, backendID)
				break
			}
		}

		portSources[backendID] = route.FlowIdleTimeout
		portsTouched[port] = struct{}{}
	}

	for port := range portsTouched {
		h.recomputeUDPFlowIdleTimeoutLocked(port)
	}
}

func (h *hubImpl) untrackUDPFlowIdleTimeout(backendID string, routes []UDPRoutePolicy) {
	if len(routes) == 0 {
		return
	}

	portsTouched := make(map[int]struct{}, len(routes))

	h.udpFlowIdleMu.Lock()
	defer h.udpFlowIdleMu.Unlock()

	for _, route := range routes {
		port := route.Port
		portSources := h.udpFlowIdleSources[port]
		if portSources == nil {
			continue
		}

		delete(portSources, backendID)
		if len(portSources) == 0 {
			delete(h.udpFlowIdleSources, port)
		}
		portsTouched[port] = struct{}{}
	}

	for port := range portsTouched {
		h.recomputeUDPFlowIdleTimeoutLocked(port)
	}
}

func (h *hubImpl) recomputeUDPFlowIdleTimeoutLocked(port int) {
	portSources := h.udpFlowIdleSources[port]
	if len(portSources) == 0 {
		h.udpFlowIdleByPort.Delete(port)
		return
	}

	effective := time.Duration(0)
	for _, d := range portSources {
		if d > effective {
			effective = d
		}
	}

	if effective > 0 {
		h.udpFlowIdleByPort.Store(port, effective)
		return
	}
	h.udpFlowIdleByPort.Delete(port)
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

	log.Printf("INFO: Backend authenticated successfully: hostnames=%v tcp_ports=%v udp_routes=%v from=%s", backend.hostnames, backend.tcpPorts, backend.udpRoutes, conn.RemoteAddr())

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


type stage0Info struct {
	Hostnames []string
	TCPPorts  []int
	UDPRoutes []UDPRoutePolicy
	Weight    int
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

	info0, err := h.validateStage0Claims(claims0)
	if err != nil {
		return nil, err
	}

	nonce, err := generateNonce()
	if err != nil {
		return nil, fmt.Errorf("generate challenge nonce: %w", err)
	}

	if err := sendChallenge(conn, protocol.ChallengeMessage{Type: protocol.ChallengeHandshake, Nonce: nonce}); err != nil {
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

	meta, err := h.buildMetadataFromClaims(claims1, info0, nonce)
	if err != nil {
		return nil, err
	}

	return NewBackend(conn, meta, h.config, h.validator, h.outboundClient), nil
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

func (h *hubImpl) validateStage0Claims(claims *auth.Claims) (*stage0Info, error) {
	var normalizedHosts []string
	if len(claims.Hostnames) > 0 {
		var err error
		normalizedHosts, err = normalizeHostnames(claims.Hostnames)
		if err != nil {
			return nil, err
		}
	}

	tcpPorts, err := normalizeTCPPortClaims(h.config, claims.TCPPorts)
	if err != nil {
		return nil, err
	}
	udpRoutes, err := normalizeUDPRouteClaims(h.config, claims.UDPRoutes)
	if err != nil {
		return nil, err
	}

	if len(normalizedHosts) == 0 && len(tcpPorts) == 0 && len(udpRoutes) == 0 {
		return nil, errors.New("stage0 token missing hostnames and port claims")
	}

	weight := claims.Weight
	if weight <= 0 {
		weight = 1
	}

	if claims.HandshakeMaxAgeSeconds != nil && *claims.HandshakeMaxAgeSeconds > 0 {
		if claims.IssuedAt == nil || claims.IssuedAt.Time.IsZero() {
			return nil, errors.New("stage0 token missing iat for handshake_max_age_seconds")
		}
		age := time.Since(claims.IssuedAt.Time)
		if age > time.Duration(*claims.HandshakeMaxAgeSeconds)*time.Second {
			return nil, fmt.Errorf("stage0 token exceeded handshake_max_age_seconds: age=%s", age)
		}
	}

	return &stage0Info{
		Hostnames: normalizedHosts,
		TCPPorts:  tcpPorts,
		UDPRoutes: udpRoutes,
		Weight:    weight,
	}, nil
}

func (h *hubImpl) buildMetadataFromClaims(claims *auth.Claims, expected *stage0Info, nonce string) (*AttestationMetadata, error) {
	if claims.SessionNonce != nonce {
		return nil, fmt.Errorf("session nonce mismatch: expected %s got %s", nonce, claims.SessionNonce)
	}

	var normalizedHosts []string
	if len(claims.Hostnames) > 0 {
		var err error
		normalizedHosts, err = normalizeHostnames(claims.Hostnames)
		if err != nil {
			return nil, err
		}
	}

	tcpPorts, err := normalizeTCPPortClaims(h.config, claims.TCPPorts)
	if err != nil {
		return nil, err
	}
	udpRoutes, err := normalizeUDPRouteClaims(h.config, claims.UDPRoutes)
	if err != nil {
		return nil, err
	}

	if len(normalizedHosts) == 0 && len(tcpPorts) == 0 && len(udpRoutes) == 0 {
		return nil, errors.New("attested token missing hostnames and port claims")
	}

	if expected == nil {
		return nil, errors.New("missing handshake expectations")
	}

	if !sameStringSets(normalizedHosts, expected.Hostnames) {
		return nil, errors.New("attested token hostnames differ from handshake token")
	}
	if !sameIntSets(tcpPorts, expected.TCPPorts) {
		return nil, errors.New("attested token tcp port claims differ from handshake token")
	}
	if !sameUDPRoutes(udpRoutes, expected.UDPRoutes) {
		return nil, errors.New("attested token udp route claims differ from handshake token")
	}

	weight := expected.Weight
	if claims.Weight > 0 && claims.Weight != expected.Weight {
		return nil, errors.New("attested token weight differs from handshake token")
	}

	meta := &AttestationMetadata{
		Hostnames:           append([]string{}, expected.Hostnames...),
		TCPPorts:            append([]int{}, expected.TCPPorts...),
		UDPRoutes:           append([]UDPRoutePolicy{}, expected.UDPRoutes...),
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

func sendChallenge(conn *websocket.Conn, msg protocol.ChallengeMessage) error {
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
		// Prevent backends from claiming reserved route-key strings via hostnames.
		// Route keys share the same namespace as hostnames in h.pools.
		if strings.HasPrefix(n, protocol.RouteKeyPrefixTCP) || strings.HasPrefix(n, protocol.RouteKeyPrefixUDP) {
			return nil, fmt.Errorf("reserved route key %q cannot be used as a hostname claim", n)
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

func normalizeTCPPortClaims(cfg *config.Config, ports []int) ([]int, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	if cfg == nil || len(cfg.AllowedTCPPortClaims) == 0 {
		return nil, errors.New("tcp port claims are disabled (allowedTCPPortClaims is empty)")
	}
	uniq := make(map[int]struct{}, len(ports))
	out := make([]int, 0, len(ports))
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid tcp port claim: %d", port)
		}
		if !portAllowed(cfg.AllowedTCPPortClaims, port) {
			return nil, fmt.Errorf("tcp port claim %d is not allowed", port)
		}
		if _, ok := uniq[port]; ok {
			continue
		}
		uniq[port] = struct{}{}
		out = append(out, port)
	}
	sort.Ints(out)
	return out, nil
}

func normalizeUDPRouteClaims(cfg *config.Config, routes []protocol.UDPRouteClaim) ([]UDPRoutePolicy, error) {
	if len(routes) == 0 {
		return nil, nil
	}
	if cfg == nil || len(cfg.AllowedUDPPortClaims) == 0 {
		return nil, errors.New("udp port claims are disabled (allowedUDPPortClaims is empty)")
	}

	minTimeout := cfg.UDPFlowIdleTimeoutMin()
	maxTimeout := cfg.UDPFlowIdleTimeoutMax()
	if maxTimeout < minTimeout {
		return nil, fmt.Errorf("udp flow idle timeout max (%s) cannot be less than min (%s)", maxTimeout, minTimeout)
	}

	seen := make(map[int]UDPRoutePolicy, len(routes))
	for _, route := range routes {
		port := route.Port
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid udp route port claim: %d", port)
		}
		if !portAllowed(cfg.AllowedUDPPortClaims, port) {
			return nil, fmt.Errorf("udp port claim %d is not allowed", port)
		}

		timeout := cfg.UDPFlowIdleTimeoutDefault()
		if route.FlowIdleTimeoutSeconds != nil && *route.FlowIdleTimeoutSeconds > 0 {
			timeout = time.Duration(*route.FlowIdleTimeoutSeconds) * time.Second
		}
		if timeout < minTimeout {
			timeout = minTimeout
		}
		if timeout > maxTimeout {
			timeout = maxTimeout
		}

		if existing, ok := seen[port]; ok {
			if existing.FlowIdleTimeout != timeout {
				return nil, fmt.Errorf("conflicting udp route policies for port %d", port)
			}
			continue
		}
		seen[port] = UDPRoutePolicy{Port: port, FlowIdleTimeout: timeout}
	}

	out := make([]UDPRoutePolicy, 0, len(seen))
	for _, route := range seen {
		out = append(out, route)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

func portAllowed(allowlist []int, port int) bool {
	for _, allowed := range allowlist {
		if allowed == port {
			return true
		}
	}
	return false
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

func sameIntSets(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	aSorted := append([]int{}, a...)
	bSorted := append([]int{}, b...)
	sort.Ints(aSorted)
	sort.Ints(bSorted)
	for i := range aSorted {
		if aSorted[i] != bSorted[i] {
			return false
		}
	}
	return true
}

func sameUDPRoutes(a, b []UDPRoutePolicy) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	aSorted := append([]UDPRoutePolicy{}, a...)
	bSorted := append([]UDPRoutePolicy{}, b...)
	sort.Slice(aSorted, func(i, j int) bool { return aSorted[i].Port < aSorted[j].Port })
	sort.Slice(bSorted, func(i, j int) bool { return bSorted[i].Port < bSorted[j].Port })
	for i := range aSorted {
		if aSorted[i].Port != bSorted[i].Port {
			return false
		}
		if aSorted[i].FlowIdleTimeout != bSorted[i].FlowIdleTimeout {
			return false
		}
	}
	return true
}

func (h *hubImpl) register(b *Backend) {
	// Register with bandwidth scheduler
	if h.bandwidthScheduler != nil {
		b.bandwidthState = h.bandwidthScheduler.Register(b.id)
		b.bandwidthScheduler = h.bandwidthScheduler
	}

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

	for _, port := range b.tcpPorts {
		routeKey := protocol.RouteKey(protocol.TransportTCP, port)
		rawPool, _ := h.pools.LoadOrStore(routeKey, NewLoadBalancerPool())
		pool := rawPool.(*LoadBalancerPool)
		pool.AddBackend(b)
		log.Printf("INFO: Backend %s registered to port pool '%s'", b.id, routeKey)
	}

	for _, route := range b.udpRoutes {
		routeKey := protocol.RouteKey(protocol.TransportUDP, route.Port)
		rawPool, _ := h.pools.LoadOrStore(routeKey, NewLoadBalancerPool())
		pool := rawPool.(*LoadBalancerPool)
		pool.AddBackend(b)
		log.Printf("INFO: Backend %s registered to UDP port pool '%s' (flow_idle_timeout=%s)", b.id, routeKey, route.FlowIdleTimeout)
	}
	h.trackUDPFlowIdleTimeout(b.id, b.udpRoutes)

	h.updateAndAnnounceRoutes()
}

func (h *hubImpl) unregister(b *Backend) {
	// Unregister from bandwidth scheduler
	if h.bandwidthScheduler != nil {
		h.bandwidthScheduler.Unregister(b.id)
	}

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

	for _, port := range b.tcpPorts {
		routeKey := protocol.RouteKey(protocol.TransportTCP, port)
		if rawPool, ok := h.pools.Load(routeKey); ok {
			pool := rawPool.(*LoadBalancerPool)
			pool.RemoveBackend(b)
			log.Printf("INFO: Backend %s unregistered from port pool '%s'", b.id, routeKey)
		}
	}

	for _, route := range b.udpRoutes {
		routeKey := protocol.RouteKey(protocol.TransportUDP, route.Port)
		if rawPool, ok := h.pools.Load(routeKey); ok {
			pool := rawPool.(*LoadBalancerPool)
			pool.RemoveBackend(b)
			log.Printf("INFO: Backend %s unregistered from UDP port pool '%s'", b.id, routeKey)
		}
	}
	h.untrackUDPFlowIdleTimeout(b.id, b.udpRoutes)

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

// GetBandwidthScheduler returns the bandwidth scheduler for peer tunneling.
func (h *hubImpl) GetBandwidthScheduler() *bandwidth.Scheduler {
	return h.bandwidthScheduler
}
