package config

import (
	"fmt"
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
	BackendListenAddress string             `yaml:"backendListenAddress"`
	PeerListenAddress    string             `yaml:"peerListenAddress"`
	RelayPorts           []int              `yaml:"relayPorts"`
	IdleTimeoutSeconds   int                `yaml:"idleTimeoutSeconds"`
	BackendsJWTSecret    string             `yaml:"backendsJWTSecret"`
	Peers                []string           `yaml:"peers"`
	PeerAuthentication   PeerAuthentication `yaml:"peerAuthentication"`

	// Manual TLS configuration
	HubTlsCertFile string `yaml:"hubTlsCertFile"`
	HubTlsKeyFile  string `yaml:"hubTlsKeyFile"`

	// Automatic TLS configuration via ACME
	HubPublicHostname string `yaml:"hubPublicHostname"`
	AcmeCacheDir      string `yaml:"acmeCacheDir"`
}

// IdleTimeout returns the idle timeout as a time.Duration.
func (c *Config) IdleTimeout() time.Duration {
	return time.Duration(c.IdleTimeoutSeconds) * time.Second
}

// validate performs comprehensive validation of the loaded configuration.
func (c *Config) validate() error {
	if c.BackendListenAddress == "" {
		return fmt.Errorf("backendListenAddress must be set")
	}
	if len(c.RelayPorts) == 0 {
		return fmt.Errorf("at least one relayPort must be specified")
	}
	if c.BackendsJWTSecret == "" {
		return fmt.Errorf("backendsJWTSecret must be set")
	}
	if c.IdleTimeoutSeconds < 0 {
		return fmt.Errorf("idleTimeoutSeconds cannot be negative")
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
