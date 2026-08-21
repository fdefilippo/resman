# ResMan Development Guide

Normative rules for extending and changing resman.

`CONTRIBUTING.md` covers **process**: how to fork, branch, name a commit, open a pull
request. `docs/ARCHITECTURE.md` covers **structure**: what the components are and how
control flows between them. This document covers **invariants**: what a change to
resman must never break, regardless of which package it touches.

It exists because of a measured problem. The 2026-08-21 audit (epic `resman-4pw`)
produced 16 findings, and 12 of them are the same few missing invariants repeated in
different packages — not 12 unrelated mistakes. A boolean carrying two meanings, a
configuration key with no consumer, a counter incremented before the thing it counts,
an error swallowed under a success log. Each rule below is written against a defect
that actually shipped, and the appendix maps every rule back to the finding that
motivated it.

Read this before your first change. Re-read the checklist in
[Definition of Done](#definition-of-done) before every pull request.

---

## How to read these rules

- **MUST** / **MUST NOT** — a reviewer is expected to block the change.
- **SHOULD** — deviation is allowed but must be justified in the pull request body.
- Rules marked **[checkable]** can be verified mechanically; see
  [Mechanical checks](#mechanical-checks).

Every rule states the invariant first, then *why* it exists, then the concrete defect
it comes from. If a rule ever blocks something legitimate, change the rule in a
dedicated pull request — do not silently make an exception in code.

---

## Baseline quality gates

Before pushing, all of these must pass locally:

```bash
make fmt        # gofmt
make lint       # golangci-lint (make lint-install to get the pinned version)
make test       # go test -v -cover ./...
go test -race ./...
go vet ./...
```

`make test` alone is not a gate. Concurrency defects in this codebase have repeatedly
survived non-race runs; `-race` is mandatory for any change touching shared state, and
cheap enough to always run.

> Note: today only `.github/workflows/release.yml` runs these gates, at release time.
> There is no pull-request workflow. Until there is, the gate is your responsibility
> locally, and the reviewer's during review.

---

# Part I — Domain invariants

## Rule 1 — Eligibility, intent, and observation are three different things **[checkable]**

For every user and every resource, resman deals with three distinct facts:

| Concept | Question it answers | Source of truth |
|---|---|---|
| **Eligibility** | May policy limit this user for this resource? | configuration (include/exclude lists) |
| **Intent** | Has the control cycle decided to limit them? | decision engine output |
| **Observation** | Is a cgroup limit actually in force right now? | cgroup state / `activeUsers` |

- A single field **MUST NOT** carry more than one of these.
- A field name **MUST** say which one it is: `EligibleForCPU`, `RequestedCPULimit`,
  `CPULimitActive`. A bare `IsLimited` is forbidden in new code.
- A consumer **MUST NOT** re-derive one of these from another. If MCP wants "is this
  user actually limited", it reads the observation, not the config.
- When a persisted field changes meaning, the change **MUST** come with a schema note
  or migration — historical rows keep the old semantics and silently corrupt any
  dashboard built on them.

**Why.** `UserMetrics.IsLimited` is currently assigned from `Config.IsUserWhitelisted`
in `metrics/collector.go:1092` (eligibility), then overwritten with runtime state in
`state/control_cycle.go:425-438` before being written to the database — while
`mcp/resources.go:216` and `mcp/tools.go:960` still read the un-overwritten collector
value. The same field means eligibility on one path and observation on another, and the
`is_limited` column in the metrics database has accumulated both.

*Findings: resman-4pw.1, resman-4pw.6*

## Rule 2 — Resource policy lists have one shared, tested contract **[checkable]**

CPU, RAM, and I/O each have their own include and exclude lists. Therefore:

- Eligibility for a resource **MUST** be computed from that resource's own lists.
  Gating RAM or I/O on CPU eligibility is a defect, not a shortcut.
- The **empty-list semantics MUST be documented per resource** and covered by a
  table-driven test. They are deliberately not uniform: an empty CPU include list means
  *nobody* (fail-safe, see commit `404e9f4`), an empty RAM or I/O include list means
  *everybody*. That asymmetry is a product decision and must stay visible in tests, not
  buried in a helper.
- Adding a new limited resource **MUST** add its own `IsUserWhitelistedFor<Resource>`
  and its own row in the empty-list test table.

**Why.** `state/control_cycle.go:447` aggregates `LimitedUsersRAMUsageBytes` and
`LimitedUsersIOWriteBytes` inside `if um.IsLimited`, which is CPU eligibility. With an
empty `USER_INCLUDE_LIST`, RAM and I/O limiting select nobody, even though
`IsUserIncludedForRAM` (`config/config.go:1194`) says an empty list includes everyone.

*Finding: resman-4pw.1*

## Rule 3 — Every configured decision dimension must be evaluated

If the configuration exposes N dimensions for a resource, the decision engine
**MUST** evaluate all N, or the unevaluated ones **MUST** be removed from the
configuration surface.

- Each dimension needs explicit activation, maintenance, and release semantics.
- The rule combining dimensions (any-of, all-of, weighted) **MUST** be documented next
  to the code that implements it.
- "Disabled" values (`max`, `0`, `""`) **MUST** be handled per dimension. One dimension
  being disabled **MUST NOT** disable the others.

**Why.** `state/decision_engine.go:61,101` activates and releases I/O limiting from
`IOWriteBPS` only. `IO_READ_BPS`, `IO_READ_IOPS`, and `IO_WRITE_IOPS` are parsed,
validated, and applied to cgroups, but can never trigger a limit — and setting
`IO_WRITE_BPS=max` disables I/O limiting entirely while the operator believes three
other limits are still armed.

*Finding: resman-4pw.4*

## Rule 4 — No knob without effect **[checkable]**

Every public configuration key **MUST** have a runtime consumer.

- A key is "public" once it appears in a `config:"..."` struct tag, in
  `configFieldHandlers`, in `config/resman.conf.example`, in the man page, or in `docs/`.
- Adding a key **MUST** be done in the same change as the code that reads it. Landing
  the knob first and the behaviour later is forbidden — there is no way for an operator
  to tell the difference between "not implemented yet" and "not working".
- **Validating an inert key is worse than not having it.** Validation is an implicit
  promise that the value matters. If a key must be kept for compatibility but has no
  effect, it **MUST** be rejected with an explicit deprecation warning, not silently
  accepted.
- Removing a key **MUST** be an explicit, documented deprecation decision, never a
  quiet deletion.

**Why.** `CPU_QUOTA_LIMITED` and `RAM_QUOTA_LIMITED` are parsed *and validated*
(`config/config.go:939,957`) with no runtime consumer. `METRICS_CACHE_FILE`,
`PROMETHEUS_FILE` are parsed and unused. `PROMETHEUS_JWT_EXPIRY` is logged and
preserved across reload (`reloader/reloader.go:160`) but does not cap token validity.

*Finding: resman-4pw.12*

---

# Part II — Contracts across boundaries

## Rule 5 — A key crossing a boundary is a typed constant, never a literal **[checkable]**

Any string key shared between a producer and a consumer in different packages —
metrics map keys, status map keys, MCP field names — **MUST** be declared once as an
exported constant (or a typed DTO) and referenced by both sides.

- String literals at both ends of a boundary are forbidden. The compiler cannot catch
  a typo, and the failure mode is a silent zero rather than an error.
- Producer and consumer **MUST** share a test that round-trips the contract.
- If two maps exist with different contents (e.g. a *metrics* map and a *status* map),
  the distinction **MUST** be explicit in the type, not left to the caller's memory.

**Why.** MCP reads `total_user_cpu_usage` in `mcp/tools.go:189`, `mcp/server.go:422`,
and `mcp/resources.go:82`. That key exists nowhere in the codebase — the producer emits
`all_users_cpu_usage` (`state/control_cycle.go:500`). Every MCP status surface has been
reporting a hardcoded zero. The neighbouring `active_users_count` lookups happen to work
only because they read a *different* map (`state/manager.go:283`), which is exactly the
confusion this rule removes.

*Finding: resman-4pw.6*

## Rule 6 — A counter counts what its name says, after it happened

- A `_total` counter **MUST** be incremented only after the operation it names has
  succeeded.
- If attempts are worth measuring, they get their **own** series
  (`..._attempts_total`) or an outcome label. Reusing a success counter for attempts is
  forbidden.
- Every metric declared **MUST** have at least one production call site **[checkable]**.
  A registered metric that nothing updates is indistinguishable, on a dashboard, from a
  system that is idle.
- Help text **MUST** match the semantics, including the unit and whether failures are
  included.

**Why.** `IncrementLimitsActivated()` is called on the first line of `activateLimits`
(`state/limits_applier.go:229`), before any cgroup write; `limits_activated_total`
therefore counts attempts under a name that promises transitions. Meanwhile
`RecordControlCycleDuration`, `RecordMetricsCollectionDuration`, and `RecordError`
(`metrics/prometheus.go:1111-1127`) are declared, registered, documented — and never
called from production code.

*Finding: resman-4pw.10*

## Rule 7 — Never log a success you did not verify

- A function that writes to an external resource (database, cgroup, filesystem,
  network) **MUST** return `error`. A `func (...)` with no error return at such a
  boundary is a design defect.
- The caller owns the success/failure log. A callee **MUST NOT** log a failure and
  return silently, leaving the caller to announce success.
- `Debug` is not a level for failures. Operator-visible failures are `Warn` or `Error`,
  and **SHOULD** also increment an error metric.
- Retry logic **MUST NOT** hide the first failure from observability.

**Why.** `Collector.WriteMetricsToDatabase` (`metrics/collector.go:1575`) returns
nothing and logs a failed batch write at `Debug` before returning; the caller then logs
`"Metrics written to database"` unconditionally (`state/control_cycle.go:622`). An
operator watching logs sees continuous successful persistence while the database
receives nothing.

*Finding: resman-4pw.11*

## Rule 8 — Configuration lifecycle is declared in one place

- Every field **MUST** be classified, in a single authoritative table, as **dynamic**
  (applied on reload) or **restart-required** (preserved on reload, with the divergence
  reported to the operator).
- Hand-maintained per-package preservation lists are forbidden. They drift the moment
  someone adds a field and forgets one of the lists.
- A field classified dynamic **MUST** reach *every* component that holds a copy of it.
  Constructing a value once at bootstrap and never updating it makes the field
  restart-required in practice, whatever the table says.
- Published configuration and the effective state of constructed components **MUST NOT**
  be allowed to diverge silently. If they can, the divergence is surfaced.

**Why.** `preserveRestartRequiredConfig` (`reloader/reloader.go:145`) is a hand-written
list of ~20 entries that omits `METRICS_DB_*` and `CREATED_CGROUPS_FILE`;
`USERNAME_CACHE_TTL` is applied only on a database-enabled bootstrap path and is not
updated by `Collector.UpdateConfig`. Published config and running components disagree,
and nothing reports it.

*Finding: resman-4pw.9*

## Rule 9 — Acknowledge, never sleep

- `time.Sleep` **MUST NOT** be used as synchronisation **[checkable]**. Not to wait for
  a reload, not to wait for a watcher, not to "give it a moment" in production code.
- An operation that triggers asynchronous work **MUST** return either a confirmed
  result or an explicit timeout/failure — never an optimistic success.
- A parameter that claims to control whether runtime state changes (`reload=false`)
  **MUST** actually control it, or be removed with a migration plan.
- Persisted configuration and the published runtime snapshot are separate concepts;
  writing one **MUST NOT** implicitly publish the other.

**Why.** MCP filter setters sleep one second (`mcp/tools.go:674,769`) while the config
watcher debounces for two (`config/watcher.go:204`), so `reload=true` reports success
before the reload can possibly have completed. And `reload=false` mutates the shared
`Config` and persists it anyway, so the flag means nothing in either position.

*Finding: resman-4pw.7*

---

# Part III — Environment, packaging, and on-disk safety

## Rule 10 — Require only the capabilities the enabled features need

- Kernel/cgroup capabilities **MUST** be discovered once and classified as **mandatory**
  for an enabled feature or **optional** (degrade explicitly).
- A controller that enforcement does not use **MUST NOT** be a hard startup
  requirement. CPU quota enforcement uses the `cpu` controller; `cpuset` is optional
  placement.
- The same mandatory/optional classification **MUST** be mirrored in packaging.
  A systemd `ExecStartPre` that fails on an optional controller makes the unit
  unstartable regardless of what the code decided.
- Diagnostics **MUST** distinguish "missing mandatory controller for enabled feature X"
  from "optional controller unavailable, degrading".

**Why.** `cgroup/manager.go:174` returns a fatal error when `+cpuset` cannot be written,
while `cgroup/manager.go:243-246` treats the identical write as best-effort and
continues. `packaging/systemd/resman.service:17` uses `sh -ec`, so the unit dies if
`+cpuset` fails. Delegated or containerised cgroup v2 hierarchies that expose `cpu` but
not `cpuset` cannot run resman, for no functional reason.

*Finding: resman-4pw.14*

## Rule 11 — Shipped operational assets track runtime defaults **[checkable]**

Anything an operator can copy and run — scrape configs, alert rules, dashboards, TLS
scripts, Dockerfiles, unit files, the man page — is part of the product.

- A change to a default (port, namespace, path, metric name) **MUST** update every
  shipped asset in the same change.
- Defaults **SHOULD** be derived from a single documented source rather than retyped.
- Shipped YAML **SHOULD** be validated by a reproducible check (`promtool check rules`,
  `promtool check config`).
- Renames **MUST** be swept across `docs/`, `packaging/`, `scripts/`, `README.md`, and
  `CONTRIBUTING.md`, not just the file that prompted the rename.

**Why.** The project was renamed from cpu-manager to resman and the exporter port moved
to 1974, but `cpu_manager_*` metric names still appear in `docs/alerting-rules.yml`,
`docs/prometheus-queries.md`, the Grafana guides, and `docs/TECHNICAL-SPECIFICATION.md`;
`9100/9101` still appears in `docs/prometheus.yml`, `docs/generate-tls-certs.sh`,
`docs/TLS-CONFIGURATION.md`, `CONTRIBUTING.md`, and — worst, because it is shipped
rather than documentation — `packaging/docker/Dockerfile:40` (`EXPOSE 9101`). Copying
any of these produces a monitoring setup that scrapes nothing.

*Finding: resman-4pw.13*

## Rule 12 — Files written by resman are as restrictive as what they contain

- Configuration can contain secrets (`MCP_AUTH_TOKEN`, password and JWT secret file
  paths). Any file derived from it — temporary file, backup, export — **MUST NOT** be
  more permissive than its source.
- Atomic replacement **MUST** `Stat` the original and create the replacement with the
  original's mode and, where possible, owner; when no prior file exists, default to a
  restrictive mode, never `0644`.
- Backup retention **MUST** be bounded or explicitly disabled. Unbounded timestamped
  backups turn one leak into a permanent archive of leaks.
- Failure paths **MUST NOT** leave readable residue containing secrets.
- Durability (`fsync` of file and parent directory) **MUST** be a documented, tested
  decision, not an accident of ordering.

**Why.** `config/config.go:1336,1350` writes both the timestamped backup and the
temporary file with mode `0644` before renaming, with no inspection of the original
file's permissions and no retention limit. A config readable only by root produces
world-readable copies of itself, one per change, forever.

*Finding: resman-4pw.5*

---

# Part IV — Engineering practice

## Rule 13 — Lock discipline

- Independent state gets an independent mutex. Do not funnel unrelated state through
  one manager-wide lock — it converts a correctness problem into a latency problem and
  back again.
- **MUST NOT** call into another component, run I/O, or invoke a callback while holding
  a lock.
- Copy what you need out from under the lock, release, then act. The existing
  `m.mu.RLock()` / copy / `m.mu.RUnlock()` pattern in `state/` is the reference.
- Any change to shared state **MUST** be validated with `go test -race ./...`, and
  repeated runs (`-count=20`) for anything touching goroutine lifecycle.

**Why.** Rotation deadlocks and username-cache races in `logging/` and `metrics/` were
each caused by a shared lock held across a call into another component. They are cheap
to prevent and expensive to find.

## Rule 14 — Tests encode the contract, not the implementation

- Every semantic fix **MUST** ship a table-driven test that would have failed before it.
  "Verified manually" does not close a behavioural issue.
- Empty/zero/disabled inputs (`""`, `0`, `max`, empty list) **MUST** be explicit rows in
  the table, not implicit paths.
- Cross-package contracts — eligibility, membership reconciliation, reload
  acknowledgement, MCP status, Prometheus outcomes, database failure — belong in the
  integration suite with isolated cgroup, config, and database fixtures. Host capability
  requirements **MUST** be explicit skips, never silent passes.
- New or modified packages **SHOULD** move coverage up, never down. Current floors, for
  reference (2026-08-21): `internal/app` 16.1%, `mcp` 29.6%, `cgroup` 42.7%,
  `config` 55.7%, `metrics` 57.2%, `database` 62.1%, `logging` 62.9%, `state` 66.6%,
  `reloader` 91.9%.

*Finding: resman-4pw.16*

## Rule 15 — One language: English

- New comments, identifiers, log messages, and documentation **MUST** be in English.
- When you touch a function whose comments are in Italian, translate that function's
  comments as part of the change. Do not open mass-translation pull requests; do not
  add new Italian.

**Why.** Several files (`state/control_cycle.go`, `config/config.go`) alternate between
Italian and English within a few lines, which makes grep-based review unreliable and
raises the cost of every external contribution.

---

## Definition of Done

A change is not done until every line is true:

- [ ] `make fmt`, `make lint`, `make test`, `go vet ./...`, `go test -race ./...` pass.
- [ ] No field carries more than one of eligibility / intent / observation (Rule 1).
- [ ] Per-resource policy lists used for their own resource; empty-list behaviour tested
      (Rule 2).
- [ ] Every configured dimension of a touched decision is evaluated (Rule 3).
- [ ] Every configuration key added has a runtime consumer, in this change (Rule 4).
- [ ] No new cross-package string-literal keys (Rule 5).
- [ ] Counters increment after success; every declared metric has a call site (Rule 6).
- [ ] External writes return errors; no unverified success log; failures above `Debug`
      (Rule 7).
- [ ] New config fields classified dynamic or restart-required in the authoritative
      place (Rule 8).
- [ ] No `time.Sleep` used as synchronisation (Rule 9).
- [ ] Capability requirements match enabled features, and packaging agrees (Rule 10).
- [ ] Shipped assets updated for any changed default, name, path, or port (Rule 11).
- [ ] Files written by resman are no more permissive than their source (Rule 12).
- [ ] Shared-state changes validated under `-race` (Rule 13).
- [ ] Table-driven test that fails without the change (Rule 14).
- [ ] New comments and identifiers in English (Rule 15).

---

## Mechanical checks

Rules marked **[checkable]** are meant to be enforced by tooling rather than by
reviewer memory. These checks are **not implemented yet**; they are tracked as
follow-up work to this guide.

Planned as `make verify-contracts`:

| Check | Rule | Approach |
|---|---|---|
| Every `config:"X"` key has a consumer outside `config/` | 4 | reflect over the struct tags, grep field usage per package |
| No cross-package string-literal map keys for known contracts | 5 | grep the known key set outside the constants file |
| Every registered Prometheus metric has a production call site | 6 | AST scan of `metrics/prometheus.go` recorders vs callers |
| No `time.Sleep` in non-test files outside allowed backoff sites | 9 | AST scan with an explicit allowlist |
| Shipped assets contain no stale port/namespace | 11 | grep `9100\|9101\|cpu_manager\|cpu-manager` in `docs/`, `packaging/`, `scripts/` |
| `promtool check rules` / `check config` on shipped YAML | 11 | invoke promtool when available, skip with a warning otherwise |

Until they exist, treat them as review checkpoints.

---

## Appendix — Rule to finding traceability

| Rule | Audit finding |
|---|---|
| 1. Eligibility / intent / observation | `resman-4pw.1`, `resman-4pw.6` |
| 2. Resource policy lists | `resman-4pw.1` |
| 3. All decision dimensions evaluated | `resman-4pw.4` |
| 4. No knob without effect | `resman-4pw.12` |
| 5. Typed keys across boundaries | `resman-4pw.6` |
| 6. Counter semantics | `resman-4pw.10` |
| 7. Truthful errors and logs | `resman-4pw.11` |
| 8. Configuration lifecycle | `resman-4pw.9` |
| 9. Acknowledge, never sleep | `resman-4pw.7` |
| 10. Capability requirements | `resman-4pw.14` |
| 11. Shipped assets | `resman-4pw.13` |
| 12. On-disk file permissions | `resman-4pw.5` |
| 13. Lock discipline | prior race/deadlock fixes in `logging/`, `metrics/` |
| 14. Tests encode the contract | `resman-4pw.16` |
| 15. One language | not covered by the audit |

Findings `resman-4pw.2` (process membership reconciliation), `resman-4pw.3` (excluded
process accounting), `resman-4pw.8` (refresh must not advance decision state), and
`resman-4pw.15` (sampling staleness window) require a **product contract decision**
before a rule can be written. When those decisions are made, record them here.

---

## See also

- `CONTRIBUTING.md` — process: branching, commits, pull requests, releases
- `docs/ARCHITECTURE.md` — component structure and control flow
- `docs/TECHNICAL-SPECIFICATION.md` — detailed component and configuration reference
- `docs/CGROUP-V2-TECHNICAL.md` — cgroup v2 behaviour
- `AGENTS.md` / `CLAUDE.md` — instructions for AI coding agents working on this project
