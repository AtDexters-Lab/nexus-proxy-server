package hub

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/bandwidth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy/internal/netutil"
	"github.com/AtDexters-Lab/nexus-proxy/internal/proxy"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 32*1024 + protocol.MessageHeaderLength // This must be sent within writeWait

	defaultReauthGrace  = 10 * time.Second
	healthCheckTimeout  = 5 * time.Second
	tcpCloseGracePeriod = 2 * time.Second
)

type outboundMessage struct {
	messageType int
	data        []byte
}

// Backend represents a single, authenticated WebSocket connection from a backend service.
type Backend struct {
	id        string
	conn      *websocket.Conn
	config    *config.Config
	validator auth.Validator

	hostnames     []string
	tcpPorts      []int
	udpRoutes     []UDPRoutePolicy
	weight        int
	policyVersion string

	clients sync.Map

	// Two-lane outbound channel: control messages (priority) and data messages.
	// Splitting prevents data backlog from starving credit replenishment, pongs,
	// reauth challenges, and other control plane traffic. writePump prefers
	// outgoingControl on every iteration.
	outgoingControl chan outboundMessage
	outgoingData    chan outboundMessage
	reauthTokens    chan string

	quit      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool

	reauthInterval      time.Duration
	reauthGrace         time.Duration
	maintenanceCap      time.Duration
	maintenanceUsed     time.Duration
	authorizerStatusURI string

	reauthMu      sync.Mutex
	reauthPending bool
	pendingNonce  string

	httpClient *http.Client

	// Outbound proxy
	outboundAllowed      bool
	allowedOutboundPorts []int
	outboundIDs          sync.Map     // uuid.UUID → struct{}: tracks outbound clientIDs
	outboundConnCount    atomic.Int64 // established outbound connections
	inFlightDials        atomic.Int64 // pending dial attempts
	maxOutboundConns     int          // -1 = no limit

	// Bandwidth management. Set by AttachBandwidthScheduler during
	// hub.register(), strictly before StartPumps() and before any
	// AddClient call. Read without synchronization; the field is
	// effectively immutable after attach because the attach method
	// panics on double-attach and on attach-after-clients.
	// bandwidthGateCached is a single shared adapter instance, reused
	// by every per-client bufferedConn — saves one allocation per
	// AddClient call vs constructing a fresh adapter each time.
	bandwidthScheduler  *bandwidth.Scheduler
	bandwidthGateCached *backendBandwidthGate

	// Forward credit-based flow control (end-user → backend direction).
	// Each entry is a chan struct{} used as a counting semaphore.
	// Nil entry = old client, unlimited.
	forwardCredits sync.Map // uuid.UUID → chan struct{}

	// Observability counters for credit replenishment and bandwidth gate
	// health. Operators monitor these to detect control plane degradation
	// (replenish) or bandwidth cap exhaustion (gateBlock) before either
	// causes user-visible stalls.
	metrics backendMetrics
}

// backendMetrics holds atomic counters for credit replenishment and
// bandwidth gate health.
//
// replenishRetryAttempts counts *attempts* (not unique drops): a single
// stuck batch can contribute many increments under sustained pressure.
// The ratio against replenishSuccess indicates control lane health.
//
// gateBlockEvents counts frames whose delivery was blocked by the
// bandwidth gate at least once (Request returned false ≥ 1 time).
// gateBlockMillisTotal accumulates the wall-clock wait across those
// blocked frames. The derived metric millisTotal/events approximates
// average block time per blocked frame — sustained > 500ms suggests
// cap exhaustion at current load.
type backendMetrics struct {
	replenishRetryAttempts atomic.Int64
	replenishSuccess       atomic.Int64
	gateBlockEvents        atomic.Int64
	gateBlockMillisTotal   atomic.Int64
	// lastWarnedGateMillis is the value of gateBlockMillisTotal at the
	// time of the most recent WARN log. A new WARN fires each additional
	// gateBlockWarnDeltaMillis of cumulative block time. Updated via CAS
	// from observeGateBlock so concurrent drain callers don't double-log.
	lastWarnedGateMillis atomic.Int64
}

// gateBlockWarnDeltaMillis is the cumulative-block-time delta (per
// backend) between WARN logs. Matches v0.3.9's retryStreakWarnThreshold
// streak-delta pattern so sustained gate pressure produces predictable
// per-backend log cadence instead of per-event log amplification.
const gateBlockWarnDeltaMillis int64 = 10_000

// NewBackend creates a new Backend instance.
func NewBackend(conn *websocket.Conn, meta *AttestationMetadata, cfg *config.Config, validator auth.Validator, httpClient *http.Client) *Backend {
	maintenanceCap := meta.MaintenanceCap
	if !meta.HasMaintenanceCap {
		maintenanceCap = cfg.MaintenanceGraceDefault()
	}

	b := &Backend{
		id:                   uuid.New().String(),
		conn:                 conn,
		config:               cfg,
		validator:            validator,
		hostnames:            meta.cloneHostnames(),
		tcpPorts:             meta.cloneTCPPorts(),
		udpRoutes:            meta.cloneUDPRoutes(),
		weight:               meta.Weight,
		policyVersion:        meta.PolicyVersion,
		outgoingControl:      make(chan outboundMessage, 256),
		outgoingData:         make(chan outboundMessage, 256),
		reauthTokens:         make(chan string, 1),
		quit:                 make(chan struct{}),
		reauthInterval:       meta.ReauthInterval,
		reauthGrace:          meta.ReauthGrace,
		maintenanceCap:       maintenanceCap,
		authorizerStatusURI:  meta.AuthorizerStatusURI,
		httpClient:           httpClient,
		outboundAllowed:      meta.OutboundAllowed,
		allowedOutboundPorts: meta.cloneAllowedOutboundPorts(),
		maxOutboundConns:     cfg.MaxOutboundConns(),
	}

	if b.reauthInterval > 0 && b.reauthGrace <= 0 {
		b.reauthGrace = defaultReauthGrace
	}

	return b
}

