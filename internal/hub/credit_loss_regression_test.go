package hub_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestCreditLossRegression_SustainedSlowConsumer reproduces the team's
// reported scenario: a credit-aware backend pushing sustained data through
// the relay to a slow consumer (simulating ESP32 over WiFi). The transfer
// is 512 frames — 8x the initial credit window of 64 — so it must traverse
// the credit replenishment loop many times. Without the credit-preservation
// fix, lost replenishments would eventually starve the sender. With the fix,
// all 512 frames arrive.
//
// The "backend" in this test simulates exactly what nexus-proxy/client lib
// does: starts with DefaultCreditCapacity credits, decrements per frame
// sent, blocks when credits hit 0, replenishes from EventResumeStream
// messages received from the relay.
//
// This is the regression guard for the silent credit-loss bug.
func TestCreditLossRegression_SustainedSlowConsumer(t *testing.T) {
	t.Parallel()

	// Set up a backend WS pair (the relay-side hub Backend ↔ a fake "backend"
	// that will push data through the WS in a credit-aware manner).
	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		serverConnCh <- conn
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	backendWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer backendWS.Close()
	hubSideWS := <-serverConnCh

	cfg := &config.Config{
		BackendsJWTSecret:  "secret",
		IdleTimeoutSeconds: 30,
	}
	meta := &hub.AttestationMetadata{Hostnames: []string{"example.com"}, Weight: 1}
	b := hub.NewBackend(hubSideWS, meta, cfg, stubValidator{}, &http.Client{})

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

	// Add a SLOW inbound client. net.Pipe is synchronous, and we pace the
	// reader to ~1ms per frame to simulate an ESP32 over WiFi. This forces
	// the relay's bufferedConn drain() to block on each TCP write,
	// exercising the credit replenishment loop many times during the
	// transfer — exactly where the original credit-loss bug lived.
	clientID := uuid.New()
	slowLocal, slowRemote := pipeTCPPair(443)
	defer slowRemote.Close()
	defer slowLocal.Close()
	require.NoError(t, b.AddClient(slowLocal, clientID, "example.com", false))

	// Channel for credits granted by the relay back to our fake backend.
	// The reader goroutine below populates this from incoming EventResumeStream
	// frames; the credit-aware sender consumes from it.
	creditsCh := make(chan int64, 256)

	// Reader goroutine: drains the backend WS for control messages.
	// Initial EventConnect carries DefaultCreditCapacity; subsequent
	// EventResumeStream frames carry replenishment counts.
	//
	// gorilla/websocket panics on ReadMessage after the underlying
	// connection has been closed (e.g., during test teardown). We guard
	// the reader with a recover so a teardown race never crashes the test.
	readerStop := make(chan struct{})
	readerDone := make(chan struct{})
	var initialCreditsCh = make(chan int64, 1)
	go func() {
		defer close(readerDone)
		defer func() { _ = recover() }() // gorilla teardown panic guard
		gotInitial := false
		for {
			select {
			case <-readerStop:
				return
			default:
			}
			backendWS.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, msg, err := backendWS.ReadMessage()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return // hard error — stop reading
			}
			if len(msg) < 2 || msg[0] != protocol.ControlByteControl {
				continue
			}
			ctrl := parseControlMessage(t, msg[1:])
			if ctrl.ClientID != clientID {
				continue
			}
			if !gotInitial && ctrl.Event == protocol.EventConnect {
				gotInitial = true
				initialCreditsCh <- ctrl.Credits
				continue
			}
			if ctrl.Event == protocol.EventResumeStream && ctrl.Credits > 0 {
				select {
				case creditsCh <- ctrl.Credits:
				case <-readerStop:
					return
				}
			}
		}
	}()
	defer func() {
		// Always tear down the reader before the test exits, even on
		// t.Fatal, so the goroutine can't outlive the connection.
		select {
		case <-readerStop:
		default:
			close(readerStop)
		}
		<-readerDone
	}()

	// Wait for the initial credit grant from EventConnect.
	var availableCredits int64
	select {
	case n := <-initialCreditsCh:
		availableCredits = n
	case <-time.After(2 * time.Second):
		t.Fatal("never received initial credit grant from EventConnect")
	}
	require.Equal(t, protocol.DefaultCreditCapacity, availableCredits,
		"EventConnect must carry initial credit grant")

	// Slow consumer: drain the inbound TCP pipe at a paced rate.
	const totalFrames = 512 // 8x the initial credit window
	const frameSize = 256
	const slowReadDelay = 1 * time.Millisecond

	consumed := make([]byte, 0, totalFrames*frameSize)
	var consumedMu sync.Mutex
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		buf := make([]byte, frameSize*4)
		for {
			consumedMu.Lock()
			done := len(consumed) >= totalFrames*frameSize
			consumedMu.Unlock()
			if done {
				return
			}
			slowRemote.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, err := slowRemote.Read(buf)
			if err != nil {
				return
			}
			consumedMu.Lock()
			consumed = append(consumed, buf[:n]...)
			consumedMu.Unlock()
			time.Sleep(slowReadDelay) // pace the consumer
		}
	}()

	// Credit-aware sender: send totalFrames, blocking on availableCredits
	// just like the real nexus-proxy/client lib's drainConnectionQueue.
	// This is the FAITHFUL simulation — the relay must keep replenishment
	// flowing or the sender stalls.
	payload := make([]byte, frameSize)
	for i := 0; i < frameSize; i++ {
		payload[i] = byte(i & 0xff)
	}
	sendDeadline := time.Now().Add(15 * time.Second)
	for sent := 0; sent < totalFrames; sent++ {
		// Block until we have at least 1 credit. This is the actual point
		// where the original bug would manifest as a stall.
		for availableCredits <= 0 {
			select {
			case n := <-creditsCh:
				availableCredits += n
			case <-time.After(time.Until(sendDeadline)):
				consumedMu.Lock()
				got := len(consumed)
				consumedMu.Unlock()
				t.Fatalf("SENDER STALLED at frame %d/%d (consumed %d/%d bytes) — relay stopped replenishing credits",
					sent, totalFrames, got, totalFrames*frameSize)
			}
		}
		// Drain any additional credits that arrived non-blocking.
		for {
			select {
			case n := <-creditsCh:
				availableCredits += n
				continue
			default:
			}
			break
		}

		dataMsg := make([]byte, 0, 1+protocol.ClientIDLength+frameSize)
		dataMsg = append(dataMsg, protocol.ControlByteData)
		dataMsg = append(dataMsg, clientID[:]...)
		dataMsg = append(dataMsg, payload...)
		require.NoError(t, backendWS.WriteMessage(websocket.BinaryMessage, dataMsg))
		availableCredits--
	}

	// Wait for the slow consumer to drain everything.
	select {
	case <-consumerDone:
	case <-time.After(15 * time.Second):
		consumedMu.Lock()
		got := len(consumed)
		consumedMu.Unlock()
		t.Fatalf("STALL: only %d/%d bytes consumed in 15s after sender finished",
			got, totalFrames*frameSize)
	}

	consumedMu.Lock()
	got := len(consumed)
	consumedMu.Unlock()
	require.Equal(t, totalFrames*frameSize, got,
		"all bytes must be delivered to the slow consumer")

	// Verify content integrity.
	for i := 0; i < totalFrames; i++ {
		base := i * frameSize
		for j := 0; j < frameSize; j++ {
			require.Equal(t, byte(j&0xff), consumed[base+j],
				"frame %d offset %d: byte corruption", i, j)
		}
	}

	// Verify replenishment metrics show healthy flow (no silent drops).
	// retryAttempts should be 0 in the happy path, success should be many.
	successCount, retryAttempts := b.ReplenishStats()
	require.Greater(t, successCount, int64(0),
		"replenishSuccess should be nonzero — replenishment is not flowing")
	t.Logf("replenishment stats: success=%d retryAttempts=%d", successCount, retryAttempts)
	// Reader cleanup happens via the deferred close(readerStop).
}

// parseControlMessage is a small helper to deserialize a ControlMessage from
// the JSON body of a control frame (after stripping the ControlByteControl).
func parseControlMessage(t *testing.T, body []byte) protocol.ControlMessage {
	t.Helper()
	var ctrl protocol.ControlMessage
	if err := json.Unmarshal(body, &ctrl); err != nil {
		t.Fatalf("failed to unmarshal control message: %v", err)
	}
	return ctrl
}
