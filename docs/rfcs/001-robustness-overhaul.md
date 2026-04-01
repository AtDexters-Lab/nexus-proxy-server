# RFC-001: Robustness Overhaul for Nexus Proxy Backend Client

**Status:** Approved (Ready for Implementation)
**Author:** [TBD]
**Created:** 2026-01-28
**Last Updated:** 2026-01-29 (Review Round 3 - GREEN)

## Summary

This RFC proposes a phased overhaul of the Nexus Proxy Backend Client to address fundamental architectural gaps in connection management, flow control, error handling, and observability. The changes ensure the client can operate reliably in production environments where one misbehaving connection should not starve others or disrupt control plane operations.

## Problem Statement

### Current Architecture Issues

The current implementation uses a simple goroutine-per-connection model that exhibits several critical issues under production conditions:

#### 1. Bandwidth Starvation (Critical)

- **Single global send queue**: All N connections share a 256-message buffer for outbound traffic
- **Blocking enqueue**: When the queue fills, `copyLocalToNexus` goroutines block indefinitely
- **No prioritization**: Control messages (health checks, disconnect, reauth) compete equally with bulk data
- **Result**: One slow connection can starve all others; control plane operations can be delayed indefinitely

#### 2. Connection State Management (Critical)

- **No explicit state machine**: Connection states are implicit (nil checks, boolean flags)
- **Race condition**: FD leak when disconnect arrives during pending dial (lines 625-663 in client.go)
- **Ambiguous cleanup**: Multiple code paths call `safeClose()` and `Delete()` independently
- **Result**: Resource leaks, undefined behavior on rapid connect/disconnect

#### 3. Flow Control (High)

- **Ingress**: Per-connection write buffer (64 slots) overflows → immediate connection kill with data loss
- **Egress**: Global send queue blocks → head-of-line blocking across all connections
- **No protocol-level signaling**: Cannot tell Nexus to slow down for specific connections
- **Result**: Data loss, unfair resource allocation, cascading failures

#### 4. Error Handling (High)

- **No error classification**: Transient network errors and permanent auth failures both trigger reconnection
- **Reauth failure kills session**: Single reauth failure terminates entire session instead of retrying
- **Silent dial failures**: Failed dials only log, no metrics, no retry logic
- **Result**: Extended outages for recoverable errors, silent failures in production

#### 5. Observability (Medium)

- **Logging only**: No metrics, no structured events
- **Opaque errors to Nexus**: Disconnect messages carry no reason codes
- **Result**: Production debugging requires log correlation, no visibility into system health

### Quantified Impact

| Scenario | Current Behavior | Impact |
|----------|------------------|--------|
| Slow local service (10 KB/s) receiving 1 MB/s | Connection killed after 64 messages buffer | Data loss, connection churn |
| 100 connections, one blocked | All 100 block on shared send queue | Total service disruption |
| Network flap during reauth | Session terminated | All connections dropped |
| Health check during heavy traffic | Delayed by data queue | False positive timeouts |

## Goals

1. **Fairness**: One connection cannot starve others or control plane operations
2. **Graceful degradation**: Slow connections experience backpressure, not termination
3. **Resilience**: Transient failures recover automatically; permanent failures surface clearly
4. **Observability**: Library users can monitor health via programmatic APIs
5. **Backward compatibility**: Existing configurations continue to work

## Non-Goals

1. Built-in Prometheus/HTTP endpoints (library users wire their own)
2. Persistent message queuing / guaranteed delivery (sessions are ephemeral)
3. Multi-Nexus failover (out of scope for this RFC)
4. UDP flow control (UDP is best-effort; packets dropped under load is acceptable)

## Detailed Design

### Phase 1: Connection State Machine & Queue Separation

**Objective**: Eliminate race conditions and ensure control messages are never starved.

#### 1.1 Explicit Connection State Machine

Replace implicit state tracking with an explicit state machine:

```go
type ConnState uint32

const (
    ConnStatePending    ConnState = iota  // Dial in progress
    ConnStateActive                        // Connected, relaying data
    ConnStateDraining                      // Graceful shutdown, no new data
    ConnStateClosed                        // Terminal state
)

type clientConn struct {
    id       [16]byte
    state    atomic.Uint32  // ConnState

    // Only valid when state >= Active
    conn     net.Conn

    // Channels
    writeCh  chan []byte
    quit     chan struct{}

    // Metrics
    lastActivity atomic.Int64
    bytesIn      atomic.Int64
    bytesOut     atomic.Int64
}

// Atomic state transitions with validation
func (c *clientConn) transition(from, to ConnState) bool {
    return c.state.CompareAndSwap(uint32(from), uint32(to))
}
```

**State Transition Rules:**

```
                    ┌─────────────┐
                    │   Pending   │
                    └──────┬──────┘
                           │
              ┌────────────┼────────────┐
              │ dial success│            │ dial fail / disconnect
              ▼            │            ▼
        ┌─────────┐        │      ┌──────────┐
        │  Active │        │      │  Closed  │
        └────┬────┘        │      └──────────┘
             │             │            ▲
             │ graceful    │            │
             │ shutdown    │            │
             ▼             │            │
        ┌───────────┐      │            │
        │ Draining  │──────┴────────────┘
        └───────────┘        drain complete
```

**Key Invariants:**
- `conn` field is only set during `Pending → Active` transition
- `safeClose()` only called during transition to `Closed`
- `localConns.Delete()` only called after reaching `Closed`

**Draining State Semantics:**
When a connection transitions to `Draining`:
1. **No new data accepted**: Attempts to enqueue to `writeCh` return error immediately
2. **Existing data flushed**: `writeToLocal` continues processing data already in `writeCh`
3. **Per-connection drain timeout**: After 5 seconds (configurable via `connectionDrainTimeout`), force transition to `Closed`
4. **Early exit on empty**: If `writeCh` empties before timeout, transition to `Closed` immediately
5. **Closed cleanup**: `conn.Close()` called, entry removed from `localConns`, disconnect message sent

