# Weighted-I/O effect qualification

This harness is the retained controlled-contention campaign for
`resman-nq6.40.5`. It is intentionally separate from
`test/functional/io-device-weight`, whose historical evidence proves only the
systemd-to-kernel property path. This campaign installs an exact ResMan RPM in a
fresh Oracle Linux 9.8 QEMU guest booted with the exact retained RHCK, configures
the packaged daemon through its public files, and measures two competing sibling
user slices on run-owned disposable virtio devices.

The candidate provenance is
`resman-nq6.40.5-ol9-rhck-20260915`. It names Oracle Linux 9.8, systemd Manager
`252-67.0.1.el9_8.2` from `Manager.Version` over D-Bus, and RHCK
`5.14.0-687.46.1.el9_8.x86_64`. The campaign covers BFQ and io.cost separately.
It is not retained or published unless the complete archive passes validation and
independent review. This exact identity is evidence provenance, never a runtime authorization rule:
any host that passes the owned live probe remains usable as
`functionally_accepted` without the label.

## Assertions

For each mechanism the guest records three fixed-window direct-read phases:

- equal public weights `100:100`;
- unequal weights `100:1000`;
- a reversed control `1000:100`.

The guest labels each completed mechanism result `MEASURED`; it never assigns the
qualification verdict. The validator recomputes every share from raw `io.stat`
snapshots. Both slices
must perform nonzero I/O. In retained qualification evidence, equal delivery must
remain within `0.40..0.60`, and the favored slice must receive at least `0.80` of
measured bytes in both opposite `1:10` phases while winning every raw interval by
at least `1.5:1`. The non-retained smoke profile keeps the wider `0.30..0.70` and
`0.60` aggregate controls because its single short interval is only a gross activity
check, never qualification evidence. The validator also requires exact
D-Bus `IODeviceWeight` readback, exact per-device kernel entries, authority
transition from complete to partial, Prometheus and SQLite provenance, the three
`W`/`H` selector regions, restart recovery, blackout release, capability-loss
release, compare-before-restore preservation, package/source hashes, and exact
cleanup.

The first campaign must observe `effect_qualified=0` and provenance `none` from the
daemon. After independent acceptance, publication of a retained per-mechanism label is
verified separately; the evidence under evaluation cannot pre-authorize itself.

The harness may select BFQ or enable io.cost only on its two disposable virtual
devices. It records and restores their original scheduler and io.cost state.
The packaged daemon never performs those setup mutations. Missing KVM, the exact
RHCK package, or another campaign prerequisite produces `BLOCKED`; it cannot be
published as effect qualification.

The QEMU runner prepares the exact OL9.8/RHCK image once in an integrity-checked
cache and creates a fresh copy-on-write overlay for every run. It first executes
a non-retained smoke profile with one two-second interval per phase. Each phase
also records a two-second scheduler-settling interval after the exact kernel
weight appears and before delivery measurement begins; this prevents BFQ queue
history from being mistaken for the newly programmed ratio. Every major step
writes a named checkpoint. The retained three-by-ten-second qualification profile
starts only after the smoke bundle passes the same structural, transport,
lifecycle, composition, and delivery-direction checks. Interruptions invoke an
exact run-owned cleanup helper and verify that the domain and its three disks no
longer exist; the reusable prepared image is deliberately retained.
Before the retained profile starts in the same disposable guest, the runner
removes only the campaign-owned SQLite and log files. Retained preflight repeats
RPM verification and accepts only the two exact configuration-file timestamp
markers caused by smoke (or a fully clean result) after their bytes and modes were restored; any content,
permission, ownership, or other package drift fails. Retained assertions therefore
cannot reuse a smoke database row or diagnostic stream.
The smoke exercises `io.cost` first because its owned probe is the costliest
capability transition. Workloads are stopped only while each mechanism is selected
and probed, then resumed before every controlled-delivery interval. The retained
diagnostics include systemd events for the transient probe units.

## Execution

Run the validator and its mutation tests without KVM:

```sh
make test-functional-io-device-weight-effect-unit
```

The live runner requires a committed clean checkout and an exact RPM plus build
manifest:

```sh
RESMAN_IO_EFFECT_QEMU_HOST=root@terra \
RESMAN_EL9_RPM=build/packages/resman-1.38.0-8.el9.x86_64.rpm \
RESMAN_EL9_RPM_MANIFEST=build/packages/resman-1.38.0-8.el9.x86_64.manifest \
RESMAN_IO_EFFECT_MECHANISMS=bfq \
make test-functional-io-device-weight-effect-qemu
```

`RESMAN_IO_EFFECT_MECHANISMS` accepts `bfq`, `io_cost`, or both as a comma-separated
list. The guest may retain diagnostics for both mechanisms, but only the selected
mechanisms contribute qualification verdicts and future registry entries.

Output is written below `build/functional/io-device-weight-effect/RUN_ID`.
Only a bundle accepted by `validate_evidence.py` may be copied into `evidence/`.
The committed evidence manifest is revalidated by the unit target. Independent
review of the evidence and mutation protections is required before the Beads
child or parent epic closes.
