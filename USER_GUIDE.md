# Haribon User Guide

## Overview

Haribon is a lightweight Go-based Layer 7 (HTTP) load balancer designed for production environments. It supports multiple balancing algorithms, health-aware routing, circuit breakers, retry policies, and Prometheus metrics — all configurable via YAML without code changes.

**Key Features:**
- **Pluggable balancing strategies**: round_robin, weighted_round_robin, least_connections, random, ip_hash
- **Active health checking** with configurable thresholds
- **Circuit breaker** per backend (FSM: closed → open → half-open → closed)
- **Retry policy** for idempotent methods (GET, HEAD, PUT, DELETE, OPTIONS)
- **Prometheus metrics** endpoint (`/metrics`)
- **Structured JSON logs** compatible with Loki/Promtail

## Installation

### From Source

```bash
go get github.com/marcuwynu23/haribon
go build -o haribon ./cli
```

### Docker

```bash
docker pull ghcr.io/marcuwynu23/haribon:latest
docker run -d -p 4444:4444 ghcr.io/marcuwynu23/haribon:latest
```

### Binary Releases

Download from: <https://github.com/marcuwynu23/haribon/releases>

## Quick Start

### 1. Basic Configuration (`haribon-config.yml`)

```yaml
host: "0.0.0.0"
port: 4444
logging: true
log_path: "./haribon.log"

# Balancer strategy
balancer:
  strategy: round_robin  # or: weighted_round_robin, least_connections, random, ip_hash

# Backends
backends:
  - url: "http://localhost:4441"
    weight: 1
  - url: "http://localhost:4442"
    weight: 2

# Health check scheduler
health:
  enabled: false
  interval_sec: 10
  timeout_sec: 2
  path: "/"
  healthy_threshold: 1
  unhealthy_threshold: 2

# Retry policy (idempotent methods only)
retry:
  max_retries: 1

# Circuit breaker per backend
breaker:
  failure_threshold: 5
  cooldown_sec: 30
```

### 2. Start the Load Balancer

```bash
haribon start --config haribon-config.yml
```

### 3. Validate Configuration

```bash
haribon check --config haribon-config.yml
# Output: ok: 2 backend(s), strategy: round_robin, probes /healthz /readyz /metrics enabled
```

## Balancer Strategies

Haribon supports 5 balancing strategies, selectable per configuration:

| Strategy | Use Case | Description |
|----------|----------|-------------|
| `round_robin` | Default | Strict rotation across all healthy backends |
| `weighted_round_robin` | Uneven request costs | Backends with higher weight get proportionally more requests |
| `least_connections` | Uneven/long-lived requests | Sends request to backend with fewest active connections |
| `random` | Stateless uniform fan-out | Uniform random selection among healthy backends |
| `ip_hash` | Session affinity | Consistent backend selection based on client IP hash |

### Configuration Example: Weighted Strategy

```yaml
balancer:
  strategy: weighted_round_robin

backends:
  - url: "http://backend-a:8080"
    weight: 3  # Gets 3x more traffic than weight: 1
  - url: "http://backend-b:8080"
    weight: 1
```

### Configuration Example: Least Connections

```yaml
balancer:
  strategy: least_connections

# Active connection counts exposed via /metrics
```

### Configuration Example: Random

```yaml
balancer:
  strategy: random

# No additional config needed - uniform random among healthy backends
```

### Configuration Example: IP Hash

```yaml
balancer:
  strategy: ip_hash

# Provides session stickiness - clients with same IP map to same backend
```

## Health Checks

Enable active health checking:

```yaml
health:
  enabled: true
  interval_sec: 10      # How often to probe backends (default: 10s)
  timeout_sec: 2        # Probe timeout (default: 2s)
  path: "/"             # Health check path (default: "/")
  healthy_threshold: 1  # Consecutive healthy probes to mark healthy (default: 1)
  unhealthy_threshold: 2 # Consecutive unhealthy probes to mark unhealthy (default: 2)
```

**Probe Behavior:**
- Probes are sent to `/healthz` endpoint
- Backends are marked unhealthy after N consecutive failures
- Unhealthy backends are skipped by the balancer
- All-unhealthy scenario returns `503 Service Unavailable`

## Circuit Breaker

Per-backend circuit breaker prevents cascading failures:

```yaml
breaker:
  failure_threshold: 5  # Fail after 5 consecutive errors (default: 5)
  cooldown_sec: 30      # Seconds in open state before half-open (default: 30)
```

**FSM States:**
- **Closed**: Requests flow normally; failures counted
- **Open**: After threshold exceeded; requests immediately rejected
- **Half-Open**: After cooldown; one test request allowed through
- **Closed**: If test succeeds, resume normal operation

**Metrics exposed:**
- `haribon_breaker_state{backend="..."}` - 0=closed, 1=open, 2=half_open

## Retry Policy

Retries are applied to idempotent methods only (GET, HEAD, PUT, DELETE, OPTIONS):

```yaml
retry:
  max_retries: 1  # Maximum number of retry attempts (default: 1)
```

