package proxy

import (
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
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
		// No need to explicitly clean the buffer before each read.
		// The io.Reader (here, net.Conn) will only fill up to n bytes,
		// and you only use buf[:n] for further processing.
		// Any leftover bytes from previous reads in buf[n:] are ignored.
		// So, it's safe to reuse pooled buffers without zeroing them.
		if err != nil {
			if err != io.EOF {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					log.Printf("INFO: Client %s idle timeout reached. Closing connection.", c.id)
					c.conn.Close()
				} else {
					log.Printf("ERROR: Failed to read from client %s: %v", c.id, err)
				}
			}
			// If we hit EOF or an error, we stop processing this client.
			break
		}

		// It is important to only read upto n bytes to avoid sending leftover data from previous reads.
		err = c.backend.SendData(c.id, buf[:n])
		if err != nil {
			log.Printf("ERROR: Failed to send data to backend %s for client %s: %v\nTerminating client connection", c.backend.ID(), c.id, err)
			break
		}
	}
}
