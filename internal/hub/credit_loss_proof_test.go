package hub

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
)

// TestPostFixCreditLoss_NoCreditsLost is the post-fix counterpart of the
// proof test that demonstrates v0.3.8's silent credit loss bug. The exact
// same scenario — bufferedConn with a callback that "drops" 1-in-3 batches
// (simulating SendControlMessage failures under control channel pressure) —
// must NOT lose any credits with the v0.3.9 fix in place.
//
// On v0.3.8 (pre-fix), this scenario loses ~33% of credits permanently.
// On v0.3.9 (post-fix), drain preserves consumed on failure and retries
// via the lazy timer until all credits land.
func TestPostFixCreditLoss_NoCreditsLost(t *testing.T) {
	t.Parallel()

	s, c := net.Pipe()
	defer c.Close()

	const totalFrames = 1000
	var totalDelivered atomic.Int64
	var callIdx atomic.Int64

	// Post-fix callback signature: func(int64) error. We "drop" 1-in-3 calls
	// by returning an error, which exercises the retry path. Drain preserves
	// consumed across retries, so the eventual successful call delivers the
	// accumulated credit count.
	bc := newBufferedConn(s, 5*time.Second, func(n int64) error {
		idx := callIdx.Add(1)
		if idx%3 == 0 {
			return errors.New("simulated control lane full")
		}
		totalDelivered.Add(n)
		return nil
	}, nil, nil)
	defer bc.Close()

	// Drain the pipe so writes go through.
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

	// Wait for the lazy retry timer to flush any failed batches.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if totalDelivered.Load() >= int64(totalFrames) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	delivered := totalDelivered.Load()
	totalCalls := callIdx.Load()
	dropped := totalCalls / 3

	t.Logf("POST-FIX VERIFICATION: drained %d frames, callback fired %d times (drops=%d)",
		totalFrames, totalCalls, dropped)
	t.Logf("POST-FIX VERIFICATION: credits delivered = %d / %d (loss = %d)",
		delivered, int64(totalFrames), int64(totalFrames)-delivered)

	// CRITICAL ASSERTION: with the fix, NO credits are lost.
	// Pre-fix v0.3.8 loses ~33% of credits with this same drop pattern.
	// Post-fix v0.3.9 delivers 100% via the lazy retry timer + preserved consumed.
	if delivered != int64(totalFrames) {
		t.Fatalf("FIX REGRESSION: expected all %d credits to be delivered, got %d (lost %d)",
			totalFrames, delivered, int64(totalFrames)-delivered)
	}

	// Note: the callback may be invoked far fewer than 125 times because
	// the 25ms backoff after a failure coalesces subsequent drain progress
	// into a single larger replenishment when the timer next fires. This
	// is desirable — fewer wakeups, same total credit delivery.
}

// reference protocol.CreditReplenishBatch so the import survives if asserts shift.
var _ = protocol.CreditReplenishBatch
