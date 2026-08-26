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

The previous audit epic, `resman-ne0`, was closed issue by issue — and the defect class
survived. Of the eleven `resman-4pw` findings traced back to it, two are direct
regressions introduced *while* closing an old issue, seven are prior remediations that
were correct but too local, one was never covered, and one is a latent coupling exposed
by a later, correct change. That is not a diligence problem. It is what happens when
fixes are verified at the level of the function that changed rather than the contract
it belongs to — which is the level these rules operate at.

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
> There is no pull-request workflow. Until there is (`resman-4pw.17.2`), the gate is
> your responsibility locally, and the reviewer's during review.

---

# Part 0 — Project policy

## Rule 1 — No backward compatibility. Breaking is allowed; silence is not

resman has **no backward-compatibility requirement**. When a contract is wrong, it is
changed, not wrapped.

- Obsolete database schemas, configuration keys, metric keys, MCP protocol revisions,
  and behavioural aliases **MUST NOT** be preserved.
- Compatibility shims, deprecation periods, dual-read paths, silent migrations, and
  "accept both spellings" aliases are **forbidden**.
- A breaking change **MUST** fail clearly or require an explicit operator reset. It
  **MUST NOT** silently reinterpret old state. Deleting a knob and ignoring it is not
  a breaking change, it is a hidden one.
- Concretely:
  - **Removed configuration key** → startup fails with the offending key and file
    named. Not warned, not ignored.
  - **Incompatible persisted schema** → refuse to open the store, tell the operator
    what to do (delete/reset), and stop. No in-place migration, no best-effort read of
    the old shape.
  - **Removed API field, metric key, or protocol revision** → absent and rejected, not
    aliased to the replacement.
- The freedom to break is not a licence to break casually. It removes the *shim* from
  the menu, not the *thinking*: the replacement must be right, documented in the same
  change, and reflected in `config/resman.conf.example`, `docs/`, and the man page.

This policy is a current product decision recorded in epic `resman-4pw`. Only a later
explicit product decision reverses it — not an individual pull request.

**Why.** Compatibility debt is what made the audit findings survivable in the first
place: an inert `CPU_QUOTA_LIMITED` that still validates, the former
`total_user_cpu_usage` consumer key that never had a producer, and the removed
`reload=false` parameter that did not mean what it said. Each was cheaper to leave
than to remove — until there were sixteen of them.

**Resolution.** `setConfigField` now rejects every key absent from
`configFieldHandlers`, so file typos fail with the key, line, and file named. Removed
public keys have explicit rejection tombstones so the same failure also applies to
environment overrides; no tombstone parses or aliases a value.

*Source: epic `resman-4pw` policy; findings `resman-4pw.7`, `resman-4pw.12`*

---

# Part I — Domain invariants

## Rule 2 — Eligibility, intent, and observation are three different things **[checkable]**

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
- When a persisted field changes meaning, the schema change is **intentionally
  breaking** (Rule 1): the store refuses to open old data and the operator resets it.
  Reading old rows under the new meaning is forbidden — historical rows written under
  the previous semantics would silently corrupt every dashboard built on them.

**Why.** Before `resman-4pw.1`, `UserMetrics.IsLimited` was assigned from CPU policy
eligibility in the collector, overwritten with runtime state before database writes,
and still exposed with its original meaning through MCP. The version 2 metrics schema
replaced that ambiguous field with explicit eligibility, requested, and active fields;
old stores are rejected and require an operator reset.

*Findings: resman-4pw.1, resman-4pw.6*

## Rule 3 — Resource policy lists have one shared, tested contract **[checkable]**

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
- `PROCESS_EXCLUDE_LIST` defines the enforceable process set for every resource.
  Excluded processes **MUST** remain visible in total-user observation, but **MUST NOT**
	contribute to CPU, RAM, or I/O decision inputs because enforcement deliberately
	leaves them outside limited cgroups. Accounting and cgroup placement **MUST** consume
	the same normalized process-policy result. The policy identity is the basename resolved
	from `/proc/PID/exe`; `/proc/PID/comm` is display-only because the process can rewrite
	it. If executable identity is unavailable, the process **MUST** remain enforceable and
	the failure **MUST** be reported explicitly. PID-decorated display names and
	user-controlled `argv[0]` values are not policy identities.
- While any resource limit remains observed as active, every control cycle **MUST**
  reconcile process membership once per limited user. Newly enforceable processes move
  into the user's current shared or standalone cgroup. Processes that become excluded
  move back only to an origin captured for the same PID start time. If that origin is
  unavailable, reconciliation fails visibly and leaves the process constrained; it
  **MUST NOT** guess an untracked destination or clear the user's active state.

