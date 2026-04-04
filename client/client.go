package client

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)


// jitterRand is a local random source seeded with crypto/rand for unpredictable backoff jitter.
// This prevents synchronized reconnection patterns across multiple client instances.
// Protected by jitterMu for thread-safe concurrent access.
var (
	jitterRand *rand.Rand
	jitterMu   sync.Mutex
)

func init() {
	var seed int64
	if err := binary.Read(crand.Reader, binary.LittleEndian, &seed); err != nil {
		// Fallback to time-based seed if crypto/rand fails (should never happen)
		seed = time.Now().UnixNano()
	}
	jitterRand = rand.New(rand.NewSource(seed))
}

const (
	healthCheckInterval = 5 * time.Second

	// Reconnection backoff parameters
	baseReconnectDelay = 5 * time.Second  // Initial delay between reconnection attempts
	maxReconnectDelay  = 60 * time.Second // Maximum delay cap
	jitterFactor       = 0.25             // ±25% jitter to prevent thundering herd

	writeToNexusBufferSize = 1024 * 16 // The server specifies a maximum read size of 32KB + header bytes, so 16KB is a safe size.
	// connection health check parameters
	writeWait        = 10 * time.Second
	pingInterval     = 5 * time.Second
	pongWait         = 10 * time.Second
	handshakeTimeout = 15 * time.Second
	maxMessageSize   = 32*1024 + protocol.MessageHeaderLength

	enqueueTimeout         = 5 * time.Second
	controlQueueSize       = 64
	connectionDrainTimeout = 5 * time.Second

	// Robustness stats keys (for observability, though we don't have metrics struct yet)
	// We'll log them for now.
	maxPermanentFailures = 5
)

// DisconnectReason is an alias for protocol.DisconnectReason so library
// consumers can reference disconnect reasons without importing protocol.
type DisconnectReason = protocol.DisconnectReason

const (
	DisconnectNormal        = protocol.DisconnectNormal
	DisconnectBufferFull    = protocol.DisconnectBufferFull
	DisconnectDialFailed    = protocol.DisconnectDialFailed
	DisconnectTimeout       = protocol.DisconnectTimeout
	DisconnectLocalError    = protocol.DisconnectLocalError
	DisconnectShutdown      = protocol.DisconnectShutdown
	DisconnectSessionEnded  = protocol.DisconnectSessionEnded
	DisconnectPauseViolated = protocol.DisconnectPauseViolated
	DisconnectUnknown       = protocol.DisconnectUnknown
)

type ErrorCategory int

const (
	ErrorTransient ErrorCategory = iota // Retry with backoff
	ErrorPermanent                      // Don't retry, surface to user
	ErrorRateLimit                      // Retry with longer backoff
)

type CategorizedError struct {
	Err      error
	Category ErrorCategory
	Reason   string // Machine-readable reason code
}

func categorizeError(err error) CategorizedError {
	if err == nil {
		return CategorizedError{nil, ErrorTransient, "none"}
	}
	// unwrapped checks
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CategorizedError{err, ErrorTransient, "timeout"}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return CategorizedError{err, ErrorTransient, "connection_refused"}
	}

	errMsg := strings.ToLower(err.Error())

	if strings.Contains(errMsg, "rate limit") {
		return CategorizedError{err, ErrorRateLimit, "rate_limited"}
	}

	// Auth failures are permanent - retrying won't help without credential changes
	if strings.Contains(errMsg, "unauthorized") ||
		strings.Contains(errMsg, "invalid token") ||
		strings.Contains(errMsg, "invalid credential") ||
		strings.Contains(errMsg, "authentication failed") ||
		strings.Contains(errMsg, "forbidden") ||
		strings.Contains(errMsg, "401") ||
		strings.Contains(errMsg, "403") {
		return CategorizedError{err, ErrorPermanent, "auth_failed"}
	}

	return CategorizedError{err, ErrorTransient, "unknown"}
}

type ConnState uint32

const (
	ConnStatePending  ConnState = iota // Dial in progress
	ConnStateActive                    // Connected, relaying data
	ConnStateDraining                  // Graceful shutdown, no new data
	ConnStateClosed                    // Terminal state
)

var errSessionInactive = errors.New("client session inactive")

// Session represents a single WebSocket session with the relay.
// A new Session is created for each successful connectAndAuthenticate.
// Fields are immutable after creation except for connected (set to false on close).
type Session struct {
	done      chan struct{}
	connected atomic.Bool
	closeOnce sync.Once
}

// Done returns a channel that is closed when the session ends.
// Returns nil if s is nil (no active session).
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// IsConnected returns whether the session is still active.
func (s *Session) IsConnected() bool {
	if s == nil {
		return false
	}
	return s.connected.Load()
}

// Close marks the session as disconnected and signals all waiters.
// Safe to call multiple times. Order matters: connected=false before close(done)
// so IsConnected() returns false before any Done() select unblocks.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.connected.Store(false)
		close(s.done)
	})
}

// EventType represents the type of client event.
type EventType int

const (
	EventConnected EventType = iota
	EventDisconnected
	EventConnectionOpened
	EventConnectionClosed
	EventPaused
	EventResumed
	EventError
	EventReauthStarted
	EventReauthCompleted
)

func (e EventType) String() string {
	switch e {
	case EventConnected:
		return "connected"
	case EventDisconnected:
		return "disconnected"
	case EventConnectionOpened:
		return "connection_opened"
	case EventConnectionClosed:
		return "connection_closed"
	case EventPaused:
		return "paused"
	case EventResumed:
		return "resumed"
	case EventError:
		return "error"
	case EventReauthStarted:
		return "reauth_started"
	case EventReauthCompleted:
		return "reauth_completed"
	default:
		return "unknown"
	}
}

// Event represents a client lifecycle event.
type Event struct {
	Type      EventType
	Timestamp time.Time

	// Connection context (if applicable)
	ClientID string
	Hostname string

	// Error context (if applicable)
	Error  error
	Reason string
}

// EventHandler is a callback function for client events.
type EventHandler func(Event)

// Stats provides a snapshot of client statistics.
type Stats struct {
	// Connection metrics
	ActiveConnections  int64
	TotalConnections   int64
	PendingConnections int64

	// Data transfer
	BytesSentTotal        int64
	BytesReceivedTotal    int64
	MessagesSentTotal     int64
	MessagesReceivedTotal int64

	// Queue metrics
	ControlQueueDepth int
	DataQueueDepth    int // Number of connections with pending data

	// Error metrics
	DroppedConnections int64
	TransientErrors    int64
	PermanentErrors    int64
	RateLimitHits      int64
	EnqueueTimeouts    int64

	// Flow control
	PausedConnections int64
	PauseViolations   int64

	// Event metrics
	DroppedEvents int64

	// UDP metrics
	UDPDroppedPackets int64

	// Session metrics
	SessionUptime   time.Duration
	LastUpdated     time.Time
	ReconnectCount  int64
	LastConnectedAt time.Time

	// Per-connection stats (optional, can be expensive)
	ConnectionStats map[string]ConnectionStats
}

// ConnectionStats provides per-connection statistics.
type ConnectionStats struct {
	ClientID     string
	Hostname     string
	State        ConnState
	BytesIn      int64
	BytesOut     int64
	BufferLevel  int
	Paused       bool
	ConnectedAt  time.Time
	LastActivity time.Time
	IsUDP        bool
}

// internalStats holds atomic counters for thread-safe stats tracking.
type internalStats struct {
	activeConns       atomic.Int64
	totalConns        atomic.Int64
	pendingConns      atomic.Int64
	bytesSent         atomic.Int64
	bytesRecv         atomic.Int64
	msgsSent          atomic.Int64
	msgsRecv          atomic.Int64
	droppedConns      atomic.Int64
	transientErrors   atomic.Int64
	permanentErrors   atomic.Int64
	rateLimitHits     atomic.Int64
	enqueueTimeouts   atomic.Int64
	pausedConns       atomic.Int64
	pauseViolations   atomic.Int64
	droppedEvents     atomic.Int64
	udpDroppedPackets atomic.Int64
	reconnectCount    atomic.Int64
	sessionStart      atomic.Int64 // Unix timestamp
	lastConnected     atomic.Int64 // Unix timestamp
}

const (
	eventBufferSize = 256
	statsCacheTTL   = 1 * time.Second
)

// backoffDelay calculates the reconnection delay with exponential backoff and jitter.
// The delay doubles with each retry up to maxReconnectDelay, with ±25% jitter.
// Uses a crypto-seeded random source to prevent synchronized reconnection patterns.
// Thread-safe for concurrent use by multiple Client instances.
func backoffDelay(retries int) time.Duration {
	delay := float64(baseReconnectDelay) * math.Pow(2, float64(retries))
	if delay > float64(maxReconnectDelay) {
		delay = float64(maxReconnectDelay)
	}
	// Add jitter: ±25% of the delay (using crypto-seeded source, mutex-protected)
	jitterMu.Lock()
	jitter := delay * jitterFactor * (jitterRand.Float64()*2 - 1)
	jitterMu.Unlock()
	result := delay + jitter
	// Cap final result to enforce maxReconnectDelay even after positive jitter
	if result > float64(maxReconnectDelay) {
		result = float64(maxReconnectDelay)
	}
	return time.Duration(result)
}

type outboundMessage struct {
	messageType int
	payload     []byte
}


