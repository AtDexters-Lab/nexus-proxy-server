package hub

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/stretchr/testify/require"
)

// writeWithBackpressure retries Write until it succeeds or the deadline
// elapses. Used by tests that push more frames than the writeCh can hold;
// the natural backpressure (buffer-full error) is not the failure mode
// under test, so we wait for drain to make room.
func writeWithBackpressure(t *testing.T, bc *bufferedConn, data []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := bc.Write(data); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("writeWithBackpressure: deadline exceeded waiting for drain")
}

// TestBufferedConn_DrainWriteError_SignalsQuit verifies that when the drain
// goroutine encounters a write error (e.g. remote end closed), it closes
// the quit channel and the underlying connection. Without this, subsequent
// Write() calls would block for writeEnqueueTimeout with no active drainer,
// stalling the readPump for up to 1 second per message.
func TestBufferedConn_DrainWriteError_SignalsQuit(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	bc := newBufferedConn(s, 5*time.Second, nil, nil, nil)

	// Close the remote end so the drain goroutine's Write fails.
	c.Close()

	// First write: drain goroutine will attempt to write, get an error,
	// close quit, and close the underlying connection.
	bc.Write([]byte("trigger_drain_error"))

	// Wait for drain goroutine to process the message and fail.
	select {
	case <-bc.done:
		// Drain goroutine exited — good.
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine did not exit after write error")
	}

	// Subsequent Write must return immediately (not block for 1s).
	start := time.Now()
	_, err := bc.Write([]byte("should_fail_fast"))
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Equal(t, net.ErrClosed, err)
	require.Less(t, elapsed, 100*time.Millisecond,
		"Write blocked for %v after drain goroutine died — quit channel was not closed", elapsed)
}

// TestBufferedConn_FlushOnClose verifies that Close() flushes remaining
// buffered data before closing the underlying connection. This prevents
// data loss when EventDisconnect arrives shortly after a data message.
func TestBufferedConn_FlushOnClose(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	bc := newBufferedConn(s, 5*time.Second, nil, nil, nil)

	// Enqueue data that the drain goroutine hasn't written yet.
	// net.Pipe is synchronous, so drain blocks on Write until we read.
	marker := []byte("MUST_BE_DELIVERED")
	bc.Write(marker)

	// Read the data from the remote end in a goroutine.
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := c.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			received <- data
		}
		close(received)
	}()

	// Close the bufferedConn — this should flush the marker before closing.
	bc.Close()

	// Verify the marker was delivered.
	select {
	case data := <-received:
		require.Equal(t, string(marker), string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("data was not flushed before close — drain-on-quit is broken")
	}

	c.Close()
}

// TestBufferedConn_ReplenishCallback verifies that the onReplenish callback
// fires after every CreditReplenishBatch (8) successful TCP writes.
func TestBufferedConn_ReplenishCallback(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()

	replenished := make(chan int64, 10)
	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		replenished <- n
		return nil
	}, nil, nil)

	// Fill the buffer with CreditReplenishBatch messages.
	// Drain goroutine blocks on first write (net.Pipe, nobody reading).
	for i := int64(0); i < protocol.CreditReplenishBatch; i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err)
	}

	// Start draining so the drain goroutine can write.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Verify onReplenish was called with the batch count.
	select {
	case n := <-replenished:
		require.Equal(t, protocol.CreditReplenishBatch, n)
	case <-time.After(3 * time.Second):
		t.Fatal("onReplenish was not called after CreditReplenishBatch drains")
	}

	bc.Close()
	c.Close()
}

// TestBufferedConn_PreservesCreditsOnReplenishFailure is the regression test
// for the silent credit-loss bug. The callback errors for the first 3
// invocations and then succeeds. We drain 32 frames (4× the batch threshold)
// and assert that no credits are lost — the eventual successful invocations
// receive the accumulated count, not just CreditReplenishBatch.
func TestBufferedConn_PreservesCreditsOnReplenishFailure(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	var totalDelivered atomic.Int64
	var failuresLeft atomic.Int32
	failuresLeft.Store(3)

	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		if failuresLeft.Add(-1) >= 0 {
			return errors.New("simulated control lane full")
		}
		totalDelivered.Add(n)
		return nil
	}, nil, nil)
	defer bc.Close()

	// Drain side: read everything as fast as possible.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Write 32 frames.
	const totalFrames = 32
	for i := 0; i < totalFrames; i++ {
		writeWithBackpressure(t, bc, []byte("x"))
	}

	// Wait for the ticker to flush all retries.
	require.Eventually(t, func() bool {
		return totalDelivered.Load() == totalFrames
	}, 3*time.Second, 25*time.Millisecond,
		"credits lost: delivered %d of %d", totalDelivered.Load(), totalFrames)
}

