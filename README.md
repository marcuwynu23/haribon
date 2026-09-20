<div align="center">
  <h1>Haribon</h1>
</div>

<p align="center">
  <img src="https://img.shields.io/github/stars/marcuwynu23/haribon.svg" alt="Stars Badge"/>
  <img src="https://img.shields.io/github/forks/marcuwynu23/haribon.svg" alt="Forks Badge"/>
  <img src="https://img.shields.io/github/issues/marcuwynu23/haribon.svg" alt="Issues Badge"/>
  <img src="https://img.shields.io/github/license/marcuwynu23/haribon.svg" alt="License Badge"/>
</p>

Haribon is a small HTTP load balancer written in Go. It sits in front of your
application servers and spreads incoming requests across them, skipping any
server that is down or failing. It runs as a single binary.

---

## Features

### Balancing traffic

- **Five balancing methods**, chosen in the config file with no code changes:
  round-robin, weighted round-robin, least-connections, random, and ip-hash.
- **Skips bad servers** — unhealthy servers and tripped breakers are passed over.
  Haribon answers `503` only when no server is left to try.
- **Weights** — give a larger machine a bigger share of the traffic.

### Staying up

- **Background health checks** — Haribon pings each server on a timer, takes a
  server out of rotation after repeated failures, and puts it back once it
  recovers.
- **Circuit breaker** — when one server keeps failing, Haribon stops sending it
  traffic for a cooldown period, then lets a single request through to see
  whether it has recovered.
- **Retries** — a failed request is retried on another server for methods that
  are safe to repeat (GET, HEAD, PUT, DELETE, OPTIONS). POST and PATCH are never
  retried, so a form is never submitted twice. The retry count is reported in
  the `X-Haribon-Retries` response header.
- **Graceful shutdown** — on `SIGINT` or `SIGTERM`, requests already in flight
  are allowed to finish before the process exits.

### Seeing what is happening

- **`GET /healthz`** — liveness probe. Always answers `200`.
- **`GET /readyz`** — readiness probe. `200` while at least one server is
  healthy, `503` otherwise.
- **`GET /metrics`** — request, response, retry, and error counters, plus gauges
  for per-server health, breaker state, active connections, and last response time.
- **Request logging** as JSON or plain text, one line per request, written to
  stdout, a file, or sent on to Loki, Elasticsearch, or Fluent Bit.
- **A log sink that is down never stalls a request.** Each destination has its
  own queue; if it fills up, log lines are dropped and counted rather than
  slowing down traffic.

### Changing things without a restart

- **Hot reload** — send `SIGHUP`, or start with `--watch_config <seconds>`, and
  Haribon re-reads its config and swaps in the new server list. A bad config is
  rejected and the running one keeps serving. Requests already in flight finish
  against the server list they started with.
- **Server discovery** — get the server list from DNS or from a JSON file that
  something else rewrites. Haribon polls it and adds and removes servers as the
  list changes.
- **Clustering** — run several Haribon replicas and they tell each other which
  servers are down, so a server that one replica cannot reach is skipped by all
  of them. Replicas that are running different config files report it as a
  mismatch so you can see the drift.

### Running it

- **Environment overrides** — `HARIBON_HOST`, `HARIBON_PORT`, `HARIBON_CONFIG`.
- **`${VAR}` in the config file** — any `${NAME}` is replaced with that
  environment variable, so one config file can be shared by many replicas.
- **Config checks for CI** — `haribon check` and `haribon validate` catch a bad
  config before it reaches a server. `haribon validate` also checks your config
  against the bundled JSON schema.
- **Docker image** published to GitHub Container Registry.

---

## Not implemented

These are accepted in the config file but do nothing, so a config that sets them
does not fail to load. They are listed here so you can plan around them.

| Config | What happens |
| --- | --- |
| `admin` | Parsed and then ignored. There is no admin API and no admin UI. |

TLS termination, request tracing, and rate limiting are not implemented either,
and neither are DNS SRV records — the `dns` provider reads `A` and `AAAA`
records and stamps `dns_port` onto each address.

---

## Installation

Build from source (Go 1.23 or newer):

```bash
git clone https://github.com/marcuwynu23/haribon.git
cd haribon
go build -o haribon ./cli
```

Or use the Makefile, which builds to `bin/haribon` and stamps the version:

```bash
make build
```

---

## Configuration

Haribon reads `haribon-config.yml` from the current directory unless you pass
`--config` or set `HARIBON_CONFIG`.