**Why.** Before `resman-4pw.1`, the control cycle aggregated RAM and I/O usage inside
the CPU-eligibility branch. An empty CPU include list therefore selected nobody for
RAM and I/O decisions even though their own empty include lists select everybody.
Independent aggregates and a table-driven policy test now preserve the intended
asymmetry. Process accounting previously included excluded processes even though
cgroup placement omitted them, allowing unenforceable workload to trigger or prolong
limits.

Initial placement previously ran only during activation or re-add. A new login or
service process created while the limit stayed active could therefore remain outside
the controlled cgroup indefinitely, and a reload that excluded a process left it
constrained under stale policy. Bounded per-cycle reconciliation now makes membership
eventually consistent and uses captured start times to avoid acting on reused PIDs.

*Findings: resman-4pw.1, resman-4pw.2, resman-4pw.3*

## Rule 4 — Every configured decision dimension must be evaluated

If the configuration exposes N dimensions for a resource, the decision engine
**MUST** evaluate all N, or the unevaluated ones **MUST** be removed from the
configuration surface — removed and rejected, per Rule 1, never left inert.

- Each dimension needs explicit activation, maintenance, and release semantics.
- The rule combining dimensions (any-of, all-of, weighted) **MUST** be documented next
  to the code that implements it.
- "Disabled" values (`max`, `0`, `""`) **MUST** be handled per dimension. One dimension
  being disabled **MUST NOT** disable the others.

**Why.** The decision engine previously activated and released I/O limiting from
write bandwidth only. Read bandwidth and read/write operation limits were parsed,
validated, and applied to cgroups but could not trigger enforcement; setting write
bandwidth to `max` disabled the entire I/O decision. The engine now evaluates each
configured dimension independently with any-of activation and all-of release.

*Finding: resman-4pw.4*

## Rule 5 — No knob without effect **[checkable]**

Every public configuration key **MUST** have a runtime consumer.

- A key is "public" once it appears in a `config:"..."` struct tag, in
  `configFieldHandlers`, in `config/resman.conf.example`, in the man page, or in `docs/`.
- Adding a key **MUST** be done in the same change as the code that reads it. Landing
  the knob first and the behaviour later is forbidden — there is no way for an operator
  to tell the difference between "not implemented yet" and "not working".
- **Validating an inert key is worse than not having it.** Validation is an implicit
  promise that the value matters. A key that validates successfully while doing nothing
  **MUST NOT** exist.
- Removing a key is **immediate and breaking** (Rule 1): the handler is deleted, the
  key is rejected at load, and the documentation, example config, and man page are
  updated in the same change. No deprecation window, no alias, no silent ignore.

**Why.** Before `resman-4pw.12`, `CPU_QUOTA_LIMITED` and `RAM_QUOTA_LIMITED` were parsed
and validated with no runtime consumer. `METRICS_CACHE_FILE` and `PROMETHEUS_FILE`
were parsed and unused. `PROMETHEUS_JWT_EXPIRY` was logged but did not cap token
validity. The remediation removed and explicitly rejects those keys, and JWT lifetime
now has one contract: the verifier requires the signed `exp` claim chosen by the token
issuer.

*Finding: resman-4pw.12*

---

# Part II — Contracts across boundaries

## Rule 6 — Typed contracts across boundaries, and metrics is not status **[checkable]**

Any structure shared between a producer and a consumer in different packages — metrics
snapshots, runtime status, MCP payloads — **MUST** be a typed DTO or a set of exported
constants referenced by both sides.

- String literals at both ends of a boundary are forbidden. The compiler cannot catch
  a typo, and the failure mode is a silent zero rather than an error.
- **The metrics map and the runtime status map are two different contracts.** They
  carry different keys, are produced by different components, and **MUST NOT** be
  interchangeable at a call site. `state/control_cycle.go:500` publishes the *metrics*
  snapshot; `state/manager.go:283` publishes *runtime status*. A helper that takes
  "a map" and a string is how a consumer ends up reading the right key from the wrong
  map. Give each its own type.
- Counts that mean different things get **different names**: users observed is not
  users eligible is not users actively limited. Do not reuse one key for whichever the
  caller happened to want.
- A key that does not exist is **removed from the consumer**, never aliased into
  existence (Rule 1).
- Producer and consumer **MUST** share a test that round-trips the contract, covering
  both maps.

**Why.** MCP formerly read `total_user_cpu_usage`, a key that had no producer, and
reported a hardcoded zero on every status surface. Neighbouring
`active_users_count` lookups mixed an observation snapshot with runtime status. The
fix replaced both string-keyed maps at this boundary with distinct typed contracts;
observed, CPU-eligible, and actively-limited user counts now have separate names.

*Finding: resman-4pw.6*

## Rule 7 — A counter counts what its name says, after it happened

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

