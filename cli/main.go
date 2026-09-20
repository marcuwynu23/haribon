package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/marcuwynu23/haribon/internal/balancer"
	"github.com/marcuwynu23/haribon/internal/cluster"
	"github.com/marcuwynu23/haribon/internal/config"
	"github.com/marcuwynu23/haribon/internal/discover"
	"github.com/marcuwynu23/haribon/internal/health"
	"github.com/marcuwynu23/haribon/internal/logging"
	"github.com/marcuwynu23/haribon/internal/metrics"
	"github.com/marcuwynu23/haribon/internal/proxy"
)

// version is injected at build time via:
//
//	-ldflags "-X main.version=$(VERSION)"
var version = "dev"

// ==========================
// LOG STRUCT (LOKI FRIENDLY)
// ==========================

// LogEntry is the structured log line written for every proxied request.
// Fields are additive-only — never remove or rename (Loki/Promtail contracts).
type LogEntry struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Backend    string `json:"backend"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Retries    int    `json:"retries,omitempty"`
	Level      string `json:"level"`
}

// ==========================
// GLOBAL STATE
// (kept for backward-compat with existing tests)
// ==========================

var (
	backends      []string
	backendsMu    sync.RWMutex // guards backends; production writes on rebuild
	currentServer uint64       // used only by legacy getNextBackend in tests

	httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	logWriter io.Writer = os.Stdout
	logSinks            = logging.NewMulti() // remote exporters + file, if any
	logFormat           = "json"             // json | text
	mu        sync.Mutex
)

var (
	backendHealth = map[string]bool{}
	healthMutex   sync.RWMutex
)

// setBackends replaces the process-wide backend pool. Called on every successful
// rebuild so /readyz and the legacy helpers track the live pool.
func setBackends(urls []string) {
	backendsMu.Lock()
	defer backendsMu.Unlock()
	backends = append([]string(nil), urls...)
}

// getBackends returns a copy of the current pool.
func getBackends() []string {
	backendsMu.RLock()
	defer backendsMu.RUnlock()
	return append([]string(nil), backends...)
}

// ==========================
// HEALTH STATE HELPERS
// (kept for legacy tests)
// ==========================

func setHealth(b string, status bool) {
	healthMutex.Lock()
	defer healthMutex.Unlock()
	backendHealth[b] = status
}

func isHealthy(b string) bool {
	healthMutex.RLock()
	defer healthMutex.RUnlock()
	v, known := backendHealth[b]
	return !known || v
}

// ==========================
// LOKI LOGGING
// ==========================

func writeLog(entry LogEntry) {
	entry.Time = time.Now().UTC().Format(time.RFC3339Nano)

	// Remote sinks (Loki, Elasticsearch, Fluent Bit) and the log file each own a
	// bounded queue; Write never blocks, so a dead sink cannot stall a request.
	if logSinks != nil && logSinks.Name() != "" {
		logSinks.Write(logging.LogEntry{
			Time:       entry.Time,
			Method:     entry.Method,
			Path:       entry.Path,
			Backend:    entry.Backend,
			Status:     entry.Status,
			DurationMS: entry.DurationMS,
			Retries:    entry.Retries,
			Level:      entry.Level,
		})
	}

	if logFormat == "text" {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintln(logWriter, logging.FormatText(logging.LogEntry{
			Time:       entry.Time,
			Method:     entry.Method,
			Path:       entry.Path,
			Backend:    entry.Backend,
			Status:     entry.Status,
			DurationMS: entry.DurationMS,
			Retries:    entry.Retries,
			Level:      entry.Level,
		}))
		return
	}

	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	_, _ = logWriter.Write(append(b, '\n'))
}

// ==========================
// LEGACY ROUND-ROBIN
// (used by cli/main_test.go — do not remove)
// ==========================

func getNextBackend() (string, error) {
	pool := getBackends()
	if len(pool) == 0 {
		return "", fmt.Errorf("no backends configured")
	}
	n := len(pool)
	start := atomic.AddUint64(&currentServer, 1) - 1
	for i := 0; i < n; i++ {
		b := pool[(int(start)+i)%n]
		if isHealthy(b) {
			return b, nil
		}
	}
	return "", fmt.Errorf("no healthy backend available")
}

// ==========================
// HEALTH CHECKER (adapter for health.Readyz probe)
// ==========================

type balancerHealthChecker struct{}

func (balancerHealthChecker) HasHealthyBackend() bool {
	healthMutex.RLock()
	defer healthMutex.RUnlock()
	pool := getBackends()
	if len(pool) == 0 {
		return false
	}
	for _, b := range pool {
		v, known := backendHealth[b]
		if !known || v {
			return true
		}
	}
	return false
}

// ==========================
// METRICS-AWARE TRANSITION LOGGER
// ==========================

func makeHealthLogger(reg *metrics.Registry) health.Logger {
	return func(backend, state, reason string) {
		entry := LogEntry{
			Backend: backend,
			Level:   "info",
			Path:    "health-scheduler",
			Status:  0,
		}
		if state == "unhealthy" {
			entry.Level = "warn"
		}
		entry.Method = "PROBE"
		writeLog(LogEntry{
			Method:  "PROBE",
			Path:    "health-check",
			Backend: backend,
			Status:  0,
			Level:   entry.Level,
		})
		_ = reason
		if reg != nil {
			v := int64(1)
			if state == "unhealthy" {
				v = 0
			}
			reg.Gauge(metrics.MetricName("haribon_backend_healthy", "backend", backend)).Set(v)
		}
	}
}

// ==========================
// PROXY LOGGER ADAPTER
// ==========================

func makeProxyLogger(reg *metrics.Registry) proxy.Logger {
	return func(e proxy.LogEntry) {
		writeLog(LogEntry{
			Method:     e.Method,
			Path:       e.Path,
			Backend:    e.Backend,
			Status:     e.Status,
			DurationMS: e.DurationMS,
			Retries:    e.Retries,
			Level:      e.Level,
		})
		if reg == nil {
			return
		}
		if e.Status >= 200 && e.Status < 300 {
			reg.Counter(metrics.MetricName("haribon_requests_total", "backend", e.Backend)).Inc()
		}
		reg.Gauge(metrics.MetricName("haribon_last_duration_ms", "backend", e.Backend)).Set(e.DurationMS)
		if e.Retries > 0 {
			reg.Counter(metrics.MetricName("haribon_retries_total", "backend", e.Backend)).Add(int64(e.Retries))
		}
	}
}

// ==========================
// START COMMAND
// ==========================

func startCommand(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	var configPath string
	var watchSecs int
	fs.StringVar(&configPath, "config", "", "config file path")
	fs.IntVar(&watchSecs, "watch_config", 0, "poll config file every N seconds")
	_ = fs.Parse(args)

	path := config.ResolveConfigPath(configPath)

	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	config.ApplyEnvOverrides(&cfg)
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "config validation error: %v\n", err)
		os.Exit(1)
	}
	config.Defaults(&cfg)

	// Logging must be wired before anything can emit a log line.
	setupLogging(cfg)

	// Atomic snapshot: the watcher swaps it, the runtime reads it.
	snapshot := config.NewSnapshot(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Metrics registry.
	reg := metrics.New()

	// Cluster node (gossip). Started only when configured; the runtime consults
	// it as one more opinion on whether a backend is healthy.
	var node *cluster.Node
	if cfg.Cluster.Enabled {
		node = cluster.NewNode(cfg.Cluster.NodeID, cfg.Cluster.GossipAddr, cfg.Cluster.Peers, cfg.Cluster.GossipSec)
		node.SetConfigHash(config.FileHash(path))
		if err := node.Start(); err != nil {
			// Refuse to run with clustering half-up: a node that sends gossip but
			// cannot receive any looks healthy to its peers while silently
			// ignoring every health update they send.
			fmt.Fprintf(os.Stderr, "cluster error: %v\n", err)
			os.Exit(1)
		}
		defer node.Stop()
		log.Printf("cluster: node %s joining %d peer(s) on %s",
			cfg.Cluster.NodeID, len(cfg.Cluster.Peers), cfg.Cluster.GossipAddr)
	}

	// The swappable runtime replaces the old build-once wiring: reloads and
	// discovery updates now rebuild the pool and publish it atomically.
	rt := newRuntime(ctx, path, snapshot, reg, node)

	// Backend discovery. Primed before the first rebuild so the initial pool
	// already contains discovered backends rather than only what the config
	// file lists.
	var provider discover.Provider
	if cfg.Discovery.Provider != "" && cfg.Discovery.Provider != "static" {
		provider, err = discover.NewProvider(
			discover.Config{
				Provider:   cfg.Discovery.Provider,
				DNSName:    cfg.Discovery.DNSName,
				DNSPort:    cfg.Discovery.DNSPort,
				FilePath:   cfg.Discovery.FilePath,
				RefreshSec: cfg.Discovery.RefreshSec,
			},
			ctx.Done(),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "discovery error: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = provider.Close() }()
		rt.StartDiscovery(provider)
		log.Printf("discovery: provider %s (refresh %ds)", cfg.Discovery.Provider, cfg.Discovery.RefreshSec)
	}

	// Build the first generation: config backends plus anything discovery found.
	rt.rebuild(cfg)

	// Config hot reload: SIGHUP and/or polling. The hook fires after the
	// snapshot swap so live routing state is rebuilt too.
	config.StartWatcherHook(snapshot, path, watchSecs, ctx.Done(), rt.OnConfigReload)

	rt.StartClusterMetrics()

	// Proxy handler. One object satisfies Balancer, HealthChecker, IPHash, and
	// BreakRecorder, so it always sees the newest generation.
	proxyHandler := proxy.New(
		rt.sw, rt.sw, rt.sw,
		httpClient,
		proxy.Config{
			HandlerTimeout: 10 * time.Second,
			MaxRetries:     cfg.Retry.MaxRetries,
		},
		reg,
		makeProxyLogger(reg),
	)

	// Mux
	addr := fmt.Sprintf("%s:%d", cfg.MainHost, cfg.MainPort)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health.Healthz)
	mux.HandleFunc("/readyz", health.Readyz(balancerHealthChecker{}))
	mux.Handle("/metrics", activeConnsHandler(rt, reg))
	mux.Handle("/", proxyHandler)

	srv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		log.Printf("running on %s\n", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(2)
		}
	}()

	<-ctx.Done()
	stop()

	shutdownTimeout := time.Duration(cfg.ShutdownTimeoutSec) * time.Second
	if shutdownTimeout <= 0 {
		shutdownTimeout = 15 * time.Second
	}

	log.Printf("shutting down (timeout: %s)…\n", shutdownTimeout)
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
	}
	// Flush buffered log entries before the process exits.
	if logSinks != nil {
		logSinks.Close()
	}
	log.Println("shutdown complete")
}

// activeConnsHandler refreshes the haribon_active_conns gauge immediately before
// rendering, so the value in the response is current rather than one scrape
// stale. Balancers that do not track connections simply contribute nothing.
func activeConnsHandler(rt *runtime, reg *metrics.Registry) http.HandlerFunc {
	render := reg.Handler()
	return func(w http.ResponseWriter, r *http.Request) {
		if e := rt.sw.load(); e != nil {
			if cr, ok := e.bal.(balancer.ConnsReporter); ok {
				for url, n := range cr.ActiveConns() {
					reg.Gauge(metrics.MetricName("haribon_active_conns", "backend", url)).Set(n)
				}
			}
		}
		render(w, r)
	}
}

// ==========================
// CHECK COMMAND
// ==========================

func checkCommand(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var configPath string
	fs.StringVar(&configPath, "config", "", "config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(config.ResolveConfigPath(configPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	config.ApplyEnvOverrides(&cfg)
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "validation error: %v\n", err)
		os.Exit(1)
	}
	config.Defaults(&cfg)

	fmt.Printf("ok: %d backend(s), strategy: %s, probes /healthz /readyz /metrics enabled\n",
		len(cfg.Backends), cfg.Balancer.Strategy)
	for i, b := range cfg.Backends {
		w := b.Weight
		if w <= 0 {
			w = 1
		}
		fmt.Printf("  [%d] %s (weight: %d)\n", i, b.Host, w)
	}
	if cfg.Discovery.Provider != "" {
		fmt.Printf("  discovery: provider=%s, refresh_sec=%d\n", cfg.Discovery.Provider, cfg.Discovery.RefreshSec)
	}
}

// validateCommand validates a config file against the JSON schema.
func validateCommand(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var configPath string
	var schemaPath string
	fs.StringVar(&configPath, "config", "", "config file path")
	fs.StringVar(&schemaPath, "schema", "", "JSON schema path (default: schema/haribon-config.schema.json)")
	_ = fs.Parse(args)

	path := config.ResolveConfigPath(configPath)

	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	config.ApplyEnvOverrides(&cfg)
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "validation error: %v\n", err)
		os.Exit(1)
	}
	config.Defaults(&cfg)

	// Structural validation against the schema file, applied to the raw file so
	// unknown keys are caught too (a round-trip through Config would drop them).
	schema := config.FindSchema(schemaPath)
	if schema == "" {
		fmt.Fprintf(os.Stderr, "error: no schema file found (looked for %s); pass --schema\n",
			strings.Join(config.SchemaPaths, ", "))
		os.Exit(1)
	}
	if err := config.ValidateAgainstSchema(schema, path); err != nil {
		fmt.Fprintf(os.Stderr, "schema validation error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("valid: config file passes %s (%d backend(s))\n",
		filepath.Base(schema), len(cfg.Backends))
}

// validateSchema validates an in-memory config against the schema file.
// It returns nil when no schema file can be found, so a build or test that runs
// outside the repository is not blocked by a missing schema.
func validateSchema(cfg config.Config) error {
	schema := config.FindSchema("")
	if schema == "" {
		return nil
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp, err := os.CreateTemp("", "haribon-schema-*.yml")
	if err != nil {
		return fmt.Errorf("temp config: %w", err)
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	return config.ValidateAgainstSchema(schema, tmp.Name())
}

// ==========================
// VERSION COMMAND
// ==========================

func versionCommand() {
	fmt.Printf("haribon %s\n", version)
}

// ==========================
// USAGE
// ==========================

func printUsage() {
	fmt.Fprintf(os.Stderr, `haribon — HTTP load balancer

