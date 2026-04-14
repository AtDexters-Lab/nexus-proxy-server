# RFC-002: Credit Ledger Refactor for Reverse Flow Control

**Status:** Draft (v4 — N1 fix after review round 3)
**Author:** [TBD]
**Created:** 2026-04-14
**Last Updated:** 2026-04-14 (v4 fixes drainConnectionQueue closeOnDrained early-exit gap via defer)
**Related:** Commit `3622119` (hotfix: reverse-credit over-send), Commit `b79bb35` (bandwidth gate relocation)

## Summary

Replace the decentralized reverse-credit bookkeeping in `client/client.go` with a single `creditLedger` type owned by each `clientConn`. The ledger encapsulates credit accounting, wakeup coalescing, the replenishment stall probe, and teardown semantics behind one invariant. The session-wide fanout `writePump` architecture is **preserved** — the ledger cooperates with the existing `dataReady chan uuid.UUID` channel via a caller-supplied `notify func()` hook rather than replacing it.

This is a follow-up to commit `3622119`, which hotfixed eight distinct P1 credit-accounting bugs surfaced over six rounds of gating review against the same diff. The Phase 4 root-cause synthesis concluded the eight bugs were symptoms of one design flaw: credit state is represented as six independent atomic fields (`availableCredits`, `signaled`, `watchdogPending`, `watchdogGen`, `closingKickstartDone`, `drained`) coordinated by convention across seven call sites, with no primitive whose invariant can be reasoned about locally. This RFC consolidates them into one type with one invariant.

## Problem Statement

### The three failure clusters from Phase 4 synthesis

