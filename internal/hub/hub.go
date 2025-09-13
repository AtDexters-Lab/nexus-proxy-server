package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
	"github.com/golang-jwt/jwt/v5"
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
	pools         sync.Map // key: hostname (string), value: *LoadBalancerPool
	peerManager   iface.PeerManager
	hubTlsConfig  *tls.Config
}

// New creates and returns a new Hub instance.
func New(cfg *config.Config, tlsConfig *tls.Config) *hubImpl {
	return &hubImpl{
		config:       cfg,
		hubTlsConfig: tlsConfig,
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
	rawPool, ok := h.pools.Load(hostname)
	if !ok {
		return nil, fmt.Errorf("no backend pool available for hostname: %s", hostname)
	}

	pool := rawPool.(*LoadBalancerPool)
	return pool.Select()
}

// handleBackendConnect handles a connection from a backend service (JWT Auth).
func (h *hubImpl) handleBackendConnect(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ERROR: Failed to upgrade backend connection: %v", err)
		return
	}

	backend, err := h.authenticateBackend(conn)
	if err != nil {
		log.Printf("WARN: Backend authentication failed for %s: %v", conn.RemoteAddr(), err)
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()))
		conn.Close()
		return
	}

	log.Printf("INFO: Backend for hostname '%s' authenticated successfully from %s", backend.hostname, conn.RemoteAddr())

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

// BackendClaims defines the JWT claims for a backend.
type BackendClaims struct {
	Hostname string `json:"hostname"`
	Weight   int    `json:"weight"`
	jwt.RegisteredClaims
}

// authenticateBackend waits for and validates a backend's JWT.
func (h *hubImpl) authenticateBackend(conn *websocket.Conn) (*Backend, error) {
	if err := conn.SetReadDeadline(time.Now().Add(authTimeout)); err != nil {
		return nil, fmt.Errorf("failed to set read deadline: %w", err)
	}
	_, tokenBytes, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to read auth message: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("failed to reset read deadline: %w", err)
	}
	tokenString := string(tokenBytes)
	claims := &BackendClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(h.config.BackendsJWTSecret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("JWT validation failed: %w", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid JWT token")
	}
	if claims.Hostname == "" {
		return nil, fmt.Errorf("JWT claim 'hostname' is missing or empty")
	}

	return NewBackend(conn, claims.Hostname, claims.Weight, h.config), nil
}

func (h *hubImpl) register(b *Backend) {
	rawPool, _ := h.pools.LoadOrStore(b.hostname, NewLoadBalancerPool())
	pool := rawPool.(*LoadBalancerPool)
	pool.AddBackend(b)
	log.Printf("INFO: Backend %s registered to pool for hostname '%s'", b.id, b.hostname)
	h.updateAndAnnounceRoutes()
}

func (h *hubImpl) unregister(b *Backend) {
	b.Close()
	if rawPool, ok := h.pools.Load(b.hostname); ok {
		pool := rawPool.(*LoadBalancerPool)
		pool.RemoveBackend(b)
		log.Printf("INFO: Backend %s unregistered from pool for hostname '%s'", b.id, b.hostname)
		h.updateAndAnnounceRoutes()
	}
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
	return hostnames
}
