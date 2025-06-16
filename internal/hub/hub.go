package hub

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/AtDexters-Lab/global-access-relay/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	authTimeout     = 10 * time.Second
	shutdownTimeout = 5 * time.Second
)

// Hub manages the lifecycle of backend and peer WebSocket connections.
type Hub struct {
	config      *config.Config
	server      *http.Server
	upgrader    websocket.Upgrader
	pools       sync.Map // key: hostname (string), value: *LoadBalancerPool
	peerManager peer.PeerManager
}

// PeerManager is an interface that the Hub uses to interact with the peer manager.
type PeerManager interface {
	HandleInboundPeer(conn *websocket.Conn)
	AnnounceLocalRoutes()
	GetPeerForHostname(hostname string) (peer.Peer, bool)
	HandleTunnelRequest(p peer.Peer, hostname string, clientID uuid.UUID)
}

// NewHub creates and returns a new Hub instance.
func NewHub(cfg *config.Config) *Hub {
	return &Hub{
		config: cfg,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// SetPeerManager sets the peer manager for the hub.
func (h *Hub) SetPeerManager(pm PeerManager) {
	h.peerManager = pm
}

// Run starts the Hub's HTTP server.
func (h *Hub) Run() {
	mux := http.NewServeMux()
	mux.HandleFunc("/connect", h.handleBackendConnect)
	mux.HandleFunc("/mesh", h.handlePeerConnect)

	h.server = &http.Server{
		Addr:    h.config.ListenAddress,
		Handler: mux,
	}

	log.Printf("INFO: Hub listening on %s", h.config.ListenAddress)
	if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("FATAL: Hub failed to start: %v", err)
	}
}

// Stop gracefully shuts down the Hub's HTTP server.
func (h *Hub) Stop() {
	log.Println("INFO: Shutting down hub...")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := h.server.Shutdown(ctx); err != nil {
		log.Printf("WARN: Hub graceful shutdown failed: %v", err)
	} else {
		log.Println("INFO: Hub shut down gracefully.")
	}
}

// SelectBackend finds the load balancer pool for the given hostname.
func (h *Hub) SelectBackend(hostname string) (*Backend, error) {
	rawPool, ok := h.pools.Load(hostname)
	if !ok {
		return nil, fmt.Errorf("no backend pool available for hostname: %s", hostname)
	}

	pool := rawPool.(*LoadBalancerPool)
	return pool.Select()
}

// handleBackendConnect handles a connection from a backend service.
func (h *Hub) handleBackendConnect(w http.ResponseWriter, r *http.Request) {
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

// handlePeerConnect handles a connection from another Nexus node.
func (h *Hub) handlePeerConnect(w http.ResponseWriter, r *http.Request) {
	if h.peerManager == nil {
		log.Printf("ERROR: Peer manager not initialized, cannot handle peer connection.")
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	if r.Header.Get("X-Nexus-Secret") != h.config.PeerSecret {
		http.Error(w, "Forbidden", http.StatusForbidden)
		log.Printf("WARN: Peer connection attempt from %s with invalid secret.", r.RemoteAddr)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ERROR: Failed to upgrade peer connection: %v", err)
		return
	}

	log.Printf("INFO: Inbound peer connection established from %s", conn.RemoteAddr())
	h.peerManager.HandleInboundPeer(conn)
}

// BackendClaims defines the JWT claims for a backend.
type BackendClaims struct {
	Hostname string `json:"hostname"`
	Weight   int    `json:"weight"`
	jwt.RegisteredClaims
}

// authenticateBackend waits for and validates a backend's JWT.
func (h *Hub) authenticateBackend(conn *websocket.Conn) (*Backend, error) {
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
		secret, ok := h.config.Backends[claims.Hostname]
		if !ok {
			return nil, fmt.Errorf("unrecognized hostname in JWT claims: %s", claims.Hostname)
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("JWT validation failed: %w", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid JWT token")
	}
	return NewBackend(conn, claims.Hostname, claims.Weight, h.config), nil
}

func (h *Hub) register(b *Backend) {
	rawPool, _ := h.pools.LoadOrStore(b.hostname, NewLoadBalancerPool())
	pool := rawPool.(*LoadBalancerPool)
	pool.AddBackend(b)
	log.Printf("INFO: Backend %s registered to pool for hostname '%s'", b.id, b.hostname)
	h.updateAndAnnounceRoutes()
}

func (h *Hub) unregister(b *Backend) {
	b.Close()
	if rawPool, ok := h.pools.Load(b.hostname); ok {
		pool := rawPool.(*LoadBalancerPool)
		pool.RemoveBackend(b)
		log.Printf("INFO: Backend %s unregistered from pool for hostname '%s'", b.id, b.hostname)
		h.updateAndAnnounceRoutes()
	}
}

// updateAndAnnounceRoutes calculates the current set of locally available hostnames
// and tells the peer manager to broadcast this state to all peers.
func (h *Hub) updateAndAnnounceRoutes() {
	if h.peerManager == nil {
		return
	}
	h.peerManager.AnnounceLocalRoutes()
}

// GetLocalRoutes returns a slice of all hostnames currently serviced by local backends.
func (h *Hub) GetLocalRoutes() []string {
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
