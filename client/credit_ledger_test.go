package client

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
)

const testMaxCap int64 = 1 << 20

func noopNotify() {}

func countingNotify() (func(), *atomic.Int64) {
	var n atomic.Int64
	return func() { n.Add(1) }, &n
}

// ---------------------------------------------------------------------
// Basic lifecycle
// ---------------------------------------------------------------------

func TestCreditLedger_TryAcquireDeductsOnce(t *testing.T) {
	l := newCreditLedger(uuid.New(), 3, testMaxCap, noopNotify)
	defer l.Close()

	for i := 0; i < 3; i++ {
		if !l.TryAcquire() {
			t.Fatalf("iter %d: TryAcquire returned false while credits remain", i)
		}
	}
	if l.TryAcquire() {
		t.Fatal("TryAcquire should return false when credits exhausted")
	}
	if got := l.Available(); got != 0 {
		t.Fatalf("Available=%d after exhaustion, want 0", got)
	}
}

func TestCreditLedger_ReleaseIncrements(t *testing.T) {
	l := newCreditLedger(uuid.New(), 2, testMaxCap, noopNotify)
	defer l.Close()

	if !l.TryAcquire() {
		t.Fatal("first TryAcquire should succeed")
	}
	l.Release()
	if got := l.Available(); got != 2 {
		t.Fatalf("Available=%d after Release, want 2", got)
	}
}

func TestCreditLedger_ReleaseClampsAtMaxCap(t *testing.T) {
	// Regression guard: TryAcquire on a full ledger, then a real
	// Replenish saturates it back to maxCap, then Release MUST NOT
	// push available past maxCap.
	l := newCreditLedger(uuid.New(), 10, 10, noopNotify)
	defer l.Close()

	if !l.TryAcquire() {
		t.Fatal("TryAcquire should succeed")
	}
	l.Replenish(1)
	if got := l.Available(); got != 10 {
		t.Fatalf("sanity: Available=%d after Replenish, want 10", got)
	}
	l.Release()
	if got := l.Available(); got > 10 {
		t.Fatalf("Release pushed Available=%d past maxCap=10", got)
	}
}

func TestCreditLedger_ReplenishClampsToMaxCap(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, 10, noopNotify)
	defer l.Close()

	l.Replenish(100)
	if got := l.Available(); got != 10 {
		t.Fatalf("Available=%d after oversized Replenish, want clamped to 10", got)
	}
	l.Replenish(5)
	if got := l.Available(); got != 10 {
		t.Fatalf("Available=%d after second Replenish, want still clamped at 10", got)
	}
}

func TestCreditLedger_ReplenishIgnoresNonPositive(t *testing.T) {
	l := newCreditLedger(uuid.New(), 5, testMaxCap, noopNotify)
	defer l.Close()

	l.Replenish(0)
	l.Replenish(-10)
	if got := l.Available(); got != 5 {
		t.Fatalf("Available=%d after noop Replenishes, want 5", got)
	}
}

// ---------------------------------------------------------------------
// Unlimited-mode-at-construction
// ---------------------------------------------------------------------

func TestCreditLedger_UnlimitedAtConstruction(t *testing.T) {
	l := newCreditLedger(uuid.New(), math.MaxInt64, testMaxCap, noopNotify)
	defer l.Close()

	if !l.UnlimitedForTest() {
		t.Fatal("expected unlimited mode")
	}
	if got := l.Available(); got != math.MaxInt64 {
		t.Fatalf("Available=%d, want MaxInt64 in unlimited mode", got)
	}
	for i := 0; i < 100; i++ {
		if !l.TryAcquire() {
			t.Fatalf("iter %d: TryAcquire should never fail in unlimited mode", i)
		}
	}
	// Release is a no-op; Available stays MaxInt64.
	l.Release()
	if got := l.Available(); got != math.MaxInt64 {
		t.Fatalf("Available=%d after Release in unlimited, want MaxInt64", got)
	}
	// Replenish is a no-op.
	l.Replenish(5)
	if got := l.Available(); got != math.MaxInt64 {
		t.Fatalf("Available=%d after Replenish in unlimited, want MaxInt64", got)
	}
}

// ---------------------------------------------------------------------
// Close semantics + I5 (kickstart-credits-spendable-after-close)
// ---------------------------------------------------------------------