**Why.** Before `resman-4pw.10`, activation and deactivation counters were incremented
before any cgroup operation, while the duration recorders had no production call sites
and `RecordError` initially had none. The remediation observes complete cycles and
collection boundaries, records bounded operational failures, and increments transition
counters only when runtime state actually changes.

*Finding: resman-4pw.10*

## Rule 8 — Never log a success you did not verify

- A function that writes to an external resource (database, cgroup, filesystem,
  network) **MUST** return `error`. A `func (...)` with no error return at such a
  boundary is a design defect.
- The caller owns the success/failure log. A callee **MUST NOT** log a failure and
  return silently, leaving the caller to announce success.
- `Debug` is not a level for failures. Operator-visible failures are `Warn` or `Error`,
  and **SHOULD** also increment an error metric.
- Retry logic **MUST NOT** hide the first failure from observability.

**Why.** Before `resman-4pw.11`, `Collector.WriteMetricsToDatabase` returned nothing
and logged a failed batch write at `Debug`; its caller then logged
`"Metrics written to database"` unconditionally. The remediation propagates a
contextual error to the control cycle, which emits a warning and a bounded Prometheus
error series while leaving the failed batch eligible for retry.

*Finding: resman-4pw.11*

## Rule 9 — Configuration lifecycle is declared in one place

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

**Why.** Before `resman-4pw.9`, `preserveRestartRequiredConfig` was a hand-written
list that omitted `METRICS_DB_*` and `CREATED_CGROUPS_FILE`, while
`USERNAME_CACHE_TTL` was applied only on a database-enabled bootstrap path. The
remediation moved every public key into `config/lifecycle.go`, rejects static changes
explicitly, and uses one configuration epoch across control-cycle consumers.

*Finding: resman-4pw.9*

## Rule 10 — Acknowledge, never sleep

- `time.Sleep` **MUST NOT** be used as synchronisation **[checkable]**. Not to wait for
  a reload, not to wait for a watcher, not to "give it a moment" in production code.
- An operation that triggers asynchronous work **MUST** return either a confirmed
  result or an explicit timeout/failure — never an optimistic success.
- A parameter that claims to control whether runtime state changes (`reload=false`)
  **MUST** actually control it, or be **removed immediately** as a breaking change
  (Rule 1). A parameter kept for compatibility while meaning nothing is forbidden.
- Persisted configuration and the published runtime snapshot are separate concepts;
  writing one **MUST NOT** implicitly publish the other.

**Why.** MCP filter setters previously slept one second while the config watcher
debounced for two, so `reload=true` reported success before the reload could possibly
have completed. And `reload=false` mutated the shared `Config` and persisted it anyway,
so the flag meant nothing in either position. The setters now persist a detached
snapshot and wait for a concrete watcher result; the obsolete flag is rejected.

*Finding: resman-4pw.7*

## Rule 11 — MCP is latest-only and protocol-stateless

The supported MCP contract is **`github.com/modelcontextprotocol/go-sdk` v1.7.0 or
newer**, serving **only** protocol revision **2026-07-28**, over both HTTP and stdio.

- Streamable HTTP **MUST** be constructed with `StreamableHTTPOptions.Stateless=true`.
  Default options are not acceptable — they are what the repository ships today.
- The server **MUST NOT** hold protocol or client-session state: no server-side
  `Mcp-Session-Id` storage, no sticky routing, no per-connection negotiation memory.
  Two interchangeable server instances **MUST** serve an authenticated request
  identically.
- **Protocol-stateless is not application-stateless.** The resource manager's own state
  — active users, cgroup membership, configuration — remains shared and authoritative.
  This rule constrains the transport, not the domain.
- The SDK can still accept older revisions; a **latest-only boundary MUST be enforced
  explicitly** in resman. Pre-2026-07-28 revisions, `initialize`/`initialized` legacy
  flows, and legacy session identifiers are **rejected**, not tolerated (Rule 1).
- `MCPGODEBUG` compatibility flags, protocol aliases, and fallback modes **MUST NOT**
  appear in the tree **[checkable]**.
- Authentication and authorisation stay **per-request middleware**. Anything that has
  to be remembered between requests to authorise the next one is a violation.
- Discovery, per-request metadata, headers, request bodies, and cancellation **MUST**
  follow the 2026-07-28 specification, with conformance tests over both transports.

**Why.** Before `resman-4pw.18`, `go.mod` pinned v1.6.1 and the HTTP transport was
built with default options, so session behaviour followed the SDK defaults and older
revisions remained negotiable. The remediation pins v1.7.0, enables stateless HTTP
explicitly, and imposes the single supported revision at the ResMan boundary.

*Finding: resman-4pw.18. References:
[go-sdk v1.7.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0),
[MCP 2026-07-28 changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog)*

---

# Part III — Environment, packaging, and on-disk safety

