package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marcuwynu23/haribon/internal/config"
)

// resetState clears all global state between tests.
func resetState() {
	backends = nil
	atomic.StoreUint64(&currentServer, 0)
	backendHealth = map[string]bool{}
	logWriter = os.Stdout
}

// ==========================
// HEALTH MAP
// ==========================

func TestHealthMap(t *testing.T) {
	resetState()
	setHealth("http://a", true)
	if !isHealthy("http://a") {
		t.Fatal("expected healthy backend")
	}
}

// ==========================
// ROUND ROBIN
// ==========================

func TestGetNextBackend_RoundRobin(t *testing.T) {
	resetState()
	backends = []string{"a", "b", "c"}

	first, _ := getNextBackend()
	second, _ := getNextBackend()
	third, _ := getNextBackend()
	fourth, _ := getNextBackend() // wrap-around

	if first != "a" || second != "b" || third != "c" || fourth != "a" {
		t.Fatalf("round robin failed: %s %s %s %s", first, second, third, fourth)
	}
}

func TestRoundRobinSkipsDead(t *testing.T) {
	resetState()
	backends = []string{"a", "b"}
	setHealth("a", false)
	setHealth("b", true)

	b, err := getNextBackend()
	if err != nil {
		t.Fatal(err)
	}
	if b != "b" {
		t.Fatalf("expected b, got %s", b)
	}
}

func TestNoBackends(t *testing.T) {
	resetState()
	_, err := getNextBackend()
	if err == nil {
		t.Fatal("expected error when backends is empty")
	}
}

func TestAllUnhealthy(t *testing.T) {
	resetState()
	backends = []string{"a", "b"}
	setHealth("a", false)
	setHealth("b", false)

	_, err := getNextBackend()
	if err == nil {
		t.Fatal("expected error when all backends unhealthy")
	}
}

func TestEmptyHealthMap_AllTreatedHealthy(t *testing.T) {
	resetState()
	backends = []string{"a", "b"}
	// backendHealth is empty → startup/test mode → all healthy

	got, err := getNextBackend()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected a backend to be selected")
	}
}

// ==========================
// CONCURRENT SAFETY
// ==========================

func TestConcurrentNextBackend_NoRace(t *testing.T) {
	resetState()
	backends = []string{"a", "b", "c"}
	setHealth("a", true)
	setHealth("b", true)
	setHealth("c", true)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = getNextBackend()
			}
		}()
	}
	wg.Wait()
}

// ==========================
// LOGGING
// ==========================

func TestWriteLog_JSONFormat(t *testing.T) {
	resetState()
	var buf strings.Builder
	logWriter = &buf

	writeLog(LogEntry{
		Method:  "GET",
		Path:    "/",
		Backend: "http://localhost",
		Status:  200,
		Level:   "info",
	})

	out := buf.String()

	if !strings.Contains(out, `"method":"GET"`) {
		t.Fatalf("missing method field in log: %s", out)
	}
	if !strings.Contains(out, `"backend"`) {
		t.Fatalf("missing backend field in log")
	}
	if !strings.Contains(out, `"duration_ms"`) {
		t.Fatalf("missing duration_ms in log")
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "}") {
		t.Fatalf("log line should end with }")
	}
	// Must be valid JSON
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatalf("log is not valid JSON: %v | %s", err, out)
	}
}

// ==========================
// ENV OVERRIDES
// ==========================

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("HARIBON_HOST", "127.0.0.1")
	t.Setenv("HARIBON_PORT", "9999")

	cfg := config.Config{}
	applyEnvOverrides(&cfg)

	if cfg.MainHost != "127.0.0.1" {
		t.Fatal("host override failed")
	}
	if cfg.MainPort != 9999 {
		t.Fatal("port override failed")
	}
}

// ==========================
// CONFIG PATH
// ==========================

func TestResolveConfigPath_CLI(t *testing.T) {
	got := resolveConfigPath("/my/config.yml")
	if got != "/my/config.yml" {
		t.Fatalf("expected cli path, got %s", got)
	}
}