**Cluster A — "Send a frame" is decentralized.** (P1s #1, #2, #5.) Every site that might enqueue, dequeue, or drop a frame must independently check `availableCredits`, decrement it, and set/clear `signaled`. There is no function whose job is "send one frame under credit control" — the sequence is inlined wherever it's needed. Race-loser branches, mid-loop guards, entry guards, and cleanup branches all re-implement the same 3-line protocol. Each review round caught another site where one copy was wrong.

**Cluster B — Teardown semantics are tangled with credit semantics.** (P1s #3, #4, #5, #6, #7.) `transitionToClosed` needs to wait for the queue to drain, but "queue drained" has four different definitions depending on whether the queue was empty at entry, whether credits are zero, whether the proxy has replenished since, and whether the 30s timeout has fired. The `drained` channel, the `closingKickstartDone` CAS, the queue-less fast path, and the credit-guarded mid-drain loop are four separate mechanisms expressing the same idea: "close is safe when the peer has consumed everything we intend to send."

**Cluster C — Watchdog is a parallel state machine.** (P1s #6, #8.) The replenishment stall probe adds a *third* coordination axis on top of the credit counter and teardown state. Arming, firing, being cancelled by a real replenish, and surviving a teardown race all need to be correct *and* composable with the other two axes. The generation counter fixed a pointer-publication race, but the conceptual cost remains: reasoning about "is there a probe in flight for this connection right now?" independently of credit state.

### Why the hotfix was symptomatic and patching more will fail

Every one of the eight P1s was a case of **one fragment of the credit invariant being enforced at one call site but missed at another**. The fixes work, but the invariant is still scattered across six `clientConn` fields mutated by seven code paths (`enqueueData`, `drainConnectionQueue`, `EventResumeStream` handler, `EventConnect` handler, `transitionToClosed`, `reSignalDataReady`, the `writePump` fanout). Each new code path must re-implement the invariant correctly. Each review round caught another place where one of them didn't.

The current direction of travel is: each review round finds another site where the invariant is under-enforced, and we add another state bit or another guard. At eight P1s in one diff, the approach has negative leverage — each fix increases the surface area for the next bug. This RFC collapses the six-field credit state behind a single type with one invariant; the fields still exist as implementation details but are no longer reachable or mutable from the seven scattered call sites.

## Goals

1. **Single source of truth for reverse credits.** One type, one invariant, one set of methods. Every send site calls the same small surface.
2. **Teardown uses the same primitive as steady-state.** `Close()` on the ledger is the only teardown-time action on credit state. The `closingKickstartDone` CAS disappears, replaced by a single `KickstartAndClose(n)` atomic operation on the ledger.
3. **Replenishment stall probe is owned by the ledger.** The probe is one-shot per stall cycle (cycle end = real `Replenish`), matching today's `watchdogPending` + `watchdogGen` semantics exactly, but as an internal field that no caller can accidentally miswire.
4. **Zero wire protocol change.** `EventResumeStream` / `EventConnect` / `Credits` remain exactly as today. Purely an internal refactor.
5. **Preserve every regression test's guarded invariant** in `client/drain_credit_window_test.go` and `client/server_backpressure_test.go`, and keep the E2E reproducer (`client/e2e_backpressure_test.go`) green at 16 MB / 1.6 MB/s. Test source sites that reach into the old fields are rewritten to use the new public surface; each test's *guarded bug* is still caught.
6. **No performance regression** in the `TestSourceThroughputSweep` / `TestSinkThroughputSweep` benches (build tag `bench`), ±5%.
7. **Preserve the session-wide fanout `writePump` architecture.** The ledger cooperates with `dataReady chan uuid.UUID` via an injected notify hook. No goroutine-per-connection rearchitecture.

## Non-Goals

1. **Changing forward credits** (Nexus → client, `acquireForwardCredit` + `forwardCredits` semaphore in `internal/hub/backend.go`). Out of scope.
2. **Changing the wire protocol.** `EventResumeStream.Credits`, `EventConnect.Credits`, `CreditReplenishBatch`, `DefaultCreditCapacity` all stay.
3. **Changing the bandwidth scheduler / HOL fix** from commit `b79bb35`. Orthogonal and already committed.
4. **Goroutine-per-connection writer rearchitecture.** The fanout `writePump` is preserved. A per-conn writer model was considered (see §Alternatives Considered) and rejected: its one clean win (credit `Acquire` blocks per-goroutine) is strictly dominated by the loss of a zero-mutex single-writer design, and it explodes the refactor scope beyond the credit code.
5. **Merging reverse credits and forward credits into one abstraction.** Different consumers, cross-package boundary. Revisit only if a third credit direction appears.
6. **Adding new metrics / telemetry.** Same observability surface as today (single `Debug() string` method is the only addition). Operational metrics are a follow-up.

## Detailed Design

### 1. The `creditLedger` primitive

A new file `client/credit_ledger.go` introduces:

```go
// creditLedger is the single source of truth for reverse-credit flow
// control for one clientConn. It encapsulates:
//
//   - the credit counter (how many frames may be sent without waiting)
//   - the notification coalescing bit (whether a wake-up is already
//     in flight to dataReady for this client)
//   - the replenishment stall probe (one-shot-per-stall-cycle)
//   - the close/kickstart state (teardown-time bonus batch + permanent
//     shutdown)
//
// All credit, coalescing, probe, and close state lives here. No field
// touched by this type is reachable from outside the package.
//
// Invariants:
//
//   I1. available is monotone non-negative.
//   I2. TryAcquire decrements available iff it returns true, and never
//       decrements below zero.
//   I3. Release increments available by exactly 1 and must only be
//       called after a successful TryAcquire whose send was not issued
//       (abandoned default branch) or failed (write error).
//   I4. Replenish(n) clamps n to [0, maxCap - available] before adding,
//       so available never exceeds maxCap.
//   I5. After Close, Replenish is a no-op, armStallProbe is a no-op,
//       and the stall probe is cancelled. TryAcquire CONTINUES to
//       spend any remaining credits (available > 0) until available
//       reaches 0 — kickstart credits added by KickstartAndClose must
//       be spendable after Close runs. Close is idempotent.
//   I6. KickstartAndClose(n) adds n credits then invokes Close. The
//       total over-send past proxy-issued credits is bounded at n per
//       ledger lifetime, but this bound is enforced by the CALLER
//       (transitionToClosed's state-machine CAS guarantees at most
//       one invocation of KickstartAndClose per clientConn lifetime),
//       NOT by the ledger's internal closed.Load() guard (which is a
//       Load, not a CAS, and is not self-serializing). Invoking
//       KickstartAndClose from a non-serialized site is a bug.
//   I7. The stall probe fires at most once per stall cycle. A "stall
//       cycle" starts when TryAcquire observes available==0 and ends
//       when Replenish(>0) is called by a remote peer (i.e., genuine
//       replenishment, not kickstart or self-grant). The probe's
//       self-grant advances available by 1.
type creditLedger struct {
    id uuid.UUID // the owning clientConn's ID; passed to notify on wakeup

    // available is the credit counter. Never negative.
    available atomic.Int64

    // signaled is the wake-up coalescing bit. Set when a push to
    // dataReady is already in flight for this client; cleared when the
    // drain returns with no more work or with zero credits.
    signaled atomic.Bool

    // probeFired guards the one-shot self-grant. Cleared only by a
    // real Replenish (the replenish method itself). See I7.
    probeFired atomic.Bool

    // probeGen is a monotonic generation counter that invalidates
    // stale time.AfterFunc callbacks across re-arms. See §5 for the
    // design rationale (identical to today's watchdogGen fix).
    probeGen atomic.Int64

    // closed is set once by Close/KickstartAndClose. After close,
    // TryAcquire returns false, Replenish is a no-op, and probe
    // arming is a no-op.
    closed atomic.Bool

    // unlimited short-circuits credit accounting entirely (old-server
    // mode: server sent Credits=0 on EventConnect, meaning "no reverse
    // flow control"). When true, TryAcquire always returns true and
    // never decrements; Release/Replenish are no-ops.
    unlimited bool

    // maxCap is the adversarial-clamp ceiling applied to Replenish.
    // Matches maxReverseCreditCapacity from the hotfix.
    maxCap int64

    // notify is called whenever the ledger believes a wake-up should
    // be delivered to the writePump for this client. The caller wires
    // this to a closure that pushes l.id to Client.dataReady, the same
    // as today's reSignalDataReady. The notify hook is invoked at
    // most once per coalescing cycle via signaled CAS.
    notify func()

    // probeTimer is the live time.AfterFunc, if any. Protected by
    // probeMu; nil when no probe is armed.
    probeMu    sync.Mutex
    probeTimer *time.Timer
}

// newCreditLedger constructs a ledger with initial credits, an
// adversarial clamp ceiling, and the notify hook. Pass initial =
// math.MaxInt64 to select unlimited mode (old-server compatibility).
func newCreditLedger(id uuid.UUID, initial, maxCap int64, notify func()) *creditLedger

// TryAcquire attempts to spend one credit. Returns true iff a credit
// was deducted. Never blocks. Callers must treat the credit as "spent
// on the wire" unless they call Release.
//
// IMPORTANT: TryAcquire does NOT gate on closed. A closed ledger with
// remaining credits (e.g., the 8 credits KickstartAndClose just
// added) MUST be spendable, otherwise teardown-time drain strands
// the bonus batch (v2 review blocker B5'). The closed flag blocks
// only future Replenish and armStallProbe, not existing credit
// spending.
//
// On the false path (credits == 0), TryAcquire arms the stall probe
// if the ledger is NOT closed. If closed, no probe is armed. See I7.
func (l *creditLedger) TryAcquire() bool

// Release returns one credit to the pool. Used on both the
// abandoned-send path (drain's select default branch) and the
// write-error path. Single method, two call-site meanings.
func (l *creditLedger) Release()

// Replenish adds n credits, clamped to [0, maxCap - available]. Wakes
// the drainer via the notify hook if the transition takes available
// from zero to non-zero (and signaled can be CAS-acquired). On a
// closed ledger this is a no-op. Replenish also clears probeFired,
// starting a fresh stall cycle. Called by the EventResumeStream
// handler.
func (l *creditLedger) Replenish(n int64)

// NotifyEnqueue is called by enqueueData after a frame has been
// pushed to the per-client queue. If credits > 0 and no wakeup is
// already in flight (signaled CAS), it fires the notify hook. If
// credits are 0, it arms the stall probe instead — a just-queued
// frame that can't drain is a potential strand.
func (l *creditLedger) NotifyEnqueue()

// DrainYielded is called by drainConnectionQueue at every exit
// point. It clears signaled, then (with signaled observed false)
// invokes hasMoreFn to sample the queue state AFTER the clear. If
// hasMoreFn() reports non-empty AND available > 0, it re-fires notify
// to schedule another drain pass. The callback form (not a bool
// parameter) is load-bearing: evaluating the queue-length sample
// BEFORE the signaled clear reintroduces the missed-wakeup race that
// client.go:2510-2557 handles today via a post-clear select pull
// (v2 review blocker B4'). The ledger owns the ordering guarantee.
func (l *creditLedger) DrainYielded(hasMoreFn func() bool)

// Available reports the current credit count for observability /
// tests. Not used for dispatch decisions (callers use TryAcquire).
// In unlimited mode, returns math.MaxInt64 so the
// TestCredits_Reverse_UnlimitedWhenZero assertion holds without
// modification.
func (l *creditLedger) Available() int64

// Close permanently disables TryAcquire and cancels the stall probe.
// Idempotent. Called by transitionToClosed once the drain goroutine
// has exited. Callers wait for drain to finish via the existing
// cc.drained channel, not via the ledger.
func (l *creditLedger) Close()

// KickstartAndClose grants n bonus credits and then invokes Close.
// The n credits are available to TryAcquire until spent (per I5,
// TryAcquire does not gate on closed). After KickstartAndClose,
// future Replenish is a no-op (closed gate) — so the only credits
// spendable past this point are the n bonus credits plus any that
// Replenish added before Close's store landed.
//
// CALLER OBLIGATION: MUST be invoked at most once per ledger
// lifetime. The ledger's internal closed.Load() entry guard is a
// racy Load, not a CAS, and is NOT self-serializing. Two concurrent
// callers would both observe closed==false and both add n — the
// one-batch bound would be violated. Today's sole caller is
// transitionToClosed, whose state-machine CAS (client.go:1517)
// guarantees at-most-once per clientConn lifetime. Invoking from any
// non-serialized site is a bug and must be caught by the call-site
// audit test (see §Test Migration Mapping).
//
// Bound (given caller obligation holds): over-send past
// proxy-issued credits is exactly n per ledger lifetime, plus any
// credits from a Replenish that linearized before Close's store —
// and those were real proxy grants that would have been spent
// anyway, so the effective over-send is n.
func (l *creditLedger) KickstartAndClose(n int64)

// Debug returns a one-line snapshot of ledger state for log dumps
// and panic messages. Not machine-parseable; for humans.
func (l *creditLedger) Debug() string
```

**Implementation sketch** (full code appears in §Appendix A; included here at one level of depth to ground the blocking-issue discussion):

- `TryAcquire`: fast path is `atomic.CompareAndSwap` on `available`. On success, clear `probeFired` is **not** done here — only real `Replenish` clears it. On failure (available==0), acquire `probeMu` and arm a `time.AfterFunc` guarded by `probeFired.CAS(false, true)` — identical one-shot semantics to today's `armReplenishWatchdog` at client.go:478-493.
- `Replenish`: clamp, add, clear `probeFired.Store(false)` and `probeGen.Add(1)` to invalidate any pending probe timer, then CAS `signaled` and fire `notify`. In `unlimited` mode, no-op (available is already the max sentinel).
- `NotifyEnqueue`: fast path — if `available.Load() > 0 && signaled.CAS(false, true)` then `notify()`. Slow path — if `available.Load() == 0`, arm the stall probe. The stall probe re-checks `available` at fire time and only self-grants if credits are still zero.
- `DrainYielded`: `signaled.Store(false)`. If `hasMore && available.Load() > 0 && signaled.CAS(false, true)` then `notify()` — closes the missed-wakeup race.
- `Close`: `closed.Store(true)`, cancel `probeTimer`, advance `probeGen`. Idempotent via `closed.CompareAndSwap(false, true)`.
- `KickstartAndClose`: `available.Store(available.Load() + n)` then `Close`. The add-then-close ordering is load-bearing: `Close` first would make `Replenish` a no-op and defeat the kickstart, but `KickstartAndClose` has privileged access to `available` and bypasses the closed check for its own add.

**Invariant sketch** (embedded as a doc comment in `credit_ledger.go`):

> At any moment, the number of frames the client has sent and for which the proxy has not yet issued an `EventResumeStream` credit is bounded above by `initial + Σ Replenish(n) + stall_probe_self_grants + Σ kickstart_n - currently_available`. Because `TryAcquire` deducts exactly 1 and `Release` returns exactly 1, and because `Replenish` never exceeds `maxCap`, and because stall-probe self-grants are bounded at one per stall cycle (I7), and because kickstart is bounded at exactly n per lifetime (I6 via closure), the client can never publish more frames than the proxy has credited plus a statically bounded safety margin.

### 2. `clientConn` field changes

**Before** (today, after hotfix `3622119`):

```go
type clientConn struct {
    // ... identity/session fields ...
    signaled             atomic.Bool
    availableCredits     atomic.Int64
    watchdogPending      atomic.Bool
    watchdogGen          atomic.Int64
    closingKickstartDone atomic.Bool
    drained              chan struct{}
    quit                 chan struct{}
    // ...
}
```

**After**:

```go
type clientConn struct {
    // ... identity/session fields ...
    credits *creditLedger
    drained chan struct{} // PRESERVED — teardown wait primitive
    quit    chan struct{}
    // ...
}
```

Five fields move **into** the ledger as private implementation details: `signaled`, `availableCredits`, `watchdogPending`, `watchdogGen`, `closingKickstartDone`. From outside the ledger they are unreachable and unmutable.

**`drained chan struct{}` is preserved.** The teardown wait primitive stays exactly as today: `transitionToClosed` blocks on `<-cc.drained` (or a 30s deadline) for synchronous wakeup, avoiding the 100ms-polling latency regression that reviewer B3a flagged in v1. The ledger does not own teardown-wait semantics; it owns credit semantics. These are different concerns and should remain separate.

### 3. `drainConnectionQueue` rewrite

The drain function becomes a thin wrapper over ledger operations:

```go
func (c *Client) drainConnectionQueue(ws *websocket.Conn, clientID uuid.UUID, maxMessages int) {
    queue := c.getQueue(clientID)
    if queue == nil {
        // Queue-less closing fast path preserved — teardown signals
        // drained immediately so connectionDrainTimeout doesn't stall.
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

    // Closing-mode safety net: every exit path from this function
    // MUST signal closeOnDrained when the queue is empty and we are
    // in teardown, otherwise transitionToClosed waits the full
    // connectionDrainTimeout (30s) on the common-case healthy
    // disconnect. Today's production code (client.go:2416, 2419,
    // 2483, 2486) duplicates the closeOnDrained call across every
    // early-exit branch. v3's review-3 finding N1 caught a regression
    // where the v3 sketch only called it from the trailing
    // post-loop check, missing all early-exit paths. v4 lifts the
    // check into a defer so it runs on every return path
    // unconditionally — branch-independent, refactor-resistant.
    //
    // Idempotency: closeOnDrained must be safe to call multiple
    // times (the queue==nil branch above already may have called it
    // — but we returned before this defer was registered, so no
    // double-call there. Inside the loop body the defer is the only
    // closeOnDrained call site).
    defer func() {
        if cConn.isClosing() && len(queue) == 0 {
            cConn.closeOnDrained()
        }
    }()

    // hasMore is a closure passed to DrainYielded; the ledger
    // evaluates it AFTER signaled.Store(false) so the queue-state
    // sample is stable relative to the clear. See ledger doc comment.
    hasMore := func() bool { return len(queue) > 0 }

    for i := 0; i < maxMessages; i++ {
        if !cConn.credits.TryAcquire() {
            // Credits exhausted (or ledger closed and drained).
            // Ledger has already armed the stall probe internally if
            // not closed. Defer handles closeOnDrained on return.
            cConn.credits.DrainYielded(hasMore)
            return
        }
        select {
        case msg, ok := <-queue:
            if !ok {
                cConn.credits.Release()
                cConn.credits.DrainYielded(hasMore)
                return
            }
            if err := c.writeMessage(ws, msg); err != nil {
                cConn.credits.Release()
                cConn.credits.DrainYielded(hasMore)
                c.handleWriteError(cConn, err)
                return
            }
        default:
            // Queue raced empty at this instant. Return the credit
            // and let DrainYielded re-check post-clear. Defer fires
            // closeOnDrained if we're closing and the queue (still)
            // empty — the common-case healthy disconnect path.
            cConn.credits.Release()
            cConn.credits.DrainYielded(hasMore)
            return
        }
    }
    // maxMessages exhausted. Yield; DrainYielded re-notifies if the
    // queue still has work after the clear. Defer handles
    // closeOnDrained on return.
    cConn.credits.DrainYielded(hasMore)
}
```

**What disappeared**:

- Entry credit guard and mid-loop credit guard — both are the same `TryAcquire` call, checked once per iteration.
- Race-branch credit decrement — the `default` branch calls `Release`, period; no chance to forget because there is no branch where credit is held without a matching `Release` or wire publication.
- `signaled` flag management — `DrainYielded` is one call at every exit point.
- Watchdog arming — internal to `TryAcquire`'s false path.
- Closing-mode kickstart CAS — moved to `KickstartAndClose` in §4.
- `mainLoopDone` cleanup branch — no state to reconcile.

**What survived**:

- The fundamental "check credit, pull frame, send it" loop, now with one failure mode per branch.
- The queue-less fast path at entry (`queue == nil && isClosing` → `closeOnDrained`).
- The closing-mode trailing check (`isClosing && len(queue) == 0` → `closeOnDrained`).
- The atomic select-default race-catch via `DrainYielded(len(queue) > 0)`.

### 4. Teardown: unchanged wait, new kickstart

`transitionToClosed` is almost untouched. The only change is the kickstart call site:

```go
func (c *Client) transitionToClosed(cConn *clientConn) {
    // ... state machine transition, unchanged ...

    go func() {
        // Kickstart: grant CreditReplenishBatch bonus credits then
        // close the ledger. KickstartAndClose fires the notify hook
        // internally — no separate reSignalDataReady call needed.
        // CALLER OBLIGATION: this is the SOLE call site of
        // KickstartAndClose in production. The state-machine CAS
        // above guarantees at-most-once invocation per clientConn
        // lifetime, which is the premise of the one-batch bound.
        cConn.credits.KickstartAndClose(protocol.CreditReplenishBatch)

        // Wait for drain to complete. Synchronous wakeup via the
        // existing cc.drained channel — same latency as today, no
        // 100ms polling regression.
        select {
        case <-cConn.drained:
        case <-time.After(connectionDrainTimeout):
            log.Printf("WARN: [%s] drain timeout for %s: %s",
                c.config.Name, cConn.id, cConn.credits.Debug())
        }

        // ... close TCP, localConns.Delete, etc — unchanged ...
    }()
}
```

**Why this is correct** (walking through both scenarios that round-3 review N1 flagged):

- **Large queue (queue.size > kickstart_n)**: KickstartAndClose adds 8 credits. Drain spends 4 (writePump's per-turn `maxMessages=4`), DrainYielded re-notifies (queue still has work, available > 0), writePump dispatches drain again. Drain spends the remaining 4 credits, exits via `!TryAcquire` (available now 0). DrainYielded does not re-notify (available == 0). Defer fires `closeOnDrained` only if `len(queue) == 0`; queue still has frames → defer skips. `transitionToClosed` waits up to `connectionDrainTimeout` (30s) for either a real EventResumeStream from the proxy (which would Replenish — but Close already ran, so it's a no-op) or the timeout. This is the same bounded-wait behavior as today: when the proxy can't drain its writeCh fast enough during the window, the client times out and force-closes. Acceptable.
- **Small queue (queue.size ≤ kickstart_n)** — the case round-3 review N1 caught: KickstartAndClose adds 8 credits. Drain spends 5 frames (queue had 5), iteration 6 enters the `default` branch (queue empty, available = 3). Defer fires: `isClosing && len(queue) == 0` → `closeOnDrained()`. `transitionToClosed`'s `<-cc.drained` wakes synchronously. Teardown completes in sub-millisecond latency — same as today's behavior. **The defer is load-bearing for this common-case path.**
- **Per invariant I5** (corrected in v3), `TryAcquire` does NOT gate on `closed` — it gates only on `available > 0`. The kickstart credits are spendable even after Close ran. `armStallProbe` IS suppressed when closed (the close gate is load-bearing for probe arming, just not for spending).
- **Concurrent Replenish**: `Close()` makes future `Replenish` a no-op. A `Replenish` that linearizes *before* the close store adds credits that are spendable — and they are real proxy grants that would have been spent anyway, so the effective over-send past proxy credits is bounded at exactly the kickstart batch. The bound holds because `transitionToClosed`'s state CAS guarantees at-most-one `KickstartAndClose` invocation per lifetime (see I6 caller obligation).
- `KickstartAndClose` fires the notify hook internally (per §Open Questions #1's decision), so no external `reSignalDataReady` call is needed.
- The `<-cc.drained` wait is synchronous (sub-millisecond wakeup) on the common-case path, avoiding the v1 100ms-polling regression flagged by reviewer B3.

**Test preservation**: `TestCredits_Reverse_ClosingKickstartFlushesOneBatch` and `TestCredits_Reverse_ClosingKickstartIsOneShot` pass unchanged because the invariant they guard (exactly `CreditReplenishBatch` frames flush past zero credits during teardown, never more) is enforced by `KickstartAndClose`'s closure semantics — any additional `Replenish` after `Close` is a no-op.

### 5. Stall probe: one-shot-per-cycle, owned by the ledger

The replenishment stall probe today lives in `armReplenishWatchdog` (client.go:478-493) and is cleared by `EventResumeStream` (client.go:1385). The v1 RFC tried to translate this into a deadline on `Acquire`, which reviewer B2 and red-team RF-1 both caught as reintroducing unbounded self-grants (P1 #8).

**v2 design**: The stall probe lives inside the ledger as `probeFired atomic.Bool` + `probeGen atomic.Int64` + `probeTimer *time.Timer`. Semantics are **exactly today's**, not a translation:

- `TryAcquire` returns false → internal `armStallProbe()` is called.
- `armStallProbe` CASes `probeFired` from false to true. If the CAS fails, a probe is already in flight for this stall cycle; do nothing.
- On CAS success, advance `probeGen`, capture the generation, and `time.AfterFunc(stallProbeInterval, fire)` with the generation baked into the closure.
- `fire` re-checks `probeGen.Load() == capturedGen` (avoids stale-timer race) and `available.Load() == 0` (real replenishment may have arrived). If both hold, self-grant exactly 1 credit via `available.Add(1)` and fire `notify`.
- `Replenish(n > 0)` clears `probeFired.Store(false)` and advances `probeGen` (invalidating any pending fire). This starts a fresh stall cycle.
- `Close` cancels the timer and advances `probeGen`.

**One-shot guarantee**: Within one stall cycle, `probeFired` is set on arm, set when fire succeeds, and only cleared by `Replenish(n > 0)` — which by definition ends the stall cycle. So the number of self-grants per stall cycle is bounded at exactly 1. This is the invariant `TestWatchdog_SingleProbePerLifetime` guards, and it is preserved unchanged.

**Generation counter**: carried over verbatim from the hotfix's `watchdogGen` fix. Avoids the pointer-publication race that led to P1 round 6.

**Test preservation**: `TestWatchdog_SingleProbePerLifetime` and `TestWatchdog_GenerationInvalidatesStaleTimers` are rewritten to use `ledger.Available()` and a new `ledger.ProbeFired()` test helper (exposed via `export_test.go`). The assertions — "probe fires exactly once", "stale generations do not fire" — remain identical.

### 6. writePump and `dataReady` — preserved unchanged

The session-wide fanout `writePump` stays exactly as today. Its select loop (client.go:2277-2318) selects on `c.controlSend`, `c.dataReady chan uuid.UUID`, `ticker.C`, `s.done`, `c.ctx.Done()`. **No change.**

The ledger's `notify` hook is constructed in `newCreditLedger`'s caller to push to `c.dataReady`:

```go
// In EventConnect handler (client.go:~1280):
cConn.credits = newCreditLedger(
    cConn.id,
    clampReverseCredits(c.config.Name, cConn.id, msg.Credits),
    maxReverseCreditCapacity,
    func() {
        // This closure replaces reSignalDataReady's direct push.
        // The ledger has already done the signaled CAS internally,
        // so we unconditionally push here.
        select {
        case c.dataReady <- cConn.id:
        case <-c.ctx.Done():
        }
    },
)
```

**This is Alternative B explicitly**: the ledger encapsulates the `signaled` coalescing bit, but it cooperates with the existing fanout rather than replacing it. The writePump still services clients via `dataReady <- uuid.UUID` with maxMessages=4 per-turn fairness. The ledger ensures that for each `signaled.CAS(false, true)` success the notify hook runs exactly once — identical coalescing semantics to today's direct `signaled` field manipulation, but now enforced by type boundary rather than by convention.

`reSignalDataReady` survives as a call in `transitionToClosed` (see §4) and potentially a few other teardown corners. Its internal implementation changes to delegate to the ledger:

```go
func (c *Client) reSignalDataReady(conn *clientConn) {
    // The ledger handles the CAS + notify internally.
    conn.credits.NotifyEnqueue()
}
```

**Or it can be deleted entirely**, with its sole call site in `transitionToClosed` inlined to `cConn.credits.NotifyEnqueue()`. Either is acceptable; the RFC prefers deletion for clarity — one fewer shim to reason about.

### 7. `enqueueData` simplification

**Before** (client.go:2080-2138): after pushing to queue, check `availableCredits`, decide whether to CAS `signaled` and push to `dataReady`, and arm the watchdog if credits are zero. ~60 lines of conditional logic.

**After**:

```go
func (c *Client) enqueueData(message outboundMessage) error {
    clientID, err := parseClientID(message.payload)
    if err != nil {
        return err
    }
    queue := c.getQueue(clientID)
    if queue == nil {
        return fmt.Errorf("connection not found for ClientID %s", clientID)
    }

    timer := time.NewTimer(enqueueTimeout)
    defer timer.Stop()
    for {
        select {
        case queue <- message:
            if rawConn, ok := c.localConns.Load(clientID); ok {
                if cConn, ok := rawConn.(*clientConn); ok {
                    cConn.credits.NotifyEnqueue()
                }
            }
            return nil
        case <-timer.C:
            if rawConn, ok := c.localConns.Load(clientID); ok {
                if cConn, ok := rawConn.(*clientConn); ok && !cConn.isClosing() && cConn.credits.Available() <= 0 {
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
```

`NotifyEnqueue` internally decides whether to wake the drainer (credits > 0 + signaled CAS) or arm the stall probe (credits == 0). The caller does neither.

### 8. `EventResumeStream` and `EventConnect` handler simplification

**`EventResumeStream` handler** (client.go:1367-1386):

```go
conn.credits.Replenish(clampReverseCredits(c.config.Name, msg.ClientID, msg.Credits))
```

One line. The ledger clamps (but `clampReverseCredits` still runs at the caller for the log message), replenishes, clears `probeFired`, advances `probeGen`, and fires the notify hook. The seven-field dance at the old call site disappears.

**`EventConnect` handler** (client.go:1285):

```go
initial := clampReverseCredits(c.config.Name, msg.ClientID, msg.Credits)
if msg.Credits == 0 {
    initial = math.MaxInt64 // old-server unlimited mode
}
cConn.credits = newCreditLedger(cConn.id, initial, maxReverseCreditCapacity, notifyFunc)
```

The `unlimited` short-circuit in the ledger recognizes `initial == math.MaxInt64` at construction and sets `unlimited = true` + seeds `available = math.MaxInt64`. `TestCredits_Reverse_UnlimitedWhenZero`'s literal assertion `cc.availableCredits.Load() == math.MaxInt64` translates to `cc.credits.Available() == math.MaxInt64`, which holds because `Available()` returns `math.MaxInt64` in unlimited mode.

## Migration Plan

Single-PR migration. Parity-assertion dual-write is scoped narrowly to the counter field only.

### Step-by-step

1. **Add `client/credit_ledger.go`** with the full implementation (see §Appendix A) and a comprehensive test file `client/credit_ledger_test.go`. Tests cover: basic TryAcquire/Release/Replenish/Close, unlimited mode, adversarial clamp, concurrent TryAcquire contention, close-during-drain, replenish-after-close (no-op), stall probe one-shot semantics, KickstartAndClose bound enforcement.
2. **Add the `credits *creditLedger` field to `clientConn`** alongside the old fields. Initialize in both `EventConnect` code paths (direct and via pending-connections). Do NOT remove old fields yet.
3. **Narrow parity dual-write**: every site that today mutates `availableCredits` also performs the equivalent `credits.TryAcquire` / `credits.Release` / `credits.Replenish` call. A build-tag `credit_parity` enables an assertion on `drainConnectionQueue` entry/exit that panics if `cc.availableCredits.Load() != cc.credits.Available()`. Wrap both reads in a retry-on-divergence loop (two consecutive equal reads → no panic) to tolerate non-atomic dual-write windows per red-team AR-1. Run the full regression suite + E2E reproducer under the tag.
4. **Cut over reads**: `drainConnectionQueue` reads from `cc.credits.TryAcquire()` instead of `cc.availableCredits`. Parity assertion still runs. Verify E2E reproducer stable at 5/5.
5. **Cut over remaining fields atomically**: in one commit, delete `signaled`, `availableCredits`, `watchdogPending`, `watchdogGen`, `closingKickstartDone` from `clientConn`. Delete `armReplenishWatchdog`, its call sites, and the `EventResumeStream` handler's `watchdogPending.Store(false)` + `watchdogGen.Add(1)` dance. Rewrite `enqueueData` per §7. Rewrite `EventResumeStream` handler per §8. Rewrite `drainConnectionQueue` per §3. Rewrite `transitionToClosed` kickstart per §4. Delete `reSignalDataReady` (or reduce to the shim shown in §6).
6. **Rewrite test seed/assert sites** to use the ledger surface. Mapping table (see §Test Migration below) guides each edit. Test meanings are unchanged.
7. **Delete the parity assertion build tag** and its supporting code.
8. **Run the full regression suite + E2E reproducer + bench sweep with `-race` and `-count=100` on concurrency-sensitive tests**. Any flake is a blocker.

### Test Migration Mapping

Every seed/assert site that references an old field maps to exactly one new-surface operation. The test's guarded invariant (the bug it catches) is unchanged.

| Old field reference | New-surface equivalent | Example test |
|---|---|---|
| `cc.availableCredits.Store(n)` at test setup | `cc.credits = newCreditLedger(id, n, maxReverseCreditCapacity, noopNotify)` | `drain_credit_window_test.go:37` |
| `cc.availableCredits.Store(n)` mid-test (absolute set) | `cc.credits.SetAvailableForTest(n)` via `export_test.go` | `server_backpressure_test.go:450` |
| `cc.availableCredits.Load()` | `cc.credits.Available()` | `drain_credit_window_test.go:100` |
| `cc.availableCredits.Add(n)` | `cc.credits.Replenish(n)` | `server_backpressure_test.go:429` |
| `cc.signaled.Store(true)` | `cc.credits.ForceSignaledForTest(true)` via `export_test.go` | `drain_credit_window_test.go:465`, `server_backpressure_test.go:217` |
| `cc.signaled.Store(false)` | `cc.credits.ForceSignaledForTest(false)` via `export_test.go` | `drain_credit_window_test.go:465` |
| `cc.signaled.Load()` | `cc.credits.SignaledForTest()` via `export_test.go` | `drain_credit_window_test.go:481` |
| `cc.watchdogPending.Load()` | `cc.credits.ProbeFiredForTest()` via `export_test.go` | `drain_credit_window_test.go:317` |
| `cc.watchdogPending.Store(false) + cc.watchdogGen.Add(1)` mid-test (simulates EventResumeStream side effect) | `cc.credits.ResetStallCycleForTest()` via `export_test.go` | `drain_credit_window_test.go:279-280` |
| `cc.watchdogGen.Load/Add` | `cc.credits.ProbeGenForTest()` via `export_test.go` | `drain_credit_window_test.go:323` |
| `cc.drained` (channel) | **unchanged** — stays on `clientConn` | `drain_credit_window_test.go:379` |
| `cc.closingKickstartDone.Load/CompareAndSwap` | No direct equivalent. Tests assert the kickstart bound by counting frames drained; the migration must additionally rewrite the test trigger (next row). | (consumed by next row) |
| `TestCredits_Reverse_ClosingKickstart{FlushesOneBatch,IsOneShot}` trigger pattern: `cc.availableCredits.Store(0); close(cc.quit); for { drainConnectionQueue(...) }` | The kickstart is no longer triggered by the drain function discovering closing-mode credits=0. The test must explicitly call `cc.credits.KickstartAndClose(protocol.CreditReplenishBatch)` *before* the drain loop, then call drain repeatedly and assert the same frame-count bound. The drain side never triggers a kickstart in v2 — only `transitionToClosed` does, via this explicit method call. | `server_backpressure_test.go:247-319` |
| **NEW**: call-site audit (replaces `closingKickstartDone` one-shot semantics) | A new test `TestCreditLedger_KickstartAndCloseHasExactlyOneCallSite` greps `client/*.go` (excluding `*_test.go`) for `KickstartAndClose` and asserts exactly one match. This is how I6's caller-uniqueness obligation is enforced as a regression guard against future refactors that might add a second caller. | new test |
| **NEW**: missed-wakeup race (`TestDrainConnectionQueue_FullDrainNeverWedges` strengthening) | The current test's `dataReadyHasSignal \|\| signaledCleared` assertion passes vacuously against v2 (`DrainYielded` always clears signaled). Strengthen the test to actively trigger the race window: pre-seed `signaled=true` via `ForceSignaledForTest(true)`, start drain, inject an `enqueueData`-equivalent `queue <- frame` *after* drain enters the default branch but *before* `DrainYielded` is called (use a test-only hook or `runtime.Gosched`), then assert the queue is fully drained or a re-notify fires. This is how B4's missed-wakeup race is actively guarded against, not just structurally avoided. | `drain_credit_window_test.go:442-488` |
| **NEW**: small-queue closing-mode synchronous teardown (regression guard for v3 review N1) | New test `TestDrainConnectionQueue_SmallQueueClosingSynchronousTeardown`: seed queue with 5 frames, call `cc.credits.KickstartAndClose(8)`, call `drainConnectionQueue` once, assert (a) all 5 frames written, (b) `<-cc.drained` selectable within 10ms (NOT 30s connectionDrainTimeout). This guards the §3 defer-based closeOnDrained from being accidentally dropped in a future refactor. Without the defer, the early-exit path fires `default:` → return without closeOnDrained → drained stays open → 30s teardown stall. The test fails fast (<10ms vs 30s) on regression. | new test |

The `export_test.go` file exposes `ProbeFiredForTest`, `ProbeGenForTest`, `SignaledForTest`, `ForceSignaledForTest`, `ResetStallCycleForTest`, and `SetAvailableForTest` as test-only accessors (gated by `//go:build test`). Production builds never link them. Full implementations are in §Appendix A.1.

### Rollback

Single-commit rollback via `git revert`. The wire protocol is unchanged, so mixed-version fleets (some clients on the ledger, some on the old bookkeeping) interoperate with Nexus without issue.

## Risks

| Risk | Severity | Mitigation |
|---|---|---|
| Ledger primitive has a latent bug that reproduces the original credit-leak | High | Narrow parity-assertion dual-write (step 3) on the one field where parity is meaningful. Full regression suite + E2E reproducer + `-race -count=100` on every iteration. Full code sketch in §Appendix A is reviewable end-to-end. |
| `signaled` coalescing inside the ledger has a missed-wakeup race | Medium | `DrainYielded(hasMore)` re-notifies on exit when `hasMore && available > 0`. This is a direct translation of the current client.go:2510-2557 race-catch logic. Unit tests cover the drain-yielded-with-pending-work case. |
| Stall probe one-shot guarantee silently regresses | High | Implementation is a 1:1 translation of today's `armReplenishWatchdog` + `watchdogPending` + `watchdogGen` + the `EventResumeStream` clear. `TestWatchdog_SingleProbePerLifetime` and `TestWatchdog_GenerationInvalidatesStaleTimers` preserve the invariant. Walk-through of the self-grant loop is documented in the doc comment. |
| KickstartAndClose race with concurrent Replenish | Medium | `KickstartAndClose` adds n credits then runs `Close`. A concurrent `Replenish(m)` that linearizes BEFORE `Close`'s store observes `closed == false` and adds m credits; a `Replenish` that linearizes AFTER is a no-op. **The bound that holds**: over-send past proxy-issued credits is exactly `n` per ledger lifetime. The m credits from the racing Replenish were a real proxy grant — the client would have spent them anyway, with or without the kickstart race. **Premise**: the bound holds only because `transitionToClosed`'s state-machine CAS (client.go:1517) guarantees at-most-one invocation of `KickstartAndClose` per clientConn lifetime. The ledger's internal `closed.Load()` entry guard is a Load, not a CAS, and is NOT self-serializing. The call-site audit test (see §Test Migration Mapping) regression-guards the caller-uniqueness premise. |
| Teardown-time FIN dropped by drain races | Medium | Same race-catch pattern as today via `DrainYielded(len(queue) > 0)` at every exit point. `TestDrainConnectionQueue_MidDrainQuitCloseSignalsDrained` guards this invariant. |
| Non-atomic dual-write parity assertion CI flake | Low | Parity check uses two-consecutive-equal-reads pattern, not a single snapshot. Red-team AR-1. |
| Writer-mutex contention regression | N/A | `writePump` architecture is preserved — no new mutex. |
| Team unfamiliar with ledger primitive | Low | Doc comment embeds invariant proof sketch. Full code in §Appendix A. Primitive has 9 public methods with a consistent naming convention (`TryAcquire/Release/Replenish/Close/KickstartAndClose/NotifyEnqueue/DrainYielded/Available/Debug`). |
| Scope creep into writePump rewrite | N/A | Alternative B explicitly scoped out of any writePump change. `dataReady` and the session-wide fanout stay. |

## Open Questions

1. **Should `KickstartAndClose` fire notify internally?** §4 calls `reSignalDataReady(cConn)` after `KickstartAndClose` as a belt-and-suspenders wake. Cleaner: `KickstartAndClose` internally fires notify unconditionally (like `Replenish`), and the external call goes away. Answer: **yes, have KickstartAndClose fire notify internally.** Added to the §Appendix A sketch. No caller needs the explicit call.
2. **Should `Release` and "abandoned send" be two methods?** Reviewer S2 suggested renaming. Single `Release()` is cleaner given both call sites are one-liners that mean "put the credit back." A two-method split would require callers to decide which one they are — an error surface the refactor is trying to eliminate. Answer: **one method, `Release()`, documented for both meanings.**
3. **Can `reSignalDataReady` be deleted entirely?** Yes. Its only non-teardown call site is the auto-resume timer in `handleControlMessage`, which also becomes `cConn.credits.NotifyEnqueue()`. Teardown call site becomes a direct `cConn.credits.NotifyEnqueue()`. Delete the function. Answer: **delete.**
4. **Does the ledger need a `WaitForDrain` method?** No. Teardown-wait is `<-cc.drained`, which stays on `clientConn`. The ledger owns credit state only; drain-completion state is a different concern and stays where it is.

## Alternatives Considered

### Alt 1: Adopt `golang.org/x/sync/semaphore.Weighted`

Rejected. The stdlib `Weighted` has symmetric acquire/release semantics — you always return what you acquired. Our semantics are asymmetric: credits are *spent on success*, not returned. Release-on-error is an exception path. Adapting `Weighted` would require inverting every call site and bolting on clamping, unlimited mode, close semantics, kickstart, and stall-probe — at which point we have most of our own primitive anyway. The `Weighted` primitive also has no concept of coalesced wakeup, which is exactly the `signaled` encapsulation this RFC is trying to achieve.

### Alt 2: Keep decentralized state, add a lint rule / CI check

Rejected. A comment-driven check that every `availableCredits.Add` site has a matching `signaled` update would turn a design flaw into a tooling problem. The tooling cannot catch logical errors — P1 #5's race-branch decrement would pass any plausible lint. This approach has been attempted eight times across six review rounds and has produced eight P1s.

### Alt 3: Merge forward + reverse credits into one bidirectional primitive

Rejected. The forward-credit path (hub's `acquireForwardCredit`) is already a clean semaphore in `internal/hub/backend.go`. Merging crosses a package boundary and expands blast radius. Revisit only if a third credit direction appears.

### Alt 4: Per-connection writer goroutines (replace writePump fanout with N goroutines)

Rejected. The one clean win — `ledger.Acquire(ctx)` blocks per-goroutine without starving anyone because the caller *owns* that goroutine — is strictly dominated by the loss of the current zero-mutex single-writer design. `gorilla/websocket` is not concurrent-write-safe, so N writer goroutines must serialize on a session-level mutex held across `ws.WriteMessage` (a syscall). Under load this is a strictly worse hot path: every frame pays mutex acquisition the current code does not. Control-message priority has to be re-engineered (no priority mutex exists in Go), ping cadence becomes awkward, shutdown complexity scales with N, goroutine count scales with N, and the refactor scope explodes beyond the credit code. The cosmetic win of "zero credit-related fields on clientConn" does not justify the cost.

### Alt 5: v1 RFC design (`creditSemaphore` with `Acquire(ctx, deadline)` + deletion of all fields)

Rejected. Review round 1 found five blocking issues, two of which reintroduced P1s from the hotfix (unbounded self-grant per §5 → P1 #8; missed wakeup per §6 → P1 #4). The v1 design was premised on the writePump having per-connection dispatch context, which it does not. v2 preserves the fanout and encapsulates `signaled` inside the ledger instead of deleting it.

## Acceptance Criteria

1. **Each regression test's guarded invariant is preserved.** Test source sites that reach into old fields are rewritten per the §Test Migration Mapping. No test's meaning changes — the bug each one catches must still be catchable. A diff of the test file that changes assertion semantics (not just field names) is a failure.
2. **`client/credit_ledger_test.go` passes under `-race -count=100`.** Concurrent TryAcquire, concurrent Replenish, concurrent Close, concurrent KickstartAndClose, stall-probe-under-close, all covered.
3. **`TestE2E_SlowConsumer_WriteChStaysWithinCreditWindow` remains 5/5 stable** at 16 MB / 1.6 MB/s with peak writeCh depth ≤ 64.
4. **`internal/hub/*` test suite unaffected** (no imports of `client/`'s primitive).
5. **Bench sweeps** (`bench` build tag) show ≤5% regression on source and sink throughput.
6. **`clientConn` struct** contains `credits *creditLedger` and `drained chan struct{}`, and none of: `signaled`, `availableCredits`, `watchdogPending`, `watchdogGen`, `closingKickstartDone`.
7. **`drainConnectionQueue` is ≤ 50 lines** (down from ~150).
8. **Code review loop (Phases 1–4)** finds zero P1s in the gating review. If any P1 surfaces, it becomes a blocking blocker — this RFC exists to end the P1 stream. One-P1 budget is zero.

## Appendix A: Full `credit_ledger.go` implementation sketch

```go
package client

import (
    "fmt"
    "math"
    "sync"
    "sync/atomic"
    "time"

    "github.com/google/uuid"
)

const stallProbeInterval = 10 * time.Second

// creditLedger — see package doc for full invariant description.
type creditLedger struct {
    id uuid.UUID

    available atomic.Int64
    signaled  atomic.Bool
    closed    atomic.Bool

    probeFired atomic.Bool
    probeGen   atomic.Int64
    probeMu    sync.Mutex
    probeTimer *time.Timer

    unlimited bool
    maxCap    int64
    notify    func()
}

func newCreditLedger(id uuid.UUID, initial, maxCap int64, notify func()) *creditLedger {
    l := &creditLedger{
        id:     id,
        maxCap: maxCap,
        notify: notify,
    }
    if initial == math.MaxInt64 {
        l.unlimited = true
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

// TryAcquire attempts to deduct one credit. Returns true iff deducted.
// Per I5: does NOT gate on closed — kickstart credits added by
// KickstartAndClose must be spendable after Close runs.
// On credits==0, arms the stall probe ONLY if the ledger is not
// closed (no point probing a closed ledger; the probe would only
// strand a teardown).
func (l *creditLedger) TryAcquire() bool {
    if l.unlimited {
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

// Release returns one credit. Used by abandoned-send and write-error
// paths. Per I5: closed ledgers MUST accept Release because the
// returned credit may belong to the kickstart batch and needs to be
// spendable on the next TryAcquire. Unlimited mode is a no-op
// because TryAcquire never decremented in the first place.
func (l *creditLedger) Release() {
    if l.unlimited {
        return
    }
    l.available.Add(1)
}

// Replenish adds n clamped credits. Clears probeFired (ends stall cycle).
// Fires notify if signaled CAS succeeds.
func (l *creditLedger) Replenish(n int64) {
    if l.closed.Load() || l.unlimited {
        return
    }
    if n <= 0 {
        return
    }
    if n > l.maxCap {
        n = l.maxCap
    }
    // Clamp post-add: saturate at maxCap.
    for {
        cur := l.available.Load()
        next := cur + n
        if next > l.maxCap {
            next = l.maxCap
        }
        if next < 0 { // overflow guard
            next = l.maxCap
        }
        if l.available.CompareAndSwap(cur, next) {
            break
        }
    }
    // End the stall cycle. Any pending probe timer is invalidated by
    // the generation bump; fire() will see gen mismatch and no-op.
    l.probeFired.Store(false)
    l.probeGen.Add(1)
    l.probeMu.Lock()
    if l.probeTimer != nil {
        l.probeTimer.Stop()
        l.probeTimer = nil
    }
    l.probeMu.Unlock()

    // Wake the drainer.
    if l.signaled.CompareAndSwap(false, true) {
        l.notify()
    }
}

// NotifyEnqueue is called by enqueueData after pushing a frame.
func (l *creditLedger) NotifyEnqueue() {
    if l.closed.Load() {
        return
    }
    if l.unlimited || l.available.Load() > 0 {
        if l.signaled.CompareAndSwap(false, true) {
            l.notify()
        }
        return
    }
    // Credits exhausted; just-queued frame would strand without a probe.
    l.armStallProbe()
}

// DrainYielded is called at every exit point of drainConnectionQueue.
// hasMoreFn is invoked AFTER signaled.Store(false) so the queue
// sample is stable relative to the clear, closing the missed-wakeup
// race that today's client.go:2510-2557 handles via a post-clear
// select pull (v2 review blocker B4'). The callback form is
// load-bearing — a bool parameter would re-introduce the race
// because the caller would necessarily evaluate len(queue) BEFORE
// the clear.
//
// Note: closed ledgers still need re-notify if hasMore (kickstart
// credits may still be spendable per I5), so we do NOT short-circuit
// on closed here.
func (l *creditLedger) DrainYielded(hasMoreFn func() bool) {
    l.signaled.Store(false)
    // Sample queue state AFTER clear. Any concurrent enqueueData
    // that pushed before our Store(false) but failed its own
    // signaled CAS (because we still held it) will be reflected by
    // hasMoreFn() returning true here.
    if hasMoreFn() && (l.unlimited || l.available.Load() > 0) {
        if l.signaled.CompareAndSwap(false, true) {
            l.notify()
        }
    }
}

// Available reports credit count. In unlimited mode returns MaxInt64
// so tests asserting the sentinel value continue to hold.
func (l *creditLedger) Available() int64 {
    if l.unlimited {
        return math.MaxInt64
    }
    return l.available.Load()
}

// Close permanently disables TryAcquire and cancels the stall probe.
// Idempotent.
func (l *creditLedger) Close() {
    if !l.closed.CompareAndSwap(false, true) {
        return
    }
    l.probeGen.Add(1)
    l.probeMu.Lock()
    if l.probeTimer != nil {
        l.probeTimer.Stop()
        l.probeTimer = nil
    }
    l.probeMu.Unlock()
}

// KickstartAndClose grants n bonus credits then invokes Close. Per
// I5, TryAcquire continues to spend credits after Close, so the n
// bonus credits are spendable by the drain goroutine. Per I6, the
// at-most-once guarantee is the CALLER's obligation, not the
// ledger's — the closed.Load() entry guard below is best-effort
// (catches accidental re-entry) but is NOT self-serializing under
// concurrent callers.
func (l *creditLedger) KickstartAndClose(n int64) {
    if l.closed.Load() {
        return // best-effort re-entry guard; not a serialization point
    }
    if n > 0 && !l.unlimited {
        for {
            cur := l.available.Load()
            next := cur + n
            if next < 0 || next > l.maxCap {
                next = l.maxCap
            }
            if l.available.CompareAndSwap(cur, next) {
                break
            }
        }
    }
    // Wake the drainer to spend the bonus batch. The CAS gate is
    // correct (and matches the rest of the ledger's coalescing
    // discipline): if signaled was already true, an in-flight drain
    // pass exists or is queued, and that drain will observe the new
    // credits when it runs (TryAcquire reads available, which
    // already reflects the kickstart add). The CAS path avoids
    // double-pushes to dataReady. Round-3 review caught a doc-vs-
    // code drift in the prior wording — the actual behavior is
    // CAS-gated, not unconditional.
    if l.signaled.CompareAndSwap(false, true) {
        l.notify()
    }
    l.Close()
}

// Debug returns a one-line state snapshot for log/panic messages.
func (l *creditLedger) Debug() string {
    return fmt.Sprintf("ledger{id=%s avail=%d signaled=%v probeFired=%v closed=%v unlimited=%v}",
        l.id, l.available.Load(), l.signaled.Load(),
        l.probeFired.Load(), l.closed.Load(), l.unlimited)
}

// armStallProbe is the internal one-shot-per-cycle probe. Matches
// the semantics of today's armReplenishWatchdog at client.go:478-493.
//
// One-shot guarantee: probeFired is set on arm and remains set after
// fire() self-grants. The ONLY path that clears probeFired is
// Replenish(n>0) — which by definition ends the stall cycle. So
// within one stall cycle, fire() runs at most once. Across cycles,
// the generation counter (probeGen) invalidates any stale fire body
// scheduled before Replenish bumped the gen.
func (l *creditLedger) armStallProbe() {
    if l.closed.Load() {
        return
    }
    if !l.probeFired.CompareAndSwap(false, true) {
        return // already in flight for this stall cycle
    }
    gen := l.probeGen.Add(1)
    l.probeMu.Lock()
    if l.probeTimer != nil {
        l.probeTimer.Stop()
    }
    l.probeTimer = time.AfterFunc(stallProbeInterval, func() {
        if l.closed.Load() {
            return
        }
        if l.probeGen.Load() != gen {
            return // stale probe, a newer cycle has started
        }
        if l.available.Load() > 0 {
            return // real replenishment arrived during the wait
        }
        // Self-grant exactly 1 credit. probeFired stays true — only
        // a real Replenish ends the stall cycle and unlocks future
        // probes.
        l.available.Add(1)
        if l.signaled.CompareAndSwap(false, true) {
            l.notify()
        }
    })
    l.probeMu.Unlock()
}
```

### Appendix A.1: `export_test.go` accessors

The §Test Migration Mapping references several test-only helpers that expose private ledger state. They live in `client/export_test.go` so production builds never link them:

```go
//go:build test

package client

// ProbeFiredForTest reports the probeFired bit. Used by
// TestWatchdog_SingleProbePerLifetime to assert the one-shot
// guarantee externally.
func (l *creditLedger) ProbeFiredForTest() bool {
    return l.probeFired.Load()
}

// ProbeGenForTest reports the current probe generation. Used by
// TestWatchdog_GenerationInvalidatesStaleTimers.
func (l *creditLedger) ProbeGenForTest() int64 {
    return l.probeGen.Load()
}

// SignaledForTest reports the signaled bit. Used by tests that
// previously read cc.signaled directly.
func (l *creditLedger) SignaledForTest() bool {
    return l.signaled.Load()
}

// ForceSignaledForTest sets the signaled bit. Used by tests that
// previously seeded cc.signaled.Store(true) to simulate
// "writePump has already scheduled this client".
func (l *creditLedger) ForceSignaledForTest(v bool) {
    l.signaled.Store(v)
}

// ResetStallCycleForTest mimics the side effect of Replenish(n>0)
// on stall-cycle state: clears probeFired, advances probeGen, and
// stops any pending probe timer. Used by tests that previously
// poked watchdogPending.Store(false) + watchdogGen.Add(1) directly
// to simulate post-EventResumeStream state mid-test.
func (l *creditLedger) ResetStallCycleForTest() {
    l.probeFired.Store(false)
    l.probeGen.Add(1)
    l.probeMu.Lock()
    if l.probeTimer != nil {
        l.probeTimer.Stop()
        l.probeTimer = nil
    }
    l.probeMu.Unlock()
}

// SetAvailableForTest sets available to an absolute value. Used by
// tests that previously seeded cc.availableCredits.Store(n) at a
// non-zero baseline, where the equivalence Replenish(n - cur) is
// not applicable (e.g., because cur != 0).
func (l *creditLedger) SetAvailableForTest(n int64) {
    l.available.Store(n)
}
```

**Line count**: ~180 lines. Reviewable end-to-end in one sitting. Every branch in every call site of the old code maps to exactly one ledger method.

## Review trail (v3 → v4)

**Round-3 convergent finding** (both reviewers, high confidence):

- **N1 / RF-v3-1 — `closeOnDrained` dropped from `drainConnectionQueue` early-exit paths**: the v3 §3 sketch only called `closeOnDrained` from the trailing post-loop check, missing every early-return branch (`!TryAcquire`, queue closed, write error, `default` queue-empty). Concrete repro: 5-frame queue + 8 kickstart credits → drain spends 5 → iteration 6 enters `default` → return without `closeOnDrained` → `transitionToClosed` waits 30s on `<-cc.drained`. Every healthy disconnect (the common case) would stall teardown 30s. Was latent in v2 (the v2 closed-gate made the kickstart credits unspendable, so the path was unreachable); v3's CV1 fix unmasked it.
- v4 fix: lift the `if cConn.isClosing() && len(queue) == 0 { cConn.closeOnDrained() }` check into a `defer` at the top of `drainConnectionQueue`, so it runs on every return path branch-independently. Refactor-resistant: a future maintainer who adds a new early-exit branch automatically gets the closeOnDrained signal. The §4 narrative is also updated to walk through both small-queue and large-queue cases explicitly. New regression test row added to §Test Migration Mapping (`TestDrainConnectionQueue_SmallQueueClosingSynchronousTeardown`).

**Round-3 minor findings**:

- **AR-v3-2 / S-v3-1 — `KickstartAndClose` doc comment said "fires unconditionally" but code is CAS-gated**: doc-only drift, behavior is correct. v4 rewrites the comment to accurately describe the CAS-gated path and explain why it's correct (in-flight drain pass observes the new credits via TryAcquire's read of available).
- **`signaledForTest` typo**: the §Test Migration Mapping footnote referenced a lowercase accessor name; v4 fixes to match the exported `SignaledForTest` from §Appendix A.1.
- **AR-v3-1 (Release-after-write-error spin loop on closed ledger)**: bounded by queue length, not pathological. Acknowledged as a follow-up implementation-PR concern, not a v4 blocker.
- **AR-v3-3 (probe re-arm window with Replenish)**: pre-existing race in today's code, 1:1 translated. Not introduced by v3. Tracked as an out-of-scope follow-up.

**Iteration cap note**: Per CLAUDE.md, the plan-review loop is capped at 3 iterations. v4 was applied without re-running reviewers because (a) N1 was a convergent finding from both reviewers in iteration 3 with identical reasoning, (b) the fix mechanism (`defer`) is unambiguous and ~5 lines, (c) the cap exists to prevent looping on contested or ambiguous concerns and N1 is neither, (d) both round-3 reviewers explicitly recommended applying the fix without escalation. v4 is presented to the user for sign-off.

## Review trail (v2 → v3)

**Round-2 convergent blocking findings** (both `rfc-reviewer` and `rfc-red-team` flagged the same Appendix-A bugs independently):

- **CV1 — `TryAcquire` closed-gating strands kickstart credits** (Red RF-v2-1 / Reviewer B5'). The v2 sketch had `if l.closed.Load() { return false }` at the top of `TryAcquire`, which made `KickstartAndClose`'s 8 bonus credits unreachable. v3 removes the closed-gate from `TryAcquire`; `closed` now only gates `Replenish` and `armStallProbe`. Invariant I5 rewritten. Same fix incidentally resolves the unlimited-mode teardown deadlock (Red RF-v2-2).
- **CV2 — `DrainYielded(hasMore bool)` takes a stale snapshot** (Red RF-v2-3 / Reviewer B4'). The bool parameter was sampled at the call site BEFORE `signaled.Store(false)`, reintroducing the missed-wakeup race that today's client.go:2510-2557 closes via a post-clear queue re-pull. v3 changes the signature to `DrainYielded(hasMoreFn func() bool)` and evaluates the callback INSIDE the ledger AFTER the clear. The ordering guarantee is now load-bearing on the type signature, not on caller discipline.
- **CV3 — Test Migration Mapping incomplete** (Red RF-v2-4 / Reviewer S2). Missing rows for `signaled.Store(true)`, mid-test absolute `availableCredits.Store(n)`, mid-test `watchdogPending.Store(false) + watchdogGen.Add(1)`, and the load-bearing `TestCredits_Reverse_ClosingKickstart*` rewrite. v3 adds explicit rows for each, plus `export_test.go` accessors (`ForceSignaledForTest`, `SetAvailableForTest`, `ResetStallCycleForTest`), plus a new call-site audit test to enforce the `KickstartAndClose` caller-uniqueness obligation.
- **CV4 — KickstartAndClose bound argument was imprecise** (Red AR-v2-1 / Reviewer S1). v2's Risks-table claim "total credits ≤ sum of proxy grants" didn't account for kickstart self-grants. v3 rewrites the row with the actual bound (over-send past proxy grants ≤ n per lifetime) and the actual premise (caller serialization from `transitionToClosed`'s state CAS, NOT ledger-internal CAS). I6 rewritten to document the caller obligation explicitly. KickstartAndClose's doc comment carries the same warning.

**Complementary findings addressed**:

- Reviewer S3 (`TestDrainConnectionQueue_FullDrainNeverWedges` passes vacuously): added a row in §Test Migration Mapping requiring the test be strengthened to actively trigger the race window.
- Reviewer S4 (`reSignalDataReady` call in §4 contradicted Open Question #1): removed the dead call from §4. `KickstartAndClose` fires notify internally.
- Reviewer CR2 (`probeFired` no-clear-after-self-grant under-documented): added doc comment on `armStallProbe` explaining the one-shot guarantee depends on `Replenish` being the sole clearer.
- Reviewer CR1 (unlimited mode `Release/Available` semantics): documented in `Release` doc comment.
- Reviewer S5 (enqueueData retry-loop `Available()` is racy): not addressed in v3 — the check is best-effort and matches today's behavior. Worth a comment in the implementation PR but does not need RFC-level resolution.
- Reviewer Suggestion (`TryAcquire` enum return): not adopted. The current bool-return + closed-bit-internal design works; introducing a tri-state enum would require all callers to handle three cases instead of two for limited benefit.

**Round-1 convergent blocking findings** (carried over from v1 → v2):

## Review trail (v1 → v2)

**Convergent blocking findings from round 1** (both `rfc-reviewer` and `rfc-red-team` flagged the same issues independently):

- **C1 — §5 watchdog translation reintroduces P1 #8** (Red RF-1 / Reviewer B2). v2 §5 keeps the one-shot-per-cycle semantics via `probeFired` + `probeGen`, translated 1:1 from today's code rather than replaced with an `Acquire` deadline.
- **C2 — §6 "no wake-up plumbing" was factually wrong about writePump architecture** (Red RF-3 / Reviewer B5). v2 §6 explicitly preserves the writePump fanout and uses the ledger's `notify` hook to cooperate with `dataReady`.
- **C3 — AC#1 vs AC#5 mutually exclusive** (Red AR-4 / Reviewer B1). v2 §Acceptance Criteria #1 says "each test's guarded invariant is preserved", not "tests unchanged". §Test Migration Mapping provides explicit rewrites.
- **C4 — §4 teardown had a 100ms polling regression and live race** (Red RF-4 / Reviewer B3). v2 §4 preserves `<-cc.drained` synchronous wakeup. `KickstartAndClose` atomicity prevents the mid-teardown Replenish race.
- **C5 — §3 drain couldn't express the kickstart bound** (Red RF-2 / Reviewer B6). v2 §4 replaces `closingKickstartDone` with `KickstartAndClose`'s closure-enforced bound (`Close` makes future Replenish a no-op, so no other site can add credits past kickstart).

**Complementary findings addressed**:

- Reviewer B4 (unlimited mode's `Available()` must return `math.MaxInt64`): v2 §Appendix A has `Available()` return the sentinel in unlimited mode.
- Reviewer B7 (parity dual-write only feasible for counter): v2 §Migration step 3 scopes parity to `availableCredits` ↔ `Available()` only.
- Red AR-1 (parity CI flake from non-atomic reads): v2 §Migration step 3 uses two-consecutive-equal-reads pattern.
- Reviewer S1 (`Acquire(ctx, deadline)` conflates concerns): v2 has no `Acquire`. Stall probe is internal; wakeup is via notify.
- Reviewer S2 (`ReleaseOnError` misnamed): v2 §1 has single `Release()` method.
- Reviewer Suggestion 5 (rename `creditSemaphore`): v2 renames to `creditLedger`.
- Reviewer Suggestion 4 (include full code sketch): v2 §Appendix A.

**Alternative B explicitly adopted**: v2 preserves the session-wide fanout writePump and encapsulates `signaled` inside the ledger as a private implementation detail. Both reviewers independently converged on Alternative B as the correct pivot.

## Out of scope / follow-up work

- **True per-client quantum DRR** for bandwidth gating (tracked with RFC for `b79bb35`).
- **Per-backend telemetry** for credit state.
- **Merging with forward-credit semaphore** if a third credit direction appears.
- **Per-connection writer goroutine rearchitecture** (Alt 4) if `writePump` fanout ever becomes a scaling bottleneck.
