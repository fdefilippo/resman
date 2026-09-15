# Weighted block-I/O policy

ResMan can assign relative block-I/O weights to authoritative `user-UID.slice`
siblings on an explicit set of whole block devices. The feature is disabled by
default and is independent of the hard bandwidth and IOPS limits configured with
`IO_LIMIT_ENABLED` and `IO_DEVICE_FILTER`.

## Configuration

```ini
IO_WEIGHT_DEVICES=8:0
IO_ROOT_WEIGHT=100
IO_DEFAULT_WEIGHT=100
IO_USER_WEIGHT_FILE=/etc/resman/io-weights.map
```

`IO_WEIGHT_DEVICES` is either empty or a comma-separated list of unique canonical
nonzero `major:minor` device identities. It does not accept `all`, paths, globs,
partitions, stacked devices, RAID, multipath, loop, zram, network block devices, or
unknown topologies. A direct virtio, SCSI, or NVMe request queue may have a terminal
LVM holder only when every holder slave is that same disk or one of its partitions.

The map path and schema marker are stable public names:
`IO_USER_WEIGHT_FILE` defaults to `/etc/resman/io-weights.map`, and the first
non-comment line is `[resman-io-weights-map-v1]`. A custom map path on a separate
mount is not added automatically to the packaged unit's `RequiresMountsFor`; add an
appropriate unit override when that mount must be ready before configuration loading.

Weights use the injective public domain 1 through 1000. `IO_ROOT_WEIGHT` applies to
`user-0.slice`; `IO_DEFAULT_WEIGHT` applies independently to every active unmapped or
excluded user slice. Mapped values come from the root-owned mode-0600 map:

```ini
[resman-io-weights-map-v1]
alice=700
buildbot=250
```

Usernames are resolved exactly through NSS when a complete immutable candidate is
loaded. Unknown or ambiguous names, duplicate UIDs, malformed content, unsafe file or
ancestor ownership, symbolic links, excessive size, or excessive entries reject the
whole candidate. Map-content and weight changes hot-reload atomically. Changing
`IO_USER_WEIGHT_FILE` requires a restart.

For BFQ, ResMan asks systemd for `100 + 11 * (weight - 100)` when the public weight is
above 100, so the value read from `io.bfq.weight` is exactly the configured 1..1000
value. For io.cost the systemd and kernel-domain value is the configured value. A
programmed value is not evidence of delivered throughput.

## Relative share and dilution

Weights are work-conserving relative shares among active sibling slices under
`user.slice`; they are not bandwidth reservations and do not manage the weight of
`user.slice` relative to `system.slice`. Every default slice receives its own weight.
For example, one slice at 1000 competing with 99 default slices at 100 has a nominal
share of `1000 / (1000 + 99 * 100)`, about 9.17 percent, before device and workload
effects. ResMan does not aggregate defaults into a finite pool.

The public denominator includes every discovered sibling slice, including a slice
whose authority is currently unavailable and which therefore is not programmed in
that sample. The per-value coverage and programming state distinguish that condition;
the nominal share is a policy denominator, not a claim about the active kernel set.

Rootless descendants remain inside their authoritative user slice and participate in
the slice weight. When part of a UID workload is outside that slice, ResMan applies the
weight to the known slice but reports `authority_split` and partial coverage; it never
claims a UID-wide guarantee. A missing or recreated unit identity cannot inherit an
old sample's authority.

## Capability activation after startup

Valid configuration never delays daemon readiness or disables CPU, RAM, or hard-I/O
enforcement. After `READY=1`, ResMan performs a read-only classification immediately.
It requires the root cgroup `io` controller and exactly one active mechanism for every
configured device: BFQ selected on the request queue, or `io.cost.qos` already enabled
with `enable=1` by the operator. ResMan never changes a scheduler, `io.cost.qos`, or
`io.cost.model`.

A read-only success is only `probe_candidate`. Before the first production mutation,
an owned transient systemd unit programs a non-default value, materializes the
controller path, confirms D-Bus readback and the exact active kernel file, then resets
and removes the probe synchronously. Only this complete proof produces
`functionally_accepted`.

Late devices, an inactive mechanism, temporarily unavailable evidence, and
observation-derived topology refusals are retried after 1, 2, 4, 8, 16, and at most 30
seconds, independently of the polling or PSI cadence. `refused_observation` remains
visible while read-only reclassification continues. A deterministic mismatch after
materialization, unsafe cleanup, or property conflict becomes
`refused_intervention`; attempts stop until reload or restart. Only the latter class
requires operator action.

`effect_qualified` is separate. It is set only by retained controlled-contention
evidence for an exact representative and mechanism. A host that passes the live probe
is usable even when its delivery effect has not been independently qualified; it is
reported as `observed_delivery=not_measured`.

## Reconciliation and recovery

The complete configured device set is one atomic property assignment per slice. Only
an `evidence_unavailable` reconciliation receives one successful-control-cadence
grace; a second consecutive unavailable reconciliation, or any observation refusal,
releases all owned weights rather than writing blindly. Reload, blackout, empty
`IO_WEIGHT_DEVICES`, shutdown, and stale unit cleanup
use the property lease journal and compare-before-restore. Externally changed values
are preserved and become an intervention-required conflict.

Hard I/O limits and weights have separate property leases. For
`W=IO_WEIGHT_DEVICES` and `H=IO_DEVICE_FILTER`, devices in both receive both policies,
devices only in W receive only weights, and devices only in H receive only hard caps.
Releasing either resource never restores the other. A recovered weight lease is
safely released at startup even when the feature is currently disabled.

## Observability

Prometheus publishes the bounded state and reason plus independent gauges for
the selected mechanism; programming and read-back attempt state; exact requested and
systemd-derived values; the expected kernel-domain value whose exact presence was
confirmed rather than a raw kernel-file capture; complete, partial and unavailable
authority; the sibling denominator and total points; request age and next retry;
functional acceptance; effect qualification; and observed delivery. Aggregate
authority is unavailable when any sibling is unavailable, while the separate counts
retain the complete distribution. Read-only post-readiness classifications and
mutating probes have separate
counters. The same typed object appears in the latest-only MCP system and limits
status. SQLite schema 8 stores these dimensions with every system sample; schema 7 is
migrated atomically, and its historical rows become `disabled` and `not_measured`.

The shipped Grafana dashboard distinguishes `refused_observation` from
`refused_intervention`. Alerting asks for operator action only for the intervention
state; persistent pending or observation refusal is a separate warning about delayed
activation.
