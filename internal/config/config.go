package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// PeerAuthentication holds the settings for peer mTLS authentication.
type PeerAuthentication struct {
	TrustedDomainSuffixes []string `yaml:"trustedDomainSuffixes"`
}

// Config holds the entire application configuration, loaded from a YAML file.
type Config struct {
	BackendListenAddress           string             `yaml:"backendListenAddress"`
	PeerListenAddress              string             `yaml:"peerListenAddress"`
	RelayPorts                     []int              `yaml:"relayPorts"`
	UDPRelayPorts                  []int              `yaml:"udpRelayPorts"`
	IdleTimeoutSeconds             int                `yaml:"idleTimeoutSeconds"`
	BackendsJWTSecret              string             `yaml:"backendsJWTSecret"`
	RemoteVerifierURL              string             `yaml:"remoteVerifierURL"`
	MaintenanceGraceDefaultSeconds int                `yaml:"maintenanceGraceDefaultSeconds"`
	Peers                          []string           `yaml:"peers"`
	PeerAuthentication             PeerAuthentication `yaml:"peerAuthentication"`

	// Manual TLS configuration
	HubTlsCertFile string `yaml:"hubTlsCertFile"`
	HubTlsKeyFile  string `yaml:"hubTlsKeyFile"`

	// Automatic TLS configuration via ACME
	HubPublicHostname string `yaml:"hubPublicHostname"`
	AcmeCacheDir      string `yaml:"acmeCacheDir"`

	// TotalBandwidthMbps is the total bandwidth limit for the proxy in Mbps.
	// Set to 0 for unlimited. When set, bandwidth is distributed fairly
	// among all active backends using Deficit Round Robin.
	TotalBandwidthMbps int `yaml:"totalBandwidthMbps"`

	// Port claim allowlists (security controls). When empty, port claims are disabled.
	AllowedTCPPortClaims []int `yaml:"allowedTCPPortClaims"`
	AllowedUDPPortClaims []int `yaml:"allowedUDPPortClaims"`

	// Orchestrator registration (optional — omit to run without DNS orchestration)
	RegistrationURL string `yaml:"registrationURL"`
	Region          string `yaml:"region"` // sent in registration body

	// UDP flow table and payload bounds (applies to udpRelayPorts).
	UDPMaxFlows                      int `yaml:"udpMaxFlows"`
	UDPMaxDatagramBytes              int `yaml:"udpMaxDatagramBytes"`
	UDPFlowIdleTimeoutDefaultSeconds int `yaml:"udpFlowIdleTimeoutDefaultSeconds"`
	UDPFlowIdleTimeoutMinSeconds     int `yaml:"udpFlowIdleTimeoutMinSeconds"`
	UDPFlowIdleTimeoutMaxSeconds     int `yaml:"udpFlowIdleTimeoutMaxSeconds"`

	// STUN server port (optional — omit or set to 0 to disable).
	// When set, a STUN Binding server listens on this UDP port.
	StunPort int `yaml:"stunPort"`

	// Outbound proxy settings. When AllowOutbound is true, backends with
	// matching JWT claims can request the proxy to open outbound TCP
	// connections on their behalf (giving them a static egress IP).
	AllowOutbound              bool  `yaml:"allowOutbound"`
	AllowedOutboundPorts       []int `yaml:"allowedOutboundPorts"`       // server-level port gate; empty = all when allowed
	MaxOutboundConnsPerBackend int   `yaml:"maxOutboundConnsPerBackend"` // 0 → default 100; -1 = no limit
	OutboundDialTimeoutSeconds int   `yaml:"outboundDialTimeoutSeconds"` // default 10
	OutboundIdleTimeoutSeconds int   `yaml:"outboundIdleTimeoutSeconds"` // 0 = no deadline

	// PeekTimeouts governs the deadline regime for the TLS/HTTP protocol-sniff
	// pass on every inbound connection. AbsoluteMaxSeconds is shared across
	// the TLS-then-HTTP fallback so a single connection's total peek time is
	// capped, not doubled. See proxy.PeekTimeouts for full semantics.
	PeekTimeouts PeekTimeoutsConfig `yaml:"peekTimeouts"`

	// MaxConcurrentPeeks bounds the number of inbound connections in the
	// protocol-sniff peek phase at once. First-line defense against the
	// slow-loris / open-and-idle pattern: each in-flight peek holds an fd
	// + goroutine + ~96 KB capture buffer for up to PeekAbsoluteMax (default
	// 30s). 0 → default 256 (≈24 MB peek-buffer ceiling at saturation,
	// comfortable on a 1 GB VM). -1 → no bound.
	//
	// The slot is released at the boundary between peek and dispatch, so
	// long-lived post-peek work (ACME serve, established backend
	// connections, peer tunnels) runs slot-free and does NOT count against
	// this cap. Established-connection concurrency is governed by
	// per-backend / per-peer flow control, not by this knob.
	//
	// In-flight peeks at restart drain on the prior cap; new connections
	// enforce the new cap immediately.
	MaxConcurrentPeeks int `yaml:"maxConcurrentPeeks"`
}

