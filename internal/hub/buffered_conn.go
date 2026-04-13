package hub

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
)

// clientWriteBufferSize is the per-client async write buffer size.
//
// Sized to 2× the credit window so transient RTT slack between credit grants
// and drain progress never exhausts the buffer. At steady state, the buffer
// depth oscillates around `credit_window − (drain_rate × RTT)`, well below
// the credit window for normal RTTs (<500ms). Doubling gives one full credit
// window of additional headroom.
//
// Hitting the non-blocking drop path (Write returning "client write buffer
// full") now indicates a protocol-violating sender that exceeded the credit
// contract — not a routine RTT race. The drop path triggers RemoveClient
// (see backend.go handleBinaryMessage error branch) to fully clean up state.
const clientWriteBufferSize = 2 * int(protocol.DefaultCreditCapacity)

// bandwidthGate is the contract bufferedConn.drain uses to honor a
// per-backend bandwidth budget. Implementations must be concurrency-safe
// because a single backend may have many drain goroutines all calling
// through the same gate concurrently.
//
// Request asks permission to send `bytes` user bytes. Returns (true, 0)
// if the reservation was made, (false, waitTime) if the caller should
// wait before retrying. On (false, _) no reservation is held.
//
// Record commits metrics for a previously-approved send. Called only
// after Request returned true (regardless of whether the subsequent
// downstream write succeeded — see drain for rationale).
//
// Note: there is intentionally no Refund method. Bytes that reach drain
// have already crossed the backend↔proxy WS link, so they must count
// against the cap regardless of whether the per-client TCP write
// succeeds. Refunding on TCP write failure would let the backend
// silently bypass totalBandwidthMbps under slow-client conditions.
type bandwidthGate interface {
	Request(bytes int) (bool, time.Duration)
	Record(bytes int)
}

// bufferedConn wraps a net.Conn with an asynchronous write buffer.
//
// Writes are enqueued to a channel and drained by a dedicated goroutine,
// preventing the caller (readPump) from blocking on slow client TCP sockets.
// This breaks the circular dependency that occurs when two clients on the
// same backend create a data flow loop (e.g. the self-loop topology where
// one client carries tunnel ACKs needed to unblock another client's TCP write).
//
// Flow control: the drain goroutine calls onReplenish after every
// CreditReplenishBatch successful TCP writes, allowing the sender to
// replenish the receiver's credits and resume sending. The callback returns
// an error on transient failure (e.g., control channel full); drain preserves
// the consumed counter and retries on the next opportunity (next data write
// or replenishTicker fire). This is the contract that prevents silent credit
// loss — the original bug fixed by this design.
//
// Bandwidth gating: if `gate` is non-nil, drain blocks on gate.Request
// before each TCP write. The wait is localized to this drain goroutine
// so one throttled client does not stall ingress for siblings on the
// same backend (the HOL class of bug this gate relocation was designed
// to fix).
type bufferedConn struct {
	net.Conn
	writeCh      chan []byte
	quit         chan struct{}
	done         chan struct{} // closed when drain goroutine exits
	closeOnce    sync.Once
	writeTimeout time.Duration
	// onReplenish is called from the drain goroutine after each CreditReplenishBatch
	// of successful TCP writes. It must be non-blocking. Returns nil if the credits
	// were successfully enqueued (drain may then reset its consumed counter); returns
	// an error if the enqueue failed (drain preserves the consumed counter for retry).
	onReplenish func(int64) error
	// gate is the per-backend bandwidth budget. nil = unlimited.
	gate bandwidthGate
	// gateBlockObserver, if non-nil, is called from drain with the elapsed
	// duration of a gate wait, exactly once per frame that was blocked at
	// least once (Request returned false ≥ 1 time). Frames that passed the
	// gate on first call do not invoke the observer. Backend uses this to
	// accumulate gateBlockEvents / gateBlockMillisTotal metrics. Called
	// synchronously on the drain goroutine — **must not block** and should
	// avoid unbounded I/O. Atomic ops are fine. Bounded I/O is acceptable
	// (Backend.observeGateBlock emits at most one WARN log per
	// gateBlockWarnDeltaMillis of cumulative block time per backend), but
	// any pattern that could stall drain under load defeats the HOL fix.
	gateBlockObserver func(time.Duration)
	lastWriteAt       atomic.Int64 // UnixNano of last successful TCP write; used for bidirectional idle detection

	// innerGateReplenishObserved is a test-only counter incremented
	// only when the replenish retry timer fires INSIDE drain's inner
	// gate-wait select branch. The load-bearing assertion in
	// TestBufferedConn_GateDoesNotBlockReplenishRetry: the test fails
	// if the inner-loop case <-replenishTimerCh branch is ever removed.
	// Written from drain, read from tests via export_test.go.
	innerGateReplenishObserved atomic.Int64
}

