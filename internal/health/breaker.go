package health

// breaker.go — per-backend circuit breaker.
//
// Problem:  A flapping backend can absorb retries and inflate tail latency;
//           we need to stop sending traffic until the backend recovers.
// Options:  (a) Hystrix-style library, (b) minimal stdlib implementation.
// Choice:   (b) — three-state FSM (closed/open/half-open) in ~60 lines;
//           zero external deps; state stored per backend in a Registry.
// Failure:  Open circuit → IsAvailable returns false so the balancer skips it;
//           half-open lets exactly one probe through; failure in half-open
//           re-opens with a fresh cooldown.
// Observability: every transition logged via Logger; BreakerState() for /metrics.

import (
	"sync"
	"time"
)

// State represents a circuit-breaker state.
type State int

const (
	StateClosed   State = iota // normal operation
	StateOpen                  // tripping — rejecting all requests
	StateHalfOpen              // one trial request allowed
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// breaker is the per-backend circuit-breaker instance.
type breaker struct {
	mu               sync.Mutex
	state            State
	consecutiveFails int
	openAt           time.Time

	failureThreshold int
	cooldown         time.Duration
	logger           Logger
	backend          string
}

// Registry holds one circuit breaker per backend URL.
type Registry struct {
	mu       sync.RWMutex
	breakers map[string]*breaker
}

// NewRegistry creates a Registry.
// logger may be nil (transitions are silently swallowed).
func NewRegistry(backends []string, failureThreshold int, cooldown time.Duration, logger Logger) *Registry {
	r := &Registry{breakers: make(map[string]*breaker, len(backends))}
	for _, b := range backends {
		l := logger
		url := b
		r.breakers[url] = &breaker{
			failureThreshold: failureThreshold,
			cooldown:         cooldown,
			logger:           l,
			backend:          url,
		}
	}
	return r
}

// IsAvailable returns true when the circuit is closed or half-open (trial slot).
// A half-open circuit only allows the first caller through; subsequent callers
// are rejected until the trial completes via RecordSuccess/RecordFailure.
func (r *Registry) IsAvailable(backend string) bool {
	r.mu.RLock()
	br, ok := r.breakers[backend]
	r.mu.RUnlock()
	if !ok {
		return true // unknown backend → optimistic
	}

	br.mu.Lock()
	defer br.mu.Unlock()

	switch br.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(br.openAt) >= br.cooldown {
			br.transition(StateHalfOpen)
			return true // allow one trial
		}
		return false
	case StateHalfOpen:
		return false // already a trial in flight
	}
	return false
}

// RecordSuccess resets the failure counter; closes a half-open circuit.
func (r *Registry) RecordSuccess(backend string) {
	r.mu.RLock()
	br, ok := r.breakers[backend]
	r.mu.RUnlock()
	if !ok {
		return
	}

	br.mu.Lock()
	defer br.mu.Unlock()

	if br.state == StateHalfOpen {
		br.transition(StateClosed)
	}
	br.consecutiveFails = 0
}

// RecordFailure increments the failure counter; opens the circuit at threshold.
func (r *Registry) RecordFailure(backend string) {
	r.mu.RLock()
	br, ok := r.breakers[backend]
	r.mu.RUnlock()
	if !ok {
		return
	}

	br.mu.Lock()
	defer br.mu.Unlock()

	br.consecutiveFails++
	if br.state == StateHalfOpen || br.consecutiveFails >= br.failureThreshold {
		br.openAt = time.Now()
		br.transition(StateOpen)
		br.consecutiveFails = 0
	}
}

// BreakerState returns the current state for the given backend.
func (r *Registry) BreakerState(backend string) State {
	r.mu.RLock()
	br, ok := r.breakers[backend]
	r.mu.RUnlock()
	if !ok {
		return StateClosed
	}
	br.mu.Lock()
	defer br.mu.Unlock()
	return br.state
}

// CarryOver copies breaker state for every backend present in both registries.
//
// Problem:  a config reload or discovery refresh rebuilds the registry from
//
//	scratch; a backend sitting in an open circuit would silently get a
//	fresh, closed breaker and immediately absorb traffic again.
//
// Choice:   copy state/consecutiveFails/openAt for URLs present in both maps;
//
//	backends that disappeared keep no state (they are gone).
//
// Failure:  a backend removed and re-added within one reload loses its state —
//
//	acceptable, the breaker re-opens within failureThreshold requests.
func (r *Registry) CarryOver(old *Registry) {
	if old == nil || old == r {
		return
	}

	old.mu.RLock()
	defer old.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	for url, ob := range old.breakers {
		nb, ok := r.breakers[url]
		if !ok {
			continue
		}
		ob.mu.Lock()
		nb.state = ob.state
		nb.consecutiveFails = ob.consecutiveFails
		nb.openAt = ob.openAt
		ob.mu.Unlock()
	}
}

// transition changes state and fires the logger (must hold br.mu).
func (br *breaker) transition(to State) {
	from := br.state
	br.state = to
	if br.logger != nil && from != to {
		br.logger(br.backend, to.String(), "transition from "+from.String())
	}
}
