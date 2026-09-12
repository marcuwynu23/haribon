<div align="center">
  <h1> Haribon </h1>
</div>

<p align="center">
  <img src="https://img.shields.io/github/stars/marcuwynu23/haribon.svg" alt="Stars Badge"/>
  <img src="https://img.shields.io/github/forks/marcuwynu23/haribon.svg" alt="Forks Badge"/>
  <img src="https://img.shields.io/github/issues/marcuwynu23/haribon.svg" alt="Issues Badge"/>
  <img src="https://img.shields.io/github/license/marcuwynu23/haribon.svg" alt="License Badge"/>
</p>

Haribon is a lightweight Go-based layer 7 (application‑layer) load balancer designed for simplicity, observability, and production readiness.
It provides **5 pluggable balancing strategies** — round-robin, weighted round-robin, least-connections, random and ip-hash — with health-aware routing, active health checks, per-backend circuit breaker, retry, Prometheus metrics, graceful shutdown and structured JSON logging (Loki/Promtail ready).

> **Vision:** serve multiple applications/hosts from a single Haribon binary with high-scale L7 HTTP performance — see [ROADMAP.md](ROADMAP.md).

---

## Features

- Layer 7 (HTTP) load balancing with **5 pluggable strategies**: round-robin, weighted round-robin, least-connections, random, ip-hash (configurable via `balancer.strategy` without code changes)
- Health-aware routing — skips unhealthy/open-breaker backends, `503` only if none healthy
- Active health-check scheduler (background per-backend probes with healthy/unhealthy thresholds)
- Per-backend circuit breaker (closed / open / half-open FSM)
- Retry policy on idempotent methods (GET, HEAD, PUT, DELETE, OPTIONS) — `X-Haribon-Retries` header
- `GET /healthz` liveness — always 200, `GET /readyz` readiness — 200 if ≥1 healthy backend else 503
- `GET /metrics` — Prometheus-format counters/gauges (requests, retries, breaker state, backend health, active conns, duration)
- Structured JSON logging (Loki-ready) — fields `time, method, path, backend, status, duration_ms, level` additive-only; file/stdout with auto `MkdirAll` and fallback
- Pluggable log exporters (`stdout`, `file`, `loki`, `fluentbit`, `elasticsearch`) with `log_format: json|text`
- Safe HTTP reverse proxying with hop-by-hop stripping, `X-Forwarded-For/Proto` preservation, correct `Content-Length` handling for large HTML/frontend assets
- Environment variable overrides (`HARIBON_HOST`, `HARIBON_PORT`, `HARIBON_CONFIG`) + `haribon check --config` validation for CI
- Graceful shutdown on `SIGINT/SIGTERM` with configurable `shutdown_timeout_sec`, `haribon version` via `-ldflags`

---

## Installation

```bash
git clone https://github.com/marcuwynu23/haribon.git
cd haribon
go build -o haribon main.go
```

---

## Configuration

`haribon-config.yml`

```yaml
host: "0.0.0.0"
port: 4444
logging: true
log_path: "./haribon.log"
balancer:
  strategy: round_robin # round_robin | weighted_round_robin | least_connections | random | ip_hash
backends:
  - url: "http://localhost:4441"
    weight: 1
  - url: "http://localhost:4442"
    weight: 1
  - url: "http://localhost:4443"
    weight: 1
```

