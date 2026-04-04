package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type constantProvider struct {
	value string
}

func (p constantProvider) IssueToken(ctx context.Context, req TokenRequest) (Token, error) {
	return Token{Value: p.value}, nil
}

func newWebsocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("failed to upgrade: %v", err)
		}
		serverConnCh <- conn
	}))

	t.Cleanup(func() {
		srv.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial test websocket: %v", err)
	}

	serverConn := <-serverConnCh

	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	return clientConn, serverConn
}

func newTestClient(t *testing.T) *Client {
	t.Helper()

	cfg := ClientBackendConfig{
		Name:         "test-backend",
		Hostnames:    []string{"example.com"},
		NexusAddress: "ws://example.com",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "127.0.0.1:80"},
		},
	}
	c, err := New(cfg, WithTokenProvider(constantProvider{value: "token"}))
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	t.Cleanup(cancel)

	return c
}

func TestReadPumpStopsHelperGoroutineOnCancel(t *testing.T) {
	c := newTestClient(t)

	clientConn, _ := newWebsocketPair(t)
	c.wsMu.Lock()
	c.ws = clientConn
	c.wsMu.Unlock()

	done := make(chan struct{})
	c.wg.Add(1)
	go func() {
		c.readPump()
		close(done)
	}()

	time.Sleep(20 * time.Millisecond) // Allow goroutines to start.

	c.cancel()
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readPump did not exit after cancellation")
	}
}

func TestWritePumpDoesNotReplayStaleMessages(t *testing.T) {
	c := newTestClient(t)

	clientConn1, serverConn1 := newWebsocketPair(t)

	c.wsMu.Lock()
	c.ws = clientConn1
	c.wsMu.Unlock()

	session1 := c.beginSession()
	done1 := make(chan struct{})
	c.wg.Add(1)
	go func() {
		c.writePump(session1)
		close(done1)
	}()

	time.Sleep(20 * time.Millisecond)

	serverConn1.Close()
	clientConn1.Close()
	c.controlSend <- outboundMessage{messageType: websocket.BinaryMessage, payload: []byte("trigger")}

	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first writePump did not exit")
	}

	staleID := uuid.New()
	if err := c.sendControlMessage(protocol.EventDisconnect, staleID); err == nil {
		t.Fatalf("expected error when queueing control message on inactive session")
	}

	localClient, localServer := net.Pipe()
	defer localServer.Close()
	cc := &clientConn{
		id:       staleID,
		conn:     localClient,
		hostname: "stale.test",
		session:  session1, // Bind to dead session 1
		quit:     make(chan struct{}),
	}
	c.localConns.Store(staleID, cc)
	go c.copyLocalToNexus(cc)

	time.Sleep(20 * time.Millisecond)
	if _, err := localServer.Write([]byte("payload")); err != nil {
		t.Fatalf("failed to write payload: %v", err)
	}
	localClient.Close()

	clientConn2, serverConn2 := newWebsocketPair(t)

	c.wsMu.Lock()
	c.ws = clientConn2
	c.wsMu.Unlock()

	session2 := c.beginSession()
	done2 := make(chan struct{})
	c.wg.Add(1)
	go func() {
		c.writePump(session2)
		close(done2)
	}()

	serverConn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, msg, err := serverConn2.ReadMessage()
	if err == nil {
		t.Fatalf("unexpected stale message delivered to new session: %x", msg)
	}

	c.cancel()
	<-done2
}

// TestTransitionToClosedNoStaleDisconnect verifies that transitionToClosed does
// NOT send a disconnect message when the session that owned the connection is
// already dead. This prevents stale disconnects from leaking onto a successor
// session after reconnect.
func TestTransitionToClosedNoStaleDisconnect(t *testing.T) {
	c := newTestClient(t)

	// Simulate session 1 lifecycle: start then die.
	session := c.beginSession()
	session.Close()
	c.activeSession.Store(nil)
	c.clearSendQueues()

	// Create a pending clientConn bound to the dead session.
	staleID := uuid.New()
	cc := &clientConn{
		id:       staleID,
		hostname: "stale.test",
		session:  session,
		quit:     make(chan struct{}),
		drained:  make(chan struct{}),
	}
	c.localConns.Store(staleID, cc)

	// transitionToClosed should NOT enqueue a disconnect (session is dead).
	c.transitionToClosed(cc, DisconnectNormal)

	// Verify no message was enqueued to controlSend.
	select {
	case msg := <-c.controlSend:
		t.Fatalf("stale disconnect leaked onto control channel: %x", msg.payload)
	default:
		// Good — no stale message.
	}

	// Verify cleanup still happened.
	if _, ok := c.localConns.Load(staleID); ok {
		t.Fatal("expected localConns entry to be cleaned up")
	}
}