```go
const connectionDrainTimeout = 5 * time.Second

func (c *Client) drainConnection(conn *clientConn) {
    timer := time.NewTimer(connectionDrainTimeout)
    defer timer.Stop()

    for {
        select {
        case data, ok := <-conn.writeCh:
            if !ok {
                return  // Channel closed, done draining
            }
            conn.conn.Write(data)  // Best effort, ignore errors during drain
        case <-timer.C:
            return  // Drain timeout, force close
        }
    }
}
```

#### 1.2 Separate Control and Data Queues

Replace single `send` channel with priority-aware dual queues:

```go
type Client struct {
    // High-priority: auth, health checks, disconnect
    // Buffer size configurable via controlQueueSize (default: 64)
    // Should be sized to handle burst of N connections disconnecting simultaneously
    // Recommended: max(64, expectedMaxConnections * 2)
    controlSend chan outboundMessage

    // Normal-priority: data relay
    dataSend chan outboundMessage     // Buffer: 256

    // ...
}

// Initialization
func NewClient(cfg Config) *Client {
    controlQueueSize := cfg.FlowControl.ControlQueueSize
    if controlQueueSize == 0 {
        controlQueueSize = 64  // Default
    }

    return &Client{
        controlSend: make(chan outboundMessage, controlQueueSize),
        dataSend:    make(chan outboundMessage, 256),
        // ...
    }
}
```

**Modified writePump with priority:**

```go
func (c *Client) writePump(sessionCh chan struct{}) {
    defer c.wg.Done()
    ticker := time.NewTicker(pingPeriod)
    defer ticker.Stop()

    for {
        // Priority 1: Always check control messages first
        select {
        case msg := <-c.controlSend:
            if err := c.writeMessage(msg); err != nil {
                return
            }
            continue
        default:
        }

        // Priority 2: Data or other events
        select {
        case msg := <-c.controlSend:
            if err := c.writeMessage(msg); err != nil {
                return
            }
        case msg := <-c.dataSend:
            if err := c.writeMessage(msg); err != nil {
                return
            }
        case <-ticker.C:
            if err := c.writePing(); err != nil {
                return
            }
        case <-sessionCh:
            return
        case <-c.ctx.Done():
            return
        }
    }
}
```

#### 1.3 Timeout on Enqueue

Replace indefinite blocking with context-aware timeout:

```go
const enqueueTimeout = 5 * time.Second

func (c *Client) enqueueData(msg outboundMessage) error {
    select {
    case c.dataSend <- msg:
        return nil
    case <-time.After(enqueueTimeout):
        return ErrEnqueueTimeout
    case <-c.ctx.Done():
        return c.ctx.Err()
    }
}

func (c *Client) enqueueControl(msg outboundMessage) error {
    select {
    case c.controlSend <- msg:
        return nil
    case <-time.After(enqueueTimeout):
        // Control messages failing is serious - log at WARN
        return ErrEnqueueTimeout
    case <-c.ctx.Done():
        return c.ctx.Err()
    }
}
```

**On timeout**: Connection experiencing the timeout transitions to `Draining` → `Closed`. Other connections unaffected.

---

### Phase 2: Per-Connection Flow Control

**Objective**: Enable backpressure per-connection without head-of-line blocking.

#### 2.1 Protocol Extension: Pause/Resume Messages (TCP Only)

Add new control message types (requires Nexus server changes):

**Client → Nexus:**
```json
{"event": "pause_stream", "client_id": "uuid", "reason": "buffer_full"}
{"event": "resume_stream", "client_id": "uuid"}
```

**Nexus Behavior (TCP):**
- On `pause_stream`: **Stop reading from upstream client's TCP connection** for that `client_id`
- This causes the OS socket buffer to fill, TCP window to shrink, and natural backpressure to propagate to the upstream client
- On `resume_stream`: Resume reading from the upstream connection
- **No server-side buffering required** - backpressure is achieved by simply not calling `conn.Read()`

**Nexus Implementation (in `proxy/client.go`):**
```go
type Client struct {
    // ... existing fields
    paused atomic.Bool
}

// In Client.Start() read loop
for {
    // Check pause flag before reading
    for c.paused.Load() {
        select {
        case <-c.ctx.Done():
            return
        case <-time.After(50 * time.Millisecond):
        }
    }

    n, err := c.conn.Read(buf)  // Only reads when not paused
    // ...
}
```

**Handling Edge Cases:**
- `pause_stream` is valid for any client ID that Nexus has registered via `AddClient()`, regardless of backend connection state (pending or active)
- The pause flag is checked in the upstream read loop, independent of backend dial completion
- If `pause_stream` arrives for an unknown client ID (connection already cleaned up), Nexus MUST ignore it and log at DEBUG level
- If `resume_stream` arrives for an unknown or non-paused client ID, Nexus MUST ignore it

**UDP Behavior (Both Directions):**

UDP is best-effort on both Nexus and backend client sides:

**Nexus → Backend Client (Nexus side):**
- When backend is slow/paused, Nexus **drops packets at application level**
- Continues reading from UDP socket to avoid blocking other flows on the same port
- Dropped packets counted in `flow.droppedPackets`

**Backend Client → Local Service (Client side - Option C):**
- **No per-connection buffer** for UDP connections
- Data written directly to local service with 1ms write deadline
- If write fails/blocks, packet is dropped (not queued, not retried)
- Dropped packets counted in `conn.droppedPackets`
- Connection is NOT closed on drop - just continue (UDP semantics)

This symmetric design ensures UDP maintains best-effort semantics end-to-end.

