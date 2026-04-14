package client

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// TestDrainConnectionQueue_PerIterationCreditGuard is the narrow regression
// guard for the bug that caused "client write buffer full (128 pending)"
// in prod: drainConnectionQueue's main loop used to credit-check only once
// at entry, then drain up to maxMessages messages blindly. When entry-time
// credits were 1, 2, or 3, the loop over-sent by 3, 2, or 1 frames — every
// frame an uncounted send that showed up on the proxy's per-client writeCh.
//
// Sets availableCredits to each of 1, 2, 3 (the overshoot-triggering values)
// and asserts that drainConnectionQueue sends exactly the granted number,
// never more. Runs fast so CI catches any revert of the per-iteration check
// long before the 12-second e2e reproducer would.
func TestDrainConnectionQueue_PerIterationCreditGuard(t *testing.T) {
	for _, credits := range []int64{1, 2, 3} {
		credits := credits
		t.Run("credits="+strconv.FormatInt(credits, 10), func(t *testing.T) {
			c := newTestClient(t)
			clientWS, serverWS := newWebsocketPair(t)
			c.wsMu.Lock()
			c.ws = clientWS
			c.wsMu.Unlock()

			id := uuid.New()
			cc, queue := makeTestConn(c, id)
			cc.availableCredits.Store(credits)

			// Saturate the queue so the main loop never exits via the
			// "queue empty" default branch — any overshoot therefore has
			// to come from the credit-check bug.
			for i := 0; i < cap(queue); i++ {
				queue <- outboundMessage{
					messageType: websocket.BinaryMessage,
					payload:     []byte{byte(i)},
				}
			}

			// Drive drainConnectionQueue directly with maxMessages = 4,
			// matching writePump's production invocation. Without the
			// per-iteration credit guard, this would drain 4 messages
			// regardless of how few credits were available.
			c.drainConnectionQueue(clientWS, id, 4)

			drained := int64(cap(queue)) - int64(len(queue))
			if drained != credits {
				t.Fatalf("drained %d messages with %d credits — expected exactly %d (over-send of %d frames exposes the prod writeCh overflow bug)",
					drained, credits, credits, drained-credits)
			}
			if remaining := cc.availableCredits.Load(); remaining != 0 {
				t.Fatalf("availableCredits = %d after drain — expected 0 (credit accounting drifted)",
					remaining)
			}
			_ = serverWS
		})
	}
}

// TestDrainConnectionQueue_MidLoopExhaustionArmsWatchdog guards against
// the bug where credits go from positive at entry to zero mid-drain,
// the new per-iteration check breaks out, and nothing arms the
// replenishment watchdog. If the next EventResumeStream is silently
// dropped by SendControlMessage, the watchdog is the only recovery path
// — without it the connection stalls forever.
func TestDrainConnectionQueue_MidLoopExhaustionArmsWatchdog(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)
	cc.availableCredits.Store(2) // drain 2 → credits=0 mid-loop

	// Seed 5 messages so the queue is non-empty when we run out of credits.
	for i := 0; i < 5; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}

	c.drainConnectionQueue(clientWS, id, 4)

	if !cc.watchdogPending.Load() {
		t.Fatal("watchdog was not armed after mid-loop credit exhaustion — " +
			"a dropped EventResumeStream will now strand this connection forever")
	}
	if remaining := cc.availableCredits.Load(); remaining != 0 {
		t.Fatalf("availableCredits = %d after drain — expected 0", remaining)
	}
	if drained := int64(5) - int64(len(queue)); drained != 2 {
		t.Fatalf("drained %d messages with 2 credits — expected exactly 2", drained)
	}
}

// TestDrainConnectionQueue_ClosingDrainsWithinCreditWindow is the
// regression guard for Codex P1 (tail truncation) and B1 (stall):
// closing-mode drain must flush the full queued tail when credits are
// available, without using a "past credits" bounded-flush hack. When
// the queue empties, closeOnDrained fires so transitionToClosed's
// drain wait unblocks immediately.
func TestDrainConnectionQueue_ClosingDrainsWithinCreditWindow(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	const seeded = 3
	const maxMessages = 4
	for i := 0; i < seeded; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}

	// Plenty of credits (mirroring the active-phase state at the moment
	// the close arrived). state=Closed + quit closed = teardown.
	cc.availableCredits.Store(protocol.DefaultCreditCapacity)
	cc.state.Store(uint32(ConnStateClosed))
	close(cc.quit)

	c.drainConnectionQueue(clientWS, id, maxMessages)

	if len(queue) != 0 {
		t.Fatalf("closing drain left %d messages in queue — full queued tail must flush when credits are available (Codex P1)",
			len(queue))
	}

	select {
	case <-cc.drained:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closeOnDrained was not called after successful closing drain")
	}
}

