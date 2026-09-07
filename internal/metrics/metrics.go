// Package metrics exposes a /metrics endpoint with Prometheus-format counters.
//
// Problem:  Operators need quantitative signals (request rate, error rate,
//
//	backend health, retries, breaker state) to build alerts.
//
// Options:  (a) prometheus/client_golang library, (b) hand-rolled text/plain.
// Choice:   (b) — zero new dependencies; expvar-style atomic counters serialised
//
//	as Prometheus text format at request time. If full Prometheus SDK is
//	needed later, the metric names are identical so dashboards survive.
//
// Failure:  /metrics always returns 200; a partial write is still useful.
// Observability: this package IS the observability layer — it exposes all counters
//
//	that the proxy and breaker update.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing int64.
type Counter struct{ v int64 }

func (c *Counter) Inc()        { atomic.AddInt64(&c.v, 1) }
func (c *Counter) Add(n int64) { atomic.AddInt64(&c.v, n) }
func (c *Counter) Load() int64 { return atomic.LoadInt64(&c.v) }

// Gauge is a settable int64 (e.g. healthy=1/0, active_conns).
type Gauge struct{ v int64 }

func (g *Gauge) Set(n int64) { atomic.StoreInt64(&g.v, n) }
func (g *Gauge) Inc()        { atomic.AddInt64(&g.v, 1) }
func (g *Gauge) Dec()        { atomic.AddInt64(&g.v, -1) }
func (g *Gauge) Load() int64 { return atomic.LoadInt64(&g.v) }

// Registry holds all named metrics and renders the /metrics response.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		counters: make(map[string]*Counter),
		gauges:   make(map[string]*Gauge),
	}
}

// Counter returns (creating if needed) a named counter.
func (r *Registry) Counter(name string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{}
	r.counters[name] = c
	return c
}

// Gauge returns (creating if needed) a named gauge.
func (r *Registry) Gauge(name string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &Gauge{}
	r.gauges[name] = g
	return g
}

// Handler returns an http.HandlerFunc that writes Prometheus text format.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		r.Render(w)
	}
}

// Render writes all metrics in Prometheus text format to w.
func (r *Registry) Render(w io.Writer) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Counters
	for name, c := range r.counters {
		fmt.Fprintf(w, "# TYPE %s counter\n", name)
		fmt.Fprintf(w, "%s %d\n", name, c.Load())
	}
	// Gauges
	for name, g := range r.gauges {
		fmt.Fprintf(w, "# TYPE %s gauge\n", name)
		fmt.Fprintf(w, "%s %d\n", name, g.Load())
	}
}

// ────────────────────────────────────────────────
// Well-known metric names (used by proxy + breaker)
// ────────────────────────────────────────────────

// MetricName builds a labelled metric name in the form:
//
//	base{label="value"}
func MetricName(base, label, value string) string {
	return fmt.Sprintf(`%s{%s="%s"}`, base, label, value)
}
