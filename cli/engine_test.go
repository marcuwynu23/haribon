package main

// engine_test.go — tests for the swappable runtime.
//
// These cover the property the whole design exists for: a rebuild publishes a
// complete new generation, and nothing that observes the runtime ever sees a
// half-built one. The previous wiring built everything once, so hot reload,
// discovery, and clustering had no effect no matter how they were configured.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcuwynu23/haribon/internal/balancer"
	"github.com/marcuwynu23/haribon/internal/cluster"
	"github.com/marcuwynu23/haribon/internal/config"
	"github.com/marcuwynu23/haribon/internal/health"
	"github.com/marcuwynu23/haribon/internal/metrics"
)

// ---------- test doubles ----------

type fakeBalancer struct {
	next  string
	err   error
	dones []string
}

func (f *fakeBalancer) Next(balancer.HealthChecker) (string, error) { return f.next, f.err }
func (f *fakeBalancer) Done(b string)                               { f.dones = append(f.dones, b) }
func (f *fakeBalancer) Backends() []string                          { return []string{f.next} }

type alwaysUp struct{}

func (alwaysUp) IsAvailable(string) bool { return true }

// testRuntime builds a runtime with a metrics registry and snapshot, which is
// what rebuild touches beyond the config itself.
func testRuntime(t *testing.T, cfg config.Config, node *cluster.Node) *runtime {
	t.Helper()
	return newRuntime(context.Background(), "haribon-config.yml", config.NewSnapshot(cfg), metrics.New(), node)
}

func baseConfig(urls ...string) config.Config {
	cfg := config.Config{
		Balancer: config.BalancerConfig{Strategy: "round_robin"},
		Breaker:  config.BreakerConfig{FailureThreshold: 2, CooldownSec: 30},
	}
	for _, u := range urls {
		cfg.Backends = append(cfg.Backends, config.Backend{Host: u, Weight: 1})
	}
	return cfg
}

// ---------- mergeEntries ----------