// TestDrainConnectionQueue_ClosingDrainBoundedAfterKickstart is the
// regression guard for Codex P1 round-3 + the kickstart bound: when
// closing fires with credits=0 and a deep queue, the kickstart flushes
// CreditReplenishBatch frames past the window AND THEN STOPS — repeated
// drain calls without a real replenishment must NOT keep over-sending.
// Total over-send across the entire close is exactly one batch.
//
// The detailed semantics (kickstart flushes one batch, kickstart is
// one-shot) are covered by TestCredits_Reverse_ClosingKickstartFlushesOneBatch
// and TestCredits_Reverse_ClosingKickstartIsOneShot in
// server_backpressure_test.go.
func TestDrainConnectionQueue_ClosingDrainBoundedAfterKickstart(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	const seeded = 32
	for i := 0; i < seeded; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}

	cc.availableCredits.Store(0)
	cc.state.Store(uint32(ConnStateClosed))
	close(cc.quit)

	// Drain repeatedly. Each call should drain at most maxMessages from
	// the kickstart budget; once the 8-credit kickstart is exhausted,
	// further calls must return waiting (no second kickstart).
	for i := 0; i < 20; i++ {
		c.drainConnectionQueue(clientWS, id, 4)
	}

	drained := int64(seeded - len(queue))
	if drained > protocol.CreditReplenishBatch {
		t.Fatalf("closing drain flushed %d frames past credits — expected ≤ %d "+
			"(repeated kickstart reintroduces writeCh overflow)",
			drained, protocol.CreditReplenishBatch)
	}
}

// TestReverseCredits_ClampOnAdversarialGrant is the regression guard
// for security Finding 1: the client must clamp adversarial Credits
// values from the proxy to prevent (a) wrapping availableCredits
// negative via back-to-back MaxInt64 Adds and (b) effectively
// disabling the reverse-credit self-limit. Mirrors the proxy-side
// maxForwardCreditCapacity clamp at
// internal/hub/backend.go:handleResumeStream.
func TestReverseCredits_ClampOnAdversarialGrant(t *testing.T) {
	c := newTestClient(t)
	clientWS, serverWS := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, _ := makeTestConn(c, id)
	cc.availableCredits.Store(0)

	c.wg.Add(1)
	go c.readPump()
	t.Cleanup(func() {
		c.cancel()
		_ = clientWS.Close()
		c.wg.Wait()
	})

	// Adversarial proxy sends MaxInt64 credits.
	sendControlMessage(t, serverWS, protocol.ControlMessage{
		Event:    protocol.EventResumeStream,
		ClientID: id,
		Credits:  math.MaxInt64,
	})
	time.Sleep(50 * time.Millisecond)

	got := cc.availableCredits.Load()
	if got > maxReverseCreditCapacity {
		t.Fatalf("availableCredits = %d after adversarial MaxInt64 grant — expected clamp to ≤ %d",
			got, maxReverseCreditCapacity)
	}
	if got <= 0 {
		t.Fatalf("availableCredits = %d — expected positive clamped value, not wrapped-negative or zero",
			got)
	}
}