// newBufferedConn wraps conn with an asynchronous write buffer.
// writeTimeout is the deadline applied to each individual TCP write;
// pass 0 to skip write deadlines. onReplenish is called from the drain
// goroutine after every CreditReplenishBatch successful writes; pass nil
// to disable credit replenishment. The callback must return nil on success
// and an error on transient failure (drain will retry). gate is the
// per-backend bandwidth gate; pass nil to disable bandwidth gating.
// gateBlockObs, if non-nil, is called from drain with the cumulative
// wait duration once per frame that was blocked in the gate at least
// once (see bufferedConn.gateBlockObserver). All fields are assigned
// before the drain goroutine is spawned, so there is no publication
// race on subsequent reads from drain.
func newBufferedConn(
	conn net.Conn,
	writeTimeout time.Duration,
	onReplenish func(int64) error,
	gate bandwidthGate,
	gateBlockObs func(time.Duration),
) *bufferedConn {
	bc := &bufferedConn{
		Conn:              conn,
		writeCh:           make(chan []byte, clientWriteBufferSize),
		quit:              make(chan struct{}),
		done:              make(chan struct{}),
		writeTimeout:      writeTimeout,
		onReplenish:       onReplenish,
		gate:              gate,
		gateBlockObserver: gateBlockObs,
	}
	go bc.drain()
	return bc
}

// Write enqueues data for asynchronous delivery to the underlying connection.
// It copies the data (caller may reuse its buffer) and never blocks on the
// underlying TCP write. Returns an error if the buffer is full or the
// connection has been closed.
func (bc *bufferedConn) Write(data []byte) (int, error) {
	buf := make([]byte, len(data))
	copy(buf, data)

	// Check closed first — if quit is already signaled (e.g. drain goroutine
	// died on write error), return immediately instead of enqueuing data
	// that will never be drained.
	select {
	case <-bc.quit:
		return 0, net.ErrClosed
	default:
	}

	// Non-blocking enqueue. readPump must never block here — any blocking
	// recreates the self-loop deadlock for the duration of the block.
	select {
	case bc.writeCh <- buf:
		return len(data), nil
	case <-bc.quit:
		return 0, net.ErrClosed
	default:
		return 0, fmt.Errorf("client write buffer full (%d pending)", clientWriteBufferSize)
	}
}

// replenishTickerInterval is the period at which drain() opportunistically
// flushes pending credits even when no new write events have occurred.
// Sized for IoT/interactive latency sensitivity (worst-case stranded credit
// latency ≈ this interval) — not for throughput.
const replenishTickerInterval = 50 * time.Millisecond

// minReplenishRetryBackoff is the minimum delay between retry attempts after
// a failed onReplenish call. Prevents tight retry loops without adding
// noticeable latency for IoT workloads.
const minReplenishRetryBackoff = 25 * time.Millisecond

// retryStreakWarnThreshold is how many consecutive replenish failures must
// occur before drain logs a WARN. The WARN signals a persistently wedged
// control plane that warrants operator investigation.
const retryStreakWarnThreshold = 50

