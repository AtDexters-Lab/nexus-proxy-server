package proxy

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
)

type failingBackend struct {
	addCalled    bool
	removeCalled bool
}

func (b *failingBackend) ID() string { return "backend" }

func (b *failingBackend) AddClient(_ net.Conn, _ uuid.UUID, _ string, _ bool) error {
	b.addCalled = true
	return fmt.Errorf("add-failed")
}

func (b *failingBackend) RemoveClient(uuid.UUID) { b.removeCalled = true }

func (b *failingBackend) SendData(uuid.UUID, []byte) error { return nil }

func TestClientStartClosesConnOnAddClientError(t *testing.T) {
	backend := &failingBackend{}
	clientConn, backendConn := net.Pipe()
	defer backendConn.Close()

	client := NewClient(clientConn, backend, nil, "example.com", true)
	client.Start()

	if !backend.addCalled {
		t.Fatalf("expected backend.AddClient to be called")
	}

	backendConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 1)
	_, err := backendConn.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected EOF after client closure, got %v", err)
	}

	if backend.removeCalled {
		t.Fatalf("expected RemoveClient not to be called when AddClient fails")
	}
}