// TestBufferedConn_TimerRetriesFailedBatch verifies that after a batch
// replenishment fails (callback returns error), the lazy retry timer
// re-fires and retries until success. This is the ONLY purpose of the
// retry timer — sub-batch credits are intentionally NOT flushed
// opportunistically (would create a control-plane storm at fan-out).
func TestBufferedConn_TimerRetriesFailedBatch(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	var attempts atomic.Int64
	var firstSuccessAt atomic.Int64
	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		attempt := attempts.Add(1)
		if attempt < 3 {
			return errors.New("simulated control lane full")
		}
		firstSuccessAt.Store(attempt)
		return nil
	}, nil, nil)
	defer bc.Close()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Drain a full batch's worth so tryReplenish fires.
	for i := int64(0); i < protocol.CreditReplenishBatch; i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err)
	}

	// Wait for the timer to retry past the failures.
	require.Eventually(t, func() bool {
		return firstSuccessAt.Load() > 0
	}, 1*time.Second, 25*time.Millisecond,
		"retry timer did not deliver credits after %d attempts", attempts.Load())
}

// TestBufferedConn_RetryBackoffPreventsAmplification verifies that the
// 25ms minimum backoff prevents tight retry loops on the data-write path
// when the callback persistently fails. We drive writes faster than the
// backoff interval and assert callback invocation count is bounded by the
// backoff window, not the write rate.
func TestBufferedConn_RetryBackoffPreventsAmplification(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	var callCount atomic.Int64
	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		callCount.Add(1)
		return errors.New("permanent failure")
	}, nil, nil)
	defer bc.Close()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Drive 100 writes with backpressure.
	for i := 0; i < 100; i++ {
		writeWithBackpressure(t, bc, []byte("x"))
	}

	// Wait long enough for all writes to drain and a few ticker fires,
	// but short enough that the ticker can only fire a bounded number of times.
	time.Sleep(150 * time.Millisecond)

	// In 150ms with a 25ms backoff and a 50ms ticker, the upper bound on
	// callback invocations is ~150/25 = 6 backoff-gated calls. The data-write
	// path adds at most one extra call between gate windows. We assert <50 to
	// be very forgiving — the goal is to catch unbounded amplification, not
	// to pin an exact count.
	require.Less(t, callCount.Load(), int64(50),
		"retry amplification: %d callback invocations for 100 writes (expected <50)", callCount.Load())
}

// TestBufferedConn_CreditConservation is the credit conservation invariant
// test: under random callback failure patterns, the total credits delivered
// must equal the total frames drained. Any deviation indicates the silent
// credit-loss bug has been reintroduced.
func TestBufferedConn_CreditConservation(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	const totalFrames = 1000
	var totalDelivered atomic.Int64
	var callIdx atomic.Int64

	// Pseudo-random failure pattern: fail on odd indices (50% failure rate).
	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		idx := callIdx.Add(1)
		if idx%2 == 1 {
			return errors.New("simulated transient failure")
		}
		totalDelivered.Add(n)
		return nil
	}, nil, nil)
	defer bc.Close()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	for i := 0; i < totalFrames; i++ {
		writeWithBackpressure(t, bc, []byte("x"))
	}

	// Wait for ticker to drain everything via retries.
	require.Eventually(t, func() bool {
		return totalDelivered.Load() == totalFrames
	}, 10*time.Second, 25*time.Millisecond,
		"credit conservation violated: delivered %d of %d", totalDelivered.Load(), totalFrames)
}

// TestBufferedConn_NoReplenishDuringFlush verifies that flushRemaining
// (the quit-path drain) does NOT call onReplenish. Credits in flight at
// teardown are intentionally discarded.
func TestBufferedConn_NoReplenishDuringFlush(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	var callsBeforeQuit atomic.Int64
	var callsAfterQuit atomic.Int64
	var quitFired atomic.Bool

	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		if quitFired.Load() {
			callsAfterQuit.Add(1)
		} else {
			callsBeforeQuit.Add(1)
		}
		return nil
	}, nil, nil)

	// Drain reader.
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Write 5 frames (sub-batch). Don't trigger ticker yet.
	for i := 0; i < 5; i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err)
	}

	// Mark quit and immediately close.
	quitFired.Store(true)
	bc.Close()
	c.Close() // explicit close so reader goroutine exits before readWg.Wait
	readWg.Wait()

	require.Equal(t, int64(0), callsAfterQuit.Load(),
		"flushRemaining must not invoke onReplenish — teardown intentionally discards in-flight credits")
}

