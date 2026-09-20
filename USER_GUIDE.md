# Haribon User Guide

## Overview

Haribon is a small HTTP load balancer written in Go, for running in production. It supports several balancing methods, health-aware routing, circuit breakers, retry policies, and Prometheus metrics — all configured in YAML, with no code changes.

**Key Features:**
- **Pluggable balancing strategies**: round_robin, weighted_round_robin, least_connections, random, ip_hash
- **Active health checking** with configurable thresholds
- **Circuit breaker** per backend (closed → open → half-open → closed)
- **Retry policy** for methods that are safe to repeat (GET, HEAD, PUT, DELETE, OPTIONS)
- **Prometheus metrics** endpoint (`/metrics`)
- **Structured logs** in JSON or plain text, sent to stdout, a file, Loki, Elasticsearch, or Fluent Bit
- **Config reload, backend discovery, and cluster-shared health** without a restart

## Installation

### From Source

```bash
git clone https://github.com/marcuwynu23/haribon.git
cd haribon
go build -o haribon ./cli
```

Or `make build`, which writes to `bin/haribon` and stamps the version.

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
- Probes are sent to the path in `health.path` (default `/`), not to a fixed endpoint
- Any `2xx` response counts as a success; a timeout or anything else is a failure
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

## Hot Reload (Zero-Downtime Config Changes)

Haribon supports zero-downtime configuration reloads without restarting the process.

### Trigger Mechanisms

1. **SIGHUP**: `kill -HUP <pid>` to reload the config file atomically
2. **File polling**: `--watch_config N` flag to poll the config file every N seconds

```bash
# Start with SIGHUP support
haribon start --config haribon-config.yml &

# Edit the config file, then trigger reload
kill -HUP %1

# Or use file polling (checks every 30 seconds)
haribon start --config haribon-config.yml --watch_config 30
```

### Behavior

- **In-flight requests** finish against the config they started with
- **Backend list, weights, balancing method, breaker and retry settings, and log destinations** are swapped in for new requests
- **A server that stays in the list keeps its state** — its failure count and breaker state carry over, so a reload does not hand a struggling server a clean slate
- **Failed reloads** log an error and keep the old config — never crash
- **Listener address/port changes** require a restart
- **All-unhealthy** scenario returns `503 All backend servers failed`

TLS certificates are not reloaded or served by Haribon; it does not terminate
TLS. Put a TLS-terminating proxy in front of it.

### Atomic Snapshot

`config.Snapshot` holds the current config behind an atomic pointer, so a reload
replaces it in one step and a request that already started keeps reading the
config it began with.

```go
s := config.NewSnapshot(cfg)
s.Reload(path)  // swaps the config in one step
s.Load()        // returns the current config
```

### Log Output

Reload messages go to standard error using Go's standard logger:

```
2026/05/04 01:31:55 config reloaded: haribon-config.yml
2026/05/04 01:31:55 config reload error: reload validate: unknown balancer strategy "foo"
```

## Backend Auto-Discovery

For dynamic environments (k8s, auto-scaling), Haribon can automatically discover backends.

### Discovery Providers

#### Static (default)
Fixed backend list from `backends:` YAML field. No dynamic updates.

#### DNS
Polls a DNS name for `A` and `AAAA` records at the configured interval.

```yaml
discovery:
  provider: dns
  dns_name: "api.internal"
  dns_port: 80          # port attached to each resolved address (default: 80)
  refresh_sec: 30
```

A DNS lookup returns bare addresses like `10.0.0.1`, which are not usable as
server addresses on their own, so `dns_port` is attached to each one. SRV
records are not supported.

#### File
Watches a JSON file containing an array of backend URLs.

```yaml
discovery:
  provider: file
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 10
```

The JSON file must contain: `["http://10.0.0.1:4441", "http://10.0.0.2:4442"]`

### What Happens on a Change

Every `refresh_sec` Haribon re-reads the source. When the list differs from the
last one it saw, it rebuilds the pool: new servers start taking traffic and
servers that disappeared stop. Discovered servers are *added* to the `backends`
list in the config file — they do not replace it.

A lookup that fails, or a file that is momentarily unreadable, is skipped and
tried again on the next poll. It never empties the pool.

### Health Flow

Discovered backends follow the same health check flow as static backends:
- Unhealthy discovered entries are skipped by the balancer
- Circuit breakers apply to all backends regardless of source
- Health scheduler probes all backends in the current snapshot

### Full Config Example with Discovery

```yaml
host: "0.0.0.0"
port: 4444
balancer:
  strategy: round_robin
discovery:
  provider: dns
  dns_name: "api.internal"
  refresh_sec: 30
health:
  enabled: true
  interval_sec: 10
  timeout_sec: 2
```

## Clustering & High Availability