func TestCreditLedger_CloseIsIdempotent(t *testing.T) {
	l := newCreditLedger(uuid.New(), 5, testMaxCap, noopNotify)
	l.Close()
	l.Close()
	l.Close()
	if !l.ClosedForTest() {
		t.Fatal("expected closed")
	}
}

func TestCreditLedger_ReplenishAcceptedAfterClose(t *testing.T) {
	// Per I5, post-Close Replenish must keep working so large-queue
	// teardowns can drain past the KickstartAndClose bonus batch.
	// Regression guard for codex round-2 P1.
	l := newCreditLedger(uuid.New(), 2, testMaxCap, noopNotify)
	l.Close()
	l.Replenish(5)
	if got := l.Available(); got != 7 {
		t.Fatalf("Available=%d after post-close Replenish, want 7 (2+5)", got)
	}
}

func TestCreditLedger_PostCloseDrainFlushesLargeQueue(t *testing.T) {
	// Scenario-level regression: a large queue enters teardown and
	// the proxy continues issuing credits. The ledger must accept
	// those credits; drain must spend them.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	// Simulate kickstart (closes ledger, grants 8).
	l.KickstartAndClose(protocol.CreditReplenishBatch)
	spent := int64(0)
	for l.TryAcquire() {
		spent++
	}
	if spent != protocol.CreditReplenishBatch {
		t.Fatalf("first batch: spent=%d, want %d", spent, protocol.CreditReplenishBatch)
	}
	// Proxy continues to issue grants post-close.
	l.Replenish(16)
	for l.TryAcquire() {
		spent++
	}
	if spent != protocol.CreditReplenishBatch+16 {
		t.Fatalf("after post-close Replenish: spent=%d, want %d",
			spent, protocol.CreditReplenishBatch+16)
	}
}

func TestCreditLedger_TryAcquireSpendsAfterClose(t *testing.T) {
	// I5: kickstart credits must be spendable after Close runs.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	l.KickstartAndClose(4)
	spent := 0
	for l.TryAcquire() {
		spent++
	}
	if spent != 4 {
		t.Fatalf("spent=%d post-KickstartAndClose(4), want 4", spent)
	}
}

// ---------------------------------------------------------------------
// Adversarial clamp
// ---------------------------------------------------------------------

func TestCreditLedger_ConstructorClampsInitialToMaxCap(t *testing.T) {
	l := newCreditLedger(uuid.New(), 999, 10, noopNotify)
	defer l.Close()
	if got := l.Available(); got != 10 {
		t.Fatalf("Available=%d, want clamped initial to 10", got)
	}
}

func TestCreditLedger_ConstructorTreatsNegativeInitialAsZero(t *testing.T) {
	l := newCreditLedger(uuid.New(), -5, testMaxCap, noopNotify)
	defer l.Close()
	if got := l.Available(); got != 0 {
		t.Fatalf("Available=%d, want 0 for negative initial", got)
	}
}

// ---------------------------------------------------------------------
// Concurrent TryAcquire
// ---------------------------------------------------------------------

