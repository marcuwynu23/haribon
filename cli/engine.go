package main

// engine.go — the swappable runtime.
//
// Problem:  startCommand used to build the balancer, the circuit-breaker
//           registry, and the health scheduler once, then hand them to the
//           proxy, which held them forever. Config reloads and discovery
//           updates could then never reach the running proxy, which is why
//           hot reload, auto-discovery, and clustering were all inert.
// Options:  (a) mutate the existing balancer in place under a lock, (b) build a
//           new generation and publish it atomically.
// Choice:   (b) — same atomic.Pointer idiom as internal/config/snapshot.go. A
//           request in flight keeps using the generation it started on, so a
//           reload never drops or reroutes a request mid-flight.
// Failure:  a rebuild that fails validation logs and leaves the previous
//           generation serving — the proxy never runs without a backend pool.
// Observability: every rebuild logs the new pool size and strategy; cluster
//           state is exported as haribon_cluster_peers / haribon_cluster_term /
//           haribon_config_hash_mismatch_total.

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcuwynu23/haribon/internal/balancer"
	"github.com/marcuwynu23/haribon/internal/cluster"
	"github.com/marcuwynu23/haribon/internal/config"
	"github.com/marcuwynu23/haribon/internal/discover"
	"github.com/marcuwynu23/haribon/internal/health"
	"github.com/marcuwynu23/haribon/internal/metrics"
)

// engine is one immutable generation of routing state.
type engine struct {
	bal      balancer.Balancer
	breaker  *health.Registry
	health   engineHealthChecker
	backends []string
}

// engineHealthChecker decides whether a backend may receive traffic. A backend
// is available only when the local health map, the cluster view, and the
// circuit breaker all agree — a disagreement means something is wrong, and the
// safe answer is "don't send traffic there".
type engineHealthChecker struct {
	breaker *health.Registry
	node    *cluster.Node // nil unless clustering is enabled
}

func (c engineHealthChecker) IsAvailable(backend string) bool {
	local := isHealthy(backend)
	if !local {
		return false
	}
	if c.node != nil && !c.node.GetHealth(backend, local) {
		// A peer gossiped an unhealthy state for this backend.
		return false
	}
	if c.breaker != nil {
		return c.breaker.IsAvailable(backend)
	}
	return true
}

// swappable is the single object handed to proxy.New. It satisfies
// balancer.Balancer, balancer.HealthChecker, balancer.IPHash, and
// proxy.BreakRecorder by delegating every call to the current engine, so
// internal/proxy needs no changes and holds no locks of its own.
type swappable struct {
	cur atomic.Pointer[engine]
}

func (s *swappable) load() *engine { return s.cur.Load() }

// Next picks a backend, staying within one generation: the balancer and the
// health checker passed to it come from the same snapshot.
func (s *swappable) Next(_ balancer.HealthChecker) (string, error) {
	e := s.load()
	if e == nil {
		return "", balancer.ErrNoHealthyBackend
	}
	return e.bal.Next(e.health)
}

// NextForIP is what makes ip_hash real — the proxy type-asserts to
// balancer.IPHash and passes the client address through here.
func (s *swappable) NextForIP(_ balancer.HealthChecker, clientIP string) (string, error) {
	e := s.load()
	if e == nil {
		return "", balancer.ErrNoHealthyBackend
	}
	if iph, ok := e.bal.(balancer.IPHash); ok {
		return iph.NextForIP(e.health, clientIP)
	}
	return e.bal.Next(e.health)
}

func (s *swappable) Done(backend string) {
	if e := s.load(); e != nil {
		e.bal.Done(backend)
	}
}

func (s *swappable) Backends() []string {
	e := s.load()
	if e == nil {
		return nil
	}
	return e.backends
}

func (s *swappable) IsAvailable(backend string) bool {
	e := s.load()
	if e == nil {
		return false
	}
	return e.health.IsAvailable(backend)
}

func (s *swappable) RecordSuccess(backend string) {
	if e := s.load(); e != nil && e.breaker != nil {
		e.breaker.RecordSuccess(backend)
	}
}

func (s *swappable) RecordFailure(backend string) {
	if e := s.load(); e != nil && e.breaker != nil {
		e.breaker.RecordFailure(backend)
	}
}