// TestTransitionToClosedNoStaleDisconnectAfterReconnect verifies that a stale
// connection from session 1 does NOT send a disconnect onto session 2 when the
// goroutine calling transitionToClosed runs after session 2 has already started.
// This is the deterministic reproducer for the race that
// TestWritePumpDoesNotReplayStaleMessages catches probabilistically.
func TestTransitionToClosedNoStaleDisconnectAfterReconnect(t *testing.T) {
	c := newTestClient(t)

	// Session 1 lifecycle: start then die.
	session1 := c.beginSession()
	session1.Close()
	c.activeSession.Store(nil)
	c.clearSendQueues()

	// Session 2 starts (simulates successful reconnect).
	_ = c.beginSession()

	// A stale connection from session 1 calls transitionToClosed.
	staleID := uuid.New()
	cc := &clientConn{
		id:       staleID,
		hostname: "stale.test",
		session:  session1, // Bound to dead session 1
		quit:     make(chan struct{}),
		drained:  make(chan struct{}),
	}
	c.localConns.Store(staleID, cc)

	c.transitionToClosed(cc, DisconnectNormal)

	// The disconnect must NOT be enqueued — it belongs to session 1, not session 2.
	select {
	case msg := <-c.controlSend:
		t.Fatalf("stale disconnect leaked onto successor session: %x", msg.payload)
	default:
	}

	if _, ok := c.localConns.Load(staleID); ok {
		t.Fatal("expected localConns entry to be cleaned up")
	}
}

// TestTransitionToClosedDrainPathNoStaleDisconnectAfterReconnect verifies the
// drain path: an Active-state connection from session 1 must NOT send a
// disconnect onto session 2 when the drain goroutine completes after reconnect.
func TestTransitionToClosedDrainPathNoStaleDisconnectAfterReconnect(t *testing.T) {
	c := newTestClient(t)

	// Session 1 lifecycle: start then die.
	session1 := c.beginSession()
	session1.Close()
	c.activeSession.Store(nil)
	c.clearSendQueues()

	// Session 2 starts (simulates successful reconnect).
	_ = c.beginSession()

	// A stale Active-state connection from session 1.
	staleID := uuid.New()
	cc := &clientConn{
		id:       staleID,
		hostname: "stale.test",
		session:  session1,
		quit:     make(chan struct{}),
		drained:  make(chan struct{}),
	}
	cc.state.Store(uint32(ConnStateActive))
	c.localConns.Store(staleID, cc)
	c.getOrCreateQueue(staleID)

	// transitionToClosed enters the drain path (Active → Closed).
	// The drain goroutine should detect session1 is dead and skip disconnect.
	c.transitionToClosed(cc, DisconnectNormal)

	// Give the async drain goroutine time to complete.
	time.Sleep(50 * time.Millisecond)

	select {
	case msg := <-c.controlSend:
		t.Fatalf("stale disconnect leaked onto successor session via drain path: %x", msg.payload)
	default:
	}

	if _, ok := c.localConns.Load(staleID); ok {
		t.Fatal("expected localConns entry to be cleaned up")
	}
}

func TestHandleTextMessageRespondsToReauthChallenge(t *testing.T) {
	c := newTestClient(t)
	c.beginSession()
	c.controlSend = make(chan outboundMessage, 1)

	msg := []byte(`{"type":"reauth_challenge","nonce":"abc123"}`)

	if err := c.handleTextMessage(msg); err != nil {
		t.Fatalf("expected handler to succeed, got error: %v", err)
	}

	select {
	case outbound := <-c.controlSend:
		if outbound.messageType != websocket.TextMessage {
			t.Fatalf("expected text message, got type %d", outbound.messageType)
		}
		if string(outbound.payload) != "token" {
			t.Fatalf("expected token payload, got %q", string(outbound.payload))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected reauth token to be enqueued")
	}
}

func TestSendControlMessageSkipsMarshalErrors(t *testing.T) {
	c := newTestClient(t)

	wantErr := "marshal failed"
	c.marshalJSON = func(v interface{}) ([]byte, error) {
		return nil, errors.New(wantErr)
	}

	c.controlSend = make(chan outboundMessage, 1)

	c.beginSession()
	err := c.sendControlMessage(protocol.EventPingClient, uuid.New())
	if err == nil {
		t.Fatalf("expected marshal error")
	}
	if !strings.Contains(err.Error(), wantErr) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected error containing %q, got %v", wantErr, err)
	}

	select {
	case msg := <-c.controlSend:
		t.Fatalf("expected no message enqueued, got %x", msg)
	default:
	}
}

