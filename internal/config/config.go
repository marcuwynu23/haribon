// Package config handles loading, validating, and applying environment overrides
// to Haribon's YAML configuration.
//
// Design: fail-fast on structural errors (empty backends, bad scheme, invalid port)
// so the binary refuses to start with a broken config. Additive YAML fields only —
// never rename existing keys without a major-version migration note.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"gopkg.in/yaml.v2"
)

// Sentinel errors — callers decide exit code and log level.
var (
	ErrNoBackends         = errors.New("no backends configured")
	ErrBadScheme          = errors.New("backend url must use http or https scheme")
	ErrInvalidPort        = errors.New("port must be between 1 and 65535")
	ErrEmptyBackendURL    = errors.New("backend url must not be empty")
	ErrUnknownStrategy    = errors.New("unknown balancer strategy")
	ErrInvalidLogFormat   = errors.New("log format must be json or text")
	ErrInvalidLogExporter = errors.New("invalid log exporter")
)

// BalancerConfig controls which algorithm is used.
type BalancerConfig struct {
	Strategy string `yaml:"strategy"` // round_robin | weighted_round_robin | least_connections | random | ip_hash
}

// HealthConfig controls the active health-check scheduler.
type HealthConfig struct {
	Enabled            bool   `yaml:"enabled"`
	IntervalSec        int    `yaml:"interval_sec"`        // default 10
	TimeoutSec         int    `yaml:"timeout_sec"`         // default 2
	Path               string `yaml:"path"`                // default /
	HealthyThreshold   int    `yaml:"healthy_threshold"`   // default 1
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"` // default 2
}

// RetryConfig controls the per-request retry policy.
type RetryConfig struct {
	MaxRetries int `yaml:"max_retries"` // default 1
}

// BreakerConfig controls the per-backend circuit breaker.
type BreakerConfig struct {
	FailureThreshold int `yaml:"failure_threshold"` // default 5
	CooldownSec      int `yaml:"cooldown_sec"`      // default 30
}

// AdminConfig controls the management UI and admin API.
type AdminConfig struct {
	Enabled bool   `yaml:"enabled"` // default false
	Addr    string `yaml:"addr"`    // default 127.0.0.1:4445
	Token   string `yaml:"token"`   // optional bearer token for mutating ops
}

// Backend represents a single upstream server.
type Backend struct {
	Host   string `yaml:"url"`
	Weight int    `yaml:"weight"` // used by weighted_round_robin; 0 == 1
}

// DiscoveryConfig controls backend auto-discovery.
type DiscoveryConfig struct {
	Provider   string `yaml:"provider"`    // static | dns | file
	DNSName    string `yaml:"dns_name"`    // DNS name for dns provider
	FilePath   string `yaml:"file_path"`   // file path for file provider
	RefreshSec int    `yaml:"refresh_sec"` // poll interval in seconds
}

// ClusterConfig controls cluster gossip-based health sharing.
type ClusterConfig struct {
	Enabled    bool     `yaml:"enabled"`             // default false
	NodeID     string   `yaml:"node_id"`             // typically ${HOSTNAME}
	Peers      []string `yaml:"peers"`               // peer gossip addresses
	GossipSec  int      `yaml:"gossip_interval_sec"` // default 5
	GossipAddr string   `yaml:"gossip_addr"`         // default 0.0.0.0:7946
}

// Config is the top-level configuration structure.
// YAML field names are stable — additive only per AGENTS.md §1.1.
type Config struct {
	MainHost           string          `yaml:"host"`
	MainPort           int             `yaml:"port"`
	Logging            bool            `yaml:"logging"`
	LogPath            string          `yaml:"log_path"`
	LogFormat          string          `yaml:"log_format"` // json (default) | text
	Exporters          []string        `yaml:"exporters"`  // stdout | file | loki | fluentbit | elasticsearch
	Admin              AdminConfig     `yaml:"admin"`
	ShutdownTimeoutSec int             `yaml:"shutdown_timeout_sec"`
	Backends           []Backend       `yaml:"backends"`
	Balancer           BalancerConfig  `yaml:"balancer"`
	Health             HealthConfig    `yaml:"health"`
	Retry              RetryConfig     `yaml:"retry"`
	Breaker            BreakerConfig   `yaml:"breaker"`
	Discovery          DiscoveryConfig `yaml:"discovery"`
	Cluster            ClusterConfig   `yaml:"cluster"`
}

// Load reads and unmarshals the YAML config at path.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

