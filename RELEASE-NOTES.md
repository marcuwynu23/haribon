# Release Notes

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
This project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Released versions are also listed in [CHANGELOG.md](CHANGELOG.md).

---

## [Unreleased]

### Added

- Runtime rebuild: routing state can now be replaced while the process keeps
  serving, so config reloads, discovered backends and cluster health changes all
  reach the balancer.
- Log exporting to Loki, Elasticsearch and Fluent Bit, each buffering in the
  background so a slow collector cannot slow down traffic.
- `log_format: text` for a plain log line instead of JSON.
- `dns_port` for DNS discovery, stamped onto each resolved address.
- `${VAR}` substitution across the config file, and a startup error when a
  referenced variable is unset.
- `haribon validate` now checks the config against
  `schema/haribon-config.schema.json` and reports keys it does not recognise.

### Changed

- `haribon_active_conns` is populated only while `least_connections` is the
  configured strategy. It was previously always absent.
- `haribon_cluster_peers` counts replicas heard from recently, and forgets them
  when they go quiet. It could previously only ever decrease.
- A `weight` of `0` or an omitted key means `1`. The JSON schema's minimum for
  `weight` changed from `1` to `0` to match.
- The Kubernetes sample is a StatefulSet rather than a Deployment. Replicas
  address each other by name in `peers`, and only a StatefulSet provides names
  that survive a restart.

### Fixed

- Clustering was written and tested but never started: no gossip node ran and the
  cluster metrics were never registered. It now starts with the process.
- A replica that went away left its "unhealthy" verdict behind on every other
  replica forever, keeping a healthy backend out of rotation. Peer findings now
  expire after about three gossip rounds, never sooner than 15 seconds.
- Config reload did nothing. The file was re-read into a snapshot that nothing
  consulted.
- Backend discovery polled and discarded its results.
- The configured log file was created but never written to, leaving it at 0
  bytes.
- `ip_hash` never read the client address and was round-robin in disguise.
- `node_id: "${HOSTNAME}"` was never expanded, so every replica shared one node
  ID and silently ignored all the others.
- `haribon validate` never opened the JSON schema file.
- The DNS provider returned bare addresses such as `10.0.0.1`, which are not
  usable backend URLs.
- A negative backend `weight` was accepted. It is now a startup error.
- The Kubernetes sample had no `backends:` list — every request would have
  returned 503 — and listed peer names a Deployment never produces.
- The docker-compose cluster sample published host port 4444 twice, so it could
  not start, and ran backends from an image that had nothing to serve.

### Security

- No security fixes in this release.

---

## [1.0.0]

See [CHANGELOG.md](CHANGELOG.md) for the release history up to the current
version.

---

## Notes

- Include links to issues or pull requests when possible, for example
  "Fixed a crash on reload (#42)".
- Call out breaking changes under a "Breaking Changes" heading.
- Keep entries about what a user will notice, not about how it was implemented.