// PeekTimeoutsConfig is the YAML shape for proxy.PeekTimeouts. Any field set
// to 0 (or omitted) falls back to the default returned by the matching
// accessor on Config.
type PeekTimeoutsConfig struct {
	FirstByteSeconds     int `yaml:"firstByteSeconds"`
	IdleExtensionSeconds int `yaml:"idleExtensionSeconds"`
	AbsoluteMaxSeconds   int `yaml:"absoluteMaxSeconds"`
}

// IdleTimeout returns the idle timeout as a time.Duration.
func (c *Config) IdleTimeout() time.Duration {
	return time.Duration(c.IdleTimeoutSeconds) * time.Second
}

// MaintenanceGraceDefault returns the default maintenance deferral window.
func (c *Config) MaintenanceGraceDefault() time.Duration {
	if c.MaintenanceGraceDefaultSeconds <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.MaintenanceGraceDefaultSeconds) * time.Second
}

// TotalBandwidthBytesPerSecond returns the bandwidth limit in bytes/second.
// Returns 0 if unlimited.
func (c *Config) TotalBandwidthBytesPerSecond() int64 {
	if c.TotalBandwidthMbps <= 0 {
		return 0 // unlimited
	}
	return int64(c.TotalBandwidthMbps) * 1_000_000 / 8
}

func (c *Config) UDPMaxFlowsOrDefault() int {
	if c.UDPMaxFlows <= 0 {
		return 200_000
	}
	return c.UDPMaxFlows
}

func (c *Config) UDPMaxDatagramBytesOrDefault() int {
	if c.UDPMaxDatagramBytes <= 0 {
		return 2048
	}
	return c.UDPMaxDatagramBytes
}

func (c *Config) UDPFlowIdleTimeoutDefault() time.Duration {
	if c.UDPFlowIdleTimeoutDefaultSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.UDPFlowIdleTimeoutDefaultSeconds) * time.Second
}

func (c *Config) UDPFlowIdleTimeoutMin() time.Duration {
	if c.UDPFlowIdleTimeoutMinSeconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.UDPFlowIdleTimeoutMinSeconds) * time.Second
}

func (c *Config) UDPFlowIdleTimeoutMax() time.Duration {
	if c.UDPFlowIdleTimeoutMaxSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.UDPFlowIdleTimeoutMaxSeconds) * time.Second
}

// RegistrationEnabled returns true if orchestrator registration is configured.
func (c *Config) RegistrationEnabled() bool {
	return c.RegistrationURL != ""
}

// StunEnabled returns true if the embedded STUN server is configured.
func (c *Config) StunEnabled() bool {
	return c.StunPort > 0
}

// MaxOutboundConns returns the per-backend outbound connection limit.
// Returns -1 for no limit.
func (c *Config) MaxOutboundConns() int {
	if c.MaxOutboundConnsPerBackend == 0 {
		return 100
	}
	return c.MaxOutboundConnsPerBackend
}

// OutboundDialTimeout returns the timeout for outbound TCP dials.
func (c *Config) OutboundDialTimeout() time.Duration {
	if c.OutboundDialTimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.OutboundDialTimeoutSeconds) * time.Second
}

// OutboundIdleTimeout returns the idle timeout for established outbound connections.
// Returns 0 for no deadline (rely on remote close or WS disconnect).
func (c *Config) OutboundIdleTimeout() time.Duration {
	if c.OutboundIdleTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.OutboundIdleTimeoutSeconds) * time.Second
}

// PeekFirstByteTimeout returns the initial read deadline for protocol sniffing
// (before any byte is observed). Defaults to 15s — chosen to accommodate
// embedded clients doing ECDHE keygen between TCP accept and ClientHello.
// Note: runtime clamps this to PeekAbsoluteMax in proxy.PeekTimeouts, so a
// hardening config that lowers AbsoluteMax alone effectively lowers the
// first-byte wait to match.
func (c *Config) PeekFirstByteTimeout() time.Duration {
	if c.PeekTimeouts.FirstByteSeconds <= 0 {
		return 15 * time.Second
	}
	return time.Duration(c.PeekTimeouts.FirstByteSeconds) * time.Second
}