// clientConn represents a single proxied connection to the local service.
// conn may be nil while dial is in progress (pending state).
type clientConn struct {
	id           uuid.UUID
	state        atomic.Uint32 // ConnState
	connMu       sync.Mutex    // Protects conn field during assignment/close race window
	conn         net.Conn      // nil while dial is pending; protected by connMu during mutation
	hostname     string
	lastActivity atomic.Int64 // Unix timestamp of the last activity.
	pingSent     atomic.Bool  // True if an inactivity ping has been sent.
	// pending      atomic.Bool  // Removed: replaced by state machine (ConnStatePending)
	quit    chan struct{}
	drained chan struct{} // Closed by writePump when per-connection outbound queue is fully drained after close
	writeCh chan []byte   // Buffered channel for non-blocking writes to local conn
	// Flow control
	flow flowControl

	// Signaling state for fair queuing
	signaled atomic.Bool

	// session is the Session that created this connection. Used by
	// transitionToClosed to check the owning session's liveness instead of
	// the global active session, preventing stale disconnects after reconnect.
	session *Session

	isUDP          bool
	droppedPackets atomic.Int64 // UDP only: packets dropped due to slow local service

	closeOnce   sync.Once   // Ensures connection is closed only once
	pongTimer   *time.Timer // Timer for pong timeout, can be cancelled
	pongTimerMu sync.Mutex  // Protects pongTimer access
}

// Atomic state transitions with validation
func (c *clientConn) transition(from, to ConnState) bool {
	return c.state.CompareAndSwap(uint32(from), uint32(to))
}

const (
	// Per-connection write buffer size
	localConnWriteBuffer = 64
	// Dial timeout for local connections
	localDialTimeout = 10 * time.Second
	// Write timeout for local connections
	localWriteTimeout = 10 * time.Second
)

// safeClose closes the connection and quit channel exactly once, preventing double-close panics.
// Safe to call even if conn is nil (pending dial).
func (cc *clientConn) safeClose() {
	cc.closeOnce.Do(func() {
		// Cancel pong timer first to prevent timer callback from racing with close
		cc.cancelPongTimer()
		close(cc.quit)
		// Use mutex to safely read conn (may be concurrently assigned by establishLocalConnection)
		cc.connMu.Lock()
		conn := cc.conn
		cc.connMu.Unlock()
		if conn != nil {
			conn.Close()
		}
	})
}

// closeOnDrained signals that the per-connection outbound queue has been fully
// drained by writePump. Safe to call multiple times (idempotent).
// Must only be called from writePump's drainConnectionQueue (single-caller
// invariant ensures no concurrent close).
func (cc *clientConn) closeOnDrained() {
	select {
	case <-cc.drained:
		// Already closed
	default:
		close(cc.drained)
	}
}

// cancelPongTimer stops the pong timeout timer if it's running.
func (cc *clientConn) cancelPongTimer() {
	cc.pongTimerMu.Lock()
	defer cc.pongTimerMu.Unlock()
	if cc.pongTimer != nil {
		cc.pongTimer.Stop()
		cc.pongTimer = nil
	}
}

// setPongTimer sets a new pong timeout timer, cancelling any existing one.
func (cc *clientConn) setPongTimer(d time.Duration, f func()) {
	cc.pongTimerMu.Lock()
	defer cc.pongTimerMu.Unlock()
	if cc.pongTimer != nil {
		cc.pongTimer.Stop()
	}
	cc.pongTimer = time.AfterFunc(d, f)
}

type flowControl struct {
	// Buffer configuration
	lowWaterMark  int // Resume threshold (e.g., 16)
	highWaterMark int // Pause threshold (e.g., 48)
	maxBuffer     int // Hard limit (e.g., 64)

	// State
	paused atomic.Bool
	level  atomic.Int32
}

// drainConnection starts a timer to force close the connection if it doesn't drain in time.
// It is called when transitioning to ConnStateDraining.
func (c *Client) drainConnection(conn *clientConn) {
	time.AfterFunc(connectionDrainTimeout, func() {
		if conn.state.Load() == uint32(ConnStateDraining) {
			log.Printf("WARN: [%s] Drain timeout for ClientID %s, forcing close", c.config.Name, conn.id)
			c.transitionToClosed(conn, DisconnectTimeout)
		}
	})
}

type ClientBackendConfig struct {
	Name             string
	Hostnames        []string
	TCPPorts         []int
	UDPRoutes        []UDPRouteConfig
	NexusAddress     string
	Weight           int
	Attestation      AttestationOptions
	PortMappings     map[int]PortMapping
	HealthChecks     HealthCheckConfig
	FlowControl      FlowControlConfig
	Socks5ListenAddr string
}

// Client manages the full lifecycle for one configured backend service.
type Client struct {
	config      ClientBackendConfig
	marshalJSON func(interface{}) ([]byte, error)
	ws          *websocket.Conn
	wsMu        sync.Mutex
	localConns  sync.Map
	controlSend chan outboundMessage
	// Per-connection outbound queues
	// Key: clientID, Value: chan outboundMessage
	connQueues sync.Map
	// Notifies writePump that a queue has data
	dataReady           chan uuid.UUID
	activeSession       atomic.Pointer[Session]
	shuttingDown        atomic.Bool // Flag for graceful shutdown
	ctx                 context.Context
	cancel              context.CancelFunc
	wg                  sync.WaitGroup // Tracks readPump and writePump per session
	healthWg            sync.WaitGroup // Tracks healthCheckPump goroutine
	connectHandler      ConnectHandler
	tokenProvider       TokenProvider
	staticTokenProvider TokenProvider

	// Phase 4: Observability
	stats        internalStats
	eventHandler EventHandler
	eventCh      chan Event // Buffered channel for ordered event delivery

	// Stats cache for rate-limiting expensive operations
	statsCacheMu   sync.RWMutex
	statsCache     Stats
	statsCacheTime time.Time

	// Outbound proxy (SOCKS5)
	socks5Listener  net.Listener
	outboundPending sync.Map // uuid.UUID → chan error
}

// New creates a new Client instance for a specific backend configuration.
func New(cfg ClientBackendConfig, opts ...Option) (*Client, error) {
	if cfg.Weight <= 0 {
		cfg.Weight = 1
	}

	// Set flow control defaults if not configured
	if cfg.FlowControl.HighWaterMark <= 0 {
		cfg.FlowControl.HighWaterMark = DefaultHighWaterMark
	}
	if cfg.FlowControl.LowWaterMark <= 0 {
		cfg.FlowControl.LowWaterMark = DefaultLowWaterMark
	}
	if cfg.FlowControl.MaxBuffer <= 0 {
		cfg.FlowControl.MaxBuffer = DefaultMaxBuffer
	}

	// PortMappings may be shared across callers. Copy before mutation.
	cfg.PortMappings = copyPortMappings(cfg.PortMappings)

	// Finalize: validate, normalize hosts, extract wildcards.
	// This is the single finalization point — LoadConfig intentionally
	// does not finalize so that copies preserve wildcard entries.
	for port, pm := range cfg.PortMappings {
		if err := pm.finalize(); err != nil {
			return nil, fmt.Errorf("port %d: %w", port, err)
		}
		cfg.PortMappings[port] = pm
	}

	defaultProvider, err := buildDefaultProvider(cfg)
	if err != nil {
		return nil, err
	}

	c := &Client{
		config:      cfg,
		marshalJSON: json.Marshal,
		controlSend: make(chan outboundMessage, controlQueueSize),
		dataReady:   make(chan uuid.UUID, 256), // Buffer: 256
		eventCh:     make(chan Event, eventBufferSize),
	}

	c.connectHandler = c.configBasedConnectHandler()
	c.staticTokenProvider = defaultProvider
	c.tokenProvider = defaultProvider

	for _, opt := range opts {
		opt(c)
	}
	if c.connectHandler == nil {
		c.connectHandler = c.configBasedConnectHandler()
	}
	if c.staticTokenProvider == nil && c.tokenProvider != nil {
		c.staticTokenProvider = c.tokenProvider
	}
	if c.tokenProvider == nil {
		if defaultProvider == nil {
			return nil, fmt.Errorf("token provider not configured and no attestation mechanism supplied")
		}
		c.tokenProvider = defaultProvider
	}
	if c.staticTokenProvider == nil {
		c.staticTokenProvider = defaultProvider
	}

	return c, nil
}

func buildDefaultProvider(cfg ClientBackendConfig) (TokenProvider, error) {
	opts := cfg.Attestation
	if strings.TrimSpace(opts.Command) != "" {
		return NewCommandTokenProvider(opts)
	}
	if strings.TrimSpace(opts.HMACSecret) != "" || strings.TrimSpace(opts.HMACSecretFile) != "" {
		return NewHMACTokenProvider(opts, cfg.Name, cfg.Hostnames, cfg.TCPPorts, cfg.UDPRoutes, cfg.Weight)
	}
	return nil, nil
}

