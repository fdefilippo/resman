# IODeviceWeight capability matrix

The characterized release remains Oracle Linux in both tables below. RHCK is its
base kernel and UEK is an extension kernel available within the same release;
`OL9/RHCK` and `OL9/UEK`, for example, are two kernel configurations of Oracle
Linux 9 rather than two releases. Exact guest identities below are retained
transport characterization and regression fixtures, not a runtime allowlist or
proof of the effect under contention. ResMan authorizes weighted I/O from
observed capabilities and a successful owned live startup probe on every
configured device. Distribution, systemd and kernel identities remain diagnostic
provenance and never substitute for that proof.

## Runtime acceptance and effect qualification

The read-only classifier can report only a `probe_candidate`: the root exposes
the `io` controller and exactly one mechanism is active for the device. It does
not require `io.weight` or `io.bfq.weight` to exist before systemd materializes
the controller. The owned adapter probe then programs and resets a non-default
value and verifies the exact `MAJ:MIN` entry. A host that passes that complete
probe is `functionally_accepted`, regardless of its software identity.

`effect_qualified` is a narrower evidence label. It requires the packaged daemon
and public configuration to demonstrate the intended relative delivery under a
controlled sibling-slice contention workload in `resman-nq6.40.5`. It applies
only to the exact representative and mechanism exercised by that evidence. A
functionally accepted but not effect-qualified host is allowed to use the
feature, but documentation, release notes and runtime observability must say that
its delivery effect is not qualified. The tables below do not confer the
`effect_qualified` label because their probes did not run ResMan or a contention
workload.

## RHCK baseline characterization

Run `r20260914092100-3057235` exercised source revision
`a9d6c9608296b42e45ee390b756cefbb3f8a1de5`. The manifest-covered evidence is
retained in
[`evidence/oracle-rhck-el8-el9-el10-r20260914092100-3057235`](evidence/oracle-rhck-el8-el9-el10-r20260914092100-3057235).

| Tested pair | Representative | systemd | Kernel | BFQ | io.cost | Both active | Policy status |
|---|---|---|---|---|---|---|---|
| OL8/RHCK | Oracle Linux 8.10 | `systemd-239-82.0.13.el8_10.19.x86_64` | `4.18.0-553.162.1.el8_10.x86_64` | UNSUPPORTED | UNSUPPORTED | NOT_APPLICABLE | `not applicable` |
| OL9/RHCK | Oracle Linux 9.8 | `systemd-252-67.0.1.el9_8.2.x86_64` | `5.14.0-687.46.1.el9_8.x86_64` | SUPPORTED | SUPPORTED | SUPPORTED | `mechanism_ambiguous` |
| OL10/RHCK | Oracle Linux 10.1 | `systemd-257-23.0.1.el10_2.2.x86_64` | `6.12.0-211.53.1.el10_2.x86_64` | SUPPORTED | SUPPORTED | SUPPORTED | `mechanism_ambiguous` |

Each guest began from the pinned Oracle image, installed `kernel`, explicitly
selected the newest `kernel-core` BLS entry, rebooted and proved that the running
`/boot/vmlinuz-$(uname -r)` belonged to that RHCK package. All characterized boots
used cgroup v2.

All three systemd versions exposed `IODeviceWeight` as `a(st)`, accepted and
read back the exact `("/dev/vda", 333)` request, materialized the `io`
controller through `user.slice`, and accepted an empty-array reset.

- OL8/RHCK selected BFQ but produced no per-device `io.bfq.weight` entry; root
  `io.cost.qos` and `io.cost.model` were absent. Both individual mechanisms are
  therefore `UNSUPPORTED` for that line candidate.
- OL9/RHCK and OL10/RHCK wrote `252:0 121` through BFQ and `252:0 333` through
  io.cost when each mechanism was active individually. Both paths are
  `SUPPORTED`.
