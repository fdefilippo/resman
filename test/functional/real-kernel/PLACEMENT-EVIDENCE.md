# Controlled placement and daemon campaign — 2026-09-07

Scope: **controlled-placement-only; not evidence for unbound workloads**.
This completes the developer campaign for resman-nq6.37, not the full nq6.9 matrix
or release approval. Independent review remains required. The operator wording
correction is resman-nq6.38. No production daemon code changed in this campaign.

## Frozen source and environment

All five runs below used clean, frozen source
`15e86866c9d0304143f888509ab9cc2247ca00dd` on Terra, UEK
`6.12.0-205.92.4.2.el9uek.x86_64`, Intel N95, four online CPUs `0-3`.
Installed RPM stayed `resman-1.33.0-1.el9.x86_64`, inactive. The daemon row
exercised the source-built binary, not that installed package. No package was
built or installed, and no SSH/PAM, global affinity or scheduler setting changed.

The [method](PLACEMENT.md) fixes six workers per active leaf with CPU assignment
multiset `[0,0,1,1,2,3]`. Every five-second sample confirms actual affinities,
runnable states, UID, parent and birth identities. Independent trees are created
anew for each run. Per-index ordering is not the invariant. Historical A4/A6
trials do not count as replicas under this stronger fixture.

## Three independent reference replicas

Each has six consecutive 60-second primary windows; no failed window or run was
discarded. Differences below are absolute percentage points, worst over each run.
Thresholds remain aggregate 0.5, leaf 1.0, incorrect-control separation at least 2.0.

| Run ID | Windows | Worst aggregate gap | Worst leaf gap | Minimum control separation | Cleanup |
|---|---:|---:|---:|---:|---|
| `r20260907094143-205806` | 6/6 PASS | 0.043294 | 0.093176 | 6.658839 | PASS |
| `r20260907094800-206567` | 6/6 PASS | 0.086930 | 0.128005 | 6.568213 | PASS |
| `r20260907095417-207931` | 6/6 PASS | 0.049591 | 0.105439 | 6.593204 | PASS |

`native_replicas.py` re-read all 219 raw frames, all compared leaves, their worker
multisets and stable births, and recomputed all windows. Its result is PASS over
18 primary windows, with worst aggregate **0.086930**, worst leaf **0.128005**,
minimum separation **6.568213**. No margin was converted into a tighter threshold.
The result carries the scope above and rejects duplicate runs, wrong revisions,
unbound characterization, missing scope/frames/leaves and failed cleanup.

## Real daemon with genuine sessions

Run `r20260907100054-209295`, scenario `systemd-native-proportional`: two mapped
cron/PAM sessions, one excluded best-effort cron/PAM session and root SSH/PAM.
Reserve/root/best effort/map = 700/100/100/50+50. Native `user.slice`, independent
reference and incorrect control each had `cpu.max=120000 100000`; their combined
ceiling was 900 host points. Correct weights were 5000/5000/10000/10000; the
incorrect control underweighted best effort at 5000. No ResMan PID migration.

All six full-contention windows passed: worst aggregate gap **0.067351**,
worst leaf gap **0.072654**, minimum control separation against native and
reference **6.657505**. Parent delivery/nominal ranged 0.997986–1.000562 across
these windows and parents, with positive throttling. Maximum counter-read skew
over all seven windows was 74.105 ms, below the fixed 200 ms bound.

During lending, workers in leaf `a` acknowledged SIGSTOP in all three trees;
the session remained alive, and the other leaves retained equal runnable CPU
assignment multisets. Native delivered shares were `a=0.009004%`, `b=20.001176%`,
`root=40.004168%`, `best=39.988361%`. Reference aggregate was 19.998024% against
native 20.010180%. Every other native sibling gained bandwidth without a weight
change. Root answered three probes in each phase; worst response was **1.061005 s**
against the declared 3 s bound.

All ten mandatory checks passed: full-budget rejection, real PAM sessions, exact
native plan, full contention, incorrect control, lending, root progress, unchanged
membership, unchanged journal/weights and graceful stop. Stop restored the native
properties while sessions were still alive. Cleanup then terminated only owned
sessions and removed reference units. No manual revert was necessary.

## Executable unbound counterexample

Run `r20260907100950-210417`, scenario `systemd-native-reference`, retained six
unbound workers per leaf. Six 60-second windows reported worst aggregate gap
2.157551 pp, worst leaf gap 5.876508 pp and minimum control separation 5.255576 pp.
Result **CHARACTERIZATION**, exit 0, cleanup PASS; the analysis contains no
comparison PASS/FAIL fields. This is no verdict on ResMan or unbound delivery.
Historical U1/U2 remain in [REFERENCE-EVIDENCE.md](REFERENCE-EVIDENCE.md).

## Evidence identity and handoff

Raw artifacts are machine-local under
`build/functional/real-kernel/<run-id>-<scenario>/`; they are ignored by Git.
This document preserves their conclusions and identities. Each bundle retains
commands, kernel/environment, source revision, measurement scope, raw counters,
actual worker placement, topology and cleanup outcome.

SHA-256 of `reference-raw.json`, in reference-run order above, then unbound:

```text
5d636838b86881abd5a1623cda781d25b4a2bdab57a1f10d4137e1bba2213883
b5bdeae669b9d73f1281dacea84380a0debaa8c6622576baf0cab4005793caed
1630688c3838957ac3845ec149d62d398bdfb9d1f5a8e1ce954f1dddc164d1cf
44c74c3d2878ebdb0e71b7302bc4e104fb47d5a0b55031b8f68708a3b56e2f41
```

SHA-256 of daemon `full-contention-0-raw.json` through `-5-raw.json`, then
`mapped-idle-raw.json`:

```text
8fc6bf9e4947bf6e1620f4a0216a583834f1fdaf83f3abccc6e992c81ea64c5b
0a3d36388a7e22e569da9c0606997175cf95f7eaa280ab103072d7c94aafa24f
02a081a5e31ebbb98ee7a0f1062b3e74d5c76ba5bca68e0dea79d81d2f649ffb
7c7c96e9bde397f106bc0a5ff74d2bfde85490afce559bb0730edda4ed3de34d
c411c92bf4d7141acbdf1a85cdefa2669275f9d065f0e0745c61679e6e52e5f6
d174e9e5592d7e3a41a0c8c7096db9d9f41949b5dd991244842cb80bd4b44e6c
bb57d2331cfa4f20bcb2e919f48d566a86c8d3fe495b9baba3800c44c894f204
```

Local verification: `make ci-quality` PASS (12 architectural contracts, shell and
Python harness tests, Go race/coverage, lint zero), plus uncached
`go test -race -count=1 ./...` PASS. Four in-memory mutations were caught:
removing multiset validation from observation, disabling runnable-state checking,
disabling birth-identity comparison, and disabling cross-leaf multiset equality.
Three additional in-memory mutations selecting the best rather than the worst
aggregate gap, leaf gap or control separation were also caught. That reporting
test was strengthened after the remote runs, without changing their fixture.
No repository file was changed while a remote runner was executing.

Final host check: packaged daemon inactive, `user.slice/cpu.max=max 100000`,
no property journal, runtime override, test cron, helper process or remote bundle.
Existing analyst artifacts and the nspawn root were preserved. No push, tag,
package identity change, merge or epic closure is part of this work.
