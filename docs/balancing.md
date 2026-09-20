# Choosing a Balancing Method

Haribon ships five ways to pick which server gets the next request. Set one in
the config file; no code changes, no restart of your servers.

```yaml
balancer:
  # round_robin | weighted_round_robin | least_connections | random | ip_hash
  strategy: round_robin

backends:
  - url: "http://localhost:4441"
    weight: 2   # only weighted_round_robin looks at this
  - url: "http://localhost:4442"
    weight: 1
```

An unknown `strategy` stops `start` and `check` with an error, so a typo cannot
reach production quietly.

Every method skips servers that are down or whose breaker is open, and answers
`503 All backend servers failed` only when none of them are left to try.

---

## `round_robin` (default)

Requests go to each server in turn: A, then B, then C, then back to A.

Best when your servers are all about the same size. It is the cheapest method —
one counter, no per-request bookkeeping.

A server that is skipped does not get a turn held open for it. The rotation moves
on, so the request goes to the next server that can take it.

## `weighted_round_robin`

Give a bigger machine a bigger share. A server with `weight: 2` next to one with
`weight: 1` takes about twice the traffic.

```yaml
balancer:
  strategy: weighted_round_robin
backends:
  - url: "http://large-server:8080"
    weight: 3
  - url: "http://small-server:8080"
    weight: 1
```

The split is exact rather than statistical: over any stretch of
`sum(weights)` requests, each server gets exactly its weight in requests.

A `weight` that is omitted or `0` is treated as `1`. A negative weight is an
error.

Best when your servers differ in capacity.

## `least_connections`

Sends each request to the server with the fewest requests in flight right now.

Best for requests that take a long time or an uneven amount of time — streaming,
uploads, slow queries — where round-robin would stack requests onto one slow
server while others sit idle.

Haribon counts a request as in flight from the moment it picks the server until
the response has been fully sent. That count is what this method compares, and
it is exposed as `haribon_active_conns` in `/metrics` — but only while
`least_connections` is the configured strategy, because it is the only method
that keeps the count.

## `random`

Picks one of the available servers at random, with no memory between requests.

Best when you just want everything spread out and have nothing to gain from a
strict rotation — no shared counter, so nothing to contend on.

Over a large number of requests the split is even. Over a small number it is
not, and that is the point: two requests in a row can land on the same server.
Use `round_robin` if you need them not to.

## `ip_hash`

Hashes the visitor's address to pick a server, so the same visitor keeps
landing on the same one. Useful when a server caches per-visitor state and a
cold cache costs you.

```yaml
balancer:
  strategy: ip_hash
```

The mapping is stable while the server list is unchanged. Two things move a
visitor off their server:

- **Their server goes down.** The request falls through to the next available
  server rather than failing, so a visitor is never sent to a server that is out.
- **The server list changes size.** Adding or removing a server changes what the
  visitor's address maps to, and some visitors are remapped. This is inherent to
  the technique, not a bug — plan for a cache warm-up after a scale event.

Behind a proxy or a cloud load balancer, every request may arrive from the same
address, which defeats the point. Check that Haribon sees the visitor's real
address before relying on this method.

`ip_hash` only pins within one Haribon process. See
[clustering.md](clustering.md) for what that means when you run several replicas.

---

## Health filtering

Before a server is picked, Haribon asks two questions about it:

1. **Has it been failing?** Requests that fail mark the server down, and a
   successful request marks it up again. This is passive — it costs nothing
   until a real request notices a problem.
2. **Is its breaker open?** After repeated failures, Haribon stops sending
   traffic to a server for a cooldown period, then lets a single request through
   to see whether it has recovered. See [breaker-retry.md](breaker-retry.md).

A server has to pass both. If none do, the request is answered `503`.

---

## Background health checks

Passive health only notices a server after a request has already failed. Turn on
background probes to find out before your users do:

```yaml
health:
  enabled: true
  interval_sec: 10       # how often to probe
  timeout_sec: 2         # give up on a probe after this long
  path: /                # appended to each server's URL
  healthy_threshold: 1   # successes in a row to mark a server healthy
  unhealthy_threshold: 2 # failures in a row to mark a server unhealthy
```

- Each server is probed on its own schedule, so one slow server does not hold up
  the others.
- The probe is an ordinary `GET` to the server's URL plus `health.path`. Any
  `2xx` counts as a success; a timeout or anything else is a failure.
- A server starts out assumed healthy and is only marked down after the
  threshold is reached, so a slow start does not take a server out immediately.
- A server that is marked down is still probed, which is how it comes back.

A newly discovered server starts being probed straight away, without a restart.

---

## Watching it work

Every change is logged as one line, tagged `PROBE`:

```json
{"time":"2026-05-04T01:31:55Z","method":"PROBE","path":"health-check","backend":"http://localhost:4442","status":0,"level":"warn"}
```

`level` is `warn` when a server goes down and `info` when it comes back.

`/metrics` shows the current state per server:

```
haribon_backend_healthy{backend="http://localhost:4442"} 0
haribon_breaker_state{backend="http://localhost:4442"} 1
haribon_active_conns{backend="http://localhost:4442"} 0
haribon_requests_total{backend="http://localhost:4442"} 42
haribon_last_duration_ms{backend="http://localhost:4442"} 3
```

`haribon_breaker_state` is `0` closed, `1` open, `2` half-open. The full list is
in [../README.md](../README.md#metrics).
