package peer

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type failingBackend struct {
	addCalls atomic.Int32
}

func (b *failingBackend) ID() string { return "backend" }

func (b *failingBackend) AddClient(net.Conn, uuid.UUID, string, bool) error {
	b.addCalls.Add(1)
	return errors.New("boom")
}

func (b *failingBackend) RemoveClient(uuid.UUID)           {}
func (b *failingBackend) SendData(uuid.UUID, []byte) error { return nil }

type stubHub struct {
	backend iface.Backend
}

func (h *stubHub) GetLocalRoutes() []string { return nil }

func (h *stubHub) SelectBackend(string) (iface.Backend, error) {
	return h.backend, nil
}

type noopPeer struct{ addr string }

func (p *noopPeer) Addr() string                       { return p.addr }
func (p *noopPeer) Send([]byte)                        {}
func (p *noopPeer) StartTunnel(net.Conn, string, bool) {}

func TestHandleTunnelRequestCleansUpOnBackendFailure(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	backend := &failingBackend{}
	mgr := NewManager(cfg, &stubHub{backend: backend}, nil)
	p := &noopPeer{addr: "peer-1"}
	clientID := uuid.New()

	mgr.HandleTunnelRequest(p, "example.com", clientID, "203.0.113.10", 443, true)

	require.Eventually(t, func() bool {
		_, ok := mgr.tunnels.Load(clientID)
		return !ok
	}, time.Second, 10*time.Millisecond, "tunnel entry should be cleared when backend rejects client")

	require.Equal(t, int32(1), backend.addCalls.Load())
}