**Nexus UDP Implementation (in `proxy/udp_listener.go`):**
```go
// In UDP receive loop
for {
    n, addr, err := pc.ReadFrom(buf)

    flow := table.get(addr)
    if flow == nil {
        // New flow handling...
        continue
    }

    // If backend can't keep up, drop the packet (UDP semantics)
    if flow.backend.IsConnectionPaused(flow.clientID) {
        flow.droppedPackets.Add(1)  // Observability
        continue  // Drop and move on - this is fine for UDP
    }

    flow.backend.SendData(flow.clientID, buf[:n])
}
```

#### 2.1.1 TCP vs UDP Flow Control Summary

| Aspect | TCP | UDP |
|--------|-----|-----|
| **Nexus backpressure** | Stop `conn.Read()` → TCP window shrinks | Drop packets at app level |
| **Client backpressure** | Buffer in `writeCh`, pause/resume | Direct write, drop on failure |
| **Protocol messages** | `pause_stream` / `resume_stream` | None |
| **Per-connection buffer** | Yes (`writeCh` with flow control) | No (direct write) |
| **Data loss** | None (TCP guarantees delivery) | Expected (best-effort) |
| **On slow local service** | Pause signal to Nexus | Drop packets silently |
| **Observability** | Pause/resume events | `droppedPackets` counter (both sides) |

#### 2.2 Adaptive Buffer Management

Replace fixed 64-slot buffer with adaptive flow control:

```go
type flowControl struct {
    // Buffer configuration
    lowWaterMark  int  // Resume threshold (e.g., 16)
    highWaterMark int  // Pause threshold (e.g., 48)
    maxBuffer     int  // Hard limit (e.g., 64)

    // State
    paused atomic.Bool
    level  atomic.Int32
}

type clientConn struct {
    // ... existing fields ...
    flow           flowControl
    isUDP          bool           // True for UDP connections
    droppedPackets atomic.Int64   // UDP only: packets dropped due to slow local service
}
```

**Modified handleDataMessage:**

```go
func (c *Client) handleDataMessage(clientID [16]byte, data []byte, isUDP bool) {
    conn := c.getConn(clientID)
    if conn == nil {
        return
    }

    // Check connection state BEFORE any modifications
    state := ConnState(conn.state.Load())
    if state != ConnStateActive {
        // Connection is draining or closed - reject new data
        if state == ConnStateDraining {
            log.Printf("DEBUG: Data received for draining connection %s, discarding", clientID)
        }
        return
    }

    // UDP: Direct write, no buffering (Option C)
    if isUDP {
        conn.conn.SetWriteDeadline(time.Now().Add(1 * time.Millisecond))
        _, err := conn.conn.Write(data)
        if err != nil {
            conn.droppedPackets.Add(1)  // Observability counter
            // Don't close connection for UDP - just drop and continue
        }
        return
    }

    // TCP: Flow control with buffering
    currentLevel := int(conn.flow.level.Add(1))

    if currentLevel >= conn.flow.highWaterMark && !conn.flow.paused.Load() {
        // Send pause signal
        conn.flow.paused.Store(true)
        c.enqueueControl(pauseStreamMessage(clientID, "buffer_full"))
    }

    // Non-blocking enqueue to per-connection buffer
    select {
    case conn.writeCh <- data:
        // Success
    default:
        // Hard limit reached - this shouldn't happen if pause works
        // But handle gracefully
        conn.flow.level.Add(-1)
        log.Printf("WARN: Hard buffer limit reached for %s", clientID)
        c.transitionToClosed(conn, "buffer_overflow")
    }
}
```

**Modified writeToLocal (drain tracking):**

```go
func (c *Client) writeToLocal(client *clientConn) {
    defer c.transitionToClosed(client, "write_complete")

    for {
        select {
        case data := <-client.writeCh:
            // Write to local service
            if _, err := client.conn.Write(data); err != nil {
                return
            }

            // Update flow control
            newLevel := int(client.flow.level.Add(-1))

            // Check if we should resume
            if client.flow.paused.Load() && newLevel <= client.flow.lowWaterMark {
                client.flow.paused.Store(false)
                c.enqueueControl(resumeStreamMessage(client.id))
            }

        case <-client.quit:
            return
        }
    }
}
```

**Flow Control Level Accuracy:**
- The `flow.level` counter is incremented on enqueue and decremented after successful write
- On write error, the connection transitions to `Closed` and is removed from `localConns`
- The level is **not** decremented on error paths - this is intentional
- Since the connection is being terminated, level accuracy doesn't matter; the entire `clientConn` object is discarded
- This avoids complexity of tracking partially-written data

**Pause/Resume Oscillation:**
- If buffer level hovers around water marks, rapid pause/resume cycles may occur
- Example: level hits 48 (pause), drains to 16 (resume), refills to 48 (pause), etc.
- **This is acceptable**: JSON control messages over WebSocket are cheap (~100 bytes each)
- Oscillation indicates the system is operating at capacity, which is the desired behavior
- No minimum time between transitions is enforced - simplicity over micro-optimization
- If oscillation becomes problematic in practice, add hysteresis (e.g., 100ms debounce) as future enhancement

#### 2.3 Egress Fair Queuing

Replace single data queue with per-connection queues and round-robin dispatch:

```go
type Client struct {
    // Per-connection outbound queues
    // Key: clientID, Value: chan outboundMessage
    connQueues sync.Map

    // Notifies writePump that a queue has data
    dataReady chan [16]byte  // Buffer: 256

    // ...
}

func (c *Client) enqueueData(clientID [16]byte, msg outboundMessage) error {
    queue := c.getOrCreateQueue(clientID)

    select {
    case queue <- msg:
        // Notify writePump
        select {
        case c.dataReady <- clientID:
        default:
            // Already notified, writePump will drain
        }
        return nil
    case <-time.After(enqueueTimeout):
        return ErrEnqueueTimeout
    case <-c.ctx.Done():
        return c.ctx.Err()
    }
}

func (c *Client) writePump(sessionCh chan struct{}) {
    for {
        // Priority 1: Control messages
        select {
        case msg := <-c.controlSend:
            c.writeMessage(msg)
            continue
        default:
        }

        // Priority 2: Round-robin data from ready connections
        select {
        case msg := <-c.controlSend:
            c.writeMessage(msg)
        case clientID := <-c.dataReady:
            c.drainConnectionQueue(clientID, maxMessagesPerTurn) // e.g., 4
        case <-ticker.C:
            c.writePing()
        case <-sessionCh:
            return
        }
    }
}

// Drain up to N messages from one connection's queue
func (c *Client) drainConnectionQueue(clientID [16]byte, maxMessages int) {
    queue := c.getQueue(clientID)
    if queue == nil {
        return
    }

    drained := 0
    for i := 0; i < maxMessages; i++ {
        select {
        case msg := <-queue:
            if err := c.writeMessage(msg); err != nil {
                return
            }
            drained++
        default:
            return  // Queue empty, no re-notify needed
        }
    }

    // Hit maxMessages limit (drained == maxMessages), there may be more data
    // Re-notify so writePump will come back to this connection
    select {
    case c.dataReady <- clientID:
    default:
        // Already notified
    }
}
```

**Fairness Guarantee**: Each connection gets at most `maxMessagesPerTurn` writes before yielding. Prevents one high-throughput connection from monopolizing the WebSocket.

**Per-Connection Queue Cleanup:**

When a connection transitions to `Closed`, its per-connection egress queue must be cleaned up:

```go
func (c *Client) cleanupConnectionQueue(clientID [16]byte) {
    // Remove queue from map
    if queue, ok := c.connQueues.LoadAndDelete(clientID); ok {
        ch := queue.(chan outboundMessage)
        // Drain and discard any remaining messages
        for {
            select {
            case <-ch:
                // Discard
            default:
                close(ch)
                return
            }
        }
    }
}

// Call from transitionToClosed()
func (c *Client) transitionToClosed(conn *clientConn, reason string) {
    if !conn.transition(conn.state.Load(), uint32(ConnStateClosed)) {
        return  // Already closed
    }

    conn.safeClose()
    c.cleanupConnectionQueue(conn.id)  // Clean up egress queue
    c.localConns.Delete(conn.id)
    c.sendControlMessage("disconnect", conn.id, reason)
    c.emit(Event{Type: EventConnectionClosed, ClientID: conn.id, Reason: reason})
}
```

**Handling stale `dataReady` notifications:**

The `writePump` may receive `dataReady` notifications for connections that have already closed. This is handled gracefully:

```go
func (c *Client) drainConnectionQueue(clientID [16]byte, maxMessages int) {
    queue := c.getQueue(clientID)
    if queue == nil {
        return  // Connection already cleaned up, ignore notification
    }
    // ... rest of drain logic
}
```

---

### Phase 3: Error Classification & Resilience

**Objective**: Distinguish transient from permanent failures; implement graceful degradation.

#### 3.1 Error Categories

```go
type ErrorCategory int

const (
    ErrorTransient  ErrorCategory = iota  // Retry with backoff
    ErrorPermanent                         // Don't retry, surface to user
    ErrorRateLimit                         // Retry with longer backoff
)

type CategorizedError struct {
    Err      error
    Category ErrorCategory
    Reason   string  // Machine-readable reason code
}

func categorizeError(err error) CategorizedError {
    switch {
    case errors.Is(err, syscall.ECONNREFUSED):
        return CategorizedError{err, ErrorTransient, "connection_refused"}
    case errors.Is(err, syscall.ETIMEDOUT):
        return CategorizedError{err, ErrorTransient, "timeout"}
    case errors.Is(err, ErrAuthFailed):
        return CategorizedError{err, ErrorPermanent, "auth_failed"}
    case errors.Is(err, ErrBackendNotRegistered):
        return CategorizedError{err, ErrorPermanent, "not_registered"}
    case strings.Contains(err.Error(), "rate limit"):
        return CategorizedError{err, ErrorRateLimit, "rate_limited"}
    default:
        return CategorizedError{err, ErrorTransient, "unknown"}
    }
}
```

#### 3.2 Differentiated Retry Strategy

```go
func (c *Client) Start(ctx context.Context) error {
    var transientRetries, permanentFailures int

    for {
        err := c.connectAndAuthenticate()
        if err == nil {
            transientRetries = 0
            permanentFailures = 0
            c.runSession()
            continue
        }

        catErr := categorizeError(err)

        switch catErr.Category {
        case ErrorTransient:
            transientRetries++
            delay := c.transientBackoff(transientRetries)
            c.stats.TransientErrors.Add(1)
            log.Printf("WARN: Transient error: %v. Retry %d in %s", err, transientRetries, delay)

        case ErrorPermanent:
            permanentFailures++
            if permanentFailures >= maxPermanentFailures {
                c.stats.PermanentErrors.Add(1)
                return fmt.Errorf("permanent failure after %d attempts: %w", permanentFailures, err)
            }
            delay := c.permanentBackoff(permanentFailures)
            log.Printf("ERROR: Permanent error: %v. Attempt %d/%d, retry in %s",
                err, permanentFailures, maxPermanentFailures, delay)

        case ErrorRateLimit:
            delay := c.rateLimitBackoff()
            c.stats.RateLimitHits.Add(1)
            log.Printf("WARN: Rate limited. Backing off for %s", delay)
        }

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(delay):
        case <-c.networkWakeup:
            transientRetries = 0  // Network change, reset transient counter
        }
    }
}
```

#### 3.3 Graceful Shutdown