// Validate checks structural correctness and returns the first error found.
// Called by both startCommand and checkCommand so validation is never skipped.
func Validate(cfg Config) error {
	if len(cfg.Backends) == 0 && cfg.Discovery.Provider == "" {
		return ErrNoBackends
	}
	for i, b := range cfg.Backends {
		if b.Host == "" {
			return fmt.Errorf("backend[%d]: %w", i, ErrEmptyBackendURL)
		}
		u, err := url.Parse(b.Host)
		if err != nil {
			return fmt.Errorf("backend[%d] url parse: %w", i, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("backend[%d] %q: %w", i, b.Host, ErrBadScheme)
		}
	}
	if cfg.Discovery.Provider != "" {
		switch cfg.Discovery.Provider {
		case "static", "dns", "file":
			// valid
		default:
			return fmt.Errorf("discovery provider %q: unknown provider", cfg.Discovery.Provider)
		}
		if cfg.Discovery.Provider == "dns" && cfg.Discovery.DNSName == "" {
			return fmt.Errorf("discovery dns requires dns_name")
		}
		if cfg.Discovery.Provider == "file" && cfg.Discovery.FilePath == "" {
			return fmt.Errorf("discovery file requires file_path")
		}
		if cfg.Discovery.RefreshSec <= 0 {
			cfg.Discovery.RefreshSec = 30
		}
	}
	if cfg.MainPort != 0 && (cfg.MainPort < 1 || cfg.MainPort > 65535) {
		return fmt.Errorf("port %d: %w", cfg.MainPort, ErrInvalidPort)
	}
	if cfg.Balancer.Strategy != "" {
		switch cfg.Balancer.Strategy {
		case "round_robin", "weighted_round_robin", "least_connections", "random", "ip_hash":
			// valid
		default:
			return fmt.Errorf("balancer strategy %q: %w", cfg.Balancer.Strategy, ErrUnknownStrategy)
		}
	}
	// Validate log format
	if cfg.LogFormat != "" && cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return fmt.Errorf("log format %q: %w", cfg.LogFormat, ErrInvalidLogFormat)
	}
	// Validate exporters if logging is enabled
	if cfg.Logging {
		validExporters := map[string]bool{"stdout": true, "file": true, "loki": true, "fluentbit": true, "elasticsearch": true}
		for _, ex := range cfg.Exporters {
			if !validExporters[ex] {
				return fmt.Errorf("log exporter %q: %w", ex, ErrInvalidLogExporter)
			}
		}
		// default exporter if none specified
		if len(cfg.Exporters) == 0 {
			cfg.Exporters = []string{"stdout"}
		}
	}
	// Validate admin config
	if cfg.Admin.Addr == "" {
		cfg.Admin.Addr = "127.0.0.1:4445"
	}
	if cfg.Admin.Enabled {
		// enabled
	} else {
		cfg.Admin.Enabled = false
	}
	return nil
}

// ApplyEnvOverrides applies HARIBON_HOST and HARIBON_PORT env variables
// over the loaded config. Invalid HARIBON_PORT is silently ignored per existing
// behaviour (only well-formed integers accepted).
func ApplyEnvOverrides(cfg *Config) {
	if host := os.Getenv("HARIBON_HOST"); host != "" {
		cfg.MainHost = host
	}
	if port := os.Getenv("HARIBON_PORT"); port != "" {
		if p, err := strconv.Atoi(port); err == nil {
			cfg.MainPort = p
		}
	}
}

// ResolveConfigPath returns cli if non-empty, otherwise the env var
// HARIBON_CONFIG, otherwise the default path.
func ResolveConfigPath(cli string) string {
	if cli != "" {
		return cli
	}
	if env := os.Getenv("HARIBON_CONFIG"); env != "" {
		return env
	}
	return "./haribon-config.yml"
}

// Defaults fills in zero-value fields with safe production defaults.
// Called after Load + ApplyEnvOverrides.
func Defaults(cfg *Config) {
	if cfg.Balancer.Strategy == "" {
		cfg.Balancer.Strategy = "round_robin"
	}
	if cfg.Health.IntervalSec <= 0 {
		cfg.Health.IntervalSec = 10
	}
	if cfg.Health.TimeoutSec <= 0 {
		cfg.Health.TimeoutSec = 2
	}
	if cfg.Health.Path == "" {
		cfg.Health.Path = "/"
	}
	if cfg.Health.HealthyThreshold <= 0 {
		cfg.Health.HealthyThreshold = 1
	}
	if cfg.Health.UnhealthyThreshold <= 0 {
		cfg.Health.UnhealthyThreshold = 2
	}
	if cfg.Retry.MaxRetries <= 0 {
		cfg.Retry.MaxRetries = 1
	}
	if cfg.Breaker.FailureThreshold <= 0 {
		cfg.Breaker.FailureThreshold = 5
	}
	if cfg.Breaker.CooldownSec <= 0 {
		cfg.Breaker.CooldownSec = 30
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = "json"
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		cfg.LogFormat = "json"
	}
	if cfg.Discovery.RefreshSec <= 0 {
		cfg.Discovery.RefreshSec = 30
	}
	if cfg.Cluster.GossipSec <= 0 && cfg.Cluster.Enabled {
		cfg.Cluster.GossipSec = 5
	}
	if cfg.Cluster.GossipAddr == "" && cfg.Cluster.Enabled {
		cfg.Cluster.GossipAddr = "0.0.0.0:7946"
	}
	if len(cfg.Exporters) == 0 {
		cfg.Exporters = []string{"stdout"}
	}
	for _, ex := range cfg.Exporters {
		switch ex {
		case "loki":
			cfg.Exporters = append(cfg.Exporters[:0], "stdout") // Loki requires config
			break
		case "fluentbit":
			cfg.Exporters = append(cfg.Exporters[:0], "stdout") // FluentBit requires config
			break
		case "elasticsearch":
			cfg.Exporters = append(cfg.Exporters[:0], "stdout") // Elasticsearch requires config
			break
		}
	}
}
