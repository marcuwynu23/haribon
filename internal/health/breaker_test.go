package health_test

import (
	"testing"
	"time"

	"github.com/marcuwynu23/haribon/internal/health"
)

func newRegistry(threshold int, cooldown time.Duration) *health.Registry {
	return health.NewRegistry([]string{"http://a", "http://b"}, threshold, cooldown, nil)
}

// ──────────────── Closed state ────────────────

func TestBreaker_InitiallyClosed(t *testing.T) {
	r := newRegistry(3, 30*time.Second)
	if !r.IsAvailable("http://a") {
		t.Fatal("new circuit should be closed (available)")
	}
}

// ──────────────── Open state ────────────────

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	r := newRegistry(3, 30*time.Second)
	for i := 0; i < 3; i++ {
		r.RecordFailure("http://a")
	}
	if r.IsAvailable("http://a") {
		t.Fatal("circuit should be open after threshold failures")
	}
	if r.BreakerState("http://a") != health.StateOpen {
		t.Fatal("expected StateOpen")
	}
}

func TestBreaker_OtherBackendUnaffected(t *testing.T) {
	r := newRegistry(3, 30*time.Second)
	for i := 0; i < 3; i++ {
		r.RecordFailure("http://a")
	}
	// http://b was not touched → still closed
	if !r.IsAvailable("http://b") {
		t.Fatal("unaffected backend should remain available")
	}
}

// ──────────────── Reset on success ────────────────

func TestBreaker_SuccessResetsCounter(t *testing.T) {
	r := newRegistry(3, 30*time.Second)
	r.RecordFailure("http://a")
	r.RecordFailure("http://a")
	r.RecordSuccess("http://a") // reset
	r.RecordFailure("http://a") // now at 1 again
	r.RecordFailure("http://a") // at 2 — below threshold
	if !r.IsAvailable("http://a") {
		t.Fatal("circuit should still be closed (counter reset)")
	}
}

// ──────────────── Half-open state ────────────────

func TestBreaker_TransitionsToHalfOpenAfterCooldown(t *testing.T) {
	r := newRegistry(2, 50*time.Millisecond) // short cooldown for test
	r.RecordFailure("http://a")
	r.RecordFailure("http://a") // opens

	if r.IsAvailable("http://a") {
		t.Fatal("should be open")
	}

	time.Sleep(60 * time.Millisecond) // wait for cooldown

	// Now IsAvailable should let one trial through (half-open)
	if !r.IsAvailable("http://a") {
		t.Fatal("should transition to half-open and allow trial")
	}
	if r.BreakerState("http://a") != health.StateHalfOpen {
		t.Fatal("expected StateHalfOpen")
	}
	// Second call while half-open should block
	if r.IsAvailable("http://a") {
		t.Fatal("half-open should reject second caller")
	}
}

func TestBreaker_HalfOpenSuccess_Closes(t *testing.T) {
	r := newRegistry(2, 50*time.Millisecond)
	r.RecordFailure("http://a")
	r.RecordFailure("http://a")
	time.Sleep(60 * time.Millisecond)
	r.IsAvailable("http://a") // trigger half-open

	r.RecordSuccess("http://a")

	if r.BreakerState("http://a") != health.StateClosed {
		t.Fatal("expected StateClosed after success in half-open")
	}
	if !r.IsAvailable("http://a") {
		t.Fatal("closed circuit should be available")
	}
}

func TestBreaker_HalfOpenFailure_ReOpens(t *testing.T) {
	r := newRegistry(2, 50*time.Millisecond)
	r.RecordFailure("http://a")
	r.RecordFailure("http://a")
	time.Sleep(60 * time.Millisecond)
	r.IsAvailable("http://a") // half-open

	r.RecordFailure("http://a") // fail in half-open → re-open

	if r.BreakerState("http://a") != health.StateOpen {
		t.Fatal("expected StateOpen after failure in half-open")
	}
}

// ──────────────── Unknown backend ────────────────

func TestBreaker_UnknownBackend_Available(t *testing.T) {
	r := newRegistry(3, 30*time.Second)
	if !r.IsAvailable("http://unknown") {
		t.Fatal("unknown backend should be optimistically available")
	}
}

// ──────────────── State string ────────────────

func TestBreakerState_String(t *testing.T) {
	tests := []struct {
		s    health.State
		want string
	}{
		{health.StateClosed, "closed"},
		{health.StateOpen, "open"},
		{health.StateHalfOpen, "half_open"},
	}
	for _, tt := range tests {
		if tt.s.String() != tt.want {
			t.Fatalf("want %q got %q", tt.want, tt.s.String())
		}
	}
}