- OL9/RHCK and OL10/RHCK also exposed both footprints during the simultaneous
  phase. This proves availability, not precedence or safe composition. Product
  policy remains `mechanism_ambiguous` and must refuse enforcement when both are
  active on one configured device.

Every guest restored the scheduler selection, disabled io.cost, removed the
per-device property and transient unit state, and passed cleanup. The host
removed all matrix VMs, overlays, probe disks and the remote run bundle.

## Retained same-release UEK extension-kernel characterization

The earlier run `r20260914060753-2990457` exercised revision
`91b9876dab9786192ea5c193d234345cdbde9163`. Its independently reviewed,
manifest-covered evidence remains in
[`evidence/oracle-el8-el9-el10-r20260914060753`](evidence/oracle-el8-el9-el10-r20260914060753).

| Tested pair | Representative | systemd | Kernel | BFQ | io.cost | Both active | Policy status |
|---|---|---|---|---|---|---|---|
| OL8/UEK | Oracle Linux 8.10 | `systemd-239-82.0.13.el8_10.19.x86_64` | `5.15.0-320.202.8.5.el8uek.x86_64` | UNSUPPORTED | UNSUPPORTED | NOT_APPLICABLE | `not applicable` |
| OL9/UEK | Oracle Linux 9.8 | `systemd-252-67.0.1.el9_8.2.x86_64` | `6.12.0-204.92.4.2.el9uek.x86_64` | SUPPORTED | UNSUPPORTED | NOT_APPLICABLE | `not applicable` |
| OL10/UEK | Oracle Linux 10.1 | `systemd-257-23.0.1.el10_2.2.x86_64` | `6.12.0-202.76.4.4.el10uek.x86_64` | SUPPORTED | UNSUPPORTED | NOT_APPLICABLE | `not applicable` |

These rows characterize the UEK extension kernel on the same Oracle Linux
releases. They neither authorize nor exclude another runtime: every enabled
host is classified and probed from its observed capabilities.

### OL8/UEK direct BFQ attribution

Run `r20260916054742-3862268` exercised revision
`378ae544a893182b2712d45d71423e83c5dba21a` on the same Oracle Linux 8.10,
`systemd-239-82.0.13.el8_10.19.x86_64`, and
`kernel-uek-core-5.15.0-320.202.8.5.el8uek.x86_64` representative as the
retained UEK matrix. Its manifest-covered evidence is retained in
[`attribution-evidence/oracle-ol8-uek-direct-bfq-r20260916054742-3862268`](attribution-evidence/oracle-ol8-uek-direct-bfq-r20260916054742-3862268).

The isolated probe selected BFQ on the owned `251:0` virtio disk, enabled the
`io` controller for one owned child cgroup, and wrote `251:0 121` directly to
that cgroup's `io.bfq.weight`. The kernel read back the exact entry. The probe
then wrote `251:0 default`, confirmed the entry disappeared, removed the cgroup,
restored the root controller state and scheduler, and removed the VM and disks.
The running configuration records `CONFIG_BFQ_GROUP_IOSCHED=y` and
`CONFIG_BLK_CGROUP_IOCOST` not set.

This narrows the OL8/UEK result: the UEK kernel accepts the exact per-device BFQ
entry that systemd 239 accepted and read back over D-Bus but did not emit into
the target cgroup. The demonstrated limitation is therefore in the systemd 239
`IODeviceWeight` delivery path, not in that UEK kernel's BFQ per-device-weight
support. The complete OL8/UEK path remains `UNSUPPORTED`; direct kernel writes
are test-only and do not create a production fallback.

`UNSUPPORTED` is a valid technical result for its characterized line and does
not block a capable newer line. `BLOCKED` is reserved for missing or invalid
qualification infrastructure; neither retained matrix has a `BLOCKED` row.
`NOT_APPLICABLE` means an individual prerequisite was unsupported and no
simultaneous phase ran. PSI was not required, enabled or qualified by either
campaign.
