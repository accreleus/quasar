#!/usr/bin/env python3
"""scripts/dx/bench_register_budget_metrics.py — register the g2g budget's
metric keys with quasar-bench (POST /v1/metrics), idempotently.

This is the one-time (and re-runnable) setup for the standing glass-to-glass
budget instrument (docs/testing-bench-mode.md "The glass-to-glass budget").
It registers direction (`better`) and a regression threshold for every stage
key the bench-mode fold emits — the `browser.stage_*` split
(docs/testing-bench-mode.md "The stage split"), the `agent.probe_*` host-stage
probe (armed per-run by `bench_run.sh --bench-mode`, see the latency_probe
override), and the `app.render_*` / `app.repaint_wait_*` / `app.submit_to_present_*`
keys benchapp's `commit_ms` split produces (scripts/dx/bench_app_samples.py).

`g2g_p50_ms` / `g2g_p95_ms` were already registered (lower, 10%) before this
script existed; it re-registers them so the whole budget's thresholds live in
one place and one command reproduces the registry from empty.

Registration keys are BARE (no `source.` prefix) — quasar-bench's metric
registry is keyed by the metric name alone; `source` (browser/agent/app) is a
query-time filter, not part of the registered key. `/v1/metrics` POST is an
upsert, so re-running this is always safe.

Usage:
    BENCH_URL=... BENCH_KEY=... scripts/dx/bench_register_budget_metrics.py [--dry-run]

Environment: BENCH_URL, BENCH_KEY (never committed — pull the harness key from
the stack's own deploy/.env at run time, per docs/testing-bench-mode.md).
"""

from __future__ import annotations

import argparse
import os
import sys

DX_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(DX_DIR, "vendor"))
sys.path.insert(0, DX_DIR)  # thresholds.py — see bench_submit.py's comment on this

from bench import Bench, BenchError  # noqa: E402
import thresholds  # noqa: E402  (scripts/dx/thresholds.py, same directory)

# (key, better, unit, regression_pct)
#
# 15% for the per-stage split (docs/testing-bench-mode.md table) — each stage
# is a smaller, noisier slice of the budget than the headline g2g figure, so a
# tighter percentage than g2g's would false-positive on ordinary run-to-run
# jitter (REPORT.md section 3: 5ms of g2g spread across three clean local
# cells, all of it in the receive path).
STAGE_PCT = 15.0
# 10% for the two headline glass-to-glass numbers — already the registered
# default for these two keys; kept explicit here so the whole budget's
# thresholds are declared in one place.
G2G_PCT = 10.0
# C11 #1/#3: the AS-04/#108 present-cadence keys bench_submit.py's ROLLUP_KEYS
# now folds (present_fps_median replacing deprecated present_fps,
# present_beat_fraction, present_long_frames, present_interval_max_ms,
# present_n) and agent.encode_ms_max, registered here too so they get a
# direction and show up in /v1/metrics and /v1/regressions. `regression_pct`
# stays the house STAGE_PCT convention — it is a PERCENT-of-baseline band,
# a different unit from thresholds.json's absolute ms/fps/count health lines,
# so a threshold value from there cannot be substituted in directly. What
# DOES come from thresholds.json (via scripts/dx/thresholds.py) is a check
# that the direction picked below still agrees with the classifier's own
# reading of "worse": both are printed so a --dry-run makes the citation
# visible rather than a silent assumption.
PRESENT_PCT = STAGE_PCT
_HITCH_SD_MS = thresholds.value("classifier.hitch_sd_ms")            # 18 ms: higher present_interval_{sd,max}_ms is worse
_ENCODER_CEILING_MS = thresholds.value("classifier.encoder_ceiling_ms")  # 16 ms: higher encode_ms_max is worse

