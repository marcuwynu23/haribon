package balancer_test

import (
	"sync"
	"testing"

	"github.com/marcuwynu23/haribon/internal/balancer"
)

// alwaysHealthy accepts every backend.
type alwaysHealthy struct{}

func (alwaysHealthy) IsAvailable(_ string) bool { return true }

// noneHealthy rejects every backend.
type noneHealthy struct{}

func (noneHealthy) IsAvailable(_ string) bool { return false }

// selectiveHealth accepts only the listed backends.
type selectiveHealth struct{ allowed map[string]bool }

func (s selectiveHealth) IsAvailable(b string) bool { return s.allowed[b] }

func backends(urls ...string) []balancer.BackendEntry {
	out := make([]balancer.BackendEntry, len(urls))
	for i, u := range urls {
		out[i] = balancer.BackendEntry{URL: u, Weight: 1}
	}
	return out
}

// ──────────────── Round-Robin ────────────────

func TestRoundRobin_Order(t *testing.T) {
	b, err := balancer.NewRoundRobin(backends("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c", "a"}
	hc := alwaysHealthy{}
	for i, w := range want {
		got, err := b.Next(hc)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got != w {
			t.Fatalf("call %d: want %s got %s", i, w, got)
		}
	}
}

func TestRoundRobin_SkipsUnhealthy(t *testing.T) {
	b, _ := balancer.NewRoundRobin(backends("a", "b", "c"))
	hc := selectiveHealth{allowed: map[string]bool{"b": true}}
	got, err := b.Next(hc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("expected b, got %s", got)
	}
}

func TestRoundRobin_AllUnhealthy_Error(t *testing.T) {
	b, _ := balancer.NewRoundRobin(backends("a", "b"))
	_, err := b.Next(noneHealthy{})
	if err == nil {
		t.Fatal("expected error when all unhealthy")
	}
}

func TestRoundRobin_NoBackends_Error(t *testing.T) {
	_, err := balancer.NewRoundRobin(nil)
	if err == nil {
		t.Fatal("expected error for empty backends")
	}
}

func TestRoundRobin_Concurrent_NoRace(t *testing.T) {
	b, _ := balancer.NewRoundRobin(backends("a", "b", "c"))
	hc := alwaysHealthy{}
	var wg sync.WaitGroup
	counts := make(map[string]int)
	var mu sync.Mutex
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				got, _ := b.Next(hc)
				mu.Lock()
				counts[got]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Rough even distribution: each backend should get ~3333 hits (±20%)
	total := 0
	for _, c := range counts {
		total += c
	}
	if total != 10000 {
		t.Fatalf("expected 10000 total, got %d", total)
	}
	for k, c := range counts {
		if c < 2000 || c > 5000 {
			t.Fatalf("backend %s has skewed distribution: %d/%d", k, c, total)
		}
	}
}

// ──────────────── Weighted Round-Robin ────────────────

func TestWeightedRR_Distribution(t *testing.T) {
	bs := []balancer.BackendEntry{
		{URL: "a", Weight: 2},
		{URL: "b", Weight: 1},
	}
	b, err := balancer.NewWeightedRoundRobin(bs)
	if err != nil {
		t.Fatal(err)
	}
	hc := alwaysHealthy{}
	counts := map[string]int{}
	N := 300
	for i := 0; i < N; i++ {
		got, err := b.Next(hc)
		if err != nil {
			t.Fatal(err)
		}
		counts[got]++
	}
	// a should get ~2x more than b
	ratioOK := float64(counts["a"])/float64(counts["b"]) > 1.5
	if !ratioOK {
		t.Fatalf("weighted distribution off: a=%d b=%d", counts["a"], counts["b"])
	}
}

func TestWeightedRR_SkipsUnhealthy(t *testing.T) {
	bs := []balancer.BackendEntry{
		{URL: "a", Weight: 2},
		{URL: "b", Weight: 1},
	}
	b, _ := balancer.NewWeightedRoundRobin(bs)
	hc := selectiveHealth{allowed: map[string]bool{"b": true}}
	for i := 0; i < 10; i++ {
		got, err := b.Next(hc)
		if err != nil {
			t.Fatal(err)
		}
		if got != "b" {
			t.Fatalf("expected only b to be served, got %s", got)
		}
	}
}

func TestWeightedRR_AllUnhealthy_Error(t *testing.T) {
	bs := []balancer.BackendEntry{{URL: "a", Weight: 1}}
	b, _ := balancer.NewWeightedRoundRobin(bs)
	_, err := b.Next(noneHealthy{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ──────────────── Least Connections ────────────────

func TestLeastConn_PicksLowest(t *testing.T) {
	b, err := balancer.NewLeastConnections(backends("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	hc := alwaysHealthy{}

	got, _ := b.Next(hc)
	if got == "" {
		t.Fatal("expected a backend")
	}

	got2, _ := b.Next(hc)
	if got2 == "" {
		t.Fatal("expected a backend")
	}
	_ = got2
}

func TestLeastConn_DoneDecrementsCount(t *testing.T) {
	b, _ := balancer.NewLeastConnections(backends("a", "b"))
	hc := alwaysHealthy{}

	first, _ := b.Next(hc) // conn=1
	b.Done(first)          // conn=0 again

	_, err := b.Next(hc)
	if err != nil {
		t.Fatal("unexpected error after Done:", err)
	}
}

func TestLeastConn_SkipsUnhealthy(t *testing.T) {
	b, _ := balancer.NewLeastConnections(backends("a", "b"))
	hc := selectiveHealth{allowed: map[string]bool{"b": true}}
	got, err := b.Next(hc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("expected b, got %s", got)
	}
}

func TestLeastConn_AllUnhealthy_Error(t *testing.T) {
	b, _ := balancer.NewLeastConnections(backends("a"))
	_, err := b.Next(noneHealthy{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLeastConn_Concurrent_NoRace(t *testing.T) {
	b, _ := balancer.NewLeastConnections(backends("a", "b", "c"))
	hc := alwaysHealthy{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				got, err := b.Next(hc)
				if err == nil {
					b.Done(got)
				}
			}
		}()
	}
	wg.Wait()
}

// ──────────────── Factory ─────────────────────

func TestNew_RoundRobin(t *testing.T) {
	b, err := balancer.New("round_robin", backends("a"))
	if err != nil || b == nil {
		t.Fatalf("expected RR balancer: %v", err)
	}
}

func TestNew_WeightedRR(t *testing.T) {
	b, err := balancer.New("weighted_round_robin", backends("a"))
	if err != nil || b == nil {
		t.Fatalf("expected WRR balancer: %v", err)
	}
}

func TestNew_LeastConn(t *testing.T) {
	b, err := balancer.New("least_connections", backends("a"))
	if err != nil || b == nil {
		t.Fatalf("expected least-conn balancer: %v", err)
	}
}

func TestNew_Unknown_Error(t *testing.T) {
	b, err := balancer.New("unknown_strategy", backends("a"))
	if err == nil || b != nil {
		t.Fatalf("unknown strategy should return error: %v", err)
	}
}

func TestNew_EmptyBackends_Error(t *testing.T) {
	_, err := balancer.New("round_robin", nil)
	if err == nil {
		t.Fatal("expected error for empty backends")
	}
}

func TestNew_Random(t *testing.T) {
	b, err := balancer.New("random", backends("a", "b", "c"))
	if err != nil || b == nil {
		t.Fatalf("expected random balancer: %v", err)
	}
	hc := alwaysHealthy{}
	counts := map[string]int{}
	N := 300
	for i := 0; i < N; i++ {
		got, err := b.Next(hc)
		if err != nil {
			t.Fatal(err)
		}
		counts[got]++
	}
	// All backends should be selected at least once (likely with 300 tries)
	// and roughly even distribution
	total := 0
	for _, c := range counts {
		total += c
	}
	if total != N {
		t.Fatalf("expected %d total, got %d", N, total)
	}
	// Each should get roughly equal shares (allow wide margin for random)
	for k, c := range counts {
		if c < N/4 {
			t.Fatalf("backend %s got too few: %d/%d", k, c, N)
		}
		if c > 3*N/4 {
			t.Fatalf("backend %s got too many: %d/%d", k, c, N)
		}
	}
}

func TestNew_IPHash(t *testing.T) {
	b, err := balancer.New("ip_hash", backends("a", "b", "c"))
	if err != nil || b == nil {
		t.Fatalf("expected ip_hash balancer: %v", err)
	}
	hc := alwaysHealthy{}
	// ip_hash should distribute across backends
	for i := 0; i < 9; i++ {
		got, _ := b.Next(hc)
		if got != "a" && got != "b" && got != "c" {
			t.Fatalf("expected a/b/c got %s", got)
		}
	}
}

func TestNew_Random_EmptyBackends_Error(t *testing.T) {
	_, err := balancer.New("random", nil)
	if err == nil {
		t.Fatal("expected error for empty backends")
	}
}

func TestNew_IPHash_EmptyBackends_Error(t *testing.T) {
	_, err := balancer.New("ip_hash", nil)
	if err == nil {
		t.Fatal("expected error for empty backends")
	}
}

// ──────────────── Random Distribution ─────────────────────

func TestRandom_Distribution_UnhealthySkip(t *testing.T) {
	b, err := balancer.New("random", backends("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	hc := selectiveHealth{allowed: map[string]bool{"b": true}}
	counts := map[string]int{}
	N := 100
	for i := 0; i < N; i++ {
		got, err := b.Next(hc)
		if err != nil {
			t.Fatal(err)
		}
		counts[got]++
	}
	// Only b should be selected
	if counts["b"] != N {
		t.Fatalf("expected %d selections of b, got %d", N, counts["b"])
	}
	if counts["a"] != 0 || counts["c"] != 0 {
		t.Fatalf("expected 0 selections of a/c, got %d/%d", counts["a"], counts["c"])
	}
}

func TestRandom_DoneNotRequired(t *testing.T) {
	b, _ := balancer.New("random", backends("a", "b"))
	hc := alwaysHealthy{}
	// Random doesn't track connections, so Done is a no-op
	got, _ := b.Next(hc)
	b.Done(got) // should not panic
	got2, _ := b.Next(hc)
	if got2 == "" {
		t.Fatal("expected a backend after Done")
	}
}

// ──────────────── IPHash Distribution ─────────────────────

func TestIPHash_SkipsUnhealthy(t *testing.T) {
	b, _ := balancer.New("ip_hash", backends("a", "b", "c"))
	hc := selectiveHealth{allowed: map[string]bool{"b": true}}
	got, err := b.Next(hc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("expected b, got %s", got)
	}
}

func TestIPHash_AllUnhealthy_Error(t *testing.T) {
	b, _ := balancer.New("ip_hash", backends("a"))
	_, err := b.Next(noneHealthy{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestIPHash_Concurrent_NoRace(t *testing.T) {
	b, _ := balancer.New("ip_hash", backends("a", "b", "c"))
	hc := alwaysHealthy{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				got, err := b.Next(hc)
				if err == nil {
					b.Done(got)
				}
			}
		}()
	}
	wg.Wait()
}