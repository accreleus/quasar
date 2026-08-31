#!/usr/bin/env python3
"""scripts/dx/bench_table.py — render a quasar-bench /v1/stats cross-tab as markdown.

    scripts/dx/bench_table.py --suite baseline \
        --rows tag.profile --cols tag.abr_mode \
        --metric harness.run_browser_fps_mean --agg p50

One `GET /v1/stats` per column value, pivoted into a `rows x cols` table, so the
report in docs/reports/ is generated from the API rather than transcribed by hand
(and the API URL of every query is printed underneath it, per the dashboard's own
"every chart is a curl call" rule).

`--metric` may be repeated: one table per metric.

Window (quasar-bench 1.1)
  `--window` scopes every aggregate to a phase. The default is `auto`: the runs
  in scope are sampled, `GET /v1/runs/{id}/phases` asked, and if they carry an
  `impaired` window that is what the table reports. This is not a nicety — a
  whole-run p50 of an impairment experiment is dominated by the clean baseline
  and recovery holds, and comes back ~identical for every arm (which is exactly
  how runs C/D/E first read as "no difference"). The window each number came
  from is printed with the table and in the API URL beneath it.
  `--window run` (or `--window ''`) restores the whole-run aggregate.

Environment: BENCH_URL, BENCH_KEY (never committed).
"""

from __future__ import annotations

import argparse
import os
import sys

DX_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(DX_DIR, "vendor"))

from bench import Bench, BenchError  # noqa: E402


def fmt(v) -> str:
    if v is None:
        return "—"
    if isinstance(v, float):
        return ("%.2f" % v).rstrip("0").rstrip(".")
    return str(v)


AUTO_WINDOW = "impaired"


def resolve_window(b: Bench, args, base_tags: dict, probe: int = 8) -> tuple:
    """(window, why). `auto` picks `impaired` when the runs in scope have one."""
    if args.window != "auto":
        w = "" if args.window == "run" else args.window
        return w, "explicit --window %s" % (args.window,)
    try:
        runs = b.runs(suite=args.suite, scenario=args.scenario, limit=probe,
                      tags=base_tags or None)
    except BenchError:
        return "", "auto: could not list runs, falling back to the whole run"
    hits = 0
    for r in runs[:probe]:
        try:
            phases = b.phases(r["id"])
        except BenchError:
            continue
        if any(p.get("phase") == AUTO_WINDOW for p in phases):
            hits += 1
    if hits:
        return AUTO_WINDOW, "auto: %d/%d probed runs have an `%s` phase" % (
            hits, len(runs[:probe]), AUTO_WINDOW)
    return "", "auto: no probed run has an `%s` phase — whole run" % AUTO_WINDOW


def main(argv=None) -> int:
    p = argparse.ArgumentParser(prog="bench_table.py", description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--metric", action="append", required=True)
    p.add_argument("--suite", default="")
    p.add_argument("--scenario", default="")
    p.add_argument("--since", default="")
    p.add_argument("--agg", default="p50")
    p.add_argument("--rows", default="tag.profile", help="tag key for table rows")
    p.add_argument("--cols", default="tag.abr_mode", help="tag key for table columns")
    p.add_argument("--window", default="auto",
                   help="phase to scope every aggregate to: auto (default; picks "
                        "`impaired` when the runs have one), run (whole run), or an "
                        "explicit phase name — baseline|impaired|settled|recovery")
    p.add_argument("--url", default=None)
    p.add_argument("--key", default=None)
    p.add_argument("--filter", action="append", default=[], metavar="TAG=VALUE",
                   help="repeatable extra tag filter applied to EVERY query — e.g. "
                        "--filter matrix=baseline-v2 to scope a table to one campaign "
                        "when the suite name is shared with earlier (withdrawn) grids")
    args = p.parse_args(argv)

    base_tags = {}
    for f in args.filter:
        if "=" not in f:
            p.error("--filter must be TAG=VALUE, got %r" % f)
        k, v = f.split("=", 1)
        base_tags[k] = v

    b = Bench(args.url, args.key)
    window, why = resolve_window(b, args, base_tags)

    for metric in args.metric:
        # One query grouped by rows, one grouped by cols, then one query per column
        # value filtered to that value and grouped by rows — the pivot the dashboard
        # does client-side.
        col_rows = b.stats(metric, args.cols, args.agg, window=window,
                           suite=args.suite, scenario=args.scenario, since=args.since,
                           tags=dict(base_tags) or None)
        cols = [r["group"] for r in col_rows if r["group"]]
        cols.sort()
        table: dict = {}
        for c in cols:
            filt = dict(base_tags)
            filt[args.cols.split(".", 1)[1]] = c
            rows = b.stats(metric, args.rows, args.agg, window=window,
                           suite=args.suite, scenario=args.scenario,
                           since=args.since, tags=filt)
            for r in rows:
                table.setdefault(r["group"], {})[c] = r["value"]

        print("### `%s` (%s, window=%s)\n" % (metric, args.agg, window or "run"))
        print("| %s | %s |" % (args.rows, " | ".join("%s=%s" % (args.cols, c) for c in cols)))
        print("|%s" % ("---|" * (len(cols) + 1)))
        for row in sorted(table):
            print("| %s | %s |" % (row, " | ".join(fmt(table[row].get(c)) for c in cols)))
        print()
        print("API: `GET %s/v1/stats?metric=%s&agg=%s&group_by=%s&suite=%s%s&window=%s%s`  (%s)\n"
              % (b.url, metric, args.agg, args.rows, args.suite,
                 ("&scenario=%s" % args.scenario) if args.scenario else "", window,
                 "".join("&tag.%s=%s" % kv for kv in sorted(base_tags.items())), why))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except BenchError as exc:
        print("error: %s" % exc, file=sys.stderr)
        sys.exit(1)
