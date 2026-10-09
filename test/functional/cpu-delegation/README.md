# EL8 cpu-delegation reproduction

This campaign reproduces, on a disposable guest, the startup refusal observed on
the production EL8 nodes `vmgclalpr2095` and `vmgclalpr2098`, where
`resman-1.38.0-10.el8` fails with

```
reason=required_capability_unavailable detail="cpu.max is unavailable for user-resmancapprobe….slice"
```

and the unit stays failed with status 78.

## The hypothesis under test

On those nodes the unified cgroup v2 hierarchy is active and the root cgroup
publishes the `cpu` controller, yet `cgroup.subtree_control` never delegates it.
The kernel is built with `CONFIG_RT_GROUP_SCHED=y`, and Veritas Cluster Server
keeps its `had` and `hashadow` threads at SCHED_FIFO priority 97 inside
`system.slice/vcs.service`. A cgroup v2 `cpu` controller cannot distribute
realtime bandwidth, so the kernel refuses `+cpu` at the root with `EINVAL` while
any realtime task lives outside the root cgroup. No descendant can then expose
`cpu.max`, the mandatory CPU capability probe fails, and the service refuses to
start.

Nobody will stop a production cluster to confirm that chain, so it is confirmed
here instead — and confirmed in both directions.

## What the campaign measures

The guest probe records measurements only. Every typed outcome is recomputed by
`validate_evidence.py` from those records, so no conclusion rests on a verdict
written by the guest that produced the data.

| Step | Measurement |
| --- | --- |
| `platform` | distribution, kernel, `CONFIG_RT_GROUP_SCHED`, controllers, delegation set |
| `baseline-delegation` | `+cpu` at the root **before** any realtime task exists, then reverted |
| `realtime-fixture` | one owned SCHED_FIFO task placed in `system.slice/<unit>.service` |
| `delegation-under-realtime` | `+cpu` at the root with that task in place, with its exact errno |
| `daemon-under-realtime` | one start of the installed package: state, exit status, typed error |
| `declared-observation` | the same host under `ENFORCEMENT_MODE=observation_only` |
| `declared-auto` | the same host under `ENFORCEMENT_MODE=auto` |
| `restored-declaration` | the default declaration put back before the control step |
| `control-without-realtime` | the fixture removed: delegation and a second start of the same package |
| `residue` | service, fixture, delegation set, leases, drop-ins and probe slices after the run |

The control step is what makes this an experiment rather than a confirmation:
if removing the realtime task did not restore both the delegation and a clean
activation, the field hypothesis would be wrong, and the validator would compute
`NOT_REPRODUCED` instead.

The two declared modes are measured against the very condition that broke the
field hosts, with the realtime task still in place. The validator computes a
second verdict for them: `REMEDIED` only when both declarations activate the
service with `Result=success`, publish `observation_only` with their own reason —
`operator_requested_observation` for the declaration, `mandatory_capability_unavailable`
for `auto` — report no capability error as a failure, and leave no capability
probe slice behind, with the declaration under observation creating no probe unit
at all. The default declaration is restored before the control step.

The validator returns `REPRODUCED` only when all of the following hold: the
baseline is `DELEGABLE`, the fixture is `ESTABLISHED`, delegation under the
fixture is `NOT_DELEGABLE` with `EINVAL`, the daemon answers
`REFUSED_CONFIGURATION` with status 78 and the typed `cpu.max` error, and the
control restores both `DELEGABLE` and `STARTED`. A refusal measured without any
realtime task outside the root, a baseline measured with one, or a run leaving
residue behind is refused outright rather than reported.

## What is pinned

* Oracle Linux 8.10 KVM template `OL8U10_x86_64-kvm-b287.qcow2`, verified against
  its published SHA-256 digest.
* The distribution kernel, not UEK: the campaign installs `kernel`, selects the
  newest `kernel-core` with `grubby`, and asserts after the reboot that the
  running kernel is not UEK, is owned by `kernel-core-`, and carries
  `CONFIG_RT_GROUP_SCHED=y`. The field nodes run that same family.
