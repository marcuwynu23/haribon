package metrics_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcuwynu23/haribon/internal/metrics"
)

func TestCounter_IncAndLoad(t *testing.T) {
	reg := metrics.New()
	c := reg.Counter("test_requests_total")
	c.Inc()
	c.Inc()
	if c.Load() != 2 {
		t.Fatalf("expected 2, got %d", c.Load())
	}
}

func TestCounter_Add(t *testing.T) {
	reg := metrics.New()
	c := reg.Counter("test_add")
	c.Add(5)
	if c.Load() != 5 {
		t.Fatalf("expected 5, got %d", c.Load())
	}
}

func TestGauge_SetAndLoad(t *testing.T) {
	reg := metrics.New()
	g := reg.Gauge("test_gauge")
	g.Set(42)
	if g.Load() != 42 {
		t.Fatalf("expected 42, got %d", g.Load())
	}
}

func TestGauge_IncDec(t *testing.T) {
	reg := metrics.New()
	g := reg.Gauge("test_incdec")
	g.Inc()
	g.Inc()
	g.Dec()
	if g.Load() != 1 {
		t.Fatalf("expected 1, got %d", g.Load())
	}
}

func TestRegistry_SameNameReturnsSameInstance(t *testing.T) {
	reg := metrics.New()
	c1 := reg.Counter("requests")
	c2 := reg.Counter("requests")
	c1.Inc()
	if c2.Load() != 1 {
		t.Fatal("same name should return same Counter instance")
	}
}

func TestHandler_Returns200WithPrometheusContent(t *testing.T) {
	reg := metrics.New()
	reg.Counter("haribon_requests_total").Add(7)
	reg.Gauge("haribon_backend_healthy").Set(1)

	handler := reg.Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "haribon_requests_total") {
		t.Fatalf("missing counter in output: %s", body)
	}
	if !strings.Contains(body, "haribon_backend_healthy") {
		t.Fatalf("missing gauge in output: %s", body)
	}
}

func TestHandler_ContentType(t *testing.T) {
	reg := metrics.New()
	handler := reg.Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", ct)
	}
}

func TestMetricName(t *testing.T) {
	got := metrics.MetricName("haribon_requests_total", "backend", "http://localhost:4441")
	want := `haribon_requests_total{backend="http://localhost:4441"}`
	if got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}