// Stats returns a snapshot of current client statistics.
// This is a lightweight operation that reads atomic counters.
func (c *Client) Stats() Stats {
	var dataQueueDepth int
	c.connQueues.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan outboundMessage); ok {
			if len(ch) > 0 {
				dataQueueDepth++
			}
		}
		return true
	})

	sessionStart := c.stats.sessionStart.Load()
	var sessionUptime time.Duration
	if sessionStart > 0 && c.activeSession.Load().IsConnected() {
		sessionUptime = time.Since(time.Unix(sessionStart, 0))
	}

	lastConnected := c.stats.lastConnected.Load()
	var lastConnectedAt time.Time
	if lastConnected > 0 {
		lastConnectedAt = time.Unix(lastConnected, 0)
	}

	return Stats{
		ActiveConnections:     c.stats.activeConns.Load(),
		TotalConnections:      c.stats.totalConns.Load(),
		PendingConnections:    c.stats.pendingConns.Load(),
		BytesSentTotal:        c.stats.bytesSent.Load(),
		BytesReceivedTotal:    c.stats.bytesRecv.Load(),
		MessagesSentTotal:     c.stats.msgsSent.Load(),
		MessagesReceivedTotal: c.stats.msgsRecv.Load(),
		ControlQueueDepth:     len(c.controlSend),
		DataQueueDepth:        dataQueueDepth,
		DroppedConnections:    c.stats.droppedConns.Load(),
		TransientErrors:       c.stats.transientErrors.Load(),
		PermanentErrors:       c.stats.permanentErrors.Load(),
		RateLimitHits:         c.stats.rateLimitHits.Load(),
		EnqueueTimeouts:       c.stats.enqueueTimeouts.Load(),
		PausedConnections:     c.stats.pausedConns.Load(),
		PauseViolations:       c.stats.pauseViolations.Load(),
		DroppedEvents:         c.stats.droppedEvents.Load(),
		UDPDroppedPackets:     c.stats.udpDroppedPackets.Load(),
		SessionUptime:         sessionUptime,
		LastUpdated:           time.Now(),
		ReconnectCount:        c.stats.reconnectCount.Load(),
		LastConnectedAt:       lastConnectedAt,
	}
}

// StatsDetailed returns stats including per-connection details.
// This is a more expensive operation that iterates over all connections.
// Results are cached for 1 second to prevent excessive CPU usage.
func (c *Client) StatsDetailed() Stats {
	c.statsCacheMu.RLock()
	if time.Since(c.statsCacheTime) < statsCacheTTL {
		cached := c.statsCache
		c.statsCacheMu.RUnlock()
		return cached
	}
	c.statsCacheMu.RUnlock()

	// Compute fresh stats
	stats := c.Stats()
	stats.ConnectionStats = make(map[string]ConnectionStats)

	c.localConns.Range(func(key, value interface{}) bool {
		if conn, ok := value.(*clientConn); ok {
			id := conn.id.String()
			stats.ConnectionStats[id] = ConnectionStats{
				ClientID:     id,
				Hostname:     conn.hostname,
				State:        ConnState(conn.state.Load()),
				BufferLevel:  int(conn.flow.level.Load()),
				Paused:       conn.flow.paused.Load(),
				LastActivity: time.Unix(conn.lastActivity.Load(), 0),
				IsUDP:        conn.isUDP,
			}
		}
		return true
	})

	c.statsCacheMu.Lock()
	c.statsCache = stats
	c.statsCacheTime = time.Now()
	c.statsCacheMu.Unlock()

	return stats
}

// emit sends an event to the event handler asynchronously.
// Events are delivered in order via a buffered channel.
func (c *Client) emit(event Event) {
	event.Timestamp = time.Now()
	select {
	case c.eventCh <- event:
		// Successfully queued
	default:
		// Buffer full - drop event
		c.stats.droppedEvents.Add(1)
	}
}

// startEventDispatcher starts the goroutine that delivers events to the handler.
func (c *Client) startEventDispatcher() {
	// safeCallHandler wraps the event handler with panic recovery
	safeCallHandler := func(event Event) {
		if c.eventHandler == nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: [%s] Event handler panicked: %v", c.config.Name, r)
			}
		}()
		c.eventHandler(event)
	}

	go func() {
		for {
			select {
			case event, ok := <-c.eventCh:
				if !ok {
					return // Channel closed
				}
				safeCallHandler(event)
			case <-c.ctx.Done():
				// Drain remaining events before exiting
				for {
					select {
					case event := <-c.eventCh:
						safeCallHandler(event)
					default:
						return
					}
				}
			}
		}
	}()
}

// Start initiates the client's connection loop.
func (c *Client) Start(ctx context.Context) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	defer c.cleanup() // runs second: waits for background goroutines
	defer c.cancel()  // runs first (LIFO): signals background goroutines to exit

	log.Printf("INFO: [%s] Manager started for hostnames: %s", c.config.Name, strings.Join(c.config.Hostnames, ", "))

	// Start the event dispatcher for async event delivery
	c.startEventDispatcher()

	// Start the health check pump to monitor connection health.
	c.healthWg.Add(1)
	go func() {
		defer c.healthWg.Done()
		c.healthCheckPump()
	}()

	c.startSocks5Listener()

	// Start the network state watcher for fast recovery (Linux only, best-effort)
	networkWakeup := c.watchNetworkState()

	var transientRetries, permanentFailures int

	for {
		select {
		case <-c.ctx.Done():
			log.Printf("INFO: [%s] Context canceled. Manager stopping.", c.config.Name)
			return
		default:
		}

		err := c.connectAndAuthenticate()
		if err == nil {
			transientRetries = 0
			permanentFailures = 0
			c.stats.sessionStart.Store(time.Now().Unix())
			c.stats.lastConnected.Store(time.Now().Unix())
			c.stats.reconnectCount.Add(1)
			c.emit(Event{Type: EventConnected})
			log.Printf("INFO: [%s] Connection established and authenticated. Starting pumps.", c.config.Name)

			session := c.beginSession()
			c.wg.Add(1)
			go c.readPump()
			c.wg.Add(1)
			go c.writePump(session)

			c.wg.Wait() // Wait for pumps to exit, indicating a disconnection.
			c.clearSendQueues()
			c.closeAllLocalConns() // Close all local connections; they can't be resumed across sessions
			c.drainOutboundPending()

			// Fix [P2]: Ensure we clear any stranded queues from the map to prevent memory leaks across sessions
			c.connQueues.Range(func(key, value interface{}) bool {
				c.connQueues.Delete(key)
				return true
			})

			c.stats.sessionStart.Store(0)
			c.emit(Event{Type: EventDisconnected})
			log.Printf("INFO: [%s] Disconnected from Nexus Proxy.", c.config.Name)
			continue
		}

		catErr := categorizeError(err)
		var delay time.Duration

		switch catErr.Category {
		case ErrorTransient:
			c.stats.transientErrors.Add(1)
			delay = backoffDelay(transientRetries)
			log.Printf("WARN: [%s] Transient error: %v. Retry %d in %s", c.config.Name, err, transientRetries+1, delay.Round(time.Millisecond))
			transientRetries++
		case ErrorPermanent:
			c.stats.permanentErrors.Add(1)
			permanentFailures++
			if permanentFailures >= maxPermanentFailures {
				log.Printf("ERROR: [%s] Permanent failure after %d attempts: %v. Stopping.", c.config.Name, permanentFailures, err)
				c.emit(Event{Type: EventError, Error: err, Reason: "permanent_failure"})
				return // Exit loop
			}
			delay = backoffDelay(permanentFailures)
			log.Printf("ERROR: [%s] Permanent error: %v. Attempt %d/%d, retry in %s", c.config.Name, err, permanentFailures, maxPermanentFailures, delay.Round(time.Millisecond))
		case ErrorRateLimit:
			c.stats.rateLimitHits.Add(1)
			delay = maxReconnectDelay
			log.Printf("WARN: [%s] Rate limited. Backing off for %s", c.config.Name, delay)
		}
		c.emit(Event{Type: EventError, Error: err, Reason: catErr.Reason})

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(delay):
		case _, ok := <-networkWakeup:
			if !ok {
				networkWakeup = nil
				// fallback wait
				select {
				case <-c.ctx.Done():
					return
				case <-time.After(delay):
				}
			} else {
				log.Printf("INFO: [%s] Network state changed, retrying immediately.", c.config.Name)
				transientRetries = 0
			}
		}
	}
}

// Stop gracefully shuts down the client and its connections.
func (c *Client) Stop() {
	log.Printf("INFO: [%s] Stopping client...", c.config.Name)

	c.shuttingDown.Store(true)

	// Graceful shutdown of local connections
	c.localConns.Range(func(key, value interface{}) bool {
		if conn, ok := value.(*clientConn); ok {
			state := ConnState(conn.state.Load())
			switch state {
			case ConnStatePending:
				c.transitionToClosed(conn, DisconnectShutdown)
			case ConnStateActive:
				if !conn.transition(ConnStateActive, ConnStateDraining) {
					// State changed concurrently, assume closed or draining
					return true
				}
				c.drainConnection(conn)
			case ConnStateDraining, ConnStateClosed:
				// Already handling
			}
		}
		return true
	})

	// Wait for queues to drain with early exit if all drained
	deadline := time.Now().Add(connectionDrainTimeout + 1*time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if c.queuesEmpty() && c.connectionsEmpty() {
				log.Printf("DEBUG: [%s] All queues drained, proceeding with shutdown", c.config.Name)
				goto forceClose
			}
			if time.Now().After(deadline) {
				log.Printf("WARN: [%s] Drain timeout reached, forcing shutdown", c.config.Name)
				goto forceClose
			}
		}
	}

forceClose:
	if c.socks5Listener != nil {
		_ = c.socks5Listener.Close()
	}
	if c.cancel != nil {
		c.cancel()
	}
}

// queuesEmpty returns true if all send queues are empty.
func (c *Client) queuesEmpty() bool {
	if len(c.controlSend) > 0 {
		return false
	}
	empty := true
	c.connQueues.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan outboundMessage); ok {
			if len(ch) > 0 {
				empty = false
				return false
			}
		}
		return true
	})
	return empty
}