## Rule 12 — Require only the capabilities the enabled features need

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
- A supported container deployment **MUST** prove the same capabilities against host
  processes and the host cgroup hierarchy. Image build success, container UID 0, or a
  listed Linux capability alone is not evidence that host PID/cgroup namespaces, NSS,
  `/proc/PID/exe`, `/proc/PID/io`, and cgroup writes compose correctly.
- A privilege-dependent observation used for a security or enforcement decision **MUST**
  fail explicitly and conservatively. It **MUST NOT** downgrade to a process-controlled
  identity or a zero-valued decision signal when access is denied.
- Coverage over a dynamic process set **MUST** travel with the aggregate. Available
  non-negative counters may prove that an activation threshold is exceeded, but an
  incomplete aggregate **MUST NOT** prove below-threshold pressure or authorize release.
  Process-exit `ENOENT` races are not persistent capability failures and do not alert.

**Why.** `cgroup/manager.go:174` returns a fatal error when `+cpuset` cannot be written,
while `cgroup/manager.go:243-246` treats the identical write as best-effort and
continues. `packaging/systemd/resman.service:17` uses `sh -ec`, so the unit dies if
`+cpuset` fails. Delegated or containerised cgroup v2 hierarchies that expose `cpu` but
not `cpuset` cannot run resman, for no functional reason.

The shipped container once compiled with `CGO_ENABLED=0`, ran as an unprivileged user,
and documented neither the host PID/cgroup namespaces nor NSS mounts. It could build and
start while being unable to resolve host users, trust foreign-user process identity, or
enforce host cgroups. The supported rootful Podman contract is now exercised inside
SmolVM instead of being inferred from the image manifest.

The collector also previously converted every `/proc/PID/io` read failure into zero.
On a host without foreign-process ptrace access this disabled I/O activation
indefinitely and silently. Procfs coverage now remains explicit through aggregation:
partial rates are lower bounds that may prove activation, but cannot prove safe release.

*Findings: resman-4pw.14, resman-4pw.23, resman-4pw.46*

## Rule 13 — Shipped operational assets track runtime defaults **[checkable]**

Anything an operator can copy and run — scrape configs, alert rules, dashboards, TLS
scripts, Dockerfiles, unit files, the man page — is part of the product.

- A change to a default (port, namespace, path, metric name) **MUST** update every
  shipped asset in the same change.
- Defaults **SHOULD** be derived from a single documented source rather than retyped.
- Shipped YAML **SHOULD** be validated by a reproducible check (`promtool check rules`,
  `promtool check config`).
- Renames **MUST** be swept across `docs/`, `packaging/`, `scripts/`, `README.md`, and
  `CONTRIBUTING.md`, not just the file that prompted the rename. Historical changelog
  entries are the one exception: they record what was true then and stay untouched.

**Why.** Before `resman-4pw.13`, the project had been renamed from cpu-manager to
resman and the exporter port moved to 1974, while `cpu_manager_*` metric names still
appeared in `docs/alerting-rules.yml`, `docs/prometheus-queries.md`, the Grafana guides
and `docs/TECHNICAL-SPECIFICATION.md`, and `9100/9101` still appeared in
`docs/prometheus.yml`, `docs/generate-tls-certs.sh`, `docs/TLS-CONFIGURATION.md`,
`CONTRIBUTING.md` and — worst, because it was shipped rather than documentation —
`packaging/docker/Dockerfile` (`EXPOSE 9101`). Copying any of them produced a monitoring
setup that scraped nothing.

The sweep also exposed what a rename hides: three of the shipped alert expressions had
never been able to fire. Two named metrics that no version of the exporter ever
published, and one tested `changes(...) == -1`, which is unsatisfiable because
`changes()` is never negative. A stale asset is not only stale — it is unverified.

`resman-4pw.48` applies the same rule to filesystem layout: runtime defaults live in
authoritative constants, package payloads assert exact paths and modes, and a
repository-wide check permits legacy paths only at explicit startup-rejection and
operator-recovery boundaries.

*Findings: resman-4pw.13, resman-4pw.48*

## Rule 14 — Files written by resman are as restrictive as what they contain

- Configuration can contain secrets (`MCP_AUTH_TOKEN`, password and JWT secret file
  paths). Any file derived from it — temporary file, backup, export — **MUST NOT** be
  more permissive than its source.
- Atomic replacement **MUST** inspect the original path without following symbolic
  links, reject a link explicitly, and create the replacement with the regular file's
  mode and owner; when no prior file exists, default to a restrictive mode, never
  `0644`.
- Ownership preservation is fail-closed. Its error **MUST** name the required owner and
  tell the operator how to make the replacement possible without weakening permissions.
- Backup retention **MUST** be bounded or explicitly disabled. Unbounded timestamped
  backups turn one leak into a permanent archive of leaks.
