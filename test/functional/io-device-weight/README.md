# IODeviceWeight platform characterization

This test-only campaign resolves `resman-nq6.40.6` before the weighted-I/O
operator contract selects any production mechanism. It measures the complete
systemd-to-kernel `IODeviceWeight` path on three exact Oracle Linux plus UEK
guest combinations. It does not characterize EL8, EL9, or EL10 kernel families
in general, run ResMan, implement daemon policy, or claim throughput delivery.

Each guest is a fresh overlay of an immutable Oracle KVM image with a second,
512 MiB disposable virtio block device. The probe device has no filesystem,
partition, mount, or dependent block device. The harness may select BFQ and
enable io.cost only for that device; it resets the D-Bus property, disables
io.cost, restores the scheduler, removes the transient service and slice
drop-ins, then destroys the VM and deletes both guest disks. Existing host and
guest devices are never selected. The immutable base-image cache is retained
and verified against its pinned SHA-256 before every run.

The guest creates one run-specific slice directly below `user.slice` and one
finite-lived service inside it. It calls the real systemd manager
`SetUnitProperties` method with signature `sba(sv)` and an `IODeviceWeight`
variant of type `a(st)`. Evidence retains the exact request, D-Bus
introspection/readback, controller ancestry, `io.weight`, `io.bfq.weight`,
`io.cost.qos`, `io.cost.model`, scheduler state, every command, every direct
control-file write, and cleanup state.

## Outcome model

Every BFQ and io.cost row is one of:

- `SUPPORTED`: the valid representative exposed the complete measured path;
- `UNSUPPORTED`: the valid representative demonstrated a technical limitation;
- `BLOCKED`: no valid representative was obtained. This is a matrix/run result,
  not a platform capability result, and cannot be published as support or lack
  of support.

The simultaneous characterization additionally uses `NOT_APPLICABLE` when BFQ
and io.cost did not both pass individually and no simultaneous phase ran. This
is not evidence that the combined path is technically unsupported.

An `UNSUPPORTED` result is useful evidence for the exact listed distribution,
systemd, kernel family/version, and mechanism. It never restricts another kernel
family or later distribution. When BFQ and io.cost are active on the same owned
device, the harness records both footprints but labels the policy state
`mechanism_ambiguous`; it does not infer precedence or double application.

## Pinned representatives

| Tested pair | Oracle image | SHA-256 |
|---|---|---|
| OL8/UEK | `OL8U10_x86_64-kvm-b287.qcow2` | `cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c` |
| OL9/UEK | `OL9U8_x86_64-kvm-b293.qcow2` | `b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b` |
| OL10/UEK | `OL10U1_x86_64-kvm-b291.qcow2` | `8e59326c4bf7cfa58a6cac404db8ed583fe3a5f4c460e2b73c64988785bb4f0f` |

The exact kernel package is retained in each result and in the published table.
No RHCK representative was run, so RHEL, Rocky Linux, AlmaLinux, and Oracle Linux
with RHCK remain uncharacterized.

EL9 and EL10 boot cgroup v2 by default. The EL8 runner records its initial
hierarchy, enables `systemd.unified_cgroup_hierarchy=1` through the image's BLS
configuration, records the changed boot state, and proves a distinct qualified
boot before running the probe. A container is never accepted as kernel-version
evidence.

All guest probes require cgroup v2. PSI is not enabled by the harness, is not a
prerequisite for `IODeviceWeight`, and is not qualified by this campaign.

## Execution

The checkout must be committed and clean so every record names immutable source
input. Run the unit/mutation checks without KVM:

```sh
make test-functional-io-device-weight-unit
```

Run the complete matrix on an explicitly selected root-controlled QEMU host:

```sh
RESMAN_IODEVICEWEIGHT_QEMU_HOST=root@terra \
  make test-functional-io-device-weight-qemu
```

The remote runner owns `/tmp/resman-iow-RUN_ID`, and the QEMU host serializes the
matrix with `/run/lock/resman-iodeviceweight-qemu.lock`. Interrupting the local
runner collects any available evidence and removes the remote bundle. Each VM
has an explicit libvirt name and work directory; its cleanup status is part of
the signed evidence manifest. Base images may be overridden only with the
matching `RESMAN_IOW_EL8_BASE_IMAGE`, `RESMAN_IOW_EL9_BASE_IMAGE`, or
`RESMAN_IOW_EL10_BASE_IMAGE` path; their reviewed digest remains mandatory.

Successful local evidence is written under
`build/functional/io-device-weight/RUN_ID`. `capability-table.md` is generated
only after the independent validator recalculates every manifest and validates
all three platform rows. Publish that table and the raw evidence reference in
`resman-nq6.40.1` before selecting the production contract.

The retained repository evidence and its interpretation are recorded in
[`CAPABILITY-MATRIX.md`](CAPABILITY-MATRIX.md). The host-side unit target
revalidates every committed evidence bundle so a changed file, missing row, or
weakened cleanup proof cannot remain unnoticed.
