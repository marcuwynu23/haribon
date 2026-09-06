# AGENTS.md — Haribon Engineering Operating System

> This file is the source of truth for any agent (human or AI) developing Haribon.
> Follow it strictly. If a request conflicts with this file, follow this file and flag the conflict.

## 0. Project Identity

**Haribon** is a lightweight Go-based Layer 7 (HTTP) load balancer: production-ready, observable, simple.

- Language: Go `1.23.0` — module `github.com/marcuwynu23/haribon`
- Entry: `cli/main.go` (`haribon start --config <file>`)
- Core: round-robin + health-aware routing, `net/http` reverse proxy, structured JSON logs (Loki/Promtail-ready)
- State: `atomic.Uint64` counter for RR, `sync.RWMutex` health map, `sync.Mutex` log writer, context-cancelled proxying
- Config: `haribon-config.yml` + env overrides `HARIBON_HOST`, `HARIBON_PORT`, `HARIBON_CONFIG`
- Artifacts: `make build|dist|release`, `Dockerfile` (multi-stage, distroless-ish `alpine:3.20`, non-root `haribon` user), `docker-compose/` observability stack (Haribon → Promtail → Loki → Grafana)
- Tests: `go test ./...` / `make test` — currently `cli/main_test.go`

Do not introduce frameworks, ORMs, or service meshes without an approved GH issue + design note.

---

## 1. Mandatory Personas — Think Like All Six At Once

Every change must satisfy all lenses. If trade-offs arise, document them in PR body under `### Trade-offs`.

### 1.1 Senior Software Engineer
- SOLID, DRY, YAGNI, KISS. Small pure functions. No god files.
- `cli/main.go` is already ~300 lines — **do not grow it**. Extract new domains into new packages:
  - `internal/config/` — load, validate, env override
  - `internal/balancer/` — RR, weighted, least-conn, health filtering
  - `internal/proxy/` — handler, transport tuning, retry
  - `internal/health/` — active/passive checks, circuit breaker
  - `internal/logging/` — Loki JSON writer, levels, rotation hook
  - `internal/metrics/` — `/metrics`, Prometheus
- Concurrency first: prefer `RWMutex` for reads, `atomic` for counters, `context.Context` for every I/O. Never `log.Fatal` inside a handler/library — return errors up.
- Error handling: wrap with `%w`, sentinel errors (`ErrNoBackends`, `ErrNoHealthyBackend`), caller decides HTTP code + log level.
- API backwards-compat: config YAML fields are additive only. Never rename `host/port/backends/url/logging/log_path` without migration + major version note.

### 1.2 DevOps Engineer
- Everything must be reproducible: `go.mod` pinned, `Dockerfile` pinned (`golang:1.23.4-alpine`, `alpine:3.20`), no `latest` in code.
- 12-factor: config via file + env, logs to stdout, no local state assumptions. Port binding via `HARIBON_PORT`.
- Local verification matrix before push:
  ```bash
  go vet ./...
  gofmt -l .
  go test ./... -race -count=1 -coverprofile=coverage.out
  go run ./cli start --config haribon-config.yml
  ```
- CI parity: mirror `.github/workflows/test.yml`. If CI adds a step, update this file.

### 1.3 Tool Developer (CLI / DX Standards)
- CLI contract: `haribon <command> [flags]`. Current: `start --config <file>`. Future: `check|version|validate`.
- Rules:
  - `--help` must always work and print usage + examples.
  - Unknown command → non-zero exit + `unknown command` + usage (keep existing behavior, add exit code `1`).
  - Flags: `--config` (string), long-form only; env fallback `HARIBON_CONFIG`.
  - Errors to `stderr`, logs JSON to `logWriter`, human boot messages to std logger. Never mix.
  - Exit codes: `0` ok, `1` usage/config error, `2` runtime bind failure.
- Version injection via `-ldflags "-X main.version=$(VERSION)"` — never hardcode. Keep `make build/dist/release` working on `windows/linux × amd64/386`.

### 1.4 Platform Engineer
- Stateless proxy core. No disk dependency except optional log file. Must run with read-only FS + non-root.
- Observability mandatory for every feature:
  - Structured log fields: `time, method, path, backend, status, duration_ms, level` — additive only.
  - New failure mode → `level: error` log + `503` semantic preserved.
  - Metrics-ready: if adding latency/retry/circuit logic, expose counters for future `/metrics`.
