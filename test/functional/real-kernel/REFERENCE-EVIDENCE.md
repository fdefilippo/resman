# Native reference comparability investigation — 2026-09-07

This is durable evidence for **resman-nq6.37**, not acceptance of the daemon,
`resman-nq6.9`, a package, or the parent epic. No production code was changed.
The existing unbound `systemd-native-proportional` scenario was not replaced.

## Environment and method

Terra ran UEK `6.12.0-205.92.4.2.el9uek.x86_64` on an Intel N95 with four
physical cores, one thread per core and one NUMA node. The packaged daemon stayed
inactive; installed RPM identity remained `resman-1.33.0-1.el9.x86_64`.
The common preflight queried the source binary's version, but never started it
as a daemon. No package was built or installed.

Each run created three private, isomorphic systemd slice trees. Each parent had
`cpu.max=120000 100000`; combined ceilings were 90% of host capacity. Each
reference used leaf weights `5000/5000/10000/10000` for logical roles
`a/b/root/best`. These roles were reference services, not genuine PAM sessions.
The deliberately incorrect control instead gave `best` weight 5000.
No native user slice, login session, SSH/PAM configuration or scheduler setting
was modified. Each workload helper held 64 MiB and spawned CPU workers.

Every run retained six consecutive, non-overlapping windows of at least 60 seconds,
with synchronized raw counters every five seconds and full scheduling topology
at each window boundary. These adjacent windows are not six independent trials;
U1 and U2 below used freshly created trees in separate runs. Descriptive 180/360s
aggregates overlap the primary observations and are not additional trials.

Shares use each parent's measured CPU time, not nominal quota. Acceptance stayed
at 0.5 percentage points for `a+b`, 1.0 for each leaf, and at least 2.0 points
separation from the incorrect control against both references. Every primary
window must pass; longer aggregates cannot rescue a failed primary window.

## Observations

All differences below compare the two correct references, without ResMan running.
Maximum gaps are absolute percentage points across the six primary windows.

| Run | Workers per leaf and affinity | Windows within both bounds | Maximum aggregate gap | Maximum leaf gap | Minimum control separation |
|---|---|---:|---:|---:|---:|
| U1 | Six, all CPUs allowed | 0/6 | 3.7618 | 6.2165 | 3.2832 |
| U2 | Six, all CPUs allowed | 0/6 | 17.2815 | 15.9857 | 0.5420 |
| A4 | Four, one bound to each CPU | 6/6 | 0.0282 | 0.0319 | 6.6308 |
| A6 | Six, bound to CPUs 0,1,2,3,0,1 | 6/6 | 0.0749 | 0.0714 | 6.6079 |

U2 also failed control discrimination in two windows. U1 and U2 parents delivered
99.959%–100.061% of nominal quota; maximum read skew was below 5 ms. Workers had
nice 0 and affinity 0-3. On the full 360s interval U1's aggregate gap fell to
-0.2833 points, but its maximum leaf gap remained 2.3326; U2 still had aggregate
gap -9.0025 and maximum leaf gap 9.7460. These observations do not support curing
the discrepancy by selecting a longer window or calling it small random jitter.

A4 changed both worker count and affinity. A6 retained the original count and
changed only worker affinity, verifying each worker's actual CPU set, parent and
birth identity at every sample. Its 360s aggregate gap was +0.0231 points and its
maximum leaf gap 0.0392. The helper remained unbound. All four accepted campaign
records above finished with cleanup PASS; U1/U2 correctly returned overall FAIL.

The result supports sensitivity to worker placement in this fixture. It does not
identify a specific kernel defect or demonstrate that arbitrary unbound user
workloads receive the promised shares. Nor does it exonerate an untested daemon:
it demonstrates that ResMan is not needed to reproduce this oracle failure.

The [kernel weight model](https://docs.kernel.org/admin-guide/cgroup-v2.html#weights)
describes proportional distribution among active children. The
[bandwidth documentation](https://docs.kernel.org/scheduler/sched-bwc.html#hierarchical-considerations)
also distinguishes own-quota and ancestor-quota throttling. Those descriptions
are not measurements establishing the project's 0.5/1.0-point tolerances on this
host; no specific scheduling cause is inferred from them here.

## Immutable evidence identities

Local raw artifacts are under `build/functional/real-kernel/<run>-<scenario>/`.
That directory is ignored by Git; this summary and the Beads comments preserve
the findings independently of that machine-local archive.

| Run | Run ID | Source revision | Scenario suffix |
|---|---|---|---|
| U1 | `r20260907083246-177312` | `c9342c3603dc839e8b9ca28a68de50359f42b4e4` | `systemd-native-reference` |
| U2 | `r20260907084651-186679` | `e55c6e973483468061172f9f57e5f86e37b3bbac` | `systemd-native-reference` |
| A4 | `r20260907085336-188229` | `e55c6e973483468061172f9f57e5f86e37b3bbac` | `systemd-native-reference-pinned` |
| A6 | `r20260907090240-189661` | `0cf9113b9c5888497372042e14595cd42cfa650d` | `systemd-native-reference-pinned-six` |

SHA-256 of each `reference-raw.json`:

```text
U1 e351f972d13f4b272fdb361607feec8d5d9083c52de4a294c23efb1551265493
U2 b120f6126ea7434ee5f36aaa0de1b215e2ab52113e4a78101055111f6673d7bd
A4 e2274efe9b704abdfb2e30e47d00263a3f9ff43a94a74593a2285489bb566a37
A6 7dc6814ecfbabf33a64efbe0e06cf8ac53e371eefd0371c0a43470a7f236d518
```

Each archive also contains analysis JSON, per-minute topology, process identities,
command arguments, environment and cleanup outcomes. Reproduce with the documented
`remote.sh <scenario> root@terra` interface on a clean, frozen revision. Never edit
the live local driver while it is executing.

An excluded run, `r20260907083930-182303` on `dadbf23`, completed remote collection
and cleanup, but the developer modified the local running shell driver, which then
reported a parse error. Its raw artifacts and method correction remain recorded in
Beads; it is not counted as an independent clean validation. U2 is its frozen-tree
replacement, not a rewritten outcome.

## Proposed next decision — pending independent review

Use explicit worker affinity as a candidate controlled measurement method, with
the original six workers, unchanged bounds, and actual affinity verification for
every compared workload. Retain the unbound counterexamples as characterization;
do not erase them or imply general unbound fairness from a controlled test.

Before changing `.9`, independent review must accept that method and its scope.
It must then be implemented and exercised with the real daemon and genuine PAM
sessions, including lending, the incorrect-plan control, root response, unchanged
ownership/weights/journal and cleanup. None of those daemon acceptance claims was
tested in these reference-only runs. `.37`, `.9` and the epic remain open.

## Subsequent review decision

The maintainer accepted controlled placement with five conditions: explicit equal
CPU assignment multisets for runnable leaves, three independent six-window fresh
replicas, unchanged thresholds, an executable unbound characterization without a
delivery verdict, and scope in gate output. See [PLACEMENT.md](PLACEMENT.md) for
the kernel mechanism, fixture assertions and execution protocol. The historical
A4/A6 trials above do not count as the three new trials under these assertions.
Operator delivery wording is tracked separately in resman-nq6.38.
The subsequent three-replica and real-daemon results are recorded separately in
[PLACEMENT-EVIDENCE.md](PLACEMENT-EVIDENCE.md), without changing the historical
observations above.