// connectionsEmpty returns true if all connections are closed.
func (c *Client) connectionsEmpty() bool {
	empty := true
	c.localConns.Range(func(key, value interface{}) bool {
		empty = false
		return false
	})
	return empty
}

func (c *Client) getAuthToken(ctx context.Context) (string, error) {
	return c.issueToken(ctx, StageHandshake, "")
}

func (c *Client) issueToken(ctx context.Context, stage TokenStage, nonce string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	provider := c.tokenProvider
	if provider == nil {
		return "", fmt.Errorf("token provider not configured")
	}

	req := TokenRequest{
		Stage:                stage,
		SessionNonce:         nonce,
		BackendName:          c.config.Name,
		Hostnames:            append([]string(nil), c.config.Hostnames...),
		TCPPorts:             append([]int(nil), c.config.TCPPorts...),
		UDPRoutes:            CopyUDPRoutes(c.config.UDPRoutes),
		Weight:               c.config.Weight,
		OutboundAllowed:      c.config.Attestation.OutboundAllowed,
		AllowedOutboundPorts: append([]int(nil), c.config.Attestation.AllowedOutboundPorts...),
	}

	token, err := provider.IssueToken(ctx, req)
	if err != nil {
		return "", fmt.Errorf("token provider failed: %w", err)
	}

	value := strings.TrimSpace(token.Value)
	if value == "" {
		return "", fmt.Errorf("token provider returned empty token")
	}

	return value, nil
}

func (c *Client) connectAndAuthenticate() error {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, c.config.NexusAddress, nil)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}

	handshakeToken, err := c.issueToken(ctx, StageHandshake, "")
	if err != nil {
		ws.Close()
		return fmt.Errorf("fetch handshake token: %w", err)
	}

	if err := ws.WriteMessage(websocket.TextMessage, []byte(handshakeToken)); err != nil {
		ws.Close()
		return fmt.Errorf("send handshake token: %w", err)
	}

	nonce, err := c.awaitChallenge(ws, protocol.ChallengeHandshake)
	if err != nil {
		ws.Close()
		return fmt.Errorf("handshake challenge: %w", err)
	}

	attestedToken, err := c.issueToken(ctx, StageAttest, nonce)
	if err != nil {
		ws.Close()
		return fmt.Errorf("fetch attested token: %w", err)
	}

	if err := ws.WriteMessage(websocket.TextMessage, []byte(attestedToken)); err != nil {
		ws.Close()
		return fmt.Errorf("send attested token: %w", err)
	}

	c.wsMu.Lock()
	c.ws = ws
	c.wsMu.Unlock()

	return nil
}

// closeAllLocalConns closes all active local connections.
// Called on WS session end since connections cannot be resumed across sessions.
func (c *Client) closeAllLocalConns() {
	var wg sync.WaitGroup
	c.localConns.Range(func(key, value interface{}) bool {
		if conn, ok := value.(*clientConn); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.transitionToClosed(conn, DisconnectSessionEnded)
			}()
		}
		return true
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(connectionDrainTimeout + 2*time.Second):
		log.Printf("WARN: [%s] closeAllLocalConns timed out, some connections may not have drained", c.config.Name)
	}
}

func (c *Client) cleanup() {
	// Wait for the health check pump to exit
	c.healthWg.Wait()

	c.closeAllLocalConns()
	log.Printf("INFO: [%s] Client cleanup complete.", c.config.Name)
}

func (c *Client) readPump() {
	defer c.wg.Done()
	c.wsMu.Lock()
	ws := c.ws
	c.wsMu.Unlock()
	defer ws.Close()

	ws.SetReadLimit(maxMessageSize)
	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		msgType, message, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ERROR: [%s] Unexpected close from Nexus: %v", c.config.Name, err)
			}
			return
		}

		// Track received message stats
		c.stats.msgsRecv.Add(1)
		c.stats.bytesRecv.Add(int64(len(message)))

		ws.SetReadDeadline(time.Now().Add(pongWait))

		switch msgType {
		case websocket.BinaryMessage:
			c.handleBinaryMessage(message)
		case websocket.TextMessage:
			if err := c.handleTextMessage(message); err != nil {
				log.Printf("ERROR: [%s] Re-authentication failed: %v", c.config.Name, err)
				return
			}
		case websocket.CloseMessage:
			return
		default:
			log.Printf("WARN: [%s] Ignoring unsupported WebSocket message type %d", c.config.Name, msgType)
		}
	}
}

func (c *Client) handleControlMessage(payload []byte) {
	var msg protocol.ControlMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("WARN: [%s] Failed to unmarshal control message: %v", c.config.Name, err)
		return
	}

	switch msg.Event {
	case protocol.EventConnect:
		// Default to TCP for backward compatibility with older servers
		transport := msg.Transport
		if transport == "" {
			transport = TransportTCP
		} else if transport != TransportTCP && transport != TransportUDP {
			log.Printf("WARN: [%s] Unrecognized transport '%s' for ClientID %s, defaulting to TCP", c.config.Name, transport, msg.ClientID)
			transport = TransportTCP
		}

		normalizedHost := normalizeHostname(msg.Hostname)
		if normalizedHost == "" {
			normalizedHost = msg.Hostname
		}
		log.Printf("INFO: [%s] Received 'connect' for ClientID %s on port %d (hostname: %s, transport: %s, tls:%v).", c.config.Name, msg.ClientID, msg.ConnPort, normalizedHost, transport, msg.IsTLS)

		if msg.ClientIP == "" {
			log.Printf("WARN: [%s] EventConnect for client %s has empty client_ip", c.config.Name, msg.ClientID)
		}
		req := ConnectRequest{
			BackendName:      c.config.Name,
			ClientID:         msg.ClientID,
			Hostname:         normalizedHost,
			OriginalHostname: msg.Hostname,
			Port:             msg.ConnPort,
			ClientIP:         msg.ClientIP,
			IsTLS:            msg.IsTLS,
			Transport:        transport,
		}

		// Create pending connection immediately to buffer early data while dial is in progress
		pendingClient := &clientConn{
			id:       msg.ClientID,
			conn:     nil, // Set after dial completes
			hostname: normalizedHost,
			session:  c.activeSession.Load(),
			quit:     make(chan struct{}),
			drained:  make(chan struct{}),
			writeCh:  make(chan []byte, localConnWriteBuffer),
			flow: flowControl{
				lowWaterMark:  c.config.FlowControl.LowWaterMark,
				highWaterMark: c.config.FlowControl.HighWaterMark,
				maxBuffer:     c.config.FlowControl.MaxBuffer,
			},
			isUDP: transport == TransportUDP,
		}
		pendingClient.state.Store(uint32(ConnStatePending))
		pendingClient.lastActivity.Store(time.Now().Unix())
		c.localConns.Store(msg.ClientID, pendingClient)

		// Create the per-connection queue upfront to avoid memory leaks from late-arriving data
		c.getOrCreateQueue(msg.ClientID)

		// Update stats
		c.stats.pendingConns.Add(1)
		c.stats.totalConns.Add(1)
		c.emit(Event{
			Type:     EventConnectionOpened,
			ClientID: msg.ClientID.String(),
			Hostname: normalizedHost,
		})

		// Spawn goroutine for dial to avoid blocking readPump
		go c.establishLocalConnection(req, pendingClient)

	case protocol.EventDisconnect:
		log.Printf("INFO: [%s] Received 'disconnect' for ClientID: %s. Closing local connection.", c.config.Name, msg.ClientID)
		if val, ok := c.localConns.Load(msg.ClientID); ok {
			if conn, ok := val.(*clientConn); ok {
				if conn.hostname != "" {
					log.Printf("DEBUG: [%s] Disconnecting client %s for hostname %s", c.config.Name, msg.ClientID, conn.hostname)
				}
				c.transitionToClosed(conn, DisconnectNormal)
			}
		}

	case protocol.EventPongClient:
		log.Printf("DEBUG: [%s] Received pong for client %s", c.config.Name, msg.ClientID)
		if val, ok := c.localConns.Load(msg.ClientID); ok {
			if conn, ok := val.(*clientConn); ok {
				conn.cancelPongTimer() // Cancel the pong timeout timer
				conn.pingSent.Store(false)
				conn.lastActivity.Store(time.Now().Unix())
			}
		}

	case protocol.EventOutboundResult:
		if val, ok := c.outboundPending.LoadAndDelete(msg.ClientID); ok {
			ch := val.(chan error)
			if msg.Success {
				ch <- nil
			} else {
				reason := msg.Reason
				if reason == "" {
					reason = "outbound connection failed"
				}
				ch <- fmt.Errorf("%s", reason)
			}
		}
	}
}

func (c *Client) resolveLocalAddress(port int, hostname string) (string, bool) {
	mapping, ok := c.config.PortMappings[port]
	if !ok {
		return "", false
	}
	return mapping.Resolve(hostname)
}

func (c *Client) openBackendConnection(ctx context.Context, req ConnectRequest) (net.Conn, error) {
	handler := c.connectHandler
	if handler == nil {
		return nil, fmt.Errorf("connect handler not configured")
	}

	if ctx == nil {
		ctx = c.ctx
		if ctx == nil {
			ctx = context.Background()
		}
	}

	conn, err := handler(ctx, req)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("connect handler returned nil connection without error")
	}
	return conn, nil
}

func (c *Client) configBasedConnectHandler() ConnectHandler {
	return func(ctx context.Context, req ConnectRequest) (net.Conn, error) {
		localAddr, ok := c.resolveLocalAddress(req.Port, req.Hostname)
		if !ok {
			return nil, ErrNoRoute
		}

		var d net.Dialer
		return d.DialContext(ctx, string(req.Transport), localAddr)
	}
}

