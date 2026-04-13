package hub_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/bandwidth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newBandwidthTestBackend stands up a Backend with a real bandwidth.Scheduler
// attached via AttachBandwidthScheduler (exercising the production
// registration path, not a test-only shortcut). Returns the backend, the
// client-side of the WebSocket pair (used by the test to push data as if
// the remote backend were sending), a cleanup closure, and the scheduler.
func newBandwidthTestBackend(t *testing.T, mbps int) (b *hub.Backend, clientWS *websocket.Conn, scheduler *bandwidth.Scheduler, cleanup func()) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		serverConnCh <- conn
	}))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	backendWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 30,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b = hub.NewBackend(backendWS, meta, cfg, stubValidator{}, &http.Client{})

	scheduler = bandwidth.NewScheduler(mbps)
	b.AttachBandwidthScheduler(scheduler)

	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()

	cleanup = func() {
		b.Close()
		pumpWg.Wait()
		clientWS.Close()
		scheduler.Stop()
		ts.Close()
	}
	return b, clientWS, scheduler, cleanup
}

// buildBandwidthTestDataMsg assembles a ControlByteData frame with the
// given client ID and payload, matching the wire format handleBinaryMessage
// expects.
func buildBandwidthTestDataMsg(clientID uuid.UUID, payload []byte) []byte {
	msg := make([]byte, 0, 1+protocol.ClientIDLength+len(payload))
	msg = append(msg, protocol.ControlByteData)
	msg = append(msg, clientID[:]...)
	msg = append(msg, payload...)
	return msg
}

// drainPipe continuously reads from conn into a channel, so writes on the
// far side never block. The returned channel receives one slice per Read
// call. Closes when the connection closes.
func drainPipe(conn net.Conn) chan []byte {
	ch := make(chan []byte, 1024)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				close(ch)
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			ch <- data
		}
	}()
	return ch
}