Haribon can run as several replicas that tell each other which servers are down, so one replica's findings are acted on by all of them.

### Architecture

Each Haribon replica runs as a node in the cluster:

- Nodes exchange server health over **UDP 7946**
- When two nodes disagree about a server, the **newer report wins**
- **A partitioned node keeps working**: it falls back to its own findings rather than sending traffic nowhere
- Minimum recommended: **3 nodes** for production

### Configuration

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"          # must differ on every replica
  peers:
    - "haribon-0:7946"            # addresses of the other replicas
    - "haribon-1:7946"
    - "haribon-2:7946"
  gossip_interval_sec: 5           # how often to exchange health
  gossip_addr: "0.0.0.0:7946"     # address this replica listens on
```

`${HOSTNAME}` is replaced with the `HOSTNAME` environment variable, which is
different in every container and pod. If `node_id` is left empty, Haribon uses
the machine's hostname instead. Two replicas sharing a node ID would ignore each
other's updates entirely, so if a `${...}` reference cannot be resolved,
`start` refuses to run rather than starting a cluster that does nothing.

Every replica needs the same `peers` list, including the entries that are not
itself. A node skips its own address when sending.

If the gossip port cannot be opened, `start` exits with an error. A node that
could send but not receive would look healthy to its peers while silently
ignoring everything they report.

### Behavior

- **Startup**: each node opens its gossip port and sends health to its peers
- **Health sharing**: when a node marks a server unhealthy, the news reaches its peers within a few gossip intervals
- **Conflict resolution**: if two nodes disagree, the newer report wins
- **Graceful degradation**: if gossip is partitioned, each node keeps routing on its own findings
- **No single point of failure**: all nodes are equal; there is no leader

### Config drift

Each node hashes its config file and includes that hash in its updates. A peer
running a different config file is counted in
`haribon_config_hash_mismatch_total` and logged, so a rollout that only reached
some replicas is visible rather than silent.

### What clustering does not do

It shares health findings only. It does not copy config between replicas, and it
does not balance requests across the replicas themselves — put them behind
something that does, or point DNS at all of them.

Nodes running different Haribon versions may not understand each other's
messages; roll out replicas together.

### Deployment Examples

#### Docker Compose (Local Cluster)

```bash
cd samples/docker-compose
docker-compose -f docker-compose.yml up -d
```

This deploys:
- **3 Haribon replicas** with gossip clustering (ports 4444, 7946)
- **3 backend servers** on ports 8081–8083
- **nginx load balancer** in front of all replicas (port 4444)

```bash
# Check cluster status
docker-compose logs haribon-0 | grep gossip
docker-compose exec haribon-0 curl -s http://localhost:4444/readyz

# Test failover: kill one replica
docker-compose -f docker-compose.yml stop haribon-0
# Traffic continues flowing through remaining replicas
```

#### Kubernetes

See `samples/k8s/manifests.yml` for a complete deployment:

```bash
kubectl apply -f samples/k8s/manifests.yml
kubectl scale deploy/haribon --replicas=5
```

Includes:
- **Deployment** with 3+ replicas and pod anti-affinity
- **ClusterIP Service** on port 4444 for client traffic
- **Headless Service** on port 7946 for gossip peer discovery
- **HPA** scaling on CPU and request rate
- **PodDisruptionBudget** ensuring minAvailable=2 during disruptions

### Full Cluster Config Example

```yaml
host: "0.0.0.0"
port: 4444
balancer:
  strategy: least_connections
cluster:
  enabled: true
  node_id: "${HOSTNAME}"
  peers: ["haribon-0:7946", "haribon-1:7946", "haribon-2:7946"]
  gossip_interval_sec: 5
health:
  enabled: true
  interval_sec: 5
  path: /healthz
```

## CLI Commands

### `haribon start`

Start the load balancer with configuration:

```bash
haribon start --config haribon-config.yml
haribon start --config haribon-config.yml --watch_config 30
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

### `haribon validate`

Validate configuration against the JSON schema:

```bash
haribon validate --config haribon-config.yml
# Output: valid: config file passes haribon-config.schema.json (2 backend(s))
```

This runs everything `check` runs, then compares the file against
`schema/haribon-config.schema.json`. That second step is what catches a
misspelled key or a value that is not one of the allowed ones — for example
`strategy: roundrobin` or `prot: 4444`. Every problem found is reported, not
just the first.

The schema is looked for at `schema/haribon-config.schema.json`,
`../schema/haribon-config.schema.json`,
`../../schema/haribon-config.schema.json`, and
`/etc/haribon/haribon-config.schema.json`. Pass `--schema <path>` to point
somewhere else. If no schema can be found, `validate` exits `1` and says so —
run it from the repository root, or pass `--schema`.

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
haribon validate --config haribon-config.yml

