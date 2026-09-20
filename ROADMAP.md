# Roadmap

Tracked as GitHub issues — pick one up and link your PR with `Closes #<n>`:

| # | Focus | Issue | Status |
|---|-------|-------|--------|
| 1 | Enterprise hardening: graceful shutdown, `/healthz` + `/readyz`, `haribon check` | [#1](https://github.com/marcuwynu23/haribon/issues/1) | [x] Done |
| 2 | High-traffic resilience: active health checks, retry, circuit breaker, weighted LB | [#2](https://github.com/marcuwynu23/haribon/issues/2) | [x] Done |
| 3 | Pluggable log exporting: Fluent Bit, Loki, Elasticsearch | [#3](https://github.com/marcuwynu23/haribon/issues/3) | [x] Done |
| 4 | Management UI + admin API | [#4](https://github.com/marcuwynu23/haribon/issues/4) | [ ] Open |
| 5 | Zero-downtime ops: hot-reload, auto-discovery, JSON schema | [#5](https://github.com/marcuwynu23/haribon/issues/5) | [x] Done |
| 6 | Clustering / HA: replicated Haribon group with gossip | [#6](https://github.com/marcuwynu23/haribon/issues/6) | [x] Done |
| 7 | TLS termination, request tracing, rate limiting | [#7](https://github.com/marcuwynu23/haribon/issues/7) | [ ] Open |
| 8 | Full observability: Prometheus metrics, OpenTelemetry tracing, Grafana | [#8](https://github.com/marcuwynu23/haribon/issues/8) | [ ] Open |
| 9 | UI Observability section: charts, trace lookup, log tail | [#9](https://github.com/marcuwynu23/haribon/issues/9) | [ ] Open |
| 10 | Multi-host serving (virtual hosts) + high-scale L7 performance | [#10](https://github.com/marcuwynu23/haribon/issues/10) | [ ] Open |

**Legend:** [x] = Implemented | [ ] = Not implemented yet

> **Note:** Issues #1, #2, #3, #5 and #6 are implemented.
>
> Issue #4 is **not**: the `admin:` block is read from the config file and its defaults are
> filled in, but nothing listens on `admin.addr`. There is no management UI and no admin
> API — setting `admin:` today changes nothing. Issues #7–#10 are open as well.