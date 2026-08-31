#!/usr/bin/env python3
"""scripts/dx/bench_budget.py — the reconciled glass-to-glass budget table for
ONE run, read back from quasar-bench.

    scripts/dx/bench_budget.py --run <run-id>
    scripts/dx/bench_budget.py --run latest --suite latency-budget
    make bench-budget RUN=<id>|latest [HOST=devbox]

This is the standing form of `docs/reports/2026-08-19-latency-budget/REPORT.md`
section 2 — the same stage names, the same A+B+C reconciliation, but computed
from whatever the run actually measured rather than transcribed once by hand.
Every stage prints its measured p50/p95, its delta against the pinned baseline
(`/v1/regressions`, which already carries the metric's registered direction and
threshold — see `bench_register_budget_metrics.py`), and a flag. Exit is
non-zero when ANY stage flagged, so this can gate a run (`bench_run.sh` calls it
at the end of every bench-mode run; `make bench-budget` is the same call by
hand).

Each stage's number and threshold come straight from `GET /v1/regressions`
(one call per stage per agg) rather than being recomputed here — the service
already owns "is this a regression", including the metric's registered
direction (a `neutral` stage, e.g. `stage_n`, never flags). This script is a
presentation + gate over that call, not a second implementation of it.

A run with no pinned baseline for its (suite, scenario, --baseline name) prints
the table with every Δ column as "no baseline" and exits 0 — a budget run
against a suite nobody has baselined yet is informative, not a failure.

Environment: BENCH_URL, BENCH_KEY (never committed — pull the harness key from
the stack's own deploy/.env at run time, per docs/testing-bench-mode.md).
"""

from __future__ import annotations

import argparse
import os
import sys

DX_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(DX_DIR, "vendor"))

from bench import Bench, BenchError  # noqa: E402

DEFAULT_BASELINE = "latency-budget/1080p60-h264-local"

# (label, source, key) — same stage names as
# docs/reports/2026-08-19-latency-budget/REPORT.md section 2, in the report's
# own order. `key` is the BARE metric key (source is a separate dimension —
# see bench_register_budget_metrics.py).
STAGE_ROWS: list[tuple[str, str, str]] = [
    ("A  app submit -> last packet (host_to_receive)", "browser", "stage_host_to_receive_p50_ms"),
    ("B  last packet -> composition submit (receive_to_present)", "browser", "stage_receive_to_present_p50_ms"),
    ("B1 frame assembly", "browser", "stage_assembly_mean_ms"),
    ("B2 receiver jitter buffer", "browser", "stage_jb_mean_ms"),
    ("B3 decode", "browser", "stage_decode_p50_ms"),
    ("B4 Chrome post-decode render queue (derived)", "browser", "stage_render_queue_derived_p50_ms"),
    ("C  composition submit -> display (present_to_display)", "browser", "stage_present_to_display_p50_ms"),
    ("   wait_queue (jb + render queue)", "browser", "stage_wait_queue_p50_ms"),
    ("   self-check: stage_reconcile", "browser", "stage_reconcile_p95_ms"),
    ("app render + GPU submit", "app", "render_p50_ms"),
    ("app repaint wait (compositor)", "app", "repaint_wait_p50_ms"),
    ("agent probe: capture -> encoder in", "agent", "probe_capture_to_enc_in_p50_ms"),
    ("agent probe: pts -> emit", "agent", "probe_pts_to_emit_p50_ms"),
    ("agent probe: encoder out -> send", "agent", "probe_enc_out_to_send_p50_ms"),
    ("agent probe: payload -> send", "agent", "probe_pay_to_send_p50_ms"),
]

# The three top-level components that must reconcile to the measured g2g
# (docs/testing-bench-mode.md "The stage split" — an identity, not a fit).
RECONCILE_KEYS = ("stage_host_to_receive_p50_ms", "stage_receive_to_present_p50_ms",
                  "stage_present_to_display_p50_ms")
G2G_KEY = "g2g_p50_ms"


def fmt(v) -> str:
    if v is None:
        return "—"
    return "%.2f" % v


def resolve_run(b: Bench, run: str, suite: str) -> dict:
    if run != "latest":
        return b.run(run)
    runs = b.runs(suite=suite, limit=1) if suite else b.runs(limit=1)
    if not runs:
        raise BenchError("no runs found for --run latest" + (" in suite %r" % suite if suite else ""))
    return b.run(runs[0]["id"])


