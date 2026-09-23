#!/usr/bin/env python3
"""Validate RH05 contract vectors, or execute them via an implementation adapter.

Adapter module must export Adapter with reset(given), act(step), observe(assertion).
This script's no-adapter mode checks the review vectors only; it is not a
substitute for the implementation's Postgres, agent-journal, or hardware tests.
"""

import argparse
import importlib
import json
from pathlib import Path


parser = argparse.ArgumentParser()
parser.add_argument("--adapter", help="Python module exporting Adapter")
args = parser.parse_args()

path = Path(__file__).resolve().parents[2] / "docs/design/rh05-conformance.json"
data = json.loads(path.read_text())
assert data["format"] == 1
cases = data["scenarios"]
ids = [case["id"] for case in cases]
assert len(ids) == len(set(ids))
assert {case["seam"] for case in cases} == {"operator", "launch", "preparation"}
for case in cases:
    assert case["given"] and case["when"] and case["expect"]
    for field in ("given", "when", "expect"):
        assert all(isinstance(item, dict) and item for item in case[field])

if args.adapter:
    adapter_type = importlib.import_module(args.adapter).Adapter
    for case in cases:
        adapter = adapter_type(case["seam"])
        adapter.reset(case["given"])
        for step in case["when"]:
            adapter.act(step)
        for assertion in case["expect"]:
            if not adapter.observe(assertion):
                raise AssertionError(f"{case['id']}: {assertion}")
    print(f"RH05 conformance: {len(cases)} scenarios passed through {args.adapter}")
else:
    print(f"RH05 conformance: {len(cases)} review vectors valid (execution needs --adapter)")
