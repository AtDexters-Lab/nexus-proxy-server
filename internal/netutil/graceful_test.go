package netutil

import (
	"net"
	"testing"
	"time"
)

func TestGracefulCloseConnTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			defer conn.Close()
			buf := make([]byte, 64)
			conn.Read(buf) // block until remote closes
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	GracefulCloseConn(conn, nil, 50*time.Millisecond)
	// After grace period, conn should be fully closed
	time.Sleep(100 * time.Millisecond)

	_, err = conn.Write([]byte("test"))
	if err == nil {
		t.Fatal("expected error writing to closed conn")
	}
}

func TestGracefulCloseConnAbort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			defer conn.Close()
			buf := make([]byte, 64)
			conn.Read(buf)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	abort := make(chan struct{})
	GracefulCloseConn(conn, abort, 10*time.Second)
	close(abort) // abort immediately
	time.Sleep(50 * time.Millisecond)

	_, err = conn.Write([]byte("test"))
	if err == nil {
		t.Fatal("expected error writing to closed conn after abort")
	}
}
