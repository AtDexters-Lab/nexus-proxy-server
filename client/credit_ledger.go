package client

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// stallProbeInterval is a var (not const) so tests can override it
// for faster fire-observation. Production keeps the 10s interval
// inherited from the legacy armReplenishWatchdog path.
var stallProbeInterval = 10 * time.Second

// creditLedger is the single source of truth for reverse-credit flow
// control for one clientConn. See RFC-002 for design rationale.
//
// Invariants:
//
//	I1. available is monotone non-negative.
//	I2. TryAcquire decrements available iff it returns true.
//	I3. Release increments available by 1; only called after a
//	    successful TryAcquire whose send was abandoned or failed.
//	I4. Replenish(n) clamps to maxCap.
//	I5. After Close, stall-probe arming is a no-op, but Replenish
//	    continues to grant credits and TryAcquire continues to spend
//	    them. The teardown path relies on post-Close EventResumeStream
//	    to flush queues larger than the KickstartAndClose bonus batch;
//	    dropping those grants would truncate responses at 30s
//	    connectionDrainTimeout. The one-batch bound (I6) bounds
//	    over-send past proxy-issued credits — real Replenish grants
//	    are not over-send.
//	I6. KickstartAndClose is ledger-serialized via CAS on closed; at
//	    most one call grants a bonus batch per ledger lifetime.
//	I7. The stall probe fires at most once per stall cycle. A cycle
//	    starts when TryAcquire observes available==0 and ends when
//	    Replenish or InitCredits runs.
//	I8. InitCredits is one-shot, fail-closed against racing Close,
//	    and add-and-clamps so a legitimately-earlier Replenish
//	    composes.
type creditLedger struct {
	id uuid.UUID

	available atomic.Int64
	signaled  atomic.Bool

	// probeFired is cleared only by Replenish or InitCredits — the
	// two paths that end a stall cycle. That is what makes the
	// self-grant one-shot per cycle.
	probeFired atomic.Bool

	// probeGen invalidates in-flight AfterFunc bodies: the closure
	// captures gen by value and no-ops on mismatch. Eliminates the
	// timer-pointer publication race that an earlier design had.
	probeGen atomic.Int64

	closed      atomic.Bool
	initialized atomic.Bool

	// unlimited is atomic because InitCredits(MaxInt64) may flip it
	// post-publication while other goroutines read it.
	unlimited atomic.Bool

	maxCap int64
	notify func()

	// initMu serializes InitCredits's install with Close and
	// KickstartAndClose's terminal-state claim. Without it, Close can
	// interleave past InitCredits's fail-closed gate and leave the
	// ledger in a partially-installed closed state. The lock is held
	// only around the minimal state-transition critical section;
	// notify is fired outside the lock.
	initMu sync.Mutex

	probeMu    sync.Mutex
	probeTimer *time.Timer
}

// newCreditLedger constructs a ledger. Pass initial=math.MaxInt64 to
// select unlimited mode at construction. Pass initial=0 with a
// follow-up InitCredits call for the two-step SOCKS5 outbound pattern.
func newCreditLedger(id uuid.UUID, initial, maxCap int64, notify func()) *creditLedger {
	l := &creditLedger{
		id:     id,
		maxCap: maxCap,
		notify: notify,
	}
	if initial == math.MaxInt64 {
		l.unlimited.Store(true)
		l.available.Store(math.MaxInt64)
	} else {
		if initial < 0 {
			initial = 0
		}
		if initial > maxCap {
			initial = maxCap
		}
		l.available.Store(initial)
	}
	return l
}

// TryAcquire attempts to spend one credit. Never blocks. Does not
// gate on closed — per I5, kickstart credits added by
// KickstartAndClose must be spendable after Close.
func (l *creditLedger) TryAcquire() bool {
	if l.unlimited.Load() {
		return true
	}
	for {
		cur := l.available.Load()
		if cur <= 0 {
			if !l.closed.Load() {
				l.armStallProbe()
			}
			return false
		}
		if l.available.CompareAndSwap(cur, cur-1) {
			return true
		}
	}
}

// Release returns one credit. Used on the abandoned-send path and
// the write-error path. Clamps at maxCap — a Release that would
// exceed the cap (because a concurrent Replenish saturated it
// between TryAcquire and Release) is dropped, matching Replenish's
// clamp policy.
func (l *creditLedger) Release() {
	if l.unlimited.Load() {
		return
	}
	for {
		cur := l.available.Load()
		if cur >= l.maxCap {
			return
		}
		if l.available.CompareAndSwap(cur, cur+1) {
			return
		}
	}
}

