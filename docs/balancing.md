# Load Balancing Algorithms — Haribon

Haribon supports three pluggable balancing strategies selectable via config.

---

## Configuration

```yaml
balancer:
  strategy: round_robin  # round_robin | weighted_round_robin | least_connections

backends:
  - url: "http://localhost:4441"
    weight: 2   # used by weighted_round_robin; ignored by others
  - url: "http://localhost:4442"
    weight: 1
```

---

## Strategies

### `round_robin` (default)

Requests cycle sequentially: A → B → C → A …

- Equal distribution across backends.
- Unhealthy/open-circuit backends are skipped; counter advances anyway so
  the next healthy backend is always tried.
- Concurrent-safe: single `atomic.Uint64` counter, no locks on the hot path.

Best for: homogeneous backends (same hardware, same capacity).

### `weighted_round_robin`

Backends with higher `weight` receive proportionally more requests.

A backend with `weight: 2` and a neighbour with `weight: 1` receives ≈ 2x the
traffic. Zero weight is treated as 1. The implementation expands each backend
into a slot list so the distribution is deterministic over any window of
`sum(weights)` requests.

Best for: heterogeneous backends where one server has more capacity.

```yaml
balancer:
  strategy: weighted_round_robin
backends:
  - url: "http://large-server:8080"
    weight: 3
  - url: "http://small-server:8080"
    weight: 1
```

### `least_connections`

Each incoming request is routed to the backend with the fewest in-flight
connections at that moment. An atomic per-backend counter is incremented on
`Next()` and decremented on `Done()` (called by the proxy after the response
is fully sent).

Best for: long-lived requests (streaming, uploads) where connections linger and
round-robin would pile up on a slow backend.

---

## Health Filtering

All three strategies call `HealthChecker.IsAvailable(backend)` before selecting
a backend. The checker combines:

1. **Passive health map** — updated by the proxy when a request fails or succeeds.
2. **Circuit breaker** — if the breaker for that backend is open, `IsAvailable`
   returns false regardless of the health map.

If no backend passes the filter, the balancer returns `ErrNoHealthyBackend` and
the proxy responds `503 All backend servers failed`.

---

## Active Health Scheduler

Enable background probes so dead backends are detected before real traffic hits:

```yaml
health:
  enabled: true
  interval_sec: 10       # probe every 10 seconds
  timeout_sec: 2         # probe timeout
  path: /                # path to probe (e.g. /healthz)
  healthy_threshold: 1   # consecutive successes to re-mark healthy
  unhealthy_threshold: 2 # consecutive failures to mark unhealthy
```

Each backend runs in its own goroutine; state is written via `setHealth()` which
updates the shared `backendHealth` map under `RWMutex`.

---

## Observability

Every backend health transition emits a structured log line:

```json
{"time":"…","method":"PROBE","path":"health-check","backend":"http://…","status":0,"level":"warn"}
```

The `/metrics` endpoint exposes:

```
haribon_backend_healthy{backend="http://..."} 1
haribon_requests_total{backend="http://..."} 42
haribon_last_duration_ms{backend="http://..."} 3
```
