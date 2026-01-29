package peer

import (
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/google/uuid"
)

// ErrPeerSendFailed is returned when the peer send queue is full.
var ErrPeerSendFailed = errors.New("peer send queue full")

// TunneledConn implements the net.Conn interface for a client connection
// that is being tunneled from another peer.
type TunneledConn struct {
	clientID uuid.UUID
	clientIp string
	connPort int
	peer     iface.Peer     // The peer that this connection is tunneled through.
	pipe     *io.PipeReader // The read end of the pipe that receives data from the peer's read pump.
	pipeW    *io.PipeWriter // The write end of the pipe that sends data to the peer's read pump.

	// Pause/resume support - tracks state for origin peer coordination.
	// IMPORTANT: Read() does NOT block locally when paused. The actual TCP
	// backpressure happens at the origin peer's PausableConn via PeerTunnelPause.
	// Blocking here would deadlock the shared readPump since io.Pipe is unbuffered.
	mu     sync.Mutex
	paused bool
	closed bool
}

// NewTunneledConn creates a new virtual connection for a tunneled client.
func NewTunneledConn(clientID uuid.UUID, peer iface.Peer, clientIp string, connPort int) *TunneledConn {
	pr, pw := io.Pipe()
	return &TunneledConn{
		clientID: clientID,
		clientIp: clientIp,
		connPort: connPort,
		peer:     peer,
		pipe:     pr,
		pipeW:    pw,
	}
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
func (c *TunneledConn) Write(b []byte) (n int, err error) {
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
func (c *TunneledConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
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
	// clientIp is in "ip:port" format from conn.RemoteAddr().String()
	host, portStr, err := net.SplitHostPort(c.clientIp)
	if err != nil {
		// Fallback: try parsing as bare IP (shouldn't happen in practice)
		return &net.TCPAddr{IP: net.ParseIP(c.clientIp), Port: 0}
	}
	port, _ := strconv.Atoi(portStr)
	return &net.TCPAddr{IP: net.ParseIP(host), Port: port}
}
func (c *TunneledConn) SetDeadline(t time.Time) error      { return nil }
func (c *TunneledConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *TunneledConn) SetWriteDeadline(t time.Time) error { return nil }