**Retry Behavior:**
- Only idempotent methods are retried
- Timeout applies per attempt (`httpClient.Timeout: 5s` default)
- Retry count included in log entries (`retries` field)
- Metrics: `haribon_retries_total`

## CLI Commands

### `haribon start`

Start the load balancer with configuration:

```bash
haribon start --config haribon-config.yml
```

Environment variables override YAML:

```bash
HARIBON_HOST=0.0.0.0 HARIBON_PORT=4444 haribon start --config haribon-config.yml
```

### `haribon check`

Validate configuration and exit:

```bash
haribon check --config haribon-config.yml
# Exit 0 if valid, exit 1 if invalid
```

Output shows:
- Number of backends
- Selected strategy
- Whether health checks, readiness, and metrics are enabled
- Per-backend weights

### `haribon version`

Print version and exit:

```bash
haribon version
# Output: haribon dev (or version injected at build time)
```

### Usage Examples

```bash
# Start with default config
haribon start

# Start with custom config and env overrides
haribon start --config prod-config.yml
HARIBON_PORT=8080 haribon start --config prod-config.yml

# Validate config
haribon check --config haribon-config.yml

# Check version
haribon version
```

## Monitoring

### Prometheus Metrics

Haribon exposes metrics at `http://<host>:<port>/metrics`:

```
# HELP haribon_requests_total Total number of proxied requests
# TYPE haribon_requests_total counter
haribon_requests_total{backend="..."} 1234

# HELP haribon_backend_healthy Whether backend is healthy
# TYPE haribon_backend_healthy gauge
haribon_backend_healthy{backend="..."} 1

# HELP haribon_breaker_state Circuit breaker state
# TYPE haribon_breaker_state gauge
haribon_breaker_state{backend="..."} 0

# HELP haribon_last_duration_ms Last request duration in ms
# TYPE haribon_last_duration_ms gauge
haribon_last_duration_ms{backend="..."} 45.2

# HELP haribon_retries_total Total retry attempts
# TYPE haribon_retries_total counter
haribon_retries_total{backend="..."} 5

# HELP haribon_active_conns Active connections per backend
# TYPE haribon_active_conns gauge
haribon_active_conns{backend="..."} 12
```

### Structured Logging

Every proxied request produces a JSON log line:

```json
{"time":"2024-01-15T10:30:00Z","method":"GET","path":"/","backend":"http://localhost:4441","status":200,"duration_ms":45,"retries":0,"level":"info"}
```

Fields (additive - never remove):
- `time` - ISO8601 UTC timestamp
- `method` - HTTP method
- `path` - Request path
- `backend` - Selected backend URL
- `status` - Response status code
- `duration_ms` - Request duration in milliseconds
- `retries` - Number of retry attempts (optional)
- `level` - Log level (info/warn/error)

### Log File Configuration

```yaml
logging: true
log_path: "./haribon.log"
```

If the log directory doesn't exist, Haribon creates it automatically. Falls back to stdout if the log file is unwritable.

## Docker Deployment

### Basic Run

```bash
docker run -d \
  -p 4444:4444 \
  -v $(pwd)/haribon-config.yml:/etc/haribon/haribon-config.yml \
  ghcr.io/marcuwynu23/haribon:latest \
  --config /etc/haribon/haribon-config.yml
```

### With Environment Overrides

```bash
docker run -d \
  -p 4444:4444 \
  -e HARIBON_PORT=8080 \
  -e HARIBON_HOST=0.0.0.0 \
  ghcr.io/marcuwynu23/haribon:latest
```

### Kubernetes

```yaml
apiVersion: v1
kind: Service
metadata:
  name: haribon
spec:
  selector:
    app: haribon
  ports:
    - port: 4444
      targetPort: 4444
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: haribon
spec:
  replicas: 3
  selector:
    matchLabels:
      app: haribon
  template:
    metadata:
      labels:
        app: haribon
    spec:
      containers:
        - name: haribon
          image: ghcr.io/marcuwynu23/haribon:latest
          env:
            - name: HARIBON_CONFIG
              value: /etc/haribon/config.yml
          volumeMounts:
            - name: config
              mountPath: /etc/haribon
      volumes:
        - name: config
          configMap:
            name: haribon-config
```

### Resource Limits

Haribon is lightweight with minimal resource requirements:
- **Memory**: ~10MB base, plus per-backend overhead
- **CPU**: < 1% at moderate traffic
- **File descriptors**: Default 100 max idle connections

## Production Checklist

- [ ] Configure appropriate balancer strategy for your workload
- [ ] Set health check intervals and thresholds
- [ ] Enable circuit breaker with appropriate failure threshold
- [ ] Configure retry policy for idempotent methods
- [ ] Set up log rotation or Loki/Promtail integration
- [ ] Expose `/metrics` for Prometheus scraping
- [ ] Set up `/healthz` and `/readyz` probes for K8s
- [ ] Configure HTTPS termination if terminating TLS before Haribon
- [ ] Set appropriate resource limits in Docker/K8s
- [ ] Test failover scenarios (kill backends, verify 503)

## License

Haribon is released under [Apache License 2.0](LICENSE).