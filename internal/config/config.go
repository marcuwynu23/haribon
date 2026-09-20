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
	"regexp"
	"strconv"
	"strings"

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
	ErrInvalidWeight      = errors.New("backend weight must not be negative")
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
//
// NOT IMPLEMENTED: the block is parsed so an existing config does not fail to
// load, but no admin server is started. See ROADMAP issues #4 and #9.
type AdminConfig struct {
	Enabled bool   `yaml:"enabled"` // parsed, not acted on
	Addr    string `yaml:"addr"`    // parsed, not acted on
	Token   string `yaml:"token"`   // parsed, not acted on
}

// Backend represents a single upstream server.
type Backend struct {
	Host   string `yaml:"url"`
	Weight int    `yaml:"weight"` // used by weighted_round_robin; 0 means unset (treated as 1)
}

// DiscoveryConfig controls backend auto-discovery.
type DiscoveryConfig struct {
	Provider   string `yaml:"provider"`    // static | dns | file
	DNSName    string `yaml:"dns_name"`    // DNS name for dns provider
	DNSPort    int    `yaml:"dns_port"`    // port stamped onto resolved A records (default 80)
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

// LokiConfig configures the Loki push exporter (enabled via exporters: [loki]).
type LokiConfig struct {
	URL    string            `yaml:"url"`    // default http://localhost:3100
	Labels map[string]string `yaml:"labels"` // extra stream labels; job is always set
}

// ElasticsearchConfig configures the Elasticsearch bulk exporter
// (enabled via exporters: [elasticsearch]).
type ElasticsearchConfig struct {
	URL   string `yaml:"url"`   // default http://localhost:9200
	Index string `yaml:"index"` // default haribon
}

// FluentbitConfig configures the Fluent Bit forward exporter
// (enabled via exporters: [fluentbit]).
type FluentbitConfig struct {
	Addr string `yaml:"addr"` // default localhost:24224
}

// Config is the top-level configuration structure.
// YAML field names are stable — additive only per AGENTS.md §1.1.
type Config struct {
	MainHost           string              `yaml:"host"`
	MainPort           int                 `yaml:"port"`
	Logging            bool                `yaml:"logging"`
	LogPath            string              `yaml:"log_path"`
	LogFormat          string              `yaml:"log_format"` // json (default) | text
	Exporters          []string            `yaml:"exporters"`  // stdout | file | loki | fluentbit | elasticsearch
	Loki               LokiConfig          `yaml:"loki"`
	Elasticsearch      ElasticsearchConfig `yaml:"elasticsearch"`
	Fluentbit          FluentbitConfig     `yaml:"fluentbit"`
	Admin              AdminConfig         `yaml:"admin"`
	ShutdownTimeoutSec int                 `yaml:"shutdown_timeout_sec"`
	Backends           []Backend           `yaml:"backends"`
	Balancer           BalancerConfig      `yaml:"balancer"`
	Health             HealthConfig        `yaml:"health"`
	Retry              RetryConfig         `yaml:"retry"`
	Breaker            BreakerConfig       `yaml:"breaker"`
	Discovery          DiscoveryConfig     `yaml:"discovery"`
	Cluster            ClusterConfig       `yaml:"cluster"`
}

// envRef matches ${VAR} references in the raw YAML.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes ${VAR} with the environment value. An unset variable is
// left verbatim so the (unresolved) reference is visible in an error rather
// than silently becoming an empty string.
//
// This exists because the documentation and samples use node_id: "${HOSTNAME}".
// Without substitution every replica would share one literal node ID, and
// MergeGossip drops messages whose NodeID matches its own — clustering would
// appear configured and do nothing at all.
func expandEnv(data []byte) []byte {
	return envRef.ReplaceAllFunc(data, func(m []byte) []byte {
		if v, ok := os.LookupEnv(string(m[2 : len(m)-1])); ok {
			return []byte(v)
		}
		return m
	})
}

// Load reads and unmarshals the YAML config at path, expanding ${VAR} across
// the raw text first.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(expandEnv(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

// Validate checks structural correctness and returns the first error found.
// Called by both startCommand and checkCommand so validation is never skipped.
//
// Validate is a pure predicate: it never mutates cfg. Defaulting lives in
// Defaults, which takes a pointer. (Validate receives cfg by value, so any
// write here would be silently discarded — a bug this split removes.)
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
		// weight: 0 means "not set" (YAML omits it as 0), so only a negative
		// weight is a genuine mistake.
		if b.Weight < 0 {
			return fmt.Errorf("backend[%d] %q: weight %d: %w", i, b.Host, b.Weight, ErrInvalidWeight)
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
		if cfg.Discovery.Provider == "dns" && cfg.Discovery.DNSPort != 0 &&
			(cfg.Discovery.DNSPort < 1 || cfg.Discovery.DNSPort > 65535) {
			return fmt.Errorf("discovery dns_port %d: %w", cfg.Discovery.DNSPort, ErrInvalidPort)
		}
		if cfg.Discovery.Provider == "file" && cfg.Discovery.FilePath == "" {
			return fmt.Errorf("discovery file requires file_path")
		}
	}
	if cfg.Discovery.Provider != "" && cfg.Discovery.Provider != "static" && cfg.Discovery.RefreshSec < 0 {
		return fmt.Errorf("discovery refresh_sec must not be negative")
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
	if cfg.LogFormat != "" && cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return fmt.Errorf("log format %q: %w", cfg.LogFormat, ErrInvalidLogFormat)
	}
	validExporters := map[string]bool{"stdout": true, "file": true, "loki": true, "fluentbit": true, "elasticsearch": true}
	for _, ex := range cfg.Exporters {
		if !validExporters[ex] {
			return fmt.Errorf("log exporter %q: %w", ex, ErrInvalidLogExporter)
		}
	}
	if cfg.Cluster.Enabled && strings.Contains(cfg.Cluster.NodeID, "${") {
		return fmt.Errorf("cluster node_id %q: ${...} could not be resolved — "+
			"every replica would share one node ID and gossip would be dropped as self-traffic",
			cfg.Cluster.NodeID)
	}
	if cfg.Cluster.Enabled && cfg.Cluster.GossipSec < 0 {
		return fmt.Errorf("cluster gossip_interval_sec must not be negative")
	}
	return nil
}