Usage:
  haribon <command> [flags]

Commands:
  start    Start the load balancer
  check    Validate a config file and exit (exit 0 ok / 1 error)
  validate Validate a config file against the JSON schema
  version  Print version and exit

Flags (start):
  --config string     Config file path (default: $HARIBON_CONFIG or ./haribon-config.yml)
  --watch_config int  Poll config file every N seconds for changes

Reloading:
  Send SIGHUP to reload the config on Unix. Windows never delivers SIGHUP,
  so use --watch_config there.

Examples:
  haribon start --config haribon-config.yml
  haribon start --config haribon-config.yml --watch_config 30
  haribon check --config haribon-config.yml
  haribon validate --config haribon-config.yml
  haribon version

Environment:
  HARIBON_HOST    Override bind host
  HARIBON_PORT    Override bind port
  HARIBON_CONFIG  Default config file path
`)
}

// ==========================
// MAIN
// ==========================

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "start":
		startCommand(os.Args[2:])
	case "check":
		checkCommand(os.Args[2:])
	case "validate":
		validateCommand(os.Args[2:])
	case "version", "--version", "-v":
		versionCommand()
	case "--help", "-h", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// ==========================
// HELPERS (backward-compat shims for existing tests)
// ==========================

func resolveConfigPath(cli string) string {
	return config.ResolveConfigPath(cli)
}

func applyEnvOverrides(cfg *config.Config) {
	config.ApplyEnvOverrides(cfg)
}

func loadConfig(filename string) (config.Config, error) {
	return config.Load(filename)
}

// stripHopByHop is kept as a thin wrapper for existing tests.
// Production code uses proxy.stripHopByHop (internal).
func stripHopByHop(h http.Header) {
	hopByHop := []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	}
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

// loadBalancer is kept as a thin wrapper for existing integration tests.
// Production code routes through the proxy.Handler wired in startCommand.
func loadBalancer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Determine client IP for X-Forwarded-For
	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		clientIP = host
	}

	for range backends {
		server, err := getNextBackend()
		if err != nil {
			break
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		targetURL := server + r.URL.RequestURI()
		req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, r.Body)
		if err != nil {
			cancel()
			continue
		}

		for k, v := range r.Header {
			req.Header[k] = v
		}
		stripHopByHop(req.Header)

		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		req.Header.Set("X-Forwarded-Proto", proto)

		resp, err := httpClient.Do(req)
		cancel()

		if err != nil {
			setHealth(server, false)
			continue
		}
		defer resp.Body.Close()

		stripHopByHop(resp.Header)
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		setHealth(server, true)

		writeLog(LogEntry{
			Method:     r.Method,
			Path:       r.URL.Path,
			Backend:    server,
			Status:     resp.StatusCode,
			DurationMS: time.Since(start).Milliseconds(),
			Level:      "info",
		})
		return
	}

	writeLog(LogEntry{
		Method:     r.Method,
		Path:       r.URL.Path,
		Backend:    "",
		Status:     503,
		DurationMS: time.Since(start).Milliseconds(),
		Level:      "error",
	})
	http.Error(w, "All backend servers failed", http.StatusServiceUnavailable)
}

// setupLogging wires the log destinations named in cfg.Exporters.
//
// The previous implementation built a local []Exporter that was discarded on
// return, so logWriter stayed os.Stdout and the configured log file was created
// but never written. This assigns logWriter and the process-wide logSinks.
func setupLogging(cfg config.Config) {
	logFormat = cfg.LogFormat
	if logFormat == "" {
		logFormat = "json"
	}
	logWriter = os.Stdout

	exporters := []logging.Exporter{}

	// stdout is always available; it is the fallback when a configured sink fails.
	if cfg.EnabledExporter("stdout") || len(cfg.Exporters) == 0 {
		logWriter = os.Stdout
	}

	if cfg.EnabledExporter("file") || cfg.Logging {
		path := cfg.LogPath
		if path == "" {
			path = "./haribon.log"
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("log dir create warning: %v", err)
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("log file error (stdout only): %v", err)
		} else {
			exporters = append(exporters, &logging.FileExporter{File: f, Format: logFormat})
			log.Printf("logging to file: %s", path)
		}
	}

	if cfg.EnabledExporter("loki") {
		exporters = append(exporters, logging.NewLokiExporter(cfg.Loki.URL, cfg.Loki.Labels))
		log.Printf("logging to loki: %s", cfg.Loki.URL)
	}
	if cfg.EnabledExporter("elasticsearch") {
		exporters = append(exporters, logging.NewElasticsearchExporter(cfg.Elasticsearch.URL, cfg.Elasticsearch.Index))
		log.Printf("logging to elasticsearch: %s (index %s)", cfg.Elasticsearch.URL, cfg.Elasticsearch.Index)
	}
	if cfg.EnabledExporter("fluentbit") {
		exporters = append(exporters, logging.NewFluentbitExporter(cfg.Fluentbit.Addr))
		log.Printf("logging to fluentbit: %s", cfg.Fluentbit.Addr)
	}

	if len(exporters) > 0 {
		logSinks = logging.NewMulti(exporters...)
	}
}

// keep strconv imported for backward-compat (test file uses it indirectly)
var _ = strconv.Itoa