func TestMergeEntries_ConfigWinsAndDedupes(t *testing.T) {
	entries := mergeEntries(
		[]config.Backend{{Host: "http://a:1", Weight: 5}, {Host: "http://a:1", Weight: 5}},
		[]string{"http://a:1", "http://b:2"},
	)

	if len(entries) != 2 {
		t.Fatalf("expected 2 deduplicated entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].URL != "http://a:1" || entries[0].Weight != 5 {
		t.Fatalf("the configured backend must keep its weight, got %+v", entries[0])
	}
	if entries[1].URL != "http://b:2" || entries[1].Weight != 1 {
		t.Fatalf("a discovered backend must be appended with weight 1, got %+v", entries[1])
	}
}

func TestMergeEntries_SkipsBlanks(t *testing.T) {
	entries := mergeEntries([]config.Backend{{Host: ""}}, []string{"", "http://b:2"})
	if len(entries) != 1 || entries[0].URL != "http://b:2" {
		t.Fatalf("blank URLs must be skipped, got %+v", entries)
	}
}

func TestMergeEntries_DiscoveredOnly(t *testing.T) {
	// A discovery-only setup has no backends in the config file at all.
	entries := mergeEntries(nil, []string{"http://dns-1:80", "http://dns-2:80"})
	if len(entries) != 2 {
		t.Fatalf("expected 2 discovered entries, got %+v", entries)
	}
}

// ---------- swappable ----------

func TestSwappable_NilEngineIsAnErrorNotAPanic(t *testing.T) {
	sw := &swappable{}
	if _, err := sw.Next(alwaysUp{}); !errors.Is(err, balancer.ErrNoHealthyBackend) {
		t.Fatalf("expected ErrNoHealthyBackend before the first rebuild, got %v", err)
	}
	if _, err := sw.NextForIP(alwaysUp{}, "10.0.0.1"); !errors.Is(err, balancer.ErrNoHealthyBackend) {
		t.Fatalf("expected ErrNoHealthyBackend from NextForIP, got %v", err)
	}
	if sw.Backends() != nil {
		t.Fatal("Backends must be empty before the first rebuild")
	}
	if sw.IsAvailable("http://a:1") {
		t.Fatal("no backend can be available before the first rebuild")
	}
	sw.Done("http://a:1")          // must not panic
	sw.RecordSuccess("http://a:1") // must not panic
	sw.RecordFailure("http://a:1") // must not panic
}

func TestSwappable_DelegatesToCurrentGeneration(t *testing.T) {
	fb := &fakeBalancer{next: "http://a:1"}
	sw := &swappable{}
	sw.cur.Store(&engine{bal: fb, backends: []string{"http://a:1"}})

	got, err := sw.Next(alwaysUp{})
	if err != nil || got != "http://a:1" {
		t.Fatalf("Next: got %q, %v", got, err)
	}
	if bs := sw.Backends(); len(bs) != 1 || bs[0] != "http://a:1" {
		t.Fatalf("Backends: got %v", bs)
	}
	sw.Done("http://a:1")
	if len(fb.dones) != 1 {
		t.Fatalf("Done must reach the inner balancer, got %v", fb.dones)
	}
}

// TestSwappable_NextForIPReachesTheIPHashBalancer is the regression guard for
// session affinity: proxy picks NextForIP only when the balancer implements
// balancer.IPHash, so swappable must forward that call rather than falling
// through to plain Next.
func TestSwappable_NextForIPReachesTheIPHashBalancer(t *testing.T) {
	entries := []balancer.BackendEntry{
		{URL: "http://a:1"}, {URL: "http://b:2"}, {URL: "http://c:3"},
	}
	iph, err := balancer.NewIPHash(entries)
	if err != nil {
		t.Fatalf("NewIPHash: %v", err)
	}
	sw := &swappable{}
	sw.cur.Store(&engine{bal: iph, backends: []string{"http://a:1", "http://b:2", "http://c:3"}})

	first, err := sw.NextForIP(alwaysUp{}, "203.0.113.7")
	if err != nil {
		t.Fatalf("NextForIP: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := sw.NextForIP(alwaysUp{}, "203.0.113.7")
		if err != nil {
			t.Fatalf("NextForIP: %v", err)
		}
		if again != first {
			t.Fatalf("the same client must stay pinned: %q then %q", first, again)
		}
	}
}

func TestSwappable_RecordFailureDrivesTheBreaker(t *testing.T) {
	reg := health.NewRegistry([]string{"http://a:1"}, 2, time.Minute, nil)
	sw := &swappable{}
	sw.cur.Store(&engine{breaker: reg, health: engineHealthChecker{breaker: reg}})

	if !sw.IsAvailable("http://a:1") {
		t.Fatal("a fresh breaker must be closed")
	}
	sw.RecordFailure("http://a:1")
	sw.RecordFailure("http://a:1") // threshold reached
	if sw.IsAvailable("http://a:1") {
		t.Fatal("the breaker must be open after reaching the failure threshold")
	}
	sw.RecordSuccess("http://a:1") // ignored while open; closing happens via half-open
}

func TestSwappable_RecordFailureWithNilBreakerIsSafe(t *testing.T) {
	sw := &swappable{}
	sw.cur.Store(&engine{})
	sw.RecordFailure("http://a:1")
	sw.RecordSuccess("http://a:1")
}

// ---------- engineHealthChecker ----------

func TestEngineHealthChecker_LocalHealthBlocksTraffic(t *testing.T) {
	setHealth("http://sick:1", false)
	defer setHealth("http://sick:1", true)

	c := engineHealthChecker{}
	if c.IsAvailable("http://sick:1") {
		t.Fatal("a locally unhealthy backend must not be available")
	}
	if !c.IsAvailable("http://never-seen:1") {
		t.Fatal("an unknown backend must default to available")
	}
}

// TestEngineHealthChecker_PeerHealthBlocksTraffic covers the cluster fan-in:
// a peer marking a backend unhealthy must stop this node routing to it, even
// though this node's own probes are happy.
func TestEngineHealthChecker_PeerHealthBlocksTraffic(t *testing.T) {
	node := cluster.NewNode("node-a", "127.0.0.1:7946", nil, 5)
	node.SetHealth("http://remote-sick:1", false)

	c := engineHealthChecker{node: node}
	if c.IsAvailable("http://remote-sick:1") {
		t.Fatal("a backend a peer reported unhealthy must not be available")
	}

	healthy := cluster.NewNode("node-b", "127.0.0.1:7947", nil, 5)
	healthy.SetHealth("http://remote-ok:1", true)
	c2 := engineHealthChecker{node: healthy}
	if !c2.IsAvailable("http://remote-ok:1") {
		t.Fatal("a peer reporting healthy must not block traffic")
	}
}

// ---------- rebuild ----------

func TestRuntime_RebuildPublishesGeneration(t *testing.T) {
	cfg := baseConfig("http://a:1", "http://b:2")
	rt := testRuntime(t, cfg, nil)
	rt.rebuild(cfg)

	e := rt.sw.load()
	if e == nil {
		t.Fatal("rebuild must publish an engine")
	}
	if len(e.backends) != 2 {
		t.Fatalf("expected 2 backends, got %v", e.backends)
	}
	if got := getBackends(); len(got) != 2 {
		t.Fatalf("/readyz pool must track the rebuild, got %v", got)
	}
	if e.breaker == nil {
		t.Fatal("a rebuild must install a breaker registry")
	}
}

// TestRuntime_RebuildKeepsServingOnFailure pins the failure behaviour: a bad
// strategy must leave the previous generation intact rather than publish a
// runtime with no backend pool at all.
func TestRuntime_RebuildKeepsServingOnFailure(t *testing.T) {
	good := baseConfig("http://a:1")
	rt := testRuntime(t, good, nil)
	rt.rebuild(good)
	before := rt.sw.load()

	bad := good
	bad.Balancer.Strategy = "not_a_strategy"
	rt.rebuild(bad)

	if rt.sw.load() != before {
		t.Fatal("a failed rebuild must not replace the serving generation")
	}
	if got := getBackends(); len(got) != 1 || got[0] != "http://a:1" {
		t.Fatalf("the existing pool must be preserved, got %v", got)
	}
}

func TestRuntime_RebuildWithEmptyPoolKeepsPrevious(t *testing.T) {
	good := baseConfig("http://a:1")
	rt := testRuntime(t, good, nil)
	rt.rebuild(good)
	before := rt.sw.load()

	rt.rebuild(baseConfig()) // no backends at all

	if rt.sw.load() != before {
		t.Fatal("an empty pool must not replace the serving generation")
	}
}

// TestRuntime_RebuildCarriesBreakerState: without carry-over, a reload hands a
// tripped backend a fresh closed breaker and immediately floods it again.
func TestRuntime_RebuildCarriesBreakerState(t *testing.T) {
	cfg := baseConfig("http://a:1", "http://b:2")
	rt := testRuntime(t, cfg, nil)
	rt.rebuild(cfg)

	// Trip http://a:1 (failure_threshold is 2 in baseConfig).
	e1 := rt.sw.load()
	e1.breaker.RecordFailure("http://a:1")
	e1.breaker.RecordFailure("http://a:1")
	if e1.breaker.BreakerState("http://a:1") != health.StateOpen {
		t.Fatal("precondition: the breaker should be open")
	}

	rt.rebuild(cfg)

	e2 := rt.sw.load()
	if e2 == e1 {
		t.Fatal("rebuild should have produced a new generation")
	}
	if got := e2.breaker.BreakerState("http://a:1"); got != health.StateOpen {
		t.Fatalf("breaker state must survive a rebuild, got %s", got)
	}
	if got := e2.breaker.BreakerState("http://b:2"); got != health.StateClosed {
		t.Fatalf("an untouched backend must stay closed, got %s", got)
	}
}

func TestRuntime_RebuildCarriesConnsForLeastConnections(t *testing.T) {
	cfg := baseConfig("http://a:1", "http://b:2")
	cfg.Balancer.Strategy = "least_connections"
	rt := testRuntime(t, cfg, nil)
	rt.rebuild(cfg)

	// Next increments the chosen backend's active count.
	picked, err := rt.sw.Next(alwaysUp{})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	before := rt.sw.load().bal.(balancer.ConnsReporter).ActiveConns()[picked]
	if before != 1 {
		t.Fatalf("precondition: %s should hold 1 active connection, got %d", picked, before)
	}

	rt.rebuild(cfg)

	after := rt.sw.load().bal.(balancer.ConnsReporter).ActiveConns()[picked]
	if after != before {
		t.Fatalf("active connections must survive a rebuild: %d then %d", before, after)
	}
}

// ---------- discovery ----------

func TestRuntime_OnDiscoveryRebuildsPool(t *testing.T) {
	cfg := baseConfig("http://a:1")
	rt := testRuntime(t, cfg, nil)
	rt.rebuild(cfg)

	rt.OnDiscovery([]string{"http://a:1", "http://discovered:80"})

	e := rt.sw.load()
	if len(e.backends) != 2 {
		t.Fatalf("a discovered backend must enter the pool, got %v", e.backends)
	}
	if got := getBackends(); len(got) != 2 {
		t.Fatalf("the global pool must follow discovery, got %v", got)
	}
}

func TestRuntime_OnDiscoveryUnchangedIsANoop(t *testing.T) {
	cfg := baseConfig("http://a:1")
	rt := testRuntime(t, cfg, nil)
	rt.OnDiscovery([]string{"http://a:1", "http://extra:80"})
	rt.rebuild(cfg)
	before := rt.sw.load()

	rt.OnDiscovery([]string{"http://a:1", "http://extra:80"}) // same list

	if rt.sw.load() != before {
		t.Fatal("an unchanged discovery result must not trigger a rebuild")
	}
}

func TestRuntime_OnDiscoveryPrunesHealthForGoneBackends(t *testing.T) {
	cfg := baseConfig()
	rt := testRuntime(t, cfg, nil)

	setHealth("http://gone:1", false)
	rt.OnDiscovery([]string{"http://stays:80"})

	healthMutex.RLock()
	_, stillKnown := backendHealth["http://gone:1"]
	healthMutex.RUnlock()
	if stillKnown {
		t.Fatal("health state for a backend no longer in the pool must be dropped")
	}
	setHealth("http://stays:80", true)
}

// ---------- scheduler ----------

func TestRuntime_StartSchedulerOnlyWhenHealthCheckEnabled(t *testing.T) {
	cfg := baseConfig("http://a:1")
	rt := testRuntime(t, cfg, nil)
	rt.rebuild(cfg)

	if rt.schedStop != nil {
		t.Fatal("no scheduler should run while health.enabled is false")
	}

	cfg.Health = config.HealthConfig{Enabled: true, IntervalSec: 60, TimeoutSec: 1, Path: "/"}
	rt.rebuild(cfg)
	if rt.schedStop == nil {
		t.Fatal("enabling health checks must start a scheduler")
	}

	rt.stopScheduler()
	if rt.schedStop != nil {
		t.Fatal("stopScheduler must clear the cancel func")
	}
	rt.stopScheduler() // idempotent
}

// TestRuntime_RebuildRestartsSchedulerForNewBackends: a discovered backend must
// get a probe loop immediately instead of waiting for a process restart.
func TestRuntime_RebuildRestartsSchedulerForNewBackends(t *testing.T) {
	cfg := baseConfig("http://a:1")
	cfg.Health = config.HealthConfig{Enabled: true, IntervalSec: 60, TimeoutSec: 1, Path: "/"}
	rt := testRuntime(t, cfg, nil)
	rt.rebuild(cfg)
	first := rt.schedStop
	if first == nil {
		t.Fatal("precondition: a scheduler should be running")
	}

	rt.rebuild(cfg)
	if rt.schedStop == nil {
		t.Fatal("the scheduler must be running after a rebuild")
	}
	rt.stopScheduler()
}

// ---------- stateWriter ----------

// TestRuntime_StateWriterFansHealthOutToCluster covers the gossip fan-out: a
// locally observed health change must be published so peers skip the backend too.
func TestRuntime_StateWriterFansHealthOutToCluster(t *testing.T) {
	node := cluster.NewNode("node-a", "127.0.0.1:7946", nil, 5)
	cfg := baseConfig("http://a:1")
	rt := testRuntime(t, cfg, node)

	rt.stateWriter()("http://a:1", false)
	defer setHealth("http://a:1", true)

	if isHealthy("http://a:1") {
		t.Fatal("the local health map must record the change")
	}
	if node.GetHealth("http://a:1", true) {
		t.Fatal("the change must be published to the cluster node for gossip")
	}
}

func TestRuntime_StateWriterWithoutCluster(t *testing.T) {
	cfg := baseConfig("http://a:1")
	rt := testRuntime(t, cfg, nil)

	rt.stateWriter()("http://a:1", false)
	defer setHealth("http://a:1", true)

	if isHealthy("http://a:1") {
		t.Fatal("the local health map must still record the change with clustering off")
	}
}

// ---------- cluster metrics ----------

func TestRuntime_StartClusterMetricsWithoutNodeIsANoop(t *testing.T) {
	cfg := baseConfig("http://a:1")
	rt := testRuntime(t, cfg, nil)
	rt.StartClusterMetrics() // must not panic
}

func TestRuntime_StartClusterMetricsPublishesGauges(t *testing.T) {
	node := cluster.NewNode("node-a", "127.0.0.1:7946", []string{"127.0.0.1:7947"}, 1)
	cfg := baseConfig("http://a:1")
	reg := metrics.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := newRuntime(ctx, "haribon-config.yml", config.NewSnapshot(cfg), reg, node)
	rt.StartClusterMetrics()

	// Registry.Gauge creates on demand, so poll the value rather than existence.
	// One tick of the gossip interval is enough for the gauges to be set.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Gauge("haribon_cluster_peers").Load() == 1 {
			if reg.Gauge("haribon_cluster_term").Load() == 0 {
				t.Fatal("haribon_cluster_term should also be published")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("haribon_cluster_peers was never published")
}