// Replenish adds n clamped credits and ends the stall cycle. Accepts
// grants post-Close per I5 — the teardown path depends on continuing
// EventResumeStream grants to flush queues larger than the
// KickstartAndClose bonus batch.
//
// Ordering: clearStallCycle runs BEFORE addClamped. If we added
// credits first and the stall probe fired between the add and the
// clear, a concurrent spender could drop available back to 0 and the
// probe's one-shot self-grant would publish a phantom credit.
func (l *creditLedger) Replenish(n int64) {
	if l.unlimited.Load() {
		return
	}
	if n <= 0 {
		return
	}
	l.clearStallCycle()
	l.addClamped(n)
	if l.signaled.CompareAndSwap(false, true) {
		l.notify()
	}
}

// InitCredits installs the first credit grant for a ledger
// constructed with initial=0. At most once per ledger lifetime.
// Fail-closed on racing Close: initMu serializes the install with
// Close/KickstartAndClose, so Close cannot interleave past our gate
// and observe a partially-installed ledger.
//
// n==math.MaxInt64 switches to unlimited mode. n>0 adds-and-clamps
// so a legitimately-earlier Replenish composes.
//
// CALLER OBLIGATION: single-site invocation guarded by
// outboundPending.LoadAndDelete at the EventOutboundResult handler.
// A second invocation panics in both release and test builds.
func (l *creditLedger) InitCredits(n int64) {
	l.initMu.Lock()
	if l.closed.Load() {
		l.initMu.Unlock()
		return
	}
	if !l.initialized.CompareAndSwap(false, true) {
		l.initMu.Unlock()
		panic("creditLedger: InitCredits called twice")
	}
	// clearStallCycle before publishing credits — same phantom-grant
	// concern as Replenish. Called under initMu so a concurrent Close
	// cannot interleave; probeMu is a separate leaf lock inside.
	l.clearStallCycle()
	if n == math.MaxInt64 {
		l.unlimited.Store(true)
		l.available.Store(math.MaxInt64)
	} else {
		if n < 0 {
			n = 0
		}
		l.addClamped(n)
	}
	l.initMu.Unlock()
	if l.signaled.CompareAndSwap(false, true) {
		l.notify()
	}
}

// NotifyEnqueue is called by enqueueData after pushing a frame.
// Wakes the drainer if credits are available; arms the stall probe
// otherwise.
//
// Post-Close: wake still fires if credits remain (kickstart or
// continuing Replenish grants, per I5). Stall-probe arming is
// closed-gated — post-close relies on connectionDrainTimeout as the
// deadline instead.
func (l *creditLedger) NotifyEnqueue() {
	if l.unlimited.Load() || l.available.Load() > 0 {
		if l.signaled.CompareAndSwap(false, true) {
			l.notify()
		}
		return
	}
	if l.closed.Load() {
		return
	}
	l.armStallProbe()
}

// DrainYielded is called at every exit point of drainConnectionQueue.
// The hasMoreFn callback (not a bool parameter) is load-bearing:
// evaluating queue-length BEFORE signaled.Store(false) would
// reintroduce the missed-wakeup race that this function exists to
// close.
//
// Three exit cases when hasMore:
//   - credits > 0 (or unlimited): re-notify the drainer
//   - credits == 0, not closed: arm the stall probe so a dropped
//     EventResumeStream is recovered. This matches today's drain-
//     exit-with-credits-exhausted behavior, where armReplenishWatchdog
//     is called at every such site.
//   - credits == 0, closed: no probe (closed-gated internally);
//     connectionDrainTimeout provides the teardown deadline.
func (l *creditLedger) DrainYielded(hasMoreFn func() bool) {
	l.signaled.Store(false)
	if !hasMoreFn() {
		return
	}
	if l.unlimited.Load() || l.available.Load() > 0 {
		if l.signaled.CompareAndSwap(false, true) {
			l.notify()
		}
		return
	}
	l.armStallProbe()
}

// Available reports credit count. Returns MaxInt64 in unlimited mode
// so sentinel-value assertions hold.
func (l *creditLedger) Available() int64 {
	if l.unlimited.Load() {
		return math.MaxInt64
	}
	return l.available.Load()
}