// TestReadPump_BandwidthHOL_FastConsumerTightGate is the direct
// regression guard for the HOL fix. Two clients share one backend
// under a very tight bandwidth cap (1 Mbps). Client A is pushed hard
// enough to sit in the gate wait continuously. Client B receives a
// small burst and must see its data arrive promptly, proving that
// A's gate block is localized to A's drain goroutine and does not
// stall readPump ingress for siblings on the same backend.
//
// Pre-fix behavior: handleBinaryMessage called RequestSend on the
// shared readPump goroutine → A's wait stalled all ingress → B
// would time out.
// Post-fix behavior: gate runs in A's per-client drain → B's
// drain runs independently → B's data arrives within ms.
func TestReadPump_BandwidthHOL_FastConsumerTightGate(t *testing.T) {
	t.Parallel()

	const capMbps = 1 // 125 KB/s
	b, clientWS, _, cleanup := newBandwidthTestBackend(t, capMbps)
	defer cleanup()

	// Client A: fast consumer (drained promptly). A's drain will hit
	// the gate because we push much faster than the cap.
	clientAID := uuid.New()
	aLocal, aRemote := pipeTCPPair(443)
	defer aRemote.Close()
	defer aLocal.Close()
	require.NoError(t, b.AddClient(aLocal, clientAID, "example.com", false))
	aReceived := drainPipe(aRemote)

	// Client B: also fast. We send a small burst and measure latency.
	clientBID := uuid.New()
	bLocal, bRemote := pipeTCPPair(443)
	defer bRemote.Close()
	defer bLocal.Close()
	require.NoError(t, b.AddClient(bLocal, clientBID, "example.com", false))
	bReceived := drainPipe(bRemote)

	// Warm-up: burn through the scheduler's initial 128 KB minDeficitCap
	// on client A so subsequent A-frames actually hit the gate. Also
	// kick the backend out of the "idle" state for deficit accumulation.
	warmupPayload := make([]byte, 16*1024) // 16 KB per frame
	for i := 0; i < 10; i++ {              // 160 KB total, past minDeficitCap
		require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage,
			buildBandwidthTestDataMsg(clientAID, warmupPayload)))
	}

	// Drain the warmup frames off client A's pipe.
	warmupDeadline := time.After(3 * time.Second)
	warmupReceived := 0
	for warmupReceived < 10 {
		select {
		case data, ok := <-aReceived:
			if !ok {
				t.Fatal("client A closed during warmup")
			}
			warmupReceived += len(data) / (16 * 1024)
			if len(data)%(16*1024) != 0 {
				// Partial — count as one progress step toward the goal.
				warmupReceived++
			}
		case <-warmupDeadline:
			t.Fatalf("warmup stalled after %d frames", warmupReceived)
		}
	}

	// Sustained pressure on A: 20 more 16 KB frames. At 125 KB/s cap,
	// drain processes ~8 frames/sec, so these 20 frames take ~2.5s to
	// clear — plenty of time for A's drain to be stuck in the gate.
	for i := 0; i < 20; i++ {
		require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage,
			buildBandwidthTestDataMsg(clientAID, warmupPayload)))
	}

	// Give readPump a moment to enqueue the burst.
	time.Sleep(50 * time.Millisecond)

	// Now send a small B frame. Pre-fix: this blocks behind A's
	// gate wait on readPump → never arrives in 500ms.
	// Post-fix: readPump enqueues it into B's writeCh immediately,
	// B's drain runs independently.
	bPayload := []byte("HOL_PROBE")
	sendTime := time.Now()
	require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage,
		buildBandwidthTestDataMsg(clientBID, bPayload)))

	// Assert: B receives its probe within 500ms.
	select {
	case data, ok := <-bReceived:
		if !ok {
			t.Fatal("client B closed unexpectedly")
		}
		elapsed := time.Since(sendTime)
		require.Equal(t, bPayload, data,
			"B's payload must arrive intact")
		require.Less(t, elapsed, 500*time.Millisecond,
			"B's small frame should not be blocked by A's gate wait")
	case <-time.After(2 * time.Second):
		t.Fatal("HOL: B's probe did not arrive within 2s — " +
			"A's bandwidth gate is still stalling readPump")
	}

	// BandwidthGateStats sanity: after sustained pressure on A, the
	// backend's gate-block counters must be non-zero (events tracks
	// frames that were blocked at least once; millisTotal accumulates
	// wait time). Verifies the gateBlockObserver callback wiring from
	// drain → Backend.observeGateBlock → backendMetrics is healthy.
	require.Eventually(t, func() bool {
		events, millis := b.BandwidthGateStats()
		return events > 0 && millis > 0
	}, 2*time.Second, 20*time.Millisecond,
		"BandwidthGateStats must reflect drain-side gate blocks under a tight cap")
}

// TestReadPump_BandwidthHOL_SlowConsumerPlusTightGate combines the
// v0.3.9 slow-consumer HOL fix and the v0.4.x gate HOL fix: client A
// has a slow TCP consumer AND a tight bandwidth cap. Client B must
// still be unaffected. This verifies both HOL fixes compose correctly.
func TestReadPump_BandwidthHOL_SlowConsumerPlusTightGate(t *testing.T) {
	t.Parallel()

	b, clientWS, _, cleanup := newBandwidthTestBackend(t, 1)
	defer cleanup()

	// Client A: slow consumer — NEVER read from aRemote. A's drain
	// will block on TCP write after exhausting the gate, and A's
	// writeCh will fill up. Pre-v0.3.9 HOL: readPump would block on
	// A's synchronous pipe write. Post-v0.3.9 HOL: readPump enqueues
	// to A's writeCh and returns.
	clientAID := uuid.New()
	aLocal, aRemote := pipeTCPPair(443)
	defer aRemote.Close()
	defer aLocal.Close()
	require.NoError(t, b.AddClient(aLocal, clientAID, "example.com", false))

	// Client B: fast, fully drained.
	clientBID := uuid.New()
	bLocal, bRemote := pipeTCPPair(443)
	defer bRemote.Close()
	defer bLocal.Close()
	require.NoError(t, b.AddClient(bLocal, clientBID, "example.com", false))
	bReceived := drainPipe(bRemote)

	// Push one frame to A. It will sit in A's writeCh (v0.3.9 fix
	// prevents readPump blocking) and A's drain will eventually block
	// on the gate or on the TCP write — either way, isolated.
	require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage,
		buildBandwidthTestDataMsg(clientAID, []byte("stalled_A"))))

	// Small pause for readPump to dispatch.
	time.Sleep(30 * time.Millisecond)

	// Send B's probe.
	bPayload := []byte("probe_B")
	sendTime := time.Now()
	require.NoError(t, clientWS.WriteMessage(websocket.BinaryMessage,
		buildBandwidthTestDataMsg(clientBID, bPayload)))

	select {
	case data, ok := <-bReceived:
		if !ok {
			t.Fatal("client B closed unexpectedly")
		}
		require.Equal(t, bPayload, data)
		require.Less(t, time.Since(sendTime), 500*time.Millisecond,
			"B must not be blocked by A's slow consumer + gate")
	case <-time.After(2 * time.Second):
		t.Fatal("HOL: B's probe never arrived")
	}
}

