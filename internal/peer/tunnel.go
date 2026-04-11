package peer

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
)

// ErrPeerSendFailed is returned when the peer send queue is full.
var ErrPeerSendFailed = errors.New("peer send queue full")

// TunneledConn implements the net.Conn interface for a client connection
// that is being tunneled from another peer.
type TunneledConn struct {
	clientID   uuid.UUID
	remoteAddr *net.TCPAddr // parsed eagerly at construction; never nil
	connPort   int
	peer       iface.Peer     // The peer that this connection is tunneled through.
	pipe       *io.PipeReader // The read end of the pipe that receives data from the peer's read pump.
	pipeW      *io.PipeWriter // The write end of the pipe that sends data to the peer's read pump.

	// Pause/resume support - tracks state for origin peer coordination.
	// IMPORTANT: Read() does NOT block locally when paused. The actual TCP
	// backpressure happens at the origin peer's PausableConn via PeerTunnelPause.
	// Blocking here would deadlock the shared readPump since io.Pipe is unbuffered.
	mu     sync.Mutex
	paused bool
	closed bool

	// Credit-based flow control for the destination → origin direction.
	// sendCredits is a counting semaphore: Write() acquires one credit before
	// sending data back to the origin peer. Credits are granted by the origin
	// via PeerTunnelCredits messages. Eagerly initialized to avoid data races.
	closeCh     chan struct{} // closed by Close(), unblocks Write's credit wait
	sendCredits chan struct{} // counting semaphore for outbound data
	creditActive atomic.Bool  // true after first AddCredits; gates blocking in Write

	// Tracks consumed direction-1 (origin → destination) messages for
	// credit replenishment back to the origin.
	forwardConsumed atomic.Int32
}

// NewTunneledConn creates a new virtual connection for a tunneled client.
// clientIp is parsed eagerly into a *net.TCPAddr using numeric-only parsing
// (no DNS resolution). If parsing fails, a warning is logged and a zero-value
// fallback is stored.
func NewTunneledConn(clientID uuid.UUID, peer iface.Peer, clientIp string, connPort int) *TunneledConn {
	addr := parseTCPAddr(clientIp)
	if addr.IP == nil || addr.IP.IsUnspecified() {
		log.Printf("WARN: [TUNNEL] Failed to parse client address %q for client %s", clientIp, clientID)
	}
	pr, pw := io.Pipe()
	return &TunneledConn{
		clientID:    clientID,
		remoteAddr:  addr,
		connPort:    connPort,
		peer:        peer,
		pipe:        pr,
		pipeW:       pw,
		closeCh:     make(chan struct{}),
		sendCredits: make(chan struct{}, protocol.DefaultCreditCapacity),
	}
}

// parseTCPAddr parses an "ip:port" string into a *net.TCPAddr using only
// numeric parsing (net.ParseIP) — never triggers DNS resolution.
func parseTCPAddr(hostPort string) *net.TCPAddr {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		// Bare IP without port — try direct parse
		if ip := net.ParseIP(hostPort); ip != nil {
			return &net.TCPAddr{IP: ip}
		}
		return &net.TCPAddr{}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return &net.TCPAddr{}
	}
	port, _ := strconv.Atoi(portStr)
	return &net.TCPAddr{IP: ip, Port: port}
}

// parseUDPAddr parses an "ip:port" string into a *net.UDPAddr using only
// numeric parsing (net.ParseIP) — never triggers DNS resolution.
// Returns nil if the input cannot be parsed.
func parseUDPAddr(hostPort string) *net.UDPAddr {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		if ip := net.ParseIP(hostPort); ip != nil {
			return &net.UDPAddr{IP: ip}
		}
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	port, _ := strconv.Atoi(portStr)
	return &net.UDPAddr{IP: ip, Port: port}
}

// Read reads from the pipe. This method does NOT block when paused because:
// 1. io.Pipe is unbuffered - blocking here would deadlock the shared readPump
// 2. The actual backpressure happens at the origin peer's PausableConn
// 3. Any in-flight data when pause is requested will still be delivered
func (c *TunneledConn) Read(b []byte) (n int, err error) {
	return c.pipe.Read(b)
}

// WriteToPipe writes data received from a peer into the tunnel's read pipe.
// Called by the peer's readPump - must never block indefinitely.
func (c *TunneledConn) WriteToPipe(b []byte) (n int, err error) {
	return c.pipeW.Write(b)
}

