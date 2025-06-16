package proxy

import (
	"bytes"
	"io"
	"net"
)

// PeekableConn is a wrapper around net.Conn that allows for "peeking" at the
// initial bytes of a connection without consuming them.
type PeekableConn struct {
	net.Conn
	// reader is the source for all read operations. It may be the original
	// connection or a MultiReader if a peek has occurred.
	reader io.Reader
}

// NewPeekableConn creates a new PeekableConn.
func NewPeekableConn(conn net.Conn) *PeekableConn {
	return &PeekableConn{Conn: conn, reader: conn}
}

// Peek reads n bytes from the connection and prepends them back onto the
// read stream, making the peek transparent to the final consumer.
// It is safe to call Peek multiple times.
func (c *PeekableConn) Peek(n int) ([]byte, error) {
	buf := make([]byte, n)
	// Use ReadFull to ensure we get exactly n bytes unless there's an error.
	// It reads from our internal reader, which could be the original conn or
	// already have peeked data at its head.
	readBytes, err := io.ReadFull(c.reader, buf)
	if err != nil {
		// Even on error (like EOF), we must prepend the partially read
		// data back to the stream.
		c.reader = io.MultiReader(bytes.NewReader(buf[:readBytes]), c.reader)
		return buf[:readBytes], err
	}

	// Prepend the bytes we just successfully read back to the reader stream.
	// This allows the next call to Read() or Peek() to see this data first.
	c.reader = io.MultiReader(bytes.NewReader(buf), c.reader)

	return buf, nil
}

// Read implements the io.Reader interface. It reads from our internal reader,
// which correctly accounts for any peeked data.
func (c *PeekableConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
