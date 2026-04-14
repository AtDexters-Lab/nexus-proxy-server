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

	enqueueTimeout   = 5 * time.Second
	controlQueueSize = 64
	// connectionDrainTimeout bounds the wait in transitionToClosed for
	// the per-connection outbound queue to drain before force-closing.
	// Sized so a slow downstream peer can flush the full 256-slot
	// queue (4 MB at 16 KB/frame) at residential-grade rates: 30 s ÷
	// 256 frames = ~120 ms/frame budget, comfortable for any peer
	// faster than ~130 KB/s. Smaller values truncate large response
	// tails on every graceful close (Codex P1 from the v0.3.10 review).
	// Stuck-connection cleanup latency is bounded by this timeout, so
	// don't push it past the order of "user-tolerable disconnect wait".
	connectionDrainTimeout = 30 * time.Second

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
	quit         chan struct{}
	drained      chan struct{} // Closed by writePump when per-connection outbound queue is fully drained after close
	localFlushed chan struct{} // Closed by writeToLocal after flushing writeCh on quit
	writeCh      chan []byte   // Buffered channel for non-blocking writes to local conn
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

	// Credit-based flow control (reverse direction: client → Nexus).
	// Nexus grants initial credits in EventConnect. The client decrements
	// before each WebSocket write and self-limits at zero.
	availableCredits atomic.Int64

	// Credit-based flow control (forward direction: Nexus → client).
	// Tracks consumed messages since last replenishment sent to Nexus.
	forwardConsumed atomic.Int32

	// Watchdog guard: prevents unbounded 10s probe cycles when credits
	// are exhausted. At most one watchdog timer in flight per stall
	// cycle (CAS on watchdogPending bounds concurrent arms).
	//
	// watchdogGen is a per-connection monotonic generation counter
	// that EVERY armReplenishWatchdog call increments. The scheduled
	// AfterFunc captures the generation at schedule time and checks
	// it at fire time — if the connection has advanced past that
	// generation (via a subsequent arm OR via an EventResumeStream
	// invalidation), the timer body is a no-op. This makes the
	// watchdog self-gating without depending on stop-vs-fire timing
	// or atomic publication of a *time.Timer pointer (the previous
	// pointer-based design had a publication race: AfterFunc was
	// scheduled before the pointer was stored, so an EventResumeStream
	// running between AfterFunc and Store would silently fail to stop
	// the live timer, and a subsequent arm cycle's blind Store would
	// orphan the stale timer indefinitely).
	watchdogPending atomic.Bool
	watchdogGen     atomic.Int64

	// closingKickstartDone is set the first time drainConnectionQueue
	// flushes a one-shot batch of frames past the credit window during
	// teardown. The proxy's tryReplenish only fires at consumed >= 8,
	// so a closing connection sitting at credits=0 with the proxy's
	// writeCh and consumed counter both empty will deadlock waiting
	// for a replenish that never comes — stalling teardown until
	// connectionDrainTimeout. The kickstart sends up to one
	// CreditReplenishBatch worth of frames past credits, just enough
	// to push the proxy's consumed counter over the 8-frame threshold
	// and trigger a real replenish, which restarts the credit-gated
	// drain. Bounded at exactly one batch per connection-close.
	closingKickstartDone atomic.Bool
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

// isClosing reports whether the quit channel has been closed. Used by
// drainConnectionQueue to route to the teardown flush path so pending
// data can be best-effort delivered before the connection goes away
// and closeOnDrained can fire to unblock transitionToClosed. Check is
// non-blocking and safe from any goroutine.
func (cc *clientConn) isClosing() bool {
	select {
	case <-cc.quit:
		return true
	default:
		return false
	}
}