def regress_row(b: Bench, suite: str, scenario: str, run_id: str, metric: str,
                agg: str, baseline: str, window: str) -> dict | None:
    """One stage's {value, baseline_value, delta_pct, regressed, better,
    threshold_pct} for run_id, or None if the run/metric is not in the
    response (no baseline pinned, or the metric never got a sample this run).
    """
    try:
        res = b.regressions(suite, scenario, metric, agg=agg, window=window, baseline=baseline)
    except BenchError:
        return None
    for row in res.get("runs") or []:
        if row.get("run_id") == run_id:
            row["better"] = row.get("better", res.get("better"))
            return row
    return None


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--run", required=True, help="run id, or 'latest'")
    p.add_argument("--suite", default="latency-budget",
                   help="suite to scope 'latest'/baseline lookups to (default: latency-budget)")
    p.add_argument("--scenario", default="",
                   help="override the run's own scenario for the baseline lookup "
                        "(rarely needed — the run's own scenario is used by default)")
    p.add_argument("--baseline", default=DEFAULT_BASELINE,
                   help="pinned baseline name (default: %s)" % DEFAULT_BASELINE)
    p.add_argument("--window", default="observe",
                   help="phase window to scope the comparison to (default: observe)")
    p.add_argument("--url", default=None)
    p.add_argument("--key", default=None)
    p.add_argument("--json", action="store_true", help="also print the raw rows as JSON")
    args = p.parse_args(argv)

    b = Bench(args.url, args.key)
    try:
        run = resolve_run(b, args.run, args.suite)
    except BenchError as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 2

    run_id = run["id"]
    scenario = args.scenario or run.get("scenario") or ""
    suite = run.get("suite") or args.suite

    print("run      %s" % run_id)
    print("suite    %s   scenario %s" % (suite, scenario))
    print("baseline %s   window %s\n" % (args.baseline, args.window))

    rows_out = []
    any_regressed = False
    any_baseline = False
    reconcile_vals: dict[str, float] = {}

    header = "%-52s %8s %8s %10s %10s  %s" % ("stage", "p50", "p95", "vs base", "thresh", "flag")
    print(header)
    print("-" * len(header))

    for label, source, key in STAGE_ROWS:
        metric = "%s.%s" % (source, key)
        r50 = regress_row(b, suite, scenario, run_id, metric, "p50", args.baseline, args.window)
        p95_key = key.replace("_p50_", "_p95_") if "_p50_" in key else key
        r95 = None
        if p95_key != key:
            r95 = regress_row(b, suite, scenario, run_id, "%s.%s" % (source, p95_key), "p95",
                              args.baseline, args.window)

        p50 = r50["value"] if r50 else None
        p95 = r95["value"] if r95 else None
        if key in RECONCILE_KEYS and p50 is not None:
            reconcile_vals[key] = p50

        if r50 is None:
            delta = "—"
            thresh = "—"
            flag = "no data"
        elif r50.get("better") == "neutral":
            delta = "%+.1f%%" % r50["delta_pct"]
            thresh = "n/a"
            flag = "neutral"
        else:
            any_baseline = True
            delta = "%+.1f%%" % r50["delta_pct"]
            thresh = "%.0f%%" % r50.get("threshold_pct", 0)
            if r50.get("regressed"):
                flag = "REGRESSED"
                any_regressed = True
            else:
                flag = "ok"

        print("%-52s %8s %8s %10s %10s  %s" % (label, fmt(p50), fmt(p95), delta, thresh, flag))
        rows_out.append({"label": label, "metric": metric, "p50": p50, "p95": p95,
                         "regression": r50})

    # g2g headline, same treatment
    g_metric = "browser.%s" % G2G_KEY
    g50 = regress_row(b, suite, scenario, run_id, g_metric, "p50", args.baseline, args.window)
    g95 = regress_row(b, suite, scenario, run_id, "browser.g2g_p95_ms", "p95", args.baseline, args.window)
    g_val = g50["value"] if g50 else None
    if g50:
        any_baseline = True
        g_delta = "%+.1f%%" % g50["delta_pct"]
        g_thresh = "%.0f%%" % g50.get("threshold_pct", 0)
        g_flag = "REGRESSED" if g50.get("regressed") else "ok"
        if g50.get("regressed"):
            any_regressed = True
    else:
        g_delta = g_thresh = "—"
        g_flag = "no data"
    print("-" * len(header))
    print("%-52s %8s %8s %10s %10s  %s" % ("measured g2g (headline)", fmt(g_val),
                                           fmt(g95["value"] if g95 else None), g_delta, g_thresh, g_flag))

    print()
    if len(reconcile_vals) == len(RECONCILE_KEYS) and g_val is not None:
        total = sum(reconcile_vals.values())
        residual = g_val - total
        print("reconcile: A+B+C = %.2f ms   measured g2g = %.2f ms   residual = %.2f ms"
             % (total, g_val, residual))
    else:
        print("reconcile: incomplete (missing %s)" %
             ", ".join(k for k in RECONCILE_KEYS if k not in reconcile_vals))

    if not any_baseline:
        print("\nno pinned baseline participated in this comparison — every "
             "delta above is unscored (run `make bench-baseline RUN=%s NAME=<suite/scenario>` "
             "to pin one)." % run_id)

    if args.json:
        import json as _json
        print("\n" + _json.dumps(rows_out, indent=2))

    print()
    if any_regressed:
        print("RESULT status=failed target=bench-budget run_id=%s regressed=1" % run_id)
        return 1
    print("RESULT status=ok target=bench-budget run_id=%s regressed=0" % run_id)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except BenchError as exc:
        print("error: %s" % exc, file=sys.stderr)
        sys.exit(2)