func TestCreditLedger_ConcurrentTryAcquireNeverOverSpends(t *testing.T) {
	const initial int64 = 1000
	const workers = 16
	l := newCreditLedger(uuid.New(), initial, testMaxCap, noopNotify)
	defer l.Close()

	var spent atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for l.TryAcquire() {
				spent.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := spent.Load(); got != initial {
		t.Fatalf("spent=%d, want initial=%d (no over- or under-spend)", got, initial)
	}
	if got := l.Available(); got != 0 {
		t.Fatalf("Available=%d, want 0", got)
	}
}

// ---------------------------------------------------------------------
// Stall probe one-shot + generation invalidation
// ---------------------------------------------------------------------

func TestCreditLedger_StallProbeOneShotArming(t *testing.T) {
	// The stall-probe interval is 10s, too long for a unit test to
	// observe a real fire. Instead we validate the one-shot arming
	// semantics via the observable probeFired/probeGen state. Probe
	// arming is driven by NotifyEnqueue (or DrainYielded with hasMore),
	// not TryAcquire — TryAcquire cannot distinguish an empty-queue
	// drain from a credit-exhausted drain with queued data.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	l.NotifyEnqueue()
	if !l.ProbeFiredForTest() {
		t.Fatal("probeFired should be set after NotifyEnqueue on 0 credits")
	}
	gen1 := l.ProbeGenForTest()

	// Second NotifyEnqueue on empty: CAS fails, no new arm.
	l.NotifyEnqueue()
	if gen := l.ProbeGenForTest(); gen != gen1 {
		t.Fatalf("probeGen advanced on redundant arm: gen1=%d cur=%d", gen1, gen)
	}

	// Replenish clears the stall cycle.
	l.Replenish(1)
	if l.ProbeFiredForTest() {
		t.Fatal("probeFired should be cleared after Replenish")
	}
	if gen := l.ProbeGenForTest(); gen <= gen1 {
		t.Fatalf("probeGen did not advance after Replenish: gen1=%d cur=%d", gen1, gen)
	}

	// Spend the Replenish, then NotifyEnqueue arms a fresh cycle.
	if !l.TryAcquire() {
		t.Fatal("TryAcquire should succeed after Replenish")
	}
	l.NotifyEnqueue()
	if !l.ProbeFiredForTest() {
		t.Fatal("probeFired should be set for the new stall cycle")
	}
}

func TestCreditLedger_StallProbeClosedGate(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	l.Close()
	l.NotifyEnqueue()
	if l.ProbeFiredForTest() {
		t.Fatal("closed ledger should not arm a stall probe")
	}
}

func TestCreditLedger_StallProbeFalseAlarmClearsProbeFired(t *testing.T) {
	// Regression guard for codex round-4 P2#1: if a probe is armed
	// but credits arrive via a path that doesn't bump probeGen
	// (conceptually: a racing TryAcquire arms AFTER Replenish's
	// clear but sees the pre-Replenish available==0), the fire body
	// must clear probeFired itself so the next real stall can arm.
	defer func(prev time.Duration) { stallProbeInterval = prev }(stallProbeInterval)
	stallProbeInterval = 5 * time.Millisecond

	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	// Arm the probe via NotifyEnqueue on an empty ledger (mirrors the
	// production path where enqueueData pushes a frame and sees
	// credits==0).
	l.NotifyEnqueue()
	if !l.ProbeFiredForTest() {
		t.Fatal("probeFired should be set after arm")
	}
	// Simulate credits arriving via a path that doesn't bump gen
	// (racing-arm aftermath).
	l.SetAvailableForTest(5)
	// Wait for the timer to fire and detect the false alarm.
	time.Sleep(50 * time.Millisecond)
	if l.ProbeFiredForTest() {
		t.Fatal("probeFired should be cleared after false-alarm fire")
	}
	if got := l.Available(); got != 5 {
		t.Fatalf("Available=%d, want 5 (no phantom self-grant)", got)
	}
}

func TestCreditLedger_StallProbeFiresOnceAndSelfGrantsOne(t *testing.T) {
	// End-to-end: with the interval dialed to 5ms, verify the probe
	// fires exactly once and self-grants exactly 1 credit when no
	// Replenish arrives.
	defer func(prev time.Duration) { stallProbeInterval = prev }(stallProbeInterval)
	stallProbeInterval = 5 * time.Millisecond

	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 0, testMaxCap, notify)
	defer l.Close()

	// NotifyEnqueue arms the probe on credits==0 (mirrors a production
	// enqueueData push onto an empty-credit ledger).
	l.NotifyEnqueue()
	time.Sleep(50 * time.Millisecond)
	if got := l.Available(); got != 1 {
		t.Fatalf("Available=%d after probe fire, want exactly 1", got)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times, want 1", got)
	}
}


// ---------------------------------------------------------------------
// KickstartAndClose: bound + wake
// ---------------------------------------------------------------------

func TestCreditLedger_KickstartAndCloseWakesDrainer(t *testing.T) {
	notify, notifyCount := countingNotify()
	l := newCreditLedger(uuid.New(), 0, testMaxCap, notify)
	l.KickstartAndClose(protocol.CreditReplenishBatch)
	if got := notifyCount.Load(); got != 1 {
		t.Fatalf("notify fired %d times, want 1", got)
	}
	if !l.ClosedForTest() {
		t.Fatal("expected closed after KickstartAndClose")
	}
	spent := 0
	for l.TryAcquire() {
		spent++
	}
	if int64(spent) != protocol.CreditReplenishBatch {
		t.Fatalf("spent=%d post-KickstartAndClose, want %d", spent, protocol.CreditReplenishBatch)
	}
}