// armReplenishWatchdog schedules a one-shot self-grant probe 10s into
// the future if none is already pending. Called from every site that
// queues data or fails a drain because availableCredits is exhausted.
// Provides a single recovery attempt if an EventResumeStream was lost
// to a transient control-plane drop.
//
// At most one timer is in flight per stall cycle: the CAS on
// watchdogPending bounds concurrent arms. Every arm advances
// watchdogGen, and every EventResumeStream advances it too — the
// scheduled AfterFunc captures the generation at schedule time and
// checks it at fire time, so any subsequent arm OR resume
// retroactively invalidates the timer. This eliminates the
// pointer-publication race that an earlier stop-on-resume design had:
// time.AfterFunc is scheduled before any pointer is stored, so a
// preempted Store racing against an EventResumeStream Stop could
// orphan a live timer indefinitely. Generations are atomic by
// construction — there is no pointer to publish.
func (c *Client) armReplenishWatchdog(cc *clientConn) {
	if !cc.watchdogPending.CompareAndSwap(false, true) {
		return
	}
	gen := cc.watchdogGen.Add(1)
	time.AfterFunc(10*time.Second, func() {
		if cc.watchdogGen.Load() != gen {
			return // Stale arm — invalidated by a later arm or by EventResumeStream.
		}
		if cc.availableCredits.Load() <= 0 {
			log.Printf("WARN: [%s] No credit replenishment for %s in 10s, probing", c.config.Name, cc.id)
			cc.availableCredits.Add(1)
			c.reSignalDataReady(cc)
		}
	})
}

// clampReverseCredits enforces the maxReverseCreditCapacity ceiling on
// a Credits value arriving from the proxy. Logs a WARN if the clamp
// fires, which under a well-behaved proxy should never happen — if it
// does, either the proxy is buggy, a message is corrupted, or an
// attacker is attempting to disable the client's self-limit.
func clampReverseCredits(backendName string, clientID uuid.UUID, credits int64) int64 {
	if credits <= maxReverseCreditCapacity {
		return credits
	}
	log.Printf("WARN: [%s] proxy credit grant %d for client %s exceeds cap %d; clamping",
		backendName, credits, clientID, maxReverseCreditCapacity)
	return maxReverseCreditCapacity
}

