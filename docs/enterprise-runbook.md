# Enterprise Runbook — Haribon

This document covers Kubernetes, systemd, and CI integration patterns for
production deployments of Haribon. See `haribon-config.yml` for the canonical
config example.

---

## Health Probes

Haribon exposes two probe endpoints:

| Endpoint | Type | Returns |
|----------|------|---------|
| `GET /healthz` | Liveness | `200 {"status":"ok"}` — always |
| `GET /readyz` | Readiness | `200 {"status":"ok"}` if ≥1 backend healthy; `503 {"status":"unavailable","message":"..."}` otherwise |

The probes are registered before the proxy mux so they respond even when
all backends are down.

```bash
curl -i localhost:4444/healthz
# HTTP/1.1 200 OK
# {"status":"ok"}

curl -i localhost:4444/readyz
# HTTP/1.1 200 OK  (or 503 when all backends unhealthy)
```

---

## Graceful Shutdown

Haribon listens for `SIGINT` and `SIGTERM`. On receipt:

1. Stop accepting new connections.
2. Wait up to `shutdown_timeout_sec` (default 15 s) for in-flight requests to complete.
3. Log `shutdown complete` and exit 0.

Configure the drain window in `haribon-config.yml`:

```yaml
shutdown_timeout_sec: 15
```

---

## Config Validation in CI

Use `haribon check` to fail the build on bad config before it reaches production:

```bash
# GitHub Actions step
- name: Validate Haribon config
  run: haribon check --config haribon-config.yml

# GitLab CI job
validate-config:
  script:
    - haribon check --config haribon-config.yml
```

Exit codes: `0` = valid, `1` = config error.

---

## Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: haribon
  labels:
    app: haribon
spec:
  replicas: 2
  selector:
    matchLabels:
      app: haribon
  template:
    metadata:
      labels:
        app: haribon
    spec:
      terminationGracePeriodSeconds: 30   # > shutdown_timeout_sec
      containers:
        - name: haribon
          image: ghcr.io/marcuwynu23/haribon:latest
          ports:
            - containerPort: 4444
          env:
            - name: HARIBON_HOST
              value: "0.0.0.0"
            - name: HARIBON_PORT
              value: "4444"
          volumeMounts:
            - name: config
              mountPath: /etc/haribon
          livenessProbe:
            httpGet:
              path: /healthz
              port: 4444
            initialDelaySeconds: 5
            periodSeconds: 10
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /readyz
              port: 4444
            initialDelaySeconds: 3
            periodSeconds: 5
            failureThreshold: 2
          resources:
            requests:
              cpu: "50m"
              memory: "32Mi"
            limits:
              cpu: "500m"
              memory: "128Mi"
      volumes:
        - name: config
          configMap:
            name: haribon-config
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: haribon-config
data:
  haribon-config.yml: |
    host: "0.0.0.0"
    port: 4444
    logging: true
    shutdown_timeout_sec: 15
    backends:
      - url: "http://backend-svc:8080"
---
apiVersion: v1
kind: Service
metadata:
  name: haribon
spec:
  selector:
    app: haribon
  ports:
    - port: 80
      targetPort: 4444
  type: ClusterIP
```

### Horizontal Pod Autoscaler

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: haribon-hpa
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: haribon
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
```

### Rolling Update — Zero Dropped Requests

Key settings that prevent dropped requests during `kubectl rollout`:

1. `terminationGracePeriodSeconds: 30` — kubelet waits at least 30 s after SIGTERM before SIGKILL.
2. `shutdown_timeout_sec: 15` — Haribon drains in-flight requests within 15 s.
3. `readinessProbe` — kubelet removes the pod from the Service endpoints before sending SIGTERM, so no new traffic arrives during the drain window.

---

## systemd Unit

```ini
[Unit]
Description=Haribon Load Balancer
After=network.target
Requires=network.target

[Service]
Type=simple
User=haribon
Group=haribon
ExecStart=/usr/local/bin/haribon start --config /etc/haribon/haribon-config.yml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
# Let systemd wait for in-flight drain (must be > shutdown_timeout_sec)
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

Deploy:

```bash
sudo install -m 0755 haribon /usr/local/bin/haribon
sudo install -m 0644 haribon-config.yml /etc/haribon/haribon-config.yml
sudo systemctl daemon-reload
sudo systemctl enable --now haribon
# Validate config before reload
haribon check --config /etc/haribon/haribon-config.yml && sudo systemctl reload haribon
```

---

## Log Format

Every request produces one JSON line on stdout (and optionally `log_path`):

```json
{
  "time": "2026-09-07T08:00:00.000Z",
  "method": "GET",
  "path": "/api/v1/resource",
  "backend": "http://backend-svc:8080",
  "status": 200,
  "duration_ms": 3,
  "level": "info"
}
```

Error case (all backends unreachable):

```json
{
  "time": "2026-09-07T08:00:00.001Z",
  "method": "GET",
  "path": "/api/v1/resource",
  "backend": "",
  "status": 503,
  "duration_ms": 5001,
  "level": "error"
}
```

Validate logs with `jq`:

```bash
tail -f haribon.log | jq .
# Filter errors:
tail -f haribon.log | jq 'select(.level == "error")'
```

---

## Security Notes

- Hop-by-hop headers (`Connection`, `Transfer-Encoding`, `Upgrade`, `Keep-Alive`, etc.) are stripped from both request and response to prevent header injection.
- `X-Forwarded-For` is appended (not replaced) to preserve the client chain.
- `X-Forwarded-Proto` is set to `http` or `https` based on whether the inbound connection used TLS.
- Config validation rejects non-`http/https` backend URLs (blocks `file://`, etc.).
- Log files are created with `0644` permissions; directories with `0755`.
- The Docker image runs as non-root user `haribon`.

---

## Observability Stack

```bash
# Start full stack (Haribon + Promtail + Loki + Grafana)
docker compose -f docker-compose/docker-compose.observability.yml up -d

# Query logs in Grafana: http://localhost:3000 (admin/admin)
# LogQL: {job="haribon"} |= "error"
```

---

*For roadmap items (active health checks, retry, circuit breaker, Prometheus metrics) see [README.md#roadmap](../README.md#roadmap).*
