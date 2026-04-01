package peer

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
)

type udpInboundTunnel struct {
	backend iface.Backend
	conn    net.Conn
}

type tunneledUDPConn struct {
	id          uuid.UUID
	peer        iface.Peer
	local       *net.UDPAddr
	remote      *net.UDPAddr
	maxDatagram int
	onClose     func() // called once on Close; may be nil

	closeOnce sync.Once
	closed    chan struct{}
}

func newTunneledUDPConn(id uuid.UUID, peer iface.Peer, remote *net.UDPAddr, localPort int, maxDatagram int) *tunneledUDPConn {
	remoteCopy := *remote
	return &tunneledUDPConn{
		id:          id,
		peer:        peer,
		local:       &net.UDPAddr{Port: localPort},
		remote:      &remoteCopy,
		maxDatagram: maxDatagram,
		closed:      make(chan struct{}),
	}
}

func (c *tunneledUDPConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *tunneledUDPConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if c.maxDatagram > 0 && len(p) > c.maxDatagram {
		return 0, errors.New("udp datagram exceeds configured limit")
	}

	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelData
	copy(header[1:], c.id[:])
	message := append(header, p...)
	if ok := c.peer.Send(message); !ok {
		return 0, errors.New("peer send queue full")
	}
	return len(p), nil
}

func (c *tunneledUDPConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		header := make([]byte, 1+protocol.ClientIDLength)
		header[0] = protocol.PeerTunnelClose
		copy(header[1:], c.id[:])
		c.peer.Send(header)
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}

func (c *tunneledUDPConn) LocalAddr() net.Addr  { return c.local }
func (c *tunneledUDPConn) RemoteAddr() net.Addr { return c.remote }

func (c *tunneledUDPConn) SetDeadline(time.Time) error      { return nil }
func (c *tunneledUDPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *tunneledUDPConn) SetWriteDeadline(time.Time) error { return nil }