* `systemd.unified_cgroup_hierarchy=1 psi=1`, systemd 239, and a boot distinct
  from the template default.
* The package under test, by identity and SHA-256, bound to the revision and
  tree recorded in its build manifest.

## Running it

```bash
make test-functional-cpu-delegation-unit        # harness contract, no VM
make test-functional-cpu-delegation-qemu \
    RESMAN_CPUDEL_QEMU_HOST=root@terra \
    RESMAN_EL8_RPM=build/packages/resman-1.38.0-10.el8.x86_64.rpm \
    RESMAN_EL8_RPM_MANIFEST=build/packages/resman-1.38.0-10.el8.x86_64.manifest
```

The tracked worktree must be clean, and the build-manifest revision must be an
ancestor of `HEAD` with no package input changed since; only files under
`test/functional/cpu-delegation/` may differ. Evidence lands in
`build/functional/cpu-delegation/<run id>/` with its own `SHA256SUMS` and the
computed verdict in `verdict.json`.

Set `CPUDEL_EXPECTED_VERDICT=NOT_REPRODUCED` to retain a falsifying run, or
`any` to validate an archive without asserting a verdict. `CPUDEL_EXPECTED_REMEDY`
does the same for the remedy and accepts `NOT_MEASURED` for a package that
predates `ENFORCEMENT_MODE`. Both default to the expected outcome, so a surprise
fails the run instead of passing quietly.

## Retained evidence

Two archives are retained, both on Oracle Linux 8.10 with
`4.18.0-553.171.1.el8_10.x86_64`, `CONFIG_RT_GROUP_SCHED=y` and systemd
239-82.el8_10.1:

* `oracle-ol8-rhck-cpu-delegation-r20261009134755-2978178/` proves the defect on
  `resman-1.38.0-10.el8.x86_64`, the exact identity installed on the field
  nodes. Verdict `REPRODUCED`; it predates `ENFORCEMENT_MODE`, so its remedy is
  `NOT_MEASURED` by construction.
* `oracle-ol8-rhck-cpu-delegation-remedy-r20261009144651-3046981/` proves the
  defect and its correction on `resman-1.39.0-1.el8.x86_64`. Verdict
  `REPRODUCED`, remedy `REMEDIED`: under `systemd_native` the same package still
  exits 78, while `observation_only` and `auto` both activate against the very
  same realtime task, each publishing its own reason.

The harness gate checks that every archive stays intact and keeps its verdict,
and that at least one of them proves the remedy.
Recomputing every typed outcome also needs the package itself, which is not
tracked:

```bash
python3 test/functional/cpu-delegation/validate_evidence.py \
    test/functional/cpu-delegation/evidence/oracle-ol8-rhck-cpu-delegation-r20261009134755-2978178/remote \
    "$(awk -F= '$1 == "qualification_revision" {print $2}' <archive>/remote/environment.txt)" \
    build/packages/resman-1.38.0-10.el8.x86_64.rpm \
    build/packages/resman-1.38.0-10.el8.x86_64.manifest --expect-remedy NOT_MEASURED
```

That first archive was produced by `resman-1.38.0-10.el8`, which predates
`ENFORCEMENT_MODE`, so its remedy is `NOT_MEASURED` by construction: it records
the defect, not its correction.

## Provenance of the package under test

The production nodes installed `resman-1.38.0-10.el8` on 16 September 2026. No
file under `internal/`, `state/`, `database/`, `packaging/`, `main.go` or the
`Makefile` changed between the revision current at that moment and the revision
this package is built from, so the enforcement behaviour is the same; the
difference is confined to documentation. The package is built in a disposable
`quay.io/rockylinux/rockylinux:8.10` container, mirroring the `rpm` job of
`.github/workflows/release.yml`, and its manifest records the builder image
digest and the Go toolchain actually used.
