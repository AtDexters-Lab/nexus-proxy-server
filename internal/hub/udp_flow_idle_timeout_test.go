package hub

import (
	"net/http"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUDPFlowIdleTimeout_TracksConnectedBackends(t *testing.T) {
	t.Parallel()

	h := New(&config.Config{}, nil, nil, &http.Client{})

	h.trackUDPFlowIdleTimeout("b1", []UDPRoutePolicy{{Port: 53, FlowIdleTimeout: 10 * time.Second}})
	d, ok := h.UDPFlowIdleTimeout(53)
	require.True(t, ok)
	require.Equal(t, 10*time.Second, d)

	// Conflicting policies are allowed (best-effort exclusivity), but should be refreshed.
	// The effective timeout uses the maximum to avoid prematurely expiring flows.
	h.trackUDPFlowIdleTimeout("b2", []UDPRoutePolicy{{Port: 53, FlowIdleTimeout: 20 * time.Second}})
	d, ok = h.UDPFlowIdleTimeout(53)
	require.True(t, ok)
	require.Equal(t, 20*time.Second, d)

	h.untrackUDPFlowIdleTimeout("b2", []UDPRoutePolicy{{Port: 53, FlowIdleTimeout: 20 * time.Second}})
	d, ok = h.UDPFlowIdleTimeout(53)
	require.True(t, ok)
	require.Equal(t, 10*time.Second, d)

	h.untrackUDPFlowIdleTimeout("b1", []UDPRoutePolicy{{Port: 53, FlowIdleTimeout: 10 * time.Second}})
	_, ok = h.UDPFlowIdleTimeout(53)
	require.False(t, ok)
}
