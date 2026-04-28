package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
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

func (b *failingBackend) HasRecentActivity(uuid.UUID, time.Time) bool { return false }

// clientGoneBackend reports a successful AddClient but returns iface.ErrClientGone
// from SendData, simulating the post-disconnect race where the per-backend client
// map has already been cleared by EventDisconnect.
type clientGoneBackend struct {
	removeCalled atomic.Bool
}

func (b *clientGoneBackend) ID() string                                       { return "backend" }
func (b *clientGoneBackend) AddClient(net.Conn, uuid.UUID, string, bool) error { return nil }
func (b *clientGoneBackend) RemoveClient(uuid.UUID)                           { b.removeCalled.Store(true) }
func (b *clientGoneBackend) SendData(uuid.UUID, []byte) error                 { return iface.ErrClientGone }
func (b *clientGoneBackend) HasRecentActivity(uuid.UUID, time.Time) bool      { return false }

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

// When SendData returns iface.ErrClientGone, Start must exit cleanly AND log
// at INFO (not ERROR) level. The whole point of the sentinel is to suppress
// dashboard noise on the post-disconnect race; if a future refactor flips
// the log branches, this test catches it.
func TestClientStartExitsCleanlyOnErrClientGone(t *testing.T) {
	backend := &clientGoneBackend{}
	clientConn, userSide := net.Pipe()
	defer userSide.Close()

	var logBuf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	client := NewClient(clientConn, backend, nil, "example.com", true)

	done := make(chan struct{})
	go func() {
		client.Start()
		close(done)
	}()

	// Push a byte from the user side so Start's Read returns and SendData fires.
	_, err := userSide.Write([]byte("x"))
	if err != nil {
		t.Fatalf("user-side write: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit on ErrClientGone")
	}

	if !backend.removeCalled.Load() {
		t.Fatalf("expected RemoveClient via deferred cleanup")
	}

	out := logBuf.String()
	if !strings.Contains(out, "INFO:") || !strings.Contains(out, "backend disconnected") {
		t.Fatalf("expected INFO-level log mentioning backend disconnect, got: %q", out)
	}
	if strings.Contains(out, "ERROR:") {
		t.Fatalf("ErrClientGone must not log at ERROR level, got: %q", out)
	}
}