```go
const drainTimeout = 10 * time.Second

func (c *Client) Shutdown(ctx context.Context) error {
    // Signal no new connections
    c.shuttingDown.Store(true)

    // Handle all connections based on their state
    c.localConns.Range(func(key, value interface{}) bool {
        if conn, ok := value.(*clientConn); ok {
            state := ConnState(conn.state.Load())
            switch state {
            case ConnStatePending:
                // Pending connections: cancel dial, transition directly to Closed
                // conn.conn is nil, so nothing to drain
                conn.transition(ConnStatePending, ConnStateClosed)
                c.localConns.Delete(key)
            case ConnStateActive:
                // Active connections: transition to Draining for graceful close
                conn.transition(ConnStateActive, ConnStateDraining)
            case ConnStateDraining, ConnStateClosed:
                // Already shutting down, no action needed
            }
        }
        return true
    })

    // Wait for queues to drain (with timeout)
    drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
    defer cancel()

    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-drainCtx.Done():
            log.Printf("WARN: Drain timeout, forcing close")
            goto forceClose
        case <-ticker.C:
            if c.queuesEmpty() {
                goto forceClose
            }
        }
    }

forceClose:
    // Close WebSocket gracefully
    if c.ws != nil {
        c.ws.WriteControl(
            websocket.CloseMessage,
            websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"),
            time.Now().Add(time.Second),
        )
    }

    // Close all local connections
    c.closeAllLocalConns()

    return nil
}
```

#### 3.4 Disconnect Reason Codes

Extend protocol to include reason in disconnect messages:

```go
type DisconnectReason string

const (
    DisconnectNormal       DisconnectReason = "normal"
    DisconnectBufferFull   DisconnectReason = "buffer_full"      // Hard buffer limit hit
    DisconnectDialFailed   DisconnectReason = "dial_failed"      // Failed to connect to local service
    DisconnectTimeout      DisconnectReason = "timeout"          // Enqueue timeout (5s default)
    DisconnectLocalError   DisconnectReason = "local_error"      // Error writing to local service
    DisconnectShutdown     DisconnectReason = "shutdown"         // Client shutting down
    DisconnectPauseViolated DisconnectReason = "pause_violated"  // Nexus kept sending despite pause
)

// Protocol message
{"event": "disconnect", "client_id": "uuid", "reason": "buffer_full"}
```

**Disconnect Reason Selection:**

| Scenario | Reason |
|----------|--------|
| Enqueue times out (5s) waiting for buffer space | `timeout` |
| Non-blocking enqueue fails (buffer at hard limit) | `buffer_full` |
| Pause sent but buffer still hit hard limit | `pause_violated` |
| Local service write returns error | `local_error` |
| Dial to local service fails | `dial_failed` |
| Client.Shutdown() called | `shutdown` |
| Normal connection close | `normal` |

**Handling `pause_violated`:**

If the client sends `pause_stream` but Nexus continues sending data and the buffer hits the hard limit:

1. Log at WARN level: "Nexus continued sending after pause_stream, buffer overflow"
2. Disconnect with reason `pause_violated`
3. Increment `stats.PauseViolations` counter

This indicates either:
- In-flight messages that were already sent before pause was processed (expected, transient)
- Nexus bug or protocol mismatch (unexpected, investigate)

```go
// In handleDataMessage, when hard limit hit:
if conn.flow.paused.Load() {
    // We already sent pause but buffer still overflowed
    log.Printf("WARN: Pause violated for connection %s - Nexus may not support pause_stream", clientID)
    c.stats.PauseViolations.Add(1)
    c.transitionToClosed(conn, "pause_violated")
} else {
    c.transitionToClosed(conn, "buffer_overflow")
}
```

---

### Phase 4: Observability (Library-Friendly)

**Objective**: Expose metrics and events via programmatic APIs.

#### 4.1 Stats Struct

```go
type Stats struct {
    // Connection metrics
    ActiveConnections    int64
    TotalConnections     int64
    PendingConnections   int64

    // Data transfer
    BytesSentTotal       int64
    BytesReceivedTotal   int64
    MessagesSentTotal    int64
    MessagesReceivedTotal int64

    // Queue metrics
    ControlQueueDepth    int
    DataQueueDepth       int

    // Error metrics
    DroppedConnections   int64
    TransientErrors      int64
    PermanentErrors      int64
    RateLimitHits        int64
    EnqueueTimeouts      int64

    // Flow control
    PausedConnections    int64
    PauseViolations      int64  // Times buffer overflowed despite pause being sent

    // Event metrics
    DroppedEvents        int64  // Events dropped due to slow handler

    // UDP metrics (client-side)
    UDPDroppedPackets    int64  // Packets dropped due to slow local UDP service

    // Session metrics
    SessionUptime        time.Duration
    LastUpdated          time.Time  // When these stats were computed (for cache staleness detection)
    ReconnectCount       int64
    LastConnectedAt      time.Time

    // Per-connection stats (optional, can be expensive)
    ConnectionStats      map[string]ConnectionStats  // nil if not enabled
}

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
}

// Thread-safe stats retrieval
func (c *Client) Stats() Stats {
    return Stats{
        ActiveConnections:  c.stats.activeConns.Load(),
        TotalConnections:   c.stats.totalConns.Load(),
        BytesSentTotal:     c.stats.bytesSent.Load(),
        BytesReceivedTotal: c.stats.bytesRecv.Load(),
        ControlQueueDepth:  len(c.controlSend),
        DataQueueDepth:     len(c.dataSend),
        // ... etc
    }
}

// Detailed per-connection stats (more expensive)
func (c *Client) StatsDetailed() Stats {
    stats := c.Stats()
    stats.ConnectionStats = make(map[string]ConnectionStats)

    c.localConns.Range(func(key, value interface{}) bool {
        if conn, ok := value.(*clientConn); ok {
            id := fmt.Sprintf("%x", conn.id)
            stats.ConnectionStats[id] = ConnectionStats{
                ClientID:     id,
                Hostname:     conn.hostname,
                State:        ConnState(conn.state.Load()),
                BytesIn:      conn.bytesIn.Load(),
                BytesOut:     conn.bytesOut.Load(),
                BufferLevel:  int(conn.flow.level.Load()),
                Paused:       conn.flow.paused.Load(),
                LastActivity: time.Unix(conn.lastActivity.Load(), 0),
            }
        }
        return true
    })

    return stats
}
```