- Resource budget: p99 proxy overhead < 5ms localhost, no per-request alloc spikes, reuse `http.Client` + `Transport` (pool, timeouts). Default `httpClient.Timeout: 5s`, handler ctx `10s` — changes require justification.
- Config defaults must be safe: `log_path: ./haribon.log`, auto `MkdirAll`, fallback to stdout on failure (keep current behavior).

### 1.5 Container / Orchestration Specialist
- Image: multi-stage, `CGO_ENABLED=0`, `-ldflags="-s -w"`, non-root `haribon`, `EXPOSE 4444`, `ENTRYPOINT` honoring `${HARIBON_CONFIG}`.
- Never run as root, never `chmod 777`, never bake secrets. `COPY haribon-config.yml /etc/haribon/` + `chown haribon:haribon`.
- K8s-ready: liveness/readiness must be satisfiable (future `/healthz`). Log to stdout so Promtail/Loki works without sidecars. Support `HARIBON_HOST=0.0.0.0`.
- Compose: keep `docker-compose/docker-compose.yml` (backends) and `docker-compose.observability.yml` (Loki/Promtail/Grafana) in sync with `README.md` ports: `4444, 4441-4443, 3100, 3000`.

### 1.6 System Designer
- Apply: separation of concerns, fail-fast + graceful degradation (skip unhealthy → `503` only if none healthy), least surprise, idempotency, backpressure via timeouts.
- Any new balancing algorithm, retry, or breaker needs a 5-line design comment at top of file: problem, options, choice, failure mode, observability.
- Roadmap alignment (from README): active health scheduler → retry policy → circuit breaker → `/metrics` + Prometheus → weighted LB. Build in that order unless GH issue says otherwise.

---

## 2. Best-Practice Gates (Hard Requirements)

### 2.1 Go Standards
- `gofmt`, `go vet` clean. No unused imports/vars. Max func ~50 lines; split if larger.
- Naming: `loadConfig`, `applyEnvOverrides`, `getNextBackend`, `setHealth/isHealthy`, `writeLog`, `resolveConfigPath`, `startCommand` — follow existing verbs.
- No globals for new state except behind constructor (e.g. `NewBalancer(backends)`). Existing globals (`backends`, `currentServer`, `backendHealth`) may stay until refactored under a tracked issue — do not add more.
- Dependencies: stdlib first. New dep → justify in PR + `go mod tidy` + license check (MIT-compatible only).

### 2.2 Git — Conventional Commits + Branching (from CONTRIBUTING.md)
- Branch from `develop` (hotfix from `main`):
  - `feature/<slug>`, `fix/<slug>`, `chore/<slug>`, `docs/<slug>`, `refactor/<slug>`, `test/<slug>`
- Commits: `<type>(optional scope): <lowercase imperative short desc>`
  - Types: `feat, fix, docs, style, refactor, test, chore, perf, ci, build`
  - Examples:
    - `feat(balancer): add weighted round-robin`
    - `fix(proxy): preserve query string on forward`
    - `test(health): cover all-unhealthy fallback`
- Rules: lowercase, concise subject ≤72 chars, body explains why. One logical change per commit. Never commit secrets, `bin/`, `dist/`, `releases/`, `*.log`, `tmp/`.
- PRs target `develop`, use `.github/PULL_REQUEST_TEMPLATE.md`, describe what/why/testing + `Closes #<n>`.

### 2.3 Tool Standards
- Keep `make dev|start|build|dist|release|clean|test` green. If you add a make target, add `.PHONY` + help text.
- Dockerfile + compose must still build: `docker build -t haribon:dev .` and `docker compose -f docker-compose/docker-compose.observability.yml config`.

### 2.4 Security
- Follow `SECURITY_POLICY.md`. Validate `url` scheme (`http/https` only), reject `file://`, SSRF-guard backend list in future.
- Copy headers allowlist-style; strip `Hop-by-hop` headers on proxy. No credential logging. `0644` max on log files, `0755` dirs (already in code — preserve).

---

## 3. GH-Issue-Aware Workflow (MANDATORY)

> Never implement blind. Every task starts and ends with GitHub issues.

### 3.1 Before Any Code — Check for Existing / Similar Issues
Run in order, stop when confident:

```bash
gh issue list --state open --limit 50 --search "<keywords>"
gh issue list --state all --limit 50 --search "<keywords>"
gh search issues "<keywords>" --repo {owner}/{repo} --state open
```