func (b *Backend) ID() string {
	return b.id
}

// ReplenishStats returns the lifetime credit replenishment counters for
// observability. success counts EventResumeStream messages successfully
// enqueued on the control lane; retryAttempts counts failed enqueue
// attempts (the same stuck batch can contribute multiple retries).
func (b *Backend) ReplenishStats() (success, retryAttempts int64) {
	return b.metrics.replenishSuccess.Load(), b.metrics.replenishRetryAttempts.Load()
}

// BandwidthGateStats returns the lifetime bandwidth gate counters for
// observability. events counts frames whose delivery was blocked by the
// per-backend bandwidth gate at least once (Request returned false
// ≥ 1 time). millisTotal accumulates the wall-clock wait across those
// blocked frames.
//
// Operator guidance:
//   - Derived metric millisTotal/events approximates average block time
//     per blocked frame. Alert threshold: sustained > 500ms average
//     indicates cap exhaustion at current load.
//   - events rate should be near-zero when totalBandwidthMbps is unset
//     (scheduler nil → gate nil → no drain-side gate activity). A
//     non-zero rate under a nil scheduler indicates a bug.
//   - Use backend-ID correlation from logs to identify noisy-neighbor
//     clients; per-client histograms are future work.
func (b *Backend) BandwidthGateStats() (events, millisTotal int64) {
	return b.metrics.gateBlockEvents.Load(), b.metrics.gateBlockMillisTotal.Load()
}

// ClientWriteBufferDepth returns the current per-client writeCh depth
// and true if the client exists on this backend. Exposed so integration
// tests and observability code can verify that credit-based flow control
// keeps the async write buffer within the credit window
// (DefaultCreditCapacity) under sustained load.
func (b *Backend) ClientWriteBufferDepth(clientID uuid.UUID) (int, bool) {
	raw, ok := b.clients.Load(clientID)
	if !ok {
		return 0, false
	}
	bc, ok := raw.(*bufferedConn)
	if !ok {
		return 0, false
	}
	return bc.Depth(), true
}

// backendBandwidthGate adapts the per-backend bandwidth.Scheduler interface
// to bufferedConn's bandwidthGate contract for the **downlink** path
// (drain → TCP client). The uplink path (SendData → backend WS) charges
// the scheduler directly without going through this adapter; both paths
// must add protocol.MessageHeaderLength bytes of WS framing overhead so
// uplink and downlink accounting stay byte-symmetric against the same
// per-backend deficit.
type backendBandwidthGate struct {
	scheduler *bandwidth.Scheduler
	backendID string
	overhead  int
}

func (g *backendBandwidthGate) Request(bytes int) (bool, time.Duration) {
	return g.scheduler.RequestSend(g.backendID, bytes+g.overhead)
}

func (g *backendBandwidthGate) Record(bytes int) {
	g.scheduler.RecordSent(g.backendID, bytes+g.overhead)
}

// gateForBufferedConn returns a bandwidthGate wired to this backend's
// scheduler, or nil if no scheduler is attached. Returns the cached
// adapter instance so every per-client bufferedConn shares one — there
// is no per-client state inside the adapter.
//
// TODO(fairness): the per-client fair share under bandwidth cap now
// reflects CAS-race on the shared per-backend deficit rather than
// WS-arrival-order serialization. Large-frame clients under sustained
// small-frame contention may see higher tail latency. Neither the old
// nor the new design implements true per-client quantum DRR; fairness
// guarantees are unchanged at the backend level. If operators begin
// relying on bandwidth-cap fairness between co-tenant clients, add
// per-client BackendBandwidth-like state in the scheduler.
func (b *Backend) gateForBufferedConn() bandwidthGate {
	if b.bandwidthGateCached == nil {
		return nil
	}
	return b.bandwidthGateCached
}

// newClientBufferedConn constructs a per-client bufferedConn wired to
// this backend's credit replenishment, bandwidth gate, and metric
// observer callbacks. Used by both AddClient (inbound clients,
// IdleTimeout) and AddOutboundClient (outbound proxy connections,
// OutboundIdleTimeout) — only the timeout differs.
func (b *Backend) newClientBufferedConn(conn net.Conn, writeTimeout time.Duration, clientID uuid.UUID) *bufferedConn {
	return newBufferedConn(
		conn,
		writeTimeout,
		b.replenishCallbackFor(clientID),
		b.gateForBufferedConn(),
		b.observeGateBlock,
	)
}

// observeGateBlock records a bandwidth gate wait event. Called from the
// bufferedConn drain goroutine via the gateBlockObserver callback (once
// per frame that was blocked at least once). Uses atomic ops — safe
// under concurrent calls from multiple client drain goroutines.
//
// WARN log: when cumulative gateBlockMillisTotal crosses each additional
// gateBlockWarnDeltaMillis (10s) threshold, emit one WARN line per CAS
// winner. Uses CAS on lastWarnedGateMillis so concurrent drain callers
// do not emit duplicate warnings at the same crossing. Note: a single
// elapsed value spanning multiple thresholds (e.g., 25s in one event)
// produces only ONE WARN, not three. Realistic per-frame waits are
// sub-second, so this coarsening is harmless in practice.
func (b *Backend) observeGateBlock(elapsed time.Duration) {
	b.metrics.gateBlockEvents.Add(1)
	total := b.metrics.gateBlockMillisTotal.Add(elapsed.Milliseconds())

	for {
		lastWarned := b.metrics.lastWarnedGateMillis.Load()
		if total-lastWarned < gateBlockWarnDeltaMillis {
			return
		}
		if b.metrics.lastWarnedGateMillis.CompareAndSwap(lastWarned, total) {
			log.Printf("WARN: backend %s gate-blocked %ds cumulative (check totalBandwidthMbps cap)",
				b.id, total/1000)
			return
		}
		// CAS lost — another drain caller beat us to it. Re-read and
		// re-check; the winning caller's store may have satisfied
		// the delta already.
	}
}

