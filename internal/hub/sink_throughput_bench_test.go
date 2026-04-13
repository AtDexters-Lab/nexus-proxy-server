//go:build bench

package hub_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestSinkThroughputSweep measures per-client SINK throughput (TCP client →
// hub → device) across a range of simulated device consumption cadences.
// It reproduces Mowgli's reported throttle symptom in a controlled loopback
// environment so we can tell whether the cap is credit-replenishment-rate-
// bound or whether it originates in the writePump/WebSocket path.
//
// Modeling caveats:
//
//  1. Per-frame, not per-byte. The simulated device delays per frame using
//     time.Sleep, modeling a FIXED per-frame processing cost. It does NOT
//     model a per-byte link-rate cap — a real TCP-bound consumer (e.g. an
//     ESP32 over WiFi) has throughput proportional to bytes, not frames,
//     so frame-size sweeps in this bench appear to scale throughput
//     linearly, which is optimistic. When interpreting results for a real
//     device, first confirm whether the bottleneck is per-frame
//     (batch-processing) or per-byte (link-rate). Mowgli's 4KB→8KB test
//     showed a per-byte ceiling; this bench would not have reproduced it
//     without per-byte sleep modeling.
//
//  2. time.Sleep floor ≈ 1ms on Linux. Go's time.Sleep on a stock Linux
//     hrtimer coalesces short sleeps up to ~1ms (see
//     TestMeasureSleepGranularity in this file — 100us sleeps actually
//     take ~1.06ms). That means any readDelayPer under ~1ms in this bench
//     produces the same effective delay. The earlier sweep results at
//     100us, 500us, and 1ms all showed ~30 Mbps precisely because the
//     actual per-frame delay in all three cases was ~1ms. This is a
//     bench-harness artifact, NOT a hub throughput cap —
//     TestMeasureCreditCycleOverhead with readDelayPer=0 shows the real
//     hub ceiling at ~3 Gbps single-client with ~10us avgSendLat.
//
// Run:
//
//	go test -tags bench -run TestSinkThroughputSweep -v ./internal/hub/... -timeout 10m
func TestSinkThroughputSweep(t *testing.T) {
	cases := []sinkBenchCase{
		{name: "readDelay=0_burst", readDelayPer: 0, frameSize: 4096, totalFrames: 4096},
		{name: "readDelay=100us", readDelayPer: 100 * time.Microsecond, frameSize: 4096, totalFrames: 4096},
		{name: "readDelay=500us", readDelayPer: 500 * time.Microsecond, frameSize: 4096, totalFrames: 2048},
		{name: "readDelay=1ms", readDelayPer: 1 * time.Millisecond, frameSize: 4096, totalFrames: 2048},
		{name: "readDelay=5ms", readDelayPer: 5 * time.Millisecond, frameSize: 4096, totalFrames: 1024},
		{name: "readDelay=10ms", readDelayPer: 10 * time.Millisecond, frameSize: 4096, totalFrames: 512},
		{name: "readDelay=20ms", readDelayPer: 20 * time.Millisecond, frameSize: 4096, totalFrames: 256},
		{name: "readDelay=45ms_mowgli", readDelayPer: 45 * time.Millisecond, frameSize: 4096, totalFrames: 128},
		{name: "readDelay=0_frame8k", readDelayPer: 0, frameSize: 8192, totalFrames: 2048},
		{name: "readDelay=1ms_frame8k", readDelayPer: 1 * time.Millisecond, frameSize: 8192, totalFrames: 1024},
		{name: "readDelay=45ms_frame16k", readDelayPer: 45 * time.Millisecond, frameSize: 16384, totalFrames: 128},
		{name: "readDelay=45ms_frame32k", readDelayPer: 45 * time.Millisecond, frameSize: 32768, totalFrames: 128},
	}

	results := make([]sinkBenchResult, 0, len(cases))
	var resultsMu sync.Mutex
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := runSinkBench(t, tc)
			resultsMu.Lock()
			results = append(results, r)
			resultsMu.Unlock()
		})
	}

	// Final table — go test -v prints t.Log lines, making the sweep easy to
	// read without scrolling through per-subtest logs.
	t.Log("")
	t.Log("==========================================================================")
	t.Log("SINK throughput sweep")
	t.Log("==========================================================================")
	t.Logf("%-22s %6s %8s %10s %12s %12s %14s",
		"case", "frames", "frameB", "totalMiB", "elapsed", "Mbps", "avgSendLat")
	for _, r := range results {
		t.Logf("%-22s %6d %8d %10.2f %12s %12.2f %14s",
			r.name, r.frames, r.frameSize, r.mib,
			r.elapsed.Round(time.Millisecond),
			r.mbps, r.avgSendLat.Round(time.Microsecond))
	}
}