# Check version
haribon version
```

### SIGHUP Reload in Scripts

```bash
#!/bin/bash
haribon start --config haribon-config.yml &
PID=$!

# When config changes:
kill -HUP $PID

# With file polling:
haribon start --config haribon-config.yml --watch_config 30 &
```

## Monitoring

### Prometheus Metrics

Haribon exposes metrics at `http://<host>:<port>/metrics`:

```
# TYPE haribon_requests_total counter
haribon_requests_total{backend="http://localhost:4441"} 1234

# TYPE haribon_backend_healthy gauge
haribon_backend_healthy{backend="http://localhost:4441"} 1

# TYPE haribon_breaker_state gauge
haribon_breaker_state{backend="http://localhost:4441"} 0

# TYPE haribon_last_duration_ms gauge
haribon_last_duration_ms{backend="http://localhost:4441"} 45

# TYPE haribon_retries_total counter
haribon_retries_total{backend="http://localhost:4441"} 5

# TYPE haribon_active_conns gauge
haribon_active_conns{backend="http://localhost:4441"} 12
```

The full list:

| Metric | Type | Meaning |
| --- | --- | --- |
| `haribon_requests_total{backend}` | counter | Attempts sent to each server - a request retried elsewhere counts on both |
| `haribon_responses_total{backend,code}` | counter | Responses, by status code |
| `haribon_retries_total{backend}` | counter | Retries, per server |
| `haribon_errors_total{reason}` | counter | Requests that failed on every server |
| `haribon_backend_healthy{backend}` | gauge | `1` healthy, `0` unhealthy |
| `haribon_breaker_state{backend}` | gauge | `0` closed, `1` open, `2` half-open |
| `haribon_active_conns{backend}` | gauge | Requests in flight, per server (`least_connections` only) |
| `haribon_last_duration_ms{backend}` | gauge | How long the last response took |
| `haribon_cluster_peers` | gauge | Replicas heard from recently (clustering only) |
| `haribon_cluster_term` | gauge | Highest update counter this replica has recorded, its own or a peer's |
| `haribon_config_hash_mismatch_total` | counter | Replicas seen running a different config file |

Every value is an integer. The response carries `# TYPE` lines but no `# HELP`
descriptions.

### Structured Logging

Every proxied request produces a log line. In JSON (the default):

```json
{"time":"2026-05-04T10:30:00.1234567Z","method":"GET","path":"/","backend":"http://localhost:4441","status":200,"duration_ms":45,"level":"info"}
```

Fields (additive - never remove):
- `time` - ISO8601 UTC timestamp
- `method` - HTTP method
- `path` - Request path
- `backend` - Selected backend URL
- `status` - Response status code
- `duration_ms` - Request duration in milliseconds
- `retries` - Number of retry attempts (omitted when the request was not retried)
- `level` - Log level (info/warn/error)

With `log_format: text` you get one plain line instead:

```
2026-05-04T10:30:00Z INFO  GET / -> http://localhost:4441 200 45ms
```

### Log Destinations

`logging: true` and `log_path` control the file. `log_format` chooses `json` or
`text`. `exporters` lists where lines go:

```yaml
logging: true
log_path: "./haribon.log"
log_format: json
exporters:
  - stdout
  - file
```

| Exporter | Sends to | Configured by |
| --- | --- | --- |
| `stdout` | standard output | — |
| `file` | `log_path` | `logging: true` |
| `loki` | Loki's push API | `loki.url`, `loki.labels` |
| `elasticsearch` | Elasticsearch's bulk API | `elasticsearch.url`, `elasticsearch.index` |
| `fluentbit` | Fluent Bit's forward input on TCP 24224 | `fluentbit.addr` |

If the log directory doesn't exist, Haribon creates it. If the log file cannot
be opened, Haribon falls back to stdout rather than refusing to start.

Each destination has its own queue and its own background sender, so one slow or
unreachable destination does not delay the others or the request that produced
the line. When a queue fills up, lines are dropped instead of being delayed. If
`exporters` is left out, it defaults to `stdout`.

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
- [ ] Configure retry policy for methods that are safe to repeat
- [ ] Set up log rotation, or send logs to Loki, Elasticsearch, or Fluent Bit with `exporters`
- [ ] Expose `/metrics` for Prometheus scraping
- [ ] Set up `/healthz` and `/readyz` probes for K8s
- [ ] Terminate TLS in front of Haribon — it does not do TLS itself
- [ ] Set appropriate resource limits in Docker/K8s
- [ ] Test failover scenarios (kill backends, verify 503)
- [ ] If running several replicas, enable `cluster` and give each a distinct `node_id`
- [ ] Watch `haribon_config_hash_mismatch_total` during rollouts to catch replicas left on an old config

## License

Haribon is released under [Apache License 2.0](LICENSE).
