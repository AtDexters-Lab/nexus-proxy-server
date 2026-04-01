package client

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func intPtr(v int) *int { return &v }

func TestHMACTokenProviderProducesExpectedClaims(t *testing.T) {
	opts := AttestationOptions{
		HMACSecret:                 "super-secret",
		TokenTTL:                   60 * time.Second,
		ReauthIntervalSeconds:      300,
		ReauthGraceSeconds:         20,
		MaintenanceGraceCapSeconds: 600,
		HandshakeMaxAgeSeconds:     5,
	}

	provider, err := NewHMACTokenProvider(opts, "backend", []string{"example.com"}, nil, nil, 2)
	if err != nil {
		t.Fatalf("failed to build provider: %v", err)
	}

	handshake, err := provider.IssueToken(context.Background(), TokenRequest{
		Stage:       StageHandshake,
		BackendName: "backend",
		Hostnames:   []string{"example.com"},
		Weight:      2,
	})
	if err != nil {
		t.Fatalf("handshake token failed: %v", err)
	}

	parse := func(tok string) jwt.MapClaims {
		parser := jwt.NewParser()
		token, _, err := parser.ParseUnverified(tok, jwt.MapClaims{})
		if err != nil {
			t.Fatalf("failed to parse token: %v", err)
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			t.Fatalf("unexpected claims type: %T", token.Claims)
		}
		return claims
	}

	handshakeClaims := parse(handshake.Value)
	if _, ok := handshakeClaims["session_nonce"]; ok {
		t.Fatalf("expected handshake token to omit session_nonce")
	}
	if handshakeClaims["weight"].(float64) != 2 {
		t.Fatalf("expected weight=2, got %v", handshakeClaims["weight"])
	}

	reauth, err := provider.IssueToken(context.Background(), TokenRequest{
		Stage:        StageReauth,
		BackendName:  "backend",
		Hostnames:    []string{"example.com"},
		Weight:       2,
		SessionNonce: "nonce123",
	})
	if err != nil {
		t.Fatalf("reauth token failed: %v", err)
	}

	reauthClaims := parse(reauth.Value)
	if got := reauthClaims["session_nonce"]; got != "nonce123" {
		t.Fatalf("expected session_nonce 'nonce123', got %v", got)
	}
	if got := reauthClaims["reauth_interval_seconds"]; got != float64(300) {
		t.Fatalf("expected reauth interval claim, got %v", got)
	}
}