// TestSourceThroughputSweep measures per-client SOURCE throughput (device →
// hub → TCP client) across a range of simulated TCP consumption cadences.
// This is the symmetric counterpart to the SINK sweep: SOURCE goes through
// hub readPump → bufferedConn.drain → real TCP write, while SINK goes
// through SendData → writePump → WS. Comparing the two tells us whether
// the asymmetry Mowgli sees is structural or measurement artifact.
//
// Run:
//
//	go test -tags bench -run TestSourceThroughputSweep -v ./internal/hub/... -timeout 10m
func TestSourceThroughputSweep(t *testing.T) {
	cases := []sourceBenchCase{
		{name: "tcpDrain=0_burst", drainDelayPer: 0, frameSize: 4096, totalFrames: 4096},
		{name: "tcpDrain=100us", drainDelayPer: 100 * time.Microsecond, frameSize: 4096, totalFrames: 4096},
		{name: "tcpDrain=1ms", drainDelayPer: 1 * time.Millisecond, frameSize: 4096, totalFrames: 2048},
		{name: "tcpDrain=5ms", drainDelayPer: 5 * time.Millisecond, frameSize: 4096, totalFrames: 1024},
		{name: "tcpDrain=10ms", drainDelayPer: 10 * time.Millisecond, frameSize: 4096, totalFrames: 512},
		{name: "tcpDrain=20ms", drainDelayPer: 20 * time.Millisecond, frameSize: 4096, totalFrames: 256},
	}

	results := make([]sinkBenchResult, 0, len(cases))
	var resultsMu sync.Mutex
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := runSourceBench(t, tc)
			resultsMu.Lock()
			results = append(results, r)
			resultsMu.Unlock()
		})
	}

	t.Log("")
	t.Log("==========================================================================")
	t.Log("SOURCE throughput sweep")
	t.Log("==========================================================================")
	t.Logf("%-22s %6s %8s %10s %12s %12s",
		"case", "frames", "frameB", "totalMiB", "elapsed", "Mbps")
	for _, r := range results {
		t.Logf("%-22s %6d %8d %10.2f %12s %12.2f",
			r.name, r.frames, r.frameSize, r.mib,
			r.elapsed.Round(time.Millisecond), r.mbps)
	}
}

type sinkBenchCase struct {
	name         string
	readDelayPer time.Duration
	frameSize    int
	totalFrames  int
}

type sourceBenchCase struct {
	name          string
	drainDelayPer time.Duration
	frameSize     int
	totalFrames   int
}

type sinkBenchResult struct {
	name       string
	frames     int
	frameSize  int
	mib        float64
	elapsed    time.Duration
	mbps       float64
	avgSendLat time.Duration
}