- Failure paths **MUST NOT** leave readable residue containing secrets.
- If an atomic rollback cannot restore a known readable state, persistence **MUST**
  enter an explicit unusable state and reject later writes until operator recovery and
  restart. Logging possible disk/runtime divergence without enforcing that boundary is
  not recovery.
- Durability (`fsync` of file and parent directory) **MUST** be a documented, tested
  decision, not an accident of ordering.

**Why.** Before `resman-4pw.5`, `config/config.go` wrote both the timestamped backup
and the temporary file with mode `0644` before renaming, with no inspection of the
original file's permissions and no retention limit. A config readable only by root
produced world-readable copies of itself, one per change, forever. The remediation
uses one rolling backup, preserves source metadata, defaults new files to `0600`, and
syncs both file data and the parent directory. `resman-4pw.26` rejects managed
configuration symlinks, makes ownership failures actionable, and makes any remaining
runtime/disk divergence explicit while blocking later writes until recovery.
The packaged configuration is installed as root-owned `0600` below a `0700`
configuration directory, so the source whose metadata is preserved is restrictive
before it can acquire an MCP token.

*Findings: resman-4pw.5, resman-4pw.26, resman-4pw.48*

---

# Part IV — Engineering practice

## Rule 15 — Lock discipline

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
each caused by a shared lock held across a call into another component. Configuration
persistence originally held the live configuration write lock across directory scans
and durability syncs, blocking control-cycle getters behind storage latency. A later
detached snapshot removed that hot-path lock but left file transactions unserialized
across reload epochs; `resman-4pw.21` separates the lock and transaction contracts.
These defects are cheap to prevent and expensive to find.

## Rule 16 — Tests encode the contract, not the implementation

- Every semantic fix **MUST** ship a table-driven test that would have failed before it.
  "Verified manually" does not close a behavioural issue.
- The test **MUST** cover the **boundary the change crosses**, not only the unit it
  edits. See Rule 18: a commit can add hundreds of lines of correct unit tests and still
  ship a regression across the contract it moved.
- Empty/zero/disabled inputs (`""`, `0`, `max`, empty list) **MUST** be explicit rows in
  the table, not implicit paths.
- Cross-package contracts — eligibility, membership reconciliation, reload
  acknowledgement, MCP status, Prometheus outcomes, database failure — belong in the
  **functional harness** (`resman-4pw.16.1`): a disposable SmolVM guest running systemd
  as PID 1 with writable cgroup v2, isolated config, database, ports, and artifacts.
  Every cgroup mutation stays inside the guest.
- A failed environment preflight (`smolvm --version`, `/dev/kvm` readable and writable,
  cgroup v2 and the required controllers present in the guest) is a **skip or a blocked
  result — never evidence that resman passed**. Host capability requirements are
  explicit skips, never silent passes. Invoke KVM-dependent commands through `sg kvm`
  when the login shell does not yet expose the group; group membership alone cannot
  substitute for a missing `/dev/kvm`.
- Functional evidence **MUST** record the SmolVM version, guest image identity, kernel,
  cgroup mount and controllers, CPU/RAM allocation, and the exact command run.
- New or modified packages **SHOULD** move coverage up, never down. Current floors, for
  reference (2026-08-21): `internal/app` 16.1%, `mcp` 29.6%, `cgroup` 42.7%,
  `config` 55.7%, `metrics` 57.2%, `database` 62.1%, `logging` 62.9%, `state` 66.6%,
  `reloader` 91.9%.

*Findings: resman-4pw.16, resman-4pw.16.1, resman-4pw.16.2*

## Rule 17 — One language: English

- New comments, identifiers, log messages, and documentation **MUST** be in English.
- When you touch a function whose comments are in Italian, translate that function's
  comments as part of the change, and give touched exported declarations godoc-form
  English comments. Translation **MUST NOT** change behaviour.
- Existing non-English documents are a transitional exception: a small corrective edit
  **MUST** follow the document's current language, while adding a new section or
  materially extending its technical or operator contract **MUST** translate the whole
  document to English in the same change. A document **MUST NOT** be left with mixed
  languages.
- This is incremental cleanup, tracked as `resman-4pw.19`. Do not open mass-translation
  pull requests over untouched files, and leave historical changelog entries as they
  are.

**Why.** Several files (`state/control_cycle.go`, `config/config.go`) alternate between
Italian and English within a few lines, which makes grep-based review unreliable and
raises the cost of every external contribution.

*Finding: resman-4pw.19*

## Rule 18 — Fixing a defect

A fix is a change of contract. Treat it with the suspicion you would give a feature.

