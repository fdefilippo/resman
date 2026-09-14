# IODeviceWeight capability matrix

Characterization run `r20260914060753-2990457` exercised source revision
`91b9876dab9786192ea5c193d234345cdbde9163` on three independent QEMU/KVM
guests. The complete, manifest-covered evidence is retained in
[`evidence/oracle-el8-el9-el10-r20260914060753`](evidence/oracle-el8-el9-el10-r20260914060753).

| Platform | Representative | systemd | Kernel | BFQ | io.cost | Both active | Policy status |
|---|---|---|---|---|---|---|---|
| EL8 | Oracle Linux 8.10 | `systemd-239-82.0.13.el8_10.19.x86_64` | `5.15.0-320.202.8.5.el8uek.x86_64` | UNSUPPORTED | UNSUPPORTED | UNSUPPORTED | `not available` |
| EL9 | Oracle Linux 9.8 | `systemd-252-67.0.1.el9_8.2.x86_64` | `6.12.0-204.92.4.2.el9uek.x86_64` | SUPPORTED | UNSUPPORTED | UNSUPPORTED | `not available` |
| EL10 | Oracle Linux 10.1 | `systemd-257-23.0.1.el10_2.2.x86_64` | `6.12.0-202.76.4.4.el10uek.x86_64` | SUPPORTED | UNSUPPORTED | UNSUPPORTED | `not available` |

These results describe the tested representatives, not a lowest-common-denominator
product contract. `UNSUPPORTED` is conclusive evidence for that exact
platform-mechanism pair and does not constrain a later distribution. No row was
`BLOCKED`.

## Measured systemd-to-kernel path

All three systemd versions expose `IODeviceWeight` on
`org.freedesktop.systemd1.Slice` with signature `a(st)`. The probe called
`SetUnitProperties` with manager signature `sba(sv)`, runtime mode enabled, and
the exact tuple `("/dev/vda", 333)` for the run-owned device `251:0`. Every
guest read the tuple back over D-Bus and accepted an empty-array reset.

The property caused systemd to materialize the `io` controller through
`user.slice` on every representative. EL10 is the strongest materialization
case: `user.slice/cgroup.controllers` did not contain `io` before the request,
then contained it while the property was active.

With BFQ selected on the disposable virtio device:

- EL8 created `io.bfq.weight` but exposed only `default 100`; it did not write a
  `251:0` entry. This is a demonstrated systemd-to-kernel limitation, so the
  EL8/BFQ row is `UNSUPPORTED`.
- EL9 and EL10 exposed `default 100` plus `251:0 121`. The value `121` is the
  systemd BFQ conversion of requested weight `333`, so both BFQ rows are
  `SUPPORTED`.
- The empty-array reset removed the per-device entry and the transient file on
  all three guests. The original scheduler selection was restored exactly.

None of the three UEK representatives exposes root `io.cost.qos` and
`io.cost.model`. Their io.cost rows are therefore `UNSUPPORTED`; this is not a
qualification-infrastructure failure. Because no representative could activate
io.cost, the simultaneous BFQ-plus-io.cost case was unavailable. No precedence
or composition is inferred: a platform that exposes both mechanisms must remain
`mechanism_ambiguous` until it is characterized separately.

## Cleanup and provenance

Each platform evidence directory includes the pinned base-image URL and digest,
initial and qualified boot identities, systemd and kernel package identities,
block-device inventory, D-Bus introspection, command log, direct control-file
writes, before/during/after kernel files, and both a platform and matrix
`SHA256SUMS` manifest. EL8 additionally retains the initial legacy hierarchy,
BLS change, and distinct cgroup-v2 boot.

All three guest cleanups and all three QEMU cleanups are `PASS`. The final host
check found no remaining matrix VM, libvirt domain, run work directory, transient
unit drop-in, enabled io.cost state, or changed scheduler selection. The base
images remain as immutable digest-checked caches and are not test devices.
