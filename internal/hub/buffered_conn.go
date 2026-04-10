package hub

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// clientWriteBufferSize is the number of write messages buffered per client
// before backpressure kicks in. Matches the per-connection write buffer
// used on the client side.
const clientWriteBufferSize = 64

// writeEnqueueTimeout is the maximum time Write will block waiting for
// room in the buffer before returning an error. This value balances two
// competing concerns:
//   - Too short: slow-but-healthy clients are disconnected unnecessarily.
//   - Too long: readPump stalls on one client, delaying all others on the
//     same backend. In the self-loop topology, this recreates the deadlock.
//
// 1 second is long enough for the drain goroutine to free a slot under
// normal TCP flow control, but short enough to break circular deadlocks
// within a single WebSocket ping period.
const writeEnqueueTimeout = 1 * time.Second

// bufferedConn wraps a net.Conn with an asynchronous write buffer.
//
// Writes are enqueued to a channel and drained by a dedicated goroutine,
// preventing the caller (readPump) from blocking on slow client TCP sockets.
// This breaks the circular dependency that occurs when two clients on the
// same backend create a data flow loop (e.g. the self-loop topology where
// one client carries tunnel ACKs needed to unblock another client's TCP write).
type bufferedConn struct {
	net.Conn
	writeCh      chan []byte
	quit         chan struct{}
	done         chan struct{} // closed when drain goroutine exits
	closeOnce    sync.Once
	writeTimeout time.Duration
}

// newBufferedConn wraps conn with an asynchronous write buffer.
// writeTimeout is the deadline applied to each individual TCP write;
// pass 0 to skip write deadlines.
func newBufferedConn(conn net.Conn, writeTimeout time.Duration) *bufferedConn {
	bc := &bufferedConn{
		Conn:         conn,
		writeCh:      make(chan []byte, clientWriteBufferSize),
		quit:         make(chan struct{}),
		done:         make(chan struct{}),
		writeTimeout: writeTimeout,
	}
	go bc.drain()
	return bc
}

// Write enqueues data for asynchronous delivery to the underlying connection.
// It copies the data (caller may reuse its buffer) and returns immediately
// when the buffer has room. If the buffer is full, it blocks for up to
// writeEnqueueTimeout — providing backpressure for slow clients while
// preventing indefinite blocking that would deadlock the self-loop topology.
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

	// Fast path: buffer has room.
	select {
	case bc.writeCh <- buf:
		return len(data), nil
	case <-bc.quit:
		return 0, net.ErrClosed
	default:
	}

	// Slow path: buffer full — wait briefly for room.
	timer := time.NewTimer(writeEnqueueTimeout)
	defer timer.Stop()
	select {
	case bc.writeCh <- buf:
		return len(data), nil
	case <-bc.quit:
		return 0, net.ErrClosed
	case <-timer.C:
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
			return
		case data := <-bc.writeCh:
			if bc.writeTimeout > 0 {
				_ = bc.Conn.SetWriteDeadline(time.Now().Add(bc.writeTimeout))
			}
			if _, err := bc.Conn.Write(data); err != nil {
				log.Printf("WARN: bufferedConn drain write failed: %v", err)
				// Signal quit so Write() returns immediately instead of
				// blocking on writeEnqueueTimeout with no active drainer.
				// Close the underlying conn so the read side (Client.Start)
				// gets an error and triggers RemoveClient cleanup.
				// Cannot call bc.Close() here — it waits on bc.done (deadlock).
				bc.closeOnce.Do(func() { close(bc.quit) })
				_ = bc.Conn.Close()
				return
			}
		}
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
		default:
			return
		}
	}
}

// StopWrites signals the drain goroutine to flush remaining data and exit.
// Called by GracefulCloseConn before TCP half-close to ensure queued data
// is delivered before the FIN is sent.
func (bc *bufferedConn) StopWrites() {
	bc.closeOnce.Do(func() {
		close(bc.quit)
	})
	<-bc.done
}

// Close stops the drain goroutine, waits for it to flush remaining data,
// and closes the underlying connection. Safe to call multiple times.
func (bc *bufferedConn) Close() error {
	bc.StopWrites()
	return bc.Conn.Close()
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
