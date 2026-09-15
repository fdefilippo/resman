# Systemd Property Lease Recovery

ResMan changes systemd-owned resource properties only through the system bus and only
with `runtime=true`. Systemd represents those changes as files below
`/run/systemd/system.control`. The files can outlive both the daemon process and the
unit lifetime, so in-memory ownership is insufficient: after a crash, their names do
not distinguish a ResMan write from an operator's `systemctl set-property --runtime`.

ResMan therefore records ownership before mutation in:

```text
/var/lib/resman/systemd-property-leases.json
```

The containing directory is root-owned mode `0700`; the regular state file is
root-owned mode `0600`. It is a versioned, atomically replaced and crash-durable
journal. Each confirmed record contains the unit identity, normalized baseline and
applied values, operation phase, and SHA-256 fingerprints of the complete managed
`system.control` footprint. A write-ahead `applying` record necessarily contains the
expected paths with pending digests until systemd has created the files; confirmation
fills those digests before success is reported. This file is an ownership journal,
not a disposable cache. Removing it while a lease is active makes automatic ownership
recovery impossible.

For `IODeviceWeight`, the record also retains the canonical device path and the typed
BFQ or `io.cost` verification mechanism for every ResMan-owned tuple. This context is
part of the lease because the D-Bus `a(st)` value does not identify which kernel file
must contain the corresponding `major:minor` override. Recovery never infers the
mechanism from whichever weight file happens to exist.
`IODeviceWeight` is classified as the independent `ResourceIOWeight`; hard-I/O
`ResourceIO` restoration never includes it, and CPU release uses a selective property
restore whenever another active resource lease shares the user slice.
Systemd treats a non-empty `IODeviceWeight` array as an update of the named device
tuples, not as replacement of the complete array. When an owned selector shrinks,
the adapter therefore sends an empty reset followed by the desired replacement in
one ordered `SetUnitProperties` call. The durable lease and readback retain only the
logical replacement value; an interrupted or divergent mutation remains uncertain
and is never restored without compare-before-restore proof.
An owned transient capability probe is stopped before its journal and runtime
drop-in are reconciled. Its cgroup disappears with the unit, so no active baseline
must be retained; reconciling the now-inactive footprint avoids placing a full
daemon reload inside the active `io.cost` probe transaction.

## Resource-specific workload authority

CPU, memory, and I/O authority are evaluated independently. A finite CPU envelope on
`user.slice` and a relative weight on `user-UID.slice` safely cover every descendant,
including a rootless container. ResMan never acquires or migrates the container's
processes.

Memory and hard I/O limits are stricter. Before their first property mutation, ResMan verifies
that every process owned by the UID is below the authoritative `user-UID.slice` and
that every process already below it shares the host PID namespace. A UID split across
another parent is reported as partial `authority_split` coverage. A rootless-container
descendant is reported as refused `runtime_owned_descendant` coverage. In both cases
CPU scheduling may continue, but ResMan does not claim or apply a complete memory or
I/O policy. If a later decision sample observes that authority was lost, ResMan restores
only that resource's owned properties before publishing the refusal; CPU and the other
independently authorized resource remain untouched. Failure to inspect the process set
or the required controller also refuses that resource before mutation. If the
persistence-phase D-Bus discovery fails before process capture can start,
reconciliation reports the discovery failure without restoring an already applied
resource. A `/proc` capture failure remains an authority refusal and restores the
affected owned resource properties.

Each decision sample captures one process-authority inventory over one `/proc`
traversal. The read is temporally smeared across directory enumeration and per-PID
operations; it is frozen after capture, but it is not an atomic or instantaneous kernel
snapshot. CPU coverage and RAM/I/O authority are independent projections of that same
inventory. CPU-only samples read cgroup membership only for tracked UIDs and never
inspect PID namespaces. If RAM or I/O may be requested, the capture reads cgroup
membership for the full process population and the relevant PID namespaces. The
inventory is bound to the decision `SampleEpochID`, and each observation is bound to
its exact UID and unit identity. A topology change unrelated to a requested target does
not invalidate that target's observation. A missing, new, or recreated target still
cannot be authorized from an old inventory or from an empty observed process set.