// PeekIdleExtension returns the duration added to the peek read deadline on
// every successful kernel-level Read. Defaults to 5s — generous enough for
// legitimate inter-packet gaps on unstable links. Runtime clamps the extended
// deadline to PeekAbsoluteMax.
func (c *Config) PeekIdleExtension() time.Duration {
	if c.PeekTimeouts.IdleExtensionSeconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.PeekTimeouts.IdleExtensionSeconds) * time.Second
}

// PeekAbsoluteMax returns the hard ceiling on total peek duration per
// connection. Defaults to 30s, aligning with common client SO_SNDTIMEO and
// bounding per-connection slowloris exposure.
func (c *Config) PeekAbsoluteMax() time.Duration {
	if c.PeekTimeouts.AbsoluteMaxSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.PeekTimeouts.AbsoluteMaxSeconds) * time.Second
}

// MaxConcurrentPeeksLimit returns the configured limit; see MaxConcurrentPeeks.
func (c *Config) MaxConcurrentPeeksLimit() int {
	if c.MaxConcurrentPeeks == 0 {
		return 256
	}
	return c.MaxConcurrentPeeks
}

// validate performs comprehensive validation of the loaded configuration.
func (c *Config) validate() error {
	if c.BackendListenAddress == "" {
		return fmt.Errorf("backendListenAddress must be set")
	}
	if len(c.RelayPorts) == 0 {
		return fmt.Errorf("at least one relayPort must be specified")
	}
	if c.BackendsJWTSecret == "" && c.RemoteVerifierURL == "" {
		return fmt.Errorf("must configure backendsJWTSecret or remoteVerifierURL")
	}
	if c.IdleTimeoutSeconds < 0 {
		return fmt.Errorf("idleTimeoutSeconds cannot be negative")
	}
	if c.MaintenanceGraceDefaultSeconds < 0 {
		return fmt.Errorf("maintenanceGraceDefaultSeconds cannot be negative")
	}

	// Validate registration URL scheme and host if set.
	if c.RegistrationURL != "" {
		u, err := url.Parse(c.RegistrationURL)
		if err != nil {
			return fmt.Errorf("registrationURL is not a valid URL: %w", err)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("registrationURL must use https scheme, got %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("registrationURL must include a host")
		}
	}

	// Validate TLS configuration: must be either manual or automatic, but not both.
	manualTls := c.HubTlsCertFile != "" || c.HubTlsKeyFile != ""
	automaticTls := c.HubPublicHostname != ""

	if manualTls && automaticTls {
		return fmt.Errorf("cannot specify both manual TLS (hubTlsCertFile/hubTlsKeyFile) and automatic TLS (hubPublicHostname) settings")
	}
	if !manualTls && !automaticTls {
		return fmt.Errorf("must specify either manual TLS (hubTlsCertFile/hubTlsKeyFile) or automatic TLS (hubPublicHostname) settings")
	}
	if manualTls && (c.HubTlsCertFile == "" || c.HubTlsKeyFile == "") {
		return fmt.Errorf("both hubTlsCertFile and hubTlsKeyFile must be set for manual TLS")
	}

	// Validate peer settings
	if len(c.Peers) > 0 {
		if c.PeerListenAddress == "" {
			return fmt.Errorf("peerListenAddress must be set if peers are defined")
		}
		if len(c.PeerAuthentication.TrustedDomainSuffixes) == 0 {
			return fmt.Errorf("peerAuthentication.trustedDomainSuffixes must be set if peers are defined")
		}
	}

	// Validate bandwidth settings
	if c.TotalBandwidthMbps < 0 {
		return fmt.Errorf("totalBandwidthMbps cannot be negative")
	}

	if c.UDPMaxFlows < 0 {
		return fmt.Errorf("udpMaxFlows cannot be negative")
	}
	if c.UDPMaxDatagramBytes < 0 {
		return fmt.Errorf("udpMaxDatagramBytes cannot be negative")
	}
	if c.UDPFlowIdleTimeoutDefaultSeconds < 0 {
		return fmt.Errorf("udpFlowIdleTimeoutDefaultSeconds cannot be negative")
	}
	if c.UDPFlowIdleTimeoutMinSeconds < 0 {
		return fmt.Errorf("udpFlowIdleTimeoutMinSeconds cannot be negative")
	}
	if c.UDPFlowIdleTimeoutMaxSeconds < 0 {
		return fmt.Errorf("udpFlowIdleTimeoutMaxSeconds cannot be negative")
	}

	if c.StunPort < 0 || c.StunPort > 65535 {
		return fmt.Errorf("stunPort must be between 0 and 65535")
	}
	if c.StunPort > 0 {
		for _, p := range c.UDPRelayPorts {
			if p == c.StunPort {
				return fmt.Errorf("stunPort %d conflicts with udpRelayPorts — both bind the same UDP socket", c.StunPort)
			}
		}
	}

	// Validate outbound proxy settings.
	if c.MaxOutboundConnsPerBackend < -1 {
		return fmt.Errorf("maxOutboundConnsPerBackend must be >= -1")
	}
	if c.OutboundDialTimeoutSeconds < 0 {
		return fmt.Errorf("outboundDialTimeoutSeconds cannot be negative")
	}
	if c.OutboundIdleTimeoutSeconds < 0 {
		return fmt.Errorf("outboundIdleTimeoutSeconds cannot be negative")
	}

	if c.PeekTimeouts.FirstByteSeconds < 0 {
		return fmt.Errorf("peekTimeouts.firstByteSeconds cannot be negative")
	}
	if c.PeekTimeouts.IdleExtensionSeconds < 0 {
		return fmt.Errorf("peekTimeouts.idleExtensionSeconds cannot be negative")
	}
	if c.PeekTimeouts.AbsoluteMaxSeconds < 0 {
		return fmt.Errorf("peekTimeouts.absoluteMaxSeconds cannot be negative")
	}
	if c.MaxConcurrentPeeks < -1 {
		return fmt.Errorf("maxConcurrentPeeks must be >= -1 (0 = default, -1 = unbounded)")
	}
	// Reject only *explicit* contradictions: operator sets both FirstByte and
	// AbsoluteMax with First > Abs. A hardening config that lowers AbsoluteMax
	// alone (leaving FirstByte at default) is permitted — runtime clamping in
	// PeekTimeouts.initialDeadline handles the asymmetry.
	if c.PeekTimeouts.FirstByteSeconds > 0 && c.PeekTimeouts.AbsoluteMaxSeconds > 0 &&
		c.PeekTimeouts.FirstByteSeconds > c.PeekTimeouts.AbsoluteMaxSeconds {
		return fmt.Errorf("peekTimeouts.firstByteSeconds (%d) cannot exceed peekTimeouts.absoluteMaxSeconds (%d)",
			c.PeekTimeouts.FirstByteSeconds, c.PeekTimeouts.AbsoluteMaxSeconds)
	}
	for _, p := range c.AllowedOutboundPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("allowedOutboundPorts contains invalid port %d", p)
		}
	}
	if c.AllowOutbound && c.MaxOutboundConns() == -1 {
		log.Printf("WARN: outbound connections enabled with no per-backend limit (maxOutboundConnsPerBackend=-1)")
	}
	if c.AllowOutbound && len(c.AllowedOutboundPorts) == 0 {
		log.Printf("WARN: outbound connections enabled with no server-level port allowlist (allowedOutboundPorts empty); authorized backends can dial any TCP port including loopback and cloud metadata")
	}

	// warnPortMismatch warns about ports present in one config list but absent from another.
	warnPortMismatch := func(ports, against []int, portsKey, againstKey, reason string) {
		if len(ports) == 0 {
			return
		}
		set := make(map[int]struct{}, len(against))
		for _, p := range against {
			set[p] = struct{}{}
		}
		seen := make(map[int]struct{}, len(ports))
		for _, p := range ports {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			if _, ok := set[p]; !ok {
				log.Printf("WARN: %s contains port %d but %s does not; %s", portsKey, p, againstKey, reason)
			}
		}
	}

	// Warn when claimable ports have no matching relay listener (dead routes).
	warnPortMismatch(c.AllowedTCPPortClaims, c.RelayPorts, "allowedTCPPortClaims", "relayPorts",
		"backends can claim it but no listener exists to serve traffic")
	warnPortMismatch(c.AllowedUDPPortClaims, c.UDPRelayPorts, "allowedUDPPortClaims", "udpRelayPorts",
		"backends can claim it but no listener exists to serve traffic")

	// Reverse check: warn about UDP relay ports no backend can claim.
	// TCP relay ports are not checked because they also serve hostname-based routing.
	warnPortMismatch(c.UDPRelayPorts, c.AllowedUDPPortClaims, "udpRelayPorts", "allowedUDPPortClaims",
		"no backend will be able to claim it")

	return nil
}

// LoadConfig reads the configuration from the given file path, unmarshals it,
// and performs validation.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml from %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}
