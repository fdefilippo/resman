#!/usr/bin/env python3
"""Bounded PAM-session workload; all children stay in their original unit."""
import json
import multiprocessing
import os
from pathlib import Path
import signal
import sys
import time


WORKER_READY_TIMEOUT_SECONDS = 10


def burn(cpu=None, ready=None):
    if cpu is not None:
        os.sched_setaffinity(0, {cpu})
    if ready is not None:
        ready.set()
    while True:
        sum(range(10000))


def worker_cpus(arguments):
    if not arguments:
        return [None] * 6
    if arguments not in (["--one-worker-per-cpu"], ["--six-pinned-workers"]):
        raise ValueError("unsupported workload mode")
    if os.sched_getaffinity(0) != set(range(4)):
        raise ValueError("pinned diagnostic requires CPUs 0-3")
    return [index % 4 for index in range(6 if arguments == ["--six-pinned-workers"] else 4)]


def wait_for_worker_readiness(children, readiness, timeout=WORKER_READY_TIMEOUT_SECONDS):
    deadline = time.monotonic() + timeout
    pending = set(range(len(children)))
    while pending:
        for index in tuple(pending):
            if readiness[index].is_set():
                pending.remove(index)
            elif not children[index].is_alive():
                raise RuntimeError("worker exited before acknowledging readiness")
        if pending and time.monotonic() >= deadline:
            raise RuntimeError("worker readiness acknowledgement timed out")
        if pending:
            time.sleep(0.01)


def start_workers(cpus, event_factory=multiprocessing.Event, process_factory=multiprocessing.Process):
    readiness = [event_factory() for _ in cpus]
    children = [process_factory(target=burn, args=(cpu, ready)) for cpu, ready in zip(cpus, readiness)]
    try:
        for child in children:
            child.start()
        wait_for_worker_readiness(children, readiness)
    except Exception:
        for child in children:
            if child.is_alive():
                child.terminate()
        for child in children:
            child.join(5)
        raise
    return children


def main():
    output = Path(sys.argv[1])
    children = start_workers(worker_cpus(sys.argv[2:]))
    # Existing resident charges must remain visible when limits are applied in place.
    resident = bytearray(64 << 20)
    for offset in range(0, len(resident), 4096):
        resident[offset] = 1
    (output / "identity.json").write_text(json.dumps({
        "pid": os.getpid(), "uid": os.getuid(),
        "session": os.environ.get("XDG_SESSION_ID", ""),
        "cgroup": Path("/proc/self/cgroup").read_text().strip(),
        "start_time": Path("/proc/self/stat").read_text().rsplit(")", 1)[1].split()[19],
        "children": [child.pid for child in children],
    }))
    def stop(_signal, _frame):
        for child in children:
            child.terminate()
        for child in children:
            child.join(5)
        raise SystemExit(0)
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    deadline = time.monotonic() + 1800
    while time.monotonic() < deadline:
        probe = output / "probe"
        if probe.exists():
            (output / "response").write_text(probe.read_text())
            probe.unlink()
        time.sleep(1)
    stop(None, None)


if __name__ == "__main__":
    main()