// EnabledExporter reports whether name is in the exporter list. When the list
// is empty the caller is expected to fall back to stdout.
func (c Config) EnabledExporter(name string) bool {
	for _, e := range c.Exporters {
		if e == name {
			return true
		}
	}
	return false
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
	if cfg.Discovery.Provider == "dns" && cfg.Discovery.DNSPort <= 0 {
		cfg.Discovery.DNSPort = 80
	}
	if cfg.Cluster.GossipSec <= 0 && cfg.Cluster.Enabled {
		cfg.Cluster.GossipSec = 5
	}
	if cfg.Cluster.GossipAddr == "" && cfg.Cluster.Enabled {
		cfg.Cluster.GossipAddr = "0.0.0.0:7946"
	}
	if cfg.Cluster.Enabled && cfg.Cluster.NodeID == "" {
		// Each replica must have a distinct identity, otherwise peers discard
		// its gossip as their own. The hostname is distinct per container/pod.
		if h, err := os.Hostname(); err == nil {
			cfg.Cluster.NodeID = h
		}
	}
	if cfg.LogPath == "" && cfg.Logging {
		cfg.LogPath = "./haribon.log"
	}
	if cfg.Admin.Addr == "" {
		cfg.Admin.Addr = "127.0.0.1:4445"
	}
	if cfg.ShutdownTimeoutSec <= 0 {
		cfg.ShutdownTimeoutSec = 15
	}
	// Exporter targets. These are only consulted when the matching name is
	// listed in exporters:, but defaulting them unconditionally keeps the
	// rendered config (haribon check) complete.
	if cfg.Loki.URL == "" {
		cfg.Loki.URL = "http://localhost:3100"
	}
	if cfg.Loki.Labels == nil {
		cfg.Loki.Labels = map[string]string{}
	}
	if _, ok := cfg.Loki.Labels["job"]; !ok {
		cfg.Loki.Labels["job"] = "haribon"
	}
	if cfg.Elasticsearch.URL == "" {
		cfg.Elasticsearch.URL = "http://localhost:9200"
	}
	if cfg.Elasticsearch.Index == "" {
		cfg.Elasticsearch.Index = "haribon"
	}
	if cfg.Fluentbit.Addr == "" {
		cfg.Fluentbit.Addr = "localhost:24224"
	}
	if len(cfg.Exporters) == 0 {
		cfg.Exporters = []string{"stdout"}
	}
}