// establishLocalConnection dials the local service and completes the pending connection.
// The pendingClient is already stored in localConns to buffer early data during dial.
func (c *Client) establishLocalConnection(req ConnectRequest, pendingClient *clientConn) {
	// Use a timeout context for the dial operation
	dialCtx, cancel := context.WithTimeout(c.ctx, localDialTimeout)
	defer cancel()

	conn, err := c.openBackendConnection(dialCtx, req)
	if err != nil {
		if dialCtx.Err() != nil {
			log.Printf("ERROR: [%s] Dial timeout for client %s to hostname '%s'", c.config.Name, req.ClientID, pendingClient.hostname)
		} else if errors.Is(err, ErrNoRoute) {
			log.Printf("ERROR: [%s] No route configured for hostname '%s' on port %d", c.config.Name, pendingClient.hostname, req.Port)
		} else {
			log.Printf("ERROR: [%s] Failed to establish local connection for client %s: %v", c.config.Name, req.ClientID, err)
		}
		// Clean up pending connection and notify Nexus
		c.transitionToClosed(pendingClient, DisconnectDialFailed)
		return
	}

	// Check if the session that created this connection is still active after dial
	if !pendingClient.session.IsConnected() {
		conn.Close()
		c.transitionToClosed(pendingClient, DisconnectShutdown)
		log.Printf("DEBUG: [%s] Session ended during dial for client %s, closing connection", c.config.Name, req.ClientID)
		return
	}

	// Check if pending connection was closed while we were dialing (e.g., disconnect received, buffer overflow)
	select {
	case <-pendingClient.quit:
		conn.Close()
		c.localConns.Delete(req.ClientID) // Ensure stale entry is removed
		log.Printf("DEBUG: [%s] Pending connection closed during dial for client %s", c.config.Name, req.ClientID)
		// Best-effort disconnect notification — only if the connection's
		// owning session is still alive to avoid stale leaks after reconnect.
		if pendingClient.session.IsConnected() {
			if err := c.sendDisconnectMessage(req.ClientID, DisconnectShutdown); err != nil {
				log.Printf("DEBUG: [%s] Failed to send disconnect for client %s after pending close: %v", c.config.Name, req.ClientID, err)
			}
		}
		return
	default:
	}

	// Complete the pending connection with mutex to prevent data race with safeClose
	pendingClient.connMu.Lock()
	pendingClient.conn = conn
	pendingClient.connMu.Unlock()

	// Re-check if quit was closed during the conn assignment race window.
	// If safeClose() ran while conn was nil, closeOnce consumed with nil conn,
	// so we must close the newly assigned conn here to prevent FD leak.
	select {
	case <-pendingClient.quit:
		conn.Close()
		c.localConns.Delete(req.ClientID)
		log.Printf("DEBUG: [%s] Connection closed during conn assignment for client %s", c.config.Name, req.ClientID)
		return
	default:
	}

	if !pendingClient.transition(ConnStatePending, ConnStateActive) {
		conn.Close()
		log.Printf("DEBUG: [%s] Failed to transition from Pending to Active for client %s, closing connection", c.config.Name, req.ClientID)
		return
	}
	pendingClient.lastActivity.Store(time.Now().Unix())

	// Update stats: pending -> active
	c.stats.pendingConns.Add(-1)
	c.stats.activeConns.Add(1)

	// Start reader (local -> Nexus) and writer (Nexus -> local) pumps
	go c.copyLocalToNexus(pendingClient)
	go c.writeToLocal(pendingClient)
}

// transitionToClosed transitions the connection to Closed state and performs cleanup.
func (c *Client) transitionToClosed(conn *clientConn, reason DisconnectReason) {
	// Attempt transition to Closed
	// We allow transition from ANY state to Closed
	// Loop to handle CAS
	var fromState ConnState
	for {
		oldState := conn.state.Load()
		if oldState == uint32(ConnStateClosed) {
			return // Already closed
		}
		fromState = ConnState(oldState)
		if conn.transition(fromState, ConnStateClosed) {
			break
		}
	}

	// Update stats based on previous state
	switch fromState {
	case ConnStatePending:
		c.stats.pendingConns.Add(-1)
	case ConnStateActive:
		c.stats.activeConns.Add(-1)
	case ConnStateDraining:
		c.stats.activeConns.Add(-1) // Draining connections were counted as active
	}

	// Track dropped connections for non-normal reasons
	if reason != DisconnectNormal {
		c.stats.droppedConns.Add(1)
	}

	// Aggregate UDP dropped packets
	if conn.isUDP {
		dropped := conn.droppedPackets.Load()
		if dropped > 0 {
			c.stats.udpDroppedPackets.Add(dropped)
		}
	}

	conn.safeClose()

	// finalize performs cleanup. When sendDisconnect is true, it also notifies
	// the relay. If the session ended (WebSocket closed), the relay already
	// knows — sending a stale disconnect could leak onto a reconnected session.
	finalize := func(sendDisconnect bool) {
		c.cleanupConnectionQueue(conn.id)
		c.localConns.Delete(conn.id)
		if sendDisconnect {
			if err := c.sendDisconnectMessage(conn.id, reason); err != nil {
				log.Printf("DEBUG: [%s] Failed to send disconnect for client %s (%s): %v", c.config.Name, conn.id, reason, err)
			}
		}
		c.emit(Event{
			Type:     EventConnectionClosed,
			ClientID: conn.id.String(),
			Hostname: conn.hostname,
			Reason:   string(reason),
		})
	}

	// Only drain connections that were active and may have pending outbound data.
	// Pending connections (dial in progress / dial failed) have no outbound queue.
	needsDrain := fromState == ConnStateActive || fromState == ConnStateDraining

	// Use the connection's owning session, not the global active session.
	// This prevents stale disconnect messages from leaking onto a successor
	// session when a goroutine from an old session runs after reconnect.
	sessionDone := conn.session.Done()

	if needsDrain && sessionDone != nil {
		// Drain asynchronously to avoid blocking the caller — transitionToClosed
		// can be called from readPump (e.g., relay-initiated disconnect) and must
		// not stall the WebSocket reader while waiting for writePump to flush.
		go func() {
			// Signal writePump to drain this connection's outbound queue.
			if conn.signaled.CompareAndSwap(false, true) {
				select {
				case c.dataReady <- conn.id:
				case <-sessionDone:
				case <-c.ctx.Done():
				}
			}

			drainTimer := time.NewTimer(connectionDrainTimeout)
			select {
			case <-conn.drained:
				// Re-check session liveness — if the session ended between
				// drain completion and now, skip disconnect to avoid leaking
				// a stale control frame onto a reconnected session.
				select {
				case <-sessionDone:
					finalize(false)
				default:
					finalize(true)
				}
			case <-sessionDone:
				// Session ended — relay already knows, skip disconnect.
				log.Printf("WARN: [%s] Write pump exited during drain for ClientID %s", c.config.Name, conn.id)
				finalize(false)
			case <-drainTimer.C:
				log.Printf("WARN: [%s] Drain timeout for ClientID %s, proceeding with disconnect", c.config.Name, conn.id)
				// Re-check session liveness — if both drainTimer and sessionDone
				// fired simultaneously, Go may have picked the timer case.
				select {
				case <-sessionDone:
					finalize(false)
				default:
					finalize(true)
				}
			case <-c.ctx.Done():
				// Client shutting down — relay connection is gone.
				finalize(false)
			}
			drainTimer.Stop()
		}()
		return
	}

	// No drain needed or session already gone — clean up immediately.
	// Only send disconnect if the session captured above is still alive;
	// otherwise the relay already knows via the broken WebSocket and
	// sending could leak a stale frame onto a reconnected session.
	sendDisconnect := false
	if sessionDone != nil {
		select {
		case <-sessionDone:
			// Session ended — relay already knows.
		default:
			sendDisconnect = true
		}
	}
	finalize(sendDisconnect)
}

func (c *Client) getOrCreateQueue(clientID uuid.UUID) chan outboundMessage {
	if v, ok := c.connQueues.Load(clientID); ok {
		return v.(chan outboundMessage)
	}
	queue := make(chan outboundMessage, 256)
	actual, loaded := c.connQueues.LoadOrStore(clientID, queue)
	if loaded {
		return actual.(chan outboundMessage)
	}
	return queue
}

// getQueue returns the queue for a connection, or nil if not found.
// Use this when you don't want to create a queue for stale notifications.
func (c *Client) getQueue(clientID uuid.UUID) chan outboundMessage {
	if v, ok := c.connQueues.Load(clientID); ok {
		return v.(chan outboundMessage)
	}
	return nil
}

func (c *Client) cleanupConnectionQueue(clientID uuid.UUID) {
	if v, ok := c.connQueues.LoadAndDelete(clientID); ok {
		queue := v.(chan outboundMessage)
		// Drain and discard any remaining messages
		for {
			select {
			case <-queue:
				// Discard message
			default:
				// Queue drained, done
				return
			}
		}
	}
}

