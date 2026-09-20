package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcuwynu23/haribon/internal/config"
)

// ---------- Load ----------

func TestLoad_ValidFile(t *testing.T) {
	yaml := `
host: "0.0.0.0"
port: 4444
logging: true
backends:
  - url: "http://localhost:4441"
  - url: "http://localhost:4442"
`
	path := writeTempYAML(t, yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MainHost != "0.0.0.0" {
		t.Fatalf("host mismatch: %s", cfg.MainHost)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(cfg.Backends))
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load("/no/such/file.yml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	// yaml.v2 is lenient; use an unclosed brace which it definitely rejects
	path := writeTempYAML(t, "{unclosed")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoad_NewFields_YAML(t *testing.T) {
	yaml := `
host: "0.0.0.0"
port: 4444
balancer:
  strategy: weighted_round_robin
health:
  enabled: true
  interval_sec: 5
retry:
  max_retries: 2
breaker:
  failure_threshold: 3
  cooldown_sec: 15
backends:
  - url: "http://localhost:4441"
    weight: 2
  - url: "http://localhost:4442"
    weight: 1
`
	path := writeTempYAML(t, yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Balancer.Strategy != "weighted_round_robin" {
		t.Fatalf("strategy: %s", cfg.Balancer.Strategy)
	}
	if cfg.Retry.MaxRetries != 2 {
		t.Fatalf("max_retries: %d", cfg.Retry.MaxRetries)
	}
	if cfg.Backends[0].Weight != 2 {
		t.Fatalf("weight: %d", cfg.Backends[0].Weight)
	}
}

// ---------- Validate ----------

func TestValidate_OK(t *testing.T) {
	cfg := config.Config{
		MainPort: 4444,
		Backends: []config.Backend{
			{Host: "http://localhost:4441"},
		},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_EmptyBackends(t *testing.T) {
	cfg := config.Config{MainPort: 4444}
	err := config.Validate(cfg)
	if !errors.Is(err, config.ErrNoBackends) {
		t.Fatalf("expected ErrNoBackends, got %v", err)
	}
}

func TestValidate_BadScheme(t *testing.T) {
	cfg := config.Config{
		MainPort: 4444,
		Backends: []config.Backend{{Host: "file:///etc/passwd"}},
	}
	err := config.Validate(cfg)
	if !errors.Is(err, config.ErrBadScheme) {
		t.Fatalf("expected ErrBadScheme, got %v", err)
	}
}

func TestValidate_EmptyURL(t *testing.T) {
	cfg := config.Config{
		MainPort: 4444,
		Backends: []config.Backend{{Host: ""}},
	}
	err := config.Validate(cfg)
	if !errors.Is(err, config.ErrEmptyBackendURL) {
		t.Fatalf("expected ErrEmptyBackendURL, got %v", err)
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	cfg := config.Config{
		MainPort: 99999,
		Backends: []config.Backend{{Host: "http://localhost:4441"}},
	}
	err := config.Validate(cfg)
	if !errors.Is(err, config.ErrInvalidPort) {
		t.Fatalf("expected ErrInvalidPort, got %v", err)
	}
}

func TestValidate_HTTPSSchemeAllowed(t *testing.T) {
	cfg := config.Config{
		MainPort: 4444,
		Backends: []config.Backend{{Host: "https://example.com"}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("https scheme should be valid, got %v", err)
	}
}

// ---------- Defaults ----------

func TestDefaults_FillsZeroValues(t *testing.T) {
	cfg := config.Config{
		Backends: []config.Backend{{Host: "http://localhost:4441"}},
	}
	config.Defaults(&cfg)

	if cfg.Balancer.Strategy != "round_robin" {
		t.Fatalf("default strategy: %s", cfg.Balancer.Strategy)
	}
	if cfg.Health.IntervalSec != 10 {
		t.Fatalf("default interval: %d", cfg.Health.IntervalSec)
	}
	if cfg.Health.TimeoutSec != 2 {
		t.Fatalf("default timeout: %d", cfg.Health.TimeoutSec)
	}
	if cfg.Health.Path != "/" {
		t.Fatalf("default path: %s", cfg.Health.Path)
	}
	if cfg.Retry.MaxRetries != 1 {
		t.Fatalf("default max_retries: %d", cfg.Retry.MaxRetries)
	}
	if cfg.Breaker.FailureThreshold != 5 {
		t.Fatalf("default failure_threshold: %d", cfg.Breaker.FailureThreshold)
	}
	if cfg.Breaker.CooldownSec != 30 {
		t.Fatalf("default cooldown: %d", cfg.Breaker.CooldownSec)
	}
}

func TestDefaults_DoesNotOverrideSetValues(t *testing.T) {
	cfg := config.Config{
		Backends: []config.Backend{{Host: "http://localhost:4441"}},
		Balancer: config.BalancerConfig{Strategy: "least_connections"},
		Retry:    config.RetryConfig{MaxRetries: 3},
	}
	config.Defaults(&cfg)

	if cfg.Balancer.Strategy != "least_connections" {
		t.Fatalf("should not override strategy: %s", cfg.Balancer.Strategy)
	}
	if cfg.Retry.MaxRetries != 3 {
		t.Fatalf("should not override max_retries: %d", cfg.Retry.MaxRetries)
	}
}

// ---------- ApplyEnvOverrides ----------

func TestApplyEnvOverrides_Host(t *testing.T) {
	t.Setenv("HARIBON_HOST", "127.0.0.1")
	t.Setenv("HARIBON_PORT", "")
	cfg := config.Config{}
	config.ApplyEnvOverrides(&cfg)
	if cfg.MainHost != "127.0.0.1" {
		t.Fatalf("host override failed: %s", cfg.MainHost)
	}
}

func TestApplyEnvOverrides_Port(t *testing.T) {
	t.Setenv("HARIBON_HOST", "")
	t.Setenv("HARIBON_PORT", "9999")
	cfg := config.Config{}
	config.ApplyEnvOverrides(&cfg)
	if cfg.MainPort != 9999 {
		t.Fatalf("port override failed: %d", cfg.MainPort)
	}
}

func TestApplyEnvOverrides_InvalidPortIgnored(t *testing.T) {
	t.Setenv("HARIBON_PORT", "notanumber")
	cfg := config.Config{MainPort: 4444}
	config.ApplyEnvOverrides(&cfg)
	if cfg.MainPort != 4444 {
		t.Fatalf("invalid port should be ignored, got %d", cfg.MainPort)
	}
}

// ---------- ResolveConfigPath ----------

func TestResolveConfigPath_CLI(t *testing.T) {
	got := config.ResolveConfigPath("/my/config.yml")
	if got != "/my/config.yml" {
		t.Fatalf("expected cli path, got %s", got)
	}
}

func TestResolveConfigPath_Env(t *testing.T) {
	t.Setenv("HARIBON_CONFIG", "/env/config.yml")
	got := config.ResolveConfigPath("")
	if got != "/env/config.yml" {
		t.Fatalf("expected env path, got %s", got)
	}
}

func TestResolveConfigPath_Default(t *testing.T) {
	t.Setenv("HARIBON_CONFIG", "")
	got := config.ResolveConfigPath("")
	if got != "./haribon-config.yml" {
		t.Fatalf("expected default path, got %s", got)
	}
}

// ---------- Snapshot ----------

func TestSnapshot_Load(t *testing.T) {
	cfg := config.Config{
		MainHost: "0.0.0.0",
		MainPort: 4444,
		Backends: []config.Backend{{Host: "http://localhost:4441"}},
	}
	s := config.NewSnapshot(cfg)
	got := s.Load()
	if got.MainHost != "0.0.0.0" || got.MainPort != 4444 {
		t.Fatalf("snapshot load failed: %+v", got)
	}
}

func TestSnapshot_Reload(t *testing.T) {
	yaml := `
host: "0.0.0.0"
port: 4444
backends:
  - url: "http://localhost:4441"
`
	path := writeTempYAML(t, yaml)
	cfg := config.Config{MainHost: "old", MainPort: 9999}
	s := config.NewSnapshot(cfg)

	if err := s.Reload(path); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	got := s.Load()
	if got.MainHost != "0.0.0.0" || got.MainPort != 4444 {
		t.Fatalf("reload didn't update: %+v", got)
	}
}

func TestSnapshot_Reload_Invalid_KeepsOld(t *testing.T) {
	cfg := config.Config{MainHost: "old", MainPort: 9999, Backends: []config.Backend{{Host: "http://old:4441"}}}
	s := config.NewSnapshot(cfg)

	// Try to reload with a bad path - should keep old config
	_ = s.Reload("/no/such/file.yml")
	got := s.Load()
	if got.MainHost != "old" {
		t.Fatalf("invalid reload should keep old config: %+v", got)
	}
}

func TestSnapshot_Reload_InvalidConfig_KeepsOld(t *testing.T) {
	yaml := `host: "bad"
port: 99999
backends: []
`
	path := writeTempYAML(t, yaml)
	cfg := config.Config{MainHost: "old", MainPort: 4444, Backends: []config.Backend{{Host: "http://old:4441"}}}
	s := config.NewSnapshot(cfg)

	// Try to reload with an invalid config - should keep old config
	_ = s.Reload(path)
	got := s.Load()
	if got.MainHost != "old" {
		t.Fatalf("invalid config reload should keep old: %+v", got)
	}
}

func TestSnapshot_Reload_UpdatesBackends(t *testing.T) {
	yaml := `host: "0.0.0.0"
port: 4444
backends:
  - url: "http://new1:4441"
  - url: "http://new2:4442"
`
	path := writeTempYAML(t, yaml)
	cfg := config.Config{MainHost: "0.0.0.0", MainPort: 4444, Backends: []config.Backend{{Host: "http://old:4441"}}}
	s := config.NewSnapshot(cfg)

	_ = s.Reload(path)
	got := s.Load()
	if len(got.Backends) != 2 {
		t.Fatalf("expected 2 backends after reload, got %d", len(got.Backends))
	}
	if got.Backends[0].Host != "http://new1:4441" {
		t.Fatalf("first backend not updated: %s", got.Backends[0].Host)
	}
}

// ---------- Config with Discovery ----------

func TestValidate_DiscoveryDNS_Valid(t *testing.T) {
	cfg := config.Config{
		MainPort:  4444,
		Backends:  []config.Backend{{Host: "http://localhost:4441"}},
		Discovery: config.DiscoveryConfig{Provider: "dns", DNSName: "api.internal", RefreshSec: 30},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("dns discovery should be valid: %v", err)
	}
}

func TestValidate_DiscoveryFile_Valid(t *testing.T) {
	cfg := config.Config{
		MainPort:  4444,
		Backends:  []config.Backend{{Host: "http://localhost:4441"}},
		Discovery: config.DiscoveryConfig{Provider: "file", FilePath: "/etc/haribon/backends.json", RefreshSec: 10},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("file discovery should be valid: %v", err)
	}
}

func TestValidate_DiscoveryDNS_MissingName(t *testing.T) {
	cfg := config.Config{
		MainPort:  4444,
		Backends:  []config.Backend{{Host: "http://localhost:4441"}},
		Discovery: config.DiscoveryConfig{Provider: "dns"},
	}
	err := config.Validate(cfg)
	if err == nil {
		t.Fatal("expected error for dns without name")
	}
}

func TestValidate_DiscoveryFile_MissingPath(t *testing.T) {
	cfg := config.Config{
		MainPort:  4444,
		Backends:  []config.Backend{{Host: "http://localhost:4441"}},
		Discovery: config.DiscoveryConfig{Provider: "file"},
	}
	err := config.Validate(cfg)
	if err == nil {
		t.Fatal("expected error for file without path")
	}
}

func TestDefaults_Discovery(t *testing.T) {
	cfg := config.Config{Discovery: config.DiscoveryConfig{Provider: "static"}}
	config.Defaults(&cfg)
	if cfg.Discovery.RefreshSec != 30 {
		t.Fatalf("default refresh_sec should be 30, got %d", cfg.Discovery.RefreshSec)
	}
}

func TestLoad_Discovery_YAML(t *testing.T) {
	yaml := `
host: "0.0.0.0"
port: 4444
backends:
  - url: "http://localhost:4441"
discovery:
  provider: dns
  dns_name: "api.internal"
  refresh_sec: 30
`
	path := writeTempYAML(t, yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Discovery.Provider != "dns" {
		t.Fatalf("discovery provider: %s", cfg.Discovery.Provider)
	}
	if cfg.Discovery.DNSName != "api.internal" {
		t.Fatalf("dns_name: %s", cfg.Discovery.DNSName)
	}
	if cfg.Discovery.RefreshSec != 30 {
		t.Fatalf("refresh_sec: %d", cfg.Discovery.RefreshSec)
	}
}

// ---------- helpers ----------

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}