func TestResolveConfigPath_Default(t *testing.T) {
	t.Setenv("HARIBON_CONFIG", "")
	got := resolveConfigPath("")
	if got != "./haribon-config.yml" {
		t.Fatalf("expected default path, got %s", got)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := loadConfig("/no/such/file.yml")
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

// ==========================
// HOP-BY-HOP HEADER STRIPPING
// ==========================

func TestStripHopByHop_RemovesFixed(t *testing.T) {
	h := http.Header{}
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Connection", "keep-alive")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("X-Custom", "preserved")

	stripHopByHop(h)

	if h.Get("Transfer-Encoding") != "" {
		t.Fatal("Transfer-Encoding should be stripped")
	}
	if h.Get("Keep-Alive") != "" {
		t.Fatal("Keep-Alive should be stripped")
	}
	if h.Get("X-Custom") != "preserved" {
		t.Fatal("X-Custom should be preserved")
	}
}

func TestStripHopByHop_RespectsConnectionHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-My-Hop")
	h.Set("X-My-Hop", "value")

	stripHopByHop(h)

	if h.Get("X-My-Hop") != "" {
		t.Fatal("header listed in Connection should be stripped")
	}
}

// ==========================
// PROXY INTEGRATION
// ==========================

func TestProxy_RoundRobinIntegration(t *testing.T) {
	resetState()

	hits := make([]int, 2)
	var mu sync.Mutex

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[0]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[1]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv2.Close()

	backends = []string{srv1.URL, srv2.URL}

	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		loadBalancer(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}

	// Each server should have received requests (rough round-robin)
	if hits[0] == 0 || hits[1] == 0 {
		t.Fatalf("round-robin skewed: srv1=%d srv2=%d", hits[0], hits[1])
	}
}

func TestProxy_AllBackendsDown_503(t *testing.T) {
	resetState()

	// Create and immediately close servers so connections fail
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backends = []string{srv1.URL, srv2.URL}
	setHealth(srv1.URL, true)
	setHealth(srv2.URL, true)
	srv1.Close()
	srv2.Close()

	var buf strings.Builder
	logWriter = &buf

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	loadBalancer(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	// Verify error log entry
	out := buf.String()
	if !strings.Contains(out, `"level":"error"`) {
		t.Fatalf("missing error level in log: %s", out)
	}
	if !strings.Contains(out, `"status":503`) {
		t.Fatalf("missing status 503 in log: %s", out)
	}
}

func TestProxy_XForwardedFor(t *testing.T) {
	resetState()

	var gotXFF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	backends = []string{srv.URL}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:54321"
	loadBalancer(rec, req)

	if !strings.Contains(gotXFF, "10.0.0.1") {
		t.Fatalf("X-Forwarded-For should contain client IP, got: %q", gotXFF)
	}
}

func TestProxy_XForwardedProto(t *testing.T) {
	resetState()

	var gotProto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	backends = []string{srv.URL}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	// r.TLS is nil → proto = "http"
	loadBalancer(rec, req)

	if gotProto != "http" {
		t.Fatalf("X-Forwarded-Proto should be 'http', got: %q", gotProto)
	}
}

// ==========================
// HAS HEALTHY BACKEND (adapter)
// ==========================

func TestHasHealthyBackend_NoBackends(t *testing.T) {
	resetState()
	checker := balancerHealthChecker{}
	if checker.HasHealthyBackend() {
		t.Fatal("should return false with no backends")
	}
}

func TestHasHealthyBackend_AllUnknown_ReturnsTrue(t *testing.T) {
	resetState()
	backends = []string{"http://a", "http://b"}
	// health map empty → unknown → optimistic true
	checker := balancerHealthChecker{}
	if !checker.HasHealthyBackend() {
		t.Fatal("unknown backends should be treated as healthy")
	}
}

func TestHasHealthyBackend_AllFalse_ReturnsFalse(t *testing.T) {
	resetState()
	backends = []string{"http://a", "http://b"}
	setHealth("http://a", false)
	setHealth("http://b", false)
	checker := balancerHealthChecker{}
	if checker.HasHealthyBackend() {
		t.Fatal("all-unhealthy should return false")
	}
}

func TestHasHealthyBackend_OneHealthy_ReturnsTrue(t *testing.T) {
	resetState()
	backends = []string{"http://a", "http://b"}
	setHealth("http://a", false)
	setHealth("http://b", true)
	checker := balancerHealthChecker{}
	if !checker.HasHealthyBackend() {
		t.Fatal("one healthy backend should return true")
	}
}
