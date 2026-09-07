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
but that is not a daemon scheduling policy. `resman-nq6.39` owns the acceptance
decision. These results cannot satisfy `weighted-io-delivery` in the final gate.

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