// writeToLocal reads from the connection's write channel and writes to the local service.
// This runs in a dedicated goroutine per connection to avoid blocking readPump.
// Note: Cleanup (safeClose, localConns.Delete, disconnect message) is handled by copyLocalToNexus.
func (c *Client) writeToLocal(client *clientConn) {
	defer c.transitionToClosed(client, DisconnectNormal)

	for {
		select {
		case <-client.quit:
			return
		case data := <-client.writeCh:
			// Set write deadline based on transport type
			// UDP: short timeout (1ms) - drop if blocked
			// TCP: longer timeout (10s) - connection health matters
			writeTimeout := localWriteTimeout
			if client.isUDP {
				writeTimeout = 1 * time.Millisecond
			}
			if tc, ok := client.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
				tc.SetWriteDeadline(time.Now().Add(writeTimeout))
			}
			_, err := client.conn.Write(data)
			if err != nil {
				// UDP: Drop packet and continue (best-effort semantics)
				if client.isUDP {
					client.droppedPackets.Add(1)
					continue
				}
				// TCP: safeClose guarantees close(quit) happens-before conn.Close(),
				// so if quit is closed the error is from intentional teardown.
				select {
				case <-client.quit:
				default:
					if client.hostname != "" {
						log.Printf("ERROR: [%s] Failed to write data to local connection for ClientID %s (%s): %v", c.config.Name, client.id, client.hostname, err)
					} else {
						log.Printf("ERROR: [%s] Failed to write data to local connection for ClientID %s: %v", c.config.Name, client.id, err)
					}
				}
				return
			}

			// TCP Flow Control: Update level and check for resume
			if !client.isUDP {
				newLevel := int(client.flow.level.Add(-1))
				if client.flow.paused.Load() && newLevel <= client.flow.lowWaterMark {
					client.flow.paused.Store(false)
					c.stats.pausedConns.Add(-1)
					c.enqueueControl(resumeStreamMessage(client.id))
					c.emit(Event{
						Type:     EventResumed,
						ClientID: client.id.String(),
						Hostname: client.hostname,
					})
				}
			}

			// Optimized Drain Check: If draining and no more data, exit immediately
			if client.state.Load() == uint32(ConnStateDraining) {
				if len(client.writeCh) == 0 {
					return
				}
			}
		}
	}
}

func (c *Client) handleDataMessage(payload []byte) {
	if len(payload) < protocol.ClientIDLength {
		return
	}

	var clientID uuid.UUID
	copy(clientID[:], payload[:protocol.ClientIDLength])

	// Copy data to avoid race with buffer reuse in read loop
	data := make([]byte, len(payload)-protocol.ClientIDLength)
	copy(data, payload[protocol.ClientIDLength:])

	val, ok := c.localConns.Load(clientID)
	if ok {
		if conn, ok := val.(*clientConn); ok {
			state := ConnState(conn.state.Load())
			// Fix [P1]: Allow Pending state to buffer data, otherwise we lose packets during dial
			if state != ConnStateActive && state != ConnStatePending {
				if state == ConnStateDraining {
					log.Printf("DEBUG: Data received for draining connection %s, discarding", clientID)
				}
				return
			}

			conn.lastActivity.Store(time.Now().Unix()) // Reset activity timer

			// UDP: Direct write when active, best-effort buffering when pending
			if conn.isUDP {
				if state == ConnStateActive && conn.conn != nil {
					// Direct write with short timeout (UDP semantics: drop if blocked)
					conn.conn.SetWriteDeadline(time.Now().Add(1 * time.Millisecond))
					_, err := conn.conn.Write(data)
					if err != nil {
						conn.droppedPackets.Add(1)
						// Don't close connection for UDP - just drop and continue
					}
					return
				}
				// Pending state: Best-effort buffering (no flow control for UDP)
				select {
				case conn.writeCh <- data:
					// Buffered for delivery after dial completes
				default:
					// Buffer full - drop packet (UDP semantics)
					conn.droppedPackets.Add(1)
				}
				return
			}

			// TCP: Flow control with pause/resume
			currentLevel := int(conn.flow.level.Add(1))
			if currentLevel >= conn.flow.highWaterMark && !conn.flow.paused.Load() {
				conn.flow.paused.Store(true)
				c.stats.pausedConns.Add(1)
				c.enqueueControl(pauseStreamMessage(conn.id, "buffer_full"))
				c.emit(Event{
					Type:     EventPaused,
					ClientID: conn.id.String(),
					Hostname: conn.hostname,
					Reason:   "buffer_full",
				})
			}

			// Non-blocking enqueue to write channel
			select {
			case conn.writeCh <- data:
				// Successfully enqueued
			default:
				// Hard limit reached - this shouldn't happen if pause works correctly
				conn.flow.level.Add(-1) // Revert the level increment
				log.Printf("WARN: [%s] Write buffer full for ClientID %s, closing connection", c.config.Name, clientID)
				c.transitionToClosed(conn, DisconnectBufferFull)
			}
		}
	} else {
		log.Printf("WARN: [%s] No local connection found for ClientID %s. Data will be dropped. Disconnect will be sent to proxy", c.config.Name, clientID)
		if err := c.sendDisconnectMessage(clientID, DisconnectUnknown); err != nil {
			log.Printf("DEBUG: [%s] Failed to enqueue disconnect for ClientID %s: %v", c.config.Name, clientID, err)
		}
	}
}

func (c *Client) copyLocalToNexus(client *clientConn) {
	defer func() {
		// Use transitionToClosed to ensure consistent cleanup state
		c.transitionToClosed(client, DisconnectNormal)
	}()

	buf := make([]byte, writeToNexusBufferSize)
	for {
		select {
		case <-client.quit:
			return
		default:
			n, err := client.conn.Read(buf)

			// Per io.Reader contract, process n > 0 bytes before considering err.
			if n > 0 {
				client.lastActivity.Store(time.Now().Unix()) // Reset activity timer

				header := make([]byte, 1+protocol.ClientIDLength)
				header[0] = protocol.ControlByteData
				copy(header[1:], client.id[:])
				message := append(header, buf[:n]...)

				outbound := outboundMessage{
					messageType: websocket.BinaryMessage,
					payload:     message,
				}

				if err := c.enqueueData(outbound); err != nil {
					if !errors.Is(err, errSessionInactive) && !errors.Is(err, context.Canceled) {
						log.Printf("WARN: [%s] Failed to enqueue data for ClientID %s: %v", c.config.Name, client.id, err)
					}
					return
				}
			}

			if err != nil {
				// safeClose guarantees close(quit) happens-before conn.Close(),
				// so if quit is closed the error is from intentional teardown.
				if err != io.EOF {
					select {
					case <-client.quit:
					default:
						if client.hostname != "" {
							log.Printf("WARN: [%s] Error reading from local connection for ClientID %s (%s): %v", c.config.Name, client.id, client.hostname, err)
						} else {
							log.Printf("WARN: [%s] Error reading from local connection for ClientID %s: %v", c.config.Name, client.id, err)
						}
					}
				}
				return
			}
		}
	}
}

func (c *Client) clearSendQueues() {
	// Drain control queue
	for {
		select {
		case <-c.controlSend:
		default:
			goto drainDataReady
		}
	}
drainDataReady:
	// Drain dataReady channel (Fix [P3])
	for {
		select {
		case <-c.dataReady:
		default:
			return
		}
	}
}

func (c *Client) beginSession() *Session {
	s := &Session{
		done: make(chan struct{}),
	}
	s.connected.Store(true)
	c.activeSession.Store(s)
	return s
}

