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
	ErrNoBackends      = errors.New("no backends configured")
	ErrBadScheme       = errors.New("backend url must use http or https scheme")
	ErrInvalidPort     = errors.New("port must be between 1 and 65535")
	ErrEmptyBackendURL = errors.New("backend url must not be empty")
)

// Config is the top-level configuration structure.
// YAML field names are stable — additive only per AGENTS.md §1.1.
type Config struct {
	MainHost           string    `yaml:"host"`
	MainPort           int       `yaml:"port"`
	Logging            bool      `yaml:"logging"`
	LogPath            string    `yaml:"log_path"`
	ShutdownTimeoutSec int       `yaml:"shutdown_timeout_sec"`
	Backends           []Backend `yaml:"backends"`
}

// Backend represents a single upstream server.
type Backend struct {
	Host string `yaml:"url"`
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
	if len(cfg.Backends) == 0 {
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
	if cfg.MainPort != 0 && (cfg.MainPort < 1 || cfg.MainPort > 65535) {
		return fmt.Errorf("port %d: %w", cfg.MainPort, ErrInvalidPort)
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
