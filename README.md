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
It supports round-robin routing, health-aware balancing, structured logging, and Loki/Promtail integration.

> **Vision:** serve multiple applications/hosts from a single Haribon binary with high-scale L7 HTTP performance — see the [roadmap](#roadmap) (issues [#1](https://github.com/marcuwynu23/haribon/issues/1)–[#10](https://github.com/marcuwynu23/haribon/issues/10)).

---

## Features

- Layer 7 (HTTP) load balancing
- Round-robin load balancing
- Health-aware routing (skip unhealthy backends)
- Structured JSON logging (Loki-ready)
- Promtail-compatible log output
- Environment variable overrides
- Safe HTTP reverse proxying
- Automatic fallback logging (stdout if file fails)
- Configurable log file creation and directory auto-creation

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

backends:
  - url: "http://localhost:4441"
  - url: "http://localhost:4442"
  - url: "http://localhost:4443"
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

## Load Balancing Behavior

- Round-robin selection
- Health-aware backend selection
- Automatic fallback if no healthy backend is available
- If health data is empty (startup/testing mode), all backends are treated as healthy

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
```

Requests are distributed using round-robin scheduling with health filtering.

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

## Roadmap

Tracked as GitHub issues — pick one up and link your PR with `Closes #<n>`:

| # | Focus | Issue |
|---|-------|-------|
| 1 | Enterprise hardening: graceful shutdown, `/healthz` + `/readyz`, `haribon check` | [#1](https://github.com/marcuwynu23/haribon/issues/1) |
| 2 | High-traffic resilience: active health checks, retry, circuit breaker, weighted LB | [#2](https://github.com/marcuwynu23/haribon/issues/2) |
| 3 | Pluggable log exporting: Fluent Bit, Loki, Elasticsearch | [#3](https://github.com/marcuwynu23/haribon/issues/3) |
| 4 | Management UI + admin API | [#4](https://github.com/marcuwynu23/haribon/issues/4) |
| 5 | Zero-downtime ops: hot-reload, auto-discovery, JSON schema | [#5](https://github.com/marcuwynu23/haribon/issues/5) |
| 6 | Clustering / HA: replicated Haribon group with gossip | [#6](https://github.com/marcuwynu23/haribon/issues/6) |
| 7 | TLS termination, request tracing, rate limiting | [#7](https://github.com/marcuwynu23/haribon/issues/7) |
| 8 | Full observability: Prometheus metrics, OpenTelemetry tracing, Grafana | [#8](https://github.com/marcuwynu23/haribon/issues/8) |
| 9 | UI Observability section: charts, trace lookup, log tail | [#9](https://github.com/marcuwynu23/haribon/issues/9) |
| 10 | Multi-host serving (virtual hosts) + high-scale L7 performance | [#10](https://github.com/marcuwynu23/haribon/issues/10) |

---

## License

MIT License