- Keywords = error message + component (e.g. `backend 503 balancer`, `log_path fallback`, `weighted`).
- If similar open issue exists: **do not open a duplicate**. Comment `/assign` intent or ask to assign, link it in branch name + commits, and implement against it.
- If similar closed issue exists: read resolution, reuse approach, reference it as `Refs #<n>` to avoid regression.
- If none exists and task is non-trivial (>15 min or user-facing behavior): create one using templates:
  - Bug → `.github/ISSUE_TEMPLATE/bug_report.md`
  - Feature → `.github/ISSUE_TEMPLATE/feature_request.md`
  - Include repro, expected/actual, env, logs.

### 3.2 During Work — Link Continuously
- Branch name includes issue number when practical: `feat/42-weighted-rr`, `fix/87-proxy-query`.
- Every commit touching the issue includes trailer: `Refs #<n>` or `Closes #<n>` (only on final commit that fully resolves).
- If scope creeps beyond issue: open new issue, link `Part of #<n>`, do not bundle.

### 3.3 On Resolve — Reference Correctly
- PR body must contain `Closes #<n>` (or `Fixes #<n>`) so merge auto-closes.
- If PR only partially resolves: use `Part of #<n>` + checklist of remaining work as task list in issue.
- After merge: verify issue auto-closed; if not, comment `Fixed by #<PR> in <sha>` + close manually with reason.
- Reopen criteria: regression → `reopen` + `regression` label + link breaking PR.

### 3.4 Issue Labels (apply via `gh issue edit`)
`bug | enhancement | docs | chore | good first issue | help wanted | priority:high | regression | observability | platform`

---

## 4. Loop Engineering — Iterate Until Green (MANDATORY)

> Single-pass code is not done. Loop: **Implement → Test → Verify → Harden → Repeat** until all gates pass.

### 4.1 The Loop (do not skip steps)

1. **Scope (issue-linked):** restate issue `#n` + acceptance criteria as checklist.
2. **Reproduce / Red:** write failing test first (TDD). For bugs, PoC repro script or `curl` + test proving failure.
3. **Implement (Green):** minimal change satisfying tests. Keep diff focused.
4. **Full Test Matrix (see 4.2):** run complete suite, not just new test.
5. **Verify Runtime:** `go run ./cli start --config haribon-config.yml` + `curl localhost:4444` against stub backends; check JSON logs.
6. **Harden:** race, vet, fmt, coverage, edge cases (empty backends, all unhealthy, slow backend, bad config, missing log dir).
7. **Review Self:** `git diff`, `git status`; ensure no stray files; update docs/README/CHANGELOG if behavior changed.
8. **Repeat** until: all tests pass, coverage non-decreasing, no vet/fmt warnings, runtime smoke passes. Max 5 loops — then stop, summarize blocker, open/update issue.

### 4.2 Complete Test Implementation Matrix (required per feature/fix)

Place tests colocated: `internal/<pkg>/*_test.go`, CLI stays `cli/main_test.go`. Use stdlib `testing` + `httptest` (no new test framework without approval).

| Layer | Command / Location | Must Cover |
|---|---|---|
| Unit | `go test ./... -run TestX -v` | `loadConfig` valid/missing/malformed YAML; `applyEnvOverrides` host/port/invalid port; `resolveConfigPath` cli/empty; `getNextBackend` empty → `ErrNoBackends`, RR order `a→b→c→a`, skip unhealthy, all unhealthy → error, empty health map → all healthy; `writeLog` JSON keys + newline; `setHealth/isHealthy` concurrent access |
| Concurrency | `go test ./... -race -count=1` | parallel `getNextBackend` (e.g. 100 goroutines × 1000 calls) distribution roughly even; parallel `setHealth/isHealthy` + `writeLog` no data race |
| Integration (proxy) | `httptest.NewServer` backends | round-robin across 2–3 stub servers; one stub `500`/closed → skipped or `503` fallback; header + status + body passthrough; query string + method preserved; timeout path logs `level:error status:503` |
| Config e2e | temp YAML + env | missing file → error; `logging:true` without `log_path` → creates `./haribon.log`; unwritable log dir → falls back stdout (no crash); env `HARIBON_PORT=9999` wins over YAML |
| Coverage | `go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out` | new code ≥80%, overall non-decreasing; attach `coverage.out` summary in PR |
| Vet/Lint/Fmt | `go vet ./... && gofmt -l .` | zero output |
| Smoke | `go run ./cli start --config haribon-config.yml` + stub backends on 4441–4443 | `curl -i localhost:4444/` 200s rotate backends; kill one backend → still 200 via healthy; kill all → 503 `All backend servers failed`; logs valid JSON per line (`jq empty haribon.log`) |
| Contract (when touching proxy/logging) | manual `curl` + `jq` | log line has `time,method,path,backend,status,duration_ms,level`; `duration_ms >= 0`; error path has `backend:""` + `status:503` |
| Load (for balancer/perf changes) | `go test -bench=. -benchtime=10x` or `hey/wrk` optional | no deadlock under burst; note p50/p99 before/after in PR |
| Docker (for Dockerfile/env changes) | `docker build -t haribon:dev . && docker run --rm -p 4444:4444 haribon:dev` | boots as `haribon` user, serves traffic, respects `HARIBON_PORT` |

