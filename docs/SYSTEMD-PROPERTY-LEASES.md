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
  -p MemoryHigh -p MemoryMax -p MemorySwapMax -p IOWeight
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