func runSinkBench(t *testing.T, tc sinkBenchCase) sinkBenchResult {
	b, deviceWS, clientID, _, cleanup := setupBenchBackend(t)
	defer cleanup()

	// --- Device reader goroutine ---
	// Consumes data frames at the configured pace, simulating a device that
	// processes each frame in readDelayPer. Sends EventResumeStream back to
	// the hub after every CreditReplenishBatch consumed frames, exactly like
	// a credit-aware device library would.
	//
	// On the first EventConnect, sends the initial EventResumeStream to
	// install the hub-side forward credit semaphore, then sends a ping and
	// waits for the corresponding pong to prove the hub has drained past
	// that replenishment before the benchmark starts.
	var framesConsumed atomic.Int64
	initialGrantSent := make(chan struct{})
	pongReceived := make(chan struct{})
	readerStop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer func() { _ = recover() }()
		gotInitialConnect := false
		consumedInBatch := int64(0)
		for {
			select {
			case <-readerStop:
				return
			default:
			}
			_ = deviceWS.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			_, msg, err := deviceWS.ReadMessage()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}
			if len(msg) < 1 {
				continue
			}
			switch msg[0] {
			case protocol.ControlByteControl:
				var ctrl protocol.ControlMessage
				if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
					continue
				}
				if !gotInitialConnect && ctrl.Event == protocol.EventConnect && ctrl.ClientID == clientID {
					gotInitialConnect = true
					if err := writeDeviceControl(deviceWS, protocol.ControlMessage{
						Event:    protocol.EventResumeStream,
						ClientID: clientID,
						Credits:  protocol.DefaultCreditCapacity,
					}); err != nil {
						return
					}
					if err := writeDeviceControl(deviceWS, protocol.ControlMessage{
						Event:    protocol.EventPingClient,
						ClientID: clientID,
					}); err != nil {
						return
					}
					close(initialGrantSent)
					continue
				}
				if ctrl.Event == protocol.EventPongClient && ctrl.ClientID == clientID {
					select {
					case <-pongReceived:
					default:
						close(pongReceived)
					}
					continue
				}
			case protocol.ControlByteData:
				if len(msg) < 1+protocol.ClientIDLength {
					continue
				}
				if tc.readDelayPer > 0 {
					time.Sleep(tc.readDelayPer)
				}
				framesConsumed.Add(1)
				consumedInBatch++
				if consumedInBatch >= protocol.CreditReplenishBatch {
					if err := writeDeviceControl(deviceWS, protocol.ControlMessage{
						Event:    protocol.EventResumeStream,
						ClientID: clientID,
						Credits:  consumedInBatch,
					}); err != nil {
						return
					}
					consumedInBatch = 0
				}
			}
		}
	}()
	defer func() {
		select {
		case <-readerStop:
		default:
			close(readerStop)
		}
		<-readerDone
	}()

	select {
	case <-initialGrantSent:
	case <-time.After(5 * time.Second):
		t.Fatal("device never sent initial replenishment")
	}
	select {
	case <-pongReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("device never received pong; hub did not process initial EventResumeStream")
	}

	// --- Sender: call b.SendData in a tight loop and measure timing ---
	payload := make([]byte, tc.frameSize)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}

	start := time.Now()
	var totalLat time.Duration
	for i := 0; i < tc.totalFrames; i++ {
		callStart := time.Now()
		if err := b.SendData(clientID, payload); err != nil {
			t.Fatalf("SendData failed at frame %d: %v", i, err)
		}
		totalLat += time.Since(callStart)
	}
	sendLoopElapsed := time.Since(start)

	// Wait for all frames to be consumed by the device so throughput
	// accounting reflects end-to-end, not just SendData enqueue latency.
	consumeDeadline := time.Now().Add(60 * time.Second)
	for framesConsumed.Load() < int64(tc.totalFrames) {
		if time.Now().After(consumeDeadline) {
			t.Logf("WARN: only %d/%d frames consumed after 60s",
				framesConsumed.Load(), tc.totalFrames)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	totalBytes := int64(tc.totalFrames) * int64(tc.frameSize)
	secs := elapsed.Seconds()
	mbps := 0.0
	if secs > 0 {
		mbps = float64(totalBytes*8) / (secs * 1e6)
	}
	mib := float64(totalBytes) / (1 << 20)

	t.Logf("sendLoop=%s totalWithDrain=%s sendAvgLat=%s",
		sendLoopElapsed.Round(time.Millisecond),
		elapsed.Round(time.Millisecond),
		(totalLat / time.Duration(tc.totalFrames)).Round(time.Microsecond))

	return sinkBenchResult{
		name:       tc.name,
		frames:     tc.totalFrames,
		frameSize:  tc.frameSize,
		mib:        mib,
		elapsed:    elapsed,
		mbps:       mbps,
		avgSendLat: totalLat / time.Duration(tc.totalFrames),
	}
}

func runSourceBench(t *testing.T, tc sourceBenchCase) sinkBenchResult {
	// SOURCE direction needs a drainable remote end so bufferedConn.drain
	// can make forward progress. setupBenchBackend already registered a
	// client and stashed the remote end in remoteConn; we use that.
	b, deviceWS, clientID, remoteConn, cleanup := setupBenchBackend(t)
	defer cleanup()
	_ = b // unused here — the sender pushes data through the WS directly

	var framesReceived atomic.Int64
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		buf := make([]byte, tc.frameSize*4)
		remaining := int64(tc.totalFrames) * int64(tc.frameSize)
		for remaining > 0 {
			_ = remoteConn.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, err := remoteConn.Read(buf)
			if err != nil {
				return
			}
			remaining -= int64(n)
			framesReceived.Add(1)
			if tc.drainDelayPer > 0 {
				time.Sleep(tc.drainDelayPer)
			}
		}
	}()

	// We don't need the ping-sync dance here because SOURCE direction does
	// not go through acquireForwardCredit on the hub — the device is the
	// sender and credit gating lives on the client-library side (which we
	// simulate with a credit-aware send loop below).
	//
	// Wait briefly for EventConnect to be delivered so we know the semaphore
	// budget before starting.
	var initialCredits int64
	initialDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(initialDeadline) {
		_ = deviceWS.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, msg, err := deviceWS.ReadMessage()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			t.Fatalf("read EventConnect: %v", err)
		}
		if len(msg) >= 1 && msg[0] == protocol.ControlByteControl {
			var ctrl protocol.ControlMessage
			if json.Unmarshal(msg[1:], &ctrl) == nil && ctrl.Event == protocol.EventConnect && ctrl.ClientID == clientID {
				initialCredits = ctrl.Credits
				break
			}
		}
	}
	if initialCredits == 0 {
		t.Fatal("never saw EventConnect with initial credits")
	}

	// Background goroutine to consume replenishments the hub sends back
	// (every CreditReplenishBatch frames written to the TCP client).
	creditsCh := make(chan int64, 1024)
	creditReaderStop := make(chan struct{})
	creditReaderDone := make(chan struct{})
	go func() {
		defer close(creditReaderDone)
		defer func() { _ = recover() }()
		for {
			select {
			case <-creditReaderStop:
				return
			default:
			}
			_ = deviceWS.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			_, msg, err := deviceWS.ReadMessage()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}
			if len(msg) < 2 || msg[0] != protocol.ControlByteControl {
				continue
			}
			var ctrl protocol.ControlMessage
			if err := json.Unmarshal(msg[1:], &ctrl); err != nil {
				continue
			}
			if ctrl.Event == protocol.EventResumeStream && ctrl.ClientID == clientID && ctrl.Credits > 0 {
				select {
				case creditsCh <- ctrl.Credits:
				case <-creditReaderStop:
					return
				}
			}
		}
	}()
	defer func() {
		select {
		case <-creditReaderStop:
		default:
			close(creditReaderStop)
		}
		<-creditReaderDone
	}()

	// Credit-aware sender (device side): write data frames into the WS,
	// respecting the credit budget exactly like the real client library.
	payload := make([]byte, tc.frameSize)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}
	available := initialCredits
	start := time.Now()
	for i := 0; i < tc.totalFrames; i++ {
		for available <= 0 {
			select {
			case n := <-creditsCh:
				available += n
			case <-time.After(30 * time.Second):
				t.Fatalf("SOURCE sender stalled at frame %d/%d", i, tc.totalFrames)
			}
		}
		// Drain any extra pending credits.
		for {
			select {
			case n := <-creditsCh:
				available += n
				continue
			default:
			}
			break
		}
		dataMsg := make([]byte, 0, 1+protocol.ClientIDLength+tc.frameSize)
		dataMsg = append(dataMsg, protocol.ControlByteData)
		dataMsg = append(dataMsg, clientID[:]...)
		dataMsg = append(dataMsg, payload...)
		if err := deviceWS.WriteMessage(websocket.BinaryMessage, dataMsg); err != nil {
			t.Fatalf("device WriteMessage: %v", err)
		}
		available--
	}

	select {
	case <-drainerDone:
	case <-time.After(60 * time.Second):
		t.Fatalf("SOURCE drainer did not receive all bytes: received=%d frames",
			framesReceived.Load())
	}
	elapsed := time.Since(start)

	totalBytes := int64(tc.totalFrames) * int64(tc.frameSize)
	secs := elapsed.Seconds()
	mbps := 0.0
	if secs > 0 {
		mbps = float64(totalBytes*8) / (secs * 1e6)
	}
	mib := float64(totalBytes) / (1 << 20)

	return sinkBenchResult{
		name:      tc.name,
		frames:    tc.totalFrames,
		frameSize: tc.frameSize,
		mib:       mib,
		elapsed:   elapsed,
		mbps:      mbps,
	}
}

