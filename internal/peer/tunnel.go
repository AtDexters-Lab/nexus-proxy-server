package peer

import (
	"io"
	"net"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/google/uuid"
)

// TunneledConn implements the net.Conn interface for a client connection
// that is being tunneled from another peer.
type TunneledConn struct {
	clientID uuid.UUID
	clientIp string
	connPort int
	peer     iface.Peer     // The peer that this connection is tunneled through.
	pipe     *io.PipeReader // The read end of the pipe that receives data from the peer's read pump.
	pipeW    *io.PipeWriter // The write end of the pipe that sends data to the peer's read pump.
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

// Read reads data from the pipe, which is written to by the peer's read pump.
func (c *TunneledConn) Read(b []byte) (n int, err error) {
	return c.pipe.Read(b)
}

// WriteToPipe writes data received from a peer into the tunnel's read pipe.
// This is the FIX for the logical loop bug.
func (c *TunneledConn) WriteToPipe(b []byte) (n int, err error) {
	return c.pipeW.Write(b)
}

// Write writes data back to the originating peer.
func (c *TunneledConn) Write(b []byte) (n int, err error) {
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelData
	copy(header[1:], c.clientID[:])

	message := append(header, b...)
	c.peer.Send(message)
	return len(b), nil
}

// Close signals to the peer that this end of the tunnel is closed.
func (c *TunneledConn) Close() error {
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelClose
	copy(header[1:], c.clientID[:])
	c.peer.Send(header)
	return c.pipeW.Close()
}

// LocalAddr, RemoteAddr, SetDeadline, etc., are part of the net.Conn interface.
func (c *TunneledConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: c.connPort}
}
func (c *TunneledConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(c.clientIp), Port: 0}
}
func (c *TunneledConn) SetDeadline(t time.Time) error      { return nil }
func (c *TunneledConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *TunneledConn) SetWriteDeadline(t time.Time) error { return nil }