```yaml
host: "0.0.0.0"
port: 4444

# Where request logs go. "stdout" always prints them; "file" also writes
# log_path; the rest send logs to the matching block at the bottom of this file.
logging: true
log_path: "./haribon.log"
log_format: json        # json | text
exporters:
  - stdout
  - file

# How long in-flight requests get to finish on shutdown.
shutdown_timeout_sec: 15

# round_robin | weighted_round_robin | least_connections | random | ip_hash
balancer:
  strategy: round_robin

# Background health checks (off unless enabled)
health:
  enabled: true
  interval_sec: 10       # how often to ping each server
  timeout_sec: 2         # give up on a ping after this long
  path: /                # path to request on each server
  healthy_threshold: 1   # successes needed to mark a server healthy
  unhealthy_threshold: 2 # failures needed to mark a server unhealthy

# Retries, for methods that are safe to repeat
retry:
  max_retries: 1

# Per-server circuit breaker
breaker:
  failure_threshold: 5   # failures before the breaker opens
  cooldown_sec: 30       # how long to wait before testing the server again

backends:
  - url: "http://localhost:4441"
    weight: 1
  - url: "http://localhost:4442"
    weight: 1
  - url: "http://localhost:4443"
    weight: 1

# Add servers from DNS or a file. Discovered servers join the list above and
# are removed again when they disappear from the source.
# discovery:
#   provider: static     # static | dns | file
#   dns_name: "api.internal"
#   dns_port: 80         # port stamped onto each resolved address
#   file_path: "/etc/haribon/backends.json"
#   refresh_sec: 30

# Share server health between replicas.
# cluster:
#   enabled: false
#   node_id: "${HOSTNAME}"   # must differ per replica
#   peers:
#     - "haribon-1:7946"
#     - "haribon-2:7946"
#   gossip_interval_sec: 5
#   gossip_addr: "0.0.0.0:7946"

# Where the "loki", "elasticsearch", and "fluentbit" exporters send logs.
# loki:
#   url: "http://localhost:3100"
#   labels:
#     job: haribon
# elasticsearch:
#   url: "http://localhost:9200"
#   index: haribon
# fluentbit:
#   addr: "localhost:24224"
```

Notes:

- A `weight` that is omitted or `0` is treated as `1`. A negative weight is an
  error.
- Any `${NAME}` in the file is replaced with that environment variable. If the
  variable is not set, the text is left alone and `start` refuses to run — this
  is deliberate for `cluster.node_id`, because two replicas sharing one node
  name would ignore each other's health updates entirely.
