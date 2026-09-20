<div align="center">
  <h1> Haribon </h1>
</div>

<p align="center">
  <img src="https://img.shields.io/github/stars/marcuwynu23/haribon.svg" alt="Stars Badge"/>
  <img src="https://img.shields.io/github/forks/marcuwynu23/haribon.svg" alt="Forks Badge"/>
  <img src="https://img.shields.io/github/issues/marcuwynu23/haribon.svg" alt="Issues Badge"/>
  <img src="https://img.shields.io/github/license/marcuwynu23/haribon.svg" alt="License Badge"/>
</p>

Haribon is a lightweight Go-based layerâ€¯7 (applicationâ€‘layer) load balancer designed for simplicity, observability, and production readiness.
It provides **5 pluggable balancing strategies** â€” round-robin, weighted round-robin, least-connections, random and ip-hash â€” with health-aware routing, active health checks, per-backend circuit breaker, retry, Prometheus metrics, graceful shutdown and structured JSON logging (Loki/Promtail ready).

> **Vision:** serve multiple applications/hosts from a single Haribon binary with high-scale L7 HTTP performance â€” see [ROADMAP.md](ROADMAP.md).

---

## Features

- Layer 7 (HTTP) load balancing with **5 pluggable strategies**: round-robin, weighted round-robin, least-connections, random, ip-hash (configurable via `balancer.strategy` without code changes)
- Health-aware routing â€” skips unhealthy/open-breaker backends, `503` only if none healthy
- Active health-check scheduler (background per-backend probes with healthy/unhealthy thresholds)
- Per-backend circuit breaker (closed / open / half-open FSM)
- Retry policy on idempotent methods (GET, HEAD, PUT, DELETE, OPTIONS) â€” `X-Haribon-Retries` header
- `GET /healthz` liveness â€” always 200, `GET /readyz` readiness â€” 200 if â‰¥1 healthy backend else 503
- `GET /metrics` â€” Prometheus-format counters/gauges (requests, retries, breaker state, backend health, active conns, duration)
- Structured JSON logging (Loki-ready) â€” fields `time, method, path, backend, status, duration_ms, level` additive-only; file/stdout with auto `MkdirAll` and fallback
- Pluggable log exporters (`stdout`, `file`, `loki`, `fluentbit`, `elasticsearch`) with `log_format: json|text`
- Safe HTTP reverse proxying with hop-by-hop stripping, `X-Forwarded-For/Proto` preservation, correct `Content-Length` handling for large HTML/frontend assets
- Environment variable overrides (`HARIBON_HOST`, `HARIBON_PORT`, `HARIBON_CONFIG`) + `haribon check --config` validation for CI
- Graceful shutdown on `SIGINT/SIGTERM` with configurable `shutdown_timeout_sec`, `haribon version` via `-ldflags`
- **Zero-downtime config hot reload** via `SIGHUP` (`kill -HUP`) or `--watch_config` file polling â€” backends, weights, TLS certs, log level swap atomically without restart
- **Backend auto-discovery** (DNS, file) for dynamic environments â€” `discovery.provider: dns|file|static`
- `haribon validate --config` â€” JSON schema validation for editor autocomplete and CI
- **Clustering & HA** â€” gossip-based health sharing across N replicas, DNS/file peer discovery, k8s Deployment + HPA + PDB support
- **Cluster metrics** â€” `haribon_cluster_peers`, `haribon_cluster_term`, `haribon_config_hash_mismatch_total`

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

# Backend auto-discovery (optional)
discovery:
  provider: static  # static | dns | file
  dns_name: "api.internal"
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 30

# Cluster gossip health sharing (optional)
cluster:
  enabled: true
  node_id: "${HOSTNAME}"
  peers:
    - "haribon-0:7946"
    - "haribon-1:7946"
  gossip_interval_sec: 5
  gossip_addr: "0.0.0.0:7946"