// TestBufferedConn_QuitObservedDuringRetryLoop verifies that a wedged
// callback (always errors) does not block the drain goroutine from exiting
// when bc.quit is closed. Without the quit observation, drain could loop
// forever on the retry ticker.
func TestBufferedConn_QuitObservedDuringRetryLoop(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		return errors.New("permanent failure")
	}, nil, nil)

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Drain a batch's worth so retries are armed.
	for i := 0; i < int(protocol.CreditReplenishBatch); i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err)
	}

	// Let the retry loop spin briefly.
	time.Sleep(60 * time.Millisecond)

	// Close — drain must exit promptly.
	closeStart := time.Now()
	bc.Close()
	closeElapsed := time.Since(closeStart)

	require.Less(t, closeElapsed, 200*time.Millisecond,
		"drain failed to observe quit during retry loop (took %v)", closeElapsed)

	// done channel should be closed.
	select {
	case <-bc.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("drain goroutine did not exit after Close")
	}
}

// TestBufferedConn_WarnsAfterRetryStreakThreshold is a smoke test for the
// retry-streak WARN log path. We drive >50 consecutive failures and assert
// the drain loop survives (does not panic or deadlock). Log assertion is
// not exercised here — the WARN is emitted via log.Printf and verifying
// log output requires intercepting the standard logger.
func TestBufferedConn_WarnsAfterRetryStreakThreshold(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	var failures atomic.Int64
	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		failures.Add(1)
		return errors.New("simulated wedged control lane")
	}, nil, nil)
	defer bc.Close()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Generate enough writes to cross the retry streak threshold.
	for i := 0; i < 200; i++ {
		writeWithBackpressure(t, bc, []byte("x"))
	}

	// Wait long enough for the ticker to fire many times.
	require.Eventually(t, func() bool {
		return failures.Load() >= int64(retryStreakWarnThreshold)
	}, 5*time.Second, 50*time.Millisecond,
		"only %d failures recorded; expected at least %d", failures.Load(), retryStreakWarnThreshold)
}

// TestBufferedConn_WriteHeadroomBeyondCreditWindow verifies that the writeCh
// has 2× the credit window of headroom — enqueueing exactly clientWriteBufferSize
// messages without draining must succeed, and the next message must fail with
// the buffer-full error. This ensures RTT slack doesn't trip the drop path.
func TestBufferedConn_WriteHeadroomBeyondCreditWindow(t *testing.T) {
	t.Parallel()

	// Use net.Pipe — the drain goroutine will block on the first Write
	// because there is no reader. This lets us fill writeCh without it
	// being drained.
	s, c := net.Pipe()
	defer c.Close()

	bc := newBufferedConn(s, 5*time.Second, nil, nil, nil)
	defer bc.Close()

	// Enqueue exactly clientWriteBufferSize-1 messages. The first one is
	// pulled out of writeCh by the drain goroutine and blocks on c.Write,
	// leaving (clientWriteBufferSize-1) slots free. We can fill those.
	// Then one more would overflow.
	//
	// Wait briefly to let drain pick up the very first message before we
	// start filling, so the math is deterministic.
	_, err := bc.Write([]byte("first"))
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < clientWriteBufferSize; i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err, "write %d/%d should succeed within headroom", i, clientWriteBufferSize)
	}

	// One more should overflow.
	_, err = bc.Write([]byte("overflow"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "client write buffer full")
}

// TestBufferedConn_WriteNonBlocking verifies that Write returns immediately
// when the buffer has room (fast path, no timer allocation).
func TestBufferedConn_WriteNonBlocking(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()
	bc := newBufferedConn(s, 5*time.Second, nil, nil, nil)
	defer bc.Close()

	// Drain so writes to the pipe don't block.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	// Write should return immediately (well under 1ms).
	start := time.Now()
	_, err := bc.Write([]byte("fast"))
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Less(t, elapsed, 10*time.Millisecond)
}

// fakeBandwidthGate is a scripted bandwidthGate used by the unit tests
// below. The Request method returns whatever the next entry in the
// `script` queue dictates; if the queue is empty, it returns the
// `defaultResp`. Counts are tracked atomically. Concurrency-safe because
// drain may invoke it from a different goroutine than the test driver.
type fakeBandwidthGate struct {
	mu          sync.Mutex
	script      []fakeGateResp
	defaultResp fakeGateResp
	requests    atomic.Int64
	allowed     atomic.Int64
	records     atomic.Int64
}