// AttachBandwidthScheduler wires a bandwidth scheduler to this backend.
// Must be called at most once, strictly before StartPumps or AddClient,
// from the construction goroutine. The Register() side effect and the
// field write happen together so the pair cannot be split by refactors.
//
// Safeguards (panic on misuse):
//   - nil scheduler → no-op.
//   - Called twice → panics. A second Register() would silently reset
//     deficit / lastActive on a live backend.
//   - Called after any client is registered → panics. Mixing gated and
//     ungated clients on the same backend is a debugging hazard.
func (b *Backend) AttachBandwidthScheduler(s *bandwidth.Scheduler) {
	if s == nil {
		return
	}
	if b.bandwidthScheduler != nil {
		panic(fmt.Sprintf("AttachBandwidthScheduler: backend %s already has a scheduler attached", b.id))
	}
	hasClient := false
	b.clients.Range(func(_, _ interface{}) bool {
		hasClient = true
		return false
	})
	if hasClient {
		panic(fmt.Sprintf("AttachBandwidthScheduler: backend %s already has clients; attach before StartPumps", b.id))
	}
	s.Register(b.id)
	b.bandwidthScheduler = s
	b.bandwidthGateCached = &backendBandwidthGate{
		scheduler: s,
		backendID: b.id,
		overhead:  protocol.MessageHeaderLength,
	}
}

func (b *Backend) Close() {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.quit) // stop pumps, reject new sends immediately
		if b.conn != nil {
			_ = b.conn.Close()
		}
		// never closed — forces gracefulCloseConn to use its full timer grace period
		noAbort := make(chan struct{})
		b.clients.Range(func(key, value interface{}) bool {
			b.clients.Delete(key)
			if clientConn, ok := value.(net.Conn); ok {
				gracefulCloseConn(clientConn, noAbort)
			}
			return true
		})
		// Drain outbound and credit tracking state.
		b.forwardCredits.Range(func(key, _ interface{}) bool {
			b.forwardCredits.Delete(key)
			return true
		})
		b.outboundIDs.Range(func(key, _ interface{}) bool {
			b.outboundIDs.Delete(key)
			return true
		})
		b.outboundConnCount.Store(0)
	})
}

func (b *Backend) StartPumps() {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		b.writePump()
	}()

	go func() {
		defer wg.Done()
		b.readPump()
	}()

	go func() {
		defer wg.Done()
		b.reauthLoop()
	}()

	wg.Wait()
}

func (b *Backend) AddClient(clientConn net.Conn, clientID uuid.UUID, hostname string, isTLS bool) error {
	var connPort int
	transport := protocol.TransportTCP
	switch addr := clientConn.LocalAddr().(type) {
	case *net.TCPAddr:
		connPort = addr.Port
		transport = protocol.TransportTCP
	case *net.UDPAddr:
		connPort = addr.Port
		transport = protocol.TransportUDP
	default:
		return fmt.Errorf("WARN: Could not determine destination port for client %s", clientID)
	}
	remoteAddr := clientConn.RemoteAddr()
	clientIP := remoteAddr.String()
	if addr, ok := remoteAddr.(*net.TCPAddr); ok && (addr.IP == nil || addr.IP.IsUnspecified()) {
		log.Printf("WARN: Client %s has empty/unspecified remote address: %s", clientID, clientIP)
	}
	msg := protocol.ControlMessage{
		Event:     protocol.EventConnect,
		ClientID:  clientID,
		ConnPort:  connPort,
		ClientIP:  clientIP,
		Transport: transport,
		Hostname:  hostname,
		IsTLS:     isTLS,
		Credits:   protocol.DefaultCreditCapacity,
	}

	if err := b.SendControlMessage(msg); err != nil {
		return fmt.Errorf("failed to send connect message for client %s: %w", clientID, err)
	}
	b.clients.Store(clientID, b.newClientBufferedConn(clientConn, b.config.IdleTimeout(), clientID))
	return nil
}

func (b *Backend) RemoveClient(clientID uuid.UUID) {
	if val, ok := b.clients.LoadAndDelete(clientID); ok {
		b.releaseOutboundSlot(clientID)
		b.forwardCredits.Delete(clientID)
		// Stop the bufferedConn's drain goroutine (Client.Start has already exited).
		if conn, ok := val.(net.Conn); ok {
			_ = conn.Close()
		}
		msg := protocol.ControlMessage{Event: protocol.EventDisconnect, ClientID: clientID}
		if err := b.SendControlMessage(msg); err != nil {
			log.Printf("ERROR: Failed to send disconnect message for client %s: %v", clientID, err)
		} else {
			log.Printf("INFO: Client %s disconnected from backend %s", clientID, b.id)
		}
	}
}

func (b *Backend) HasRecentActivity(clientID uuid.UUID, since time.Time) bool {
	raw, ok := b.clients.Load(clientID)
	if !ok {
		return false
	}
	if bc, ok := raw.(*bufferedConn); ok {
		return bc.HasRecentWrite(since)
	}
	return false
}

