package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/marcuwynu23/haribon/internal/balancer"
	"github.com/marcuwynu23/haribon/internal/proxy"
)

// ─── helpers ───

type alwaysHealthy struct{}

func (alwaysHealthy) IsAvailable(_ string) bool { return true }

type noneHealthy struct{}

func (noneHealthy) IsAvailable(_ string) bool { return false }

type selectiveHealth struct{ allowed map[string]bool }

func (s selectiveHealth) IsAvailable(b string) bool { return s.allowed[b] }

func newHandler(bal balancer.Balancer, hc balancer.HealthChecker, maxRetries int) *proxy.Handler {
	return proxy.New(
		bal, hc, nil, nil,
		proxy.Config{MaxRetries: maxRetries},
		nil, nil,
	)
}

// ─── round-robin integration ───

func TestProxy_RoundRobinIntegration(t *testing.T) {
	hits := [2]int{}
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

	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{
		{URL: srv1.URL}, {URL: srv2.URL},
	})
	h := newHandler(bal, alwaysHealthy{}, 0)

	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200 got %d", i, rec.Code)
		}
	}
	if hits[0] == 0 || hits[1] == 0 {
		t.Fatalf("round-robin skewed: %v", hits)
	}
}

func TestProxy_AllBackendsDown_503(t *testing.T) {
	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{{URL: "http://127.0.0.1:1"}})
	h := newHandler(bal, alwaysHealthy{}, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestProxy_NoHealthyBackend_503(t *testing.T) {
	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{{URL: "http://a"}})
	h := newHandler(bal, noneHealthy{}, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// ─── retry ───

func TestProxy_RetriesIdempotentOnFailure(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Two backends: first is dead port, second is live server
	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{
		{URL: "http://127.0.0.1:2"},
		{URL: srv.URL},
	})
	h := proxy.New(bal, alwaysHealthy{}, nil, nil, proxy.Config{MaxRetries: 1}, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", rec.Code)
	}

	// X-Haribon-Retries header should be set
	if rec.Header().Get("X-Haribon-Retries") == "" {
		t.Fatal("expected X-Haribon-Retries header")
	}
}

func TestProxy_DoesNotRetryPOST(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// First backend dead, second alive
	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{
		{URL: "http://127.0.0.1:3"},
		{URL: srv.URL},
	})
	h := proxy.New(bal, alwaysHealthy{}, nil, nil, proxy.Config{MaxRetries: 1}, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	// POST is not idempotent → no retry → should be 503
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST should not be retried, expected 503 got %d", rec.Code)
	}
}

// ─── header stripping ───

func TestProxy_StripHopByHopFromResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Custom", "keep")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{{URL: srv.URL}})
	h := newHandler(bal, alwaysHealthy{}, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Transfer-Encoding") != "" {
		t.Fatal("Transfer-Encoding should be stripped")
	}
	if rec.Header().Get("X-Custom") != "keep" {
		t.Fatal("X-Custom should be preserved")
	}
}

// ─── X-Forwarded headers ───

func TestProxy_XForwardedHeaders(t *testing.T) {
	var gotXFF, gotProto string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bal, _ := balancer.NewRoundRobin([]balancer.BackendEntry{{URL: srv.URL}})
	h := newHandler(bal, alwaysHealthy{}, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	h.ServeHTTP(rec, req)

	if !strings.Contains(gotXFF, "10.0.0.1") {
		t.Fatalf("X-Forwarded-For should contain client IP, got: %q", gotXFF)
	}
	if gotProto != "http" {
		t.Fatalf("X-Forwarded-Proto should be http, got: %q", gotProto)
	}
}
