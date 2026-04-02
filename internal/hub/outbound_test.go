package hub

import (
	"testing"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOutboundPortClaims_DisabledByConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: false}
	_, err := normalizeOutboundPortClaims(cfg, true, nil)
	require.Error(t, err, "should reject when server disables outbound")
}

func TestNormalizeOutboundPortClaims_NotAllowedByBackend(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: true}
	ports, err := normalizeOutboundPortClaims(cfg, false, nil)
	require.NoError(t, err)
	require.Nil(t, ports)
}

func TestNormalizeOutboundPortClaims_PortsWithoutAllowed(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: true}
	_, err := normalizeOutboundPortClaims(cfg, false, []int{80})
	require.Error(t, err, "ports set without outbound_allowed should error")
}

func TestNormalizeOutboundPortClaims_AllowedAllPorts(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: true}
	ports, err := normalizeOutboundPortClaims(cfg, true, nil)
	require.NoError(t, err)
	require.Nil(t, ports, "empty means all ports when allowed")
}

func TestNormalizeOutboundPortClaims_DeduplicatesAndSorts(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: true}
	ports, err := normalizeOutboundPortClaims(cfg, true, []int{443, 80, 443, 25})
	require.NoError(t, err)
	require.Equal(t, []int{25, 80, 443}, ports)
}

func TestNormalizeOutboundPortClaims_InvalidPort(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: true}
	_, err := normalizeOutboundPortClaims(cfg, true, []int{0})
	require.Error(t, err)
	_, err = normalizeOutboundPortClaims(cfg, true, []int{70000})
	require.Error(t, err)
}

func TestNormalizeOutboundPortClaims_ServerAllowlistEnforced(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowOutbound: true, AllowedOutboundPorts: []int{80, 443}}

	// Allowed
	ports, err := normalizeOutboundPortClaims(cfg, true, []int{80})
	require.NoError(t, err)
	require.Equal(t, []int{80}, ports)

	// Not in server allowlist
	_, err = normalizeOutboundPortClaims(cfg, true, []int{25})
	require.Error(t, err)
}

func TestConfigMaxOutboundConns_Defaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  int
		expect int
	}{
		{"zero defaults to 100", 0, 100},
		{"explicit value", 50, 50},
		{"minus one means no limit", -1, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{MaxOutboundConnsPerBackend: tt.value}
			require.Equal(t, tt.expect, cfg.MaxOutboundConns())
		})
	}
}