METRICS: list[tuple[str, str, str, float]] = [
    # ── browser.stage_* — the A/B/C split (docs/testing-bench-mode.md) ──────
    ("stage_host_to_receive_p50_ms", "lower", "ms", STAGE_PCT),
    ("stage_host_to_receive_p95_ms", "lower", "ms", STAGE_PCT),
    ("stage_receive_to_present_p50_ms", "lower", "ms", STAGE_PCT),
    ("stage_receive_to_present_p95_ms", "lower", "ms", STAGE_PCT),
    ("stage_decode_p50_ms", "lower", "ms", STAGE_PCT),
    ("stage_decode_p95_ms", "lower", "ms", STAGE_PCT),
    ("stage_wait_queue_p50_ms", "lower", "ms", STAGE_PCT),
    ("stage_wait_queue_p95_ms", "lower", "ms", STAGE_PCT),
    # headless caveat (testing-bench-mode.md): can legitimately read ~0 or
    # slightly negative on a headless peer — still "lower is better" in
    # direction, just with a wide realistic band; the % threshold, not the
    # sign, is what keeps this from false-positiving.
    ("stage_present_to_display_p50_ms", "lower", "ms", STAGE_PCT),
    ("stage_present_to_display_p95_ms", "lower", "ms", STAGE_PCT),
    ("stage_render_queue_derived_p50_ms", "lower", "ms", STAGE_PCT),
    ("stage_reconcile_p95_ms", "lower", "ms", STAGE_PCT),
    ("stage_jb_mean_ms", "lower", "ms", STAGE_PCT),
    ("stage_jb_target_mean_ms", "lower", "ms", STAGE_PCT),
    ("stage_assembly_mean_ms", "lower", "ms", STAGE_PCT),
    ("stage_processing_mean_ms", "lower", "ms", STAGE_PCT),
    ("stage_decode_stats_mean_ms", "lower", "ms", STAGE_PCT),
    ("stage_interframe_mean_ms", "lower", "ms", STAGE_PCT),
    ("stage_frames_dropped", "lower", "frames", STAGE_PCT),
    # counts describing HOW MANY frames carried a stage, not a quality signal
    # by themselves — flagging these as regressions would just track window
    # length / frame rate, so they are registered neutral (never flagged) but
    # still declared, so an operator reading /v1/metrics sees every stage key
    # accounted for rather than wondering why half of them are missing.
    ("stage_n", "neutral", "frames", STAGE_PCT),
    ("stage_host_to_receive_n", "neutral", "frames", STAGE_PCT),
    ("stage_decode_n", "neutral", "frames", STAGE_PCT),
    ("stage_frames_decoded", "neutral", "frames", STAGE_PCT),

    # ── browser.g2g_* — the headline figures ─────────────────────────────────
    ("g2g_p50_ms", "lower", "ms", G2G_PCT),
    ("g2g_p95_ms", "lower", "ms", G2G_PCT),

    # ── agent.probe_* — the host-stage probe (armed by --bench-mode) ────────
    ("probe_capture_to_enc_in_p50_ms", "lower", "ms", STAGE_PCT),
    ("probe_capture_to_enc_in_p95_ms", "lower", "ms", STAGE_PCT),
    ("probe_enc_out_to_send_p50_ms", "lower", "ms", STAGE_PCT),
    ("probe_enc_out_to_send_p95_ms", "lower", "ms", STAGE_PCT),
    ("probe_pay_to_send_p50_ms", "lower", "ms", STAGE_PCT),
    ("probe_pay_to_send_p95_ms", "lower", "ms", STAGE_PCT),
    ("probe_pts_to_emit_p50_ms", "lower", "ms", STAGE_PCT),
    ("probe_pts_to_emit_p95_ms", "lower", "ms", STAGE_PCT),
    # a fixed compositor cadence (~16.67ms at 60Hz), not a latency — direction
    # is not meaningful, so neutral (declared, never flagged).
    ("probe_compositor_frame_interval_p95_ms", "neutral", "ms", STAGE_PCT),
    ("probe_send_desyncs", "lower", "count", STAGE_PCT),
    ("probe_pts_unmatched", "lower", "count", STAGE_PCT),

    # ── app.* — benchapp's commit_ms split (docs/testing-bench-mode.md
    #    "The app -> compositor head, split by owner") ────────────────────────
    ("submit_to_present_p50_ms", "lower", "ms", STAGE_PCT),
    ("submit_to_present_p95_ms", "lower", "ms", STAGE_PCT),
    ("render_p50_ms", "lower", "ms", STAGE_PCT),
    ("render_p95_ms", "lower", "ms", STAGE_PCT),
    ("repaint_wait_p50_ms", "lower", "ms", STAGE_PCT),
    ("repaint_wait_p95_ms", "lower", "ms", STAGE_PCT),

    # ── browser.present_* — the AS-04/#108 cadence keys (C11 #1) ────────────
    # present_fps_median is THE reading (metrics.json deprecated_for on
    # present_fps): higher is better, same as any other fps key.
    ("present_fps_median", "higher", "fps", PRESENT_PCT),
    # Judder magnitude — same threshold family as classifier.hitch_sd_ms
    # (18 ms): lower is better for both the per-window sigma and the worst
    # single interval in the window.
    ("present_interval_sd_ms", "lower", "ms", PRESENT_PCT),
    ("present_interval_max_ms", "lower", "ms", PRESENT_PCT),
    # Share of intervals sitting on the inherent vsync beat (~2x median) — not
    # itself a defect signal (present_long_frames is the one that is), so
    # neutral: declared for visibility, never flagged.
    ("present_beat_fraction", "neutral", "fraction", PRESENT_PCT),
    # Genuine stalls (>2.5x median) in the window — fewer is better.
    ("present_long_frames", "lower", "count", PRESENT_PCT),
    # How many intervals the window held — a sample-count key like stage_n,
    # not a quality signal by itself.
    ("present_n", "neutral", "count", PRESENT_PCT),

    # ── agent.encode_ms_max — the one-frame-stall key (C11 #1) ──────────────
    # Same family as classifier.encoder_ceiling_ms (16 ms, one frame at 60fps):
    # lower is better.
    ("encode_ms_max", "lower", "ms", PRESENT_PCT),
]


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--dry-run", action="store_true",
                   help="print the plan and stop — nothing POSTed")
    p.add_argument("--url", default=None)
    p.add_argument("--key", default=None)
    args = p.parse_args(argv)

    print("     thresholds classifier.hitch_sd_ms=%s classifier.encoder_ceiling_ms=%s "
          "(docs/session-trace/thresholds.json, via scripts/dx/thresholds.py)"
          % (_HITCH_SD_MS, _ENCODER_CEILING_MS))

    if args.dry_run:
        for key, better, unit, pct in METRICS:
            print("would register %-38s better=%-8s unit=%-7s regression_pct=%s" %
                 (key, better, unit, pct))
        print("\n%d metric keys (dry-run, nothing POSTed)" % len(METRICS))
        return 0

    b = Bench(args.url, args.key)
    ok, failed = 0, []
    for key, better, unit, pct in METRICS:
        try:
            b.register_metric(key, better, unit, pct)
            print("registered %-38s better=%-8s unit=%-7s regression_pct=%s" %
                 (key, better, unit, pct))
            ok += 1
        except BenchError as exc:
            print("FAILED %-38s %s" % (key, exc), file=sys.stderr)
            failed.append(key)

    print("\n%d/%d metric keys registered" % (ok, len(METRICS)))
    if failed:
        print("failed: %s" % ", ".join(failed), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
