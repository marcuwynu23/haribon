# Circuit Breaker & Retry Policy — Haribon

---

## Retry Policy

### Configuration

```yaml
retry:
  max_retries: 1  # additional attempts beyond the first
```

### Behaviour

| Condition | Retried? |
|-----------|----------|
| Network error (connection refused, timeout) | Yes — if method is idempotent |
| HTTP 502 / 503 / 504 response | Yes — if method is idempotent |
| HTTP 500 or other 4xx/5xx | No — forwarded to client as-is |
| POST / PATCH / CONNECT | Never — non-idempotent, single attempt only |
| GET / HEAD / PUT / DELETE / OPTIONS | Yes — up to `max_retries` extra attempts |

### Retry Loop

1. First attempt: pick next backend from the balancer.
2. On failure: sleep 25 ms, pick the next healthy backend, retry.
3. After `max_retries + 1` total attempts all fail → `503 All backend servers failed`.
4. Every retry increments `haribon_retries_total{backend}` in `/metrics`.
5. The response header `X-Haribon-Retries: N` is set when N > 0, so clients
   and upstream proxies can see how many retries occurred.

### Safety

POST and PATCH are **never retried** to prevent duplicate mutations (e.g.
creating two database records). If a POST fails, the 503 is returned
immediately after one attempt.

---

## Circuit Breaker

### Configuration

```yaml
breaker:
  failure_threshold: 5   # consecutive failures before opening
  cooldown_sec: 30       # seconds before attempting a half-open probe
```

### Three-State FSM

```
         N consecutive failures
Closed ────────────────────────► Open
  ▲                               │
  │    success in half-open       │  cooldown elapsed
  │                               ▼
  └──────────────────────── Half-Open
         failure → re-opens
```

| State | Behaviour |
|-------|-----------|
| **Closed** | Normal operation. Failures counted. |
| **Open** | All requests to this backend rejected instantly. No network calls made. |
| **Half-Open** | Exactly one trial request allowed through after cooldown. |

### State Transitions

- **Closed → Open**: `failure_threshold` consecutive failures (network error or 502/503/504).
- **Open → Half-Open**: `cooldown_sec` elapsed since opening; first `IsAvailable()` call transitions and returns `true`.
- **Half-Open → Closed**: Trial request succeeds (`RecordSuccess()`).
- **Half-Open → Open**: Trial request fails (`RecordFailure()`); fresh cooldown starts.

### Observability

Every state transition emits a log line:

```json
{"time":"…","method":"BREAKER","path":"circuit-breaker","backend":"http://…","status":0,"level":"warn"}
```

The `/metrics` endpoint tracks:

```
haribon_breaker_state{backend="http://..."} 0   # 0=closed, 1=open, 2=half_open
haribon_retries_total{backend="http://..."} 12
haribon_errors_total{reason="all_backends_failed"} 3
```

### Interaction with Balancer

The `compositeHealthChecker` in `cli/main.go` combines:

1. Passive health map (updated by proxy on success/failure).
2. Circuit breaker `IsAvailable()`.

A backend is only routed to when **both** agree it is available. This means
a half-open circuit only allows one in-flight request at a time.

---

## Example Config (all features enabled)

```yaml
balancer:
  strategy: weighted_round_robin

health:
  enabled: true
  interval_sec: 10
  timeout_sec: 2
  path: /healthz
  healthy_threshold: 1
  unhealthy_threshold: 2

retry:
  max_retries: 1

breaker:
  failure_threshold: 5
  cooldown_sec: 30

backends:
  - url: "http://backend-1:8080"
    weight: 2
  - url: "http://backend-2:8080"
    weight: 1
```

---

## Testing

```bash
# Run all tests including breaker and retry
go test ./... -count=1 -v

# Specific packages
go test ./internal/health/... -run TestBreaker -v
go test ./internal/proxy/... -run TestProxy_Retry -v
```
