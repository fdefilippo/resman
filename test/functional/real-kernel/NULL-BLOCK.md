# Dedicated null_blk / BFQ characterization

The maintainer approved a dedicated virtual block device on Terra for the
weighted-I/O investigation in `resman-nq6.9`. Do not change `sda` or any existing
device's scheduler. `null_blk_characterization.py` creates its own configfs
device, sets BFQ on that device alone, and removes its device at exit. It unloads
the module only if it loaded it and no other devices appeared. Pre-existing
modules and devices are preserved; active ResMan prevents the run. No filesystem,
partition table, image or persistent device configuration is written.

## Scope boundary

This is **kernel-only characterization**, not daemon enforcement or physical
disk performance. The fixture writes `IOWeight` on its own transient services;
ResMan does not run. The current native planner generates hard bandwidth/IOPS
limits, not `IOWeight` assignments. The adapter can represent and verify weights,
but that is not a daemon scheduling policy. The approved `resman-nq6.39` decision
removes weighted delivery from the daemon coverage row and requires a distinct
adapter proof instead. The future daemon policy and production device-capability
semantics are deferred to `resman-nq6.40`. These standalone characterization
results cannot satisfy the adapter row or any daemon-delivery claim.

## Revision-bound adapter acceptance

```sh
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-weighted-io-adapter root@terra
```

`native_weighted_io.py` uses two quiescent fixture accounts (`resman-t1` and
`resman-t2`) in genuine cron/PAM sessions. No SSH credentials are installed or
needed. The retained `systemdunit-real.test` runs `TestRealIOWeightGateProbe` for
every operation, with separate root-private lease journals. It applies IOWeight
100/100, then 100/2300 (BFQ readback 100/300), and restores the original values
through the adapter before ending the sessions. No daemon process is started.

The row declares `adapter_exercised=true` and `daemon_exercised=false`. PASS means
exact adapter/kernel roundtrip, independently verified BFQ on the dedicated device,
a negative phase and exact cleanup. It does not mean proportional delivery by
ResMan. Two 20-second direct-read windows record raw per-device counters and
recomputed shares as CHARACTERIZATION, with no ratio threshold or CPU tolerance.

The negative phase selects `none` or `mq-deadline` on this device only. IOWeight
can still be programmed and `io.bfq.weight` can remain readable; neither proves
that the selected scheduler consumes it. The fixture additionally checks that
the device has no enabled io.cost policy before reporting effective weight
capability false. This is not a change to the production adapter's verification
contract. Missing schedulers, configfs or other prerequisites remain BLOCKED.

The new device is named `resmanweight<RUN_ID>` (sanitized to lowercase letters
and digits), not `nullb<index>`: UEK exposes the configfs name as the block name.
Existing devices, including Terra's unrelated `nullb0`, are not reused or changed.
The module and all existing scheduler selections are preserved. Both fixtures
share the same exclusive lock; the remote runner also retains its bounded
controller, cancellation/drain and artifact-provenance contract. Cleanup errors
are FAIL, never silently downgraded to a capability refusal.

The synthetic device uses one submission queue, depth one, 1 ms completion delay,
256 MiB logical size and no memory backing. Read requests use direct I/O. BFQ
low-latency weight raising is disabled only on this device. Two competing leaves
first receive equal weights, then systemd weights 100/2300, read back as BFQ
100/300. Each phase has a declared five-second warmup and three successive
20-second intervals, with identity, scheduler, weight and counter inspections
every five seconds. This is not a calibration of universal tolerances: shares
are reported from actual per-device `io.stat` deltas with **no ratio verdict**.

See the upstream [null_blk driver documentation](https://www.kernel.org/doc/html/latest/block/null_blk.html)
and [BFQ documentation](https://www.kernel.org/doc/html/latest/block/bfq-iosched.html)
for synthetic completion and the low-latency heuristics being controlled.

## Execution

Use a quiescent disposable host and coordinate with other campaigns. Keep the
local checkout unchanged while executing and retain the exact script with the
evidence. Run IDs contain only `r` followed by 6–30 lowercase letters or digits.
The evidence parent must exist; its final directory must not exist.

Run as root with `/usr/sbin:/sbin:/usr/bin:/bin` in `PATH`:

```sh
python3 /root/null_blk_characterization.py \
  --run-id rYYYYMMDDHHMMSS \
  --source-revision FULL_40_CHARACTER_COMMIT \
  --evidence /root/nullblk-rYYYYMMDDHHMMSS
```

Use a run-specific controller unit with `RuntimeMaxSec=240`,
`TimeoutStopSec=45` and `KillMode=mixed` to bound the campaign independently of
the SSH connection. Individual I/O services also have a 120-second lifetime.
On local interruption, stop and drain that controller before collecting evidence.
Do not start a second campaign while the controller is active. The exclusive
lock file under `/run` is inert after exit and is deliberately not unlinked,
avoiding two concurrent lock inodes.

`characterization.json` and `commands.jsonl` retain settings, raw samples,
recomputed intervals, source/script identity and cleanup. A successful run says
`CHARACTERIZATION`, never `PASS`. Failed cleanup makes the overall result `FAIL`.
Inspect the recorded failure before any manual cleanup; never revert unrelated
units or unload a module the fixture did not load.
