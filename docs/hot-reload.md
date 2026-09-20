# Hot Reload & Backend Discovery

## Overview

Haribon supports zero-downtime configuration reloads via `SIGHUP` and optional file polling. Backend auto-discovery (DNS SRV / file provider) allows backends to appear and disappear without restarting the load balancer.

## Config Hot Reload

### Trigger Mechanisms

1. **SIGHUP**: Send `kill -HUP <pid>` to reload the config file atomically.
2. **File polling**: Set `discovery.refresh_sec` > 0 to poll the config file at the given interval (seconds).

### Semantics

| Action | What swaps | What needs restart |
|---|---|---|
| SIGHUP / poll | Backend list, weights, TLS certs, log level | Listener address/port |
| New config validates | Atomic snapshot swap via `atomic.Pointer` | — |
| Validation failure | Old config preserved | — |

### Behavior

- **In-flight requests** complete on the previous snapshot.
- **Failed reloads** log `level:error` and keep the old config — never crash the process.
- **New backends** discovered via providers feed the same snapshot swap and follow the normal health flow.

### Usage

```bash
# Start haribon
haribon start --config haribon-config.yml &

# Edit haribon-config.yml, then trigger reload
kill -HUP %1

# Or with file polling (watch_config equivalent)
# Set refresh_sec > 0 in the discovery section
```

### Example Config

```yaml
host: "0.0.0.0"
port: 4444
backends:
  - url: "http://localhost:4441"
    weight: 1
discovery:
  provider: dns
  dns_name: "api.internal"
  refresh_sec: 30
```

## Backend Auto-Discovery

### Providers

#### Static (default)
Fixed backend list from `backends:` YAML field. No dynamic updates.

#### DNS
Polls a DNS name for A/SRV records at the configured interval.

```yaml
discovery:
  provider: dns
  dns_name: "api.internal"
  refresh_sec: 30
```

#### File
Watches a JSON file containing an array of backend URLs.

```yaml
discovery:
  provider: file
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 10
```

The JSON file must contain a valid JSON array of strings:
```json
["http://10.0.0.1:4441", "http://10.0.0.2:4442"]
```

### Health Flow

Discovered backends follow the same health check flow as static backends:
- Unhealthy discovered entries are skipped by the balancer.
- Circuit breakers apply to all backends regardless of source.
- Health scheduler probes all backends in the current snapshot.

## Schema Validation

A JSON Schema is shipped at `schema/haribon-config.schema.json` (draft 2020-12) for editor autocomplete and CI validation.

```bash
# Validate a config file against the schema
haribon validate --config haribon-config.yml
```

Editors can hook up the schema via `$schema` in YAML editors or use the `haribon validate` command in CI.

## Graceful Behavior on Reload

- **Backend list changes**: New backends are added to the atomic snapshot immediately. Old backends are removed from future `Next()` calls but in-flight requests to them complete.
- **Weight changes**: Weights are re-expanded on the next `Next()` call for weighted RR.
- **Listener address changes**: Require a restart (the HTTP server binds at startup).
- **All backends unhealthy**: Returns `503 All backend servers failed` with `level:error` in logs.