func (c *Client) enqueueData(message outboundMessage) error {
	// Parse ClientID from message to ensure fair queuing
	// message payload: [ControlByte + ClientID + Data...]
	if len(message.payload) < protocol.MessageHeaderLength {
		return fmt.Errorf("invalid data message length")
	}
	var clientID uuid.UUID
	copy(clientID[:], message.payload[1:protocol.MessageHeaderLength])

	// Use getQueue (not getOrCreateQueue) to avoid memory leaks from late-arriving data
	// for closed connections. Queue is created upfront in handleControlMessage.
	queue := c.getQueue(clientID)
	if queue == nil {
		// Connection already closed/cleaned up - drop the data
		return fmt.Errorf("connection not found for ClientID %s", clientID)
	}

	select {
	case queue <- message:
		// Notify writePump using signaled flag to prevent duplicate notifications.
		// The signaled flag ensures only one notification is in dataReady at a time
		// per connection, preventing busy connections from flooding the channel.
		if conn, ok := c.localConns.Load(clientID); ok {
			if cConn, ok := conn.(*clientConn); ok {
				if cConn.signaled.CompareAndSwap(false, true) {
					// We own the notification responsibility - must push to dataReady
					// Block here to apply backpressure (P3 fix: avoid unbounded goroutines)
					select {
					case c.dataReady <- clientID:
						// Successfully notified
					case <-c.ctx.Done():
						// Context canceled, stop trying
						cConn.signaled.Store(false) // Reset flag since we didn't notify
					}
				}
				// If CAS failed, another notification is already pending - no action needed
			}
		}
		return nil
	case <-time.After(enqueueTimeout):
		c.stats.enqueueTimeouts.Add(1)
		return fmt.Errorf("enqueue timeout")
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *Client) enqueueControl(message outboundMessage) error {
	return c.enqueue(message, c.controlSend)
}

func (c *Client) enqueue(message outboundMessage, queue chan outboundMessage) error {
	s := c.activeSession.Load()
	if s == nil || !s.IsConnected() {
		return errSessionInactive
	}

	select {
	case queue <- message:
		return nil
	case <-time.After(enqueueTimeout):
		c.stats.enqueueTimeouts.Add(1)
		return fmt.Errorf("enqueue timeout")
	case <-s.Done():
		return errSessionInactive
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}


func (c *Client) sendControlMessage(event protocol.EventType, clientID uuid.UUID) error {
	msg := protocol.ControlMessage{
		Event:    event,
		ClientID: clientID,
	}

	payload, err := c.marshalJSON(msg)
	if err != nil {
		log.Printf("ERROR: [%s] Failed to marshal control message '%s' for client %s: %v", c.config.Name, event, clientID, err)
		return err
	}
	header := []byte{protocol.ControlByteControl}
	message := append(header, payload...)
	outbound := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     message,
	}

	if err := c.enqueueControl(outbound); err != nil {
		if errors.Is(err, errSessionInactive) {
			log.Printf("DEBUG: [%s] Dropping control message '%s' for client %s: session inactive", c.config.Name, event, clientID)
		} else if errors.Is(err, context.Canceled) {
			log.Printf("DEBUG: [%s] Dropping control message '%s' for client %s: client context canceled", c.config.Name, event, clientID)
		} else {
			log.Printf("WARN: [%s] Failed to enqueue control message '%s' for client %s: %v", c.config.Name, event, clientID, err)
		}
		return err
	}
	return nil
}

// sendDisconnectMessage sends a disconnect message with a reason code to Nexus.
func (c *Client) sendDisconnectMessage(clientID uuid.UUID, reason DisconnectReason) error {
	msg := protocol.ControlMessage{
		Event:    protocol.EventDisconnect,
		ClientID: clientID,
		Reason:   string(reason),
	}

	payload, err := c.marshalJSON(msg)
	if err != nil {
		log.Printf("ERROR: [%s] Failed to marshal disconnect for client %s: %v", c.config.Name, clientID, err)
		return err
	}
	header := []byte{protocol.ControlByteControl}
	message := append(header, payload...)
	outbound := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     message,
	}

	if err := c.enqueueControl(outbound); err != nil {
		if errors.Is(err, errSessionInactive) {
			log.Printf("DEBUG: [%s] Dropping disconnect for client %s (%s): session inactive", c.config.Name, clientID, reason)
		} else if errors.Is(err, context.Canceled) {
			log.Printf("DEBUG: [%s] Dropping disconnect for client %s (%s): context canceled", c.config.Name, clientID, reason)
		} else {
			log.Printf("WARN: [%s] Failed to enqueue disconnect for client %s (%s): %v", c.config.Name, clientID, reason, err)
		}
		return err
	}
	return nil
}

func (c *Client) writeMessage(ws *websocket.Conn, message outboundMessage) error {
	if err := ws.WriteMessage(message.messageType, message.payload); err != nil {
		log.Printf("ERROR: [%s] Failed to write message to Nexus: %v", c.config.Name, err)
		return err
	}
	// Track stats
	c.stats.msgsSent.Add(1)
	c.stats.bytesSent.Add(int64(len(message.payload)))
	return nil
}

func (c *Client) writePump(s *Session) {
	defer c.wg.Done()
	c.wsMu.Lock()
	ws := c.ws
	c.wsMu.Unlock()

	ticker := time.NewTicker(pingInterval)
	defer func() {
		s.Close()
		c.activeSession.Store(nil)
		c.clearSendQueues()
		if ws != nil {
			ws.Close()
		}
		ticker.Stop()
		log.Printf("DEBUG: [%s] Write pump stopped.", c.config.Name)
	}()

	for {
		// Priority 1: Control messages
		select {
		case message, ok := <-c.controlSend:
			if !ok {
				return
			}
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.writeMessage(ws, message); err != nil {
				return
			}
			continue
		default:
		}

		// Priority 2: Data messages (Fair Queuing)
		select {
		case message, ok := <-c.controlSend:
			if !ok {
				ws.SetWriteDeadline(time.Now().Add(writeWait))
				ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.writeMessage(ws, message); err != nil {
				return
			}
		case clientID := <-c.dataReady:
			c.drainConnectionQueue(ws, clientID, 4) // Drain up to 4 messages per turn
		case <-ticker.C:
			// Send a WebSocket-level ping to keep the connection alive.
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				log.Printf("ERROR: [%s] Failed to write ping to Nexus: %v", c.config.Name, err)
				return // Terminate the pump and session.
			}
		case <-s.done:
			return
		case <-c.ctx.Done():
			// The session context was canceled.
			return
		}
	}
}

func (c *Client) drainConnectionQueue(ws *websocket.Conn, clientID uuid.UUID, maxMessages int) {
	queue := c.getQueue(clientID)
	if queue == nil {
		// Stale notification - connection was already cleaned up
		return
	}

	drained := 0
	queueEmpty := false
	for i := 0; i < maxMessages; i++ {
		select {
		case msg := <-queue:
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.writeMessage(ws, msg); err != nil {
				return // Write error will be caught by main loop ping or next write
			}
			drained++
		default:
			queueEmpty = true
			goto cleanup // P0 fix: Don't return early, go through signaling cleanup
		}
	}

cleanup:
	conn, ok := c.localConns.Load(clientID)
	if !ok {
		return // Connection gone
	}
	cConn := conn.(*clientConn)

	// Log when drain actually flushed data for a closing connection (observability).
	if drained > 0 && cConn.state.Load() == uint32(ConnStateClosed) {
		log.Printf("INFO: [%s] Drained %d messages for ClientID %s before disconnect", c.config.Name, drained, clientID)
	}

	if queueEmpty || drained < maxMessages {
		// Queue is empty - clear signaled flag so we can be notified again
		cConn.signaled.Store(false)

		// Race check: Use select to safely check if data arrived after we thought queue was empty
		select {
		case msg := <-queue:
			// Race: new data arrived - send it immediately to preserve ordering
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.writeMessage(ws, msg); err != nil {
				return // Write error will be caught by main loop
			}
			// Re-signal since there might be more data
			if cConn.signaled.CompareAndSwap(false, true) {
				select {
				case c.dataReady <- clientID:
				default:
					go func(id uuid.UUID) {
						select {
						case c.dataReady <- id:
						case <-c.ctx.Done():
						}
					}(clientID)
				}
			}
		default:
			// Truly empty, signaled flag is cleared.
			// If connection is closed, signal that drain is complete.
			if cConn.state.Load() == uint32(ConnStateClosed) {
				cConn.closeOnDrained()
			}
		}
	} else {
		// Processed maxMessages but queue may have more - re-queue for round-robin fairness
		select {
		case c.dataReady <- clientID:
		default:
			// Must not drop continuation signal
			go func(id uuid.UUID) {
				select {
				case c.dataReady <- id:
				case <-c.ctx.Done():
				}
			}(clientID)
		}
	}
}

func pauseStreamMessage(clientID uuid.UUID, reason string) outboundMessage {
	msg := protocol.ControlMessage{
		Event:    protocol.EventPauseStream,
		ClientID: clientID,
		Reason:   reason,
	}
	payload, _ := json.Marshal(msg)
	return outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append([]byte{protocol.ControlByteControl}, payload...),
	}
}

func resumeStreamMessage(clientID uuid.UUID) outboundMessage {
	msg := protocol.ControlMessage{
		Event:    protocol.EventResumeStream,
		ClientID: clientID,
	}
	payload, _ := json.Marshal(msg)
	return outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append([]byte{protocol.ControlByteControl}, payload...),
	}
}

func (c *Client) healthCheckPump() {
	if !c.config.HealthChecks.Enabled {
		return
	}

	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	inactivityTimeout := time.Duration(c.config.HealthChecks.InactivityTimeout) * time.Second
	pongTimeout := time.Duration(c.config.HealthChecks.PongTimeout) * time.Second

	for {
		select {
		case <-ticker.C:
			c.localConns.Range(func(key, value interface{}) bool {
				conn, ok := value.(*clientConn)
				if !ok {
					return true
				}

				if conn.pingSent.Load() {
					// Ping was sent, but we haven't received a pong yet.
					// We let the pong timeout handle the cleanup.
					return true
				}

				if time.Since(time.Unix(conn.lastActivity.Load(), 0)) > inactivityTimeout {
					if conn.hostname != "" {
						log.Printf("DEBUG: [%s] Client %s (%s) is idle, sending ping.", c.config.Name, conn.id, conn.hostname)
					} else {
						log.Printf("DEBUG: [%s] Client %s is idle, sending ping.", c.config.Name, conn.id)
					}
					conn.pingSent.Store(true)
					if err := c.sendControlMessage(protocol.EventPingClient, conn.id); err != nil {
						conn.pingSent.Store(false)
						log.Printf("DEBUG: [%s] Failed to send ping for client %s: %v", c.config.Name, conn.id, err)
					}

					// Start a cancellable timer to check for the pong.
					conn.setPongTimer(pongTimeout, func() {
						if conn.pingSent.Load() {
							// Pong was not received in time.
							if conn.hostname != "" {
								log.Printf("WARN: [%s] Did not receive pong for idle client %s (%s) within %s. Closing connection.", c.config.Name, conn.id, conn.hostname, pongTimeout)
							} else {
								log.Printf("WARN: [%s] Did not receive pong for idle client %s within %s. Closing connection.", c.config.Name, conn.id, pongTimeout)
							}
							c.transitionToClosed(conn, DisconnectTimeout)
						}
					})
				}
				return true
			})
		case <-c.ctx.Done():
			return
		}
	}
}
func (c *Client) awaitChallenge(ws *websocket.Conn, expectedType protocol.ChallengeType) (string, error) {
	if err := ws.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return "", err
	}
	defer ws.SetReadDeadline(time.Time{})

	messageType, payload, err := ws.ReadMessage()
	if err != nil {
		return "", err
	}
	if messageType != websocket.TextMessage {
		return "", fmt.Errorf("expected text message during handshake, got type %d", messageType)
	}

	var challenge protocol.ChallengeMessage
	if err := json.Unmarshal(payload, &challenge); err != nil {
		return "", fmt.Errorf("decode challenge: %w", err)
	}
	if challenge.Type != expectedType {
		return "", fmt.Errorf("unexpected challenge type %q", challenge.Type)
	}
	if strings.TrimSpace(challenge.Nonce) == "" {
		return "", fmt.Errorf("challenge missing nonce")
	}
	return challenge.Nonce, nil
}
func (c *Client) handleBinaryMessage(message []byte) {
	if len(message) < 1 {
		log.Printf("WARN: [%s] Received empty binary message from Nexus", c.config.Name)
		return
	}

	controlByte := message[0]
	payload := message[1:]

	switch controlByte {
	case protocol.ControlByteControl:
		c.handleControlMessage(payload)
	case protocol.ControlByteData:
		c.handleDataMessage(payload)
	default:
		log.Printf("ERROR: [%s] Received unknown control byte: %d", c.config.Name, controlByte)
	}
}