> Today Haribon serves **one** frontend (one `host:port`) and **one** backend pool per process.
> Serving multiple applications/hosts from a single binary (virtual hosts, multi-frontend listeners)
> with high-scale L7 performance is tracked in
> [#10](https://github.com/marcuwynu23/haribon/issues/10) — the flat config above will keep working unchanged.

---

## Environment Overrides

| Variable     | Description    | Example |
| ------------ | -------------- | ------- |
| HARIBON_HOST | Bind host      | 0.0.0.0 |
| HARIBON_PORT | Listening port | 4444    |

---

## Run

```bash
./haribon start --config haribon-config.yml
```

---

## Commands

| Command | Description | Exit code |
|---------|-------------|-----------|
| `haribon start --config <file>` | Start the load balancer | 0 ok / 2 bind error |
| `haribon check --config <file>` | Validate config, print backends, exit | 0 ok / 1 error |
| `haribon version` | Print version | 0 |
| `haribon --help` | Print usage | 0 |

### Config validation (CI)

```bash
# In your CI pipeline — fails build on bad config before deployment
haribon check --config haribon-config.yml
# ok: 3 backend(s), probes /healthz /readyz enabled
#   [0] http://localhost:4441
#   [1] http://localhost:4442
#   [2] http://localhost:4443
```

### Health probes

```bash
curl -i localhost:4444/healthz   # 200 always (liveness)
curl -i localhost:4444/readyz    # 200 if >=1 healthy backend, 503 otherwise
```

---

## Load Balancing Behavior

All strategies are health-aware (skip unhealthy/open-breaker) and fall back to `503` only if none healthy; empty health map = all healthy (startup/tests).

| Strategy | Config `balancer.strategy` | Use when |
|---|---|---|
| round-robin | `round_robin` (default) | 3 identical backends, fair rotation |
| weighted round-robin | `weighted_round_robin` | heterogeneous capacity — `backends[].weight` (slots) |
| least-connections | `least_connections` | uneven/long-lived requests, `ActiveConns()` tracked |
| random | `random` | stateless uniform fan-out, no stickiness |
| ip-hash | `ip_hash` | session affinity (best-effort, consistent while pool unchanged) |

Unknown `strategy` → fail-fast at `haribon start`/`check` (`ErrUnknownStrategy`); invalid `weight <1` → validation error.

---

## Health Checking

- Backend health state stored in memory
- Unhealthy backends are skipped automatically
- Only healthy services receive traffic

---

## Logging

Haribon outputs structured JSON logs compatible with Loki and Promtail.

### Example log

```json
{
  "time": "2026-05-04T01:31:55.3551454Z",
  "method": "GET",
  "path": "/",
  "backend": "http://localhost:4442",
  "status": 200,
  "duration_ms": 5,
  "level": "info"
}
```

### Logging behavior

- Logs are written in JSON format
- Supports stdout and file output simultaneously
- Automatically creates log file if missing
- Falls back to stdout if file cannot be created

---

## Docker Usage

### Pull image

```bash
docker pull ghcr.io/marcuwynu23/haribon:latest
```

### Run container

```bash
docker run -d -p 4444:4444 ghcr.io/marcuwynu23/haribon:latest
```

---

## Recommended Structure

```
data/
  haribon-config.yml
  logs/
    haribon.log
```

---

## Example Request

```bash
curl http://localhost:4444
curl http://localhost:4444/index.html
```

Supports both frontend and backend web servers — requests are distributed using the configured strategy (default `round_robin`) with health filtering.

---

## Observability Stack (Loki + Promtail + Grafana)

Haribon includes a full observability stack via `docker-compose.observability.yml`.

### Architecture

```
Haribon → JSON logs → Promtail → Loki → Grafana
```

### Start stack

```bash
docker compose -f docker-compose.observability.yml up -d
```

### Services

#### Haribon

- Load balancer on port 4444
- Writes structured logs to `/var/log/haribon.log`

#### Backend services

- backend1: 4441
- backend2: 4442
- backend3: 4443

#### Loki

- Log storage endpoint: [http://localhost:3100](http://localhost:3100)

#### Promtail

- Tails `./data/logs/haribon.log`
- Forwards logs to Loki

#### Grafana

- [http://localhost:3000](http://localhost:3000)
- Default login: admin / admin

### Query logs

```logql
{job="haribon"}
```

Filter errors:

```logql
{job="haribon"} |= "error"
```

---

## Testing

```bash
go test ./...
```

---

## Architecture Notes

- Stateless proxy core
- Atomic counter for routing
- Mutex-protected log writer
- RWMutex backend health store
- Context-based request cancellation

---

## License

Apache 2.0 License