// replenishCallbackFor returns a non-blocking callback that sends credit
// replenishment (EventResumeStream with Credits) to the backend for the
// given client. Called by bufferedConn's drain goroutine after every
// CreditReplenishBatch successful TCP writes.
//
// Returns nil if the credit message was successfully enqueued onto the
// control lane. Returns an error if the enqueue failed — the caller (drain)
// will preserve its consumed counter and retry, ensuring no credit is ever
// permanently lost. This is the contract that fixes the silent credit-loss
// bug from v0.3.7/v0.3.8.
func (b *Backend) replenishCallbackFor(clientID uuid.UUID) func(int64) error {
	return func(credits int64) error {
		msg := protocol.ControlMessage{
			Event:    protocol.EventResumeStream,
			ClientID: clientID,
			Credits:  credits,
		}
		if err := b.SendControlMessage(msg); err != nil {
			b.metrics.replenishRetryAttempts.Add(1)
			return err
		}
		b.metrics.replenishSuccess.Add(1)
		return nil
	}
}

// acquireForwardCredit blocks until a forward credit is available for the
// given client. Returns immediately if no credit tracking exists for this
// client (old client, unlimited). Uses IdleTimeout as an escape valve to
// prevent goroutine leaks when the end-user disconnects while blocked.
func (b *Backend) acquireForwardCredit(clientID uuid.UUID) error {
	raw, ok := b.forwardCredits.Load(clientID)
	if !ok {
		return nil // unlimited (old client or not yet registered)
	}
	timeout := b.config.IdleTimeout()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-raw.(chan struct{}):
		return nil
	case <-timer.C:
		return fmt.Errorf("forward credit timeout for client %s", clientID)
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	}
}

// releaseOutboundSlot decrements the outbound connection count if clientID
// was an outbound connection. Safe to call for inbound connections (no-op).
func (b *Backend) releaseOutboundSlot(clientID uuid.UUID) {
	if _, ok := b.outboundIDs.LoadAndDelete(clientID); ok {
		b.outboundConnCount.Add(-1)
	}
}

// AddOutboundClient stores a proxy-dialed outbound connection in the clients
// map. Unlike AddClient, it does not send EventConnect (the backend initiated
// the request and already knows about the connection).
func (b *Backend) AddOutboundClient(conn net.Conn, clientID uuid.UUID) error {
	if b.closed.Load() {
		return fmt.Errorf("backend %s is closing", b.id)
	}
	wrapped := proxy.NewPausableConn(conn)
	buffered := b.newClientBufferedConn(wrapped, b.config.OutboundIdleTimeout(), clientID)
	if _, loaded := b.clients.LoadOrStore(clientID, buffered); loaded {
		buffered.Close()
		return fmt.Errorf("client ID %s already exists", clientID)
	}
	b.outboundIDs.Store(clientID, struct{}{})
	b.outboundConnCount.Add(1)
	return nil
}

func (b *Backend) SendData(clientID uuid.UUID, data []byte) error {
	if b.closed.Load() {
		return fmt.Errorf("backend %s is already closed", b.id)
	}
	// Short-circuit when the client has already been removed (typically because
	// EventDisconnect arrived and handleControlMessage ran LoadAndDelete).
	// Without this, the user→backend read loop in proxy/client.go:Start keeps
	// pumping bytes during the tcpCloseGracePeriod (CloseWrite-only on the
	// user TCP), each one triggering a "No local connection found" WARN +
	// redundant disconnect on the backend side. Residual TOCTOU race between
	// this check and the WS frame landing on the backend remains, but is
	// bounded to ~1 frame per disconnect rather than every byte the user
	// sends during the 2s grace window.
	if _, ok := b.clients.Load(clientID); !ok {
		return iface.ErrClientGone
	}
	header := make([]byte, protocol.MessageHeaderLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], clientID[:])
	message := append(header, data...)

	messageSize := len(message) // protocol.MessageHeaderLength + len(data)

	// Forward credit check — blocks until the backend client has buffer
	// room. This is a per-client goroutine (Client.Start), so blocking
	// here doesn't affect readPump or other clients.
	if err := b.acquireForwardCredit(clientID); err != nil {
		return err
	}

	// Bandwidth check with retry loop (if scheduler enabled)
	if b.bandwidthScheduler != nil {
		for {
			allowed, waitTime := b.bandwidthScheduler.RequestSend(b.id, messageSize)
			if allowed {
				break
			}
			// Block and wait (backpressure to client via TCP flow control)
			select {
			case <-b.quit:
				return fmt.Errorf("backend %s is closing", b.id)
			case <-time.After(waitTime):
				// Continue to retry
			}
		}
	}

	outbound := outboundMessage{messageType: websocket.BinaryMessage, data: message}
	select {
	case <-b.quit:
		// Refund reserved bandwidth since the message won't be sent
		if b.bandwidthScheduler != nil {
			b.bandwidthScheduler.RefundSend(b.id, messageSize)
		}
		return fmt.Errorf("backend %s is closing", b.id)
	case b.outgoingData <- outbound:
	default:
		// Refund reserved bandwidth since the message is dropped
		if b.bandwidthScheduler != nil {
			b.bandwidthScheduler.RefundSend(b.id, messageSize)
		}
		return fmt.Errorf("backend %s data channel full, dropping data for client %s", b.id, clientID)
	}

	// Record successful send
	if b.bandwidthScheduler != nil {
		b.bandwidthScheduler.RecordSent(b.id, messageSize)
	}

	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	default:
	}
	return nil
}

