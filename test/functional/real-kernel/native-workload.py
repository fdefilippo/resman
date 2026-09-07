#!/usr/bin/env python3
"""Bounded PAM-session workload; all children stay in their original unit."""
import json
import multiprocessing
import os
from pathlib import Path
import signal
import sys
import time


def burn():
    while True:
        sum(range(10000))


def main():
    output = Path(sys.argv[1])
    children = [multiprocessing.Process(target=burn) for _ in range(6)]
    # Existing resident charges must remain visible when limits are applied in place.
    resident = bytearray(64 << 20)
    for offset in range(0, len(resident), 4096):
        resident[offset] = 1
    for child in children:
        child.start()
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
        time.sleep(1)
    stop(None, None)


if __name__ == "__main__":
    main()
