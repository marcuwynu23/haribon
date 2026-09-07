// Package proxy implements the HTTP reverse-proxy handler with retry policy
// and request forwarding logic.
//
// Problem:  A single backend failure on the first attempt should transparently
//
//	retry on the next healthy backend before returning 503.
//
// Options:  (a) retry inside balancer, (b) retry loop in handler.
// Choice:   (b) — the handler owns the response lifecycle (status code, headers,
//
//	body copy) so it is the natural place to retry; balancer stays pure.
//
// Failure:  After max_retries+1 total attempts all fail → 503 + error log.
//
//	Non-idempotent methods (POST, PATCH) are NOT retried to prevent
//	duplicate side-effects; they get exactly one attempt.
//
// Observability: X-Haribon-Retries response header; per-backend request and
//
//	retry counters incremented in the provided metrics.Registry.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marcuwynu23/haribon/internal/balancer"
	"github.com/marcuwynu23/haribon/internal/metrics"
)

// hopByHopHeaders is the RFC 7230 §6.1 fixed list plus Proxy-*.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// stripHopByHop removes hop-by-hop headers from h.
func stripHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// idempotentMethod returns true for HTTP methods safe to retry.
func idempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// retryableStatus returns true when the response status warrants a retry.
func retryableStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// BreakRecorder allows the proxy to report success/failure to the circuit breaker.
type BreakRecorder interface {
	RecordSuccess(backend string)
	RecordFailure(backend string)
}

// LogEntry is the structured log emitted after each proxied request.
type LogEntry struct {
	Time       string
	Method     string
	Path       string
	Backend    string
	Status     int
	DurationMS int64
	Retries    int
	Level      string
}

// Logger callback (same signature used in main.go).
type Logger func(e LogEntry)

// Config holds proxy behaviour parameters.
type Config struct {
	HandlerTimeout time.Duration // per-attempt context deadline (default 10s)
	MaxRetries     int           // additional attempts beyond the first (default 1)
}

// Handler is the HTTP handler that proxies requests through the balancer.
type Handler struct {
	bal     balancer.Balancer
	hc      balancer.HealthChecker
	breaker BreakRecorder
	client  *http.Client
	cfg     Config
	reg     *metrics.Registry
	logger  Logger
}

// New constructs a proxy Handler.
//
//   - bal: the load-balancer (picks next backend)
//   - hc: the health checker (filtered by bal.Next)
//   - breaker: circuit-breaker recorder (may be nil)
//   - client: shared http.Client (nil → 5s default)
//   - cfg: retry / timeout config
//   - reg: metrics registry (nil → no metrics)
//   - logger: structured log callback (nil → silent)
func New(
	bal balancer.Balancer,
	hc balancer.HealthChecker,
	breaker BreakRecorder,
	client *http.Client,
	cfg Config,
	reg *metrics.Registry,
	logger Logger,
) *Handler {
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	if cfg.HandlerTimeout <= 0 {
		cfg.HandlerTimeout = 10 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	return &Handler{
		bal: bal, hc: hc, breaker: breaker,
		client: client, cfg: cfg, reg: reg, logger: logger,
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientIP = r.RemoteAddr
	}

	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}

	// Buffer body for retries on idempotent methods.
	var bodyBuf []byte
	if r.Body != nil && idempotentMethod(r.Method) {
		bodyBuf, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}

	maxAttempts := 1
	if idempotentMethod(r.Method) {
		maxAttempts = 1 + h.cfg.MaxRetries
	}

	retries := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			retries++
			time.Sleep(25 * time.Millisecond) // backoff
		}

		server, err := h.bal.Next(h.hc)
		if err != nil {
			break // no healthy backend at all
		}

		h.incCounter("haribon_requests_total", "backend", server)

		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.HandlerTimeout)

		var reqBody io.Reader
		if idempotentMethod(r.Method) {
			reqBody = bytes.NewReader(bodyBuf)
		} else {
			reqBody = r.Body
		}

		targetURL := server + r.URL.RequestURI()
		req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, reqBody)
		if err != nil {
			cancel()
			h.bal.Done(server)
			if h.breaker != nil {
				h.breaker.RecordFailure(server)
			}
			continue
		}

		for k, v := range r.Header {
			req.Header[k] = v
		}
		stripHopByHop(req.Header)

		// X-Forwarded-For append
		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		req.Header.Set("X-Forwarded-Proto", proto)

		resp, err := h.client.Do(req)
		cancel()
		h.bal.Done(server)

		if err != nil {
			if h.breaker != nil {
				h.breaker.RecordFailure(server)
			}
			continue
		}

		// Retry on 502/503/504 for idempotent methods.
		if retryableStatus(resp.StatusCode) && idempotentMethod(r.Method) && attempt < maxAttempts-1 {
			_ = resp.Body.Close()
			if h.breaker != nil {
				h.breaker.RecordFailure(server)
			}
			h.incCounter("haribon_retries_total", "backend", server)
			continue
		}

		// Success path.
		if h.breaker != nil {
			h.breaker.RecordSuccess(server)
		}

		stripHopByHop(resp.Header)
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		if retries > 0 {
			w.Header().Set("X-Haribon-Retries", strconv.Itoa(retries))
		}

		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		_ = resp.Body.Close()

		h.incCounter(
			fmt.Sprintf(`haribon_responses_total{backend="%s",code="%d"}`, server, resp.StatusCode),
			"", "",
		)

		if h.logger != nil {
			h.logger(LogEntry{
				Method: r.Method, Path: r.URL.Path, Backend: server,
				Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
				Retries: retries, Level: "info",
			})
		}

		h.setDurationGauge(server, time.Since(start).Milliseconds())
		return
	}

	// All attempts exhausted.
	if h.logger != nil {
		h.logger(LogEntry{
			Method: r.Method, Path: r.URL.Path, Backend: "",
			Status: http.StatusServiceUnavailable, DurationMS: time.Since(start).Milliseconds(),
			Retries: retries, Level: "error",
		})
	}
	h.incCounter("haribon_errors_total", "reason", "all_backends_failed")

	if retries > 0 {
		w.Header().Set("X-Haribon-Retries", strconv.Itoa(retries))
	}
	http.Error(w, "All backend servers failed", http.StatusServiceUnavailable)
}

func (h *Handler) incCounter(name, label, value string) {
	if h.reg == nil {
		return
	}
	key := name
	if label != "" {
		key = metrics.MetricName(name, label, value)
	}
	h.reg.Counter(key).Inc()
}

func (h *Handler) setDurationGauge(backend string, ms int64) {
	if h.reg == nil {
		return
	}
	key := metrics.MetricName("haribon_last_duration_ms", "backend", backend)
	h.reg.Gauge(key).Set(ms)
}