func (b *Backend) SendControlMessage(msg protocol.ControlMessage) error {
	if b.closed.Load() {
		return fmt.Errorf("backend %s is already closed", b.id)
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ERROR: Failed to marshal control message for backend %s: %v", b.id, err)
		return err
	}

	header := []byte{protocol.ControlByteControl}
	outbound := outboundMessage{messageType: websocket.BinaryMessage, data: append(header, payload...)}

	// Lifecycle events that bracket data on a client connection MUST preserve
	// FIFO ordering with that data:
	//
	//   - EventConnect must precede the first data frame, otherwise the
	//     backend's handleDataMessage sees an unknown clientID and tears
	//     down the new connection (drops the prelude / first UDP packet).
	//   - EventDisconnect must follow all queued data, otherwise the backend
	//     closes the local socket before the tail of the stream is delivered.
	//   - EventOutboundResult with Success=true must precede data on the
	//     outbound connection for the same reason as EventConnect.
	//
	// Routing these through outgoingData makes them share the same FIFO
	// lane as the data they bracket. EventOutboundResult with Success=false
	// has NO subsequent data (no AddOutboundClient, no readOutboundConn),
	// so it stays on outgoingControl — otherwise a busy data lane would
	// delay the failure reply and the SOCKS5 caller would hit its
	// outboundConnectTimeout (~15s) instead of getting the real dial error
	// promptly. Other control messages (credits, pongs, reauth challenges)
	// also stay on outgoingControl where they cannot be starved by data.
	lane := b.outgoingControl
	laneName := "control"
	switch msg.Event {
	case protocol.EventConnect, protocol.EventDisconnect:
		lane = b.outgoingData
		laneName = "data"
	case protocol.EventOutboundResult:
		if msg.Success {
			lane = b.outgoingData
			laneName = "data"
		}
	}

	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	case lane <- outbound:
	default:
		return fmt.Errorf("backend %s %s channel full, dropping %s for client %s", b.id, laneName, msg.Event, msg.ClientID)
	}
	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	default:
	}
	return nil
}

func (b *Backend) readPump() {
	defer func() {
		log.Printf("INFO: Closing backend %s read pump", b.id)
		b.Close()
	}()
	b.conn.SetReadLimit(maxMessageSize)
	b.conn.SetReadDeadline(time.Now().Add(pongWait))
	b.conn.SetPongHandler(func(string) error {
		b.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		messageType, message, err := b.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ERROR: Unexpected close error from backend %s: %v", b.id, err)
			}
			break
		}

		b.conn.SetReadDeadline(time.Now().Add(pongWait))

		switch messageType {
		case websocket.BinaryMessage:
			b.handleBinaryMessage(message)
		case websocket.TextMessage:
			b.handleTextMessage(string(message))
		default:
			log.Printf("WARN: Unsupported websocket message type %d from backend %s", messageType, b.id)
		}
	}
}

// handleBinaryMessage runs on the shared readPump goroutine. It MUST
// NOT block on any per-client state (TCP writes, bandwidth gates,
// credit semaphores, etc.) — a block here stalls ingress for every
// client on this backend, including control messages. All per-client
// synchronous work belongs in bufferedConn.drain.
func (b *Backend) handleBinaryMessage(message []byte) {
	if len(message) < 1 {
		return
	}

	controlByte := message[0]
	payload := message[1:]

	switch controlByte {
	case protocol.ControlByteControl:
		var msg protocol.ControlMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			log.Printf("WARN: Malformed control message from backend %s", b.id)
			return
		}
		b.handleControlMessage(msg)

	case protocol.ControlByteData:
		if len(payload) < protocol.ClientIDLength {
			log.Printf("WARN: Malformed data message from backend %s", b.id)
			return
		}
		var clientID uuid.UUID
		copy(clientID[:], payload[:protocol.ClientIDLength])
		data := payload[protocol.ClientIDLength:]

		rawConn, ok := b.clients.Load(clientID)
		if !ok {
			return
		}

		if clientConn, ok := rawConn.(net.Conn); ok {
			// Non-blocking enqueue: bufferedConn.Write returns
			// immediately. Drain (per-client goroutine) owns the
			// TCP write, the bandwidth gate wait, and the credit
			// replenishment — none of which block this readPump.
			if _, err := clientConn.Write(data); err != nil {
				// Use RemoveClient (not just Close) so all per-client state
				// — clients map entry, forwardCredits semaphore, outboundIDs,
				// EventDisconnect notification — is cleaned up. A bare Close
				// leaks all of these and leaves the backend dispatching to
				// a closed conn until the backend itself disconnects.
				log.Printf("WARN: Failed to write to client %s: %v. Removing.", clientID, err)
				b.RemoveClient(clientID)
			}
		} else {
			log.Printf("ERROR: Client %s is not a net.Conn type in backend %s", clientID, b.id)
		}
	}
}

func (b *Backend) handleTextMessage(message string) {
	b.reauthMu.Lock()
	expecting := b.reauthPending
	b.reauthMu.Unlock()

	if !expecting {
		log.Printf("WARN: Unexpected text frame from backend %s while no re-auth is pending", b.id)
		return
	}

	select {
	case b.reauthTokens <- message:
	default:
		log.Printf("WARN: Dropping re-auth token from backend %s: channel full", b.id)
	}
}

func (b *Backend) handleControlMessage(msg protocol.ControlMessage) {
	switch msg.Event {
	case protocol.EventPingClient:
		if _, ok := b.clients.Load(msg.ClientID); ok {
			pongMsg := protocol.ControlMessage{Event: protocol.EventPongClient, ClientID: msg.ClientID}
			if err := b.SendControlMessage(pongMsg); err != nil {
				log.Printf("ERROR: Failed to send pong for client %s to backend %s: %v", msg.ClientID, b.id, err)
			}
		} else {
			log.Printf("INFO: Client %s is no longer active, not sending pong to backend %s", msg.ClientID, b.id)
		}
	case protocol.EventDisconnect:
		if rawConn, ok := b.clients.LoadAndDelete(msg.ClientID); ok {
			b.releaseOutboundSlot(msg.ClientID)
			if conn, ok := rawConn.(net.Conn); ok {
				log.Printf("INFO: Backend %s reported disconnect for client %s. Closing client connection.", b.id, msg.ClientID)
				gracefulCloseConn(conn, b.quit)
			}
		}
	case protocol.EventOutboundConnect:
		b.handleOutboundConnect(msg)
	case protocol.EventPauseStream:
		b.handlePauseStream(msg.ClientID)
	case protocol.EventResumeStream:
		b.handleResumeStream(msg.ClientID, msg.Credits)
	default:
		log.Printf("WARN: Unknown control event '%s' from backend %s", msg.Event, b.id)
	}
}