// TestBandwidthGate_RegistrationGoesThroughProductionPath asserts that
// AttachBandwidthScheduler actually registers the backend with the
// scheduler's internal map (not just writes the field). Without the
// Register side effect, RequestSend takes the "unknown backend" path
// and returns (true, 0) — silently disabling bandwidth caps in
// production while every test that uses a test helper still passes.
// This test exercises the production path directly.
//
// Explicitly uses NewScheduler(1) (not 0) so totalBytesPerSecond > 0;
// a zero-cap scheduler would pass the test via the "unlimited" path
// at scheduler.go:171 without actually proving registration.
func TestBandwidthGate_RegistrationGoesThroughProductionPath(t *testing.T) {
	t.Parallel()

	b, _, scheduler, cleanup := newBandwidthTestBackend(t, 1)
	defer cleanup()

	// Request more than any single deficit refill can satisfy. If
	// the backend was registered, RequestSend hits the per-backend
	// deficit path and returns (false, wait). If the backend was
	// NOT registered, RequestSend takes the "unknown backend" path
	// at scheduler.go:177 and returns (true, 0).
	allowed, _ := scheduler.RequestSend(b.ID(), 10_000_000) // 10 MB
	require.False(t, allowed,
		"RequestSend must return false after AttachBandwidthScheduler — "+
			"if true, the backend was never registered (Register side effect was lost)")
}

// TestAttachBandwidthScheduler_RejectsDoubleAttach verifies the
// iteration-2 red-team AR1 safeguard: calling AttachBandwidthScheduler
// twice on the same backend panics instead of silently resetting the
// in-flight BackendBandwidth entry.
func TestAttachBandwidthScheduler_RejectsDoubleAttach(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{BackendsJWTSecret: "secret", IdleTimeoutSeconds: 30}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(nil, meta, cfg, stubValidator{}, &http.Client{})

	s1 := bandwidth.NewScheduler(1)
	defer s1.Stop()
	b.AttachBandwidthScheduler(s1)

	require.Panics(t, func() {
		s2 := bandwidth.NewScheduler(1)
		defer s2.Stop()
		b.AttachBandwidthScheduler(s2)
	}, "double AttachBandwidthScheduler must panic")
}

// TestAttachBandwidthScheduler_RejectsAttachAfterAddClient verifies the
// "attach must precede AddClient" safeguard: mixing gated and ungated
// clients on one backend is disallowed.
func TestAttachBandwidthScheduler_RejectsAttachAfterAddClient(t *testing.T) {
	t.Parallel()

	// Need a real WS pair so AddClient can send EventConnect.
	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := upgrader.Upgrade(w, r, nil)
		serverConnCh <- conn
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer clientWS.Close()
	backendWS := <-serverConnCh

	cfg := &config.Config{BackendsJWTSecret: "secret", IdleTimeoutSeconds: 30}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(backendWS, meta, cfg, stubValidator{}, &http.Client{})

	// Start pumps so SendControlMessage (called by AddClient) can drain.
	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()
	defer func() {
		b.Close()
		pumpWg.Wait()
	}()

	// Reader goroutine to drain the backend WS side (so SendControlMessage
	// from AddClient doesn't back up).
	go func() {
		for {
			if _, _, err := clientWS.ReadMessage(); err != nil {
				return
			}
		}
	}()

	clientID := uuid.New()
	local, remote := pipeTCPPair(443)
	defer remote.Close()
	defer local.Close()

	require.NoError(t, b.AddClient(local, clientID, "example.com", false))

	require.Panics(t, func() {
		s := bandwidth.NewScheduler(1)
		defer s.Stop()
		b.AttachBandwidthScheduler(s)
	}, "AttachBandwidthScheduler after AddClient must panic")
}