// HasHealthyBackend reports whether at least one backend is available for
// routing. Used by /readyz so the readiness probe agrees with the proxy's
// actual accept/reject decision — local health, cluster peer health, and the
// circuit breaker all count, same as engineHealthChecker.IsAvailable.
func (s *swappable) HasHealthyBackend() bool {
	e := s.load()
	if e == nil || len(e.backends) == 0 {
		return false
	}
	for _, b := range e.backends {
		if e.health.IsAvailable(b) {
			return true
		}
	}
	return false
}

// ────────────────────────────────────────────────
// runtime
// ────────────────────────────────────────────────

// runtime owns the live generation and rebuilds it when the config file or the
// discovery provider reports a change.
type runtime struct {
	configPath string
	snapshot   *config.Snapshot
	reg        *metrics.Registry
	sw         *swappable
	node       *cluster.Node // nil unless clustering is enabled

	mu         sync.Mutex // serializes rebuilds
	discovered []string   // latest discovery result

	baseCtx   context.Context
	schedStop context.CancelFunc
}

func newRuntime(ctx context.Context, path string, snapshot *config.Snapshot, reg *metrics.Registry, node *cluster.Node) *runtime {
	return &runtime{
		configPath: path,
		snapshot:   snapshot,
		reg:        reg,
		sw:         &swappable{},
		node:       node,
		baseCtx:    ctx,
	}
}

// stateWriter is the health scheduler's sink. It records the local health map
// and, when clustering is on, fans the change out over gossip.
func (r *runtime) stateWriter() health.StateWriter {
	return func(backend string, healthy bool) {
		setHealth(backend, healthy)
		if r.node != nil {
			r.node.SetHealth(backend, healthy)
		}
	}
}

// breakerLog emits a log line and updates the breaker-state gauge.
func (r *runtime) breakerLog(backend, state, reason string) {
	writeLog(LogEntry{
		Method:  "BREAKER",
		Path:    "circuit-breaker",
		Backend: backend,
		Status:  0,
		Level:   "warn",
	})
	if r.reg != nil {
		v := int64(0) // closed=0, open=1, half_open=2
		switch state {
		case "open":
			v = 1
		case "half_open":
			v = 2
		}
		r.reg.Gauge(metrics.MetricName("haribon_breaker_state", "backend", backend)).Set(v)
	}
	_ = reason
}

// OnConfigReload is called by the config watcher after the snapshot has been
// swapped. It rebuilds the live generation from the new config.
func (r *runtime) OnConfigReload(cfg config.Config) {
	log.Printf("config: applying reload")
	r.rebuild(cfg)
}

// OnDiscovery is called by the discovery provider when the backend list changes.
func (r *runtime) OnDiscovery(urls []string) {
	r.mu.Lock()
	if discover.SameList(urls, r.discovered) {
		r.mu.Unlock()
		return
	}
	r.discovered = append([]string(nil), urls...)
	r.mu.Unlock()

	log.Printf("discovery: backend pool changed (%d entries)", len(urls))
	r.rebuild(r.snapshot.Load())
}

// rebuild constructs a new generation and publishes it atomically.
func (r *runtime) rebuild(cfg config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entries := mergeEntries(cfg.Backends, r.discovered)
	if len(entries) == 0 {
		log.Printf("config: rebuild produced an empty pool; keeping the previous one")
		return
	}

	urls := make([]string, len(entries))
	for i, e := range entries {
		urls[i] = e.URL
	}

	bal, err := balancer.New(cfg.Balancer.Strategy, entries)
	if err != nil {
		log.Printf("config: balancer rebuild failed (%s): %v — keeping the previous pool",
			cfg.Balancer.Strategy, err)
		return
	}

	// Carry per-backend state across the swap so a backend that is mid-cooldown
	// or mid-request keeps its place instead of looking fresh and idle.
	prev := r.sw.load()
	if prev != nil {
		if carrier, ok := bal.(balancer.ConnsCarrier); ok {
			carrier.CarryOverConns(prev.bal)
		}
	}

	breaker := health.NewRegistry(
		urls,
		cfg.Breaker.FailureThreshold,
		time.Duration(cfg.Breaker.CooldownSec)*time.Second,
		r.breakerLog,
	)
	if prev != nil {
		breaker.CarryOver(prev.breaker)
	}

	// Seed gauges for any backend we have not seen before.
	if r.reg != nil {
		for _, u := range urls {
			r.reg.Gauge(metrics.MetricName("haribon_backend_healthy", "backend", u)).Set(1)
			r.reg.Gauge(metrics.MetricName("haribon_breaker_state", "backend", u)).Set(0)
		}
	}

	r.stopScheduler()

	r.sw.cur.Store(&engine{
		bal:      bal,
		breaker:  breaker,
		health:   engineHealthChecker{breaker: breaker, node: r.node},
		backends: urls,
	})

	setBackends(urls)
	pruneHealth(urls)
	r.startScheduler(cfg, urls)

	log.Printf("config: routing %d backend(s) with strategy %s",
		len(urls), cfg.Balancer.Strategy)
}