// gracefulCloseConn performs a TCP half-close with a grace period. The abort
// channel skips the grace period when closed; pass a never-closed channel for
// the full grace period (used in Backend.Close).
func gracefulCloseConn(conn net.Conn, abort <-chan struct{}) {
	netutil.GracefulCloseConn(conn, abort, tcpCloseGracePeriod)
}

func (b *Backend) handlePauseStream(clientID uuid.UUID) {
	rawConn, ok := b.clients.Load(clientID)
	if !ok {
		log.Printf("WARN: pause_stream for unknown client %s on backend %s (ignored)", clientID, b.id)
		return
	}
	if pausable, ok := rawConn.(interface{ Pause() }); ok {
		pausable.Pause()
		log.Printf("DEBUG: Paused stream for client %s on backend %s", clientID, b.id)
	}
}

func (b *Backend) handleResumeStream(clientID uuid.UUID, credits int64) {
	// PausableConn resume (backward compat for forward direction watermarks)
	rawConn, ok := b.clients.Load(clientID)
	if !ok {
		log.Printf("WARN: resume_stream for unknown client %s on backend %s (ignored)", clientID, b.id)
		return
	}
	if pausable, ok := rawConn.(interface{ Resume() }); ok {
		pausable.Resume()
	}

	// Forward credit replenishment
	if credits > 0 {
		// Defensive cap: a malicious or buggy backend that sends Credits=MaxInt64
		// would otherwise cause `make(chan struct{}, MaxInt64)` to panic and
		// crash the relay. Sane workloads have credits ≤ ~128 (= 2x default
		// capacity), so 16384 is >100x more than legitimate use ever needs.
		if credits > maxForwardCreditCapacity {
			log.Printf("WARN: Backend %s requested credit capacity %d for client %s; capping at %d",
				b.id, credits, clientID, maxForwardCreditCapacity)
			credits = maxForwardCreditCapacity
		}
		// Use the larger of credits and DefaultCreditCapacity as semaphore
		// capacity so both initial grants and replenishments fit.
		capacity := protocol.DefaultCreditCapacity
		if credits > capacity {
			capacity = credits
		}
		sem := make(chan struct{}, capacity)
		raw, loaded := b.forwardCredits.LoadOrStore(clientID, sem)
		if !loaded {
			// First credit grant — fill with non-blocking sends to avoid
			// wedging the readPump if credits > capacity somehow.
			for i := int64(0); i < credits; i++ {
				select {
				case sem <- struct{}{}:
				default:
				}
			}
			log.Printf("DEBUG: Registered %d forward credits for client %s on backend %s", credits, clientID, b.id)
			return
		}
		// Existing semaphore — replenish.
		existing := raw.(chan struct{})
		for i := int64(0); i < credits; i++ {
			select {
			case existing <- struct{}{}:
			default:
				// At capacity — ignore excess credits.
			}
		}
	}
}

// maxForwardCreditCapacity is a defensive upper bound for credit grants from
// backends, preventing make(chan, MaxInt64) crashes if a backend sends an
// adversarial Credits value. Sized large enough (~1M) to never truncate any
// legitimate FlowControl.MaxBuffer configuration, while still bounding the
// per-client semaphore allocation to ~8MB worst-case.
const maxForwardCreditCapacity = 1 << 20

// maxTargetAddrLen limits the length of outbound target addresses.
const maxTargetAddrLen = 260

