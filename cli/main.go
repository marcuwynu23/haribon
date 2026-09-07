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

	"github.com/marcuwynu23/haribon/internal/config"
	"github.com/marcuwynu23/haribon/internal/health"
)

// version is injected at build time via:
//
//	-ldflags "-X main.version=$(VERSION)"
var version = "dev"

// ==========================
// LOG STRUCT (LOKI FRIENDLY)
// ==========================

type LogEntry struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Backend    string `json:"backend"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Level      string `json:"level"`
}

// ==========================
// GLOBAL STATE
// ==========================

var (
	backends      []string
	currentServer uint64

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
// HEALTH CHECKER (adapter)
// ==========================

// balancerHealthChecker satisfies health.HealthChecker using the global
// backendHealth map.
type balancerHealthChecker struct{}

func (balancerHealthChecker) HasHealthyBackend() bool {
	healthMutex.RLock()
	defer healthMutex.RUnlock()
	if len(backends) == 0 {
		return false
	}
	for _, b := range backends {
		v, known := backendHealth[b]
		// unknown → optimistic (same as isHealthy)
		if !known || v {
			return true
		}
	}
	return false
}

// ==========================
// HEALTH STATE HELPERS
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
	// unknown backend → optimistic: treat as healthy until proven otherwise
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
// HOP-BY-HOP HEADERS
// ==========================

// hopByHopHeaders is the fixed set defined in RFC 7230 §6.1 plus Proxy-*.
// These must be stripped before forwarding a response to the client.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop removes hop-by-hop headers from h, including any headers
// listed in the Connection header value itself (RFC 7230 §6.1).
func stripHopByHop(h http.Header) {
	// headers named in Connection: header must also be stripped
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// ==========================
// LOAD BALANCER CORE
// ==========================

func getNextBackend() (string, error) {
	if len(backends) == 0 {
		return "", fmt.Errorf("no backends configured")
	}

	n := len(backends)
	start := atomic.AddUint64(&currentServer, 1) - 1

	for i := 0; i < n; i++ {
		idx := (int(start) + i) % n
		b := backends[idx]
		if isHealthy(b) {
			return b, nil
		}
	}

	return "", fmt.Errorf("no healthy backend available")
}

// ==========================
// HANDLER
// ==========================

func loadBalancer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Determine client IP for X-Forwarded-For
	clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientIP = r.RemoteAddr
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

		// Copy request headers (minus hop-by-hop)
		for k, v := range r.Header {
			req.Header[k] = v
		}
		stripHopByHop(req.Header)

		// Append X-Forwarded-For
		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", clientIP)
		}

		// Set X-Forwarded-Proto
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

		// Strip hop-by-hop from response before forwarding
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

	for _, b := range cfg.Backends {
		backends = append(backends, b.Host)
	}

	// Setup logging
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

	// Determine shutdown timeout (default 15s)
	shutdownTimeout := time.Duration(cfg.ShutdownTimeoutSec) * time.Second
	if shutdownTimeout <= 0 {
		shutdownTimeout = 15 * time.Second
	}

	addr := fmt.Sprintf("%s:%d", cfg.MainHost, cfg.MainPort)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health.Healthz)
	mux.HandleFunc("/readyz", health.Readyz(balancerHealthChecker{}))
	mux.HandleFunc("/", loadBalancer)

	srv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		MaxHeaderBytes: 1 << 20, // 1 MiB
	}

	// Graceful shutdown on SIGINT / SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("running on %s\n", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(2)
		}
	}()

	<-ctx.Done()
	stop() // release signal resources

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

	fmt.Printf("ok: %d backend(s), probes /healthz /readyz enabled\n", len(cfg.Backends))
	for i, b := range cfg.Backends {
		fmt.Printf("  [%d] %s\n", i, b.Host)
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

// resolveConfigPath kept for backward-compat with existing tests.
// Delegates to config.ResolveConfigPath.
func resolveConfigPath(cli string) string {
	return config.ResolveConfigPath(cli)
}

// applyEnvOverrides kept for backward-compat with existing tests.
func applyEnvOverrides(cfg *config.Config) {
	config.ApplyEnvOverrides(cfg)
}

// loadConfig kept for backward-compat with existing tests.
func loadConfig(filename string) (config.Config, error) {
	return config.Load(filename)
}

// Expose port parsing for backward-compat with existing tests.
var _ = strconv.Atoi
