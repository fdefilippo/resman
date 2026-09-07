#!/usr/bin/env python3
"""PSI summary flags never replace the actual raw refresh-only measurements."""
from pathlib import Path
import tempfile
import unittest

from native_psi_evidence import validate


def fixture(path):
    values = {"psi_available": "true", "psi_event_driven_active": "true",
              "control_cycles_before": 2, "control_cycles_after": 2, "collections_before": 4, "collections_after": 6,
              "decision_cpu_before": 15, "decision_cpu_after": 15, "decision_ema_before": 12, "decision_ema_after": 12,
              "system_observation_before": 25, "system_observation_after": 2}
    (path / "psi-refresh-neutrality.txt").write_text("".join("%s=%s\n" % item for item in values.items()))
    with (path / "environment.txt").open("a") as env:
        env.write("test_user=fixture\n")
    (path / "psi-resman.log").write_text("PSI event-driven mode enabled\n")
    for suffix, filename in (("before", "psi-baseline.prom"), ("after", "psi-after-refresh.prom")):
        (path / filename).write_text("\n".join((
            "resman_control_cycles_total %d" % values["control_cycles_" + suffix],
            "resman_metrics_collection_duration_seconds_count %d" % values["collections_" + suffix],
            'resman_user_cpu_usage_percent{username="fixture"} 15',
            'resman_user_cpu_usage_ema_percent{username="fixture"} 12',
            "resman_observation_host_cpu_sample_available 1",
            "resman_cpu_total_usage_percent %d" % values["system_observation_" + suffix])))


class PSIEvidenceTests(unittest.TestCase):
    def test_unavailable_or_missing_sample_never_proves_host_refresh(self):
        for filename in ("psi-baseline.prom", "psi-after-refresh.prom"):
            for availability in ("", "resman_observation_host_cpu_sample_available 0"):
                with self.subTest(filename=filename, availability=availability), tempfile.TemporaryDirectory() as directory:
                    path = Path(directory)
                    fixture(path)
                    raw = path / filename
                    raw.write_text(raw.read_text().replace("resman_observation_host_cpu_sample_available 1", availability))
                    if filename == "psi-after-refresh.prom":
                        raw.write_text(raw.read_text().replace("resman_cpu_total_usage_percent 2", "resman_cpu_total_usage_percent 0"))
                        proof = path / "psi-refresh-neutrality.txt"
                        proof.write_text(proof.read_text().replace("system_observation_after=2", "system_observation_after=0"))
                    with self.assertRaises(ValueError):
                        validate(path)

    def test_actual_scrapes_log_and_every_summary_field_are_required(self):
        for mutation in ("none", "missing-active", "missing-available", "no-log", "event", "changed-decision", "too-few-refreshes", "missing-scrape"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                path = Path(directory)
                fixture(path)
                if mutation.startswith("missing-") and mutation != "missing-scrape":
                    key = "psi_event_driven_active" if mutation == "missing-active" else "psi_available"
                    proof = path / "psi-refresh-neutrality.txt"
                    proof.write_text("\n".join(line for line in proof.read_text().splitlines() if not line.startswith(key + "=")))
                if mutation == "no-log": (path / "psi-resman.log").write_text("")
                if mutation == "event": (path / "psi-resman.log").write_text("PSI event-driven mode enabled\nPSI event received")
                if mutation == "missing-scrape": (path / "psi-after-refresh.prom").unlink()
                if mutation in ("changed-decision", "too-few-refreshes"):
                    raw = path / "psi-after-refresh.prom"
                    before, after = ('} 15', '} 16') if mutation == "changed-decision" else ("seconds_count 6", "seconds_count 5")
                    raw.write_text(raw.read_text().replace(before, after))
                if mutation == "none": self.assertEqual(validate(path)["collections_after"], 6)
                else:
                    with self.assertRaises((ValueError, OSError)): validate(path)


if __name__ == "__main__":
    unittest.main()