```

> Today Haribon serves **one** frontend (one `host:port`) and **one** backend pool per process.
> Serving multiple applications/hosts from a single binary (virtual hosts, multi-frontend listeners)
> with high-scale L7 performance is tracked in
> [#10](https://github.com/marcuwynu23/haribon/issues/10) â€” the flat config above will keep working unchanged.

> **Hot reload**: Edit the config, then `kill -HUP <pid>` or use `--watch_config N` to poll every N seconds. Backends, weights, TLS certs, and log level swap atomically â€” in-flight requests complete on the previous snapshot. Failed reloads keep the old config. See [docs/hot-reload.md](docs/hot-reload.md).

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
# With file polling every 30 seconds
./haribon start --config haribon-config.yml --watch_config 30
```

---

## Commands

| Command | Description | Exit code |
|---------|-------------|-----------|
| `haribon start --config <file>` | Start the load balancer | 0 ok / 2 bind error |
| `haribon start --config <file> --watch_config N` | Start with file polling every N seconds | 0 ok / 2 bind error |
| `haribon check --config <file>` | Validate config, print backends, exit | 0 ok / 1 error |
| `haribon validate --config <file>` | Validate config against JSON schema | 0 ok / 1 error |
| `haribon version` | Print version | 0 |
| `haribon --help` | Print usage | 0 |

### Config validation (CI)

```bash
# In your CI pipeline â€” fails build on bad config before deployment
haribon check --config haribon-config.yml
haribon validate --config haribon-config.yml
# ok: 3 backend(s), probes /healthz /readyz enabled
#   [0] http://localhost:4441
#   [1] http://localhost:4442
#   [2] http://localhost:4443
```

### Hot reload (zero-downtime config changes)

```bash
# Start the load balancer
haribon start --config haribon-config.yml &

# Edit haribon-config.yml, then trigger reload
kill -HUP %1

# Or use file polling (checks every 30 seconds)
haribon start --config haribon-config.yml --watch_config 30

# Logs: {"level":"info","msg":"config reloaded","path":"haribon-config.yml"}
```

- In-flight requests complete on the previous config snapshot
- Failed reloads log `level:error` and keep the old config â€” never crash
- Backend list, weights, TLS certs, and log level swap atomically
- Listener address/port changes require a restart

### Backend auto-discovery

```yaml
discovery:
  provider: dns        # static | dns | file
  dns_name: "api.internal"
  refresh_sec: 30
```

DNS provider resolves A records on each poll. File provider reads a JSON array of backend URLs. Discovered backends follow the same health checks as static backends. See [docs/hot-reload.md](docs/hot-reload.md).

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
| weighted round-robin | `weighted_round_robin` | heterogeneous capacity â€” `backends[].weight` (slots) |
| least-connections | `least_connections` | uneven/long-lived requests, `ActiveConns()` tracked |
| random | `random` | stateless uniform fan-out, no stickiness |
| ip-hash | `ip_hash` | session affinity (best-effort, consistent while pool unchanged) |

Unknown `strategy` â†’ fail-fast at `haribon start`/`check` (`ErrUnknownStrategy`); invalid `weight <1` â†’ validation error.

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

Supports both frontend and backend web servers â€” requests are distributed using the configured strategy (default `round_robin`) with health filtering.

---

## Observability Stack (Loki + Promtail + Grafana)

Haribon includes a full observability stack via `samples/docker-compose/docker-compose.observability.yml`.

### Architecture

```
Haribon â†’ JSON logs â†’ Promtail â†’ Loki â†’ Grafana
```

### Start stack

```bash
docker compose -f samples/docker-compose/docker-compose.observability.yml up -d
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
- `config.Snapshot` with `atomic.Pointer` for atomic config swaps on SIGHUP
- `internal/discover` package with Provider interface (static, dns, file)
- `internal/cluster` package with gossip protocol for health sharing (port 7946 UDP)
- JSON schema at `schema/haribon-config.schema.json` for editor autocomplete
- AP consistency model: eventual consistency, partition-tolerant, split-brain safe

---

## License

Apache 2.0 License

