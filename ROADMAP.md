# Roadmap

Tracked as GitHub issues — pick one up and link your PR with `Closes #<n>`:

| # | Focus | Issue | Status |
|---|-------|-------|--------|
| 1 | Enterprise hardening: graceful shutdown, `/healthz` + `/readyz`, `haribon check` | [#1](https://github.com/marcuwynu23/haribon/issues/1) | [x] Done |
| 2 | High-traffic resilience: active health checks, retry, circuit breaker, weighted LB | [#2](https://github.com/marcuwynu23/haribon/issues/2) | [x] Done |
| 3 | Pluggable log exporting: Fluent Bit, Loki, Elasticsearch | [#3](https://github.com/marcuwynu23/haribon/issues/3) | [x] Done |
| 4 | Management UI + admin API | [#4](https://github.com/marcuwynu23/haribon/issues/4) | [x] Done |
| 5 | Zero-downtime ops: hot-reload, auto-discovery, JSON schema | [#5](https://github.com/marcuwynu23/haribon/issues/5) | [x] Done |
| 6 | Clustering / HA: replicated Haribon group with gossip | [#6](https://github.com/marcuwynu23/haribon/issues/6) | [x] Done |
| 7 | TLS termination, request tracing, rate limiting | [#7](https://github.com/marcuwynu23/haribon/issues/7) | [ ] Open |
| 8 | Full observability: Prometheus metrics, OpenTelemetry tracing, Grafana | [#8](https://github.com/marcuwynu23/haribon/issues/8) | [ ] Open |
| 9 | UI Observability section: charts, trace lookup, log tail | [#9](https://github.com/marcuwynu23/haribon/issues/9) | [ ] Open |
| 10 | Multi-host serving (virtual hosts) + high-scale L7 performance | [#10](https://github.com/marcuwynu23/haribon/issues/10) | [ ] Open |

**Legend:** [x] = Closed/Impemented | [ ] = Open/Issue exists

> **Note:** Issues #1–#6 are closed and implemented in releases v2.0.0 through v2.2.0.
> Issues #7–#10 are open and tracked for future work.