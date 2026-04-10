package hub

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
)

// clientWriteBufferSize is the number of write messages buffered per client.
// Equals DefaultCreditCapacity — the sender cannot enqueue more than the
// credits it was granted.
const clientWriteBufferSize = int(protocol.DefaultCreditCapacity)

// bufferedConn wraps a net.Conn with an asynchronous write buffer.
//
// Writes are enqueued to a channel and drained by a dedicated goroutine,
// preventing the caller (readPump) from blocking on slow client TCP sockets.
// This breaks the circular dependency that occurs when two clients on the
// same backend create a data flow loop (e.g. the self-loop topology where
// one client carries tunnel ACKs needed to unblock another client's TCP write).
//
// Flow control: the drain goroutine calls onReplenish after every
// CreditReplenishBatch successful TCP writes, allowing the sender to
// replenish the receiver's credits and resume sending.
type bufferedConn struct {
	net.Conn
	writeCh      chan []byte
	quit         chan struct{}
	done         chan struct{} // closed when drain goroutine exits
	closeOnce    sync.Once
	writeTimeout time.Duration
	onReplenish  func(int64) // called every CreditReplenishBatch drains; must be non-blocking
}

// newBufferedConn wraps conn with an asynchronous write buffer.
// writeTimeout is the deadline applied to each individual TCP write;
// pass 0 to skip write deadlines. onReplenish is called from the drain
// goroutine after every CreditReplenishBatch successful writes; pass nil
// to disable credit replenishment.
func newBufferedConn(conn net.Conn, writeTimeout time.Duration, onReplenish func(int64)) *bufferedConn {
	bc := &bufferedConn{
		Conn:         conn,
		writeCh:      make(chan []byte, clientWriteBufferSize),
		quit:         make(chan struct{}),
		done:         make(chan struct{}),
		writeTimeout: writeTimeout,
		onReplenish:  onReplenish,
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

	// Non-blocking enqueue. readPump must never block here — any blocking
	// recreates the self-loop deadlock for the duration of the block.
	select {
	case bc.writeCh <- buf:
		return len(data), nil
	case <-bc.quit:
		return 0, net.ErrClosed
	default:
		return 0, fmt.Errorf("client write buffer full (%d pending)", clientWriteBufferSize)
	}
}

// drain writes buffered data to the underlying connection. Runs in its
// own goroutine until the connection is closed or a write error occurs.
// On quit signal, flushes any remaining buffered data before exiting.
func (bc *bufferedConn) drain() {
	defer close(bc.done)
	var consumed int64
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
				// enqueuing data that will never be drained. Close the
				// underlying conn so the read side (Client.Start) gets an
				// error and triggers RemoveClient cleanup.
				bc.closeOnce.Do(func() { close(bc.quit) })
				_ = bc.Conn.Close()
				return
			}
			consumed++
			if bc.onReplenish != nil && consumed >= protocol.CreditReplenishBatch {
				bc.onReplenish(consumed)
				consumed = 0
			}
		}
	}
}

// flushRemaining best-effort drains any data still in writeCh.
// Stops on the first write error or empty channel. Does not count
// toward replenishment (connection is being torn down).
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
// remaining data (via StopWrites), then closes the underlying connection
// to unblock any stuck drain Write, and waits for drain to exit.
// Safe to call multiple times — the second call is a no-op.
func (bc *bufferedConn) Close() error {
	bc.StopWrites() // includes 50ms flush window
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
