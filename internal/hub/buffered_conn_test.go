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
	bc := newBufferedConn(s, 5*time.Second, nil)

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
	bc := newBufferedConn(s, 5*time.Second, nil)

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
	})

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
	})
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
	})
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
	})
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
	})
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
	})

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
	})

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
	})
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

	bc := newBufferedConn(s, 5*time.Second, nil)
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
	bc := newBufferedConn(s, 5*time.Second, nil)
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
