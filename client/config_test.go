package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPortMappingResolveDefault(t *testing.T) {
	pm := PortMapping{Default: "localhost:8080"}
	if err := pm.finalize(); err != nil {
		t.Fatalf("unexpected finalize error: %v", err)
	}

	addr, ok := pm.Resolve("app.example.com")
	if !ok {
		t.Fatal("expected resolution to succeed")
	}
	if addr != "localhost:8080" {
		t.Fatalf("expected default target, got %s", addr)
	}
}

func TestPortMappingResolveExactOverride(t *testing.T) {
	pm := PortMapping{
		Default: "localhost:8080",
		Hosts: map[string]string{
			"api.example.com": "localhost:9090",
		},
	}
	if err := pm.finalize(); err != nil {
		t.Fatalf("unexpected finalize error: %v", err)
	}

	addr, ok := pm.Resolve("API.EXAMPLE.COM")
	if !ok {
		t.Fatal("expected resolution to succeed")
	}
	if addr != "localhost:9090" {
		t.Fatalf("expected override target, got %s", addr)
	}
}

func TestPortMappingResolveWildcard(t *testing.T) {
	pm := PortMapping{
		Default: "localhost:8080",
		Hosts: map[string]string{
			"*.example.com": "localhost:7070",
		},
	}
	if err := pm.finalize(); err != nil {
		t.Fatalf("unexpected finalize error: %v", err)
	}

	tests := []struct {
		host     string
		wantOK   bool
		wantAddr string
	}{
		{"foo.example.com", true, "localhost:7070"},
		{"foo.bar.example.com", true, "localhost:8080"},
		{"example.com", true, "localhost:8080"},
	}

	for _, tc := range tests {
		addr, ok := pm.Resolve(tc.host)
		if ok != tc.wantOK {
			t.Fatalf("host %s: expected ok=%v, got %v", tc.host, tc.wantOK, ok)
		}
		if !ok {
			continue
		}
		if addr != tc.wantAddr {
			t.Fatalf("host %s: expected addr=%s, got %s", tc.host, tc.wantAddr, addr)
		}
	}

	// host without matching override but no default should fail
	pmNoDefault := PortMapping{
		Hosts: map[string]string{
			"*.example.com": "localhost:7070",
		},
	}
	if err := pmNoDefault.finalize(); err != nil {
		t.Fatalf("unexpected finalize error: %v", err)
	}
	if _, ok := pmNoDefault.Resolve("unmatched.test"); ok {
		t.Fatal("expected resolution to fail without default")
	}
}

func TestLoadConfigRejectsUnknownPortMappingField(t *testing.T) {
	configYAML := `backends:
  - name: "dynamic"
    hostnames:
      - "example.com"
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      command: "/usr/bin/true"
    portMappings:
      80:
        default: "localhost:8080"
        hostnames:
          "example.com": "localhost:9090"
`

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error due to unknown 'hostnames' field in port mapping")
	}
}

func TestLoadConfigRequiresAttestationMechanism(t *testing.T) {
	configYAML := `backends:
  - name: "missing"
    hostnames:
      - "example.com"
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    portMappings:
      80:
        default: "localhost:8080"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error because no attestation command or secret configured")
	}
}

func TestLoadConfigAllowsHMACSecret(t *testing.T) {
	configYAML := `backends:
  - name: "hmac"
    hostnames:
      - "example.com"
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "top-secret"
    portMappings:
      80:
        default: "localhost:8080"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}
	if cfg.Backends[0].Attestation.HMACSecret != "top-secret" {
		t.Fatalf("expected hmac secret to be preserved")
	}
	if cfg.Backends[0].Attestation.TokenTTLSeconds != 30 {
		t.Fatalf("expected default token TTL of 30 seconds, got %d", cfg.Backends[0].Attestation.TokenTTLSeconds)
	}
}

func TestLoadConfigWithTCPPorts(t *testing.T) {
	configYAML := `backends:
  - name: "tcp-only"
    tcpPorts: [22, 443]
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      22:
        default: "localhost:22"
      443:
        default: "localhost:443"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}
	if len(cfg.Backends[0].TCPPorts) != 2 {
		t.Fatalf("expected 2 TCP ports, got %d", len(cfg.Backends[0].TCPPorts))
	}
}

func TestLoadConfigWithUDPRoutes(t *testing.T) {
	timeout := 30
	configYAML := `backends:
  - name: "udp-only"
    udpRoutes:
      - port: 53
        flowIdleTimeoutSeconds: 30
      - port: 67
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      53:
        default: "localhost:53"
      67:
        default: "localhost:67"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}
	if len(cfg.Backends[0].UDPRoutes) != 2 {
		t.Fatalf("expected 2 UDP routes, got %d", len(cfg.Backends[0].UDPRoutes))
	}
	if cfg.Backends[0].UDPRoutes[0].Port != 53 {
		t.Fatalf("expected first UDP route port 53, got %d", cfg.Backends[0].UDPRoutes[0].Port)
	}
	if cfg.Backends[0].UDPRoutes[0].FlowIdleTimeoutSeconds == nil || *cfg.Backends[0].UDPRoutes[0].FlowIdleTimeoutSeconds != timeout {
		t.Fatalf("expected first UDP route timeout %d, got %v", timeout, cfg.Backends[0].UDPRoutes[0].FlowIdleTimeoutSeconds)
	}
	if cfg.Backends[0].UDPRoutes[1].FlowIdleTimeoutSeconds != nil {
		t.Fatalf("expected second UDP route timeout to be nil")
	}
}