Example canonical tests to add/keep (extend, don't delete):

```go
func TestGetNextBackend_RoundRobin(t *testing.T) { /* a,b,c order, wrap-around */ }
func TestRoundRobinSkipsDead(t *testing.T) { /* a=false,b=true → b */ }
func TestNoBackends(t *testing.T) { /* empty → error */ }
func TestAllUnhealthy(t *testing.T) { /* all false → ErrNoHealthyBackend */ }
func TestConcurrentNextBackend_NoRace(t *testing.T) { /* goroutines + -race */ }
func TestProxy_RoundRobinIntegration(t *testing.T) { /* 2 httptest servers */ }
func TestProxy_AllBackendsDown_503(t *testing.T) { /* closed servers → 503 + error log */ }
func TestWriteLog_JSONFormat(t *testing.T) { /* method/backend keys */ }
func TestApplyEnvOverrides(t *testing.T) { /* host/port, invalid port ignored */ }
func TestLoadConfig_MissingFile(t *testing.T) { /* error, no panic */ }
```

Test hygiene: each test starts with `resetState()` (clear `backends`, `atomic.StoreUint64(&currentServer,0)`, `backendHealth = map[string]bool{}`), restores `logWriter` + env via `t.Cleanup`. No sleeps >100ms; use `httptest` + channels.

### 4.3 Definition of Done
- [ ] Linked issue exists, no duplicate (3.1 evidence in PR: `Checked: gh issue list --search "..." → #n or none`)
- [ ] Full matrix 4.2 run, pasted commands + results in PR
- [ ] `go vet`, `gofmt`, `-race`, coverage all green
- [ ] Runtime smoke + log JSON validated
- [ ] Docs updated (`README.md` / `docs/` / `CHANGELOG.md` if user-facing)
- [ ] Conventional commits, PR targets `develop`, `Closes #n`

---

## 5. Command Cheat Sheet

```bash
# issues first
gh issue list --state open --limit 50 --search "<keywords>"
gh issue view <n>

# dev
make dev                    # air hot-reload
make start                  # go run ./cli
go run ./cli start --config haribon-config.yml
HARIBON_HOST=0.0.0.0 HARIBON_PORT=4444 go run ./cli start --config haribon-config.yml

# quality loop
gofmt -l . ; go vet ./...
go test ./... -race -count=1 -coverprofile=coverage.out
go tool cover -func=coverage.out
make test ; make build

# containers
docker build -t haribon:dev .
docker run --rm -p 4444:4444 haribon:dev
docker compose -f docker-compose/docker-compose.observability.yml up -d
```

---

## 6. File Map — Where Things Go

- `cli/main.go` — thin wiring only (parse flags → load config → build balancer/proxy → serve). No new business logic here.
- `cli/main_test.go` — CLI/config/handler integration tests.
- `haribon-config.yml` — canonical example; keep ports `4444/4441-4443`.
- `internal/*` — all new logic (create as needed, one package per domain).
- `docs/` — design notes + runbooks. New feature → 1-page `docs/<feature>.md`.
- `.github/` — do not bypass templates/workflows. Changes to workflows need `ci:` commit + issue.
- Never touch: `bin/`, `dist/`, `releases/`, `*.log`, `tmp/` (gitignored artifacts).

---

## 7. Agent Rules of Engagement

1. Read this file + `CONTRIBUTING.md` + linked issue before coding.
2. Plan ≤5 steps, then loop per §4 until green. Narrate loop iterations briefly (`Loop 1: red test … Loop 2: green …`).
3. Small diffs. Prefer edit over create. Never invent URLs/APIs.
4. Verify by execution (`go test`, `curl`, `docker build`) — never claim green without output.
5. On conflict between user ask and this file: follow file, explain why, propose issue update.
6. Close with: issue link, test evidence, coverage delta, smoke result, follow-ups as new issues.
