# Clustering and High Availability

## Overview

Haribon can run as a cluster of N identical stateless replicas behind a shared load balancer (k8s Service, cloud LB, or DNS). Each replica independently proxies traffic and shares backend health state via gossip protocol, ensuring all replicas agree on which backends are healthy.

## Architecture

```
client → k8s Service → [haribon-0, haribon-1, haribon-2] → backends 4441-4443
              ↕ gossip :7946 (health + term)
```

Each replica:
- Proxies HTTP traffic independently (stateless, no coordination needed)
- Probes backends via active health checks
- Gossip health state and breaker state to peers on port 7946 (UDP)
- Uses last-writer-wins with monotonic term for conflict resolution

## Consistency Model

- **AP (Availability + Partition tolerance)**: each replica serves traffic even during network partitions
- **Eventual consistency**: health state converges across replicas within `gossip_interval_sec × 2`
- **Split-brain safe**: partitioned nodes degrade to local health only — never 503 healthy traffic
- **Higher term wins**: if two replicas report conflicting health for the same backend, the one with the higher term wins

## Configuration

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"           # typically the pod name in k8s
  peers:                           # list of peer gossip addresses
    - "haribon-0:7946"
    - "haribon-1:7946"
    - "haribon-2:7946"
  gossip_interval_sec: 5           # how often to gossip (default 5)
  gossip_addr: "0.0.0.0:7946"     # UDP listen address
```

## Peer Discovery

Peers can be specified as static addresses or resolved via DNS:

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"
  peers:
    - "haribon-headless:7946"     # DNS SRV/A resolution
```

## Health Sharing

When replica A marks a backend unhealthy, the gossip message propagates to B and C:

1. A sets `backend_1: {healthy: false, failures: 3, term: 5}`
2. A gossips to B and C
3. B receives the message, sees higher term (5 > local term)
4. B updates its local health state
5. C receives the message and does the same

All replicas now agree backend_1 is unhealthy within one gossip interval.

## Config Hash Consistency

Each node gossips its config hash. If a peer receives a message with a mismatched hash:

- `level:warn` logged in the receiving node
- `/readyz` annotation reflects the mismatch
- No auto-reload (hot-reload is a follow-up feature)

## Rolling Update Procedure

```bash
# Rolling update with no downtime
kubectl rollout restart deployment/haribon

# Each pod goes through graceful shutdown:
# 1. Receives SIGTERM
# 2. Stops accepting new connections
# 3. Finishes in-flight requests
# 4. Leaves the k8s Service endpoints
# 5. Other replicas detect the node is gone and adjust health state
```

## Chaos Testing

```bash
# Kill one replica — traffic should be unaffected
kubectl delete pod haribon-0

# Verify remaining replicas still serve traffic
kubectl exec haribon-1 -- curl -s http://localhost:4444/readyz
# Expected: 200

# Verify /metrics shows correct peer count
kubectl exec haribon-1 -- curl -s http://localhost:4444/metrics | grep cluster_peers
```

## Metrics

| Metric | Description |
|--------|-------------|
| `haribon_cluster_peers` | Number of peers detected |
| `haribon_cluster_term` | Current gossip term |
| `haribon_config_hash_mismatch_total` | Count of config hash mismatches |

## k8s Deployment

```bash
kubectl apply -f k8s/manifests.yml
kubectl scale deploy/haribon --replicas=5
```

The `k8s/manifests.yml` includes:
- Deployment with 3 replicas
- ClusterIP Service on port 4444
- Headless Service on port 7946 for peer discovery
- HPA on CPU + `haribon_requests_total`
- PodDisruptionBudget (minAvailable: 2)

## Production Checklist

- [ ] Set `cluster.enabled: true` with at least 3 replicas
- [ ] Configure `gossip_interval_sec` appropriately (default 5s)
- [ ] Use headless Service for DNS-based peer discovery
- [ ] Set PodDisruptionBudget to ensure min available replicas
- [ ] Configure HPA with CPU and request metrics
- [ ] Verify `/readyz` returns 200 during rolling updates
- [ ] Test chaos scenarios (kill -9 one replica)
- [ ] Monitor `haribon_cluster_peers` and `haribon_cluster_term`

## Non-Goals

- **Sticky sessions**: Haribon remains stateless L7; document as non-goal
- **External consensus (etcd/Consul)**: gossip is sufficient for health hints
- **Disk-based coordination**: non-root + read-only FS compatible
- **Leader election**: no leader needed for MVP
