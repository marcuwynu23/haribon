# Hot Reload & Backend Discovery

## Overview

Haribon can reload its configuration without restarting, via `SIGHUP` or file
polling. Backend discovery lets servers appear and disappear — from DNS or a
JSON file — without restarting the load balancer.

## Config Hot Reload

### Trigger Mechanisms

1. **SIGHUP**: `kill -HUP <pid>` reloads the config file.
   Unix only — Windows never delivers `SIGHUP`.
2. **File polling**: start with `--watch_config <seconds>` to check the config
   file every N seconds.

```bash
# Reload on demand
haribon start --config haribon-config.yml &
kill -HUP %1

# Or poll every 30 seconds
haribon start --config haribon-config.yml --watch_config 30
```

`--watch_config` is the flag that polls the *config file*.
`discovery.refresh_sec` is unrelated: it sets how often the *discovery source*
(DNS or the backends file) is re-read.

### Semantics

| Action | What swaps in | What needs a restart |
|---|---|---|
| SIGHUP / `--watch_config` | Backend list, weights, balancing method, breaker and retry settings, health-check settings, log destinations | Listener address and port |
| Config validates | The whole config is replaced in one step | — |
| Config fails validation | The running config is kept and the error is logged | — |

### Behavior

- **In-flight requests** finish against the config they started with.
- **A server that stays in the list keeps its state** — its failure count and
  breaker state carry over, so a reload does not hand a struggling server a
  clean slate.
- **Failed reloads** log an error and keep the old config. The process never
  exits because of a bad reload.
- Haribon does not terminate TLS, so there are no certificates to reload.

### Reload Log Output

Reload messages go to standard error using Go's standard logger:

```
2026/05/04 01:31:55 config reloaded: haribon-config.yml
2026/05/04 01:31:55 config reload error: reload validate: unknown balancer strategy "foo"
```

### Example Config

```yaml
host: "0.0.0.0"
port: 4444
backends:
  - url: "http://localhost:4441"
    weight: 1
```

## Backend Auto-Discovery

Discovered servers are **added to** the `backends` list; they do not replace it.

### Providers

#### Static (default)
Fixed server list from the `backends:` field. No dynamic updates.

#### DNS
Polls a DNS name for `A` and `AAAA` records at the configured interval.

```yaml
discovery:
  provider: dns
  dns_name: "api.internal"
  dns_port: 80        # attached to each resolved address (default: 80)
  refresh_sec: 30
```

A DNS lookup returns bare addresses like `10.0.0.1`, which are not usable as
server addresses on their own, so `dns_port` is attached to each one. IPv6
addresses are bracketed correctly. **SRV records are not supported.**

#### File
Watches a JSON file containing an array of server URLs.

```yaml
discovery:
  provider: file
  file_path: "/etc/haribon/backends.json"
  refresh_sec: 10
```

The file must contain a valid JSON array of strings:

```json
["http://10.0.0.1:4441", "http://10.0.0.2:4442"]
```

### What Happens on a Change

Every `refresh_sec` Haribon re-reads the source. When the list differs from the
last one it saw, it rebuilds the pool: new servers start taking traffic and
servers that disappeared stop. A poll that fails, or a file that is momentarily
unreadable, is skipped and retried on the next tick — it never empties the pool.

There is one poller per provider, so the DNS name or file is read once per
interval however many times the pool changes.

### Health Flow

Discovered servers follow the same health flow as configured ones:

- Unhealthy discovered servers are skipped by the balancer.
- Circuit breakers apply to every server regardless of where it came from.
- The health scheduler probes the current server list, so a newly discovered
  server gets a probe loop straight away rather than waiting for a restart.

## Schema Validation

A JSON Schema is shipped at `schema/haribon-config.schema.json`, written against
draft 2020-12.

```bash
haribon validate --config haribon-config.yml
```

`validate` runs the same checks as `check` and then compares the raw file
against the schema, which is what catches a misspelled key or a value outside an
allowed list. It reports every problem it finds.

Haribon's built-in validator implements the subset of JSON Schema this file
uses (`type`, `enum`, `minimum`/`maximum`, `required`, `properties`,
`additionalProperties`, `items`). `$ref`, `oneOf`/`anyOf`/`allOf`, `pattern`,
and `format` are not implemented — keep them out of the schema.

Editors can point at the schema for autocomplete and inline validation.

## Graceful Behavior on Reload

- **Server list changes**: new servers join the pool for new requests; servers
  that left stop receiving new requests, while requests already sent to them
  finish.
- **Weight changes**: applied on the next request after the swap.
- **Listener address changes**: need a restart — the HTTP listener is bound once
  at startup.
- **All servers unhealthy**: `503 All backend servers failed`.
