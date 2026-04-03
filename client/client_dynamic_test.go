package client

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
)

type trackingProvider struct {
	value   string
	onIssue func()
}

func (p *trackingProvider) IssueToken(ctx context.Context, req TokenRequest) (Token, error) {
	if p.onIssue != nil {
		p.onIssue()
	}
	return Token{Value: p.value}, nil
}

func TestClientWithCustomConnectHandler(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "dynamic",
		Hostnames:    []string{"hello.example.com"},
		NexusAddress: "wss://nexus.example.com/connect",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "localhost:8080"},
		},
	}

	var (
		gotReq        ConnectRequest
		handlerCalled = make(chan struct{}, 1)
		appConnCh     = make(chan net.Conn, 1)
	)

	handler := func(ctx context.Context, req ConnectRequest) (net.Conn, error) {
		gotReq = req
		server, app := net.Pipe()
		appConnCh <- app
		handlerCalled <- struct{}{}
		return server, nil
	}

	c, err := New(cfg, WithConnectHandler(handler), WithTokenProvider(constantProvider{value: "token"}))
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.connected.Store(true) // Simulate active session
	defer c.cancel()

	msg := protocol.ControlMessage{
		Event:    protocol.EventConnect,
		ClientID: uuid.New(),
		ConnPort: 80,
		ClientIP: "203.0.113.10:54321",
		Hostname: "Hello.EXAMPLE.com",
		IsTLS:    true,
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal control message: %v", err)
	}

	c.handleControlMessage(payload)

	select {
	case <-handlerCalled:
	case <-time.After(time.Second):
		t.Fatal("connect handler was not invoked")
	}

	if gotReq.Hostname != "hello.example.com" {
		t.Fatalf("expected normalized hostname, got %s", gotReq.Hostname)
	}
	if gotReq.OriginalHostname != msg.Hostname {
		t.Fatalf("expected original hostname %s, got %s", msg.Hostname, gotReq.OriginalHostname)
	}
	if gotReq.Port != msg.ConnPort {
		t.Fatalf("expected port %d, got %d", msg.ConnPort, gotReq.Port)
	}
	if gotReq.ClientIP != msg.ClientIP {
		t.Fatalf("expected client IP %s, got %s", msg.ClientIP, gotReq.ClientIP)
	}
	if !gotReq.IsTLS {
		t.Fatalf("expected IsTLS to be true")
	}

	if _, ok := c.localConns.Load(msg.ClientID); !ok {
		t.Fatalf("expected client connection to be tracked")
	}

	appConn := <-appConnCh
	appConn.Close()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := c.localConns.Load(msg.ClientID); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("expected client connection cleanup after handler close")
}

func TestClientGetAuthTokenUsesProvider(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "provider-token",
		Hostnames:    []string{"example.com"},
		NexusAddress: "wss://nexus.example.com/connect",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "localhost:8080"},
		},
	}

	var calls int
	provider := &trackingProvider{
		value: "provided-token",
		onIssue: func() {
			calls++
		},
	}

	c, err := New(cfg, WithTokenProvider(provider))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	token, err := c.getAuthToken(context.Background())
	if err != nil {
		t.Fatalf("expected token, got error: %v", err)
	}
	if token != "provided-token" {
		t.Fatalf("expected provider token, got %q", token)
	}
	if calls != 1 {
		t.Fatalf("expected provider to be invoked exactly once, got %d calls", calls)
	}
}

func TestWithTokenProviderOverrides(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "dynamic-token",
		Hostnames:    []string{"example.com"},
		NexusAddress: "wss://nexus.example.com/connect",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "localhost:8080"},
		},
	}

	c, err := New(cfg, WithTokenProvider(constantProvider{value: "first"}))
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}

	token, err := c.getAuthToken(context.Background())
	if err != nil {
		t.Fatalf("expected token, got error: %v", err)
	}
	if token != "first" {
		t.Fatalf("expected first provider token, got %q", token)
	}

	WithTokenProvider(constantProvider{value: "second"})(c)
	token, err = c.getAuthToken(context.Background())
	if err != nil {
		t.Fatalf("expected token after override, got error: %v", err)
	}
	if token != "second" {
		t.Fatalf("expected second provider token, got %q", token)
	}
}

func TestWithTokenProviderNilResetsToStatic(t *testing.T) {
	cfg := ClientBackendConfig{
		Name:         "reset-token",
		Hostnames:    []string{"example.com"},
		NexusAddress: "wss://nexus.example.com/connect",
		Weight:       1,
		PortMappings: map[int]PortMapping{
			80: {Default: "localhost:8080"},
		},
	}

	c, err := New(cfg, WithTokenProvider(constantProvider{value: "static-token"}))
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}

	initial, err := c.getAuthToken(context.Background())
	if err != nil {
		t.Fatalf("expected initial static token, got error: %v", err)
	}
	if initial != "static-token" {
		t.Fatalf("expected initial static token, got %q", initial)
	}

	WithTokenProvider(constantProvider{value: "dynamic"})(c)
	dynamic, err := c.getAuthToken(context.Background())
	if err != nil {
		t.Fatalf("expected dynamic token, got error: %v", err)
	}
	if dynamic != "dynamic" {
		t.Fatalf("expected dynamic token, got %q", dynamic)
	}

	WithTokenProvider(nil)(c)
	reset, err := c.getAuthToken(context.Background())
	if err != nil {
		t.Fatalf("expected reset static token, got error: %v", err)
	}
	if reset != "static-token" {
		t.Fatalf("expected reset static token, got %q", reset)
	}
}