func (c *Client) handleTextMessage(message []byte) error {
	var challenge protocol.ChallengeMessage
	if err := json.Unmarshal(message, &challenge); err != nil {
		log.Printf("WARN: [%s] Failed to decode text message from Nexus: %v", c.config.Name, err)
		return nil
	}

	switch challenge.Type {
	case protocol.ChallengeReauth:
		if strings.TrimSpace(challenge.Nonce) == "" {
			return fmt.Errorf("reauth challenge missing nonce")
		}
		return c.handleReauthChallenge(challenge.Nonce)
	case protocol.ChallengeHandshake:
		// Should not occur after initial handshake; ignore quietly.
		log.Printf("WARN: [%s] Received unexpected handshake challenge after session establishment", c.config.Name)
	default:
		log.Printf("WARN: [%s] Ignoring unknown text message type '%s' from Nexus", c.config.Name, challenge.Type)
	}
	return nil
}

func (c *Client) handleReauthChallenge(nonce string) error {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	c.emit(Event{Type: EventReauthStarted})

	token, err := c.issueToken(ctx, StageReauth, nonce)
	if err != nil {
		c.emit(Event{Type: EventError, Error: err, Reason: "reauth_token_failed"})
		return fmt.Errorf("issue reauth token: %w", err)
	}

	outbound := outboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(token),
	}

	if err := c.enqueueControl(outbound); err != nil {
		c.emit(Event{Type: EventError, Error: err, Reason: "reauth_enqueue_failed"})
		return fmt.Errorf("enqueue reauth token: %w", err)
	}

	c.emit(Event{Type: EventReauthCompleted})
	return nil
}

// --- Outbound proxy (SOCKS5) ---

const outboundConnectTimeout = 15 * time.Second

// startSocks5Listener starts the SOCKS5 listener if configured. It is
// non-fatal if the listener fails to bind.
func (c *Client) startSocks5Listener() {
	addr := c.config.Socks5ListenAddr
	if addr == "" {
		return
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("ERROR: [%s] Invalid socks5ListenAddr %q: %v", c.config.Name, addr, err)
		return
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		log.Printf("WARN: [%s] SOCKS5 listener bound to non-loopback address %s — unauthenticated access from the network", c.config.Name, addr)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("ERROR: [%s] Failed to start SOCKS5 listener on %s: %v", c.config.Name, addr, err)
		return
	}
	c.socks5Listener = ln
	log.Printf("INFO: [%s] SOCKS5 outbound proxy listening on %s", c.config.Name, addr)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-c.ctx.Done():
					return
				default:
				}
				if !errors.Is(err, net.ErrClosed) {
					log.Printf("WARN: [%s] SOCKS5 accept error: %v", c.config.Name, err)
				}
				return
			}
			go c.handleSocks5Conn(conn)
		}
	}()
}

// handleSocks5Conn processes a single SOCKS5 CONNECT request.
// On success, conn ownership transfers to the bidirectional relay goroutines.
// On failure, conn is closed explicitly in each error path.
func (c *Client) handleSocks5Conn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := socks5Handshake(conn); err != nil {
		log.Printf("WARN: [%s] SOCKS5 handshake failed: %v", c.config.Name, err)
		_ = conn.Close()
		return
	}

	host, port, err := socks5ReadConnect(conn)
	if err != nil {
		log.Printf("WARN: [%s] SOCKS5 connect request failed: %v", c.config.Name, err)
		_ = conn.Close()
		return
	}
	targetAddr := socks5TargetAddr(host, port)

	// Clear the deadline set for the handshake.
	_ = conn.SetDeadline(time.Time{})

	// Capture session once for both the pre-flight check and binding.
	sess := c.activeSession.Load()
	if sess == nil || !sess.IsConnected() {
		log.Printf("WARN: [%s] SOCKS5 outbound to %s rejected: not connected to proxy", c.config.Name, targetAddr)
		_ = socks5SendReply(conn, socks5RepGeneralFailure)
		_ = conn.Close()
		return
	}

	clientID := uuid.New()

	// Create pending clientConn to buffer any early data from the proxy.
	pendingClient := &clientConn{
		id:      clientID,
		conn:    conn,
		session: sess,
		quit:    make(chan struct{}),
		drained: make(chan struct{}),
		writeCh: make(chan []byte, localConnWriteBuffer),
		flow: flowControl{
			lowWaterMark:  c.config.FlowControl.LowWaterMark,
			highWaterMark: c.config.FlowControl.HighWaterMark,
			maxBuffer:     c.config.FlowControl.MaxBuffer,
		},
	}
	pendingClient.state.Store(uint32(ConnStatePending))
	pendingClient.lastActivity.Store(time.Now().Unix())
	c.localConns.Store(clientID, pendingClient)
	c.getOrCreateQueue(clientID)

	c.stats.pendingConns.Add(1)
	c.stats.totalConns.Add(1)
	c.emit(Event{
		Type:     EventConnectionOpened,
		ClientID: clientID.String(),
	})

	// Register a channel for the proxy's result.
	resultCh := make(chan error, 1)
	c.outboundPending.Store(clientID, resultCh)

	fail := func(reason DisconnectReason, rep byte) {
		c.outboundPending.Delete(clientID)
		// Send the SOCKS5 error reply before closing so the client gets
		// a proper RFC 1928 response instead of EOF/reset.
		_ = socks5SendReply(conn, rep)
		c.transitionToClosed(pendingClient, reason)
	}

	// Send the outbound connect request.
	msg := protocol.ControlMessage{
		Event:      protocol.EventOutboundConnect,
		ClientID:   clientID,
		TargetAddr: targetAddr,
	}
	payload, err := c.marshalJSON(msg)
	if err != nil {
		log.Printf("ERROR: [%s] Failed to marshal outbound connect for %s: %v", c.config.Name, targetAddr, err)
		fail(DisconnectDialFailed, socks5RepGeneralFailure)
		return
	}
	header := []byte{protocol.ControlByteControl}
	outbound := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     append(header, payload...),
	}
	if err := c.enqueueControl(outbound); err != nil {
		log.Printf("WARN: [%s] Failed to enqueue outbound connect for %s: %v", c.config.Name, targetAddr, err)
		fail(DisconnectDialFailed, socks5RepGeneralFailure)
		return
	}

	// Wait for the proxy's result.
	timer := time.NewTimer(outboundConnectTimeout)
	defer timer.Stop()

	select {
	case err := <-resultCh:
		if err != nil {
			log.Printf("WARN: [%s] Outbound connect to %s failed: %v", c.config.Name, targetAddr, err)
			fail(DisconnectDialFailed, socks5RepConnRefused)
			return
		}
	case <-timer.C:
		log.Printf("WARN: [%s] Outbound connect to %s timed out", c.config.Name, targetAddr)
		fail(DisconnectTimeout, socks5RepGeneralFailure)
		return
	case <-c.ctx.Done():
		fail(DisconnectShutdown, socks5RepGeneralFailure)
		return
	}

	// Success — send SOCKS5 reply and start bidirectional relay.
	if err := socks5SendReply(conn, socks5RepSuccess); err != nil {
		log.Printf("WARN: [%s] Failed to send SOCKS5 success reply for %s: %v", c.config.Name, targetAddr, err)
		fail(DisconnectLocalError, socks5RepGeneralFailure)
		return
	}

	log.Printf("INFO: [%s] SOCKS5 outbound connection %s to %s established", c.config.Name, clientID, targetAddr)

	// Transition to active and start the relay goroutines (same as inbound).
	if !pendingClient.transition(ConnStatePending, ConnStateActive) {
		// Concurrent session teardown already moved to ConnStateClosed
		// via transitionToClosed, which handled stats, safeClose, and cleanup.
		return
	}
	c.stats.pendingConns.Add(-1)
	c.stats.activeConns.Add(1)

	go c.writeToLocal(pendingClient)
	go c.copyLocalToNexus(pendingClient)
}

// drainOutboundPending unblocks all goroutines waiting for outbound results
// by sending errors to their channels. Called on session teardown.
func (c *Client) drainOutboundPending() {
	c.outboundPending.Range(func(key, value interface{}) bool {
		c.outboundPending.Delete(key)
		if ch, ok := value.(chan error); ok {
			select {
			case ch <- fmt.Errorf("session disconnected"):
			default:
			}
		}
		return true
	})
}
