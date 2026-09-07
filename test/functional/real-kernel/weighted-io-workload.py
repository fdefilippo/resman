#!/usr/bin/env python3
"""Bounded direct-read PAM workload on the controller's dedicated null device."""
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time


def main():
    output, device = Path(sys.argv[1]), sys.argv[2]
    if not re.fullmatch(r"/dev/resmanweight[a-z0-9]+", device):
        raise ValueError("not a dedicated fixture device")
    (output / "identity.json").write_text(json.dumps({
        "pid": os.getpid(), "uid": os.getuid(),
        "cgroup": Path("/proc/self/cgroup").read_text().strip(),
        "start_time": Path("/proc/self/stat").read_text().rsplit(")", 1)[1].split()[19],
        "children": [],
    }))
    child = None

    def stop(_signal, _frame):
        if child is not None and child.poll() is None:
            child.terminate()
            child.wait(timeout=5)
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    deadline = time.monotonic() + 900
    while time.monotonic() < deadline:
        child = subprocess.Popen(["/usr/bin/dd", "if=" + device, "of=/dev/null", "bs=64K",
                                  "count=4096", "iflag=direct", "status=none"])
        if child.wait(timeout=60) != 0:
            raise RuntimeError("dedicated-device reader failed")


if __name__ == "__main__":
    main()
