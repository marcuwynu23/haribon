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

Use `haribon check` to fail the build on a bad config before it reaches production:

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

`haribon check` applies the rules Haribon itself enforces at startup. Use
`haribon validate` instead to also compare the file against
`schema/haribon-config.schema.json`, which additionally catches misspelled keys
and values that are not in a list of allowed ones. `validate` reports every
problem it finds, not just the first.

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

1. `terminationGracePeriodSeconds: 30` — kubelet waits at least 30 s after SIGTERM before SIGKILL. Must be longer than `shutdown_timeout_sec`, or the drain is cut short.
2. `shutdown_timeout_sec: 15` — Haribon drains in-flight requests within 15 s.
3. `preStop: sleep 5` — see below.

Item 3 is the one people miss. Kubernetes removes a pod from Service endpoints
and sends SIGTERM **at the same time**, not one before the other, so a request
can still be routed to a pod that has just stopped accepting connections. A
`preStop` hook that pauses for a few seconds gives the endpoint removal time to
propagate first. The pause is inside `terminationGracePeriodSeconds`, so it does
not shorten the drain.

The `readinessProbe` is what takes a replica out of rotation when it genuinely
cannot serve, but it is not a rolling-update mechanism — its period is measured
in seconds, and the gap it leaves is exactly the one `preStop` closes.

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

- The per-connection headers that only apply to one hop (`Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade`) are removed from both the request and the response, along with anything the `Connection` header names. A client cannot smuggle a header through Haribon to your servers.
- `X-Forwarded-For` is appended to, not replaced, so the existing chain is preserved.
- `X-Forwarded-Proto` is set from the inbound connection. Haribon does not terminate TLS, so in practice it is always `http` — a TLS-terminating proxy in front of Haribon should set the real value itself.
- Config validation rejects backend URLs that are not `http` or `https` (so `file://` and the like cannot be configured).
- Log files are created with `0644` permissions; directories with `0755`.
- The Docker image runs as non-root user `haribon`.

---

## Observability Stack

```bash
# Start full stack (Haribon + Promtail + Loki + Grafana)
docker compose -f samples/docker-compose/docker-compose.observability.yml up -d

# Query logs in Grafana: http://localhost:3000 (admin/admin)
# LogQL: {job="haribon"} |= "error"
```

---

*Active health checks, retries, the circuit breaker, Prometheus metrics, hot reload, backend discovery, and cluster-shared health are all implemented. See [../README.md](../README.md) for what each one does and [ROADMAP.md](../ROADMAP.md) for what is still planned.*

