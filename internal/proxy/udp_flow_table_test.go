package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type testUDPBackend struct {
	removed int
}

func (b *testUDPBackend) AddClient(net.Conn, uuid.UUID, string, bool) error { return nil }
func (b *testUDPBackend) RemoveClient(uuid.UUID)                            { b.removed++ }
func (b *testUDPBackend) SendData(uuid.UUID, []byte) error                  { return nil }
func (b *testUDPBackend) ID() string                                        { return "test" }

func TestUDPFlowTable_RefreshIdleTimeoutAffectsEviction(t *testing.T) {
	t.Parallel()

	backend := &testUDPBackend{}
	table := newUDPFlowTable(30*time.Second, 1024)

	flow := &udpFlow{
		key:      "127.0.0.1:12345",
		backend:  backend,
		clientID: uuid.New(),
	}
	table.add(flow)

	flow.lastSeen = time.Now().Add(-10 * time.Second)

	table.evict()
	require.Equal(t, 0, backend.removed)

	table.setIdleTimeout(5 * time.Second)
	table.evict()
	require.Equal(t, 1, backend.removed)
}