// stopAndDrainTimer halts t and discards any pending value on its channel.
// Canonical "Stop, drain stale firing, ready to Reset" sequence. Caller
// is responsible for the subsequent Reset. Safe ONLY when the timer is
// owned by a single goroutine — concurrent receivers on t.C would race
// the drain.
func stopAndDrainTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// drain writes buffered data to the underlying connection. Runs in its
// own goroutine until the connection is closed or a write error occurs.
// On quit signal, flushes any remaining buffered data before exiting.
//
// Credit replenishment: every CreditReplenishBatch successful TCP writes,
// drain calls onReplenish. If the callback returns nil, the consumed counter
// is reset. If it returns an error, the counter is preserved and a retry is
// attempted on the next data write or via a per-bufferedConn replenish timer,
// gated by minReplenishRetryBackoff to prevent retry amplification.
//
// The replenish timer is ONLY armed after a failed batch replenishment —
// it exists to retry stuck batches, not to opportunistically flush sub-batch
// credits. Flushing sub-batch credits would create a control-plane storm
// at fan-out (every connection draining 1–7 frames in a 50ms window emits
// its own credit frame, saturating the 256-slot outgoingControl lane).
// Stranded sub-batch credits self-correct when the next burst on the same
// client crosses the batch threshold; idle connections don't need
// replenishment because they aren't sending data.
//
// flushRemaining (the quit-path drain) does NOT call onReplenish — credits
// in flight at teardown are intentionally discarded since the client is
// disconnecting.
//
// Bandwidth gate: if bc.gate is non-nil, drain blocks on gate.Request
// before each TCP write. The wait is localized to this goroutine so
// sibling clients on the same backend are unaffected (the HOL fix).
// The inner gate-wait select observes replenishTimerCh so credit
// replenishment retries are not delayed by bandwidth pressure — see
// TestBufferedConn_GateDoesNotBlockReplenishRetry.
//
// replenishTimerCh may be nil at the moment drain enters the gate loop:
// intentional, because `consumed` cannot change while drain is
// suspended in the gate (no Write has happened since gate entry), so
// no credits are pending replenishment. A future refactor that adds
// another writer to `consumed` must re-verify this invariant.
func (bc *bufferedConn) drain() {
	defer close(bc.done)
	var consumed int64
	var lastReplenishAttempt time.Time
	var retryStreak int
	var lastWarnedStreak int

	// Lazy replenish timer: nil channel disables the select case while
	// no credits are pending. The timer is created stopped and only started
	// when consumed transitions from 0 → nonzero. This avoids the wakeup
	// storm of a perpetual ticker when many bufferedConns are mostly idle.
	var replenishTimer *time.Timer
	var replenishTimerCh <-chan time.Time
	// Hoisted gate wait timer: reused across iterations of the gate loop
	// to avoid per-iteration `time.After` allocations under sustained
	// bandwidth pressure. Canonical Go Stop-drain-Reset pattern.
	var gateWaitTimer *time.Timer
	defer func() {
		if replenishTimer != nil {
			replenishTimer.Stop()
		}
		if gateWaitTimer != nil {
			gateWaitTimer.Stop()
		}
	}()

	armReplenishTimer := func() {
		if replenishTimer == nil {
			replenishTimer = time.NewTimer(replenishTickerInterval)
			replenishTimerCh = replenishTimer.C
			return
		}
		stopAndDrainTimer(replenishTimer)
		replenishTimer.Reset(replenishTickerInterval)
		replenishTimerCh = replenishTimer.C
	}

	disarmReplenishTimer := func() {
		if replenishTimer != nil {
			stopAndDrainTimer(replenishTimer)
		}
		replenishTimerCh = nil
	}

	// resetGateTimer arms/reuses the hoisted gateWaitTimer for duration d
	// and returns its channel for selection. Lazily allocates the timer
	// on first use — drains that never block on the gate (the common
	// case when no scheduler is configured) pay zero allocation.
	resetGateTimer := func(d time.Duration) <-chan time.Time {
		if gateWaitTimer == nil {
			gateWaitTimer = time.NewTimer(d)
			return gateWaitTimer.C
		}
		stopAndDrainTimer(gateWaitTimer)
		gateWaitTimer.Reset(d)
		return gateWaitTimer.C
	}

	tryReplenish := func() {
		if bc.onReplenish == nil || consumed == 0 {
			disarmReplenishTimer()
			return
		}
		if !lastReplenishAttempt.IsZero() && time.Since(lastReplenishAttempt) < minReplenishRetryBackoff {
			// Backoff active — re-arm timer to retry after the interval.
			armReplenishTimer()
			return
		}
		lastReplenishAttempt = time.Now()
		if err := bc.onReplenish(consumed); err == nil {
			consumed = 0
			lastReplenishAttempt = time.Time{}
			retryStreak = 0
			lastWarnedStreak = 0
			disarmReplenishTimer()
		} else {
			retryStreak++
			// Log on threshold crossings (50, 100, 150, …) to surface persistent
			// failures without spamming the log on every retry.
			if retryStreak-lastWarnedStreak >= retryStreakWarnThreshold {
				log.Printf("WARN: bufferedConn replenishment has failed %d consecutive times; control plane may be wedged", retryStreak)
				lastWarnedStreak = retryStreak
			}
			// Re-arm the timer so a future tick retries even if no new
			// data arrives to drive tryReplenish from the write path.
			armReplenishTimer()
		}
	}

	for {
		select {
		case <-bc.quit:
			// Intentionally do not replenish; teardown discards in-flight credits.
			bc.flushRemaining()
			return
		case data := <-bc.writeCh:
			// Bandwidth gate wait loop. Blocks on a shared per-backend
			// deficit, but only this drain goroutine blocks — siblings on
			// the same backend run independently. Observes quit, replenish
			// retry, and the wait timer. On quit during wait, best-effort
			// writes the in-flight frame (no Refund needed — all Requests
			// during the wait returned false, no reservation held).
			//
			// gateEntry is stamped lazily on the FIRST denied Request,
			// not on every entry — frames that pass the gate on their
			// first call don't pay for a time.Now() syscall.
			if bc.gate != nil {
				var gateEntry time.Time
				blockedAtLeastOnce := false
			GateLoop:
				for {
					allowed, wait := bc.gate.Request(len(data))
					if allowed {
						break GateLoop
					}
					if !blockedAtLeastOnce {
						gateEntry = time.Now()
						blockedAtLeastOnce = true
					}
					select {
					case <-bc.quit:
						// Teardown during gate wait. No reservation held
						// (Request returned false every iteration). Best-
						// effort write the in-flight frame, then flush.
						if bc.writeTimeout > 0 {
							_ = bc.Conn.SetWriteDeadline(time.Now().Add(bc.writeTimeout))
						}
						_, _ = bc.Conn.Write(data)
						bc.flushRemaining()
						return
					case <-replenishTimerCh:
						// Credit replenishment retry must make progress
						// even while drain is bandwidth-blocked.
						bc.innerGateReplenishObserved.Add(1)
						tryReplenish()
					case <-resetGateTimer(wait):
					}
				}
				if blockedAtLeastOnce && bc.gateBlockObserver != nil {
					bc.gateBlockObserver(time.Since(gateEntry))
				}
			}

			if bc.writeTimeout > 0 {
				_ = bc.Conn.SetWriteDeadline(time.Now().Add(bc.writeTimeout))
			}
			if _, err := bc.Conn.Write(data); err != nil {
				if bc.gate != nil {
					// Record even on TCP write failure: the bytes have
					// already crossed the backend↔proxy WS link, so they
					// must count against the cap regardless of whether
					// downstream delivery succeeded. NOT refunded — see
					// the bandwidthGate doc comment.
					bc.gate.Record(len(data))
				}
				log.Printf("WARN: bufferedConn drain write failed: %v", err)
				// Signal quit so Write() returns immediately instead of
				// enqueuing data that will never be drained. Close the
				// underlying conn so the read side (Client.Start) gets an
				// error and triggers RemoveClient cleanup.
				bc.closeOnce.Do(func() { close(bc.quit) })
				_ = bc.Conn.Close()
				return
			}
			if bc.gate != nil {
				bc.gate.Record(len(data))
			}
			bc.lastWriteAt.Store(time.Now().UnixNano())
			consumed++
			// Only attempt replenishment when we cross the batch threshold.
			// Sub-batch credits stay until the next batch (or until the
			// connection closes — credits in flight at teardown are
			// intentionally discarded). The replenish timer is armed only
			// inside tryReplenish on a failed attempt.
			if consumed >= protocol.CreditReplenishBatch {
				tryReplenish()
			}
		case <-replenishTimerCh:
			tryReplenish()
		}
	}
}