func TestCreditLedger_KickstartAndCloseIdempotent(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	l.KickstartAndClose(4)
	l.KickstartAndClose(100) // second call no-ops via closed.Load() gate
	if got := l.Available(); got != 4 {
		t.Fatalf("Available=%d after second KickstartAndClose, want 4", got)
	}
}

func TestCreditLedger_KickstartAndCloseUnlimitedSkipsAdd(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), math.MaxInt64, testMaxCap, notify)
	l.KickstartAndClose(4)
	if got := l.Available(); got != math.MaxInt64 {
		t.Fatalf("Available=%d, want MaxInt64 (unlimited)", got)
	}
	// Notify still fires in unlimited mode: the signaled CAS path is
	// independent of whether any credits were added. A drain woken
	// here will observe unlimited-mode TryAcquire = true and drain
	// whatever is queued.
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times in unlimited KickstartAndClose, want 1", got)
	}
}

func TestCreditLedger_KickstartAndCloseConcurrentCallsDoNotDoubleGrant(t *testing.T) {
	// I6: concurrent KickstartAndClose callers must not both grant
	// their bonus batch. Only the CAS winner adds n credits.
	for trial := 0; trial < 64; trial++ {
		l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
		var wg sync.WaitGroup
		const callers = 4
		wg.Add(callers)
		for i := 0; i < callers; i++ {
			go func() {
				defer wg.Done()
				l.KickstartAndClose(protocol.CreditReplenishBatch)
			}()
		}
		wg.Wait()

		if !l.ClosedForTest() {
			t.Fatalf("trial %d: expected closed", trial)
		}
		// Exactly one caller's batch was granted.
		if got := l.Available(); got != protocol.CreditReplenishBatch {
			t.Fatalf("trial %d: Available=%d, want exactly %d (one batch, not %d)",
				trial, got, protocol.CreditReplenishBatch, callers*int(protocol.CreditReplenishBatch))
		}
	}
}

// ---------------------------------------------------------------------
// NotifyEnqueue
// ---------------------------------------------------------------------

func TestCreditLedger_NotifyEnqueueWithCredits(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 1, testMaxCap, notify)
	defer l.Close()

	l.NotifyEnqueue()
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times, want 1 (credits>0 + signaled CAS success)", got)
	}
	if !l.SignaledForTest() {
		t.Fatal("signaled should be set after NotifyEnqueue wake")
	}
}

func TestCreditLedger_NotifyEnqueueZeroCreditsArmsProbe(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 0, testMaxCap, notify)
	defer l.Close()

	l.NotifyEnqueue()
	if got := n.Load(); got != 0 {
		t.Fatalf("notify fired %d times, want 0 (credits==0 path arms probe, no wake)", got)
	}
	if !l.ProbeFiredForTest() {
		t.Fatal("probeFired should be set after NotifyEnqueue on 0 credits")
	}
}

func TestCreditLedger_NotifyEnqueueCoalesces(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 5, testMaxCap, notify)
	defer l.Close()

	for i := 0; i < 10; i++ {
		l.NotifyEnqueue()
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times across 10 NotifyEnqueue calls, want 1 (coalescing)", got)
	}
}

func TestCreditLedger_NotifyEnqueueClosedWithCreditsWakes(t *testing.T) {
	// Regression guard for codex round-5 P1: closed ledgers with
	// spendable credits must still wake the drainer. Teardown may
	// enqueue a tail frame after a previous drain cleared signaled.
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 5, testMaxCap, notify)
	l.Close()
	l.NotifyEnqueue()
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times, want 1 (closed+credits>0 must wake)", got)
	}
}

func TestCreditLedger_NotifyEnqueueClosedNoCreditsNoArm(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 0, testMaxCap, notify)
	l.Close()
	l.NotifyEnqueue()
	if got := n.Load(); got != 0 {
		t.Fatalf("notify fired %d times on closed+empty ledger, want 0", got)
	}
	if l.ProbeFiredForTest() {
		t.Fatal("closed ledger should not arm a stall probe from NotifyEnqueue")
	}
}