// handleOutboundConnect processes an outbound connection request from a backend.
// Validation and the dial are handled here; the dial runs in a goroutine to
// avoid blocking the readPump. The readPump is single-goroutined, so this
// method is never called concurrently for the same backend.
func (b *Backend) handleOutboundConnect(msg protocol.ControlMessage) {
	sendFailure := func(reason string) {
		result := protocol.ControlMessage{
			Event:    protocol.EventOutboundResult,
			ClientID: msg.ClientID,
			Success:  false,
			Reason:   reason,
		}
		if err := b.SendControlMessage(result); err != nil {
			log.Printf("ERROR: Failed to send outbound failure result to backend %s: %v", b.id, err)
		}
	}

	if !b.config.AllowOutbound {
		sendFailure("outbound connections disabled on this proxy")
		return
	}
	if !b.outboundAllowed {
		sendFailure("outbound connections not allowed for this backend")
		return
	}

	// Validate target address.
	addr := msg.TargetAddr
	if len(addr) > maxTargetAddrLen {
		sendFailure("target address too long")
		return
	}
	for _, c := range addr {
		if c < 0x20 || c == 0x7f {
			sendFailure("target address contains control characters")
			return
		}
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host == "" || portStr == "" {
		sendFailure("invalid target address")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		sendFailure("invalid target port")
		return
	}

	// Check port allowlists (server-level then backend-level).
	if len(b.config.AllowedOutboundPorts) > 0 && !portAllowed(b.config.AllowedOutboundPorts, port) {
		sendFailure(fmt.Sprintf("outbound port %d not allowed by server", port))
		return
	}
	if len(b.allowedOutboundPorts) > 0 && !portAllowed(b.allowedOutboundPorts, port) {
		sendFailure(fmt.Sprintf("outbound port %d not allowed for this backend", port))
		return
	}

	// Check combined limit (established + in-flight).
	if b.maxOutboundConns >= 0 {
		if b.outboundConnCount.Load()+b.inFlightDials.Load() >= int64(b.maxOutboundConns) {
			sendFailure("outbound connection limit exceeded")
			return
		}
	}

	if msg.ClientID == uuid.Nil {
		sendFailure("missing client ID")
		return
	}

	// Reserve an in-flight slot before spawning the goroutine.
	b.inFlightDials.Add(1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), b.config.OutboundDialTimeout())
		defer cancel()

		// Cancel the dial if the backend disconnects.
		go func() {
			select {
			case <-b.quit:
				cancel()
			case <-ctx.Done():
			}
		}()

		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		// Dial phase complete — release the in-flight slot regardless of outcome.
		// Must happen before readOutboundConn to avoid double-counting established
		// connections (outboundConnCount already tracks them).
		b.inFlightDials.Add(-1)
		if err != nil {
			log.Printf("WARN: Outbound dial to %s for backend %s failed: %v", addr, b.id, err)
			sendFailure("dial failed")
			return
		}

		if err := b.AddOutboundClient(conn, msg.ClientID); err != nil {
			_ = conn.Close()
			sendFailure(fmt.Sprintf("registration failed: %v", err))
			return
		}

		result := protocol.ControlMessage{
			Event:    protocol.EventOutboundResult,
			ClientID: msg.ClientID,
			Success:  true,
			Credits:  protocol.DefaultCreditCapacity,
		}
		if err := b.SendControlMessage(result); err != nil {
			log.Printf("WARN: Failed to send outbound success result for %s, cleaning up: %v", msg.ClientID, err)
			b.RemoveClient(msg.ClientID)
			return
		}

		// Retrieve the PausableConn wrapper stored by AddOutboundClient so that
		// reads go through the pausable layer and pause/resume actually works.
		rawWrapped, ok := b.clients.Load(msg.ClientID)
		if !ok {
			return
		}
		wrappedConn, ok := rawWrapped.(net.Conn)
		if !ok {
			return
		}

		log.Printf("INFO: Outbound connection %s established to %s for backend %s", msg.ClientID, addr, b.id)
		b.readOutboundConn(msg.ClientID, wrappedConn)
	}()
}