type fakeGateResp struct {
	allow bool
	wait  time.Duration
}

func (g *fakeBandwidthGate) Request(bytes int) (bool, time.Duration) {
	g.requests.Add(1)
	g.mu.Lock()
	var resp fakeGateResp
	if len(g.script) > 0 {
		resp = g.script[0]
		g.script = g.script[1:]
	} else {
		resp = g.defaultResp
	}
	g.mu.Unlock()
	if resp.allow {
		g.allowed.Add(1)
	}
	return resp.allow, resp.wait
}

func (g *fakeBandwidthGate) Record(bytes int) {
	g.records.Add(1)
}

// setDefault atomically replaces the default response. Used by tests
// that flip the gate's behavior mid-run.
func (g *fakeBandwidthGate) setDefault(r fakeGateResp) {
	g.mu.Lock()
	g.defaultResp = r
	g.mu.Unlock()
}

// TestBufferedConn_GateBlocksUntilAllowed verifies drain waits in the
// gate loop until Request returns true. The scripted gate denies twice
// (each with a 50ms wait) then allows; the test asserts the elapsed
// time covers both denials and that the data eventually arrives at the
// remote pipe end.
func TestBufferedConn_GateBlocksUntilAllowed(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	gate := &fakeBandwidthGate{
		script: []fakeGateResp{
			{allow: false, wait: 50 * time.Millisecond},
			{allow: false, wait: 50 * time.Millisecond},
		},
		defaultResp: fakeGateResp{allow: true},
	}

	bc := newBufferedConn(s, 5*time.Second, nil, gate, nil)
	defer bc.Close()

	// Drain reader: collects bytes received on the remote pipe end.
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		received <- append([]byte(nil), buf[:n]...)
	}()

	start := time.Now()
	_, err := bc.Write([]byte("hello"))
	require.NoError(t, err)

	select {
	case got := <-received:
		require.Equal(t, []byte("hello"), got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for gated frame to arrive")
	}
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"drain should have waited through both 50ms denials")
	require.GreaterOrEqual(t, gate.requests.Load(), int64(3),
		"at least three Request calls expected (2 deny + 1 allow)")
	// Record is called after Conn.Write returns, which races with the
	// remote end's Read unblocking — give drain a brief moment to reach
	// the Record call.
	require.Eventually(t, func() bool {
		return gate.records.Load() == int64(1)
	}, 500*time.Millisecond, 5*time.Millisecond, "Record once on success")
}

// TestBufferedConn_GateRecordsOnWriteError verifies that when the gate
// allows a send (reservation held) and the underlying TCP write fails,
// drain still calls gate.Record — the bytes have already crossed the
// backend↔proxy WS link and must count against the cap regardless of
// whether the per-client TCP delivery succeeded. There is no Refund
// path; refunding here would let a busy backend silently bypass
// totalBandwidthMbps under slow-client conditions.
func TestBufferedConn_GateRecordsOnWriteError(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	gate := &fakeBandwidthGate{
		defaultResp: fakeGateResp{allow: true},
	}
	bc := newBufferedConn(s, 5*time.Second, nil, gate, nil)

	// Close the remote pipe end so the drain goroutine's Write fails.
	c.Close()

	_, err := bc.Write([]byte("trigger_drain_error"))
	require.NoError(t, err)

	select {
	case <-bc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine did not exit after write error")
	}

	require.Equal(t, int64(1), gate.requests.Load(), "one Request call")
	require.Equal(t, int64(1), gate.allowed.Load(), "Request returned true once")
	require.Equal(t, int64(1), gate.records.Load(), "Record fires even on write failure (bytes already crossed WS)")
}

// TestBufferedConn_GateRecordsOnSuccess verifies that successful drain
// writes invoke gate.Record exactly once per frame, and never Refund.
func TestBufferedConn_GateRecordsOnSuccess(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	gate := &fakeBandwidthGate{
		defaultResp: fakeGateResp{allow: true},
	}
	bc := newBufferedConn(s, 5*time.Second, nil, gate, nil)
	defer bc.Close()

	// Drain reader.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	const N = 25
	for i := 0; i < N; i++ {
		_, err := bc.Write([]byte("frame"))
		require.NoError(t, err)
	}

	// Allow drain to process all frames.
	require.Eventually(t, func() bool {
		return gate.records.Load() == int64(N)
	}, 2*time.Second, 5*time.Millisecond)

	require.Equal(t, int64(N), gate.records.Load())
}

