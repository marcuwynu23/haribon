package config

// env_defaults_test.go — internal tests.
//
// Internal (not config_test) because it covers two things that are only visible
// from inside the package: the ${VAR} expansion applied to raw YAML, and the
// invariant that Validate is a pure predicate while Defaults owns all mutation.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// ---------- ${VAR} expansion ----------

// TestLoad_ExpandsEnvInNodeID is the regression test for a bug that made
// clustering silently useless. The samples and docs use
// node_id: "${HOSTNAME}", but nothing expanded it, so every replica loaded the
// literal string "${HOSTNAME}". MergeGossip ignores messages whose NodeID
// matches the receiver's, so every node treated every peer as itself and no
// health state ever crossed the wire.
func TestLoad_ExpandsEnvInNodeID(t *testing.T) {
	t.Setenv("HARIBON_TEST_NODE", "haribon-7")

	path := writeFile(t, "cfg.yml", `
host: "0.0.0.0"
port: 4444
backends:
  - url: "http://localhost:4441"
cluster:
  enabled: true
  node_id: "${HARIBON_TEST_NODE}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cluster.NodeID != "haribon-7" {
		t.Fatalf("expected the env value, got %q", cfg.Cluster.NodeID)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("HARIBON_A", "alpha")

	cases := []struct {
		name, in, want string
	}{
		{"single", "${HARIBON_A}", "alpha"},
		{"embedded", "prefix-${HARIBON_A}-suffix", "prefix-alpha-suffix"},
		{"unset stays verbatim", "${HARIBON_DEFINITELY_UNSET}", "${HARIBON_DEFINITELY_UNSET}"},
		{"lowercase not a reference", "$haribon_a", "$haribon_a"},
		{"dollar brace with bad name", "${1BAD}", "${1BAD}"},
		{"no reference at all", "plain text", "plain text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(expandEnv([]byte(c.in))); got != c.want {
				t.Fatalf("expandEnv(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestValidate_RejectsUnresolvedEnvInNodeID: leaving "${HOSTNAME}" in place is
// the safe choice (it is visible in an error) but it must not start. A cluster
// whose members all share one node ID gossips to nobody.
func TestValidate_RejectsUnresolvedEnvInNodeID(t *testing.T) {
	cfg := Config{
		Backends: []Backend{{Host: "http://localhost:4441"}},
		Cluster:  ClusterConfig{Enabled: true, NodeID: "${HARIBON_DEFINITELY_UNSET}"},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("an unresolved ${...} node_id must be rejected")
	}
	if !strings.Contains(err.Error(), "node_id") {
		t.Fatalf("the error should name node_id, got: %v", err)
	}
}

func TestValidate_AllowsLiteralNodeID(t *testing.T) {
	cfg := Config{
		Backends: []Backend{{Host: "http://localhost:4441"}},
		Cluster:  ClusterConfig{Enabled: true, NodeID: "haribon-0"},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("a literal node_id is fine: %v", err)
	}
}

// ---------- Validate is a pure predicate ----------

// TestValidate_DoesNotMutateConfig pins the split between Validate and Defaults.
// Validate takes Config by value, so any write inside it is discarded — which is
// exactly what happened to the old defaulting code, making it look like
// defaults were applied when they never were.
func TestValidate_DoesNotMutateConfig(t *testing.T) {
	cfg := Config{Backends: []Backend{{Host: "http://localhost:4441"}}}
	_ = Validate(cfg)

	if len(cfg.Exporters) != 0 {
		t.Fatalf("Validate must not populate exporters, got %v", cfg.Exporters)
	}
	if cfg.Discovery.RefreshSec != 0 {
		t.Fatalf("Validate must not set discovery defaults, got %d", cfg.Discovery.RefreshSec)
	}
	if cfg.LogFormat != "" {
		t.Fatalf("Validate must not set log_format, got %q", cfg.LogFormat)
	}
}

// ---------- weight semantics ----------

// TestValidate_WeightZeroMeansUnset: YAML cannot distinguish an omitted weight
// from an explicit 0, so 0 must be accepted and treated as 1.
func TestValidate_WeightZeroMeansUnset(t *testing.T) {
	cfg := Config{Backends: []Backend{{Host: "http://localhost:4441", Weight: 0}}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("weight 0 means unset and must be valid: %v", err)
	}
}

func TestValidate_NegativeWeightRejected(t *testing.T) {
	cfg := Config{Backends: []Backend{{Host: "http://localhost:4441", Weight: -1}}}
	if err := Validate(cfg); err == nil {
		t.Fatal("a negative weight must be rejected")
	}
}

// ---------- exporters ----------

func TestValidate_ExporterNames(t *testing.T) {
	valid := []string{"stdout", "file", "loki", "elasticsearch", "fluentbit"}
	for _, name := range valid {
		cfg := Config{
			Backends:  []Backend{{Host: "http://localhost:4441"}},
			Exporters: []string{name},
		}
		if err := Validate(cfg); err != nil {
			t.Fatalf("exporter %q should be valid: %v", name, err)
		}
	}

	cfg := Config{
		Backends:  []Backend{{Host: "http://localhost:4441"}},
		Exporters: []string{"splunk"},
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("an unknown exporter must be rejected")
	}
}

func TestConfig_EnabledExporter(t *testing.T) {
	cfg := Config{Exporters: []string{"stdout", "loki"}}
	if !cfg.EnabledExporter("loki") {
		t.Fatal("loki is listed and should be enabled")
	}
	if cfg.EnabledExporter("fluentbit") {
		t.Fatal("fluentbit is not listed and should be disabled")
	}
}

// ---------- log format ----------

func TestValidate_LogFormat(t *testing.T) {
	for _, f := range []string{"", "json", "text"} {
		cfg := Config{Backends: []Backend{{Host: "http://localhost:4441"}}, LogFormat: f}
		if err := Validate(cfg); err != nil {
			t.Fatalf("log format %q should be valid: %v", f, err)
		}
	}
	cfg := Config{Backends: []Backend{{Host: "http://localhost:4441"}}, LogFormat: "xml"}
	if err := Validate(cfg); err == nil {
		t.Fatal("an unknown log format must be rejected")
	}
}

// ---------- defaults ----------

func TestDefaults_ExporterBlockDefaults(t *testing.T) {
	cfg := Config{
		Backends:  []Backend{{Host: "http://localhost:4441"}},
		Exporters: []string{"loki"},
	}
	Defaults(&cfg)

	if cfg.Loki.URL != "http://localhost:3100" {
		t.Fatalf("loki url default: %q", cfg.Loki.URL)
	}
	if cfg.Loki.Labels["job"] != "haribon" {
		t.Fatalf("the job label must always be present, got %v", cfg.Loki.Labels)
	}
	if cfg.Elasticsearch.URL != "http://localhost:9200" || cfg.Elasticsearch.Index != "haribon" {
		t.Fatalf("elasticsearch defaults: %+v", cfg.Elasticsearch)
	}
	if cfg.Fluentbit.Addr != "localhost:24224" {
		t.Fatalf("fluentbit addr default: %q", cfg.Fluentbit.Addr)
	}
}

// TestDefaults_DoesNotClobberConfiguredExporters is the regression test for a
// loop that overwrote the exporter list with []string{"stdout"} as soon as it
// saw a named exporter, so `exporters: [loki]` silently became stdout only.
func TestDefaults_DoesNotClobberConfiguredExporters(t *testing.T) {
	cfg := Config{Exporters: []string{"loki", "elasticsearch"}}
	Defaults(&cfg)

	if len(cfg.Exporters) != 2 || cfg.Exporters[0] != "loki" || cfg.Exporters[1] != "elasticsearch" {
		t.Fatalf("Defaults must preserve the configured exporters, got %v", cfg.Exporters)
	}
}

func TestDefaults_EmptyExportersBecomesStdout(t *testing.T) {
	cfg := Config{}
	Defaults(&cfg)
	if len(cfg.Exporters) != 1 || cfg.Exporters[0] != "stdout" {
		t.Fatalf("expected stdout by default, got %v", cfg.Exporters)
	}
}

func TestDefaults_DNSPortOnlyForDNSProvider(t *testing.T) {
	cfg := Config{Discovery: DiscoveryConfig{Provider: "dns", DNSName: "api.internal"}}
	Defaults(&cfg)
	if cfg.Discovery.DNSPort != 80 {
		t.Fatalf("dns_port should default to 80 for the dns provider, got %d", cfg.Discovery.DNSPort)
	}

	cfg2 := Config{Discovery: DiscoveryConfig{Provider: "file", FilePath: "/tmp/b.json"}}
	Defaults(&cfg2)
	if cfg2.Discovery.DNSPort != 0 {
		t.Fatalf("dns_port is meaningless for the file provider, got %d", cfg2.Discovery.DNSPort)
	}
}

// TestDefaults_NodeIDFromHostname covers the fallback when a config enables
// clustering but leaves node_id empty: without a distinct identity per replica,
// peers discard the gossip as their own.
func TestDefaults_NodeIDFromHostname(t *testing.T) {
	cfg := Config{Cluster: ClusterConfig{Enabled: true}}
	Defaults(&cfg)

	if cfg.Cluster.NodeID == "" {
		t.Fatal("an enabling cluster block must end up with a node_id")
	}
	if strings.Contains(cfg.Cluster.NodeID, "${") {
		t.Fatalf("the derived node_id must be concrete, got %q", cfg.Cluster.NodeID)
	}
	host, _ := os.Hostname()
	if host != "" && cfg.Cluster.NodeID != host {
		t.Fatalf("expected the hostname %q, got %q", host, cfg.Cluster.NodeID)
	}
}

func TestDefaults_ClusterOnlyWhenEnabled(t *testing.T) {
	cfg := Config{}
	Defaults(&cfg)

	if cfg.Cluster.GossipSec != 0 || cfg.Cluster.GossipAddr != "" {
		t.Fatalf("a disabled cluster must not be defaulted: %+v", cfg.Cluster)
	}
	if cfg.Cluster.NodeID != "" {
		t.Fatalf("a disabled cluster must not acquire a node id, got %q", cfg.Cluster.NodeID)
	}
}

// ---------- FileHash ----------

func TestFileHash_StableAndContentSensitive(t *testing.T) {
	a := writeFile(t, "a.yml", "port: 4444\n")
	b := writeFile(t, "b.yml", "port: 4444\n")
	c := writeFile(t, "c.yml", "port: 4445\n")

	if FileHash(a) != FileHash(b) {
		t.Fatal("identical configs must hash identically")
	}
	if FileHash(a) == FileHash(c) {
		t.Fatal("different configs must hash differently")
	}
	if FileHash(filepath.Join(t.TempDir(), "missing.yml")) != "" {
		t.Fatal("an unreadable file must hash to the empty string")
	}
}

// ---------- schema validation ----------

func TestNormalizeYAML_ConvertsNestedMaps(t *testing.T) {
	in := map[interface{}]interface{}{
		"a": map[interface{}]interface{}{"b": 1},
		"c": []interface{}{map[interface{}]interface{}{"d": true}},
	}
	out, ok := normalizeYAML(in).(map[string]interface{})
	if !ok {
		t.Fatalf("expected a map[string]interface{}, got %T", normalizeYAML(in))
	}
	if _, ok := out["a"].(map[string]interface{}); !ok {
		t.Fatalf("nested map was not normalised: %T", out["a"])
	}
	arr, ok := out["c"].([]interface{})
	if !ok || len(arr) != 1 {
		t.Fatalf("array was not preserved: %#v", out["c"])
	}
	if _, ok := arr[0].(map[string]interface{}); !ok {
		t.Fatalf("map inside an array was not normalised: %T", arr[0])
	}
}

func TestValidateNode_TypeEnumAndBounds(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"strategy": map[string]interface{}{
				"type": "string",
				"enum": []interface{}{"round_robin", "ip_hash"},
			},
			"port": map[string]interface{}{
				"type":    "integer",
				"minimum": float64(1),
				"maximum": float64(65535),
			},
		},
		"additionalProperties": false,
	}

	good := map[string]interface{}{"strategy": "ip_hash", "port": 4444}
	if p := validateNode("", good, schema); len(p) != 0 {
		t.Fatalf("a conforming document must pass, got %v", p)
	}

	bad := map[string]interface{}{"strategy": "nope", "port": 99999, "typo_key": 1}
	problems := validateNode("", bad, schema)
	if len(problems) != 3 {
		t.Fatalf("expected 3 problems (enum, maximum, unknown key), got %d: %v", len(problems), problems)
	}
	joined := strings.Join(problems, " ")
	for _, want := range []string{"strategy", "port", "typo_key"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("problem list should mention %q: %v", want, problems)
		}
	}
}

func TestValidateNode_RequiredAndTypeMismatch(t *testing.T) {
	schema := map[string]interface{}{
		"type":     "object",
		"required": []interface{}{"backends"},
	}
	if p := validateNode("", map[string]interface{}{}, schema); len(p) != 1 {
		t.Fatalf("a missing required key must be reported, got %v", p)
	}

	var problems []string
	problems = append(problems, validateNode("", "not-an-object", schema)...)
	if len(problems) != 1 || !strings.Contains(problems[0], "object") {
		t.Fatalf("a type mismatch must be reported, got %v", problems)
	}
}

func TestValidateNode_ArrayItems(t *testing.T) {
	schema := map[string]interface{}{
		"type":  "array",
		"items": map[string]interface{}{"type": "string"},
	}
	if p := validateNode("", []interface{}{"a", "b"}, schema); len(p) != 0 {
		t.Fatalf("all-string array should pass, got %v", p)
	}
	problems := validateNode("", []interface{}{"a", 2}, schema)
	if len(problems) != 1 || !strings.Contains(problems[0], "[1]") {
		t.Fatalf("the offending index must be named, got %v", problems)
	}
}

func TestValidateNode_NumericTypeAcceptsYAMLFloats(t *testing.T) {
	// YAML and JSON decode integers differently (int vs float64); a whole
	// float64 must still satisfy "integer".
	schema := map[string]interface{}{"type": "integer"}
	if p := validateNode("", float64(4444), schema); len(p) != 0 {
		t.Fatalf("a whole float should satisfy integer, got %v", p)
	}
	if p := validateNode("", float64(4444.5), schema); len(p) != 1 {
		t.Fatalf("a fractional float must not satisfy integer, got %v", p)
	}
}

func TestFindSchema_ExplicitMissingReturnsEmpty(t *testing.T) {
	if got := FindSchema(filepath.Join(t.TempDir(), "nope.json")); got != "" {
		t.Fatalf("a missing explicit schema must yield an empty path, got %q", got)
	}
}

func TestFindSchema_FindsRepoSchema(t *testing.T) {
	// Internal tests run with the package directory as cwd, so the second
	// SchemaPaths entry (../schema/...) is the one that resolves.
	got := FindSchema("")
	if got == "" {
		t.Skip("schema file not reachable from the test working directory")
	}
	if !strings.HasSuffix(got, "haribon-config.schema.json") {
		t.Fatalf("unexpected schema path: %q", got)
	}
}
