"""Observed worker placement, shared by reference and daemon measurements."""
import os
from pathlib import Path

from native_gate import field, require


SCOPE = "controlled-placement-only; not evidence for unbound workloads"
LAYOUT = [0, 0, 1, 1, 2, 3]


def inspect(identity, pinned=True, count=6, paused=False):
    parent = field("/proc/%d/stat" % identity["pid"]).rsplit(")", 1)[1].split()
    require(parent[19] == identity["start_time"], "workload owner was recreated")
    children = identity["children"]
    require(len(children) == count and len(set(children)) == count, "wrong worker count")
    workers = {}
    for pid in children:
        path = Path("/proc/%d" % pid)
        stat = field(path / "stat").rsplit(")", 1)[1].split()
        require(stat[1] == str(identity["pid"]), "worker changed owner")
        require(path.stat().st_uid == identity["uid"], "worker changed UID")
        require(stat[0] == ("T" if paused else "R"), "worker is not in its declared runnable state")
        affinity = sorted(os.sched_getaffinity(pid))
        require((len(affinity) == 1 and affinity[0] in range(4)) if pinned else affinity == list(range(4)),
                "worker affinity differs from fixture")
        final = field(path / "stat").rsplit(")", 1)[1].split()
        require(final[19] == stat[19] and final[1] == stat[1] and final[0] == stat[0],
                "worker changed while inspecting placement")
        workers[str(pid)] = {"birth": stat[19], "affinity": affinity, "state": stat[0]}
    return workers


def assert_equal_layout(layouts, expected=LAYOUT):
    require(bool(layouts), "no runnable leaf placement")
    shapes = [sorted(cpu for worker in workers.values() for cpu in worker["affinity"])
              for workers in layouts.values()]
    require(all(shape == shapes[0] for shape in shapes), "compared leaves have different CPU assignment multisets")
    require(shapes[0] == expected, "CPU assignment multiset differs from declared fixture")


def observe(identities, previous, pinned=True, count=6, paused=()):
    layouts = {name: inspect(identity, pinned, count, name in paused) for name, identity in identities.items()}
    births = {name: {pid: worker["birth"] for pid, worker in workers.items()}
              for name, workers in layouts.items()}
    require(previous is None or previous == births, "worker identity changed")
    if pinned:
        assert_equal_layout({name: workers for name, workers in layouts.items() if name not in paused},
                            sorted(index % 4 for index in range(count)))
    return layouts, births