#### 4.2 Event Callbacks

```go
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

type Event struct {
    Type      EventType
    Timestamp time.Time

    // Connection context (if applicable)
    ClientID  string
    Hostname  string

    // Error context (if applicable)
    Error     error
    Reason    string

    // Metrics snapshot (if applicable)
    Stats     *Stats
}

type EventHandler func(Event)

// Option to register handler
func WithEventHandler(h EventHandler) Option {
    return func(c *Client) {
        c.eventHandler = h
    }
}

// Internal emit helper - see "Resolved Design Decisions" section 5 for
// the actual implementation using buffered channel with ordered delivery
```

#### 4.3 Usage Example

```go
client, _ := client.New(cfg,
    client.WithEventHandler(func(e client.Event) {
        switch e.Type {
        case client.EventConnected:
            metrics.NexusConnected.Set(1)
        case client.EventDisconnected:
            metrics.NexusConnected.Set(0)
        case client.EventError:
            metrics.ErrorsTotal.WithLabelValues(e.Reason).Inc()
        case client.EventConnectionClosed:
            if e.Reason == "buffer_full" {
                metrics.BufferOverflows.Inc()
            }
        }
    }),
)

// Periodic stats export
go func() {
    ticker := time.NewTicker(10 * time.Second)
    for range ticker.C {
        stats := client.Stats()
        metrics.ActiveConnections.Set(float64(stats.ActiveConnections))
        metrics.BytesSent.Add(float64(stats.BytesSentTotal))
        // ...
    }
}()
```

---

## Protocol Changes Summary

Changes required on Nexus server:

| Message | Direction | Purpose |
|---------|-----------|---------|
| `pause_stream` | Client → Nexus | Request Nexus stop reading from upstream TCP connection |
| `resume_stream` | Client → Nexus | Request Nexus resume reading from upstream TCP connection |
| `disconnect` (extended) | Bidirectional | Now includes `reason` field |

**Nexus Server Requirements:**

1. **TCP Flow Control:**
   - Track per-client pause state via `paused atomic.Bool` in Client struct
   - On `pause_stream`: Set pause flag; read loop checks flag and waits instead of reading
   - On `resume_stream`: Clear pause flag; read loop resumes
   - **No server-side buffering** - TCP backpressure propagates naturally via OS socket buffer and TCP window

2. **UDP Handling (Best-Effort):**
   - No pause/resume protocol for UDP
   - When backend signals a connection is paused, drop incoming UDP packets at application level
   - Count dropped packets in `droppedPackets` counter for observability
   - Continue reading from UDP socket to avoid blocking other flows on the same port
   - Requires new method on Backend interface:
   ```go
   // Add to Backend interface in hub/backend.go
   type Backend interface {
       // ... existing methods
       IsConnectionPaused(clientID [16]byte) bool
   }

   // Implementation tracks per-client pause state
   type backend struct {
       // ... existing fields
       pausedClients sync.Map  // clientID -> struct{}
   }

   func (b *backend) IsConnectionPaused(clientID [16]byte) bool {
       _, paused := b.pausedClients.Load(clientID)
       return paused
   }
   ```

3. **Reduced Outgoing Buffer:**
   - Change outgoing channel from 256 to configurable size (default: 16-32 slots)
   - Maximum in-flight data: ~512KB-1MB per backend (down from ~8MB)
   - Configuration option: `outgoingBufferSize` in Nexus config

4. **Observability:**
   - Log pause/resume events at DEBUG level
   - Expose metrics: `paused_connections`, `udp_dropped_packets`

---

## Nexus Server-Side Changes (Detailed)

This section provides the complete specification for Nexus server modifications required for Phase 2.

### Protocol Event Types

Add to `/internal/protocol/protocol.go`:

```go
// Add new event types (after existing EventType constants)
const (
    EventPauseStream  EventType = "pause_stream"
    EventResumeStream EventType = "resume_stream"
)

// ControlMessage already supports arbitrary fields via JSON
// pause_stream includes: {"event": "pause_stream", "client_id": "...", "reason": "buffer_full"}
// resume_stream includes: {"event": "resume_stream", "client_id": "..."}
```

### Backend Handler Changes

Modify `/internal/hub/backend.go`:

```go
// Add to backend struct
type backend struct {
    // ... existing fields
    pausedClients sync.Map  // map[string]struct{} - clientID -> paused marker
}

// Add method to check pause state (used by UDP handler)
func (b *backend) IsConnectionPaused(clientID string) bool {
    _, paused := b.pausedClients.Load(clientID)
    return paused
}

// Add to handleControlMessage switch statement (in readPump)
case protocol.EventPauseStream:
    clientID := msg.ClientID
    b.pausedClients.Store(clientID, struct{}{})
    // Notify the TCP client to pause (via channel or direct reference)
    if client := b.getClientByID(clientID); client != nil {
        client.SetPaused(true)
    }
    log.Printf("DEBUG: [%s] Paused stream for client %s, reason: %s", b.name, clientID, msg.Reason)

case protocol.EventResumeStream:
    clientID := msg.ClientID
    b.pausedClients.Delete(clientID)
    if client := b.getClientByID(clientID); client != nil {
        client.SetPaused(false)
    }
    log.Printf("DEBUG: [%s] Resumed stream for client %s", b.name, clientID)
```

