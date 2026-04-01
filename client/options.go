package client

import (
	"context"
	"errors"
	"net"

	"github.com/google/uuid"
)

// ErrNoRoute is returned by connect handlers to indicate that the request
// should fall back to the default configuration-based routing.
var ErrNoRoute = errors.New("client: no route configured")

// ConnectRequest provides context about a client connection request coming from
// the Nexus proxy.
type ConnectRequest struct {
	BackendName      string
	ClientID         uuid.UUID
	Hostname         string
	OriginalHostname string
	Port             int
	ClientIP         string
	IsTLS            bool
	Transport        Transport // "tcp" or "udp" (defaults to "tcp" if empty)
}

// ConnectHandler is invoked whenever the proxy asks us to establish a new
// local connection. Returning ErrNoRoute will defer to the default
// port-mapping behaviour. Any other error is treated as fatal for that request.
type ConnectHandler func(ctx context.Context, req ConnectRequest) (net.Conn, error)

// Option mutates a Client during construction.
type Option func(*Client)

// WithConnectHandler registers a custom connect handler. The handler is invoked
// before the default port-mapping logic. Returning ErrNoRoute (or a nil
// connection) will fall back to the default handler.
func WithConnectHandler(handler ConnectHandler) Option {
	return func(c *Client) {
		if handler == nil {
			return
		}
		base := c.connectHandler
		c.connectHandler = func(ctx context.Context, req ConnectRequest) (net.Conn, error) {
			conn, err := handler(ctx, req)
			switch {
			case errors.Is(err, ErrNoRoute):
				return base(ctx, req)
			case err != nil:
				return nil, err
			case conn == nil:
				return base(ctx, req)
			default:
				return conn, nil
			}
		}
	}
}

// WithTokenProvider installs a TokenProvider that is consulted for handshake,
// attestation, and re-auth tokens. Passing nil restores the default provider.
func WithTokenProvider(provider TokenProvider) Option {
	return func(c *Client) {
		if provider == nil {
			c.tokenProvider = c.staticTokenProvider
			return
		}
		c.tokenProvider = provider
	}
}

// WithEventHandler registers a callback for client lifecycle events.
// Events are delivered asynchronously in order via a dedicated goroutine.
// The handler should not block for extended periods as this can cause
// events to be dropped. Passing nil disables event delivery.
func WithEventHandler(handler EventHandler) Option {
	return func(c *Client) {
		c.eventHandler = handler
	}
}
