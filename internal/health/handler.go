// Package health provides HTTP liveness and readiness probe handlers.
//
// Problem:  Kubernetes kubelet needs /healthz (liveness) and /readyz (readiness)
//
//	to decide whether to restart a pod or route traffic to it.
//
// Options:  (a) embed in main mux, (b) separate internal mux, (c) this package.
// Choice:   (c) — thin pure handlers that take a HealthChecker interface so they
//
//	can be unit-tested without starting a real server.
//
// Failure:  /readyz returns 503 + JSON when no healthy backend exists; callers
//
//	must not retry — the scheduler will re-probe.
//
// Observability: probe hits are tagged internal:true so access log aggregation
//
//	can filter them out if desired.
package health

import (
	"encoding/json"
	"net/http"
)

// HealthChecker is the minimal interface the health handlers need from the
// balancer layer. The main package's backendHealth map satisfies this via an
// adapter.
type HealthChecker interface {
	// HasHealthyBackend returns true when at least one registered backend
	// is currently healthy.
	HasHealthyBackend() bool
}

// readyzResponse is the JSON body returned by /readyz.
type readyzResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Healthz handles GET /healthz — liveness probe.
// Always returns 200 OK; it only fails if the process itself is broken.
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(readyzResponse{Status: "ok"})
}

// Readyz handles GET /readyz — readiness probe.
// Returns 200 if at least one backend is healthy, 503 otherwise.
func Readyz(checker HealthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if checker.HasHealthyBackend() {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(readyzResponse{Status: "ok"})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(readyzResponse{
			Status:  "unavailable",
			Message: "no healthy backend available",
		})
	}
}
