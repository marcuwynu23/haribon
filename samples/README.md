# Haribon Configuration Samples

This folder contains sample configurations for different deployment scenarios.

## Files

| File | Scenario |
|------|----------|
| `basic-round-robin.yml` | Simplest configuration with 3 backends |
| `weighted-round-robin.yml` | Weighted traffic distribution |
| `dns-discovery.yml` | Dynamic backend resolution via DNS |
| `file-discovery.yml` | JSON file-based backend discovery |
| `production.yml` | Full-featured production config with all options |
| `minimal.yml` | Quick start with just host/port/backends |
| `ip-hash.yml` | Session affinity configuration |
| `cluster.yml` | Cluster configuration with gossip health sharing |
| `cluster.yml` | Cluster configuration with gossip health sharing |
| `cluster.yml` | Cluster configuration with gossip health sharing |
| `docker-compose/` | Local cluster deployment with 3 replicas |
| `k8s/manifests.yml` | Kubernetes deployment (Deployment, Service, HPA, PDB) |

## Quick Start

```bash
# Basic usage
haribon start --config basic-round-robin.yml

# Cluster deployment (local)
cd docker-compose && docker-compose -f docker-compose.yml up -d

# Cluster deployment (k8s)
kubectl apply -f k8s/manifests.yml
kubectl scale deploy/haribon --replicas=5
```

## Cluster Deployment

### Local (docker-compose)
```bash
cd docker-compose
docker-compose -f docker-compose.yml up -d
# Access: http://localhost:4444
# Metrics: http://localhost:4444/metrics

# Test gossip health sharing
docker-compose logs haribon-0 | grep gossip
docker-compose exec haribon-0 curl -s http://localhost:4444/readyz

# Chaos test: kill one replica
docker-compose -f docker-compose.yml stop haribon-0
# Traffic should be unaffected
```

### Kubernetes
```bash
kubectl apply -f samples/k8s/manifests.yml
kubectl scale deploy/haribon --replicas=5
curl localhost:4444/readyz
```

## Cluster Features
- Gossip-based health sharing on port 7946 (UDP)
- Last-writer-wins term-based conflict resolution
- Partition-tolerant: degraded to local health during network splits
- Horizontal scaling: `kubectl scale` or `docker-compose scale`
