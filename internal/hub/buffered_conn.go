package hub

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// clientWriteBufferSize is the number of write messages buffered per client
	// before the client is disconnected (buffer full = backpressure failed).
	clientWriteBufferSize = 64

	// Watermarks for reverse backpressure (Nexus → backend).
	// When the buffer level crosses highWater, onPause is called to tell the
	// backend to stop sending for this client. When it drains to lowWater,
	// onResume is called. The gap (24 entries) absorbs in-flight data during
	// the pause propagation round-trip.
	writeBufferHighWater = 40
	writeBufferLowWater  = 16

	// pauseResumeCooldown is the minimum time between a pause and the
	// subsequent resume. Without this, the drain goroutine writes to the
	// kernel TCP buffer (fast), drops the level to lowWater, and fires
	// resume before the pause signal has even reached the client. The
	// client receives pause+resume almost simultaneously, barely pauses,
	// and in-flight data overflows the headroom.
	pauseResumeCooldown = 500 * time.Millisecond
)

// bufferedConn wraps a net.Conn with an asynchronous write buffer.
//
// Writes are enqueued to a channel and drained by a dedicated goroutine,
// preventing the caller (readPump) from blocking on slow client TCP sockets.
// This breaks the circular dependency that occurs when two clients on the
// same backend create a data flow loop (e.g. the self-loop topology where
// one client carries tunnel ACKs needed to unblock another client's TCP write).
//
// Flow control: when the buffer level crosses writeBufferHighWater, the
// onPause callback signals the backend to stop sending data for this client.
// When it drains to writeBufferLowWater, onResume signals the backend to
// resume. This mirrors the existing client→Nexus flow control in reverse.
type bufferedConn struct {
	net.Conn
	writeCh      chan []byte
	quit         chan struct{}
	done         chan struct{} // closed when drain goroutine exits
	closeOnce    sync.Once
	writeTimeout time.Duration

	// Flow control — callbacks must be non-blocking (they run inside
	// Write and drain, which are on the readPump and drain goroutines).
	level    atomic.Int64  // current buffer occupancy
	paused   atomic.Bool   // true if onPause was called and onResume hasn't
	pausedAt atomic.Int64  // UnixNano timestamp of last pause (for cooldown)
	onPause  func()        // called when level >= writeBufferHighWater
	onResume func()        // called when level <= writeBufferLowWater
}

// newBufferedConn wraps conn with an asynchronous write buffer.
// writeTimeout is the deadline applied to each individual TCP write;
// pass 0 to skip write deadlines. onPause/onResume are optional callbacks
// for reverse backpressure; pass nil to disable.
func newBufferedConn(conn net.Conn, writeTimeout time.Duration, onPause, onResume func()) *bufferedConn {
	bc := &bufferedConn{
		Conn:         conn,
		writeCh:      make(chan []byte, clientWriteBufferSize),
		quit:         make(chan struct{}),
		done:         make(chan struct{}),
		writeTimeout: writeTimeout,
		onPause:      onPause,
		onResume:     onResume,
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

	// Increment level BEFORE enqueuing so drain's decrement never causes
	// level to go negative (drain can dequeue between our send and Add).
	lvl := bc.level.Add(1)

	// Non-blocking enqueue. readPump must never block here — any blocking
	// recreates the self-loop deadlock for the duration of the block.
	select {
	case bc.writeCh <- buf:
		// Check high watermark — signal backend to pause sending for this client.
		if bc.onPause != nil && lvl >= writeBufferHighWater {
			if bc.paused.CompareAndSwap(false, true) {
				bc.pausedAt.Store(time.Now().UnixNano())
				bc.onPause()
				// Schedule a resume check after the cooldown, in case the
				// drain goroutine has no more data to trigger a check.
				time.AfterFunc(pauseResumeCooldown, bc.tryResume)
			}
		}
		return len(data), nil
	case <-bc.quit:
		bc.level.Add(-1)
		return 0, net.ErrClosed
	default:
		bc.level.Add(-1)
		return 0, fmt.Errorf("client write buffer full (%d pending)", clientWriteBufferSize)
	}
}

// drain writes buffered data to the underlying connection. Runs in its
// own goroutine until the connection is closed or a write error occurs.
// On quit signal, flushes any remaining buffered data before exiting.
func (bc *bufferedConn) drain() {
	defer close(bc.done)
	for {
		select {
		case <-bc.quit:
			bc.flushRemaining()
			bc.resumeIfPaused()
			return
		case data := <-bc.writeCh:
			if bc.writeTimeout > 0 {
				_ = bc.Conn.SetWriteDeadline(time.Now().Add(bc.writeTimeout))
			}
			if _, err := bc.Conn.Write(data); err != nil {
				log.Printf("WARN: bufferedConn drain write failed: %v", err)
				bc.resumeIfPaused()
				// Signal quit so Write() returns immediately instead of
				// enqueuing data that will never be drained. Close the
				// underlying conn so the read side (Client.Start) gets an
				// error and triggers RemoveClient cleanup.
				bc.closeOnce.Do(func() { close(bc.quit) })
				_ = bc.Conn.Close()
				return
			}
			lvl := bc.level.Add(-1)
			if bc.onResume != nil && lvl <= writeBufferLowWater {
				bc.tryResume()
			}
		}
	}
}

// tryResume sends a resume signal if the backend was paused AND the
// cooldown has elapsed. The cooldown prevents premature resume: without
// it, the drain goroutine writes to the kernel TCP buffer (fast), drops
// the level, and fires resume before the pause signal has reached the
// client. Also called by a timer scheduled when pause fires, to handle
// the case where the drain goroutine is idle after the cooldown.
func (bc *bufferedConn) tryResume() {
	if !bc.paused.Load() {
		return
	}
	if time.Since(time.Unix(0, bc.pausedAt.Load())) < pauseResumeCooldown {
		return
	}
	if bc.level.Load() <= writeBufferLowWater {
		if bc.paused.CompareAndSwap(true, false) {
			bc.onResume()
		}
	}
}

// resumeIfPaused sends a resume signal unconditionally (no cooldown).
// Used on drain exit paths (error, quit) where the connection is being
// torn down and we must not leave the backend permanently paused.
func (bc *bufferedConn) resumeIfPaused() {
	if bc.onResume != nil && bc.paused.CompareAndSwap(true, false) {
		bc.onResume()
	}
}

// flushRemaining best-effort drains any data still in writeCh.
// Stops on the first write error or empty channel.
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
			bc.level.Add(-1)
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
// remaining data, then closes the underlying connection and waits for
// drain to exit. If drain is stuck in a blocking Write (slow client),
// the conn close unblocks it after a short timeout.
// Safe to call multiple times — the second call is a no-op.
func (bc *bufferedConn) Close() error {
	bc.StopWrites()
	// Best-effort flush: give drain a moment to write remaining data.
	// Non-stuck drains complete in microseconds; stuck drains are
	// unblocked by the conn close after the timeout.
	select {
	case <-bc.done:
		// Drain already exited — all data flushed.
	case <-time.After(50 * time.Millisecond):
		// Drain is stuck in a blocking Write.
	}
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
