// Package health provides liveness/readiness HTTP handlers and an active
// health-check scheduler that continuously probes backends.
//
// (See handler.go for the HTTP handlers.)
//
// Problem:  Passive health only marks a backend unhealthy after a request
//
//	fails; dead backends stay in rotation until a real user hits them.
//
// Options:  (a) external sidecar, (b) per-request probe, (c) background goroutine.
// Choice:   (c) — lightweight goroutine per-backend; stdlib http.Client; results
//
//	written via the StateWriter callback (decoupled from globals).
//
// Failure:  Probe timeout or non-2xx → consecutive-failure counter incremented;
//
//	backend marked unhealthy at unhealthy_threshold; re-marked healthy at
//	healthy_threshold consecutive successes.
//
// Observability: every state transition (healthy→unhealthy, unhealthy→healthy)
//
//	emits a structured log line via the Logger callback.
package health

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// StateWriter is called by the scheduler when a backend's health state changes.
// Implementations must be goroutine-safe.
type StateWriter func(backend string, healthy bool)

// Logger is called to emit a structured transition log line.
type Logger func(backend, state, reason string)

// SchedulerConfig holds tunable parameters for the health scheduler.
type SchedulerConfig struct {
	IntervalSec        int
	TimeoutSec         int
	Path               string
	HealthyThreshold   int
	UnhealthyThreshold int
}

// Scheduler runs per-backend health probes until its context is cancelled.
type Scheduler struct {
	backends []string
	cfg      SchedulerConfig
	write    StateWriter
	log      Logger
	client   *http.Client
}

// NewScheduler constructs a Scheduler.
// client may be nil — a default 2s-timeout client is used.
func NewScheduler(
	backends []string,
	cfg SchedulerConfig,
	write StateWriter,
	logger Logger,
	client *http.Client,
) *Scheduler {
	if client == nil {
		client = &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}
	}
	return &Scheduler{
		backends: backends,
		cfg:      cfg,
		write:    write,
		log:      logger,
		client:   client,
	}
}

// Run starts one goroutine per backend. It blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for _, b := range s.backends {
		go s.probe(ctx, b)
	}
	<-ctx.Done()
}

// probe polls a single backend until ctx is cancelled.
func (s *Scheduler) probe(ctx context.Context, backend string) {
	ticker := time.NewTicker(time.Duration(s.cfg.IntervalSec) * time.Second)
	defer ticker.Stop()

	path := s.cfg.Path
	if path == "" {
		path = "/"
	}

	consecutiveOK := 0
	consecutiveFail := 0
	currentHealthy := true // optimistic start

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok := s.check(backend, path)
			if ok {
				consecutiveOK++
				consecutiveFail = 0
				if !currentHealthy && consecutiveOK >= s.cfg.HealthyThreshold {
					currentHealthy = true
					s.write(backend, true)
					s.log(backend, "healthy", fmt.Sprintf("%d consecutive successes", consecutiveOK))
				}
			} else {
				consecutiveFail++
				consecutiveOK = 0
				if currentHealthy && consecutiveFail >= s.cfg.UnhealthyThreshold {
					currentHealthy = false
					s.write(backend, false)
					s.log(backend, "unhealthy", fmt.Sprintf("%d consecutive failures", consecutiveFail))
				}
			}
		}
	}
}

// check performs a single HTTP GET probe and returns true on 2xx.
func (s *Scheduler) check(backend, path string) bool {
	target := backend + path
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.cfg.TimeoutSec)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
