package hub

import (
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNormalizeTCPPortClaims_Disabled(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	_, err := normalizeTCPPortClaims(cfg, []int{53})
	require.Error(t, err)
}

func TestNormalizeTCPPortClaims_AllowsAndSorts(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{AllowedTCPPortClaims: []int{53, 80}}
	ports, err := normalizeTCPPortClaims(cfg, []int{80, 53, 53})
	require.NoError(t, err)
	require.Equal(t, []int{53, 80}, ports)
}

func TestNormalizeHostnames_RejectsReservedRouteKeys(t *testing.T) {
	t.Parallel()

	_, err := normalizeHostnames([]string{"tcp:53"})
	require.Error(t, err)

	_, err = normalizeHostnames([]string{"udp:53"})
	require.Error(t, err)

	_, err = normalizeHostnames([]string{"example.com", "tcp:53"})
	require.Error(t, err)
}

func TestNormalizeUDPRouteClaims_ClampsTimeout(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		AllowedUDPPortClaims:             []int{53},
		UDPFlowIdleTimeoutDefaultSeconds: 30,
		UDPFlowIdleTimeoutMinSeconds:     5,
		UDPFlowIdleTimeoutMaxSeconds:     300,
	}

	routes, err := normalizeUDPRouteClaims(cfg, []auth.UDPRouteClaim{
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(1)},
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, 53, routes[0].Port)
	require.Equal(t, 5*time.Second, routes[0].FlowIdleTimeout)

	routes, err = normalizeUDPRouteClaims(cfg, []auth.UDPRouteClaim{
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(1000)},
	})
	require.NoError(t, err)
	require.Equal(t, 300*time.Second, routes[0].FlowIdleTimeout)

	routes, err = normalizeUDPRouteClaims(cfg, []auth.UDPRouteClaim{
		{Port: 53},
	})
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, routes[0].FlowIdleTimeout)
}

func TestNormalizeUDPRouteClaims_ConflictingPolicies(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		AllowedUDPPortClaims:             []int{53},
		UDPFlowIdleTimeoutDefaultSeconds: 30,
		UDPFlowIdleTimeoutMinSeconds:     5,
		UDPFlowIdleTimeoutMaxSeconds:     300,
	}

	_, err := normalizeUDPRouteClaims(cfg, []auth.UDPRouteClaim{
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(10)},
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(20)},
	})
	require.Error(t, err)
}

func ptrInt(v int) *int { return &v }
