package hub

import (
	"net"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/stretchr/testify/require"
)

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
	bc := newBufferedConn(s, 5*time.Second, func(n int64) {
		replenished <- n
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
