# Samples and Deployment Examples

This page references all sample configurations and deployment manifests shipped with Haribon.

## Configuration Samples

All samples live in `samples/` directory. Copy any sample as a starting point for your deployment.

### Basic Round-Robin

Simplest possible configuration — 3 backends, no extra features.

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

### Weighted Round-Robin

Higher weight = more traffic proportionally.

```yaml
backends:
  - url: "http://backend-a:8080"
    weight: 3
  - url: "http://backend-b:8080"
    weight: 1
```

### DNS Discovery

Backends resolved dynamically from DNS (k8s, auto-scaling).

```yaml
discovery:
  provider: dns
  dns_name: "api.internal"
  refresh_sec: 30
```

### File Discovery

Backends read from a JSON file at runtime.

```yaml
discovery:
  provider: file
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 10
```

### Cluster with Gossip

3+ replicas sharing backend health via gossip protocol.

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"
  peers:
    - "haribon-0:7946"
    - "haribon-1:7946"
  gossip_interval_sec: 5
```

## Docker Compose Cluster

Local cluster deployment with 3 Haribon replicas and nginx load balancer.

```bash
cd samples/docker-compose
docker-compose up -d
```

Access: `http://localhost:4444`
Metrics: `http://localhost:4444/metrics`

```bash
# Test gossip
docker-compose logs haribon-0 | grep gossip

# Chaos test: kill one replica
docker-compose stop haribon-0
# Traffic still flows through remaining replicas
```

## Kubernetes Deployment

Full k8s deployment with HPA, PDB, headless Service for gossip.

```bash
kubectl apply -f samples/k8s/manifests.yml
kubectl scale deploy/haribon --replicas=5
```

Includes:
- Deployment (3 replicas, pod anti-affinity)
- ClusterIP Service on port 4444
- Headless Service on port 7946 for peer discovery
- HPA on CPU + `haribon_requests_total`
- PodDisruptionBudget (minAvailable: 2)
- ConfigMaps for config and cluster settings

## Full Production Config

All features enabled including clustering, health checks, circuit breakers.

```yaml
host: "0.0.0.0"
port: 4444
balancer:
  strategy: least_connections
backends:
  - url: "http://backend1:8080"
    weight: 1
cluster:
  enabled: true
  node_id: "${HOSTNAME}"
  peers: ["haribon-0:7946", "haribon-1:7946"]
health:
  enabled: true
  interval_sec: 5
```

See `samples/production.yml` for the complete example.

## Quick Comparison

| Feature | Basic | Weighted | DNS Discovery | Cluster |
|---------|-------|----------|---------------|---------|
| Backends | Static | Weighted | DNS | Static + gossip |
| Hot reload | No | No | No | SIGHUP |
| Health sharing | No | No | No | Yes |
| Min replicas | 1 | 1 | 1 | 3 |
| Protocol | HTTP | HTTP | HTTP | HTTP + UDP 7946 |