// ---------------------------------------------------------------------
// DrainYielded: missed-wakeup race via post-clear callback
// ---------------------------------------------------------------------

func TestCreditLedger_DrainYieldedClearsSignaled(t *testing.T) {
	l := newCreditLedger(uuid.New(), 5, testMaxCap, noopNotify)
	defer l.Close()
	l.ForceSignaledForTest(true)
	l.DrainYielded(func() bool { return false })
	if l.SignaledForTest() {
		t.Fatal("signaled should be cleared after DrainYielded")
	}
}

func TestCreditLedger_DrainYieldedRenotifiesWhenHasMore(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 5, testMaxCap, notify)
	defer l.Close()

	// hasMore == true AND available > 0 → re-notify.
	l.DrainYielded(func() bool { return true })
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times, want 1 (hasMore + credits>0 re-notify)", got)
	}
	if !l.SignaledForTest() {
		t.Fatal("signaled should be set after re-notify")
	}
}

func TestCreditLedger_DrainYieldedNoRenotifyOnZeroCredits(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 0, testMaxCap, notify)
	defer l.Close()
	l.DrainYielded(func() bool { return true }) // hasMore=true but credits=0
	if got := n.Load(); got != 0 {
		t.Fatalf("notify fired %d times, want 0 (credits==0 suppresses re-notify)", got)
	}
}

func TestCreditLedger_DrainYieldedArmsProbeOnZeroCreditsHasMore(t *testing.T) {
	// Regression guard for codex round-2 P2: drain exits via
	// maxMessages with available==0 and queue still non-empty. The
	// stall probe MUST be armed so a dropped EventResumeStream can
	// be recovered — matches today's armReplenishWatchdog-on-exit.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	l.DrainYielded(func() bool { return true })
	if !l.ProbeFiredForTest() {
		t.Fatal("probe should be armed when drain yields with hasMore+credits==0")
	}
}

func TestCreditLedger_DrainYieldedNoArmOnClosedLedger(t *testing.T) {
	// Closed ledger + hasMore + credits==0: no probe (closed-gated);
	// connectionDrainTimeout is the deadline.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	l.Close()
	l.DrainYielded(func() bool { return true })
	if l.ProbeFiredForTest() {
		t.Fatal("closed ledger should not arm a stall probe from DrainYielded")
	}
}

// ---------------------------------------------------------------------
// InitCredits: sequential cases
// ---------------------------------------------------------------------

func TestCreditLedger_InitCreditsSequentialClampedN(t *testing.T) {
	notify, n := countingNotify()
	l := newCreditLedger(uuid.New(), 0, testMaxCap, notify)
	defer l.Close()

	l.InitCredits(32)
	if got := l.Available(); got != 32 {
		t.Fatalf("Available=%d, want 32", got)
	}
	if !l.InitializedForTest() {
		t.Fatal("initialized should be set")
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("notify fired %d times, want 1 (defensive wake)", got)
	}
}

func TestCreditLedger_InitCreditsMaxInt64SwitchesUnlimited(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	l.InitCredits(math.MaxInt64)
	if !l.UnlimitedForTest() {
		t.Fatal("expected unlimited mode after InitCredits(MaxInt64)")
	}
	if got := l.Available(); got != math.MaxInt64 {
		t.Fatalf("Available=%d, want MaxInt64", got)
	}
	// Post-init TryAcquire succeeds indefinitely.
	for i := 0; i < 100; i++ {
		if !l.TryAcquire() {
			t.Fatalf("iter %d: TryAcquire should always succeed", i)
		}
	}
}

func TestCreditLedger_InitCreditsClampsOversizedN(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, 10, noopNotify)
	defer l.Close()
	l.InitCredits(999)
	if got := l.Available(); got != 10 {
		t.Fatalf("Available=%d, want clamped to 10", got)
	}
}

func TestCreditLedger_InitCreditsNegativeTreatedAsZero(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()
	l.InitCredits(-5)
	if got := l.Available(); got != 0 {
		t.Fatalf("Available=%d, want 0 (negative n → 0)", got)
	}
	if !l.InitializedForTest() {
		t.Fatal("initialized should be set even for zero-result init")
	}
}