func TestHandleControlMessageWithTransport(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "test-backend",
		Hostnames:    []string{"example.com"},
		NexusAddress: "ws://example.com",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			53: {Default: "127.0.0.1:53"},
		},
	}

	var capturedReq ConnectRequest
	done := make(chan struct{}, 1)
	c, err := New(cfg,
		WithTokenProvider(constantProvider{value: "token"}),
		WithConnectHandler(func(ctx context.Context, req ConnectRequest) (net.Conn, error) {
			capturedReq = req
			close(done)
			return nil, ErrNoRoute
		}),
	)
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	defer cancel()

	clientID := uuid.New()
	payload := fmt.Sprintf(`{"event":"connect","client_id":"%s","conn_port":53,"transport":"udp","hostname":"udp:53"}`, clientID)
	c.handleControlMessage([]byte(payload))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connect handler was not called")
	}

	if capturedReq.Transport != TransportUDP {
		t.Fatalf("expected transport UDP, got %s", capturedReq.Transport)
	}
	if capturedReq.Hostname != "udp:53" {
		t.Fatalf("expected hostname 'udp:53', got %s", capturedReq.Hostname)
	}
}

func TestHandleControlMessageDefaultsToTCP(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "test-backend",
		Hostnames:    []string{"example.com"},
		NexusAddress: "ws://example.com",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "127.0.0.1:80"},
		},
	}

	var capturedReq ConnectRequest
	done := make(chan struct{}, 1)
	c, err := New(cfg,
		WithTokenProvider(constantProvider{value: "token"}),
		WithConnectHandler(func(ctx context.Context, req ConnectRequest) (net.Conn, error) {
			capturedReq = req
			close(done)
			return nil, ErrNoRoute
		}),
	)
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	defer cancel()

	clientID := uuid.New()
	payload := fmt.Sprintf(`{"event":"connect","client_id":"%s","conn_port":80,"hostname":"example.com"}`, clientID)
	c.handleControlMessage([]byte(payload))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connect handler was not called")
	}

	if capturedReq.Transport != TransportTCP {
		t.Fatalf("expected transport TCP (default), got %s", capturedReq.Transport)
	}
}

func TestConfigBasedConnectHandlerUsesTransport(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "test-backend",
		Hostnames:    []string{"example.com"},
		NexusAddress: "ws://example.com",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			53: {Default: "127.0.0.1:53"},
		},
	}

	c, err := New(cfg, WithTokenProvider(constantProvider{value: "token"}))
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	defer cancel()

	handler := c.configBasedConnectHandler()

	// Test UDP dial (will fail to connect but we can verify no panic)
	req := ConnectRequest{
		BackendName: "test-backend",
		ClientID:    uuid.New(),
		Hostname:    "udp:53",
		Port:        53,
		Transport:   TransportUDP,
	}

	// The dial will fail because nothing is listening, but it should attempt UDP
	conn, err := handler(ctx, req)
	if conn != nil {
		conn.Close()
	}
	// We just verify no panic occurred; error is expected since nothing is listening
	_ = err
}

func TestHandleControlMessageWithUnrecognizedTransport(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "test-backend",
		Hostnames:    []string{"example.com"},
		NexusAddress: "ws://example.com",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "127.0.0.1:80"},
		},
	}

	var capturedReq ConnectRequest
	done := make(chan struct{}, 1)
	c, err := New(cfg,
		WithTokenProvider(constantProvider{value: "token"}),
		WithConnectHandler(func(ctx context.Context, req ConnectRequest) (net.Conn, error) {
			capturedReq = req
			close(done)
			return nil, ErrNoRoute
		}),
	)
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	defer cancel()

	clientID := uuid.New()
	payload := fmt.Sprintf(`{"event":"connect","client_id":"%s","conn_port":80,"transport":"invalid_transport","hostname":"example.com"}`, clientID)
	c.handleControlMessage([]byte(payload))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connect handler was not called")
	}

	if capturedReq.Transport != TransportTCP {
		t.Fatalf("expected unrecognized transport to default to TCP, got %s", capturedReq.Transport)
	}
}
