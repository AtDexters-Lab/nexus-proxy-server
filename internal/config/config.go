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
