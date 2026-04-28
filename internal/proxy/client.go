package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/google/uuid"
)

// copyBufferSize defines the size of the buffer used for copying data between client and backend.
const copyBufferSize = 32 * 1024 // 32KB

// bufferPool is a pool of byte slices used for copying data to reduce allocations.
var bufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, copyBufferSize)
		return &b
	},
}

// GetBuffer retrieves a buffer from the pool.
func GetBuffer() *[]byte {
	return bufferPool.Get().(*[]byte)
}

// PutBuffer returns a buffer to the pool.
func PutBuffer(buf *[]byte) {
	bufferPool.Put(buf)
}

// Client represents a single connection from an end-user.
type Client struct {
	id       uuid.UUID
	conn     net.Conn
	backend  iface.Backend
	config   *config.Config
	hostname string
	initial  []byte
	isTLS    bool
}

// NewClient creates a new client handler.
func NewClient(conn net.Conn, backend iface.Backend, cfg *config.Config, hostname string, isTLS bool) *Client {
	return &Client{
		id:       uuid.New(),
		conn:     conn,
		backend:  backend,
		config:   cfg,
		hostname: hostname,
		isTLS:    isTLS,
	}
}

// NewClientWithPrelude is like NewClient, but will send the provided initial
// bytes to the backend immediately after establishing the client association
// before streaming any further data read from conn. Useful when earlier sniff
// logic consumed and recorded initial bytes (e.g., TLS ClientHello or HTTP headers).
func NewClientWithPrelude(conn net.Conn, backend iface.Backend, cfg *config.Config, hostname string, initial []byte, isTLS bool) *Client {
	c := NewClient(conn, backend, cfg, hostname, isTLS)
	c.initial = initial
	return c
}

// Start begins the bi-directional proxying of data.
func (c *Client) Start() {
	if err := c.backend.AddClient(c.conn, c.id, c.hostname, c.isTLS); err != nil {
		log.Printf("ERROR: Failed to register client %s with backend %s: %v", c.id, c.backend.ID(), err)
		c.conn.Close()
		return
	}
	defer c.backend.RemoveClient(c.id)

	bufPtr := GetBuffer()
	defer PutBuffer(bufPtr)
	buf := *bufPtr

	// If we have captured initial bytes (from protocol sniffing), send them
	// to the backend before reading anything further from the client.
	if len(c.initial) > 0 {
		if err := c.backend.SendData(c.id, c.initial); err != nil {
			log.Printf("ERROR: Failed to send initial bytes to backend %s for client %s: %v\nTerminating client connection", c.backend.ID(), c.id, err)
			return
		}
		// Allow GC to reclaim the slice.
		c.initial = nil
	}

	for {
		var readDeadline time.Time
		var deadlineStart time.Time
		if c.config != nil && c.config.IdleTimeout() > 0 {
			deadlineStart = time.Now()
			readDeadline = deadlineStart.Add(c.config.IdleTimeout())
		}
		c.conn.SetReadDeadline(readDeadline)

		n, err := c.conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Check if the backend has been writing data to this
					// client since the deadline was set (e.g. download in
					// progress). If so, the connection is not truly idle.
					if !deadlineStart.IsZero() && c.backend.HasRecentActivity(c.id, deadlineStart) {
						continue // write-side active, reset deadline
					}
					log.Printf("INFO: Client %s idle timeout reached. Closing connection.", c.id)
					c.conn.Close()
				} else {
					log.Printf("ERROR: Failed to read from client %s: %v", c.id, err)
				}
			}
			break
		}

		err = c.backend.SendData(c.id, buf[:n])
		if err != nil {
			if errors.Is(err, iface.ErrClientGone) {
				// Backend already disconnected this client (typical
				// post-response close on no-keepalive HTTP). Forward bytes
				// arriving during the user-side TCP grace window are
				// expected race noise, not a failure.
				log.Printf("INFO: Forward path closing for client %s; backend disconnected", c.id)
			} else {
				log.Printf("ERROR: Failed to send data to backend %s for client %s: %v\nTerminating client connection", c.backend.ID(), c.id, err)
			}
			break
		}
	}
}