**18.1 — The regression test belongs to the boundary the fix crossed.**
If the change touches a function signature, an error-propagation path, a unit of
measure, a timing source, or the shape of persisted data, the test **MUST** exercise the
full path across that boundary — not the unit that was edited. Unit tests around the
changed code are necessary and not sufficient.

**18.2 — A constant borrowed from an unrelated contract is a defect.**
A threshold **MUST** derive from the contract it governs. A sampling staleness window
comes from the sampling cadence; a cache expiry comes from the cache. Reusing a
convenient neighbouring value couples two contracts that will diverge, and the coupling
is invisible at the call site.

**18.3 — Enforcement MUST NOT exceed the contract.**
Do not harden a promise the code does not keep. Do not validate a key that has no
runtime consumer, do not make a controller mandatory that enforcement does not use, and
do not tighten a check whose underlying behaviour is unimplemented. If tightening looks
right, implement the contract first — the tightening is the second commit, not the
first. Corollary: a correct hardening can surface a latent defect elsewhere, so a change
that makes a rule stricter **MUST** be checked against every consumer of the value it
constrains.

**18.4 — The third local remedy on one mechanism is a design signal.**
When you are about to extend a hand-maintained list, patch another consumer of a
stringly-typed contract, or repair one more key of a family, stop and replace the
mechanism instead. Two rounds of local repair on the same structure is the point at
which the structure is the defect.

**Why.** Commit `a52c77b` closed two `resman-ne0` findings and introduced two of this
epic's: it moved persistence to `WriteMetricsBatch` and changed error propagation to the
control cycle without a test on that boundary (`resman-4pw.11`), and it took
`2 × MetricsCacheTTL` as the staleness window for a new jiffy baseline
(`resman-4pw.15`). The commit carried roughly 255 lines of new unit tests and satisfied
Rule 16 as originally written. Separately, `ad6c0a0` strengthened validation of the
inert `CPU_QUOTA_LIMITED`, `ca2af8d` shipped a fatal `ExecStartPre` for the optional
`cpuset` controller, and `404e9f4` — a correct fail-safe change — exposed the latent
CPU-to-RAM/IO coupling behind `resman-4pw.1`. And `preserveRestartRequiredConfig` was
introduced by `04b7419`, extended by `76529bb`, and is now up for replacement in
`resman-4pw.9`.

*Findings: resman-4pw.11, resman-4pw.15, resman-4pw.12, resman-4pw.14, resman-4pw.9,
resman-4pw.6; historical provenance from epic `resman-ne0`*

## Rule 19 — An anomaly is tracked or refuted, never documented

Most rules address the author of a change. This one addresses whoever reviews it.

**Every anomaly noticed during a review MUST end in one of three states:**

1. **Fixed** in the change under review.
2. **Filed** as an issue, with reproduction steps.
3. **Refuted** in writing, with the reason.

Writing it into a note, a code comment, or a document is **not** one of the three
states. A written note feels resolved, which is exactly why it is dangerous: it buys
the feeling of having handled something while leaving nothing that will ever force the
work.

- A note on an issue is admissible **only** when that issue is **open** and its
  acceptance criteria force the anomaly to be handled. A note on a closed issue is
  deferred forgetting with a timestamp.
- A refutation **MUST** say why the condition cannot occur. "Seems minor", "narrow",
  and "unlikely in practice" are estimates, not refutations. If the reason is a
  probability rather than a mechanism, the anomaly is filed, not refuted.
- **Follow the anomaly to its consequence before classifying it.** Ask what the failure
  disables, for how long, and who sees it. Severity is a property of the consequence,
  not of the symptom, and the two are routinely an order of magnitude apart.
- A review **MUST** close by stating, for each anomaly, which of the three states it
  reached. A review that raises nothing says so explicitly.

**Why.** The `resman-ne0` epic was closed issue by issue and the defect class survived;
the historical provenance recorded on eleven issues of `resman-4pw` is the evidence.
The rule exists because the same thing happened again during this epic, in miniature:
in the review of `resman-4pw.2` a permanently repeating error was classified as a
documentation gap, and one follow-up question showed it aborts the control cycle at
stage 6 of 11, disabling history recording, I/O remediation, pattern detection and PSI
boost reversion for as long as it lasts. It became `resman-4pw.31` only because someone
asked for clarification. The reviewer had already traced that pipeline behaviour in an
earlier review. Nothing was unknown; the anomaly simply was not followed to its
consequence.

*Findings: resman-4pw.31, resman-4pw.32, resman-4pw.33; historical provenance from
epic `resman-ne0`*

## Rule 20 — Observation cadence does not advance decision state

Metrics collection has two temporal contracts: observation keeps dashboards and MCP
current; decision sampling advances the baselines and smoothing used to apply or
release limits. They are not interchangeable.