// readOutboundConn reads data from a proxy-dialed outbound TCP connection
// and relays it to the backend over the WebSocket.
func (b *Backend) readOutboundConn(clientID uuid.UUID, conn net.Conn) {
	defer b.RemoveClient(clientID)

	bufPtr := proxy.GetBuffer()
	defer proxy.PutBuffer(bufPtr)
	buf := *bufPtr

	idleTimeout := b.config.OutboundIdleTimeout()
	for {
		if idleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		}
		n, err := conn.Read(buf)
		if n > 0 {
			if sendErr := b.SendData(clientID, buf[:n]); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (b *Backend) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		log.Printf("INFO: Closing backend %s write pump", b.id)
		ticker.Stop()
		b.Close()
	}()

	writeOutbound := func(outbound outboundMessage) bool {
		_ = b.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := b.conn.WriteMessage(outbound.messageType, outbound.data); err != nil {
			log.Printf("ERROR: Failed to write to backend %s: %v", b.id, err)
			return false
		}
		return true
	}

	// Fair select across both lanes + ticker + quit. The channel split
	// (outgoingControl + outgoingData) is what fixes the silent credit-loss
	// bug — control messages have their own buffer that data cannot exhaust.
	// We do NOT use strict priority for outgoingControl: a sustained burst of
	// control messages (e.g., credit replenishment under load) would
	// otherwise starve outgoingData and the ping ticker, breaking uploads
	// and triggering pongWait disconnects on otherwise-healthy backends.
	for {
		select {
		case <-b.quit:
			return
		case outbound := <-b.outgoingControl:
			if !writeOutbound(outbound) {
				return
			}
		case outbound := <-b.outgoingData:
			if !writeOutbound(outbound) {
				return
			}
		case <-ticker.C:
			_ = b.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := b.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (b *Backend) reauthLoop() {
	if b.reauthInterval <= 0 {
		<-b.quit
		return
	}
	timer := time.NewTimer(b.jitterDuration(b.reauthInterval))
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	for {
		select {
		case <-b.quit:
			return
		case <-timer.C:
			if err := b.performReauth(); err != nil {
				log.Printf("ERROR: Re-authentication failed for backend %s: %v", b.id, err)
				b.Close()
				return
			}
			next := b.reauthInterval
			if next <= 0 {
				// Claims may disable future reauth.
				return
			}
			timer.Reset(b.jitterDuration(next))
		}
	}
}

func (b *Backend) performReauth() error {
	if b.authorizerStatusURI != "" {
		for {
			healthy, err := b.authorizerHealthy()
			if err == nil && healthy {
				b.maintenanceUsed = 0
				break
			}

			if !b.deferForMaintenance() {
				break
			}
		}
	}

	nonce, err := generateNonce()
	if err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	challenge := protocol.ChallengeMessage{Type: protocol.ChallengeReauth, Nonce: nonce}
	payload, err := json.Marshal(challenge)
	if err != nil {
		return fmt.Errorf("marshal reauth challenge: %w", err)
	}

	b.prepareForNonce(nonce)

	// Reauth challenge bypasses SendControlMessage because it uses a TextMessage
	// frame and the ChallengeMessage schema (not protocol.ControlMessage). It
	// routes through the priority control lane to avoid being starved by data
	// backlog — a delayed reauth challenge can cause backend timeout and cascade
	// 1000+ client disconnects.
	outbound := outboundMessage{messageType: websocket.TextMessage, data: payload}
	select {
	case <-b.quit:
		b.clearPendingNonce()
		return errors.New("backend closing during reauth challenge")
	case b.outgoingControl <- outbound:
	default:
		b.clearPendingNonce()
		return errors.New("reauth challenge channel full")
	}

	// Drain any stale token.
	select {
	case <-b.reauthTokens:
	default:
	}

	timer := time.NewTimer(b.reauthGrace)
	defer timer.Stop()

	var token string
	select {
	case <-b.quit:
		b.clearPendingNonce()
		return errors.New("backend closed during reauth")
	case <-timer.C:
		b.clearPendingNonce()
		return fmt.Errorf("reauthentication timed out after %s", b.reauthGrace)
	case token = <-b.reauthTokens:
	}
	b.clearPendingNonce()

	return b.processReauthToken(token, nonce)
}

func (b *Backend) processReauthToken(token, expectedNonce string) error {
	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()

	claims, err := b.validator.Validate(ctx, token)
	if err != nil {
		return fmt.Errorf("validate token: %w", err)
	}
	if claims.SessionNonce != expectedNonce {
		return fmt.Errorf("session nonce mismatch: expected %s got %s", expectedNonce, claims.SessionNonce)
	}

	return b.applyClaims(claims)
}

func (b *Backend) applyClaims(claims *auth.Claims) error {
	var normalizedHosts []string
	if len(claims.Hostnames) > 0 {
		var err error
		normalizedHosts, err = normalizeHostnames(claims.Hostnames)
		if err != nil {
			return err
		}
	}

	tcpPorts, err := normalizeTCPPortClaims(b.config, claims.TCPPorts)
	if err != nil {
		return err
	}
	udpRoutes, err := normalizeUDPRouteClaims(b.config, claims.UDPRoutes)
	if err != nil {
		return err
	}

	if len(normalizedHosts) == 0 && len(tcpPorts) == 0 && len(udpRoutes) == 0 {
		return errors.New("claims missing hostnames and port claims")
	}

	if !sameStringSets(normalizedHosts, b.hostnames) {
		return errors.New("hostnames in token differ from registered set")
	}
	if !sameIntSets(tcpPorts, b.tcpPorts) {
		return errors.New("tcp port claims in token differ from registered set")
	}
	if !sameUDPRoutes(udpRoutes, b.udpRoutes) {
		return errors.New("udp route claims in token differ from registered set")
	}
	if claims.OutboundAllowed != b.outboundAllowed {
		return errors.New("outbound_allowed in token differs from registered value")
	}
	outboundPorts, err := normalizeOutboundPortClaims(b.config, claims.OutboundAllowed, claims.AllowedOutboundPorts)
	if err != nil {
		return err
	}
	if !sameIntSets(outboundPorts, b.allowedOutboundPorts) {
		return errors.New("outbound port claims in token differ from registered set")
	}

	if claims.Weight != 0 && claims.Weight != b.weight {
		// Weight changes are not supported without rebalancing pools.
		return errors.New("weight change requested via token is not supported")
	}

	if val := claims.ReauthIntervalSeconds; val != nil && *val > 0 {
		b.reauthInterval = time.Duration(*val) * time.Second
	} else {
		b.reauthInterval = 0
	}

	if b.reauthInterval > 0 {
		if grace := claims.ReauthGraceSeconds; grace != nil && *grace > 0 {
			b.reauthGrace = time.Duration(*grace) * time.Second
		} else {
			b.reauthGrace = defaultReauthGrace
		}
	}

	if capVal := claims.MaintenanceGraceCapSeconds; capVal != nil {
		if *capVal < 0 {
			return errors.New("maintenance_grace_cap_seconds cannot be negative")
		}
		b.maintenanceCap = time.Duration(*capVal) * time.Second
	}

	if uri := claims.AuthorizerStatusURI; uri != "" {
		if err := validateAuthorizerStatusURI(uri); err != nil {
			return fmt.Errorf("invalid authorizer_status_uri in reauth token: %w", err)
		}
		b.authorizerStatusURI = uri
	} else {
		b.authorizerStatusURI = ""
	}
	b.policyVersion = claims.PolicyVersion
	b.maintenanceUsed = 0

	return nil
}

func (b *Backend) authorizerHealthy() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.authorizerStatusURI, nil)
	if err != nil {
		return false, err
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

func (b *Backend) deferForMaintenance() bool {
	capDuration := b.maintenanceCap
	if capDuration < 0 {
		capDuration = b.config.MaintenanceGraceDefault()
	}
	if capDuration == 0 {
		return false
	}
	remaining := capDuration - b.maintenanceUsed
	if remaining <= 0 {
		return false
	}

	delay := b.reauthInterval
	if delay <= 0 || delay > remaining {
		delay = remaining
	}
	jittered := b.jitterDuration(delay)
	if jittered > remaining {
		jittered = remaining
	}

	log.Printf("INFO: Deferring re-auth for backend %s by %s (maintenance window)", b.id, jittered)

	select {
	case <-time.After(jittered):
		b.maintenanceUsed += jittered
		return true
	case <-b.quit:
		return false
	}
}

func (b *Backend) prepareForNonce(nonce string) {
	b.reauthMu.Lock()
	b.pendingNonce = nonce
	b.reauthPending = true
	b.reauthMu.Unlock()
}

func (b *Backend) clearPendingNonce() {
	b.reauthMu.Lock()
	b.pendingNonce = ""
	b.reauthPending = false
	b.reauthMu.Unlock()
}

func (b *Backend) jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	jitter := base / 5
	if jitter <= 0 {
		jitter = time.Second
	}
	n, err := cryptoRandInt64(jitter)
	if err != nil {
		return base
	}
	return base + time.Duration(n)
}

func cryptoRandInt64(max time.Duration) (int64, error) {
	if max <= 0 {
		return 0, nil
	}
	val, err := crand.Int(crand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, err
	}
	return val.Int64(), nil
}
