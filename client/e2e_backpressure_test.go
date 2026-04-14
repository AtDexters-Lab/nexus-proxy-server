package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// pipeTCPConn wraps a net.Conn with fake TCP addresses so that
// backend.AddClient's LocalAddr type assertion to *net.TCPAddr succeeds.
type pipeTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (p *pipeTCPConn) LocalAddr() net.Addr  { return p.local }
func (p *pipeTCPConn) RemoteAddr() net.Addr { return p.remote }

func pipeTCPPair(port int) (*pipeTCPConn, *pipeTCPConn) {
	s, c := net.Pipe()
	local := &pipeTCPConn{
		Conn:   s,
		local:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
		remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 50000},
	}
	remote := &pipeTCPConn{
		Conn:   c,
		local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 50000},
		remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
	}
	return local, remote
}

type e2eStubValidator struct{}

func (e2eStubValidator) Validate(ctx context.Context, token string) (*auth.Claims, error) {
	return (&auth.Claims{BackendClaims: protocol.BackendClaims{Hostnames: []string{"example.com"}}}).Copy(), nil
}

// TestE2E_SlowConsumer_WriteChStaysWithinCreditWindow wires the real
// client.Client together with the real hub.Backend over an in-process
// WebSocket pair and drives a large data flow through a slow TCP
// consumer (simulating Chrome on a slow residential link downloading
// a file served by a local TCP backend). Under correct credit obedience
// the hub-side bufferedConn.writeCh MUST stay at or below
// DefaultCreditCapacity (64). This is the e2e reproducer for the prod
// "client write buffer full (128 pending)" symptom seen on
// piccoloroot.atdexters.com.
func TestE2E_SlowConsumer_WriteChStaysWithinCreditWindow(t *testing.T) {
	// ---------------- Local TCP "data source" ------------------------------
	// Plain TCP server that dumps `fileSize` bytes as soon as a connection
	// is accepted. Mimics the reverse direction of a file download where
	// the device-side local service pushes bytes into the nexus client as
	// fast as TCP will let it.
	// 16 MB is empirically the size needed for the e2e test to be
	// load-bearing: it sustains drain long enough for credit-leak drift
	// to accumulate past the 64-entry writeCh window when either of the
	// two fixed bugs (per-iter credit guard, race-branch decrement) is
	// reverted. Smaller sizes (tested down to 6 MB) complete before the
	// leak reaches the detection threshold and the test silently passes
	// on broken code. The narrow unit tests in drain_credit_window_test.go
	// cover the individual bug paths fast; this e2e confirms the
	// end-to-end invariant under sustained load.
	const fileSize = 16 * 1024 * 1024
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = appLn.Close() })
	go func() {
		for {
			c, err := appLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 16*1024)
				for i := range buf {
					buf[i] = byte(i)
				}
				remaining := fileSize
				for remaining > 0 {
					n := len(buf)
					if n > remaining {
						n = remaining
					}
					if _, err := conn.Write(buf[:n]); err != nil {
						return
					}
					remaining -= n
				}
			}(c)
		}
	}()
	appHostPort := appLn.Addr().String()

	// ---------------- WebSocket pair: hub <-> device client -----------------
	wsPairCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		wsPairCh <- conn
	}))
	t.Cleanup(wsSrv.Close)

	deviceWS, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsSrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	hubSideWS := <-wsPairCh

	// ---------------- Real hub.Backend on the server side ------------------
	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 60,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(hubSideWS, meta, cfg, e2eStubValidator{}, &http.Client{})

	var pumpsWg sync.WaitGroup
	pumpsWg.Add(1)
	go func() {
		defer pumpsWg.Done()
		b.StartPumps()
	}()
	t.Cleanup(func() {
		b.Close()
		pumpsWg.Wait()
	})

	// ---------------- Real client.Client on the device side ----------------
	dc := newTestClient(t)
	dc.config.PortMappings[443] = PortMapping{Default: appHostPort}
	dc.config.FlowControl = FlowControlConfig{
		LowWaterMark:  DefaultLowWaterMark,
		HighWaterMark: DefaultHighWaterMark,
		MaxBuffer:     DefaultMaxBuffer,
	}

	dc.wsMu.Lock()
	dc.ws = deviceWS
	dc.wsMu.Unlock()

	session := dc.beginSession()
	dc.wg.Add(2)
	go dc.writePump(session)
	go dc.readPump()
	t.Cleanup(func() {
		dc.cancel()
		_ = deviceWS.Close()
		dc.wg.Wait()
	})

	// ---------------- Slow "Chrome" drainer on hub client side ---------------
	localPipe, remotePipe := pipeTCPPair(443)
	clientID := uuid.New()
	if err := b.AddClient(localPipe, clientID, "example.com", false); err != nil {
		t.Fatalf("AddClient: %v", err)
	}

	// Slow consumer: read ~16 KB every 10 ms ≈ 1.6 MB/s — matches
	// residential-link Chrome downloads and the prod failure rate.
	var totalRead atomic.Int64
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		buf := make([]byte, 16*1024)
		for {
			_ = remotePipe.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := remotePipe.Read(buf)
			if n > 0 {
				totalRead.Add(int64(n))
			}
			if err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// ---------------- Sample writeCh depth every 5 ms ----------------------
	var peakDepth atomic.Int32
	var sampleCount atomic.Int32
	samplerStop := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-samplerStop:
				return
			case <-ticker.C:
				d, ok := b.ClientWriteBufferDepth(clientID)
				if !ok {
					continue
				}
				sampleCount.Add(1)
				for {
					cur := peakDepth.Load()
					if int32(d) <= cur {
						break
					}
					if peakDepth.CompareAndSwap(cur, int32(d)) {
						break
					}
				}
			}
		}
	}()

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if totalRead.Load() >= int64(fileSize) {
			break
		}
		select {
		case <-drainerDone:
			goto done
		case <-time.After(100 * time.Millisecond):
		}
	}
done:
	close(samplerStop)
	<-samplerDone

	_ = remotePipe.Close()
	_ = localPipe.Close()

	t.Logf("bytes delivered to slow consumer: %d / %d", totalRead.Load(), fileSize)
	t.Logf("writeCh depth peak: %d (credit window: %d, hard cap: %d) across %d samples",
		peakDepth.Load(), protocol.DefaultCreditCapacity, 2*protocol.DefaultCreditCapacity, sampleCount.Load())
	if int64(peakDepth.Load()) > protocol.DefaultCreditCapacity {
		t.Fatalf("writeCh peak %d exceeded credit window %d — prod overflow reproduced",
			peakDepth.Load(), protocol.DefaultCreditCapacity)
	}
}
