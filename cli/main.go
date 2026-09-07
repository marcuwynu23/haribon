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

	"github.com/marcuwynu23/haribon/internal/balancer"
	"github.com/marcuwynu23/haribon/internal/config"
	"github.com/marcuwynu23/haribon/internal/health"
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
	currentServer uint64 // used only by legacy getNextBackend in tests

	httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	logWriter io.Writer = os.Stdout
	mu        sync.Mutex
)

var (
	backendHealth = map[string]bool{}
	healthMutex   sync.RWMutex
)

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
	if len(backends) == 0 {
		return "", fmt.Errorf("no backends configured")
	}
	n := len(backends)
	start := atomic.AddUint64(&currentServer, 1) - 1
	for i := 0; i < n; i++ {
		b := backends[(int(start)+i)%n]
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
	if len(backends) == 0 {
		return false
	}
	for _, b := range backends {
		v, known := backendHealth[b]
		if !known || v {
			return true
		}
	}
	return false
}

// compositeHealthChecker bridges the balancer.HealthChecker interface and
// the circuit breaker: a backend is available only if both the health map
// and the breaker agree.
type compositeHealthChecker struct {
	breaker *health.Registry
}

func (c compositeHealthChecker) IsAvailable(backend string) bool {
	if !isHealthy(backend) {
		return false
	}
	if c.breaker != nil {
		return c.breaker.IsAvailable(backend)
	}
	return true
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
	fs.StringVar(&configPath, "config", "", "config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(config.ResolveConfigPath(configPath))
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

	// Populate legacy globals (tests + readyz probe)
	for _, b := range cfg.Backends {
		backends = append(backends, b.Host)
	}

	// Logging
	setupLogging(cfg)

	// Shutdown timeout
	shutdownTimeout := time.Duration(cfg.ShutdownTimeoutSec) * time.Second
	if shutdownTimeout <= 0 {
		shutdownTimeout = 15 * time.Second
	}

	// Metrics registry
	reg := metrics.New()
	// Pre-register backend gauges
	for _, b := range cfg.Backends {
		reg.Gauge(metrics.MetricName("haribon_backend_healthy", "backend", b.Host)).Set(1)
		reg.Gauge(metrics.MetricName("haribon_breaker_state", "backend", b.Host)).Set(0)
	}

	// Build backend entries for balancer
	entries := make([]balancer.BackendEntry, len(cfg.Backends))
	for i, b := range cfg.Backends {
		entries[i] = balancer.BackendEntry{URL: b.Host, Weight: b.Weight}
	}

	// Balancer
	bal, err := balancer.New(cfg.Balancer.Strategy, entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "balancer error: %v\n", err)
		os.Exit(1)
	}
	log.Printf("balancer strategy: %s", cfg.Balancer.Strategy)

	// Circuit breaker registry
	breakerURLs := make([]string, len(cfg.Backends))
	for i, b := range cfg.Backends {
		breakerURLs[i] = b.Host
	}
	breakerLog := func(backend, state, reason string) {
		writeLog(LogEntry{
			Method:  "BREAKER",
			Path:    "circuit-breaker",
			Backend: backend,
			Status:  0,
			Level:   "warn",
		})
		if reg != nil {
			v := int64(0) // closed=0, open=1, half_open=2
			switch state {
			case "open":
				v = 1
			case "half_open":
				v = 2
			}
			reg.Gauge(metrics.MetricName("haribon_breaker_state", "backend", backend)).Set(v)
		}
		_ = reason
	}
	breakerReg := health.NewRegistry(
		breakerURLs,
		cfg.Breaker.FailureThreshold,
		time.Duration(cfg.Breaker.CooldownSec)*time.Second,
		breakerLog,
	)

	// Composite health checker (health map + breaker)
	hc := compositeHealthChecker{breaker: breakerReg}

	// Proxy handler
	proxyHandler := proxy.New(
		bal, hc, breakerReg,
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
	mux.Handle("/metrics", reg.Handler())
	mux.Handle("/", proxyHandler)

	srv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		MaxHeaderBytes: 1 << 20,
	}

	// Context for graceful shutdown + active health scheduler
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Active health scheduler (if enabled)
	if cfg.Health.Enabled {
		sched := health.NewScheduler(
			backends,
			health.SchedulerConfig{
				IntervalSec:        cfg.Health.IntervalSec,
				TimeoutSec:         cfg.Health.TimeoutSec,
				Path:               cfg.Health.Path,
				HealthyThreshold:   cfg.Health.HealthyThreshold,
				UnhealthyThreshold: cfg.Health.UnhealthyThreshold,
			},
			setHealth,
			makeHealthLogger(reg),
			nil,
		)
		go sched.Run(ctx)
		log.Printf("active health scheduler started (interval: %ds)", cfg.Health.IntervalSec)
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

	log.Printf("shutting down (timeout: %s)…\n", shutdownTimeout)
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
	}
	log.Println("shutdown complete")
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
	fmt.Fprintf(os.Stderr, `haribon — lightweight Layer 7 load balancer

Usage:
  haribon <command> [flags]

Commands:
  start    Start the load balancer
  check    Validate a config file and exit (exit 0 ok / 1 error)
  version  Print version and exit

Flags (start, check):
  --config string   Config file path (default: $HARIBON_CONFIG or ./haribon-config.yml)

Examples:
  haribon start --config haribon-config.yml
  haribon check --config haribon-config.yml
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
	case "check", "validate":
		checkCommand(os.Args[2:])
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

// setupLogging wires logWriter to stdout + optional file.
func setupLogging(cfg config.Config) {
	if cfg.Logging {
		if cfg.LogPath == "" {
			cfg.LogPath = "./haribon.log"
		}
		if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0755); err != nil {
			log.Printf("log dir create warning: %v", err)
		}
		f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("log file error (fallback to stdout only): %v", err)
			logWriter = os.Stdout
		} else {
			logWriter = io.MultiWriter(os.Stdout, f)
		}
	} else {
		logWriter = os.Stdout
	}
}

// keep strconv imported for backward-compat (test file uses it indirectly)
var _ = strconv.Itoa