// flushRemaining best-effort drains any data still in writeCh.
// Stops on the first write error or empty channel. Does not count
// toward replenishment (connection is being torn down).
func (bc *bufferedConn) flushRemaining() {
	for {
		select {
		case data := <-bc.writeCh:
			if bc.writeTimeout > 0 {
				_ = bc.Conn.SetWriteDeadline(time.Now().Add(bc.writeTimeout))
			}
			if _, err := bc.Conn.Write(data); err != nil {
				return
			}
		default:
			return
		}
	}
}

// StopWrites signals the drain goroutine to flush remaining data and exit.
// It waits briefly for drain to complete so that GracefulCloseConn's
// subsequent CloseWrite doesn't race with in-progress flush writes.
// If the drain goroutine is stuck in a blocking Write, the 50ms timeout
// lets the caller proceed with CloseWrite to unblock it.
func (bc *bufferedConn) StopWrites() {
	bc.closeOnce.Do(func() {
		close(bc.quit)
	})
	select {
	case <-bc.done:
	case <-time.After(50 * time.Millisecond):
	}
}

// Close signals quit, gives the drain goroutine a brief window to flush
// remaining data (via StopWrites), then closes the underlying connection
// to unblock any stuck drain Write, and waits for drain to exit.
// Safe to call multiple times — the second call is a no-op.
func (bc *bufferedConn) Close() error {
	bc.StopWrites() // includes 50ms flush window
	err := bc.Conn.Close()
	<-bc.done // ensure drain has fully exited
	return err
}

// Unwrap returns the underlying net.Conn, allowing GracefulCloseConn to
// reach the *net.TCPConn for TCP half-close.
func (bc *bufferedConn) Unwrap() net.Conn {
	return bc.Conn
}

// Pause delegates to the underlying connection's Pause method if available
// (e.g. PausableConn for outbound connections).
func (bc *bufferedConn) Pause() {
	if p, ok := bc.Conn.(interface{ Pause() }); ok {
		p.Pause()
	}
}

// Resume delegates to the underlying connection's Resume method if available.
func (bc *bufferedConn) Resume() {
	if p, ok := bc.Conn.(interface{ Resume() }); ok {
		p.Resume()
	}
}

// HasRecentWrite reports whether the drain goroutine has successfully written
// data to the underlying connection since the given time. Used by the idle
// timeout logic to avoid killing connections during active downloads.
func (bc *bufferedConn) HasRecentWrite(since time.Time) bool {
	lastWrite := bc.lastWriteAt.Load()
	return lastWrite > 0 && lastWrite > since.UnixNano()
}
