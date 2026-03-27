package proxy

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestPausableConnBlocksOnPause(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	pc := NewPausableConn(clientConn)
	defer pc.Close()

	// Write some data from the server side
	go func() {
		time.Sleep(50 * time.Millisecond)
		serverConn.Write([]byte("hello"))
	}()

	// Read should work when not paused
	buf := make([]byte, 10)
	n, err := pc.Read(buf)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(buf[:n]))
	}

	// Pause the connection
	pc.Pause()
	if !pc.IsPaused() {
		t.Fatal("expected connection to be paused")
	}

	// Read should now block
	readDone := make(chan struct{})
	go func() {
		pc.Read(buf)
		close(readDone)
	}()

	// Write data from server - but read should remain blocked
	go func() {
		time.Sleep(20 * time.Millisecond)
		serverConn.Write([]byte("world"))
	}()

	select {
	case <-readDone:
		t.Fatal("Read should have blocked while paused")
	case <-time.After(100 * time.Millisecond):
		// Expected - read is blocked
	}

	// Resume the connection
	pc.Resume()
	if pc.IsPaused() {
		t.Fatal("expected connection to be resumed")
	}

	// Now read should complete
	select {
	case <-readDone:
		// Expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read should have completed after resume")
	}
}

func TestPausableConnCloseWakesBlockedReaders(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	pc := NewPausableConn(clientConn)

	// Pause the connection
	pc.Pause()

	// Start a blocked read
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 10)
		_, err := pc.Read(buf)
		readErr <- err
	}()

	// Give the goroutine time to start blocking
	time.Sleep(50 * time.Millisecond)

	// Close the connection - should wake the reader
	pc.Close()

	select {
	case err := <-readErr:
		if err != net.ErrClosed {
			t.Fatalf("expected net.ErrClosed, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read should have been unblocked by Close")
	}
}

func TestPausableConnIdempotent(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	pc := NewPausableConn(clientConn)
	defer pc.Close()

	// Multiple pauses should be idempotent
	pc.Pause()
	pc.Pause()
	pc.Pause()
	if !pc.IsPaused() {
		t.Fatal("expected connection to be paused")
	}

	// Multiple resumes should be idempotent
	pc.Resume()
	pc.Resume()
	pc.Resume()
	if pc.IsPaused() {
		t.Fatal("expected connection to be resumed")
	}
}

func TestPausableConnPauseResumeAfterClose(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	pc := NewPausableConn(clientConn)
	pc.Close()

	// Pause after close should be a no-op
	pc.Pause()
	// This should not panic or cause issues

	// Resume after close should also be a no-op
	pc.Resume()
	// This should not panic or cause issues
}

func TestPausableConnConcurrentPauseResume(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	pc := NewPausableConn(clientConn)
	defer pc.Close()

	// Rapid concurrent pause/resume calls should not cause races
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			pc.Pause()
		}()
		go func() {
			defer wg.Done()
			pc.Resume()
		}()
	}
	wg.Wait()

	// Verify we can still use the connection
	pc.Resume()

	go func() {
		serverConn.Write([]byte("test"))
	}()

	buf := make([]byte, 10)
	n, err := pc.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "test" {
		t.Fatalf("expected 'test', got %q", string(buf[:n]))
	}
}

func TestPausableConnUnwrap(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	pc := NewPausableConn(clientConn)
	defer pc.Close()
	if pc.Unwrap() != clientConn {
		t.Fatal("Unwrap should return the underlying connection")
	}
}