// setupBenchBackend spins up a hub Backend connected over an in-process
// WebSocket pair, starts the pumps, registers an inbound client using a
// synchronous pipe, and returns the backend, the device-side WS, the
// clientID, and the remote end of the TCP pipe.
//
// SINK benchmarks ignore the remote TCP end (SINK data flows toward the
// device, not through the TCP pipe). SOURCE benchmarks drain the remote
// end so the hub's bufferedConn.drain can make forward progress.
func setupBenchBackend(t *testing.T) (*hub.Backend, *websocket.Conn, uuid.UUID, net.Conn, func()) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		serverConnCh <- conn
	}))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	deviceWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	hubSideWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 120,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(hubSideWS, meta, cfg, stubValidator{}, &http.Client{})

	var pumpWg sync.WaitGroup
	pumpWg.Add(1)
	go func() {
		defer pumpWg.Done()
		b.StartPumps()
	}()

	clientID := uuid.New()
	localConn, remoteConn := pipeTCPPair(443)
	require.NoError(t, b.AddClient(localConn, clientID, "example.com", false))

	cleanup := func() {
		b.Close()
		pumpWg.Wait()
		_ = deviceWS.Close()
		_ = localConn.Close()
		_ = remoteConn.Close()
		ts.Close()
	}
	return b, deviceWS, clientID, remoteConn, cleanup
}