// ---------------------------------------------------------------------
// InitCredits double-call panic
// ---------------------------------------------------------------------

func TestCreditLedger_InitCreditsPanicsOnDoubleCall(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	l.InitCredits(16)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second InitCredits")
		}
	}()
	l.InitCredits(32) // panics
}

// ---------------------------------------------------------------------
// InitCredits composes with pre-init Replenish (C2 Add-and-clamp)
// ---------------------------------------------------------------------

func TestCreditLedger_ReplenishBeforeInitCredits(t *testing.T) {
	// Regression guard: InitCredits must add-and-clamp, not Store,
	// so a legitimately-earlier Replenish composes.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	l.Replenish(8)
	if got := l.Available(); got != 8 {
		t.Fatalf("sanity: Available=%d after Replenish, want 8", got)
	}
	l.InitCredits(32)
	if got := l.Available(); got != 40 {
		t.Fatalf("Available=%d after Replenish(8)+InitCredits(32), want 40 (compose)", got)
	}
}

func TestCreditLedger_ReplenishBeforeInitCreditsClampsCombined(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, 10, noopNotify)
	defer l.Close()

	l.Replenish(7)
	l.InitCredits(6)
	if got := l.Available(); got != 10 {
		t.Fatalf("Available=%d, want clamped to 10", got)
	}
}

// ---------------------------------------------------------------------
// InitCredits clears probe state (R1 fix)
// ---------------------------------------------------------------------

func TestCreditLedger_InitCreditsClearsProbeState(t *testing.T) {
	// A probe armed during the pending window must be cleared so the
	// first post-init exhaustion cycle can re-arm.
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	// Arm the probe during pending via NotifyEnqueue (mirrors an
	// enqueueData push landing while outboundConnect is still pending).
	l.NotifyEnqueue()
	if !l.ProbeFiredForTest() {
		t.Fatal("probeFired should be set during pending")
	}
	gen1 := l.ProbeGenForTest()

	// Init — this must clear probe state.
	l.InitCredits(2)
	if l.ProbeFiredForTest() {
		t.Fatal("probeFired should be cleared by InitCredits")
	}
	if gen := l.ProbeGenForTest(); gen <= gen1 {
		t.Fatalf("probeGen did not advance: gen1=%d cur=%d", gen1, gen)
	}

	// Spend the 2 credits, exhaust, and verify a fresh probe arms via
	// NotifyEnqueue (mirrors the production path).
	for i := 0; i < 2; i++ {
		if !l.TryAcquire() {
			t.Fatalf("iter %d: TryAcquire should succeed post-init", i)
		}
	}
	if l.TryAcquire() {
		t.Fatal("TryAcquire should fail on exhausted post-init ledger")
	}
	l.NotifyEnqueue()
	if !l.ProbeFiredForTest() {
		t.Fatal("probeFired should be set on post-init exhaustion (clearStallCycle allowed re-arm)")
	}
}

// ---------------------------------------------------------------------
// InitCredits from pending (SOCKS5 two-step) — concurrent TryAcquire
// ---------------------------------------------------------------------

func TestCreditLedger_InitCreditsFromPending(t *testing.T) {
	// Two-step SOCKS5 pattern under worker contention.
	const workers = 16
	const grant int64 = 64

	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	var spent atomic.Int64
	start := make(chan struct{})
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				if l.TryAcquire() {
					spent.Add(1)
				}
			}
		}()
	}

	close(start)
	// Small delay so workers are actively calling TryAcquire on 0.
	time.Sleep(2 * time.Millisecond)
	l.InitCredits(grant)

	// Let workers drain the grant.
	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()

	if got := spent.Load(); got != grant {
		t.Fatalf("spent=%d, want grant=%d", got, grant)
	}
	if got := l.Available(); got != 0 {
		t.Fatalf("Available=%d, want 0", got)
	}
}

// ---------------------------------------------------------------------
// InitCredits racing Close (fail-closed gate)
// ---------------------------------------------------------------------