### Client Lookup Mechanism

The backend currently stores clients as `net.Conn` in a sync.Map. To support pause/resume, we need to store the `*Client` object instead:

```go
// Change in backend struct
type backend struct {
    // Change from: clients sync.Map  // map[string]net.Conn
    // To:
    clients sync.Map  // map[string]*clientEntry
}

type clientEntry struct {
    conn   net.Conn
    client *proxy.Client  // Reference to the proxy Client for pause control
}

// Add helper method
func (b *backend) getClientByID(clientID string) *proxy.Client {
    if entry, ok := b.clients.Load(clientID); ok {
        if ce, ok := entry.(*clientEntry); ok {
            return ce.client
        }
    }
    return nil
}
```

### TCP Client Pause Implementation

Modify `/internal/proxy/client.go`:

```go
type Client struct {
    // ... existing fields
    paused    atomic.Bool
    pauseCh   chan struct{}  // Signaled when pause state changes
}

func NewClient(conn net.Conn, ...) *Client {
    return &Client{
        // ... existing initialization
        pauseCh: make(chan struct{}, 1),
    }
}

func (c *Client) SetPaused(paused bool) {
    c.paused.Store(paused)
    // Non-blocking signal to wake up read loop
    select {
    case c.pauseCh <- struct{}{}:
    default:
    }
}

func (c *Client) Start() {
    // ... existing setup code

    buf := make([]byte, copyBufferSize)
    for {
        // Wait while paused
        for c.paused.Load() {
            select {
            case <-c.ctx.Done():
                return
            case <-c.pauseCh:
                // Re-check pause state
            case <-time.After(100 * time.Millisecond):
                // Periodic check in case signal was missed
            }
        }

        c.conn.SetReadDeadline(time.Now().Add(idleTimeout))
        n, err := c.conn.Read(buf)
        if err != nil {
            // ... existing error handling
            return
        }

        if err := c.backend.SendData(c.id, buf[:n]); err != nil {
            return
        }
    }
}
```

### UDP Handler Changes

Modify `/internal/proxy/udp_listener.go`:

```go
// In the UDP receive loop
for {
    n, addr, err := pc.ReadFrom(buf)
    if err != nil {
        break
    }

    flow := table.get(addr)
    if flow == nil {
        // New flow handling...
        continue
    }

    // Check if this flow's backend connection is paused
    if flow.backend.IsConnectionPaused(flow.clientID) {
        atomic.AddInt64(&flow.droppedPackets, 1)
        continue  // Drop packet, continue reading (don't block other flows)
    }

    flow.backend.SendData(flow.clientID, buf[:n])
}
```

### Wiring Backend to Client

When a new TCP connection is accepted, the backend must be informed of the Client object:

```go
// In listener.go, after creating Client
client := proxy.NewClient(conn, backend, clientID, hostname, isTLS)
backend.RegisterClient(clientID, conn, client)  // New method
go client.Start()

// In backend.go
func (b *backend) RegisterClient(clientID string, conn net.Conn, client *proxy.Client) {
    b.clients.Store(clientID, &clientEntry{
        conn:   conn,
        client: client,
    })
}
```

---

## Migration Strategy

### Deployment Assumptions

- **No existing Nexus deployments** - This is a greenfield deployment
- **Server and client deployed together** - Nexus server will always include pause/resume support
- **No backward compatibility required** - All components run the latest version

This simplifies deployment significantly: no version negotiation, no feature detection, no legacy fallback paths.

### Rollout Plan

1. **Phase 1**: Deploy client changes only (no Nexus changes needed)
2. **Phase 2**: Deploy Nexus server with pause/resume support, then deploy updated client
3. **Phase 3**: Client-only changes
4. **Phase 4**: Client-only changes

### Future Consideration

If backward compatibility becomes necessary (e.g., gradual rollouts, multi-tenant deployments), the following can be added later:
- Protocol version field in attestation claims
- Feature capability negotiation during handshake
- Graceful fallback to buffer-and-kill behavior for legacy servers

### Configuration Migration

New configuration options (with backward-compatible defaults):

```yaml
# New in Phase 2
flowControl:
  enabled: true              # Default: true
  highWaterMark: 48          # Default: 48
  lowWaterMark: 16           # Default: 16
  maxBuffer: 64              # Default: 64
  maxMessagesPerTurn: 4      # Default: 4 (messages per connection per round-robin cycle)
  controlQueueSize: 64       # Default: 64 (increase for high connection counts)

# New in Phase 3
resilience:
  maxPermanentFailures: 3    # Default: 3
  drainTimeout: 10s          # Default: 10s (global shutdown drain)
  connectionDrainTimeout: 5s # Default: 5s (per-connection drain)

# New in Phase 4
observability:
  detailedStats: false       # Default: false (expensive)
  eventBufferSize: 256       # Default: 256 (events buffered before dropping)
```

---

## Testing Strategy

### Unit Tests

- State machine transitions (all valid/invalid paths)
- Flow control water mark triggers
- Error categorization
- Queue priority behavior

### Integration Tests

- Slow local service (verify pause/resume cycle)
- Multiple connections with one slow (verify fairness)
- Network flap during various states
- Graceful shutdown with pending data
- Reauth during high traffic

### Chaos Tests

- Random connection kills
- Nexus restart during active session
- Local service crashes
- Memory pressure scenarios

### Benchmarks

- Throughput with N connections (before/after)
- Latency percentiles (p50, p99, p999)
- Memory usage under load
- GC pause impact

---

## Resolved Design Decisions

The following questions were resolved during RFC review:

### 1. Server-side buffer limit
**Decision:** No server-side buffering. Backpressure via stopping `conn.Read()`.