// safeClose closes the connection and quit channel exactly once, preventing double-close panics.
// Safe to call even if conn is nil (pending dial).
func (cc *clientConn) safeClose() {
	cc.closeOnce.Do(func() {
		// Cancel pong timer first to prevent timer callback from racing with close
		cc.cancelPongTimer()
		close(cc.quit)
		// Flush + close in a goroutine to avoid blocking the caller, which
		// may be the WebSocket readPump processing a batch of disconnects.
		go func() {
			// Best-effort flush window: give writeToLocal up to 50ms to drain
			// remaining writeCh data before we close the connection.
			if cc.localFlushed != nil {
				select {
				case <-cc.localFlushed:
				case <-time.After(50 * time.Millisecond):
				}
			}
			// Use mutex to safely read conn (may be concurrently assigned by establishLocalConnection)
			cc.connMu.Lock()
			conn := cc.conn
			cc.connMu.Unlock()
			if conn != nil {
				conn.Close()
			}
		}()
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
			id:           msg.ClientID,
			conn:         nil, // Set after dial completes
			hostname:     normalizedHost,
			session:      c.activeSession.Load(),
			quit:         make(chan struct{}),
			drained:      make(chan struct{}),
			localFlushed: make(chan struct{}),
			writeCh:      make(chan []byte, localConnWriteBuffer),
			flow: flowControl{
				lowWaterMark:  c.config.FlowControl.LowWaterMark,
				highWaterMark: c.config.FlowControl.HighWaterMark,
				maxBuffer:     c.config.FlowControl.MaxBuffer,
			},
			isUDP: transport == TransportUDP,
		}
		pendingClient.state.Store(uint32(ConnStatePending))
		pendingClient.lastActivity.Store(time.Now().Unix())

		// Initialize reverse credits (client → Nexus direction). Clamp
		// against an adversarial or corrupted proxy sending an oversized
		// Credits value — symmetric defense with the proxy's own
		// handleResumeStream clamp at maxForwardCreditCapacity.
		if msg.Credits > 0 {
			pendingClient.availableCredits.Store(clampReverseCredits(c.config.Name, msg.ClientID, msg.Credits))
		} else {
			pendingClient.availableCredits.Store(math.MaxInt64) // old server, unlimited
		}

		c.localConns.Store(msg.ClientID, pendingClient)

		// Create the per-connection queue upfront to avoid memory leaks from late-arriving data
		c.getOrCreateQueue(msg.ClientID)

		// Send forward credits (Nexus → client direction) so Nexus can
		// self-limit before our writeCh overflows.
		c.enqueueControl(creditMessage(msg.ClientID, int64(c.config.FlowControl.MaxBuffer)))

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
				// Store initial reverse credits for outbound connections.
				// Same clamp as EventConnect — symmetric defense against
				// adversarial proxy Credits field.
				if conn, ok := c.localConns.Load(msg.ClientID); ok {
					if msg.Credits > 0 {
						conn.(*clientConn).availableCredits.Store(clampReverseCredits(c.config.Name, msg.ClientID, msg.Credits))
					} else {
						conn.(*clientConn).availableCredits.Store(math.MaxInt64) // old server
					}
				}
				// Send forward credits so the hub can self-limit before
				// our writeCh overflows (mirrors EventConnect at line 1181).
				c.enqueueControl(creditMessage(msg.ClientID, int64(c.config.FlowControl.MaxBuffer)))
			} else {
				reason := msg.Reason
				if reason == "" {
					reason = "outbound connection failed"
				}
				ch <- fmt.Errorf("%s", reason)
			}
		}

	case protocol.EventResumeStream:
		// Credit replenishment from Nexus (reverse direction). Clamp
		// the per-message grant (see maxReverseCreditCapacity). This
		// prevents a single adversarial MaxInt64 from wrapping the
		// atomic negative, and bounds per-grant defense-in-depth.
		if val, ok := c.localConns.Load(msg.ClientID); ok {
			conn := val.(*clientConn)
			if msg.Credits > 0 {
				conn.availableCredits.Add(clampReverseCredits(c.config.Name, msg.ClientID, msg.Credits))
				// Real replenishment: the previous stall (if any) is
				// resolved. Clear watchdogPending so a future stall on
				// this connection can re-arm, AND advance the watchdog
				// generation so any in-flight AfterFunc from a previous
				// arm cycle becomes a no-op when it fires (its captured
				// generation will no longer match watchdogGen).
				//
				// The earlier pointer-based design (Stop-the-timer via
				// watchdogTimer.Swap) had a publication race: AfterFunc
				// was scheduled before the timer pointer was stored, so
				// EventResumeStream could see a nil pointer and skip
				// Stop(), then armReplenishWatchdog's delayed Store
				// would install a stale handle that the next arm cycle
				// blindly overwrote — orphaning the live stale timer,
				// which would later fire and grant a phantom credit.
				// The generation counter eliminates the publication
				// race because there is no pointer to publish.
				conn.watchdogPending.Store(false)
				conn.watchdogGen.Add(1)
				c.reSignalDataReady(conn)
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
	if pendingClient.isClosing() {
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
	}

	// Complete the pending connection with mutex to prevent data race with safeClose
	pendingClient.connMu.Lock()
	pendingClient.conn = conn
	pendingClient.connMu.Unlock()

	// Re-check if quit was closed during the conn assignment race window.
	// If safeClose() ran while conn was nil, closeOnce consumed with nil conn,
	// so we must close the newly assigned conn here to prevent FD leak.
	if pendingClient.isClosing() {
		conn.Close()
		c.localConns.Delete(req.ClientID)
		log.Printf("DEBUG: [%s] Connection closed during conn assignment for client %s", c.config.Name, req.ClientID)
		return
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

// forwardCreditRetryInterval is the period at which writeToLocal retries a
// previously-failed forward credit flush. Sized for IoT/interactive latency
// sensitivity (matches the hub-side replenishTickerInterval). The timer is
// only armed AFTER a failed batch flush — sub-batch credits are not
// opportunistically flushed because doing so would create a control-plane
// storm at fan-out (every connection draining 1-7 frames in a 50ms window
// emitting its own credit frame, saturating the 64-slot controlSend queue
// and crowding out pause_stream / resume_stream / disconnect messages).
// Stranded sub-batch credits self-correct via the next burst on the same
// client.
const forwardCreditRetryInterval = 50 * time.Millisecond

// drainBeforeDisconnectLogFmt is shared by the teardown-path and
// normal-cleanup-path log lines so the messages don't drift if either
// call site is edited.
const drainBeforeDisconnectLogFmt = "INFO: [%s] Drained %d messages for ClientID %s before disconnect"

// maxReverseCreditCapacity is the defensive upper bound on a single
// EventConnect / EventOutboundResult / EventResumeStream Credits value
// from the proxy. Mirrors the proxy-side maxForwardCreditCapacity clamp
// at internal/hub/backend.go:handleResumeStream. Protects against a
// malicious or buggy proxy sending MaxInt64 (which would either wrap
// availableCredits negative or effectively disable the client's
// self-limit, defeating the reverse-credit contract entirely).
// 1<<20 == ~1M credits, far above any legitimate grant (the protocol
// uses 64 initial + CreditReplenishBatch=8 per replenish).
const maxReverseCreditCapacity int64 = 1 << 20


// writeToLocal reads from the connection's write channel and writes to the local service.
// This runs in a dedicated goroutine per connection to avoid blocking readPump.
// Note: Cleanup (safeClose, localConns.Delete, disconnect message) is handled by copyLocalToNexus.
func (c *Client) writeToLocal(client *clientConn) {
	defer c.transitionToClosed(client, DisconnectNormal)

	defer close(client.localFlushed)

	// Lazy credit-flush retry timer: nil channel disables the select case
	// while no failed-batch retry is pending. The timer is ONLY armed after
	// a failed batch flush — sub-batch credits are not opportunistically
	// flushed (mirrors the hub-side bufferedConn.drain pattern, and avoids
	// the control-plane storm at fan-out).
	var creditFlushTimer *time.Timer
	var creditFlushTimerCh <-chan time.Time
	defer func() {
		if creditFlushTimer != nil {
			creditFlushTimer.Stop()
		}
	}()
	armCreditFlushTimer := func() {
		if creditFlushTimer == nil {
			creditFlushTimer = time.NewTimer(forwardCreditRetryInterval)
			creditFlushTimerCh = creditFlushTimer.C
			return
		}
		if !creditFlushTimer.Stop() {
			select {
			case <-creditFlushTimer.C:
			default:
			}
		}
		creditFlushTimer.Reset(forwardCreditRetryInterval)
		creditFlushTimerCh = creditFlushTimer.C
	}
	disarmCreditFlushTimer := func() {
		if creditFlushTimer != nil {
			if !creditFlushTimer.Stop() {
				select {
				case <-creditFlushTimer.C:
				default:
				}
			}
		}
		creditFlushTimerCh = nil
	}

	for {
		select {
		case <-client.quit:
			c.flushWriteCh(client)
			return
		case <-creditFlushTimerCh:
			// Opportunistic flush of any pending forward credits. Without
			// this, a stream that crosses the batch threshold and pauses
			// leaves credits stranded — drifting the hub-side semaphore
			// and adding latency on the next burst.
			c.flushForwardCredits(client)
			// Re-arm if credits are still pending after the flush attempt
			// (e.g., enqueue failed and credits were restored).
			if client.forwardConsumed.Load() > 0 {
				armCreditFlushTimer()
			} else {
				disarmCreditFlushTimer()
			}
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

			// TCP Flow Control: Update level and check for resume (watermark safety net)
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

			// Forward credit replenishment (both TCP and UDP): tell Nexus
			// it can send more data for this connection. Only flush at
			// batch boundaries — sub-batch credits stay until the next
			// batch boundary or until the connection closes.
			if int64(client.forwardConsumed.Add(1)) >= protocol.CreditReplenishBatch {
				c.flushForwardCredits(client)
				if client.forwardConsumed.Load() > 0 {
					// Flush failed (credits restored). Arm retry timer.
					armCreditFlushTimer()
				} else {
					disarmCreditFlushTimer()
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

// flushForwardCredits sends any pending forward credits to Nexus. The send
// is non-blocking: a full controlSend queue or inactive session causes the
// pending count to be restored so the next batch (or the next ticker fire)
// retries delivery — preventing silent credit loss without ever blocking
// writeToLocal. The non-blocking semantics matter because writeToLocal is
// called from a per-client goroutine that must not stall on control plane
// backpressure.
//
// Liveness check uses the connection's OWNING session (client.session), not
// the global active session, so a late retry from a torn-down connection
// cannot leak a stale credit frame onto a successor session after reconnect.
// This matches the pattern used by the disconnect paths in client.go.
func (c *Client) flushForwardCredits(client *clientConn) {
	pending := client.forwardConsumed.Swap(0)
	if pending <= 0 {
		return
	}
	if !client.session.IsConnected() {
		client.forwardConsumed.Add(pending)
		return
	}
	msg := creditMessage(client.id, int64(pending))
	select {
	case c.controlSend <- msg:
		// Successfully enqueued.
	default:
		// controlSend full — restore credits so the next batch or ticker
		// tick retries delivery.
		client.forwardConsumed.Add(pending)
	}
}

// flushWriteCh best-effort drains any remaining data in writeCh to the local
// connection. Called by writeToLocal on quit before exiting. Stops on the
// first write error or empty channel. Mirrors bufferedConn.flushRemaining().
func (c *Client) flushWriteCh(client *clientConn) {
	for {
		select {
		case data := <-client.writeCh:
			writeTimeout := localWriteTimeout
			if client.isUDP {
				writeTimeout = 1 * time.Millisecond
			}
			if tc, ok := client.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
				tc.SetWriteDeadline(time.Now().Add(writeTimeout))
			}
			if _, err := client.conn.Write(data); err != nil {
				return
			}
		default:
			return
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

	timer := time.NewTimer(enqueueTimeout)
	defer timer.Stop()
	for {
		select {
		case queue <- message:
			// Notify writePump using signaled flag to prevent duplicate notifications.
			// The signaled flag ensures only one notification is in dataReady at a time
			// per connection, preventing busy connections from flooding the channel.
			if conn, ok := c.localConns.Load(clientID); ok {
				if cConn, ok := conn.(*clientConn); ok {
					// Suppress writePump signaling when out of credits.
					// Data stays in the queue until credits are replenished.
					// Arm the replenishment watchdog so a silently-dropped
					// EventResumeStream can't strand this just-queued data
					// indefinitely. The watchdog's CAS bounds concurrent
					// timers; if a real replenishment arrives later the
					// EventResumeStream handler clears the flag, allowing
					// future stalls on the same connection to re-arm.
					if cConn.availableCredits.Load() <= 0 {
						c.armReplenishWatchdog(cConn)
						return nil
					}
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
		case <-timer.C:
			// If out of credits, the queue is intentionally not draining.
			// Loop back and wait rather than killing the connection.
			if conn, ok := c.localConns.Load(clientID); ok {
				if cConn, ok := conn.(*clientConn); ok && !cConn.isClosing() && cConn.availableCredits.Load() <= 0 {
					timer.Reset(enqueueTimeout)
					continue
				}
			}
			c.stats.enqueueTimeouts.Add(1)
			return fmt.Errorf("enqueue timeout")
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}
}

// reSignalDataReady notifies writePump that a previously-paused client has
// pending data to drain. Called when EventResumeStream arrives or the
// auto-resume timer fires.
func (c *Client) reSignalDataReady(conn *clientConn) {
	if conn.signaled.CompareAndSwap(false, true) {
		select {
		case c.dataReady <- conn.id:
		default:
			go func(id uuid.UUID, quit <-chan struct{}) {
				select {
				case c.dataReady <- id:
				case <-quit:
				case <-c.ctx.Done():
				}
			}(conn.id, conn.quit)
		}
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

// drainConnectionQueue drains the per-connection outbound queue, honoring
// the reverse credit window. Called by writePump when dataReady fires and
// by transitionToClosed's drain kick on teardown.
//
// Credit accounting invariants this function MUST preserve (an earlier
// entry-only credit check allowed the main loop to push maxMessages−1
// frames past zero and the race branch silently skipped the decrement,
// jointly blowing the proxy's per-client writeCh — the v0.3.10 "client
// write buffer full (128 pending)" incident):
//
//   - Every ws.WriteMessage for a data frame is paired with exactly one
//     cConn.availableCredits.Add(-1) on the same send path.
//   - Every site that returns early because credits are exhausted arms
//     the replenishment watchdog (non-closing only) so a silently-dropped
//     EventResumeStream cannot strand the connection.
//   - Every closing-mode exit either (a) calls closeOnDrained when the
//     queue has been fully drained, or (b) clears signaled so the next
//     EventResumeStream will re-signal and resume draining. Otherwise
//     transitionToClosed stalls on connectionDrainTimeout.
//
// Teardown is credit-gated, NOT a special "flush past credits" path.
// The proxy's drain processes in-flight frames during the close
// window (connectionDrainTimeout) and sends EventResumeStream back as
// it makes progress; the
// client's EventResumeStream handler calls reSignalDataReady, which
// re-invokes drainConnectionQueue, which drains more. This is the
// ONLY safe shape — a bounded-flush-past-credits teardown truncates
// the tail of large responses (Codex P1), and an unbounded-flush-past-
// credits teardown reproduces the original v0.3.10 writeCh overflow.
func (c *Client) drainConnectionQueue(ws *websocket.Conn, clientID uuid.UUID, maxMessages int) {
	queue := c.getQueue(clientID)
	if queue == nil {
		// No per-connection queue. If the connection still exists and
		// is in teardown, signal drained so transitionToClosed's wait
		// doesn't stall the full connectionDrainTimeout — there's no
		// data to flush. Without this, queue-less closes (e.g., a
		// connection torn down before any data was queued) hang for
		// 30 s on each disconnect (Codex round-5 P1).
		if rawConn, ok := c.localConns.Load(clientID); ok {
			if cConn, ok := rawConn.(*clientConn); ok && cConn.isClosing() {
				cConn.closeOnDrained()
			}
		}
		return
	}

	rawConn, ok := c.localConns.Load(clientID)
	if !ok {
		return
	}
	cConn := rawConn.(*clientConn)

	// Entry credit guard. No credits → clear signaled, return. In
	// closing mode, also signal drained if the queue is already empty
	// so transitionToClosed doesn't stall waiting for replenishment we
	// don't need.
	//
	// Closing-mode kickstart (one-shot per connection): when credits
	// hit zero AND the queue still has data AND we haven't already
	// kickstarted, grant ourselves CreditReplenishBatch credits
	// (matching the proxy's tryReplenish threshold). This unsticks the
	// "client at credits=0, proxy at consumed<8 with empty writeCh"
	// deadlock that would otherwise stall teardown until
	// connectionDrainTimeout (Codex P1 from round 3): the proxy never
	// emits a sub-batch replenishment, so we have to push it over the
	// threshold ourselves. Bounded at one batch per connection-close,
	// so total over-send is ≤ CreditReplenishBatch frames. The proxy
	// drains those, crosses 8, fires a real replenishment, and
	// credit-gated drain resumes naturally for the rest of the queue.
	//
	// Watchdog arm is non-closing only — a closing connection relies
	// on the kickstart + natural replenishments, not a self-probe.
	if cConn.availableCredits.Load() <= 0 {
		cConn.signaled.Store(false)
		if cConn.isClosing() {
			// Race-check: enqueueData may push a final FIN frame
			// between our len() observation and closeOnDrained.
			// The atomic select-pull pattern catches the race; if a
			// last frame raced in, fall through so the kickstart can
			// pick it up. The frame goes back into the queue via the
			// fall-through path's main loop only because we can't
			// un-pull from a channel — so we have to handle this
			// pulled message inline.
			if len(queue) == 0 {
				select {
				case msg := <-queue:
					// Last-moment FIN: send past credits as a
					// one-frame race exception. Bounded at 1 frame
					// per drain call (and only on this exact race),
					// far under the writeCh hard cap. Same idea as
					// the kickstart but for a 1-frame race window.
					ws.SetWriteDeadline(time.Now().Add(writeWait))
					_ = c.writeMessage(ws, msg)
					cConn.availableCredits.Add(-1)
				default:
					cConn.closeOnDrained()
					return
				}
				cConn.closeOnDrained()
				return
			}
			if cConn.closingKickstartDone.CompareAndSwap(false, true) {
				cConn.availableCredits.Add(protocol.CreditReplenishBatch)
				// Fall through to the main loop to drain the kickstart batch.
			} else {
				return
			}
		} else {
			// Arm the watchdog ONLY if there's actually queued data
			// waiting on credits. An idle drain on an empty queue
			// (e.g., the round-robin re-signal at drained==maxMessages
			// firing on a connection whose queue just emptied) would
			// otherwise burn the connection's one-shot recovery probe
			// and leak a phantom credit 10s later — Codex P2.
			if len(queue) > 0 {
				c.armReplenishWatchdog(cConn)
			}
			return
		}
	}

	drained := 0
	for i := 0; i < maxMessages; i++ {
		if cConn.availableCredits.Load() <= 0 {
			break
		}
		select {
		case msg := <-queue:
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.writeMessage(ws, msg); err != nil {
				return // Write error will be caught by main loop ping or next write
			}
			cConn.availableCredits.Add(-1)
			drained++
		default:
			goto mainLoopDone
		}
	}
mainLoopDone:

	if drained > 0 && cConn.state.Load() == uint32(ConnStateClosed) {
		log.Printf(drainBeforeDisconnectLogFmt, c.config.Name, drained, clientID)
	}

	// Re-check isClosing here — quit may have been closed mid-drain by
	// transitionToClosed while the main loop was running. Without this
	// re-check, a drain that enters the non-closing path and then
	// completes after quit closes would fall through to the non-closing
	// cleanup branch and never call closeOnDrained, stalling
	// transitionToClosed for the full connectionDrainTimeout (Codex P2).
	if cConn.isClosing() {
		cConn.signaled.Store(false)
		// Race-check: enqueueData may push a final frame between our
		// len() observation and closeOnDrained — same race as the
		// entry-guard closing branch. Atomic select-pull catches it.
		if len(queue) == 0 {
			select {
			case msg := <-queue:
				ws.SetWriteDeadline(time.Now().Add(writeWait))
				_ = c.writeMessage(ws, msg)
				cConn.availableCredits.Add(-1)
			default:
				cConn.closeOnDrained()
				return
			}
			cConn.closeOnDrained()
			return
		}
		// Queue still has data. Clear signaled so the next
		// EventResumeStream arrival re-invokes drain; or, if we still
		// have credits after the main loop, re-signal immediately to
		// continue draining in this close window.
		if cConn.availableCredits.Load() > 0 {
			select {
			case c.dataReady <- clientID:
				cConn.signaled.Store(true)
			default:
				go func(id uuid.UUID) {
					select {
					case c.dataReady <- id:
					case <-c.ctx.Done():
					}
				}(clientID)
				cConn.signaled.Store(true)
			}
		}
		return
	}

	// Normal cleanup (non-closing). `drained < maxMessages` implies we
	// exited the main loop early — either the queue emptied or credits
	// were exhausted mid-drain.
	if drained < maxMessages {
		cConn.signaled.Store(false)

		// Credits were exhausted mid-loop. Arm the watchdog ONLY if
		// the queue still has data to drain. Burning the one-shot
		// watchdog on a credit-exhaustion that happens to coincide
		// with an empty queue (a normal burst that finishes on a
		// credit boundary) wastes the connection's only recovery
		// probe — a later real stall would have no recovery path.
		if cConn.availableCredits.Load() <= 0 {
			if len(queue) > 0 {
				c.armReplenishWatchdog(cConn)
			}
			return
		}

		// Race check: enqueueData may have pushed a new message into the
		// queue between the main loop's default-branch exit and our
		// signaled.Store(false) above. Drain exactly one message if so.
		// This branch MUST decrement credits and MUST honor the credit
		// guard above — omitting the decrement was the other root cause
		// of the v0.3.10 prod writeCh overflow.
		select {
		case msg := <-queue:
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.writeMessage(ws, msg); err != nil {
				return
			}
			cConn.availableCredits.Add(-1)
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
		}
		return
	}

	// Filled maxMessages — queue may have more, re-signal for fair
	// round-robin. The re-signal is unconditional (even if the queue
	// happens to be empty right now) because returning here without
	// clearing signaled or re-signaling creates a missed-wakeup race:
	// enqueueData would see signaled==true and skip its own push, so
	// the connection wedges until an unrelated event clears signaled.
	// The next drainConnectionQueue call handles the empty-queue case
	// gracefully via the entry credit guard, which only arms the
	// watchdog when credits are exhausted AND data is actually queued
	// — see the entry-guard `len(queue) > 0` condition.
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

// creditMessage creates a credit replenishment message (EventResumeStream
// with Credits). Used for both initial forward credit grant and periodic
// replenishment from writeToLocal.
func creditMessage(clientID uuid.UUID, credits int64) outboundMessage {
	msg := protocol.ControlMessage{
		Event:    protocol.EventResumeStream,
		ClientID: clientID,
		Credits:  credits,
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
		id:           clientID,
		conn:         conn,
		session:      sess,
		quit:         make(chan struct{}),
		drained:      make(chan struct{}),
		localFlushed: make(chan struct{}),
		writeCh:      make(chan []byte, localConnWriteBuffer),
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

	// If the session died between capture and enqueue, the connect message
	// may have been drained or sent on a dying websocket. Either way the
	// relay-side connection is orphaned — clean up locally.
	if !sess.IsConnected() {
		log.Printf("DEBUG: [%s] Session ended during outbound connect for %s, cleaning up", c.config.Name, targetAddr)
		fail(DisconnectSessionEnded, socks5RepGeneralFailure)
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