- Observation-only refreshes and reads **MUST NOT** advance decision baselines, EMA,
  threshold trackers, cool-down state, or any other temporal enforcement input.
- Each sampling purpose **MUST** own its cache key and its complete temporal state. A
  sample cached for observation **MUST NOT** satisfy a decision read, and neither
  stream may share per-process baselines or smoothing state with the other.
- A control decision **MUST** consume one authoritative decision sample. It **MUST NOT**
  re-read the collector partway through the decision or stability evaluation.
- Tests **MUST** interleave zero, one, and multiple observation refreshes between equal
  decision samples and prove that the resulting decision input and outcome are equal.

**Why.** `resman-ne0.30` serialized concurrent process scans and stopped duplicate EMA
updates at cache expiry, but it left observation refresh and control cycles on the same
per-process baselines, fixed-alpha EMA, and cache entry. `METRICS_REFRESH_INTERVAL`
could therefore change enforcement while promising only fresher telemetry. The
contract chosen in `resman-4pw.8` gives observation and decision two complete temporal
streams and makes the control-cycle sample authoritative through the entire decision.

*Finding: resman-4pw.8; historical provenance from resman-ne0.30*

## Rule 21 — Difference monotonic counters before aggregating dynamic identities

A cumulative counter is monotonic only for the identity that owns it. Process sets,
user sessions, and cgroup membership are dynamic and their aggregate is not monotonic.

- Rates over process counters **MUST** be calculated from deltas for the same PID and
  process start time, then aggregated. Differencing two per-user sums is forbidden.
- A counter reset **MUST** zero only the affected identity and dimension. It **MUST NOT**
  discard valid deltas from the user's other processes.
- PID reuse **MUST** establish a new baseline. A reused numeric PID cannot inherit the
  previous process's cumulative counters.
- Baselines for disappeared identities **MUST** be pruned after each completed scan and
  by bounded stale-state cleanup.
- An unavailable counter sample **MUST NOT** advance or preserve a baseline in a way
  that turns a later multi-interval delta into a one-interval rate.
- When enforcement moves a workload between kernel accounting identities, raw source
  and destination counters **MUST NOT** be differenced. The transition must either
  carry the final source delta into a logical cumulative counter with the destination's
  initial value as its new baseline, or report the interval as unavailable. It **MUST
  NOT** publish an artificial spike or zero-rate window.
- Tests for a movable accounting source **MUST** cover both directions of the placement
  transition while traffic continues.

**Why.** Before `resman-4pw.30`, resman summed cumulative `/proc/PID/io` counters per
user and then differenced consecutive user sums. When any process exited, its lifetime
counters disappeared from the sum; the aggregate fell and the monotonic guard returned
zero for the entire user even while surviving processes continued sustained I/O. The
remediation tracks PID plus start time, sums only per-process non-negative deltas, and
lets one process reset or disappear without erasing its peers' traffic. `resman-4pw.29`
applies the same rule to `io.stat`: observation and CPU-enforcement cgroups are distinct
kernel identities, so their raw counters are joined through an explicit logical ledger
rather than treated as one counter by name.

*Findings: resman-4pw.29, resman-4pw.30*

---

## Definition of Done

A change is not done until every line is true:

- [ ] `make fmt`, `make lint`, `make test`, `go vet ./...`, `go test -race ./...` pass.
- [ ] Nothing was kept for compatibility; anything removed is rejected loudly, not
      ignored (Rule 1).
- [ ] No field carries more than one of eligibility / intent / observation (Rule 2).
- [ ] Per-resource policy lists used for their own resource; empty-list behaviour tested
      (Rule 3).
- [ ] Every configured dimension of a touched decision is evaluated (Rule 4).
- [ ] Every configuration key added has a runtime consumer, in this change (Rule 5).
- [ ] No new cross-package string-literal keys; metrics and status contracts kept
      distinct (Rule 6).
- [ ] Counters increment after success; every declared metric has a call site (Rule 7).
- [ ] External writes return errors; no unverified success log; failures above `Debug`
      (Rule 8).
- [ ] New config fields classified dynamic or restart-required in the authoritative
      place (Rule 9).
- [ ] No `time.Sleep` used as synchronisation (Rule 10).
- [ ] MCP stays latest-only and protocol-stateless; no session state, no fallback
      (Rule 11).
- [ ] Capability requirements match enabled features, and packaging agrees (Rule 12).
- [ ] Shipped assets updated for any changed default, name, path, or port (Rule 13).
- [ ] Files written by resman are no more permissive than their source (Rule 14).
- [ ] Shared-state changes validated under `-race` (Rule 15).
- [ ] Table-driven test that fails without the change; functional evidence recorded if
      the harness was used (Rule 16).
