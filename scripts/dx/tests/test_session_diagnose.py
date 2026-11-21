"""Offline CLI coverage for single and folded diagnostic bundles."""

import json
from pathlib import Path
import subprocess
import sys
import unittest


DX = Path(__file__).resolve().parents[1]


def classifier(verdict, start, end):
    return {"verdict": verdict, "window": {"from_ms": start, "to_ms": end}, "evidence": []}


def diagnose(bundle, *args):
    window = {"from_ms": 1000, "to_ms": 601000}
    fixture = {
        "trace": {},
        "window": window,
        "clock": {"client_offset_ms": 0},
        "series": {},
        "events": [],
        "derived_windows": {
            key: [] for key in ("hitches", "abr_downshifts", "encoder_saturation", "likely_network_congestion")
        },
        "classifier": classifier("nominal", 1000, 601000),
    }
    return subprocess.run(
        [sys.executable, "-B", "-m", "session_diagnose.runner", "--bundle", "-", *args],
        cwd=DX, input=json.dumps(fixture | bundle), text=True, capture_output=True, check=True,
    ).stdout


class SessionDiagnoseTests(unittest.TestCase):
    def test_folded_bundle_prints_each_window_and_its_verdict(self):
        reads = [classifier("nominal", 1000, 601000), classifier("future_worse_verdict", 601000, 1201000)]
        fixture = {"classifier_reads": reads, "window": {"from_ms": 1000, "to_ms": 1201000}}
        out = diagnose(fixture)
        self.assertIn("CLASSIFIER  (1000 → 601000)\n  verdict:  nominal", out)
        self.assertIn("CLASSIFIER  (601000 → 1201000)\n  verdict:  future_worse_verdict", out)
        self.assertEqual(json.loads(diagnose(fixture, "--json"))["classifier_reads"], reads)

    def test_single_read_preserves_its_verdict_and_bottom_line(self):
        out = diagnose({})
        self.assertIn("CLASSIFIER\n  verdict:  nominal", out)
        self.assertIn("BOTTOM LINE: NOMINAL", out)


if __name__ == "__main__":
    unittest.main()
