#!/usr/bin/env python3
"""scripts/dx/bench_baseline.py — pin a run as quasar-bench's named baseline.

    scripts/dx/bench_baseline.py --run <run-id> --name latency-budget/1080p60-h264-local
    make bench-baseline RUN=<id> NAME=<suite/scenario>

Thin wrapper over `Bench.set_baseline` (scripts/dx/vendor/bench.py) — suite and
scenario are read from the run itself (GET /v1/runs/{id}) rather than typed
again, so the baseline can never be pinned against the wrong suite/scenario by
a typo. `--name` is the free-form label quasar-bench stores it under (default:
`<suite>/<scenario>`, matching the docs/testing-bench-mode.md convention);
override it when pinning more than one named baseline for the same
suite+scenario (e.g. a `-clean` and a `-netem` baseline).

This is the "when to re-baseline" half of the standing budget instrument
(docs/testing-bench-mode.md "The glass-to-glass budget" — re-baseline after an
intentional default change, never to make a red run quietly green).

Environment: BENCH_URL, BENCH_KEY (never committed).
"""

from __future__ import annotations

import argparse
import os
import sys

DX_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(DX_DIR, "vendor"))

from bench import Bench, BenchError  # noqa: E402


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--run", required=True, help="run id to pin as the baseline")
    p.add_argument("--name", default="",
                   help="baseline name (default: <run's suite>/<run's scenario>)")
    p.add_argument("--url", default=None)
    p.add_argument("--key", default=None)
    args = p.parse_args(argv)

    b = Bench(args.url, args.key)
    try:
        run = b.run(args.run)
    except BenchError as exc:
        print("error: could not read run %s: %s" % (args.run, exc), file=sys.stderr)
        return 2

    suite = run.get("suite") or ""
    scenario = run.get("scenario") or ""
    if not suite or not scenario:
        print("error: run %s has no suite/scenario on record — refusing to guess" % args.run,
             file=sys.stderr)
        return 2

    name = args.name or "%s/%s" % (suite, scenario)
    try:
        b.set_baseline(suite, scenario, args.run, name=name)
    except BenchError as exc:
        print("error: could not pin baseline: %s" % exc, file=sys.stderr)
        return 1

    print("baseline '%s' -> run %s  (suite=%s scenario=%s)" % (name, args.run, suite, scenario))
    print("RESULT status=ok target=bench-baseline run_id=%s name=%s" % (args.run, name))
    return 0


if __name__ == "__main__":
    sys.exit(main())