- [ ] Comments, identifiers, logs, and documentation follow the language policy (Rule 17).
- [ ] If this is a fix: the boundary it crosses is tested, no threshold borrowed from an
      unrelated contract, no enforcement added ahead of the behaviour, and no third
      local remedy where the mechanism should be replaced (Rule 18).
- [ ] Every anomaly raised in review is fixed, filed, or refuted in writing — none left
      in a note (Rule 19).
- [ ] Observation refreshes cannot advance or populate decision temporal state, and
      each decision consumes one authoritative sample (Rule 20).
- [ ] Rates from cumulative counters are differenced at their stable identity before
      aggregation; resets, reuse, and disappearance are isolated (Rule 21).
- [ ] Any operator-visible discontinuity introduced by this change has its entry in the
      upgrade notes, in this commit (Rule 1, `resman-4pw.32`).

---

## Mechanical checks

Rules marked **[checkable]** are meant to be enforced by tooling rather than by
reviewer memory. These checks are **not implemented yet**; they are tracked as
`resman-4pw.17.1`, with pull-request wiring in `resman-4pw.17.2`.

Planned as `make verify-contracts`:

| Check | Rule | Approach |
|---|---|---|
| Every `config:"X"` key has a consumer outside `config/`, and unknown keys are rejected at load | 1, 5 | reflect over struct tags, grep field usage per package; assert `setConfigField` errors on unknown keys |
| No cross-package string-literal map keys for known contracts | 6 | grep the known key set outside the constants file |
| Every registered Prometheus metric has a production call site | 7 | AST scan of `metrics/prometheus.go` recorders vs callers |
| No `time.Sleep` in non-test files outside allowed backoff sites | 10 | AST scan with an explicit allowlist |
| No `MCPGODEBUG`, `Mcp-Session-Id` storage, or pre-2026-07-28 revision strings | 11 | grep the tree, allowlist the rejection sites themselves |
| Shipped assets contain no stale port/namespace | 13 | grep `9100\|9101\|cpu_manager\|cpu-manager` in `docs/`, `packaging/`, `scripts/`, excluding changelogs |
| `promtool check rules` / `check config` on shipped YAML | 13 | invoke promtool when available, skip with a warning otherwise |

Until they exist, treat them as review checkpoints.

---

## Appendix — Rule to finding traceability

| Rule | Audit finding |
|---|---|
| 1. No backward compatibility | epic `resman-4pw` policy; `resman-4pw.7`, `resman-4pw.12` |
| 2. Eligibility / intent / observation | `resman-4pw.1`, `resman-4pw.6` |
| 3. Resource policy lists | `resman-4pw.1` |
| 4. All decision dimensions evaluated | `resman-4pw.4` |
| 5. No knob without effect | `resman-4pw.12` |
| 6. Typed contracts; metrics ≠ status | `resman-4pw.6` |
| 7. Counter semantics | `resman-4pw.10` |
| 8. Truthful errors and logs | `resman-4pw.11` |
| 9. Configuration lifecycle | `resman-4pw.9` |
| 10. Acknowledge, never sleep | `resman-4pw.7` |
| 11. MCP latest-only and stateless | `resman-4pw.18` |
| 12. Capability requirements | `resman-4pw.14`, `.23`, `.46` |
| 13. Shipped assets | `resman-4pw.13`, `.48` |
| 14. On-disk file permissions | `resman-4pw.5`, `.26`, `.48` |
| 15. Lock discipline | `resman-4pw.21`; prior race/deadlock fixes in `logging/`, `metrics/` |
| 16. Tests encode the contract | `resman-4pw.16`, `.16.1`, `.16.2` |
| 17. One language | `resman-4pw.19` |
| 18. Fixing a defect | `resman-4pw.11`, `.15`, `.12`, `.14`, `.9`, `.6`; epic `resman-ne0` provenance |
| 19. Anomalies tracked or refuted | `resman-4pw.31`, `.32`, `.33`; epic `resman-ne0` provenance |
| 20. Observation cadence is decision-neutral | `resman-4pw.8`; `resman-ne0.30` provenance |
| 21. Difference before aggregating | `resman-4pw.30` |

Finding `resman-4pw.15` chose `POLLING_INTERVAL` as the normal host-CPU baseline
contract. Its rule-level provenance is recorded under Rule 18.2 because the defect was
borrowing staleness from the unrelated metrics-cache contract.

---

## See also

- `CONTRIBUTING.md` — process: branching, commits, pull requests, releases
- `docs/ARCHITECTURE.md` — component structure and control flow
- `docs/TECHNICAL-SPECIFICATION.md` — detailed component and configuration reference
- `docs/CGROUP-V2-TECHNICAL.md` — cgroup v2 behaviour
- `docs/MCP-README.md` — MCP server configuration and transports
- `AGENTS.md` / `CLAUDE.md` — instructions for AI coding agents working on this project
