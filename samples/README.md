# Haribon Configuration Samples

Ready-made configurations for common setups. Copy one as a starting point.

> Every sample uses `localhost:4441-4443` or an `http-echo` container as its
> backends. Replace those with your own servers before using it for real.

## Files

| File | Scenario |
|------|----------|
| `minimal.yml` | The smallest config that runs |
| `basic-round-robin.yml` | 3 backends, round-robin |
| `weighted-round-robin.yml` | Weighted traffic distribution |
| `ip-hash.yml` | The same visitor keeps the same server |
| `dns-discovery.yml` | Get the server list from DNS |
| `file-discovery.yml` | Get the server list from a JSON file |
| `cluster.yml` | Several replicas sharing what they find |
| `production.yml` | Everything switched on, with comments |
| `docker-compose/` | A 3-replica cluster, nginx in front, on your machine |
| `k8s/manifests.yml` | Kubernetes: StatefulSet, Services, HPA, PDB, ConfigMap |

`samples/docker-compose/` also contains `docker-compose-backends.yml` (Haribon
plus three `http-echo` backends, single instance) and
`docker-compose.observability.yml` (Loki, Promtail, Grafana, and the backends).

## Quick start

```bash
# Single instance
haribon start --config samples/minimal.yml

# Check a config without starting anything
haribon check --config samples/production.yml
```

## The cluster sample

### On your machine

```bash
cd samples/docker-compose
docker-compose -f docker-compose.yml up -d
```

| What | Where |
|---|---|
| Load balancer (nginx → the 3 replicas) | http://localhost:4444 |
| The replicas themselves | 4445, 4446, 4447 |
| Backends | 8081, 8082, 8083 |

```bash
# Each replica is a separate process agreeing on backend health
curl -s http://localhost:4445/metrics | grep cluster

# Kill one replica; the other two keep serving
docker-compose -f docker-compose.yml stop haribon-0
curl -s http://localhost:4444/
```

Watch `haribon_cluster_peers` on the survivors: it drops while the stopped
replica's findings are still remembered, then falls back to the replicas that
are left.

### On Kubernetes

```bash
kubectl apply -f samples/k8s/manifests.yml
kubectl rollout status statefulset/haribon
kubectl port-forward svc/haribon 4444:4444
curl http://localhost:4444/
```

It is a StatefulSet rather than a Deployment on purpose: replicas list each
other by name in `peers`, and only a StatefulSet gives pods names that survive a
restart (`haribon-0`, `haribon-1`, `haribon-2`).

## What clustering gives you

- Replicas exchange which servers they find unhealthy, over UDP on port 7946.
  There is no separate coordination service to run.
- A replica that cannot reach the others keeps serving from its own findings
  instead of failing.
- A replica's findings are forgotten about three gossip rounds after it stops
  reporting, so a replica that goes away cannot keep a healthy server out of
  rotation.
- Scaling changes how many replicas there are, not what Haribon does. A new
  replica can only exchange findings with replicas that name it in `peers`, so
  after scaling up, add the new names to the list. This is why the manifest's
  HPA starts at `minReplicas: 3`: replicas it creates beyond the three named in
  `peers` would serve traffic but take no part in health sharing.

Details, including what clustering deliberately does *not* do, are in
[docs/clustering.md](../docs/clustering.md).