// TestMeasureSleepGranularity probes the actual duration of short
// time.Sleep calls to determine whether the ~1ms throughput floor seen in
// TestSinkThroughputSweep (where 100us, 500us, and 1ms readDelay all
// produce ~30 Mbps) is a Go runtime measurement artifact or a real cap
// somewhere in the credit round-trip path.
//
// If time.Sleep(100us) actually takes ~1ms, the floor is a test artifact
// and there is nothing to fix in production. If time.Sleep is accurate
// (~100us as requested), the floor lives somewhere else — channel wakeup
// on the forwardCredits semaphore, gorilla WS roundtrip, or writePump
// select — and warrants a second round of investigation.
//
// Run:
//
//	go test -tags bench -run TestMeasureSleepGranularity -v ./internal/hub/...
func TestMeasureSleepGranularity(t *testing.T) {
	durations := []time.Duration{
		10 * time.Microsecond,
		50 * time.Microsecond,
		100 * time.Microsecond,
		200 * time.Microsecond,
		500 * time.Microsecond,
		1 * time.Millisecond,
		2 * time.Millisecond,
		5 * time.Millisecond,
	}
	iters := 1000
	t.Log("")
	t.Log("=============================================================")
	t.Log("time.Sleep granularity probe")
	t.Log("=============================================================")
	t.Logf("%-15s %-15s %-15s %-10s",
		"requested", "avg actual", "total", "ratio")
	for _, d := range durations {
		start := time.Now()
		for i := 0; i < iters; i++ {
			time.Sleep(d)
		}
		total := time.Since(start)
		avg := total / time.Duration(iters)
		ratio := float64(avg) / float64(d)
		t.Logf("%-15s %-15s %-15s %-10.2fx",
			d, avg.Round(time.Microsecond), total.Round(time.Millisecond), ratio)
	}
}

// TestMeasureCreditCycleOverhead isolates the per-credit-cycle cost in
// the hub's forward-credit path without any simulated device delay.
// This separates "base cost of a credit cycle" from "device consumption
// cost" so we can tell whether the ~1ms observed floor is dominated by
// time.Sleep, by channel wakeup, or by the gorilla WS write path.
//
// The setup: sender calls b.SendData in a tight loop; device reads and
// replenishes with ZERO sleep. Whatever latency appears per SendData call
// is pure hub + WS overhead.
//
// Run:
//
//	go test -tags bench -run TestMeasureCreditCycleOverhead -v ./internal/hub/...
func TestMeasureCreditCycleOverhead(t *testing.T) {
	cases := []sinkBenchCase{
		{name: "zero_delay_1024f", readDelayPer: 0, frameSize: 4096, totalFrames: 1024},
		{name: "zero_delay_4096f", readDelayPer: 0, frameSize: 4096, totalFrames: 4096},
		{name: "zero_delay_16384f", readDelayPer: 0, frameSize: 4096, totalFrames: 16384},
	}
	results := make([]sinkBenchResult, 0, len(cases))
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			results = append(results, runSinkBench(t, tc))
		})
	}
	t.Log("")
	t.Log("=============================================================")
	t.Log("credit-cycle base cost (zero device delay)")
	t.Log("=============================================================")
	t.Logf("%-20s %8s %12s %14s %14s",
		"case", "frames", "elapsed", "Mbps", "avgSendLat")
	for _, r := range results {
		t.Logf("%-20s %8d %12s %14.2f %14s",
			r.name, r.frames, r.elapsed.Round(time.Millisecond),
			r.mbps, r.avgSendLat.Round(time.Microsecond))
	}
}

func writeDeviceControl(ws *websocket.Conn, ctrl protocol.ControlMessage) error {
	payload, err := json.Marshal(ctrl)
	if err != nil {
		return fmt.Errorf("marshal control: %w", err)
	}
	msg := make([]byte, 0, 1+len(payload))
	msg = append(msg, protocol.ControlByteControl)
	msg = append(msg, payload...)
	return ws.WriteMessage(websocket.BinaryMessage, msg)
}
