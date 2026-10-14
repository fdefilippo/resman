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
| `consented-enforcement` | `systemd_native`, empty `REALTIME_PROTECTED_UNITS` | ResMan selects systemd-native enforcement on the first start, is ready before `sysinit.target` and before `dbus.socket`; the root delegates `cpu`; the fixture's realtime request fails with `EPERM`, the accepted trade-off |

On every boot the journal must contain no system bus failure, `sysinit.target`
must be reached, and the kernel must report `CONFIG_RT_GROUP_SCHED=y` with a
limited `sched_rt_runtime_us`. Readiness before `sysinit.target` is the
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
* systemd 239, 252 and 257 respectively.
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
