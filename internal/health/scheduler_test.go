package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/marcuwynu23/haribon/internal/health"
)

func TestScheduler_MarksUnhealthyOnFailure(t *testing.T) {
	// A server that always returns 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var mu sync.Mutex
	states := map[string]bool{srv.URL: true}

	write := func(backend string, healthy bool) {
		mu.Lock()
		defer mu.Unlock()
		states[backend] = healthy
	}
	logger := func(_, _, _ string) {}

	cfg := health.SchedulerConfig{
		IntervalSec:        1,
		TimeoutSec:         1,
		Path:               "/",
		HealthyThreshold:   1,
		UnhealthyThreshold: 2,
	}

	ctx, cancel := ctxWithTimeout(t, 4*time.Second)
	defer cancel()

	sched := health.NewScheduler([]string{srv.URL}, cfg, write, logger, nil)
	go sched.Run(ctx)

	// Wait for at least 2 probe intervals + margin
	time.Sleep(3 * time.Second)
	cancel()

	mu.Lock()
	healthy := states[srv.URL]
	mu.Unlock()

	if healthy {
		t.Fatal("backend should be marked unhealthy after consecutive failures")
	}
}

func TestScheduler_MarksHealthyOnRecovery(t *testing.T) {
	// Flip from unhealthy to healthy after first request
	failCount := 0
	var once sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Lock()
		fc := failCount
		failCount++
		once.Unlock()
		if fc < 2 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	states := map[string]bool{srv.URL: true}
	var logMsgs []string

	write := func(backend string, healthy bool) {
		mu.Lock()
		defer mu.Unlock()
		states[backend] = healthy
	}
	logger := func(backend, state, reason string) {
		mu.Lock()
		defer mu.Unlock()
		logMsgs = append(logMsgs, state)
	}

	cfg := health.SchedulerConfig{
		IntervalSec:        1,
		TimeoutSec:         1,
		Path:               "/",
		HealthyThreshold:   1,
		UnhealthyThreshold: 2,
	}

	ctx, cancel := ctxWithTimeout(t, 6*time.Second)
	defer cancel()

	sched := health.NewScheduler([]string{srv.URL}, cfg, write, logger, nil)
	go sched.Run(ctx)

	time.Sleep(5 * time.Second)
	cancel()

	mu.Lock()
	msgs := make([]string, len(logMsgs))
	copy(msgs, logMsgs)
	healthy := states[srv.URL]
	mu.Unlock()

	if !healthy {
		t.Fatal("backend should recover to healthy")
	}
	// Should have seen at least one "unhealthy" and one "healthy" log
	sawUnhealthy, sawHealthy := false, false
	for _, m := range msgs {
		if m == "unhealthy" {
			sawUnhealthy = true
		}
		if m == "healthy" {
			sawHealthy = true
		}
	}
	if !sawUnhealthy {
		t.Fatal("expected at least one unhealthy transition log")
	}
	if !sawHealthy {
		t.Fatal("expected at least one healthy transition log")
	}
}

func ctxWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}
