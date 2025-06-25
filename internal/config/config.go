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
	HubTlsCertFile       string             `yaml:"hubTlsCertFile"`
	HubTlsKeyFile        string             `yaml:"hubTlsKeyFile"`
	RelayPorts           []int              `yaml:"relayPorts"`
	IdleTimeoutSeconds   int                `yaml:"idleTimeoutSeconds"`
	BackendsJWTSecret    string             `yaml:"backendsJWTSecret"`
	PeerAuthentication   PeerAuthentication `yaml:"peerAuthentication"` // New: mTLS settings
	Peers                []string           `yaml:"peers"`
}

// IdleTimeout returns the idle timeout as a time.Duration.
func (c *Config) IdleTimeout() time.Duration {
	return time.Duration(c.IdleTimeoutSeconds) * time.Second
}

// LoadConfig reads the configuration from the given file path, unmarshals it,
// and performs basic validation.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml from %s: %w", path, err)
	}

	if cfg.BackendListenAddress == "" {
		return nil, fmt.Errorf("config validation failed: backendListenAddress must be set")
	}
	if cfg.HubTlsCertFile == "" || cfg.HubTlsKeyFile == "" {
		return nil, fmt.Errorf("config validation failed: both hubTlsCertFile and hubTlsKeyFile must be set")
	}
	if len(cfg.RelayPorts) == 0 {
		return nil, fmt.Errorf("config validation failed: at least one relayPort must be specified")
	}
	if cfg.BackendsJWTSecret == "" {
		return nil, fmt.Errorf("config validation failed: backendsJWTSecret must be set")
	}
	if cfg.IdleTimeoutSeconds < 0 {
		return nil, fmt.Errorf("config validation failed: idleTimeoutSeconds cannot be negative")
	}

	// Validation for new peer settings
	if len(cfg.Peers) > 0 {
		if cfg.PeerListenAddress == "" {
			return nil, fmt.Errorf("config validation failed: peerListenAddress must be set if peers are defined")
		}
		if len(cfg.PeerAuthentication.TrustedDomainSuffixes) == 0 {
			return nil, fmt.Errorf("config validation failed: peerAuthentication.trustedDomainSuffixes must be set if peers are defined")
		}
	}

	return &cfg, nil
}
