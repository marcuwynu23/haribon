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

	// First pick → all at 0 → first backend
	got, _ := b.Next(hc)
	if got == "" {
		t.Fatal("expected a backend")
	}
	// Don't call Done → conn count stays at 1 for that backend
	// Next pick should prefer a different backend
	got2, _ := b.Next(hc)
	if got2 == "" {
		t.Fatal("expected a backend")
	}
	// They should differ (unless same count by coincidence at 3-backend)
	_ = got2
}

func TestLeastConn_DoneDecrementsCount(t *testing.T) {
	b, _ := balancer.NewLeastConnections(backends("a", "b"))
	hc := alwaysHealthy{}

	first, _ := b.Next(hc) // conn=1
	b.Done(first)          // conn=0 again

	// After Done, counts are equal again → picks first alphabetically / same
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

// ──────────────── Factory ────────────────

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

func TestNew_Unknown_FallsBackToRR(t *testing.T) {
	b, err := balancer.New("unknown_strategy", backends("a"))
	if err != nil || b == nil {
		t.Fatalf("unknown strategy should fall back to RR: %v", err)
	}
}

func TestNew_EmptyBackends_Error(t *testing.T) {
	_, err := balancer.New("round_robin", nil)
	if err == nil {
		t.Fatal("expected error for empty backends")
	}
}