// TestBufferedConn_GateNoRecordOnQuitDuringWait verifies that when
// drain is suspended in the gate wait (Request returning false
// repeatedly) and the connection is torn down, Record is NOT called
// for the teardown best-effort write. The bytes never crossed Request,
// so the cap accounting must not see them as "delivered through the
// gate" — they're a teardown side-effect, not a normal data path.
func TestBufferedConn_GateNoRecordOnQuitDuringWait(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	gate := &fakeBandwidthGate{
		defaultResp: fakeGateResp{allow: false, wait: 500 * time.Millisecond},
	}
	bc := newBufferedConn(s, 5*time.Second, nil, gate, nil)

	_, err := bc.Write([]byte("blocked"))
	require.NoError(t, err)

	// Let drain enter the gate wait.
	time.Sleep(30 * time.Millisecond)

	// Tear down. Drain should observe quit promptly and exit.
	closeErr := bc.Close()
	_ = closeErr // pipe may or may not error on close

	select {
	case <-bc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not exit after Close")
	}

	require.Equal(t, int64(0), gate.records.Load(),
		"Record must not be called on a teardown best-effort write")
	require.Greater(t, gate.requests.Load(), int64(0),
		"at least one Request call expected before quit fired")
}

// TestBufferedConn_GateQuitTeardownWritesInflightFrame verifies that on
// teardown during a gate wait, the in-flight frame is best-effort
// written to the underlying connection (not silently dropped).
func TestBufferedConn_GateQuitTeardownWritesInflightFrame(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	// Concurrent reader on the remote pipe end so the best-effort write
	// can complete when teardown fires.
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		received <- append([]byte(nil), buf[:n]...)
	}()

	gate := &fakeBandwidthGate{
		defaultResp: fakeGateResp{allow: false, wait: 500 * time.Millisecond},
	}
	bc := newBufferedConn(s, 5*time.Second, nil, gate, nil)

	_, err := bc.Write([]byte("teardown-frame"))
	require.NoError(t, err)

	// Let drain enter the gate wait and block.
	time.Sleep(30 * time.Millisecond)

	// Trigger teardown. Drain's quit branch should best-effort write
	// the in-flight frame before exiting.
	go func() { _ = bc.Close() }()

	select {
	case got := <-received:
		require.Equal(t, []byte("teardown-frame"), got,
			"in-flight frame should reach the remote end via teardown best-effort write")
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive in-flight frame after teardown")
	}
}