func TestCreditLedger_InitCreditsRacingClose(t *testing.T) {
	// External Stop() race: InitCredits(MaxInt64) vs Close on the
	// same ledger. If Close wins, the ledger MUST NOT end up in
	// unlimited mode or with available>0 — Close's win means
	// InitCredits's fail-closed gate must hold.
	for trial := 0; trial < 256; trial++ {
		l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
		var initRan atomic.Bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			l.InitCredits(math.MaxInt64)
			initRan.Store(true)
		}()
		go func() {
			defer wg.Done()
			l.Close()
		}()
		wg.Wait()

		if !l.ClosedForTest() {
			t.Fatalf("trial %d: expected closed", trial)
		}
		// If the ledger is marked initialized, InitCredits won the
		// race fully (Timeline A): unlimited=true + available=MaxInt64
		// is the legitimate normal-flow outcome.
		//
		// If the ledger is NOT initialized, Close won the race
		// (Timeline B): we must see a clean terminal state — no
		// unlimited, no spendable credits.
		if !l.InitializedForTest() {
			if l.UnlimitedForTest() {
				t.Fatalf("trial %d: Close won but ledger is unlimited: %s", trial, l.Debug())
			}
			if l.Available() != 0 {
				t.Fatalf("trial %d: Close won but ledger has credits: %s", trial, l.Debug())
			}
		}
	}
}

// ---------------------------------------------------------------------
// InitCredits racing KickstartAndClose (SOCKS5 timeout race)
// ---------------------------------------------------------------------

func TestCreditLedger_SOCKS5TimeoutRace(t *testing.T) {
	// SOCKS5 timer-C race: EventOutboundResult (InitCredits) vs
	// transitionToClosed (KickstartAndClose) on the same pending
	// ledger.
	for trial := 0; trial < 64; trial++ {
		l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			l.InitCredits(math.MaxInt64)
		}()
		go func() {
			defer wg.Done()
			l.KickstartAndClose(protocol.CreditReplenishBatch)
		}()
		wg.Wait()

		if !l.ClosedForTest() {
			t.Fatalf("trial %d: expected closed", trial)
		}
		// Spend credits — must never deadlock.
		ch := make(chan struct{})
		go func() {
			for i := 0; i < 100; i++ {
				if !l.TryAcquire() {
					break
				}
			}
			close(ch)
		}()
		select {
		case <-ch:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("trial %d: drain deadlock", trial)
		}
	}
}

// ---------------------------------------------------------------------
// InitCredits → concurrent TryAcquire happens-before (race-count=100)
// ---------------------------------------------------------------------

func TestCreditLedger_InitCreditsHappensBefore(t *testing.T) {
	l := newCreditLedger(uuid.New(), 0, testMaxCap, noopNotify)
	defer l.Close()

	var spent atomic.Int64
	stop := make(chan struct{})
	workerStarted := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		close(workerStarted)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if l.TryAcquire() {
				spent.Add(1)
			}
		}
	}()
	<-workerStarted
	l.InitCredits(32)
	// Wait until the worker observes the grant. Deadline guards
	// against a race where the fix regressed.
	deadline := time.After(500 * time.Millisecond)
	for spent.Load() < 32 {
		select {
		case <-deadline:
			close(stop)
			<-workerDone
			t.Fatalf("spent=%d after 500ms, want 32 (happens-before violated?)", spent.Load())
		default:
			runtime.Gosched()
		}
	}
	close(stop)
	<-workerDone

	if got := spent.Load(); got != 32 {
		t.Fatalf("spent=%d, want 32", got)
	}
}

// ---------------------------------------------------------------------
// DrainYielded: missed-wakeup race ordering
// ---------------------------------------------------------------------

