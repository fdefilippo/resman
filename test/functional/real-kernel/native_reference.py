#!/usr/bin/env python3
"""Reference-versus-reference diagnostic; never evidence of daemon acceptance."""
import os
from pathlib import Path
import shutil
import signal
import sys
import time
import traceback

from native_gate import Blocked, NativeGate, field, require, sha
from native_proportional import LEAVES, ProportionalGate, compare_reference, ratios
from native_placement import SCOPE, observe


WINDOWS = 6


def analyze(frames):
    require(len(frames) == 73, "six complete windows are required")
    results = []
    for length in (12, 36, 72):
        for start in range(0, 72, length):
            segment = frames[start:start + length + 1]
            # Every intermediate sample is part of the validity contract.
            for old, new in zip(segment, segment[1:]):
                ratios(old, new, minimum_seconds=0)
            measured = ratios(segment[0], segment[-1], minimum_seconds=length * 5)
            duration = segment[-1]["time"] - segment[0]["time"]
            for group, values in measured.items():
                values["delivered_over_nominal"] = values["parent_usec"] / (duration * 1200000)
                require(values["delivered_over_nominal"] >= 0.8, group + " parent lacks usable bandwidth")
                require(values["throttling"]["nr_throttled"] > 0, group + " parent never throttled")
            first, second = measured["referencea"], measured["referenceb"]
            differences = {key: first[key] - second[key] for key in LEAVES + ("mapped",)}
            violation = ""
            try:
                compare_reference({"native": first, "oracle": second})
            except AssertionError as err:
                violation = str(err)
            separation = min(abs(measured["stale"]["mapped"] - ref["mapped"]) for ref in (first, second))
            results.append({"start_seconds": start * 5, "window_seconds": length * 5,
                            "measured": measured, "differences_pp": differences,
                            "reference_comparable": not violation, "violation": violation,
                            "stale_separation_pp": separation, "stale_discriminating": separation >= 2.0})
    return results


def comparable(results):
    primary = [row for row in results if row["window_seconds"] == 60]
    return (len(primary) == WINDOWS and {row["start_seconds"] for row in primary} == set(range(0, 360, 60))
            and all(row["reference_comparable"] and row["stale_discriminating"] for row in primary))


class ReferenceDiagnostic(ProportionalGate):
    pinned = False
    six_pinned = False

    def scenario(self):
        if self.six_pinned:
            return "systemd-native-reference-pinned-six"
        return "systemd-native-reference-pinned" if self.pinned else "systemd-native-reference"

    def reference_workload_args(self):
        if self.six_pinned:
            return ("--six-pinned-workers",)
        return ("--one-worker-per-cpu",) if self.pinned else ()

    def placement(self):
        identities = {group + "/" + leaf: identity for (group, leaf), identity in self.reference_identities.items()}
        require(set(identities) == {group + "/" + leaf for group in self.paths for leaf in LEAVES},
                "missing compared workload identity")
        layouts, self.worker_births = observe(identities, self.worker_births, self.pinned,
                                              4 if self.pinned and not self.six_pinned else 6)
        return layouts

    def preflight(self):
        NativeGate.preflight(self)
        if field("/sys/devices/system/cpu/online") != "0-3" or os.sched_getaffinity(0) != set(range(4)):
            raise Blocked("reference diagnostic requires four unrestricted online CPUs")
        self.save("diagnostic-contract", {
            "runner_sha256": sha(__file__), "daemon_started": False,
            "tested_artifact": "reference fixture only; bundled daemon binary is not exercised",
            "primary_windows": 6, "primary_window_seconds": 60, "sampling_seconds": 5,
            "exploratory_windows_seconds": [180, 360], "aggregate_tolerance_pp": 0.5,
            "leaf_tolerance_pp": 1.0, "minimum_stale_separation_pp": 2.0,
            "groups": ["referencea", "referenceb", "stale"],
            "workers_per_leaf": 4 if self.pinned and not self.six_pinned else 6,
            "worker_affinity": ("round-robin CPUs 0,1,2,3,0,1" if self.six_pinned else
                                "one worker on each CPU 0-3" if self.pinned else "unrestricted CPUs 0-3"),
            "note": "Longer windows are descriptive, not an alternative route to PASS.",
        })

    def run(self):
        status, detail, cleanup = "FAIL", "reference diagnostic incomplete", "PASS"
        try:
            self.preflight()
            self.helper.mkdir(mode=0o700)
            shutil.copyfile(self.bundle / "native-workload.py", self.helper / "workload.py")
            self.start_references(("referencea", "referenceb", "stale"))
            self.save("workload-identities", {group + "/" + leaf: identity
                                             for (group, leaf), identity in self.reference_identities.items()})
            frames = []
            for step in range(73):
                if step:
                    # Fixed sampling interval, never a readiness assumption.
                    time.sleep(max(0, frames[-1]["time"] + 5 - time.monotonic()))
                frames.append(self.snapshot())
                self.save("reference-raw", frames)
                if step % 12 == 0:
                    self.record_topology("reference-topology-%03d" % (step * 5))
                    print("reference sampling: %d/360 seconds" % (step * 5), flush=True)
            results = analyze(frames)
            if not self.pinned:
                # An executable counterexample reports dispersion, never acceptance.
                for row in results:
                    for key in ("reference_comparable", "violation", "stale_discriminating"):
                        row.pop(key)
            self.save("reference-analysis", results)
            status = ("PASS" if comparable(results) else "FAIL") if self.pinned else "CHARACTERIZATION"
            detail = "reference comparability only; no daemon, lending or release acceptance"
            if status == "FAIL":
                detail = "identical references exceed declared bounds or stale control is not discriminating"
        except Blocked as err:
            status, detail = "BLOCKED", str(err)
        except Exception as err:
            detail = str(err)
            traceback.print_exc()
        finally:
            try:
                self.cleanup()
            except Exception as err:
                cleanup, status = "FAIL", "FAIL"
                detail += "; cleanup: " + str(err)
                traceback.print_exc()
            (self.evidence / "result").write_text(status + "\n")
            self.save("measurement-scope", {"scope": SCOPE if self.pinned else "unbound-characterization; no acceptance verdict",
                                           "source_revision": self.revision, "run_id": self.run_id,
                                           "daemon_exercised": False, "primary_windows": 6,
                                           "cpu_assignment_multiset": sorted(index % 4 for index in range(4 if not self.six_pinned else 6))
                                           if self.pinned else None})
            (self.evidence / "environment.txt").write_text(
                "scenario=%s\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\ndetail=%s\n" %
                (self.scenario(),
                 self.revision, os.uname().release, cleanup, status, detail.replace("\n", " ")))
            print(status + ": " + detail, flush=True)
        return {"PASS": 0, "CHARACTERIZATION": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    require(sys.argv[1] in ("systemd-native-reference", "systemd-native-reference-pinned", "systemd-native-reference-pinned-six"),
            "unsupported reference diagnostic")
    gate = ReferenceDiagnostic(Path(__file__).parent, sys.argv[2], sys.argv[3])
    gate.pinned = sys.argv[1] != "systemd-native-reference"
    gate.six_pinned = sys.argv[1] == "systemd-native-reference-pinned-six"
    def interrupted(_signal, _frame):
        raise RuntimeError("reference diagnostic interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