// TestBufferedConn_GateDoesNotBlockReplenishRetry is the load-bearing
// regression guard for the v0.3.9 credit-loss fix under the new gate.
// Drain's inner gate-wait select MUST observe replenishTimerCh, or
// credit replenishment retries are silently delayed by bandwidth
// pressure — re-introducing the credit-loss bug v0.3.9 fixed.
//
// Timeline (allow-then-block barrier design):
//  1. Gate starts in allow mode. Drive CreditReplenishBatch writes.
//     Drain processes them, hits the threshold, calls tryReplenish →
//     onReplenish, which blocks on a test-controlled barrier.
//  2. Drain is now suspended inside the SYNCHRONOUS onReplenish call.
//     Test flips gate to block mode and enqueues frame N+1.
//  3. Test releases the barrier. onReplenish returns error → drain arms
//     replenishTimerCh (50ms — the package default
//     replenishTickerInterval) → drain returns to the OUTER select.
//  4. Outer select: writeCh has frame N+1 (ready NOW);
//     replenishTimerCh just armed, 50ms in the future (NOT ready).
//     Deterministic dispatch to writeCh because only writeCh is ready.
//     Drain enters the gate loop.
//  5. Gate denies repeatedly with 500ms wait per iteration. Drain
//     iterates the inner select, which has replenishTimerCh (~50ms
//     in the future at step 4 → ready within one 500ms iteration).
//  6. Inner select observes replenishTimerCh → innerGateReplenishObserved
//     increments. tryReplenish runs from inside the gate loop,
//     succeeds, returns. Drain continues looping on the gate.
//  7. Test observes innerGateReplenishObserved >= 1 → asserts the
//     load-bearing invariant. Test then flips the gate back to allow
//     so drain can complete and exit cleanly.
//
// If anyone removes the inner-loop's <-replenishTimerCh case, drain
// would never observe the timer (the outer select is unreachable while
// drain holds frame N+1), tryReplenish would never fire while gate-
// blocked, and innerGateReplenishObserved would stay at 0 → test fails.
// This load-bearing property was verified during implementation by
// temporarily removing the branch and confirming the test fails.
//
// Determinism window: step 4's "writeCh is ready, timer is not"
// relies on drain dispatching to writeCh within 50ms of timer arming.
// On any sensibly-loaded machine, this is microseconds. On a
// pathologically overloaded CI runner with a GC pause between
// tryReplenish-return and outer-select-entry exceeding 50ms, the
// timer could fire before dispatch, and Go's select randomization
// could route the observation to outerReplenishObserved instead.
// Mitigation: drain's gate loop re-arms the timer on every failed
// tryReplenish, so even a single misrouted first observation is
// followed by more attempts — the test's Eventually loop polls for
// ≥1 inner observation over a 2-second window, which gives the gate
// loop dozens of opportunities.
func TestBufferedConn_GateDoesNotBlockReplenishRetry(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	// Drain reader: keep the pipe non-blocking on the consumer side.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()

	gate := &fakeBandwidthGate{
		defaultResp: fakeGateResp{allow: true},
	}

	// Replenish callback: signals on `entered`, blocks on `release`,
	// then returns error on first call; subsequent calls return nil.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var replenishCalls atomic.Int64
	onReplenish := func(n int64) error {
		idx := replenishCalls.Add(1)
		if idx == 1 {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return errors.New("first replenish forced fail")
		}
		return nil
	}

	bc := newBufferedConn(s, 5*time.Second, onReplenish, gate, nil)
	defer bc.Close()

	// Step 1: drive CreditReplenishBatch writes in allow mode.
	for i := int64(0); i < protocol.CreditReplenishBatch; i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err)
	}

	// Step 2 (wait for it): drain reaches the threshold and is now
	// suspended in onReplenish.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("drain never reached the first replenish call")
	}

	// Step 2 (continued): flip gate to block mode and enqueue frame N+1.
	gate.setDefault(fakeGateResp{allow: false, wait: 500 * time.Millisecond})
	_, err := bc.Write([]byte("frame_N_plus_1"))
	require.NoError(t, err)

	// Step 3: release the barrier. Drain's first replenish fails,
	// arms the 5s timer, and loops back to the outer select.
	close(release)

	// Steps 4-6: wait for the inner counter to increment. The default
	// replenish timer (replenishTickerInterval ≈ 50ms) fires inside
	// the inner gate select as long as drain entered the gate loop
	// before the timer's deadline.
	require.Eventually(t, func() bool {
		return bc.InnerGateReplenishObservedCount() >= 1
	}, 2*time.Second, 5*time.Millisecond,
		"inner gate-loop branch must observe replenishTimerCh — credit replenishment is delayed by bandwidth wait")

	// Load-bearing assertions:
	require.GreaterOrEqual(t, bc.InnerGateReplenishObservedCount(), int64(1),
		"the inner gate-wait select branch is what must observe the timer")
	require.GreaterOrEqual(t, gate.requests.Load(), int64(2),
		"drain should have called gate.Request at least twice (looping on the gate)")
	require.GreaterOrEqual(t, replenishCalls.Load(), int64(2),
		"a second replenish must have fired while drain was gate-blocked")

	// Step 7: flip the gate back to allow so drain can finish frame N+1
	// and the test can clean up cleanly.
	gate.setDefault(fakeGateResp{allow: true})
}

// TestBufferedConn_GateNilBackwardsCompat verifies that a bufferedConn
// with gate=nil behaves identically to pre-refactor: no nil-deref, no
// extra latency, frames flow through unchanged.
func TestBufferedConn_GateNilBackwardsCompat(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	bc := newBufferedConn(s, 5*time.Second, nil, nil, nil)
	defer bc.Close()

	received := make(chan []byte, 100)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			received <- append([]byte(nil), buf[:n]...)
		}
	}()

	const N = 100
	start := time.Now()
	for i := 0; i < N; i++ {
		_, err := bc.Write([]byte("x"))
		require.NoError(t, err)
	}

	// Wait for all frames to arrive.
	count := 0
	deadline := time.After(2 * time.Second)
	for count < N {
		select {
		case <-received:
			count++
		case <-deadline:
			t.Fatalf("only %d/%d frames arrived", count, N)
		}
	}
	elapsed := time.Since(start)
	require.Less(t, elapsed, 1*time.Second,
		"nil gate should not add measurable latency")
}