// Write writes data back to the originating peer.
// Returns ErrPeerSendFailed if the peer send queue is full.
// When credit-based flow control is active (creditActive is true), Write
// blocks until a credit is available. This is safe because Write is called
// from bufferedConn.drain(), which is a per-connection goroutine.
func (c *TunneledConn) Write(b []byte) (n int, err error) {
	// Fail fast if already closed.
	select {
	case <-c.closeCh:
		return 0, net.ErrClosed
	default:
	}

	// Block on credits if flow control is active.
	if c.creditActive.Load() {
		select {
		case <-c.sendCredits:
			// credit acquired
		case <-c.closeCh:
			return 0, net.ErrClosed
		}
	}

	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelData
	copy(header[1:], c.clientID[:])

	message := append(header, b...)
	if !c.peer.Send(message) {
		return 0, ErrPeerSendFailed
	}
	return len(b), nil
}

// Close signals to the peer that this end of the tunnel is closed.
// Safe to call multiple times - only sends close message once.
// Closes closeCh to unblock any Write waiting on credit acquisition.
func (c *TunneledConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.closeCh) // unblock any Write waiting on credits
	c.mu.Unlock()

	// Send close signal to peer
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelClose
	copy(header[1:], c.clientID[:])
	c.peer.Send(header)
	return c.pipeW.Close()
}

// Pause propagates pause to the origin peer where actual backpressure occurs.
// The local paused flag tracks state but does NOT block Read() locally.
// Local state is only updated if the peer message is successfully queued.
func (c *TunneledConn) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.paused {
		return // Already closed or already paused
	}

	// Propagate to origin peer to pause reading from actual client
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelPause
	copy(header[1:], c.clientID[:])
	if c.peer.Send(header) {
		c.paused = true
	} else {
		log.Printf("WARN: Failed to send PeerTunnelPause for client %s (peer send queue full); pause not applied", c.clientID)
	}
}

// Resume propagates resume to the origin peer to restore data flow.
// Local state is only updated if the peer message is successfully queued.
func (c *TunneledConn) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || !c.paused {
		return // Already closed or not paused
	}

	// Propagate to origin peer to resume reading from actual client
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelResume
	copy(header[1:], c.clientID[:])
	if c.peer.Send(header) {
		c.paused = false
	} else {
		log.Printf("WARN: Failed to send PeerTunnelResume for client %s (peer send queue full); resume not applied", c.clientID)
	}
}

// IsPaused returns the current pause state.
func (c *TunneledConn) IsPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// LocalAddr, RemoteAddr, SetDeadline, etc., are part of the net.Conn interface.
func (c *TunneledConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: c.connPort}
}
func (c *TunneledConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}
func (c *TunneledConn) SetDeadline(t time.Time) error      { return nil }
func (c *TunneledConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *TunneledConn) SetWriteDeadline(t time.Time) error { return nil }

// AddCredits grants send credits to this tunnel (direction 2: destination → origin).
// Called from readPump when the origin peer sends PeerTunnelCredits.
// Activates credit-based flow control on the first call.
func (c *TunneledConn) AddCredits(credits int64) {
	// Cap to prevent CPU-burn from a malicious credit value.
	if credits > protocol.DefaultCreditCapacity {
		credits = protocol.DefaultCreditCapacity
	}
	if credits <= 0 {
		return
	}
	c.creditActive.Store(true)
	for i := int64(0); i < credits; i++ {
		select {
		case c.sendCredits <- struct{}{}:
		default:
		}
	}
}

// makeTunnelCreditMessage builds a PeerTunnelCredits binary message.
// Wire format: [PeerTunnelCredits][clientID 16B][credits 8B big-endian].
func makeTunnelCreditMessage(clientID uuid.UUID, credits int64) []byte {
	msg := make([]byte, 1+protocol.ClientIDLength+8)
	msg[0] = protocol.PeerTunnelCredits
	copy(msg[1:], clientID[:])
	binary.BigEndian.PutUint64(msg[1+protocol.ClientIDLength:], uint64(credits))
	return msg
}

// retryCreditSend retries a failed credit replenishment send with exponential
// backoff. Without this, a dropped credit message permanently stalls the
// tunnel because the sender blocks on credits and no new data arrives to
// retrigger the batch threshold. Gives up after 5 attempts (~1.5s total).
func retryCreditSend(peer iface.Peer, msg []byte, done <-chan struct{}) {
	backoff := 50 * time.Millisecond
	for range 5 {
		select {
		case <-time.After(backoff):
			if peer.Send(msg) {
				return
			}
			backoff *= 2
		case <-done:
			return
		}
	}
}
