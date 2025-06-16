package proxy

import (
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/google/uuid"
)

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
	id      uuid.UUID
	conn    net.Conn
	backend iface.Backend
	config  *config.Config
}

// NewClient creates a new client handler.
func NewClient(conn net.Conn, backend iface.Backend, cfg *config.Config) *Client {
	return &Client{
		id:      uuid.New(),
		conn:    conn,
		backend: backend,
		config:  cfg,
	}
}

// Start begins the bi-directional proxying of data.
func (c *Client) Start() {
	c.backend.AddClient(c.conn, c.id)
	defer c.backend.RemoveClient(c.id)

	bufPtr := GetBuffer()
	defer PutBuffer(bufPtr)
	buf := *bufPtr

	for {
		var readDeadline time.Time
		if c.config != nil && c.config.IdleTimeout() > 0 {
			readDeadline = time.Now().Add(c.config.IdleTimeout())
		}
		c.conn.SetReadDeadline(readDeadline)

		n, err := c.conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					log.Printf("INFO: Client %s idle timeout reached. Closing connection.", c.id)
				} else {
					log.Printf("ERROR: Failed to read from client %s: %v", c.id, err)
				}
			}
			break
		}

		c.backend.SendData(c.id, buf[:n])
	}
}
