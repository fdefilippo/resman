# Early-boot qualification

This campaign proves, on disposable Oracle Linux guests, how the packaged unit
behaves on a cold boot now that the adapter talks to PID 1 over
`/run/systemd/private` (resman-318.2), and how the realtime admission
protection (resman-318.4) keeps Veritas Cluster Server realtime unless the
operator consents.

The packaged unit starts with `DefaultDependencies=no` before `sysinit.target`
and `vcs.service`. Through 1.39.2 the adapter needed the system bus, which is
ordered after `sysinit.target`, so the early start established nothing. On a
kernel with `CONFIG_RT_GROUP_SCHED=y`, establishing enforcement before
`vcs.service` also means that `had`'s own `sched_setscheduler(SCHED_FIFO)` fails
with `EPERM`, which is why the protection exists.

## What the campaign measures

One guest per platform boots the distribution kernel (never UEK) with the
unified cgroup v2 hierarchy, installs the package under test and a
`vcs.service` fixture. Like `had`, the fixture requests `SCHED_FIFO` itself, so
its unit declares no `CPUSchedulingPolicy`, and it records whether the kernel
granted the request. Each scenario is one cold boot:

| Scenario | Declaration | Required outcome |
| --- | --- | --- |
| `protected-refusal` | `systemd_native`, default `REALTIME_PROTECTED_UNITS` | ResMan fails with status 78, no restart, `failure_class=realtime_admission_protected` naming `vcs.service`; the fixture keeps `SCHED_FIFO`; the root does not delegate `cpu` |
| `protected-observation` | `auto`, default `REALTIME_PROTECTED_UNITS` | ResMan runs and observes with reason `realtime_admission_protected` for `vcs.service`; the fixture keeps `SCHED_FIFO`; the root does not delegate `cpu` |
| `consented-enforcement` | `systemd_native`, empty `REALTIME_PROTECTED_UNITS` | ResMan selects systemd-native enforcement on the first start and is ready before `sysinit.target` and before `dbus.socket`; the fixture's outcome agrees with the delegation systemd leaves at the root after the probe restores `user.slice`: `EPERM` when `cpu` stays delegated, `SCHED_FIFO` when systemd withdraws it |

The table applies to a kernel with realtime group scheduling. The validator pins
that build option per platform: the EL8 distribution kernel has
`CONFIG_RT_GROUP_SCHED=y`, while the EL9 (5.14) and EL10 (6.12) distribution
kernels are built without it. There the kernel never refuses a realtime request,
so every scenario must enforce on the first start, the guard must never publish
`realtime_admission_protected`, and the fixture must keep `SCHED_FIFO` whatever
the root delegates.

On every boot the journal must contain no system bus failure and
`sysinit.target` must be reached; a kernel with realtime group scheduling must
also report a limited `sched_rt_runtime_us`. Readiness before `sysinit.target` is the
measured proof that no transient probe unit waited for `sysinit.target`, which
waits for ResMan.

The guest records facts only. `validate_evidence.py` recomputes every outcome
and returns `QUALIFIED` only when all three scenarios pass on distinct boots
with the reviewed template digest and the package identified by its build
manifest. In the protected scenarios a root that delegates `cpu` makes the
fixture's outcome meaningless, so it fails the scenario rather than passing it.

## What is pinned

* Oracle Linux KVM templates, verified against their published SHA-256 digests:
  `OL8U10_x86_64-kvm-b287`, `OL9U8_x86_64-kvm-b293`, `OL10U1_x86_64-kvm-b291`.
* The distribution kernel, selected with `grubby` after installing `kernel`;
  EL8 also boots with `systemd.unified_cgroup_hierarchy=1`.
* systemd 239, 252 and 257 respectively, and the realtime group scheduling build
  option of each distribution kernel (EL8 only).
* The package by identity and SHA-256, bound to the revision and tree in its
  build manifest; only files under `test/functional/early-boot/` may differ
  between that revision and the qualification revision.

## Running it

```bash
make test-functional-early-boot-unit
make test-functional-early-boot-qemu \
    RESMAN_EBOOT_QEMU_HOST=root@terra RESMAN_EBOOT_PLATFORM=el8 \
    RESMAN_EBOOT_RPM=build/packages/resman-….el8.x86_64.rpm \
    RESMAN_EBOOT_RPM_MANIFEST=build/packages/resman-….el8.x86_64.manifest
```

The tracked worktree must be clean. Evidence lands in
`build/functional/early-boot/<platform>-<run id>/` with its own `SHA256SUMS` and
the computed verdict in `verdict.json`. `EBOOT_EXPECTED_VERDICT` accepts
`QUALIFIED` (default), `NOT_QUALIFIED` or `any`.

## Retained evidence

Three archives are retained, one per platform, all for the package
`resman-1.39.2-1.el8.dev318.x86_64` built in a disposable `oraclelinux:8`
container from revision `aa4f733` and qualified from revision `3205118`:

| Archive | Kernel | systemd | Realtime group scheduling | Root delegates cpu after an idle boot |
| --- | --- | --- | --- | --- |
| `oracle-el8-rhck-early-boot-r20261014032112-625540` | 4.18.0-553 | 239 | yes | no |
| `oracle-el9-rhck-early-boot-r20261014032754-626359` | 5.14.0-687 | 252 | no | no |
| `oracle-el10-rhck-early-boot-r20261014033414-627180` | 6.12.0-211 | 257 | no | yes |

All three are `QUALIFIED`. On EL8 the protected scenarios refuse with status 78
or observe with `realtime_admission_protected` while the fixture keeps
`SCHED_FIFO`, and the consented boot enforces before `sysinit.target` with the
fixture still realtime, because systemd withdraws `cpu` from the root once the
probe restores `user.slice`. On EL9 and EL10 every scenario enforces on the first
start and the fixture keeps `SCHED_FIFO`, because those kernels never refuse a
realtime request. `host_test.sh` checks that every archive stays intact, keeps its
verdict, and that the three platforms remain covered.
