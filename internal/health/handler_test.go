package health_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marcuwynu23/haribon/internal/health"
)

// stubChecker implements health.HealthChecker for testing.
type stubChecker struct{ healthy bool }

func (s stubChecker) HasHealthyBackend() bool { return s.healthy }

// ---------- /healthz ----------

func TestHealthz_AlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	health.Healthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")
}

func TestHealthz_ContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	health.Healthz(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected application/json, got %s", ct)
	}
}

// ---------- /readyz ----------

func TestReadyz_HealthyBackend_Returns200(t *testing.T) {
	handler := health.Readyz(stubChecker{healthy: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")
}

func TestReadyz_NoHealthyBackend_Returns503(t *testing.T) {
	handler := health.Readyz(stubChecker{healthy: false})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	assertJSONStatus(t, rec.Body.Bytes(), "unavailable")
}

func TestReadyz_503_HasMessage(t *testing.T) {
	handler := health.Readyz(stubChecker{healthy: false})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler(rec, req)

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["message"] == "" {
		t.Fatal("503 response must include a message field")
	}
}

func TestReadyz_ContentType(t *testing.T) {
	handler := health.Readyz(stubChecker{healthy: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected application/json, got %s", ct)
	}
}

// ---------- helpers ----------

func assertJSONStatus(t *testing.T, body []byte, want string) {
	t.Helper()
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v | body: %s", err, body)
	}
	if resp["status"] != want {
		t.Fatalf("expected status=%q, got %q | body: %s", want, resp["status"], body)
	}
}