- TCP: Stop reading from upstream → OS buffer fills → TCP window shrinks → natural backpressure
- UDP: Drop packets at application level (best-effort semantics)
- Nexus outgoing channel reduced from 256 to 16-32 slots (~512KB-1MB max in-flight)

**Rationale:** Simpler implementation, no memory accumulation, leverages TCP's built-in flow control.

### 2. Pause/resume protocol format
**Decision:** JSON (Option A)

```json
{"event": "pause_stream", "client_id": "uuid", "reason": "buffer_full"}
```

**Rationale:** These are infrequent control messages (only on threshold crossings). Debuggability outweighs micro-optimization.

### 3. Fair queuing granularity
**Decision:** Pure round-robin (Option A)

- Each connection gets `maxMessagesPerTurn` (default: 4) messages before yielding
- No weighting by priority or connection age

**Rationale:** Start simple. Weighting can be added later if needed. The existing `weight` config field is for Nexus-side load balancing, not client-side queuing.

### 4. Detailed stats availability
**Decision:** Always available, internally rate-limited (Option C)

```go
// Internal caching with 1-second TTL
func (c *Client) StatsDetailed() Stats {
    c.statsCacheMu.RLock()
    if time.Since(c.statsCacheTime) < time.Second {
        cached := c.statsCache
        c.statsCacheMu.RUnlock()
        return cached
    }
    c.statsCacheMu.RUnlock()

    // Compute fresh stats...
    stats := c.computeDetailedStats()

    c.statsCacheMu.Lock()
    c.statsCache = stats
    c.statsCacheTime = time.Now()
    c.statsCacheMu.Unlock()

    return stats
}
```

**Rationale:** Simple API, prevents accidental hot-path performance issues.

### 5. Event handler threading
**Decision:** Async with ordered delivery (Option D)

```go
type Client struct {
    eventCh chan Event  // Buffered channel for ordering
}

func (c *Client) startEventDispatcher() {
    go func() {
        for event := range c.eventCh {
            if c.eventHandler != nil {
                c.eventHandler(event)  // Delivered in order
            }
        }
    }()
}

func (c *Client) emit(event Event) {
    event.Timestamp = time.Now()
    select {
    case c.eventCh <- event:
    default:
        // Buffer full - drop event (shouldn't happen with reasonable buffer)
        c.stats.droppedEvents.Add(1)
    }
}
```

**Rationale:** Order matters for debugging (e.g., "connected" before "connection_opened"). Async ensures slow handlers don't block client operations. Single dispatcher goroutine guarantees ordering.

**Note:** This is for observability events only, not data message delivery. Data ordering is handled by TCP (in-order) or UDP (best-effort/no guarantee) at the transport level.

---

## Appendix A: Current vs Proposed Architecture

### Current

```
                           ┌──────────────────────────────┐
                           │         Client               │
                           │                              │
  Nexus ─────WebSocket────►│  readPump ──► handleMsg     │
                           │                    │         │
                           │              ┌─────┴─────┐   │
                           │              ▼           ▼   │
                           │         writeCh[64]  send[256]│◄── All conns share
                           │              │           │   │
                           │              ▼           │   │
                           │        writeToLocal      │   │
                           │              │           │   │
                           │              ▼           │   │
  Local ◄────TCP──────────│────── net.Conn          │   │
  Service                  │                          │   │
                           │              writePump ◄─┘   │
                           │                   │          │
  Nexus ◄────WebSocket────│───────────────────┘          │
                           └──────────────────────────────┘
```

### Proposed

```
                           ┌────────────────────────────────────┐
                           │            Client                  │
                           │                                    │
  Nexus ─────WebSocket────►│  readPump ──► handleMsg           │
                           │       │            │               │
                           │       │      ┌─────┴──────┐        │
                           │       │      ▼            ▼        │
                           │       │  flowCtrl    connQueue[N]  │◄── Per-conn
                           │       │  pause/      (fair RR)     │
                           │       │  resume          │         │
                           │       │      │           │         │
                           │       ▼      ▼           ▼         │
                           │  controlSend[64]   dataSend[256]   │◄── Separate
                           │       │                  │         │
                           │       └────────┬─────────┘         │
                           │                ▼                   │
                           │           writePump                │
                           │         (priority: ctrl)           │
                           │                │                   │
  Nexus ◄────WebSocket────│────────────────┘                   │
                           │                                    │
                           │  Per-conn:                         │
                           │  ┌─────────────────┐               │
                           │  │ state machine   │               │
                           │  │ Pending→Active  │               │
                           │  │ →Draining→Closed│               │
                           │  │                 │               │
                           │  │ writeCh[64]     │               │
                           │  │ + flow control  │               │
                           │  └────────┬────────┘               │
                           │           ▼                        │
  Local ◄────TCP──────────│────── net.Conn                     │
  Service                  └────────────────────────────────────┘
```

---

## Appendix B: Bandwidth Fairness Proof

With the proposed round-robin fair queuing:

**Given:**
- N connections
- Connection i has arrival rate λᵢ
- WebSocket capacity C messages/sec
- `maxMessagesPerTurn` = M

**Guarantee:**
- Each connection gets at most M messages sent per round-robin cycle
- Cycle time = N × (time to send M messages) = N × M / C
- Per-connection throughput ≤ M / (N × M / C) = C / N

**Result:** In the worst case (all connections saturated), each gets fair share C/N. Connections using less than their share don't block others.

---

## References

- [RFC 7540: HTTP/2](https://tools.ietf.org/html/rfc7540) - Flow control design
- [QUIC RFC 9000](https://www.rfc-editor.org/rfc/rfc9000) - Stream multiplexing
- [Envoy Proxy Architecture](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/arch_overview) - Production proxy patterns
- [Go sync.Map documentation](https://pkg.go.dev/sync#Map) - Concurrent map semantics
