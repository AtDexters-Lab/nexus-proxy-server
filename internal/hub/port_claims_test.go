package hub

import (
	"strconv"
	"testing"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/stretchr/testify/require"
)

func TestNormalizeTCPPortClaims_Disabled(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	_, _, _, err := normalizeTCPPortClaims(cfg, []int{53})
	require.Error(t, err)
}

func TestNormalizeTCPPortClaims_AllowsAndSorts(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{AllowedTCPPortClaims: []int{53, 80}}
	ports, rejected, truncated, err := normalizeTCPPortClaims(cfg, []int{80, 53, 53})
	require.NoError(t, err)
	require.Equal(t, []int{53, 80}, ports)
	require.Empty(t, rejected)
	require.False(t, truncated)
}

func TestNormalizeTCPPortClaims_PartialAccept(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{AllowedTCPPortClaims: []int{443}}
	ports, rejected, truncated, err := normalizeTCPPortClaims(cfg, []int{443, 2222, 8080})
	require.NoError(t, err)
	require.Equal(t, []int{443}, ports)
	require.Len(t, rejected, 2)
	require.Equal(t, protocol.RejectedKindTCPPort, rejected[0].Kind)
	require.Equal(t, protocol.RejectedCodeTCPPortNotAllowed, rejected[0].Code)
	require.Equal(t, "2222", rejected[0].Value)
	require.Equal(t, "8080", rejected[1].Value)
	require.False(t, truncated)
}

func TestNormalizeTCPPortClaims_OutOfRangeStaysTerminal(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{AllowedTCPPortClaims: []int{443}}
	_, _, _, err := normalizeTCPPortClaims(cfg, []int{99999})
	require.Error(t, err)
}

func TestNormalize_RejectedSliceBounded(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{AllowedTCPPortClaims: []int{443}}
	// Build a claim list with 50 disallowed ports.
	claims := []int{443}
	for p := 1000; p < 1050; p++ {
		claims = append(claims, p)
	}
	ports, rejected, truncated, err := normalizeTCPPortClaims(cfg, claims)
	require.NoError(t, err)
	require.Equal(t, []int{443}, ports)
	require.Len(t, rejected, protocol.RejectedListBound, "rejected should cap at RejectedListBound entries")
	require.True(t, truncated)
	// Verify ordering preserved: first 32 disallowed ports in input order.
	for i := 0; i < protocol.RejectedListBound; i++ {
		require.Equal(t, strconv.Itoa(1000+i), rejected[i].Value)
	}
}

func TestNormalizeHostnames_ReservedRouteKey_SoftRejected(t *testing.T) {
	t.Parallel()

	// Single reserved key alone → no valid hostnames, but rejected list captures it.
	// With no surviving hostnames AND a rejected entry, we return success with empty
	// accepted (the cross-claim emptiness check at the call site handles this).
	hosts, rejected, _, err := normalizeHostnames([]string{"tcp:53"})
	require.NoError(t, err)
	require.Empty(t, hosts)
	require.Len(t, rejected, 1)
	require.Equal(t, protocol.RejectedCodeHostnameReservedRouteKey, rejected[0].Code)

	// Mixed: one good + one reserved → good accepted, reserved soft-rejected.
	hosts, rejected, _, err = normalizeHostnames([]string{"example.com", "tcp:53"})
	require.NoError(t, err)
	require.Equal(t, []string{"example.com"}, hosts)
	require.Len(t, rejected, 1)
}

func TestNormalizeHostnames_EmptyAfterTrimStaysSilent(t *testing.T) {
	t.Parallel()

	// Whitespace-only entries are silently dropped (today's behavior preserved);
	// they do NOT surface as soft-rejections to avoid migration noise.
	hosts, rejected, _, err := normalizeHostnames([]string{"example.com", " "})
	require.NoError(t, err)
	require.Equal(t, []string{"example.com"}, hosts)
	require.Empty(t, rejected)
}

func TestNormalizeUDPRouteClaims_ClampsTimeout(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		AllowedUDPPortClaims:             []int{53},
		UDPFlowIdleTimeoutDefaultSeconds: 30,
		UDPFlowIdleTimeoutMinSeconds:     5,
		UDPFlowIdleTimeoutMaxSeconds:     300,
	}

	routes, _, _, err := normalizeUDPRouteClaims(cfg, []protocol.UDPRouteClaim{
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(1)},
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, 53, routes[0].Port)
	require.Equal(t, 5*time.Second, routes[0].FlowIdleTimeout)

	routes, _, _, err = normalizeUDPRouteClaims(cfg, []protocol.UDPRouteClaim{
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(1000)},
	})
	require.NoError(t, err)
	require.Equal(t, 300*time.Second, routes[0].FlowIdleTimeout)

	routes, _, _, err = normalizeUDPRouteClaims(cfg, []protocol.UDPRouteClaim{
		{Port: 53},
	})
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, routes[0].FlowIdleTimeout)
}

func TestNormalizeUDPRouteClaims_PartialAccept(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		AllowedUDPPortClaims:             []int{53},
		UDPFlowIdleTimeoutDefaultSeconds: 30,
		UDPFlowIdleTimeoutMinSeconds:     5,
		UDPFlowIdleTimeoutMaxSeconds:     300,
	}

	routes, rejected, _, err := normalizeUDPRouteClaims(cfg, []protocol.UDPRouteClaim{
		{Port: 53},
		{Port: 5353},
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, 53, routes[0].Port)
	require.Len(t, rejected, 1)
	require.Equal(t, protocol.RejectedCodeUDPRouteNotAllowed, rejected[0].Code)
	require.Equal(t, "5353", rejected[0].Value)
}

func TestNormalizeUDPRouteClaims_ConflictingPolicies(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		AllowedUDPPortClaims:             []int{53},
		UDPFlowIdleTimeoutDefaultSeconds: 30,
		UDPFlowIdleTimeoutMinSeconds:     5,
		UDPFlowIdleTimeoutMaxSeconds:     300,
	}

	_, _, _, err := normalizeUDPRouteClaims(cfg, []protocol.UDPRouteClaim{
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(10)},
		{Port: 53, FlowIdleTimeoutSeconds: ptrInt(20)},
	})
	require.Error(t, err)
}

func TestNormalizeOutboundPortClaims_PartialAccept(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		AllowOutbound:        true,
		AllowedOutboundPorts: []int{443},
	}
	ports, rejected, _, err := normalizeOutboundPortClaims(cfg, true, []int{443, 25})
	require.NoError(t, err)
	require.Equal(t, []int{443}, ports)
	require.Len(t, rejected, 1)
	require.Equal(t, protocol.RejectedCodeOutboundPortNotAllowed, rejected[0].Code)
}

func ptrInt(v int) *int { return &v }