func TestHMACTokenProviderIncludesTCPPorts(t *testing.T) {
	opts := AttestationOptions{
		HMACSecret: "super-secret",
		TokenTTL:   60 * time.Second,
	}

	provider, err := NewHMACTokenProvider(opts, "backend", nil, []int{22, 443}, nil, 1)
	if err != nil {
		t.Fatalf("failed to build provider: %v", err)
	}

	token, err := provider.IssueToken(context.Background(), TokenRequest{
		Stage:       StageHandshake,
		BackendName: "backend",
		TCPPorts:    []int{22, 443},
		Weight:      1,
	})
	if err != nil {
		t.Fatalf("token failed: %v", err)
	}

	parser := jwt.NewParser()
	parsed, _, err := parser.ParseUnverified(token.Value, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("failed to parse token: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)

	tcpPorts, ok := claims["tcp_ports"].([]interface{})
	if !ok {
		t.Fatalf("expected tcp_ports claim to be an array, got %T", claims["tcp_ports"])
	}
	if len(tcpPorts) != 2 {
		t.Fatalf("expected 2 TCP ports, got %d", len(tcpPorts))
	}
	if tcpPorts[0].(float64) != 22 || tcpPorts[1].(float64) != 443 {
		t.Fatalf("expected TCP ports [22, 443], got %v", tcpPorts)
	}
}

func TestHMACTokenProviderIncludesUDPRoutes(t *testing.T) {
	opts := AttestationOptions{
		HMACSecret: "super-secret",
		TokenTTL:   60 * time.Second,
	}

	udpRoutes := []UDPRouteConfig{
		{Port: 53, FlowIdleTimeoutSeconds: intPtr(30)},
		{Port: 67},
	}

	provider, err := NewHMACTokenProvider(opts, "backend", nil, nil, udpRoutes, 1)
	if err != nil {
		t.Fatalf("failed to build provider: %v", err)
	}

	token, err := provider.IssueToken(context.Background(), TokenRequest{
		Stage:       StageHandshake,
		BackendName: "backend",
		UDPRoutes:   udpRoutes,
		Weight:      1,
	})
	if err != nil {
		t.Fatalf("token failed: %v", err)
	}

	parser := jwt.NewParser()
	parsed, _, err := parser.ParseUnverified(token.Value, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("failed to parse token: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)

	udpRoutesRaw, ok := claims["udp_routes"].([]interface{})
	if !ok {
		t.Fatalf("expected udp_routes claim to be an array, got %T", claims["udp_routes"])
	}
	if len(udpRoutesRaw) != 2 {
		t.Fatalf("expected 2 UDP routes, got %d", len(udpRoutesRaw))
	}

	first := udpRoutesRaw[0].(map[string]interface{})
	if first["port"].(float64) != 53 {
		t.Fatalf("expected first UDP route port 53, got %v", first["port"])
	}
	if first["flow_idle_timeout_seconds"].(float64) != 30 {
		t.Fatalf("expected first UDP route timeout 30, got %v", first["flow_idle_timeout_seconds"])
	}

	second := udpRoutesRaw[1].(map[string]interface{})
	if second["port"].(float64) != 67 {
		t.Fatalf("expected second UDP route port 67, got %v", second["port"])
	}
	if _, hasTimeout := second["flow_idle_timeout_seconds"]; hasTimeout {
		t.Fatalf("expected second UDP route to not have timeout")
	}
}

func TestHMACTokenProviderOmitsEmptyArrays(t *testing.T) {
	opts := AttestationOptions{
		HMACSecret: "super-secret",
		TokenTTL:   60 * time.Second,
	}

	provider, err := NewHMACTokenProvider(opts, "backend", []string{"example.com"}, nil, nil, 1)
	if err != nil {
		t.Fatalf("failed to build provider: %v", err)
	}

	token, err := provider.IssueToken(context.Background(), TokenRequest{
		Stage:       StageHandshake,
		BackendName: "backend",
		Hostnames:   []string{"example.com"},
		Weight:      1,
	})
	if err != nil {
		t.Fatalf("token failed: %v", err)
	}

	parser := jwt.NewParser()
	parsed, _, err := parser.ParseUnverified(token.Value, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("failed to parse token: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)

	if _, ok := claims["tcp_ports"]; ok {
		t.Fatalf("expected tcp_ports to be omitted when empty")
	}
	if _, ok := claims["udp_routes"]; ok {
		t.Fatalf("expected udp_routes to be omitted when empty")
	}
}

func TestFormatTCPPorts(t *testing.T) {
	tests := []struct {
		ports    []int
		expected string
	}{
		{nil, ""},
		{[]int{}, ""},
		{[]int{53}, "53"},
		{[]int{22, 80, 443}, "22,80,443"},
	}

	for _, tc := range tests {
		result := formatTCPPorts(tc.ports)
		if result != tc.expected {
			t.Errorf("formatTCPPorts(%v) = %q, want %q", tc.ports, result, tc.expected)
		}
	}
}

func TestFormatUDPRoutes(t *testing.T) {
	tests := []struct {
		routes   []UDPRouteConfig
		expected string
	}{
		{nil, ""},
		{[]UDPRouteConfig{}, ""},
		{[]UDPRouteConfig{{Port: 53}}, "53"},
		{[]UDPRouteConfig{{Port: 53, FlowIdleTimeoutSeconds: intPtr(30)}}, "53:30"},
		{
			[]UDPRouteConfig{
				{Port: 53, FlowIdleTimeoutSeconds: intPtr(30)},
				{Port: 67},
			},
			"53:30,67",
		},
	}

	for _, tc := range tests {
		result := formatUDPRoutes(tc.routes)
		if result != tc.expected {
			t.Errorf("formatUDPRoutes(%v) = %q, want %q", tc.routes, result, tc.expected)
		}
	}
}

func TestCopyUDPRoutesNilInput(t *testing.T) {
	result := CopyUDPRoutes(nil)
	if result != nil {
		t.Fatalf("expected nil for nil input, got %v", result)
	}
}

func TestCopyUDPRoutesEmptySlice(t *testing.T) {
	result := CopyUDPRoutes([]UDPRouteConfig{})
	if result != nil {
		t.Fatalf("expected nil for empty slice, got %v", result)
	}
}

func TestCopyUDPRoutesDeepCopy(t *testing.T) {
	timeout := 30
	original := []UDPRouteConfig{
		{Port: 53, FlowIdleTimeoutSeconds: &timeout},
		{Port: 67},
	}

	copied := CopyUDPRoutes(original)

	// Verify basic equality
	if len(copied) != len(original) {
		t.Fatalf("expected %d routes, got %d", len(original), len(copied))
	}
	if copied[0].Port != 53 || copied[1].Port != 67 {
		t.Fatalf("ports not copied correctly")
	}
	if copied[0].FlowIdleTimeoutSeconds == nil || *copied[0].FlowIdleTimeoutSeconds != 30 {
		t.Fatalf("timeout not copied correctly for first route")
	}
	if copied[1].FlowIdleTimeoutSeconds != nil {
		t.Fatalf("expected nil timeout for second route")
	}

	// Verify deep copy: modifying original should not affect copy
	*original[0].FlowIdleTimeoutSeconds = 99
	original[0].Port = 999

	if *copied[0].FlowIdleTimeoutSeconds != 30 {
		t.Fatalf("modifying original timeout affected copy: got %d", *copied[0].FlowIdleTimeoutSeconds)
	}
	if copied[0].Port != 53 {
		t.Fatalf("modifying original port affected copy: got %d", copied[0].Port)
	}

	// Verify pointer independence
	if copied[0].FlowIdleTimeoutSeconds == original[0].FlowIdleTimeoutSeconds {
		t.Fatalf("copy shares same pointer as original")
	}
}
