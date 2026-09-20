# Retries and the Circuit Breaker

Two features that keep one sick server from spoiling everyone's requests.
Retries spend a second attempt on another server; the breaker stops sending
traffic to a server that keeps failing.

---

## Retries

```yaml
retry:
  max_retries: 1   # extra attempts after the first
```

### When a request is retried

| What happened | Retried? |
|---|---|
| The connection failed, timed out, or was refused | Yes, for methods that are safe to repeat |
| The server answered `502`, `503`, or `504` | Yes, for methods that are safe to repeat |
| The server answered `500` or any other `4xx`/`5xx` | No — passed straight back to the client |
| A `POST` or `PATCH` failed for any reason | No — never, under any setting |

`GET`, `HEAD`, `OPTIONS`, `PUT`, and `DELETE` are treated as safe to repeat.

**`POST` and `PATCH` are never retried.** Sending the same form twice can create
two records or charge a card twice, so a failed `POST` is reported as failed
after one attempt rather than risk a duplicate. This is not configurable.

### How the retry works

1. Pick a server and send the request.
2. If that fails in a retryable way, wait 25 ms, pick again — which skips the
   server that just failed — and send it there.
3. After `max_retries + 1` attempts have all failed, answer
   `503 All backend servers failed`.
4. Each retry is counted in `haribon_retries_total{backend}` against the server
   that failed.
5. If any retry happened, the response carries `X-Haribon-Retries: <n>`, so the
   client can tell a first-try success from a rescued one.

For a request that is safe to repeat, the body is held in memory so it can be
sent again. That means `max_retries` costs memory proportional to the request
size — worth knowing if you accept large uploads with `PUT`.

---

## Circuit breaker

```yaml
breaker:
  failure_threshold: 5   # failures in a row before the breaker opens
  cooldown_sec: 30       # how long to leave it open before testing again
```

A retry still costs a real connection and a real timeout. When a server is
thoroughly down, the breaker stops making those attempts at all.

### The three states

| State | What happens |
|---|---|
| **Closed** | Normal. The server takes traffic and its failures are counted. |
| **Open** | Every request to this server is refused immediately. No connection is made, so a request costs nothing to skip. |
| **Half-open** | After the cooldown, exactly one request is let through as a test. |

### Moving between them

- **Closed to open** — after `failure_threshold` failures in a row. A failure
  here means a network error or a `502`/`503`/`504`. Other responses reset the
  counter, including a `500`: the server answered, so it is reachable and the
  breaker is not the right tool for an application bug.
- **Open to half-open** — automatically, once `cooldown_sec` has passed.
- **Half-open to closed** — the trial request succeeded. The server is back in
  normal rotation.
- **Half-open to open** — the trial request failed. The breaker reopens and the
  cooldown starts over.

While the trial is in flight, other requests to that server are still refused,
so a recovering server gets one request to prove itself, not a flood.

Every transition is logged:

```json
{"time":"2026-05-04T01:31:55Z","method":"BREAKER","path":"circuit-breaker","backend":"http://localhost:4442","status":0,"level":"warn"}
```

### What the breaker is not

It is per Haribon process and in memory only. Restarting Haribon clears every
breaker, and two replicas keep their own breakers — see
[clustering.md](clustering.md).

---

## How it fits together

When Haribon is deciding where to send a request, a server has to pass all of
these:

1. **It is not known to be down** — from background probes if `health.enabled`,
   or from earlier requests if not.
2. **No replica has reported it down** — only when clustering is on.
3. **Its breaker is not open.**

If every server fails all three, the request is answered `503`. A server that is
skipped because of its breaker is not "tried and failed" — it is passed over
without a connection, which is the point.

---

## Metrics

```
haribon_breaker_state{backend="http://localhost:4442"} 0   # 0 closed, 1 open, 2 half-open
haribon_retries_total{backend="http://localhost:4442"} 12
haribon_errors_total{reason="all_backends_failed"} 3
```

`haribon_errors_total` counts requests that failed on every server — the ones a
client actually saw fail. The full list is in
[../README.md](../README.md#metrics).

---

## Full example

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

## Testing it yourself

```bash
go test ./internal/health/... -run TestBreaker -v
go test ./internal/proxy/... -run TestProxy_Retry -v
```

`pkill -STOP <backend pid>` is a quick way to make a server stop answering, so
you can watch the breaker open and the retries appear in `/metrics`.