func TestLoadConfigMixedClaims(t *testing.T) {
	configYAML := `backends:
  - name: "mixed"
    hostnames:
      - "example.com"
    tcpPorts: [443]
    udpRoutes:
      - port: 53
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      443:
        default: "localhost:443"
      53:
        default: "localhost:53"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}
	if len(cfg.Backends[0].Hostnames) != 1 {
		t.Fatalf("expected 1 hostname, got %d", len(cfg.Backends[0].Hostnames))
	}
	if len(cfg.Backends[0].TCPPorts) != 1 {
		t.Fatalf("expected 1 TCP port, got %d", len(cfg.Backends[0].TCPPorts))
	}
	if len(cfg.Backends[0].UDPRoutes) != 1 {
		t.Fatalf("expected 1 UDP route, got %d", len(cfg.Backends[0].UDPRoutes))
	}
}

func TestLoadConfigRequiresSomeRoute(t *testing.T) {
	configYAML := `backends:
  - name: "empty"
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      80:
        default: "localhost:80"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error because no hostnames, tcpPorts, or udpRoutes")
	}
}

func TestLoadConfigRejectsRouteKeyHostnames(t *testing.T) {
	configYAML := `backends:
  - name: "bad-hostname"
    hostnames:
      - "tcp:53"
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      53:
        default: "localhost:53"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error because hostname looks like route key")
	}
}

func TestLoadConfigValidatesPortRange(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{
			name: "TCP port 0",
			config: `backends:
  - name: "bad"
    tcpPorts: [0]
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      80:
        default: "localhost:80"
`,
		},
		{
			name: "TCP port > 65535",
			config: `backends:
  - name: "bad"
    tcpPorts: [70000]
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      80:
        default: "localhost:80"
`,
		},
		{
			name: "UDP port 0",
			config: `backends:
  - name: "bad"
    udpRoutes:
      - port: 0
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      80:
        default: "localhost:80"
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatalf("failed to write temp config: %v", err)
			}

			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error for invalid port")
			}
		})
	}
}

func TestLoadConfigRejectsNegativeFlowIdleTimeout(t *testing.T) {
	configYAML := `backends:
  - name: "bad"
    udpRoutes:
      - port: 53
        flowIdleTimeoutSeconds: -1
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      53:
        default: "localhost:53"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for negative flowIdleTimeoutSeconds")
	}
}

func TestLoadConfigRejectsDuplicateUDPPorts(t *testing.T) {
	configYAML := `backends:
  - name: "bad"
    udpRoutes:
      - port: 53
      - port: 53
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      53:
        default: "localhost:53"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for duplicate UDP ports")
	}
}

func TestLoadConfigDeduplicatesTCPPorts(t *testing.T) {
	configYAML := `backends:
  - name: "dedup"
    tcpPorts: [22, 80, 22, 443, 80]
    nexusAddresses:
      - "wss://nexus.example.com/connect"
    attestation:
      hmacSecret: "secret"
    portMappings:
      22:
        default: "localhost:22"
      80:
        default: "localhost:80"
      443:
        default: "localhost:443"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}

	// Should be deduplicated to [22, 80, 443] preserving original order
	expected := []int{22, 80, 443}
	if len(cfg.Backends[0].TCPPorts) != len(expected) {
		t.Fatalf("expected %d TCP ports after deduplication, got %d", len(expected), len(cfg.Backends[0].TCPPorts))
	}
	for i, port := range expected {
		if cfg.Backends[0].TCPPorts[i] != port {
			t.Fatalf("expected TCP port %d at index %d, got %d", port, i, cfg.Backends[0].TCPPorts[i])
		}
	}
}

func TestPortMappingResolvesRouteKeys(t *testing.T) {
	pm := PortMapping{
		Default: "localhost:53",
		Hosts: map[string]string{
			"example.com": "localhost:5353",
		},
	}
	if err := pm.finalize(); err != nil {
		t.Fatalf("unexpected finalize error: %v", err)
	}

	// Route keys like "tcp:53" should fall through to Default
	addr, ok := pm.Resolve("tcp:53")
	if !ok {
		t.Fatal("expected resolution to succeed for route key")
	}
	if addr != "localhost:53" {
		t.Fatalf("expected default target for route key, got %s", addr)
	}

	// Same for UDP route keys
	addr, ok = pm.Resolve("udp:53")
	if !ok {
		t.Fatal("expected resolution to succeed for UDP route key")
	}
	if addr != "localhost:53" {
		t.Fatalf("expected default target for UDP route key, got %s", addr)
	}
}