// Close permanently disables future Replenish and probe arming.
// Idempotent. TryAcquire continues to spend any remaining credits
// (I5). The initMu hold during the closed CAS serializes against
// an in-flight InitCredits — only one of {Close, InitCredits}
// observes a non-terminal ledger.
func (l *creditLedger) Close() {
	l.initMu.Lock()
	won := l.closed.CompareAndSwap(false, true)
	l.initMu.Unlock()
	if !won {
		return
	}
	l.clearStallCycle()
}

// KickstartAndClose grants n bonus credits and marks the ledger
// closed. Ledger-serialized via the closed CAS under initMu — a
// racing InitCredits cannot interleave past our gate.
//
// clearStallCycle runs BEFORE addClamped (same phantom-grant concern
// as Replenish).
func (l *creditLedger) KickstartAndClose(n int64) {
	l.initMu.Lock()
	won := l.closed.CompareAndSwap(false, true)
	l.initMu.Unlock()
	if !won {
		return
	}
	l.clearStallCycle()
	if n > 0 && !l.unlimited.Load() {
		l.addClamped(n)
	}
	if l.signaled.CompareAndSwap(false, true) {
		l.notify()
	}
}

// Debug returns a one-line state snapshot for log/panic messages.
func (l *creditLedger) Debug() string {
	return fmt.Sprintf("ledger{id=%s avail=%d signaled=%v probeFired=%v closed=%v unlimited=%v initialized=%v}",
		l.id, l.available.Load(), l.signaled.Load(),
		l.probeFired.Load(), l.closed.Load(), l.unlimited.Load(), l.initialized.Load())
}

// addClamped atomically adds n to available, saturating at maxCap.
// Shared by Replenish, InitCredits, and KickstartAndClose. Caller is
// responsible for gating on closed/unlimited.
func (l *creditLedger) addClamped(n int64) {
	if n <= 0 {
		return
	}
	if n > l.maxCap {
		n = l.maxCap
	}
	for {
		cur := l.available.Load()
		next := cur + n
		if next > l.maxCap || next < 0 {
			next = l.maxCap
		}
		if l.available.CompareAndSwap(cur, next) {
			return
		}
	}
}

// armStallProbe schedules a one-shot self-grant probe. The one-shot
// guarantee holds because probeFired is set on arm and cleared by
// Replenish/InitCredits (stall-cycle end) or by the fire body itself
// when it detects a false alarm (credits arrived while the timer
// slept). Across cycles, probeGen invalidates stale fire bodies.
//
// The arm sequence runs under probeMu, serializing with
// clearStallCycle: a clear cannot interleave between the CAS and the
// timer install. The available re-check under the lock skips arming
// when credits arrived while we waited for the lock (common with
// concurrent Replenish).
//
// The fire body also takes probeMu so its check-and-add is atomic
// with clearStallCycle's gen bump.
func (l *creditLedger) armStallProbe() {
	if l.closed.Load() {
		return
	}
	l.probeMu.Lock()
	if l.closed.Load() {
		l.probeMu.Unlock()
		return
	}
	if l.available.Load() > 0 {
		l.probeMu.Unlock()
		return
	}
	if !l.probeFired.CompareAndSwap(false, true) {
		l.probeMu.Unlock()
		return
	}
	gen := l.probeGen.Add(1)
	if l.probeTimer != nil {
		l.probeTimer.Stop()
	}
	l.probeTimer = time.AfterFunc(stallProbeInterval, func() {
		var notifyCaller bool
		l.probeMu.Lock()
		switch {
		case l.closed.Load() || l.probeGen.Load() != gen:
			// Stale — newer cycle or Close owns probeFired.
		case l.available.Load() > 0:
			// False alarm — credits arrived while the timer slept.
			// Clear probeFired so the next stall cycle can re-arm.
			l.probeFired.Store(false)
		default:
			l.available.Add(1)
			notifyCaller = l.signaled.CompareAndSwap(false, true)
		}
		l.probeMu.Unlock()
		if notifyCaller {
			l.notify()
		}
	})
	l.probeMu.Unlock()
}

// clearStallCycle ends the current stall cycle. Shared by Replenish,
// InitCredits, KickstartAndClose, and Close. The probeMu hold is
// load-bearing: it excludes the fire body's check-and-add, which
// would otherwise leak a phantom credit if it interleaved with the
// gen bump here.
func (l *creditLedger) clearStallCycle() {
	l.probeMu.Lock()
	l.probeFired.Store(false)
	l.probeGen.Add(1)
	if l.probeTimer != nil {
		l.probeTimer.Stop()
		l.probeTimer = nil
	}
	l.probeMu.Unlock()
}