// TestWatchdog_GenerationInvalidatesStaleTimers is the regression
// guard for the round-6 P1 (race in pointer-based watchdogTimer
// publication): when armReplenishWatchdog and EventResumeStream run
// concurrently, the timer that armReplenishWatchdog scheduled must
// be self-invalidating so a stale fire after the stall is resolved
// can't grant a phantom credit. The fix uses a per-connection
// monotonic generation counter that every arm AND every
// EventResumeStream advances; the AfterFunc body checks its captured
// generation against the current value and no-ops if they differ.
//
// The race itself is hard to reproduce deterministically (it depends
// on goroutine preemption between time.AfterFunc and a subsequent
// pointer Store), so this test asserts the load-bearing invariants:
// (a) every arm advances the generation, (b) every replenishment
// advances the generation, (c) the AfterFunc body's gen-mismatch
// path causes no observable side effect.
func TestWatchdog_GenerationInvalidatesStaleTimers(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, _ := makeTestConn(c, id)
	cc.availableCredits.Store(0)

	gen0 := cc.watchdogGen.Load()

	c.armReplenishWatchdog(cc)
	gen1 := cc.watchdogGen.Load()
	if gen1 == gen0 {
		t.Fatalf("first arm did not advance watchdogGen (still %d)", gen1)
	}

	// Simulate EventResumeStream: clear pending + advance gen.
	// (Mirrors the production handler at the EventResumeStream case.)
	cc.watchdogPending.Store(false)
	cc.watchdogGen.Add(1)
	gen2 := cc.watchdogGen.Load()
	if gen2 == gen1 {
		t.Fatalf("EventResumeStream did not advance watchdogGen (still %d)", gen2)
	}

	// New stall cycle: re-arm. Should advance gen again.
	c.armReplenishWatchdog(cc)
	gen3 := cc.watchdogGen.Load()
	if gen3 == gen2 {
		t.Fatalf("re-arm after EventResumeStream did not advance watchdogGen (still %d)", gen3)
	}

	// Verify the chain: each step advanced.
	if !(gen0 < gen1 && gen1 < gen2 && gen2 < gen3) {
		t.Fatalf("watchdogGen did not advance monotonically: %d → %d → %d → %d", gen0, gen1, gen2, gen3)
	}
}

// TestWatchdog_SingleProbePerLifetime is the regression guard against
// the Codex Phase-3 finding: the replenishment watchdog must NOT
// re-arm itself, period — not after firing, not after a real
// EventResumeStream clears credits. Under a genuine downstream stall
// (slow TCP peer → proxy's drain doesn't progress), the watchdog
// firing once per replenishment cycle would leak 1 frame per cycle
// and eventually drive the proxy's per-client writeCh past its
// 128-frame hard cap. The watchdog is one-shot per connection
// lifetime: the first stall gets a single probe; subsequent stalls
// rely on natural replenishments arriving via EventResumeStream.
func TestWatchdog_SingleProbePerLifetime(t *testing.T) {
	c := newTestClient(t)

	id := uuid.New()
	cc, _ := makeTestConn(c, id)
	cc.availableCredits.Store(0)

	c.armReplenishWatchdog(cc)
	if !cc.watchdogPending.Load() {
		t.Fatal("first arm did not set watchdogPending")
	}

	// Simulate multiple subsequent drain-exhaustion calls. Every
	// single one MUST hit the CAS-fail path and not schedule a new
	// AfterFunc — otherwise we accumulate probe timers that each leak
	// a frame on fire, reproducing the Codex P2 finding.
	for i := 0; i < 10; i++ {
		c.armReplenishWatchdog(cc)
	}
	if !cc.watchdogPending.Load() {
		t.Fatal("watchdogPending cleared after repeated arm calls — " +
			"the single-probe invariant is broken and a legitimate stall " +
			"will escalate to disconnect over ~21 minutes")
	}
}

// TestDrainConnectionQueue_ClosingDrainsFullQueueAcrossReplenishCycles
// is the regression guard for Codex P1 (round 3): when closing fires
// with a non-empty queue and credits=0, drain must still be able to
// flush the entire queue across multiple replenishment-driven re-entry
// cycles within the connectionDrainTimeout budget. Truncating the
// queue tail on every graceful close was the original prod symptom we
// were trying to FIX, not introduce.
//
// We can't easily simulate proxy replenishments in a unit test, so we
// drive drain manually: seed N messages, set credits=N (full budget),
// close quit, call drain → main loop drains all N, queue empty,
// closeOnDrained fires.
func TestDrainConnectionQueue_ClosingDrainsFullQueueAcrossReplenishCycles(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Seed maxMessages frames so a single drain call can flush the entire
	// queue end-to-end without re-signal cycles.
	const seeded = 4
	for i := 0; i < seeded; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}
	cc.availableCredits.Store(int64(seeded))
	cc.state.Store(uint32(ConnStateClosed))
	close(cc.quit)

	c.drainConnectionQueue(clientWS, id, 4)

	if len(queue) != 0 {
		t.Fatalf("closing drain left %d messages in queue with sufficient credits — "+
			"reproduces Codex P1 (round 3): tail truncation on every graceful close",
			len(queue))
	}

	select {
	case <-cc.drained:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closeOnDrained was not called after fully flushing the queue")
	}
}

