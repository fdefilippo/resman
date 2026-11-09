# Native certified block-IOPS evidence

Issue: `resman-82p`; source release identity: `1.42.0-1`.

These tests ran in an owned disposable local QEMU/KVM guest on 2026-11-08.
They did not install ResMan or attach probes on the development host.
The guest ran Oracle Linux 9.8, systemd `252-67.0.1.el9_8.2`, fio 3.35,
Go 1.27.3, and the two exact kernel builds below. The VM was rebooted between profiles.

| Profile | Kernel | Source with service restrictions | Decisions and restoration |
| --- | --- | --- | --- |
| RHCK | `5.14.0-687.66.1.el9_8.x86_64` | PASS | PASS |
| UEK | `6.12.0-204.92.4.2.el9uek.x86_64` | PASS | PASS |

`evidence/<profile>/source-caps.txt` records the actual kernel-source tests:

- Synchronous direct writes are selected with no exclusions. With `^fio$`,
  64 direct writes are excluded on each observed accounting layer.
- A process naming its `comm` `fio` remains selected when its trusted executable
  is Python. Delayed buffered writeback remains unresolved.
- Included Python and excluded fio write different sectors of the same file
  block; the writeback interval has a zero minimum and a positive maximum.
- Retiring and registering the same unit changes the watch identity; reads
  with the retired identity fail.
- Injecting the kernel health flag for counter-map loss or an accounting-read
  failure invalidates the source permanently. These are health-flag injections,
  not a claim that the test filled the actual counter map.
- Closing a real BPF attachment is detected as source loss.

The source runs as a transient root service with the packaged capability set,
including `CAP_BPF` and `CAP_PERFMON` without `CAP_SYS_ADMIN`, plus
`ProtectSystem=full`, `ProtectHome=yes`, `PrivateTmp=yes`,
`ProtectKernelModules=yes`, `ProtectKernelLogs=yes`, `ProtectClock=yes`,
`RestrictRealtime=yes`, `RestrictNamespaces=yes`, `LockPersonality=yes`,
`SystemCallArchitectures=native` and the packaged address-family restriction.

`evidence/<profile>/decisions.txt` records production-component integration:

- Real systemd authority inventory, `IOAccounting` property lease and kernel
  source feed IOPS-only read/write activation before a finite quota is required.
- Source counts continue while a real write IOPS quota of 100 is applied;
  read traffic activates with the read quota applied too.
- Logical-volume device lookup agrees with the actual `io.max` device. The
  source preserves separate virtual/backing layers; the decision uses the
  effective systemd quota device and does not add duplicate layer counts.
- With certified idle, active CPU quota (`50000 100000`) and RAM limit
  (`536870912`) are restored. A policy reload warms the new source and excludes
  fio from the activation minimum.
- Closing the source with CPU/RAM limits active returns an explicit error and
  restores CPU quota, memory limit and the acquired `IOAccounting` property.

The state test runs as root in the disposable guest and uses synthetic process
CPU/RAM observations with real process-authority inventory, kernel I/O and
systemd mutations. Only the source test runs under all service restrictions.
This is component qualification, not an installed daemon/early-boot test,
production deployment, exhaustive I/O-stack proof or EL8/EL10 qualification.
Kernel functions are private ABI: unqualified builds are explicitly rejected.
Additional distribution/kernel qualification is tracked in `resman-4em`.

`evidence/ci-quality.txt` retains the successful full local build, vet, contracts,
ShellCheck, functional-harness unit checks, race/coverage suite and lint gate.
`report.json` states the scope and limitations in machine-readable form.

See [the source contract](../../../docs/BLOCK-IOPS-SOURCE-DESIGN.md) for accounting,
provenance limits, capacity, interval mathematics, lifecycle and reproduction.
The native test binaries use the `resman_native` tag and the explicit disposable
VM environment guard. Ordinary unit tests never load BPF. Native binary digests
and production/native-test source digests are retained alongside the logs. Private SSH keys and VM
disks are excluded from the retained evidence.
