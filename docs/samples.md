# Samples and Deployment Examples

Ready-made configurations and deployment manifests. Copy one as a starting
point — every sample uses `localhost:4441-4443` or an `http-echo` container as
its backends, so replace those with your own servers before using any of them
for real.

The full file list, with a one-line description of each, is in
[`samples/README.md`](../samples/README.md).

## Configuration samples

### Basic round-robin

`samples/basic-round-robin.yml` — three backends, one after another, nothing
else set.

```yaml
host: "0.0.0.0"
port: 4444
balancer:
  strategy: round_robin
backends:
  - url: "http://localhost:4441"
  - url: "http://localhost:4442"
  - url: "http://localhost:4443"
```

`samples/minimal.yml` is the same idea with only the settings that have no
usable default.

### Weighted round-robin

`samples/weighted-round-robin.yml` — give a bigger machine a bigger share. The
split is exact: over any five requests, a backend with `weight: 3` gets three of
them.

```yaml
balancer:
  strategy: weighted_round_robin
backends:
  - url: "http://backend-a:8080"
    weight: 3
  - url: "http://backend-b:8080"
    weight: 1
```

`weight` is read only by this strategy. A weight that is omitted or `0` counts
as `1`; a negative weight is refused at startup.

### Same visitor, same backend

`samples/ip-hash.yml` — a visitor keeps landing on the server that already has
their session or their cache entry.

```yaml
balancer:
  strategy: ip_hash
```

Two things to know before relying on this, both spelled out in the comments in
that file: Haribon hashes the address the request arrives from, so anything
sitting in front of it can collapse every visitor onto one hash; and the mapping
is only stable while the backend list is unchanged.

### DNS discovery

`samples/dns-discovery.yml` — the server list comes from DNS, so a
Kubernetes Service or an autoscaling group can hand you servers without a
restart or a config edit.

```yaml
discovery:
  provider: dns
  dns_name: "api.internal"
  dns_port: 8080     # the port stamped onto resolved addresses
  refresh_sec: 30
```

`dns_port` is required in practice: DNS hands back bare addresses like
`10.0.0.1`, and Haribon needs a port to turn that into a usable backend. SRV
records are not supported.

### File discovery

`samples/file-discovery.yml` — a deploy script, an orchestrator, or a database
job writes a JSON array of backend URLs, and Haribon follows it.

```yaml
discovery:
  provider: file
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 10
```

The file holds `["http://10.0.0.1:4441", "http://10.0.0.2:4442"]`. Write it by
creating a new file and renaming it over the old one. Until the file exists and
parses, the pool is empty and every request gets a 503.

### Several replicas sharing what they find

`samples/cluster.yml` — more than one Haribon, all of them skipping a server any
one of them cannot reach.

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"   # must differ on every replica
  peers:
    - "haribon-0:7946"
    - "haribon-1:7946"
    - "haribon-2:7946"
  gossip_interval_sec: 5
  gossip_addr: "0.0.0.0:7946"
```

`node_id` and `peers` are the two things people get wrong. A replica ignores any
message carrying its own id, so two replicas sharing an id silently ignore each
other. And every replica must be able to reach every other one directly — the
list is a full mesh, not a ring. `${VAR}` is expanded from the environment
before the config is used; if a variable is unset, `haribon start` refuses to
run rather than let that happen.

What clustering does and does not give you is in
[docs/clustering.md](clustering.md).

## Docker Compose: a cluster on your machine

`samples/docker-compose/docker-compose.yml` runs three Haribon replicas behind
an nginx load balancer, with three `http-echo` backends.

```bash
cd samples/docker-compose
docker-compose -f docker-compose.yml up -d
```

| What | Where |
|---|---|
| Load balancer (nginx, in front of the replicas) | http://localhost:4444 |
| The replicas, for debugging | 4445, 4446, 4447 |
| The backends | 8081, 8082, 8083 |

```bash
# What each replica knows about the others
curl -s http://localhost:4445/metrics | grep cluster

# Kill one replica; the other two keep serving
docker-compose -f docker-compose.yml stop haribon-0
curl -s http://localhost:4444/
```

On the survivors, `haribon_cluster_peers` drops once the stopped replica's
findings are forgotten — about three gossip rounds later, never sooner than 15
seconds.

There are two more compose files in that directory:
`docker-compose-backends.yml` (one Haribon and three backends, no clustering)
and `docker-compose.observability.yml` (the same plus Loki, Promtail and
Grafana).

## Kubernetes

`samples/k8s/manifests.yml` deploys a three-replica StatefulSet with a
ClusterIP Service, a headless Service for gossip, an HPA and a
PodDisruptionBudget.

```bash
kubectl apply -f samples/k8s/manifests.yml
kubectl rollout status statefulset/haribon
kubectl port-forward svc/haribon 4444:4444
curl http://localhost:4444/
```

It is a StatefulSet rather than a Deployment on purpose. Replicas list each
other by name in `peers`, and only a StatefulSet gives pods names that survive a
restart (`haribon-0`, `haribon-1`, `haribon-2`). A Deployment would hand out
random pod names and the `peers` list would point at nothing.

Two consequences worth knowing:

- The HPA starts at `minReplicas: 3` and its rule is CPU-only, because scaling
  on a request-count metric needs `prometheus-adapter` installed in the cluster.
  Replicas it creates beyond the three named in `peers` serve traffic but take
  no part in health sharing — after scaling up, add the new names to the list.
- The `preStop` hook sleeps for a few seconds before shutdown so the Endpoints
  controller can take the pod out of rotation first. Without it, in-flight
  requests hit a socket that is already closing.

## Full production config

`samples/production.yml` switches on clustering, health checks, circuit
breaking, retries and log exporting, with a comment on each block explaining
what it does and what happens if you get it wrong. Run it through the checker
before you deploy it:

```bash
haribon check --config samples/production.yml
```

## Which one to start from

| You want | Start from |
|---|---|
| The smallest thing that runs | `minimal.yml` |
| Plain round-robin over a fixed list | `basic-round-robin.yml` |
| Unequal machines | `weighted-round-robin.yml` |
| Sessions pinned to a server | `ip-hash.yml` |
| A server list that changes | `dns-discovery.yml` or `file-discovery.yml` |
| More than one Haribon | `cluster.yml` |
| Everything on, commented | `production.yml` |

`admin:` does not appear in any sample, because nothing acts on it yet — see
[ROADMAP.md](../ROADMAP.md).