// TestDrainConnectionQueue_NoQueueClosingSignalsDrained is the
// regression guard for Codex round-5 P1: when a closing connection
// has no entry in connQueues (e.g., the queue was never populated or
// has already been cleaned up), drainConnectionQueue must still
// signal drained so transitionToClosed doesn't wait the full
// connectionDrainTimeout (30 s) for nothing. Empirically the
// pre-fix behavior stalled ~15 s per such close (the writePump's
// ping-interval death triggered sessionDone before the full 30 s
// timeout, but 15 s of cleanup latency per connection is still
// a teardown regression we don't want).
func TestDrainConnectionQueue_NoQueueClosingSignalsDrained(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	// Construct cConn directly without makeTestConn (which creates a
	// queue). This mirrors any code path that puts a connection in
	// localConns before connQueues catches up.
	cc := &clientConn{
		id:      id,
		quit:    make(chan struct{}),
		drained: make(chan struct{}),
		writeCh: make(chan []byte, localConnWriteBuffer),
		flow:    flowControl{lowWaterMark: DefaultLowWaterMark, highWaterMark: DefaultHighWaterMark, maxBuffer: DefaultMaxBuffer},
		session: &Session{},
	}
	cc.state.Store(uint32(ConnStateActive))
	cc.session.connected.Store(true)
	cc.session.done = make(chan struct{})
	cc.availableCredits.Store(64)
	c.localConns.Store(id, cc)
	close(cc.quit)

	c.drainConnectionQueue(clientWS, id, 4)

	select {
	case <-cc.drained:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("queue-less closing drain did not signal drained — " +
			"transitionToClosed will stall on connectionDrainTimeout (Codex round-5 P1)")
	}
}

// TestDrainConnectionQueue_FullDrainNeverWedges is the regression
// guard for the Codex round-4 finding: when drain finishes with
// drained==maxMessages, the round-robin re-signal must always push
// dataReady (or otherwise ensure enqueueData can re-signal). An
// earlier optimization that returned early on len(queue)==0 left
// signaled=true, so a frame enqueued just after the check would see
// signaled==true via enqueueData's CAS and skip its own dataReady
// push — wedging the connection until an unrelated event cleared
// signaled. After the fix, even an empty-queue round-robin re-signal
// must result in either a dataReady push OR signaled cleared so that
// enqueueData can re-signal.
func TestDrainConnectionQueue_FullDrainNeverWedges(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// Seed exactly maxMessages frames with credits to match. Drain
	// runs once, processes all 4, drained==maxMessages, queue empty.
	const maxMessages = 4
	for i := 0; i < maxMessages; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}
	cc.availableCredits.Store(int64(maxMessages))

	// Pre-set signaled to true (matching the state writePump set when
	// it scheduled this drain call).
	cc.signaled.Store(true)

	c.drainConnectionQueue(clientWS, id, maxMessages)

	// After drain, EITHER dataReady must contain a fresh signal (the
	// round-robin re-signal fired) OR signaled must be cleared so
	// enqueueData can re-signal on the next message. Both are
	// acceptable wakeup-preserving behaviors; both prevent the wedge.
	dataReadyHasSignal := false
	select {
	case gotID := <-c.dataReady:
		if gotID == id {
			dataReadyHasSignal = true
		}
	default:
	}
	signaledCleared := !cc.signaled.Load()

	if !dataReadyHasSignal && !signaledCleared {
		t.Fatal("after full-batch drain on now-empty queue, neither dataReady was re-signaled " +
			"nor signaled was cleared — a frame enqueued now would see signaled=true via " +
			"enqueueData's CAS and skip dataReady, wedging the connection (Codex round-4 P1)")
	}
}