// TestBandwidthGate_MixedFrameSizesLiveness is the disclosed-regression
// liveness guard for §9 of the RFC. Under sustained contention with
// asymmetric frame sizes, large-frame clients may see higher tail
// latency (known, accepted semantic change) — but must NOT be starved
// to zero throughput. This test verifies both streams deliver at
// least some progress within a reasonable window. It is intentionally
// NOT a fairness bound.
func TestBandwidthGate_MixedFrameSizesLiveness(t *testing.T) {
	t.Parallel()

	// 4 Mbps = 500 KB/s cap. Offered total below cap so the gate
	// engages occasionally but neither stream is adversarially
	// starved — this is a LIVENESS guard, not a fairness bound.
	// §9 of the RFC explicitly disclaims per-client fair-share
	// under contention.
	b, clientWS, _, cleanup := newBandwidthTestBackend(t, 4)
	defer cleanup()

	// Client A: small frames. Client B: large frames.
	clientAID := uuid.New()
	aLocal, aRemote := pipeTCPPair(443)
	defer aRemote.Close()
	defer aLocal.Close()
	require.NoError(t, b.AddClient(aLocal, clientAID, "example.com", false))
	aReceived := drainPipe(aRemote)

	clientBID := uuid.New()
	bLocal, bRemote := pipeTCPPair(443)
	defer bRemote.Close()
	defer bLocal.Close()
	require.NoError(t, b.AddClient(bLocal, clientBID, "example.com", false))
	bReceived := drainPipe(bRemote)

	smallPayload := make([]byte, 100)
	largePayload := make([]byte, 16*1024)

	stop := make(chan struct{})

	// Single pump goroutine. gorilla/websocket panics on concurrent
	// WriteMessage, so a single goroutine serializes sends with
	// conservative offered rates:
	//   A: 50 frames/s × 100 B = 5 KB/s
	//   B: 10 frames/s × 16 KB = 160 KB/s
	// Total: ~165 KB/s, well under the 500 KB/s cap. Gate still
	// occasionally engages (minDeficitCap is 128 KB, one large
	// frame already consumes 12.5% of available deficit), which
	// exercises the drain-side gate path without starving either
	// stream.
	go func() {
		aTicker := time.NewTicker(20 * time.Millisecond) // 50/s
		bTicker := time.NewTicker(100 * time.Millisecond) // 10/s
		defer aTicker.Stop()
		defer bTicker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-aTicker.C:
				_ = clientWS.WriteMessage(websocket.BinaryMessage,
					buildBandwidthTestDataMsg(clientAID, smallPayload))
			case <-bTicker.C:
				_ = clientWS.WriteMessage(websocket.BinaryMessage,
					buildBandwidthTestDataMsg(clientBID, largePayload))
			}
		}
	}()

	// Run for 5 seconds collecting frames.
	aFrames := 0
	bFrames := 0
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case data, ok := <-aReceived:
			if !ok {
				break collect
			}
			aFrames += len(data) / len(smallPayload)
		case data, ok := <-bReceived:
			if !ok {
				break collect
			}
			bFrames += len(data) / len(largePayload)
		case <-deadline:
			break collect
		}
	}
	close(stop)

	// Liveness: each stream must deliver >0 frames in 5s. Not a
	// fairness bound — just "no zero-throughput starvation".
	require.Greater(t, aFrames, 0, "client A (small-frame) must make progress")
	require.Greater(t, bFrames, 0, "client B (large-frame) must make progress")
}
