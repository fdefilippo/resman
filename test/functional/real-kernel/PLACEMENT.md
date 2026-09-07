# Controlled placement measurement contract

This is a fixture control, not a ResMan affinity feature. The daemon's systemd
adapter does not program CPU affinity. A PASS is **controlled-placement-only;
not evidence for unbound workloads**. This scope also appears in each run's
`measurement-scope.json` and the three-replica calibration gate output.

## Why placement matters

Linux's per-CPU group entity shares depend on the group's local runqueue load
relative to its total load; see
[`calc_group_shares` in Linux 6.12](https://github.com/torvalds/linux/blob/v6.12/kernel/sched/fair.c#L3670-L3790).
Equal normalized runnable-load distributions remove this source of unequal
local shares. Identical CPU-bound workers with identical nice values and the same
CPU assignment multiset establish that controlled distribution in this fixture.
They do not remove load-average approximations, history or bandwidth throttling,
nor establish exact instantaneous fairness. Hence measured independent references
and fixed tolerances are still necessary.

Each compared active leaf has six workers pinned to CPUs `0,1,2,3,0,1`.
At every five-second sample, including every window boundary, inspect the actual
affinity, UID, parent PID, birth identity and runnable state. Compare multisets
`[0,0,1,1,2,3]`, not worker index ordering or merely the set of available CPUs.
A missing, dead, blocked or replaced worker invalidates the sample. Helpers remain
unbound and their small measured contribution is not assumed to be zero.
In the lending phase the deliberately paused `a` leaves must acknowledge SIGSTOP;
their workers must stay stopped and retain identity/affinity. Only those declared
paused leaves are excluded from the runnable-distribution comparison.

## Reproducible sequence

Freeze and commit the source tree before starting remote commands. Never edit a
running driver. Run these sequentially on a disposable, quiescent four-CPU host:

```sh
# Three separate invocations, each creating new private trees, six 60s windows.
test/functional/real-kernel/remote.sh systemd-native-reference-pinned-six root@terra
test/functional/real-kernel/remote.sh systemd-native-reference-pinned-six root@terra
test/functional/real-kernel/remote.sh systemd-native-reference-pinned-six root@terra
python3 test/functional/real-kernel/native_replicas.py REVISION EVIDENCE_1 EVIDENCE_2 EVIDENCE_3
# Real daemon, cron/PAM users and root SSH/PAM: six full-contention windows,
# followed by lending, root responsiveness and unchanged ownership/plan/release.
test/functional/real-kernel/remote.sh systemd-native-proportional root@terra
# Counterexample remains executable; dispersion only, never acceptance.
test/functional/real-kernel/remote.sh systemd-native-reference root@terra
```

All eighteen calibration windows must meet aggregate 0.5 pp, leaf 1.0 pp and
incorrect-control separation at least 2.0 pp against both correct references.
Report worst gaps and minimum separation, never the best run or a pooled average.
The unbound row returns `CHARACTERIZATION` on valid collection, without a delivery
verdict; invalid collection or cleanup still fails. It cannot substitute for a
required PASS. The daemon test uses the same bounds, placement inspection and
counter synchronization. Its PASS is not the complete nq6.9 matrix or release
approval; independent review and the other acceptance rows remain required.

Counter synchronization is measured rather than inferred. Every `cpu.stat` read has
its own monotonic start and finish time, and a parent plus its four leaves form one
adjacent read block. Intermediate five-second conservation is bilateral and permits
only the fixture's four-CPU instantaneous execution capacity multiplied by the
measured block spans at both endpoints, plus ten microseconds for integer-counter
quantization. The parent's 120% CFS quota is an average over its 100 ms period and
does not limit a shorter sampling span to 1.2 CPUs before throttling. Primary and longer
windows retain the fixed bilateral one-percent limit. The 200 ms frame-skew ceiling
does not become an intermediate conservation tolerance.