// TestCreditLedger_DrainYieldedClearsSignaledBeforeHasMore actively
// exercises the RFC §3 ordering invariant: DrainYielded MUST clear
// signaled before sampling hasMoreFn. Without this ordering, an
// enqueueData push that races between drain's queue-sample and
// signaled-clear would slip through both ends of the CAS and wedge
// the connection.
//
// The test passes a hasMoreFn that asserts signaled is already false
// when invoked. If a future refactor inverts the ordering (sample
// queue THEN clear signaled), the assertion fires from inside
// hasMoreFn and the test fails. Previously,
// TestDrainConnectionQueue_FullDrainNeverWedges passed vacuously
// against this design because DrainYielded unconditionally cleared
// signaled; strengthening that test to catch the ordering regression
// was RFC-mandated.
func TestCreditLedger_DrainYieldedClearsSignaledBeforeHasMore(t *testing.T) {
	l := newCreditLedger(uuid.New(), 4, testMaxCap, noopNotify)
	defer l.Close()

	// Pre-condition: pretend drain was scheduled (signaled=true) —
	// matches the state writePump leaves after pushing to dataReady.
	l.ForceSignaledForTest(true)

	var observedSignaled bool
	hasMoreObserved := false
	hasMore := func() bool {
		// This runs inside DrainYielded, AFTER Store(false) and
		// BEFORE any other branch. If the ordering is inverted,
		// observedSignaled is true (refactor regression).
		observedSignaled = l.SignaledForTest()
		hasMoreObserved = true
		return false // simulate empty queue → no re-notify
	}

	l.DrainYielded(hasMore)

	if !hasMoreObserved {
		t.Fatal("hasMoreFn was never called — DrainYielded short-circuited unexpectedly")
	}
	if observedSignaled {
		t.Fatal("hasMoreFn observed signaled=true — DrainYielded sampled queue BEFORE clearing signaled. " +
			"This reintroduces the missed-wakeup race the RFC §3 ordering was designed to close.")
	}
}

// TestCreditLedger_DrainYieldedRenotifiesOnHasMore is the complementary
// guard for the re-notify branch: when hasMoreFn reports work and
// credits remain, DrainYielded must re-signal the drainer so the
// caller loop re-enters drainConnectionQueue. Without this, a
// drained==maxMessages exit with queue still full would stall until
// the next producer or replenish.
func TestCreditLedger_DrainYieldedRenotifiesOnHasMore(t *testing.T) {
	notify, notifyCount := countingNotify()
	l := newCreditLedger(uuid.New(), 4, testMaxCap, notify)
	defer l.Close()

	l.DrainYielded(func() bool { return true })

	if got := notifyCount.Load(); got != 1 {
		t.Fatalf("notify fired %d times on hasMore=true + credits>0, want 1", got)
	}
	if !l.SignaledForTest() {
		t.Fatal("signaled should be re-latched after re-notify")
	}
}

// ---------------------------------------------------------------------
// Call-site audits: I6 and I8 caller-uniqueness regression guards
// ---------------------------------------------------------------------

// TestCreditLedger_KickstartAndCloseHasExactlyOneCallSite pins the
// per-lifetime one-batch bound (I6) to a single production caller.
// A refactor that adds a second KickstartAndClose call would silently
// double the over-send bound; this grep-based audit catches that
// regression at test time.
func TestCreditLedger_KickstartAndCloseHasExactlyOneCallSite(t *testing.T) {
	assertSingleCallSite(t, "KickstartAndClose")
}

// TestCreditLedger_InitCreditsHasExactlyOneCallSite pins the
// one-shot init contract (I8) to a single production caller. A second
// InitCredits call would panic at runtime, but this grep-based audit
// catches the regression earlier — at test time, before a release
// ever sees the panic.
func TestCreditLedger_InitCreditsHasExactlyOneCallSite(t *testing.T) {
	assertSingleCallSite(t, "InitCredits")
}

func assertSingleCallSite(t *testing.T, method string) {
	t.Helper()
	matches, err := grepCallSites(method)
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 production call site of .credits.%s(), got %d: %v",
			method, len(matches), matches)
	}
}

// callSitePattern matches `.credits.<method>(` — anchored to the
// ledger receiver so a hypothetical future method with a similar
// suffix (e.g., `FooInitCredits`) cannot false-match. Whitespace
// around `.credits.` permits formatter preferences without weakening
// the anchor.
func callSitePattern(method string) *regexp.Regexp {
	return regexp.MustCompile(`\.credits\.` + regexp.QuoteMeta(method) + `\(`)
}

// grepCallSites returns one match per invocation of
// `.credits.<method>(` in the package's non-test `.go` files, skipping
// leading `//` comment lines.
func grepCallSites(method string) ([]string, error) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	pattern := callSitePattern(method)
	var matches []string
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if pattern.MatchString(line) {
				matches = append(matches, path+": "+trimmed)
			}
		}
	}
	return matches, nil
}