// stopScheduler cancels a running health scheduler, if any.
func (r *runtime) stopScheduler() {
	if r.schedStop != nil {
		r.schedStop()
		r.schedStop = nil
	}
}

// startScheduler runs health probes against the current pool. Restarting the
// scheduler on each rebuild gives newly discovered backends a probe loop
// immediately, instead of waiting for a restart.
func (r *runtime) startScheduler(cfg config.Config, urls []string) {
	if !cfg.Health.Enabled {
		return
	}
	ctx, cancel := context.WithCancel(r.baseCtx)
	r.schedStop = cancel

	sched := health.NewScheduler(
		urls,
		health.SchedulerConfig{
			IntervalSec:        cfg.Health.IntervalSec,
			TimeoutSec:         cfg.Health.TimeoutSec,
			Path:               cfg.Health.Path,
			HealthyThreshold:   cfg.Health.HealthyThreshold,
			UnhealthyThreshold: cfg.Health.UnhealthyThreshold,
		},
		r.stateWriter(),
		makeHealthLogger(r.reg),
		nil,
	)
	go sched.Run(ctx)
}

// StartDiscovery primes the pool and then watches for changes until ctx ends.
// The provider's own Watch is used rather than a second polling loop here, so
// there is exactly one poller per provider.
//
// The initial lookup is synchronous on purpose. Discovering asynchronously
// leaves the proxy serving from a pool that may contain nothing but the config
// file's backends — or nothing at all for a discovery-only setup — for however
// long the first DNS lookup or file read takes.
func (r *runtime) StartDiscovery(p discover.Provider) {
	if urls, err := p.Discover(); err == nil && len(urls) > 0 {
		r.mu.Lock()
		r.discovered = append([]string(nil), urls...)
		r.mu.Unlock()
	} else if err != nil {
		log.Printf("discovery: initial lookup failed: %v", err)
	}
	go p.Watch(r.baseCtx.Done(), r.OnDiscovery)
}

// StartClusterMetrics publishes cluster gauges on the gossip interval.
func (r *runtime) StartClusterMetrics() {
	if r.node == nil || r.reg == nil {
		return
	}
	interval := time.Duration(r.node.GossipSec) * time.Second
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.baseCtx.Done():
				return
			case <-t.C:
				r.reg.Gauge("haribon_cluster_peers").Set(int64(r.node.LivePeerCount()))
				r.reg.Gauge("haribon_cluster_term").Set(int64(r.node.GetTerm()))
				r.reg.Counter("haribon_config_hash_mismatch_total").Set(r.node.ConfigMismatches())
			}
		}
	}()
}

// ────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────

// mergeEntries combines the config file's backends with anything discovery
// found. Config entries win on conflict and keep their weight; discovered URLs
// are appended with weight 1.
func mergeEntries(cfgBackends []config.Backend, discovered []string) []balancer.BackendEntry {
	entries := make([]balancer.BackendEntry, 0, len(cfgBackends)+len(discovered))
	seen := make(map[string]bool, len(cfgBackends)+len(discovered))

	for _, b := range cfgBackends {
		if b.Host == "" || seen[b.Host] {
			continue
		}
		seen[b.Host] = true
		entries = append(entries, balancer.BackendEntry{URL: b.Host, Weight: b.Weight})
	}
	for _, u := range discovered {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		entries = append(entries, balancer.BackendEntry{URL: u, Weight: 1})
	}
	return entries
}

// pruneHealth drops health records for backends no longer in the pool so a
// long-running process with churning discovery does not accumulate state for
// servers that went away.
func pruneHealth(keep []string) {
	alive := make(map[string]bool, len(keep))
	for _, u := range keep {
		alive[u] = true
	}
	healthMutex.Lock()
	defer healthMutex.Unlock()
	for u := range backendHealth {
		if !alive[u] {
			delete(backendHealth, u)
		}
	}
}