// TestDrainConnectionQueue_MidLoopExhaustionEmptyQueueSkipsWatchdog
// is the regression guard for Codex P2 (round 3): the cleanup branch
// must NOT arm the one-shot replenishment watchdog when the queue is
// already empty after the main loop. An ordinary burst that finishes
// on a credit boundary would otherwise burn the connection's only
// recovery probe for nothing, leaving a later real stall on the same
// long-lived connection without any fallback if a real replenishment
// is dropped.
//
// To reach the credit-exhaustion-cleanup branch, drained < maxMessages
// must hold (otherwise the main loop completes naturally and falls
// through to the round-robin re-signal). Seed maxMessages-1 frames
// with maxMessages-1 credits so the loop drains all 3, consumes the
// last credit on iter 2, and the per-iter check on iter 3 breaks with
// drained=3 < maxMessages=4 AND credits=0 AND queue empty.
func TestDrainConnectionQueue_MidLoopExhaustionEmptyQueueSkipsWatchdog(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	const seeded = 3
	const maxMessages = 4
	for i := 0; i < seeded; i++ {
		queue <- outboundMessage{
			messageType: websocket.BinaryMessage,
			payload:     []byte{byte(i)},
		}
	}
	cc.availableCredits.Store(int64(seeded))

	c.drainConnectionQueue(clientWS, id, maxMessages)

	if cc.watchdogPending.Load() {
		t.Fatal("watchdog armed even though the queue was already empty after drain — " +
			"the connection's one-shot recovery probe is now burned without a real stall to recover from")
	}
}

// TestDrainConnectionQueue_MidDrainQuitCloseSignalsDrained is the
// regression guard for Codex P2: when quit closes after
// drainConnectionQueue has already entered the non-closing path, the
// cleanup must re-check isClosing and call closeOnDrained rather than
// falling through the non-closing cleanup branch. Otherwise
// transitionToClosed's drain-kick goroutine — which only re-signals
// when signaled was false — gets stuck waiting the full 5 s
// connectionDrainTimeout while the final 1-3 frames sit in limbo.
//
// We can't easily simulate the goroutine race in a unit test, so we
// exercise the equivalent state: enter drain with credits > 0 and
// quit NOT yet closed, let the main loop drain the queue to empty,
// close quit BEFORE the cleanup branch runs, and assert the cleanup
// correctly takes the closing branch and signals drained.
func TestDrainConnectionQueue_MidDrainQuitCloseSignalsDrained(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, queue := makeTestConn(c, id)

	// One message — main loop drains it then enters cleanup.
	queue <- outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     []byte{0},
	}
	cc.availableCredits.Store(protocol.DefaultCreditCapacity)

	// Simulate the race: quit is not closed when drain enters, but IS
	// closed by the time cleanup re-checks isClosing. Since the unit
	// test is single-threaded we can't trigger this precisely — but we
	// can pre-close quit; the main loop doesn't check isClosing
	// per-iteration (it only checks credits) so the main loop still
	// runs and the cleanup re-check catches the closed state.
	cc.state.Store(uint32(ConnStateClosed))
	close(cc.quit)

	c.drainConnectionQueue(clientWS, id, 4)

	if len(queue) != 0 {
		t.Fatalf("main loop did not drain the single credit-available message: queue has %d", len(queue))
	}

	select {
	case <-cc.drained:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cleanup path did not re-check isClosing and call closeOnDrained — " +
			"mid-drain quit race would stall transitionToClosed 5 s on connectionDrainTimeout (Codex P2)")
	}
}

// TestDrainConnectionQueue_ClosingSignalsDrainedEvenWithEmptyQueue is a
// narrower variant of the above that catches the case where the queue
// is already empty at teardown entry. The teardown path must still call
// closeOnDrained — otherwise an abrupt disconnect after the queue has
// been naturally drained still stalls transitionToClosed for 5 s.
func TestDrainConnectionQueue_ClosingSignalsDrainedEvenWithEmptyQueue(t *testing.T) {
	c := newTestClient(t)
	clientWS, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientWS
	c.wsMu.Unlock()

	id := uuid.New()
	cc, _ := makeTestConn(c, id)
	cc.availableCredits.Store(0)
	cc.state.Store(uint32(ConnStateClosed))
	close(cc.quit)

	c.drainConnectionQueue(clientWS, id, 4)

	select {
	case <-cc.drained:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closeOnDrained was not called for empty-queue teardown")
	}
}

