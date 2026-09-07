// Package balancer implements pluggable load-balancing algorithms.
//
// Problem:  A single round-robin counter cannot fairly distribute load across
//
//	heterogeneous backends or track which server is least busy.
//
// Options:  (a) switch in handler, (b) strategy pattern behind interface.
// Choice:   (b) — Balancer interface with three concrete types; cli/main.go
//
//	calls NewBalancer and stays algorithm-agnostic.
//
// Failure:  All methods return ErrNoHealthyBackend when no backend passes the
//
//	health filter; caller emits 503.
//
// Observability: active-connection counter exposed via ActiveConns(); strategy
//
//	name logged at startup.
package balancer

import (
	"errors"
	"sync"
	"sync/atomic"
)

// Sentinel errors.
var (
	ErrNoBackends       = errors.New("no backends configured")
	ErrNoHealthyBackend = errors.New("no healthy backend available")
)

// HealthChecker is the minimal interface the balancer needs from the health
// layer to filter out unhealthy / open-circuit backends.
type HealthChecker interface {
	IsAvailable(backend string) bool
}

// Balancer picks the next backend URL for an incoming request.
type Balancer interface {
	// Next returns the next backend URL, filtered by the HealthChecker.
	Next(hc HealthChecker) (string, error)
	// Done must be called when a proxied request completes (used by least-conn).
	Done(backend string)
	// Backends returns the configured backend URLs (for logging / readyz).
	Backends() []string
}

// BackendEntry holds URL and weight for a single upstream.
type BackendEntry struct {
	URL    string
	Weight int // ignored by RR and least-conn
}

// ────────────────────────────────────────────────
// Round-Robin
// ────────────────────────────────────────────────

type roundRobin struct {
	backends []BackendEntry
	counter  uint64
}

// NewRoundRobin builds a pure round-robin balancer.
func NewRoundRobin(bs []BackendEntry) (Balancer, error) {
	if len(bs) == 0 {
		return nil, ErrNoBackends
	}
	return &roundRobin{backends: bs}, nil
}

func (r *roundRobin) Next(hc HealthChecker) (string, error) {
	n := len(r.backends)
	start := atomic.AddUint64(&r.counter, 1) - 1
	for i := 0; i < n; i++ {
		b := r.backends[(int(start)+i)%n]
		if hc.IsAvailable(b.URL) {
			return b.URL, nil
		}
	}
	return "", ErrNoHealthyBackend
}

func (r *roundRobin) Done(_ string) {}

func (r *roundRobin) Backends() []string {
	out := make([]string, len(r.backends))
	for i, b := range r.backends {
		out[i] = b.URL
	}
	return out
}

// ────────────────────────────────────────────────
// Weighted Round-Robin
// ────────────────────────────────────────────────
//
// Expands each backend into a slot list where backend i appears weight(i) times.
// A backend with weight 0 is treated as weight 1.

type weightedRR struct {
	slots   []string // expanded slot list, e.g. [a a b] for weights [2,1]
	counter uint64
}

// NewWeightedRoundRobin builds a weighted round-robin balancer.
func NewWeightedRoundRobin(bs []BackendEntry) (Balancer, error) {
	if len(bs) == 0 {
		return nil, ErrNoBackends
	}
	var slots []string
	for _, b := range bs {
		w := b.Weight
		if w <= 0 {
			w = 1
		}
		for j := 0; j < w; j++ {
			slots = append(slots, b.URL)
		}
	}
	return &weightedRR{slots: slots}, nil
}

func (w *weightedRR) Next(hc HealthChecker) (string, error) {
	n := len(w.slots)
	start := atomic.AddUint64(&w.counter, 1) - 1
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		url := w.slots[(int(start)+i)%n]
		if seen[url] {
			continue
		}
		seen[url] = true
		if hc.IsAvailable(url) {
			return url, nil
		}
	}
	return "", ErrNoHealthyBackend
}

func (w *weightedRR) Done(_ string) {}

func (w *weightedRR) Backends() []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range w.slots {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ────────────────────────────────────────────────
// Least Connections
// ────────────────────────────────────────────────

type leastConn struct {
	backends []BackendEntry
	conns    map[string]*int64 // atomic per-backend active-conn counter
	mu       sync.RWMutex
}

// NewLeastConnections builds a least-connections balancer.
func NewLeastConnections(bs []BackendEntry) (Balancer, error) {
	if len(bs) == 0 {
		return nil, ErrNoBackends
	}
	conns := make(map[string]*int64, len(bs))
	for _, b := range bs {
		v := int64(0)
		conns[b.URL] = &v
	}
	return &leastConn{backends: bs, conns: conns}, nil
}

func (lc *leastConn) Next(hc HealthChecker) (string, error) {
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	best := ""
	bestCount := int64(-1)
	for _, b := range lc.backends {
		if !hc.IsAvailable(b.URL) {
			continue
		}
		c := atomic.LoadInt64(lc.conns[b.URL])
		if bestCount < 0 || c < bestCount {
			best = b.URL
			bestCount = c
		}
	}
	if best == "" {
		return "", ErrNoHealthyBackend
	}
	atomic.AddInt64(lc.conns[best], 1)
	return best, nil
}

func (lc *leastConn) Done(backend string) {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	if p, ok := lc.conns[backend]; ok {
		atomic.AddInt64(p, -1)
	}
}

func (lc *leastConn) Backends() []string {
	out := make([]string, len(lc.backends))
	for i, b := range lc.backends {
		out[i] = b.URL
	}
	return out
}

// ActiveConns returns the current active-connection counts (for /metrics).
func (lc *leastConn) ActiveConns() map[string]int64 {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	out := make(map[string]int64, len(lc.conns))
	for k, v := range lc.conns {
		out[k] = atomic.LoadInt64(v)
	}
	return out
}

// ────────────────────────────────────────────────
// Factory
// ────────────────────────────────────────────────

// New returns a Balancer for the given strategy name.
// Valid values: "round_robin", "weighted_round_robin", "least_connections".
// Unknown strategy falls back to round_robin.
func New(strategy string, bs []BackendEntry) (Balancer, error) {
	switch strategy {
	case "weighted_round_robin":
		return NewWeightedRoundRobin(bs)
	case "least_connections":
		return NewLeastConnections(bs)
	default:
		return NewRoundRobin(bs)
	}
}