- One Haribon process serves one frontend address and one pool of servers.
  Serving several applications from a single binary is tracked in
  [#10](https://github.com/marcuwynu23/haribon/issues/10).

---

## Run

```bash
./haribon start --config haribon-config.yml

# Keep logs
./haribon start --config haribon-config.yml > haribon.log 2>&1
```

---

## Environment overrides

| Variable | What it does | Example |
| --- | --- | --- |
| `HARIBON_HOST` | Address to bind | `0.0.0.0` |
| `HARIBON_PORT` | Port to listen on | `4444` |
| `HARIBON_CONFIG` | Config file path | `./haribon-config.yml` |

---

## Commands

| Command | What it does |
| --- | --- |
| `haribon start --config <file>` | Start the load balancer |
| `haribon check --config <file>` | Check the config, print the servers found, then exit |
| `haribon validate --config <file>` | Check the config *and* validate it against the JSON schema, for use in CI |
| `haribon version` | Print the version |
| `haribon --help` | Print usage |

`start` exits with `1` on a bad config and `2` if it cannot bind its address.
`check` and `validate` exit `0` on success and `1` on error.

The difference between them: `check` applies the rules Haribon itself enforces at
startup (unknown strategy, bad URL, port out of range, and so on). `validate`
runs those same checks and then compares the file against
`schema/haribon-config.schema.json`, which also catches misspelled keys and
values that are not in a list of allowed ones. It reports every problem it
finds, not just the first.

### Checking a config in CI

```bash
haribon check --config haribon-config.yml
# ok: 3 backend(s), strategy: round_robin, probes /healthz /readyz /metrics enabled
#   [0] http://localhost:4441 (weight: 1)
#   [1] http://localhost:4442 (weight: 1)
#   [2] http://localhost:4443 (weight: 1)
```

A JSON schema lives at `schema/haribon-config.schema.json`. Point your editor at
it for autocomplete and inline validation while writing a config.

---

## Choosing a balancing method

Set `balancer.strategy` in the config. Every method skips unhealthy servers, and
Haribon answers `503` only when none are left.

| Method | Config value | Use it when |
| --- | --- | --- |
| Round-robin | `round_robin` (default) | Your servers are all about the same size |
| Weighted round-robin | `weighted_round_robin` | Servers differ in capacity — set `weight` |
| Least-connections | `least_connections` | Requests take a long or uneven time to finish |
| Random | `random` | You want an even spread with no bookkeeping |
| IP hash | `ip_hash` | The same visitor should keep landing on the same server |

An unknown `strategy` stops `start` and `check` with an error.

With `ip_hash`, Haribon hashes the visitor's address to pick a server, so a
visitor keeps the same one as long as the server list does not change. If that
server goes down the request falls through to the next one, so a visitor is
never sent to a server that is out.

---

## Health checks

Background probing is off by default. Turn it on with `health.enabled: true`.

- Each server is requested over HTTP on a timer, at `health.path`.
- Any `2xx` response counts as a success. A timeout or anything else counts as a
  failure.
- A server is marked unhealthy after `unhealthy_threshold` failures in a row,
  and healthy again after `healthy_threshold` successes in a row.
- Unhealthy servers get no traffic, and every change is logged.

You can also check the process by hand at any time:

```bash
curl -i localhost:4444/healthz   # 200 always
curl -i localhost:4444/readyz    # 200 if a server is healthy, otherwise 503
```

---

## Reloading the config

Haribon can pick up a changed config file without a restart, two ways:

```bash
# Ask a running process to reload (Unix only)
kill -HUP $(pidof haribon)

# Or poll the file every 30 seconds
./haribon start --config haribon-config.yml --watch_config 30
```

On reload Haribon re-reads the file, validates it, and swaps in the new server
list, balancing method, and settings. If the new file is invalid, the error is
logged and the running config keeps serving — a typo cannot take the proxy down.

Requests already in flight finish against the config they started with. A server
that stays in the list keeps its failure count and its breaker state, so a
reload does not hand a struggling server a clean slate.

`SIGHUP` is a Unix signal. Windows never delivers it, so use `--watch_config`
there.

---

## Discovery

Instead of listing servers in the config file, Haribon can ask DNS or read a JSON
file. Discovered servers are added to the list in `backends` — they do not
replace it.

```yaml
discovery:
  provider: dns          # static | dns | file
  dns_name: "api.internal"
  dns_port: 80
  refresh_sec: 30
```

```yaml
discovery:
  provider: file
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 30
```

The file is a JSON array of server URLs:

```json
["http://10.0.0.1:8080", "http://10.0.0.2:8080"]
```

- Every `refresh_sec` Haribon re-reads the source. When the list changes, the
  pool is rebuilt: new servers start taking traffic, and servers that went away
  stop.
- A failed lookup is skipped and retried on the next poll; it does not empty the
  pool.
- The `dns` provider reads `A` and `AAAA` records. Each address gets `dns_port`
  attached, because a bare IP is not a usable server address. SRV records are
  not supported.

---

## Clustering

By default each Haribon replica decides on its own which servers are healthy, so
two replicas can disagree about the same server. Turn on `cluster` and they tell
each other:

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"
  peers:
    - "haribon-1:7946"
    - "haribon-2:7946"
  gossip_interval_sec: 5
  gossip_addr: "0.0.0.0:7946"
```

Each replica sends its health findings to the others over UDP every
`gossip_interval_sec`. A server marked unhealthy by any replica is skipped by
all of them, and marked healthy again once a replica sees it recover.

Things worth knowing:

- **`node_id` must be different on every replica.** `${HOSTNAME}` is expanded
  from the environment, which is unique per container or pod. If `node_id` is
  left empty Haribon uses the machine's hostname. If a `${...}` reference cannot
  be resolved, `start` refuses to run — replicas sharing one node ID would
  ignore each other completely.
- **A partitioned replica keeps working.** It falls back to its own findings
  rather than sending traffic nowhere. Two replicas that cannot see each other
  may disagree for a while, which is the safe direction.
- **`gossip_addr` is the address this replica listens on**; `peers` are the
  addresses of the others. Every replica needs the same `peers` list, including
  the ones that are not itself.
- If a replica's config file differs from another's, it is counted in
  `haribon_config_hash_mismatch_total` and logged, so you can spot a rollout
  that only reached some replicas.
- If the gossip port cannot be opened, `start` exits with an error rather than
  running half-connected.

Clustering shares health findings only. It does not replicate config, and it
does not balance requests between the replicas themselves — put them behind
something that does, or point DNS at all of them.

---

## Logging

Each request produces one JSON line:

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

A `retries` field appears when a request was retried. Health-check and breaker
changes are logged the same way, so you can watch a server drop out and come
back.

Set `log_format: text` for a plain line instead of JSON:

```
2026-05-04T01:31:55Z INFO  GET / -> http://localhost:4442 200 5ms
```

### Where logs go

`exporters` lists the destinations. `stdout` always prints to standard output;
`file` writes to `log_path`, creating the file and its directory if needed.
The other three send logs to the address in their own config block:

| Exporter | Sends to | Block |
| --- | --- | --- |
| `loki` | Loki's push API | `loki.url`, `loki.labels` |
| `elasticsearch` | Elasticsearch's bulk API | `elasticsearch.url`, `elasticsearch.index` |
| `fluentbit` | Fluent Bit's forward input on TCP 24224 | `fluentbit.addr` |

Each destination has its own queue and its own background sender, so one slow or
unreachable sink does not hold up the others or the request it came from. When a
queue fills up, lines are dropped rather than delayed.

```yaml
exporters:
  - stdout
  - loki
  - fluentbit

loki:
  url: "http://localhost:3100"
  labels:
    job: haribon
    env: prod
fluentbit:
  addr: "localhost:24224"
```

If `exporters` is left out it defaults to `stdout`.

---

## Metrics

`GET /metrics` returns Prometheus text format:

| Metric | Type | Meaning |
| --- | --- | --- |
| `haribon_requests_total{backend}` | counter | Attempts sent to each server — a request retried on another server counts on both |
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

The response body carries `# TYPE` lines but no `# HELP` descriptions.

---

## Docker

```bash
docker pull ghcr.io/marcuwynu23/haribon:latest
docker run -d -p 4444:4444 ghcr.io/marcuwynu23/haribon:latest
```

The image reads `/etc/haribon/haribon-config.yml` by default.

---

## Trying it out

With the config above running on port 4444:

```bash
curl http://localhost:4444
curl http://localhost:4444/index.html
```

Requests are spread across your servers using the configured method.

---

## Grafana dashboard stack

A ready-made Loki, Promtail, and Grafana setup lives in
`samples/docker-compose/docker-compose.observability.yml`, along with three test
servers.

```bash
docker compose -f samples/docker-compose/docker-compose.observability.yml up -d
```

| Service | Address |
| --- | --- |
| Haribon | [http://localhost:4444](http://localhost:4444) |
| Test servers | 4441, 4442, 4443 |
| Loki | [http://localhost:3100](http://localhost:3100) |
| Grafana | [http://localhost:3000](http://localhost:3000) — log in with `admin` / `admin` |

In Grafana, query `{job="haribon"}`. Promtail reads Haribon's log volume, so set
`logging: true` and `log_path: "./haribon.log"` and keep `file` in `exporters` —
or point Promtail at Haribon's stdout instead. More deployment examples are in
[docs/samples.md](docs/samples.md).

---

## Tests

```bash
go test ./...
```

---

## How it is built

The only dependency is a YAML parser (`gopkg.in/yaml.v2`); everything else is
the Go standard library.

| Package | Responsibility |
| --- | --- |
| `internal/balancer` | The five balancing methods, behind one small interface |
| `internal/health` | Health scheduler, probe handlers, and circuit breaker |
| `internal/proxy` | Forwarding requests, and the retry loop |
| `internal/config` | Loading, validating, watching, and holding config |
| `internal/discover` | Finding servers through DNS or a file |
| `internal/cluster` | Replicas telling each other which servers are down |
| `internal/metrics` | The counters behind `/metrics` |
| `internal/logging` | Log destinations, including the Loki, Elasticsearch, and Fluent Bit senders |

`cli/engine.go` holds the running state — the balancer, breaker, and server list
for the current config. A reload or a discovery change builds a fresh set and
swaps it in, so the old set keeps serving any request already in flight.

---

## Status

Haribon works today as a single-process HTTP load balancer with health checks, a
circuit breaker, retries, metrics, hot reload, server discovery, and
cluster-shared health. TLS, tracing, rate limiting, and the admin API are not
implemented. See [ROADMAP.md](ROADMAP.md) for what is planned.

---

## License

Apache 2.0