Processes and runtime descendants that arrive after capture are classified by the next
successful decision sample, not by another scan after `Apply`. With polling, the normal
upper bound is approximately `POLLING_INTERVAL` plus cycle processing. With an active
PSI watcher, it is the first relevant event or, at latest,
`PSI_FALLBACK_INTERVAL` plus cycle processing. Failed reconciliation can extend either
delay, and a process that appears and disappears entirely between samples can remain
unobserved. During the interval, a new descendant inherits active `memory.high`,
`memory.max`, `memory.swap.max`, and `io.max` policy from its slice. Any OOM or OOM kill
that occurs before restoration is irreversible.

After capture, every resource path still performs the remaining persistence/accounting
work and decision construction before RAM/I/O application. `ACTIVATE_LIMITS` also
reconciles CPU Points inside `activateSystemdEnforcement`; `MAINTAIN_CURRENT_STATE`
enters resource reconciliation directly because the pipeline reconciled CPU Points
before metrics collection and inventory capture. D-Bus unit identity, topology, lease,
property readback, and effective-kernel verification remain live checks through
acknowledgement. Finite memory values are rounded down to the host page size before
they are written. The systemd readback remains exact, while kernel verification compares
the corresponding page-normalized value exposed by `memory.high`, `memory.max`, or
`memory.swap.max`.

This conservative rule avoids presenting a leaf-only memory or I/O limit as coverage
of a workload whose runtime owns a nested resource boundary. It does not provide a
per-container policy; authoritative whole-container management remains a separate
contract.

## Automatic startup recovery

Before accepting a new systemd property mutation, ResMan compares every journal record
with authoritative systemd state and with the files on disk:

- An exact active-unit match is reclaimed without rewriting the property.
- An exact match under a recreated unit identity is reported as an
  `orphaned_resman_footprint` and rebound durably to the new identity.
- An inactive recorded unit is inspected on disk without loading it. An exact
  footprint can be reverted and followed by one daemon reload before the record is
  removed. ResMan never calls `LoadUnit` to manufacture an identity for this purpose.
- A crash after `RevertUnitFiles` but before `Reload` is a benign recorded phase:
  ResMan decides from the on-disk footprint, completes the reload, verifies the
  baseline, and then removes the record. The pre-reload D-Bus `DropInPaths` and
  normalized property values are intentionally not consulted because systemd keeps
  them stale until the reload.
- Any missing, additional, or changed file, changed property, unsafe journal, or
  incompatible identity fails closed. The record is retained and no property is
  overwritten or reverted.

An unrecorded `50-<Property>.conf` is never adopted by filename. Conversely, an
operator who issues `systemctl set-property --runtime` with exactly the same value
while ResMan owns the property produces the same file content and digest. Linux and
systemd expose no provenance for that indistinguishable write, so ResMan legitimately
retains ownership and may later restore the recorded baseline. Operators must not
modify a leased property behind ResMan when they need their intent to survive release.

## Resolving a conflict

First stop ResMan so the evidence does not change while it is inspected:

```bash
systemctl stop resman
install -m 0600 -o root -g root \
  /var/lib/resman/systemd-property-leases.json \
  /root/resman-systemd-property-leases.recovery.json
systemctl show user-1000.slice \
  -p FragmentPath -p DropInPaths \
  -p CPUWeight -p CPUQuotaPerSecUSec -p CPUQuotaPeriodUSec \
  -p MemoryHigh -p MemoryMax -p MemorySwapMax -p IOWeight -p IODeviceWeight
find /etc/systemd/system /run/systemd/system \
  /etc/systemd/system.control /run/systemd/system.control \
  -path '*user-1000.slice*' -ls
```

Replace `user-1000.slice` with the unit named by the diagnostic. Compare every path
and value with the protected journal copy. If operator configuration is present or
ownership remains uncertain, preserve it and decide the desired values explicitly;
do not run `systemctl revert` as a diagnostic.

`systemctl revert UNIT` is destructive at unit scope: it can remove persistent
operator files below `/etc/systemd/system` as well as runtime `system.control`
drop-ins. It is safe here only after the operator has proved that every mutable file
for that exact unit is expendable and has retained any required configuration. After
that decision, recover all units still present in the journal, not just one, then run:

```bash
systemctl revert user-1000.slice
systemctl daemon-reload
systemctl show user-1000.slice -p DropInPaths -p CPUWeight
```

Verify every recorded unit and its kernel-visible resource values. Only when all
journal records have been resolved may the stopped daemon's journal be moved aside as
one file. Do not delete the whole journal to clear one unit while other leases remain.
Restart ResMan and confirm that no lease-store or external-conflict outcome remains.
