#!/usr/bin/env python3
"""scripts/harness/lib/soak_report.py — PROF-03 soak CSV -> leak verdicts + HTML report.

Stdlib only (no numpy/pandas — this has to run on a bare Tower/hermes host with
whatever python3 docker gives us, and on the operator's Mac with none of those
installed either).

Usage:
    python3 scripts/harness/lib/soak_report.py <csv_path> [--html PATH] [--json PATH]
    python3 scripts/harness/lib/soak_report.py --selftest

Input CSV columns (see scripts/harness/run-soak-profile.sh, exact order):
    cycle, ts_iso, cycle_ok, session_id, launch_to_running_s,
    cp_goroutines, cp_heap_inuse_bytes, cp_heap_objects, cp_heap_alloc_bytes,
    cp_sys_bytes, cp_num_gc, cp_rss_kb, cp_fds, cp_uptime_s,
    pool_acquired, pool_idle, pool_total, pool_empty_acquire_count,
    db_sessions, db_session_metrics, db_trace_events, db_auth_tokens,
    db_session_tokens, db_admin_activity,
    agent_rss_kb, agent_threads, agent_fds, vram_used_mb, gst_alive,
    agent_uptime_s

cp_uptime_s and agent_uptime_s are META columns (like ts_iso) — neither is
ever itself classified as a leak series (each climbs by construction between
restarts). They exist purely to detect a CONTROL-PLANE or NODE-AGENT
container restart mid-run: a restart resets that container's process uptime
to ~0, and a restart is itself a leak-candidate signal, not noise to average
over (issue #420: an agent-only restart — e.g. the Tower 04:01 backup
restarting the whole stack — used to be invisible, because only cp_uptime_s
was ever sampled; agent fd/RSS baselines silently reset with no segmentation
banner). When a restart on EITHER container (or a counter-column reset, e.g.
a DB row count going backwards — also only possible via a restart/rollback)
is detected, verdicts are computed on the LONGEST contiguous segment only,
and the run is flagged RESTARTED (see run_report / render_html).
`agent_uptime_s` is appended at the END of the CSV schema (issue #420) so
report-only mode over an OLD CSV written before this column existed still
works: it is simply absent from that CSV's fieldnames/columns, and every
agent_uptime_s-aware code path here treats a missing/empty cell as "no
signal", never a fabricated restart.

Verdict vocabulary (gauges): LEAK, PLATEAU, STEP, FLAT_CLEAN, FLAT_NOISY,
DOWNWARD, SUSPECT, INSUFFICIENT.
Verdict vocabulary (monotonic counters): GROWING_BY_DESIGN, ACCELERATING,
INSUFFICIENT.

WHY two vocabularies: pool_empty_acquire_count, cp_num_gc, and every db_*
column are monotonic COUNTERS by construction — db_sessions grows by design
every cycle because every cycle inserts a session row. A row-count series
growing linearly is not evidence of a leak; it is the schema working exactly
as intended. Calling that "LEAK" would train an operator to ignore soak
reports. So counters get their own, narrower classifier: report the observed
growth rate and only escalate if the rate itself is accelerating (each cycle
adding a growing amount of rows/count, which WOULD be unusual) or is
implausibly large. Everything else about a counter series is expected.

STEP exists because a one-time level shift (something allocated once, not
per cycle — e.g. a cache warming to its steady-state size) can pass a naive
"rising + significant + growth-above-floor" gate. STEP is detected by a
brute-force single-changepoint check: if splitting the series into two flat
(mean) halves explains the data far better than the overall linear trend, it
is a level shift, not a per-cycle leak, and is reported separately (yellow,
not red) with guidance to investigate what allocated once.
"""

from __future__ import annotations

import argparse
import csv
import html
import json
import math
import os
import random
import statistics
import sys
import tempfile

# ── column classification ───────────────────────────────────────────────────

# Columns that are never analysed as a "series" — identifiers, timestamps, or
# per-cycle scalars/meta that aren't a leak signal in themselves.
# cp_uptime_s / agent_uptime_s are meta (restart detection only, see module
# docstring and issue #420 — the agent side of the same mechanism).
NON_SERIES_COLUMNS = {
    "cycle", "ts_iso", "cycle_ok", "session_id", "launch_to_running_s",
    "cp_uptime_s", "agent_uptime_s",
}

# Monotonic-by-construction counters. See module docstring for why these get
# a different verdict vocabulary than gauges.
COUNTER_COLUMNS = {
    "pool_empty_acquire_count",
    "cp_num_gc",
    "db_sessions",
    "db_session_metrics",
    "db_trace_events",
    "db_auth_tokens",
    "db_session_tokens",
    "db_admin_activity",
}

# Noise floors: the absolute per-run growth (fitted(end) - fitted(start))
# below which we do not care, even if the slope is "statistically
# significant" (a long, dead-flat run can still produce t >= 4 on genuinely
# tiny jitter — the floor is what keeps that from being reported as a leak).
#
# These are the 2h-TIER sensitivity. An 8h run does NOT get to halve these:
# more cycles buys more statistical POWER (a smaller real slope becomes
# detectable), not a smaller noise floor — the floor is about what we care
# about in absolute terms (bytes/handles/threads), not about how confident
# we are. Keep the floors fixed across run lengths; let t (which scales with
# sqrt(n) roughly) do the escalating.
NOISE_FLOOR = {
    "cp_goroutines": 5,
    "cp_fds": 5,
    "agent_fds": 5,
    "agent_threads": 4,
    "cp_heap_inuse_bytes": 8 * 1024 * 1024,
    "cp_heap_alloc_bytes": 8 * 1024 * 1024,
    "cp_heap_objects": 5000,
    "cp_sys_bytes": 16 * 1024 * 1024,
    "cp_rss_kb": 16 * 1024,       # rss is tracked in KB, floor stated in MiB
    "agent_rss_kb": 16 * 1024,
    "vram_used_mb": 64,
    "pool_acquired": 2,
    "pool_idle": 2,
    "pool_total": 2,
    # gst_alive (live GStreamer object count, --gst-leaks only) has no
    # spec'd floor — a handful of live objects is normal churn between
    # cycles (pad probes, caps events). Use a conservative default and say
    # so; tighten once we have real gst_alive data from a soak run.
    "gst_alive": 5,
}
# Fallback floor for any gauge series not listed above (defensive — every
# gauge column in the CSV schema IS listed above, but a future column
# addition should not silently get floor=0 and flag every jitter as a leak).
DEFAULT_NOISE_FLOOR = 1.0

ESCALATION_RULE = (
    "2h flat and clean: clear at this sensitivity. 2h flat but noisy, or "
    "slope present but not significant: rerun at 8h / ~150 cycles. Before a "
    "release tag: one 8h run per host regardless."
)

# ── least-squares helpers (stdlib only) ─────────────────────────────────────


def _ls_fit(xs, ys):
    """Least-squares fit y = a + b*x. Returns (a, b, s_res, se, t, sxx).

    s_res is the residual standard deviation (n-2 degrees of freedom, or 0 if
    n<3). se is the standard error of the slope. t is the slope's t-statistic
    (b/se); it is +inf/-inf if se==0 and b!=0 (perfectly linear, may happen
    with e.g. two points), or 0 if b==0 too (constant line).
    """
    n = len(xs)
    xbar = sum(xs) / n
    ybar = sum(ys) / n
    sxx = sum((x - xbar) ** 2 for x in xs)
    sxy = sum((x - xbar) * (y - ybar) for x, y in zip(xs, ys))
    if sxx == 0:
        return ybar, 0.0, 0.0, float("inf"), 0.0, 0.0
    b = sxy / sxx
    a = ybar - b * xbar
    if n > 2:
        resid_ss = sum((y - (a + b * x)) ** 2 for x, y in zip(xs, ys))
        s_res = math.sqrt(resid_ss / (n - 2))
    else:
        s_res = 0.0
    if sxx > 0 and s_res > 0:
        se = s_res / math.sqrt(sxx)
    else:
        se = 0.0
    if se > 0:
        t = b / se
    elif b == 0:
        t = 0.0
    else:
        t = math.inf if b > 0 else -math.inf
    return a, b, s_res, se, t, sxx


def _monotonic_fraction(ys):
    """Fraction of consecutive deltas (in the pointwise-available sequence)
    that are >= 0. Ties count as non-decreasing. A single point has no
    deltas to judge -> defined as 1.0 (nothing contradicts monotonicity).
    Reported as a statistic only — it no longer gates any verdict (see
    classify_gauge; a GC-noisy series can be a real leak while flipping sign
    on most consecutive deltas)."""
    if len(ys) < 2:
        return 1.0
    nonneg = sum(1 for a, b in zip(ys, ys[1:]) if b - a >= 0)
    return nonneg / (len(ys) - 1)


def _subrange_slope(xs, ys):
    """LS slope over a sub-range, or None if n<5 (plateau-check guard).
    Used by classify_counter's accelerating check."""
    if len(xs) < 5:
        return None
    _, b, _, _, _, _ = _ls_fit(xs, ys)
    return b


def _subrange_fit(xs, ys):
    """LS (slope, se, t) over a sub-range, or None if n<5 (plateau-check
    guard — an unmeasurable sub-range must never block a LEAK verdict)."""
    if len(xs) < 5:
        return None
    _, b, _, se, t, _ = _ls_fit(xs, ys)
    return b, se, t


def _changepoint_variance_reduction(xs, ys):
    """Brute-force the best single interior changepoint splitting ys into two
    flat (mean) segments. Returns (best_pooled_ss, cycle_at_split) or
    (None, None) if there aren't enough points to test (need >=2 each side).
    """
    n = len(ys)
    if n < 4:
        return None, None
    best_ss = None
    best_idx = None
    for i in range(2, n - 1):
        left, right = ys[:i], ys[i:]
        ml = sum(left) / len(left)
        mr = sum(right) / len(right)
        ss = sum((y - ml) ** 2 for y in left) + sum((y - mr) ** 2 for y in right)
        if best_ss is None or ss < best_ss:
            best_ss = ss
            best_idx = i
    if best_idx is None:
        return None, None
    return best_ss, xs[best_idx]


# ── gauge classification ────────────────────────────────────────────────────


def classify_gauge(xs, ys, series_name):
    """Classify a gauge series. xs = cycle indices, ys = values, pointwise
    (already excludes missing cells). Returns a dict with verdict + stats."""
    n = len(xs)
    if n < 10:
        return {
            "verdict": "INSUFFICIENT",
            "start": None,
            "end": None,
            "slope": None,
            "t": None,
            "monotonic_frac": None,
            "n": n,
            "detail": f"only {n} usable points (<10) — not enough to judge",
        }

    a, b, s_res, se, t, sxx = _ls_fit(xs, ys)
    x0, xend = xs[0], xs[-1]
    fitted_start = a + b * x0
    fitted_end = a + b * xend
    total_growth = fitted_end - fitted_start
    mean_y = sum(ys) / n
    cv = (s_res / max(abs(mean_y), 1)) if s_res else 0.0
    m = _monotonic_fraction(ys)
    floor = NOISE_FLOOR.get(series_name, DEFAULT_NOISE_FLOOR)

    # Plateau check (fix #3): the old rule (b_last < 0.25*b_first) misfiled
    # ~10% of real linear leaks as PLATEAU on small-sample noise in b_last.
    # New rule needs BOTH a decayed-and-uncertain last-third slope AND a
    # last-third trend that is not itself significant. Guarded by >=5 points
    # per sub-range; an unmeasurable sub-range never blocks a LEAK verdict.
    split = max(1, (2 * n) // 3)
    first_fit = _subrange_fit(xs[:split], ys[:split])
    last_fit = _subrange_fit(xs[split:], ys[split:])
    plateaued = False
    b_first = b_last = se_last = t_last = None
    if first_fit is not None and last_fit is not None:
        b_first, _, _ = first_fit
        b_last, se_last, t_last = last_fit
        if b_first > 0:
            plateaued = (b_last + 2 * se_last) < 0.25 * b_first and abs(t_last) < 2

    # Noise-robust monotone-trend confirmation (fix #2), replacing the
    # monotonic-fraction gate entirely: compares the median of the last
    # quartile of points to the median of the first quartile. A GC-sawtooth
    # series can flip sign on most consecutive deltas while the underlying
    # level is genuinely, steadily climbing — the quartile-median comparison
    # shrugs off that per-step noise and looks at the net level shift.
    q = max(1, n // 4)
    first_med = statistics.median(ys[:q])
    last_med = statistics.median(ys[-q:])
    quartile_delta = last_med - first_med
    quartile_confirmed = quartile_delta > max(floor / 2.0, s_res)

    # Step / one-time level-shift check (fix #6): brute-force the best single
    # interior changepoint splitting the series into two flat (mean) halves;
    # if that explains the data far better than the overall linear trend,
    # this is a one-time allocation, not a per-cycle leak. Runs independently
    # of b_first / monotonicity so a flat-then-jump-then-flat series can
    # never read LEAK even though the quartile-median check above would pass
    # it (a step IS a net level shift between the first and last quartile).
    resid_ss_linear = (s_res ** 2) * (n - 2) if n > 2 else 0.0
    is_step = False
    step_cycle = None
    step_reduction = None
    if resid_ss_linear > 0:
        best_ss, cp_cycle = _changepoint_variance_reduction(xs, ys)
        if best_ss is not None:
            step_reduction = 1 - (best_ss / resid_ss_linear)
            if step_reduction > 0.70:
                is_step = True
                step_cycle = cp_cycle

    t_abs = abs(t) if math.isfinite(t) else math.inf

    verdict = None
    detail = ""

    if is_step:
        verdict = "STEP"
        detail = (
            "one-time level shift, not a per-cycle leak; investigate what "
            f"allocated once (best changepoint near cycle {step_cycle}, "
            f"residual variance reduced {step_reduction:.0%} vs the linear fit)"
        )
    elif t >= 4 and total_growth > floor and not plateaued and quartile_confirmed:
        verdict = "LEAK"
        detail = (
            f"rising, significant (t={t:.2f}), growth={total_growth:.1f} > "
            f"floor {floor}, quartile-median delta={quartile_delta:.1f} "
            f"(monotonic {m:.0%}, reported not gating)"
        )
    elif t >= 4 and total_growth > floor and plateaued:
        verdict = "PLATEAU"
        detail = (
            f"rose then flattened (t={t:.2f} overall; last-third slope "
            f"{b_last:.3f} + 2*se({se_last:.3f}) < 25% of first-two-thirds "
            f"slope {b_first:.3f}, and last-third trend not significant "
            f"t={t_last:.2f}), growth={total_growth:.1f}"
        )
    elif t <= -4:
        verdict = "DOWNWARD"
        detail = f"falling, significant (t={t:.2f}) — unusual, worth a look"
    elif t_abs < 2 and cv < 0.05:
        verdict = "FLAT_CLEAN"
        detail = f"no trend (t={t:.2f}), low residual CV ({cv:.3f})"
    elif t >= 4:
        # Significant slope that failed the LEAK gate on growth magnitude or
        # the quartile-median trend confirmation. Report only the reason(s)
        # that actually failed (fix #14 — don't print an unconditional list
        # of every possible cause).
        reasons = []
        if not (total_growth > floor):
            reasons.append(f"growth {total_growth:.1f} <= floor {floor}")
        if not quartile_confirmed:
            reasons.append(
                f"quartile-median delta {quartile_delta:.1f} did not clear "
                "the noise-robust threshold"
            )
        if not reasons:
            reasons.append("did not clear the LEAK gate")
        verdict = "SUSPECT"
        detail = (
            f"statistically significant slope (t={t:.2f}) but "
            f"{'; '.join(reasons)} — {ESCALATION_RULE}"
        )
    elif 2 <= t < 4 and total_growth > floor:
        verdict = "SUSPECT"
        detail = (
            f"borderline rising slope (t={t:.2f}), growth={total_growth:.1f} "
            f"> floor {floor} — {ESCALATION_RULE}"
        )
    else:
        verdict = "FLAT_NOISY"
        detail = f"no significant trend (t={t:.2f}) but noisy (CV={cv:.3f}) — {ESCALATION_RULE}"

    return {
        "verdict": verdict,
        "start": fitted_start,
        "end": fitted_end,
        "slope": b,
        "t": t if math.isfinite(t) else None,
        "monotonic_frac": m,
        "n": n,
        "detail": detail,
        "values": ys,
        "xs": xs,
    }


# ── counter classification ──────────────────────────────────────────────────


def classify_counter(xs, ys, series_name):
    n = len(xs)
    if n < 10:
        return {
            "verdict": "INSUFFICIENT",
            "start": None,
            "end": None,
            "slope": None,
            "t": None,
            "monotonic_frac": None,
            "n": n,
            "detail": f"only {n} usable points (<10) — not enough to judge",
        }

    a, b, s_res, se, t, sxx = _ls_fit(xs, ys)
    x0, xend = xs[0], xs[-1]
    fitted_start = a + b * x0
    fitted_end = a + b * xend
    m = _monotonic_fraction(ys)

    split = max(1, (2 * n) // 3)
    b_first = _subrange_slope(xs[:split], ys[:split])
    b_last = _subrange_slope(xs[split:], ys[split:])
    accelerating = False
    if b_first is not None and b_last is not None and b_first > 0:
        accelerating = b_last > 1.5 * b_first

    if accelerating:
        verdict = "ACCELERATING"
        detail = (
            f"per-cycle growth rate increased ({b_first:.2f}/cycle -> "
            f"{b_last:.2f}/cycle) — counters are expected to grow linearly, "
            f"not accelerate; worth checking for an unbounded-batch cause"
        )
    else:
        verdict = "GROWING_BY_DESIGN"
        detail = f"grows ~{b:.2f}/cycle by design (expected for a counter/row-count series)"

    return {
        "verdict": verdict,
        "start": fitted_start,
        "end": fitted_end,
        "slope": b,
        "t": t if math.isfinite(t) else None,
        "monotonic_frac": m,
        "n": n,
        "detail": detail,
        "values": ys,
        "xs": xs,
    }


# ── CSV ingestion ───────────────────────────────────────────────────────────


def read_csv_rows(csv_path):
    """Parse the CSV once. Returns (rows, fieldnames, malformed_count).

    rows: list of raw-string dicts (one per well-formed data row), each
    augmented with parsed "_cycle" (int) and "_cycle_ok" (bool). A row with
    MORE fields than the header (csv.DictReader stashes the overflow under
    the None key) is REJECTED outright — never silently name-mapped, since a
    shifted-column row would poison every series it touches. Rejected rows
    are counted in malformed_count and excluded from `rows` entirely. A row
    whose `cycle` cell doesn't parse as an int is also dropped (pre-existing
    behaviour), but not counted as malformed (that's a different failure
    mode - an unparseable cycle number, not a width mismatch).
    """
    with open(csv_path, newline="", encoding="utf-8") as fh:
        reader = csv.DictReader(fh)
        fieldnames = reader.fieldnames or []
        rows = []
        malformed = 0
        for raw_row in reader:
            if raw_row.get(None):
                malformed += 1
                continue
            cycle_raw = raw_row.get("cycle", "")
            try:
                cycle = int(cycle_raw)
            except (TypeError, ValueError):
                continue
            cycle_ok = str(raw_row.get("cycle_ok", "")).strip() == "1"
            row = dict(raw_row)
            row["_cycle"] = cycle
            row["_cycle_ok"] = cycle_ok
            rows.append(row)
    return rows, fieldnames, malformed


# Uptime meta-columns that flag a container restart on decrease, and the
# human label used in the banner/verdicts for "which container restarted"
# (issue #420 — agent_uptime_s is the agent side of the cp_uptime_s
# mechanism; the CSV carries no docker container NAME, only these two
# process-uptime series, so the label is generic role names, not the
# operator's --cp/--agent container name).
UPTIME_BREAK_COLUMNS = {
    "cp_uptime_s": "control-plane",
    "agent_uptime_s": "node-agent",
}


def detect_segment(rows):
    """Detect a control-plane or node-agent restart (cp_uptime_s /
    agent_uptime_s decreasing) or a counter reset (any COUNTER_COLUMNS value
    decreasing) across usable rows (cycle==0 baseline, or cycle_ok rows).
    Returns:

        break_cycles: sorted list of cycles where a restart/reset first showed
        segments: list of (lo, hi, count) contiguous cycle ranges
        best_range: (lo, hi) of the longest segment (by point count), or None
        restarted: bool, True iff any break was detected
        break_reasons: dict cycle -> sorted list of human-readable reason
            strings (e.g. "node-agent restart", "counter reset: db_sessions"),
            for the report banner to name WHICH container restarted (#420).

    Same segmenting logic covers all cases (finding #9, extended by #420): a
    restart usually manifests as BOTH an uptime drop and a counter-column
    reset (a fresh process re-reads its own counters from zero, or the DB
    itself rolled back), but any one signal alone is sufficient to flag a
    break — and a node-agent-only restart (no cp_uptime_s drop) must flag a
    break exactly like a control-plane-only one; the two uptime columns are
    checked independently, not OR'd into a single "some restart happened"
    boolean before detection.

    A CSV predating agent_uptime_s (issue #420) simply never has that key in
    a row dict (report-only mode over an old CSV) — `r.get("agent_uptime_s",
    "")` then always sees "", which contributes no break signal, exactly
    like any other missing-sample cell. Backward compatible by construction.
    """
    usable = sorted(
        (r for r in rows if r["_cycle"] == 0 or r["_cycle_ok"]),
        key=lambda r: r["_cycle"],
    )
    if not usable:
        return [], [], None, False, {}

    break_cycles = set()
    break_reasons = {}

    def _flag(cycle, reason):
        break_cycles.add(cycle)
        break_reasons.setdefault(cycle, set()).add(reason)

    prev_uptime = {col: None for col in UPTIME_BREAK_COLUMNS}
    counter_prev = {}
    for r in usable:
        for col, label in UPTIME_BREAK_COLUMNS.items():
            up_raw = r.get(col, "")
            if up_raw in (None, ""):
                continue
            try:
                up = float(up_raw)
            except ValueError:
                continue
            if prev_uptime[col] is not None and up < prev_uptime[col]:
                _flag(r["_cycle"], f"{label} restart")
            prev_uptime[col] = up
        for col in COUNTER_COLUMNS:
            raw = r.get(col, "")
            if raw in (None, ""):
                continue
            try:
                val = float(raw)
            except ValueError:
                continue
            if col in counter_prev and val < counter_prev[col]:
                _flag(r["_cycle"], f"counter reset: {col}")
            counter_prev[col] = val

    all_cycles = [r["_cycle"] for r in usable]
    if not break_cycles:
        return [], [], (min(all_cycles), max(all_cycles)), False, {}

    bpoints = sorted(break_cycles)
    bounds = [min(all_cycles)] + bpoints + [max(all_cycles) + 1]
    segments = []
    for i in range(len(bounds) - 1):
        lo, hi = bounds[i], bounds[i + 1] - 1
        cnt = sum(1 for c in all_cycles if lo <= c <= hi)
        segments.append((lo, hi, cnt))
    best = max(segments, key=lambda s: s[2])
    reasons_out = {c: sorted(reasons) for c, reasons in break_reasons.items()}
    return bpoints, segments, (best[0], best[1]), True, reasons_out


def build_series(rows, fieldnames, cycle_range=None):
    """Return dict: column -> (xs, ys), pointwise-available (missing cells
    excluded per-column, never treated as 0), restricted to cycle_range
    (inclusive) when given."""
    series_cols = [c for c in fieldnames if c not in NON_SERIES_COLUMNS]
    series = {c: ([], []) for c in series_cols}
    for row in rows:
        cycle = row["_cycle"]
        cycle_ok = row["_cycle_ok"]
        is_baseline = cycle == 0
        if not (is_baseline or cycle_ok):
            continue
        if cycle_range is not None and not (cycle_range[0] <= cycle <= cycle_range[1]):
            continue
        for col in series_cols:
            raw = row.get(col, "")
            if raw is None or str(raw).strip() == "":
                continue
            try:
                val = float(raw)
            except ValueError:
                continue
            series[col][0].append(cycle)
            series[col][1].append(val)
    return series


def load_series(csv_path):
    """Convenience wrapper (used directly by simple selftests and any
    external caller that just wants the full, unsegmented series): parses
    the whole CSV, no restart/segment restriction. Returns column -> (xs, ys).
    """
    rows, fieldnames, _malformed = read_csv_rows(csv_path)
    return build_series(rows, fieldnames)


# ── HTML report ──────────────────────────────────────────────────────────


BADGE_COLOR = {
    "LEAK": "#c0392b",
    "DOWNWARD": "#c0392b",
    "STEP": "#f39c12",
    "SUSPECT": "#e67e22",
    "FLAT_NOISY": "#e67e22",
    "PLATEAU": "#f1c40f",
    "FLAT_CLEAN": "#27ae60",
    "GROWING_BY_DESIGN": "#27ae60",
    "ACCELERATING": "#c0392b",
    "INSUFFICIENT": "#95a5a6",
}


def _sparkline_svg(xs, ys, width=300, height=60):
    if not xs or not ys:
        return '<svg width="%d" height="%d"></svg>' % (width, height)
    ymin, ymax = min(ys), max(ys)
    if ymax == ymin:
        ymax = ymin + 1
    xmin, xmax = min(xs), max(xs)
    if xmax == xmin:
        xmax = xmin + 1
    pad = 4
    pts = []
    for x, y in zip(xs, ys):
        px = pad + (x - xmin) / (xmax - xmin) * (width - 2 * pad)
        py = height - pad - (y - ymin) / (ymax - ymin) * (height - 2 * pad)
        pts.append(f"{px:.1f},{py:.1f}")
    points = " ".join(pts)
    return (
        f'<svg width="{width}" height="{height}" viewBox="0 0 {width} {height}" '
        f'xmlns="http://www.w3.org/2000/svg">'
        f'<polyline points="{points}" fill="none" stroke="#3498db" stroke-width="2"/>'
        f"</svg>"
    )


def render_html(csv_path, verdicts, params, run_meta=None):
    run_meta = run_meta or {}
    counts = {}
    for v in verdicts.values():
        counts[v["verdict"]] = counts.get(v["verdict"], 0) + 1

    rows_html = []
    # LEAK / ACCELERATING / DOWNWARD / STEP first, then the rest, alphabetical within group.
    priority = {"LEAK": 0, "ACCELERATING": 0, "DOWNWARD": 1, "STEP": 1, "SUSPECT": 2, "FLAT_NOISY": 2,
                "PLATEAU": 3, "FLAT_CLEAN": 4, "GROWING_BY_DESIGN": 4, "INSUFFICIENT": 5}
    for name in sorted(verdicts, key=lambda n: (priority.get(verdicts[n]["verdict"], 9), n)):
        v = verdicts[name]
        color = BADGE_COLOR.get(v["verdict"], "#7f8c8d")
        spark = _sparkline_svg(v.get("xs", []), v.get("values", []))
        start = "" if v["start"] is None else f"{v['start']:.2f}"
        end = "" if v["end"] is None else f"{v['end']:.2f}"
        slope = "" if v["slope"] is None else f"{v['slope']:.4f}"
        tval = "" if v["t"] is None else f"{v['t']:.2f}"
        mono = "" if v["monotonic_frac"] is None else f"{v['monotonic_frac']:.0%}"
        rows_html.append(f"""
        <tr>
          <td class="series">{html.escape(name)}</td>
          <td><span class="badge" style="background:{color}">{v['verdict']}</span></td>
          <td>{start}</td>
          <td>{end}</td>
          <td>{slope}</td>
          <td>{tval}</td>
          <td>{mono}</td>
          <td>{v['n']}</td>
          <td class="spark">{spark}</td>
          <td class="detail">{html.escape(v['detail'])}</td>
        </tr>""")

    params_rows = "".join(
        f"<tr><td>{html.escape(str(k))}</td><td>{html.escape(str(val))}</td></tr>"
        for k, val in params.items()
    )
    counts_html = "".join(
        f'<span class="count-chip" style="background:{BADGE_COLOR.get(k, "#7f8c8d")}">{k}: {c}</span>'
        for k, c in sorted(counts.items())
    )

    banner_html = ""
    if run_meta.get("restarted"):
        seg = run_meta.get("analysed_segment") or {}
        break_reasons = run_meta.get("break_reasons", {})
        # Name WHICH container(s) restarted (#420) — a break at cycle N may
        # carry multiple reasons (e.g. both an agent-uptime drop AND a
        # counter reset at the same cycle); join per-cycle, then join cycles.
        per_cycle = []
        for c in run_meta.get("break_cycles", []):
            reasons = break_reasons.get(c) or break_reasons.get(str(c)) or []
            reason_str = ", ".join(reasons) if reasons else "restart/reset detected"
            per_cycle.append(f"cycle {c} ({reason_str})")
        break_str = "; ".join(per_cycle) if per_cycle else ", ".join(
            str(c) for c in run_meta.get("break_cycles", [])
        )
        banner_html = (
            f'<div class="banner">restart detected — {html.escape(break_str)} — '
            f"verdicts cover cycles {seg.get('from_cycle')}..{seg.get('to_cycle')} only; "
            "a restart during a leak soak is itself a leak-candidate signal, "
            "investigate the restart cause</div>"
        )

    summary_bits = []
    if run_meta.get("malformed_rows"):
        summary_bits.append(f"{run_meta['malformed_rows']} malformed CSV row(s) rejected (field-width mismatch)")
    summary_bits.append(
        f"{run_meta.get('failed_cycles', 0)} of {run_meta.get('total_cycles', 0)} cycle(s) failed "
        "(cycle_ok=0 — excluded from all series analysis; repeated failures are themselves a signal)"
    )
    summary_html = "<br>".join(html.escape(s) for s in summary_bits)

    return f"""<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Quasar soak report — {html.escape(os.path.basename(csv_path))}</title>
<style>
  body {{ font-family: -apple-system, Helvetica, Arial, sans-serif; margin: 24px; color: #1b1f23; background: #fafafa; }}
  h1 {{ font-size: 20px; }}
  h2 {{ font-size: 16px; margin-top: 32px; }}
  table {{ border-collapse: collapse; width: 100%; margin-top: 8px; }}
  th, td {{ border: 1px solid #ddd; padding: 6px 8px; font-size: 13px; text-align: left; vertical-align: middle; }}
  th {{ background: #eee; }}
  .series {{ font-family: ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }}
  .badge {{ color: white; padding: 2px 8px; border-radius: 10px; font-weight: 600; font-size: 12px; white-space: nowrap; }}
  .detail {{ max-width: 420px; }}
  .spark {{ width: 310px; }}
  .count-chip {{ color: white; padding: 3px 10px; border-radius: 10px; margin-right: 6px; font-size: 12px; font-weight: 600; }}
  .legend {{ font-size: 13px; background: #fff; border: 1px solid #ddd; padding: 12px 16px; border-radius: 6px; max-width: 900px; }}
  .escalation {{ font-weight: 600; }}
  .banner {{ background: #c0392b; color: white; font-weight: 700; padding: 12px 16px; border-radius: 6px; margin: 12px 0; max-width: 900px; }}
  .summary {{ font-size: 13px; background: #fff; border: 1px solid #ddd; padding: 10px 16px; border-radius: 6px; max-width: 900px; margin-top: 8px; }}
</style>
</head>
<body>
<h1>Quasar soak report</h1>
<p>Source CSV: <code>{html.escape(csv_path)}</code></p>
{banner_html}

<h2>Run parameters</h2>
<table>{params_rows}</table>

<h2>Run summary</h2>
<div class="summary">{summary_html}</div>

<h2>Verdict summary</h2>
<p>{counts_html}</p>
<p class="escalation">Escalation rule: {html.escape(ESCALATION_RULE)}</p>

<div class="legend">
  <strong>Legend.</strong>
  Gauge verdicts (goroutines, heap, rss, fds, threads, vram, pool_acquired/idle/total, gst_alive):
  <b>LEAK</b> (red) significant + sizeable + monotone-confirmed rise (via a
  noise-robust quartile-median check, not raw monotonicity — a GC-noisy
  series that flips sign on most consecutive deltas can still be a real
  leak);
  <b>STEP</b> (yellow/orange) a one-time level shift (best single changepoint
  explains the data far better than a linear trend) — not a per-cycle leak,
  investigate what allocated once;
  <b>PLATEAU</b> (yellow) rose then flattened — was probably warm-up, not a leak;
  <b>FLAT_CLEAN</b> (green) no trend, low noise — clear at this sensitivity;
  <b>FLAT_NOISY</b> / <b>SUSPECT</b> (orange) inconclusive — rerun longer;
  <b>DOWNWARD</b> (red) significant fall, unusual, worth a look;
  <b>INSUFFICIENT</b> (grey) fewer than 10 usable points.
  <br>
  Counter verdicts (pool_empty_acquire_count, cp_num_gc, db_* row counts):
  these grow by design every cycle (a session row is inserted every cycle,
  audit rows accrue, etc.) — a growing db_sessions count is NOT a leak.
  <b>GROWING_BY_DESIGN</b> (green) reports the expected linear rate;
  <b>ACCELERATING</b> (red) flags only when the per-cycle growth RATE itself
  is increasing, which would be unusual and worth investigating.
  <br>
  Rows with <b>cycle_ok=0</b> are excluded from all series analysis (only the
  cycle-0 baseline row and cycle_ok=1 rows feed the classifiers) — see the
  failed-cycle count in the run summary above; a run with a high failed-cycle
  count is itself worth investigating even before looking at any series
  verdict.
</div>

<h2>Per-series detail</h2>
<table>
<tr><th>series</th><th>verdict</th><th>start</th><th>end</th><th>slope/cycle</th><th>t</th><th>monotonic</th><th>n</th><th>trend</th><th>detail</th></tr>
{''.join(rows_html)}
</table>

</body>
</html>
"""


# ── driver ───────────────────────────────────────────────────────────────


def run_report(csv_path, html_path=None, json_path=None, params_path=None):
    out_dir = os.path.dirname(os.path.abspath(csv_path)) or "."
    if html_path is None:
        html_path = os.path.join(out_dir, "report.html")
    if json_path is None:
        json_path = os.path.join(out_dir, "verdicts.json")
    if params_path is None:
        params_path = os.path.join(out_dir, "params.json")

    rows, fieldnames, malformed = read_csv_rows(csv_path)
    break_cycles, _segments, best_range, restarted, break_reasons = detect_segment(rows)
    series = build_series(rows, fieldnames, cycle_range=best_range if restarted else None)

    verdicts = {}
    for name, (xs, ys) in series.items():
        if name in COUNTER_COLUMNS:
            verdicts[name] = classify_counter(xs, ys, name)
        else:
            verdicts[name] = classify_gauge(xs, ys, name)

    total_cycles = sum(1 for r in rows if r["_cycle"] != 0)
    failed_cycles = sum(1 for r in rows if r["_cycle"] != 0 and not r["_cycle_ok"])

    run_meta = {
        "malformed_rows": malformed,
        "restarted": restarted,
        "break_cycles": break_cycles,
        "break_reasons": break_reasons,
        "analysed_segment": (
            {"from_cycle": best_range[0], "to_cycle": best_range[1]}
            if (restarted and best_range) else None
        ),
        "total_cycles": total_cycles,
        "failed_cycles": failed_cycles,
    }

    params = {}
    if os.path.isfile(params_path):
        try:
            with open(params_path, encoding="utf-8") as fh:
                params = json.load(fh)
        except (OSError, ValueError):
            params = {"note": f"failed to parse {params_path}"}
    else:
        params = {"note": f"no params.json found at {params_path} — run params unavailable"}

    # Print one line per series to stdout.
    for name in sorted(verdicts):
        v = verdicts[name]
        start = "" if v["start"] is None else f"{v['start']:.2f}"
        end = "" if v["end"] is None else f"{v['end']:.2f}"
        slope = "" if v["slope"] is None else f"{v['slope']:.4f}"
        print(f"{name} {v['verdict']} {start} {end} {slope} {v['detail']}")

    if restarted:
        print(f"RESTART DETECTED at cycle(s) {break_cycles} ({break_reasons}) — analysing segment {run_meta['analysed_segment']}")
    if malformed:
        print(f"MALFORMED ROWS REJECTED: {malformed}")

    html_out = render_html(csv_path, verdicts, params, run_meta)
    with open(html_path, "w", encoding="utf-8") as fh:
        fh.write(html_out)

    # verdicts.json: machine-readable, strip the raw xs/values arrays (they're
    # only needed for the inline sparkline) to keep the file small. "_run" is
    # a run-level key (not a series name — series names come straight from
    # the CSV header, so "_run" can never collide with one).
    json_verdicts = {}
    for name, v in verdicts.items():
        json_verdicts[name] = {k: val for k, val in v.items() if k not in ("values", "xs")}
    json_verdicts["_run"] = run_meta
    with open(json_path, "w", encoding="utf-8") as fh:
        json.dump(json_verdicts, fh, indent=2)
        fh.write("\n")

    print(f"html: {html_path}")
    print(f"json: {json_path}")

    leak_candidates = sum(
        1 for v in verdicts.values() if v["verdict"] in ("LEAK", "ACCELERATING")
    )
    # Exit 0 regardless — verdicts are data, not a test failure. The harness
    # is responsible for turning leak_candidates > 0 into a highlighted
    # warning (finding a real leak is the harness WORKING, not the harness
    # failing).
    print(f"LEAK CANDIDATES: {leak_candidates}")
    return 0


# ── self-test ────────────────────────────────────────────────────────────

CSV_HEADER = [
    "cycle", "ts_iso", "cycle_ok", "session_id", "launch_to_running_s",
    "cp_goroutines", "cp_heap_inuse_bytes", "cp_heap_objects", "cp_heap_alloc_bytes",
    "cp_sys_bytes", "cp_num_gc", "cp_rss_kb", "cp_fds", "cp_uptime_s",
    "pool_acquired", "pool_idle", "pool_total", "pool_empty_acquire_count",
    "db_sessions", "db_session_metrics", "db_trace_events", "db_auth_tokens",
    "db_session_tokens", "db_admin_activity",
    "agent_rss_kb", "agent_threads", "agent_fds", "vram_used_mb", "gst_alive",
    "agent_uptime_s",
]

# A pre-#420 CSV header (no agent_uptime_s) — used by the backward-compat
# selftest scenario to prove report-only mode over an OLD-format CSV still
# works untouched.
CSV_HEADER_PRE_420 = [c for c in CSV_HEADER if c != "agent_uptime_s"]


def _write_selftest_csv(path, n_cycles, row_fn):
    """row_fn(cycle) -> dict of column->value (strings; '' for missing)."""
    with open(path, "w", newline="", encoding="utf-8") as fh:
        w = csv.DictWriter(fh, fieldnames=CSV_HEADER)
        w.writeheader()
        for c in range(0, n_cycles + 1):  # cycle 0 = baseline
            row = {col: "" for col in CSV_HEADER}
            row["cycle"] = c
            row["ts_iso"] = f"2026-07-31T00:{c:02d}:00Z"
            row["cycle_ok"] = 1
            row["session_id"] = f"sess-{c}"
            row["launch_to_running_s"] = 5
            row.update(row_fn(c))
            w.writerow(row)


def _selftest_flat_noise(tmpdir):
    """Every gauge series flat with small noise -> expect FLAT_CLEAN."""
    rnd = random.Random(1)
    path = os.path.join(tmpdir, "flat_noise.csv")

    def row(c):
        return {
            "cp_goroutines": 120 + rnd.uniform(-1, 1),
            "cp_fds": 40 + rnd.uniform(-0.5, 0.5),
            "cp_rss_kb": 200000 + rnd.uniform(-100, 100),
            "agent_rss_kb": 500000 + rnd.uniform(-100, 100),
            "vram_used_mb": 1024 + rnd.uniform(-2, 2),
            "pool_acquired": 2 + rnd.uniform(-0.2, 0.2),
        }

    _write_selftest_csv(path, 40, row)
    return path


def _selftest_linear_leak(tmpdir):
    """cp_heap_inuse_bytes and cp_goroutines climb steadily -> expect LEAK."""
    path = os.path.join(tmpdir, "linear_leak.csv")

    def row(c):
        return {
            "cp_heap_inuse_bytes": 50 * 1024 * 1024 + c * 2 * 1024 * 1024,  # +2MiB/cycle
            "cp_goroutines": 100 + c * 2,  # +2/cycle, monotonic
            "cp_fds": 40,  # flat control column
        }

    _write_selftest_csv(path, 40, row)
    return path


def _selftest_rise_then_plateau(tmpdir):
    """agent_rss_kb rises for the first third then flattens (with small
    noise on the flattened tail, so its own trend isn't spuriously
    "infinitely significant" from a perfectly deterministic near-zero
    slope) -> expect PLATEAU."""
    path = os.path.join(tmpdir, "rise_then_plateau.csv")
    rnd = random.Random(42)
    n = 30
    knee = 10

    def row(c):
        if c <= knee:
            rss = 400000 + c * 8000
        else:
            rss = 400000 + knee * 8000 + (c - knee) * 5 + rnd.uniform(-400, 400)
        return {"agent_rss_kb": rss, "cp_fds": 40}

    _write_selftest_csv(path, n, row)
    return path


def _selftest_counter(tmpdir):
    """db_sessions grows by exactly 1/cycle -> expect GROWING_BY_DESIGN.
    A second counter (pool_empty_acquire_count) accelerates -> ACCELERATING."""
    path = os.path.join(tmpdir, "counter.csv")
    n = 30

    def row(c):
        accel = c if c <= 20 else 20 + (c - 20) * 5  # rate increases after cycle 20
        return {
            "db_sessions": 100 + c,
            "pool_empty_acquire_count": accel,
        }

    _write_selftest_csv(path, n, row)
    return path


def _selftest_missing_cells(tmpdir):
    """agent_fds has ~30% missing cells but is otherwise flat -> classifier
    must exclude pointwise (never treat blank as 0) and still call it clean."""
    rnd = random.Random(2)
    path = os.path.join(tmpdir, "missing_cells.csv")
    n = 40

    def row(c):
        d = {"cp_fds": 40 + rnd.uniform(-0.5, 0.5)}
        if rnd.random() < 0.7:
            d["agent_fds"] = 60 + rnd.uniform(-1, 1)
        else:
            d["agent_fds"] = ""  # missing cell
        return d

    _write_selftest_csv(path, n, row)
    return path


def _selftest_insufficient(tmpdir):
    """Fewer than 10 cycles -> expect INSUFFICIENT."""
    path = os.path.join(tmpdir, "insufficient.csv")

    def row(c):
        return {"cp_goroutines": 100 + c}

    _write_selftest_csv(path, 5, row)
    return path


def _gc_sawtooth_value(base, amplitude, c, rnd, period=6):
    """A deterministic sawtooth (simulating a Go GC alloc/collect cycle: heap
    rises then drops each GC pass) plus a little jitter, so 100 trials aren't
    bit-identical."""
    phase = (c % period) / period
    saw = amplitude * (phase - 0.5) * 2  # ranges -amplitude .. +amplitude
    jitter = rnd.uniform(-amplitude * 0.15, amplitude * 0.15)
    return base + saw + jitter


def _selftest_noisy_series_csv(tmpdir, label, trial, drift_per_cycle, n=55,
                                base=60 * 1024 * 1024, amplitude_frac=0.4):
    """GC-noisy cp_heap_inuse_bytes series: base + drift*cycle + sawtooth
    noise at ~amplitude_frac of base. drift_per_cycle=0 -> noisy flat;
    drift_per_cycle>0 -> noisy linear leak. This is the reviewer's exact
    acceptance-gate scenario for fixes #2/#3/#6."""
    path = os.path.join(tmpdir, f"noisy_{label}_{trial}.csv")
    rnd = random.Random(f"{label}-{trial}")
    amplitude = amplitude_frac * base

    def row(c):
        val = base + drift_per_cycle * c + _gc_sawtooth_value(0, amplitude, c, rnd)
        return {"cp_heap_inuse_bytes": max(0.0, val)}

    _write_selftest_csv(path, n, row)
    return path


def _selftest_step_csv(tmpdir):
    """flat 100 -> jump to 140 at cycle 45 of 55 -> flat after -> STEP."""
    path = os.path.join(tmpdir, "step.csv")
    rnd = random.Random(777)
    n = 55
    jump_at = 45

    def row(c):
        level = 100.0 if c < jump_at else 140.0
        return {"cp_goroutines": level + rnd.uniform(-1.5, 1.5)}

    _write_selftest_csv(path, n, row)
    return path


def _selftest_linear_leak_noisy_tail_csv(tmpdir, trial, n=55, slope=3000.0):
    """A genuine steady linear leak, but with extra noise injected into the
    last third — the scenario the old small-sample b_last plateau rule
    misfiled ~10% of the time. New rule must not call this PLATEAU."""
    path = os.path.join(tmpdir, f"noisy_tail_{trial}.csv")
    rnd = random.Random(f"tail-{trial}")

    def row(c):
        val = 50 * 1024 * 1024 + slope * c
        if c > n * 2 // 3:
            val += rnd.uniform(-3 * 1024 * 1024, 3 * 1024 * 1024)
        return {"cp_heap_inuse_bytes": val}

    _write_selftest_csv(path, n, row)
    return path


def _selftest_restart_csv(tmpdir):
    """cp_uptime_s drops mid-run (a control-plane restart) -> verdicts must
    be computed on the longest contiguous segment, and the run-level "_run"
    key in verdicts.json must record it."""
    path = os.path.join(tmpdir, "restart.csv")
    n = 40
    restart_at = 20

    def row(c):
        if c < restart_at:
            uptime = c * 10
            heap = 50 * 1024 * 1024 + c * 100000
        else:
            uptime = (c - restart_at) * 10  # resets low after the restart
            heap = 50 * 1024 * 1024 + (c - restart_at) * 100000
        return {"cp_uptime_s": uptime, "cp_heap_inuse_bytes": heap, "cp_goroutines": 100}

    _write_selftest_csv(path, n, row)
    return path


def _selftest_agent_restart_csv(tmpdir, n=60, restarts=(20, 40), slope=5000.0, seed=99):
    """Issue #420 scenario: the NODE-AGENT container restarts mid-run
    (agent_uptime_s decreases, TWICE) while the control plane never does
    (cp_uptime_s climbs monotonically throughout) — the exact D-5 case this
    ticket fixes: a Tower backup restart of the whole stack, but the old
    harness only ever watched cp_uptime_s, so an agent-only restart was
    invisible and agent_rss_kb baselines silently reset with no banner.

    agent_rss_kb genuinely leaks +slope/cycle WITHIN each agent lifetime, but
    resets to baseline at each restart. TWO restarts (three lifetimes) is
    the deliberate choice here: with only one restart, the run ends mid-way
    up the second hump, so even a NAIVE whole-series fit still sees a large
    net rise (start-of-run baseline to end-of-run near-peak) and happens to
    also read LEAK — not a useful control. With two restarts the series is a
    3-hump sawtooth that starts AND ends near baseline, so the naive
    whole-series OLS trend is small and swamped by residual noise (fails the
    LEAK gate), while segmenting on agent_uptime_s (exactly like cp_uptime_s)
    isolates a single clean lifetime and sees the real per-cycle leak. That
    asymmetry — naive fit misses it, segmented fit catches it — is the whole
    point of #420; see the "asymmetry" checks in selftest().
    """
    path = os.path.join(tmpdir, "agent_restart.csv")
    rnd = random.Random(seed)

    def row(c):
        cp_uptime = c * 10  # control plane: never restarts in this scenario
        seg_start = 0
        for r in restarts:
            if c >= r:
                seg_start = r
        a_uptime = (c - seg_start) * 10
        rss = 400000 + slope * (c - seg_start) + rnd.uniform(-2000, 2000)
        return {
            "cp_uptime_s": cp_uptime,
            "agent_uptime_s": a_uptime,
            "agent_rss_kb": rss,
            "cp_goroutines": 100,  # flat control column, unaffected by either restart
        }

    _write_selftest_csv(path, n, row)
    return path, list(restarts)


def _selftest_pre_420_csv(tmpdir, n=15):
    """A CSV in the OLD (pre-#420) format — no agent_uptime_s column at all —
    to prove report-only mode over past results does not break when the
    column is simply absent (backward compat, appended-at-end schema)."""
    path = os.path.join(tmpdir, "pre_420_format.csv")
    with open(path, "w", newline="", encoding="utf-8") as fh:
        w = csv.DictWriter(fh, fieldnames=CSV_HEADER_PRE_420)
        w.writeheader()
        for c in range(0, n + 1):
            row = {col: "" for col in CSV_HEADER_PRE_420}
            row["cycle"] = c
            row["ts_iso"] = f"2026-07-31T00:{c:02d}:00Z"
            row["cycle_ok"] = 1
            row["session_id"] = f"sess-{c}"
            row["launch_to_running_s"] = 5
            row["cp_goroutines"] = 100 + c  # trivial series, just needs to classify without crashing
            row["cp_uptime_s"] = c * 10
            w.writerow(row)
    return path


def _selftest_malformed_width_csv(tmpdir):
    """One row (cycle 6) has an extra trailing field — simulates the 7-field
    shell CSV bug (finding #1). Written by hand (not DictWriter) since
    DictWriter can't produce a malformed row on purpose."""
    path = os.path.join(tmpdir, "malformed_width.csv")

    def good_row(c):
        vals = {col: "" for col in CSV_HEADER}
        vals["cycle"] = str(c)
        vals["ts_iso"] = f"2026-07-31T00:{c:02d}:00Z"
        vals["cycle_ok"] = "1"
        vals["session_id"] = f"sess-{c}"
        vals["launch_to_running_s"] = "5"
        vals["cp_goroutines"] = str(100 + c)
        return ",".join(vals[col] for col in CSV_HEADER)

    lines = [",".join(CSV_HEADER)]
    for c in range(0, 12):
        line = good_row(c)
        if c == 6:
            line += ",EXTRA_FIELD"
        lines.append(line)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")
    return path


def selftest():
    failures = []

    def check(label, cond, extra=""):
        status = "PASS" if cond else "FAIL"
        print(f"{status}: {label} {extra}".rstrip())
        if not cond:
            failures.append(label)

    with tempfile.TemporaryDirectory(prefix="soak-report-selftest-") as tmpdir:
        # 1. flat + noise
        p = _selftest_flat_noise(tmpdir)
        series = load_series(p)
        v = classify_gauge(*series["cp_goroutines"], "cp_goroutines")
        check("flat_noise cp_goroutines -> FLAT_CLEAN", v["verdict"] == "FLAT_CLEAN", v["verdict"])
        v2 = classify_gauge(*series["vram_used_mb"], "vram_used_mb")
        check("flat_noise vram_used_mb -> FLAT_CLEAN", v2["verdict"] == "FLAT_CLEAN", v2["verdict"])

        # 2. linear leak
        p = _selftest_linear_leak(tmpdir)
        series = load_series(p)
        v = classify_gauge(*series["cp_heap_inuse_bytes"], "cp_heap_inuse_bytes")
        check("linear_leak cp_heap_inuse_bytes -> LEAK", v["verdict"] == "LEAK", v["verdict"])
        v2 = classify_gauge(*series["cp_goroutines"], "cp_goroutines")
        check("linear_leak cp_goroutines -> LEAK", v2["verdict"] == "LEAK", v2["verdict"])
        v3 = classify_gauge(*series["cp_fds"], "cp_fds")
        check("linear_leak control column cp_fds -> FLAT_CLEAN", v3["verdict"] == "FLAT_CLEAN", v3["verdict"])

        # 3. rise then plateau
        p = _selftest_rise_then_plateau(tmpdir)
        series = load_series(p)
        v = classify_gauge(*series["agent_rss_kb"], "agent_rss_kb")
        check("rise_then_plateau agent_rss_kb -> PLATEAU", v["verdict"] == "PLATEAU", v["verdict"])

        # 4. counter series
        p = _selftest_counter(tmpdir)
        series = load_series(p)
        v = classify_counter(*series["db_sessions"], "db_sessions")
        check("counter db_sessions -> GROWING_BY_DESIGN", v["verdict"] == "GROWING_BY_DESIGN", v["verdict"])
        v2 = classify_counter(*series["pool_empty_acquire_count"], "pool_empty_acquire_count")
        check("counter pool_empty_acquire_count (rate increases) -> ACCELERATING", v2["verdict"] == "ACCELERATING", v2["verdict"])

        # 5. missing cells
        p = _selftest_missing_cells(tmpdir)
        series = load_series(p)
        xs, ys = series["agent_fds"]
        check("missing_cells agent_fds excludes blanks (n < 41)", len(xs) < 41, f"n={len(xs)}")
        check("missing_cells agent_fds no zero values (blank != 0)", all(y > 10 for y in ys), f"min={min(ys) if ys else None}")
        v = classify_gauge(xs, ys, "agent_fds")
        check("missing_cells agent_fds -> FLAT_CLEAN despite gaps", v["verdict"] == "FLAT_CLEAN", v["verdict"])

        # 6. insufficient data
        p = _selftest_insufficient(tmpdir)
        series = load_series(p)
        v = classify_gauge(*series["cp_goroutines"], "cp_goroutines")
        check("insufficient (n<10) -> INSUFFICIENT", v["verdict"] == "INSUFFICIENT", v["verdict"])

        # 7. end-to-end: run_report doesn't crash and produces files + exit 0
        p = _selftest_linear_leak(tmpdir)
        rc = run_report(p)
        check("run_report end-to-end exits 0", rc == 0, f"rc={rc}")
        check("run_report writes report.html", os.path.isfile(os.path.join(tmpdir, "report.html")))
        check("run_report writes verdicts.json", os.path.isfile(os.path.join(tmpdir, "verdicts.json")))
        with open(os.path.join(tmpdir, "verdicts.json"), encoding="utf-8") as fh:
            vj = json.load(fh)
        check("verdicts.json has cp_heap_inuse_bytes=LEAK", vj.get("cp_heap_inuse_bytes", {}).get("verdict") == "LEAK")
        check("verdicts.json has a _run key (not restarted here)", vj.get("_run", {}).get("restarted") is False)

        # 8. STEP: flat-then-jump-then-flat must NOT read LEAK
        p = _selftest_step_csv(tmpdir)
        series = load_series(p)
        v = classify_gauge(*series["cp_goroutines"], "cp_goroutines")
        check("step function (flat 100 -> jump 140 @ cycle45/55) -> STEP (not LEAK)", v["verdict"] == "STEP", v["verdict"])

        # 9. field-width guard: a row with MORE fields than the header must
        # be REJECTED (not silently name-mapped) and counted as malformed.
        p = _selftest_malformed_width_csv(tmpdir)
        rows, _fieldnames, malformed = read_csv_rows(p)
        check("field-width guard: malformed row rejected, count==1", malformed == 1, f"malformed={malformed}")
        check("field-width guard: rejected row excluded (not name-mapped)",
              all(r["_cycle"] != 6 for r in rows), sorted(r["_cycle"] for r in rows))
        rc = run_report(p)
        with open(os.path.join(tmpdir, "verdicts.json"), encoding="utf-8") as fh:
            vj = json.load(fh)
        check("field-width guard: malformed count surfaced in run summary",
              vj.get("_run", {}).get("malformed_rows") == 1, vj.get("_run"))

        # 10. restart segmentation: cp_uptime_s drops mid-run
        p = _selftest_restart_csv(tmpdir)
        rc = run_report(p)
        with open(os.path.join(tmpdir, "verdicts.json"), encoding="utf-8") as fh:
            vj = json.load(fh)
        run_meta = vj.get("_run", {})
        check("restart segmentation: restarted flag set", run_meta.get("restarted") is True, run_meta)
        seg = run_meta.get("analysed_segment") or {}
        check("restart segmentation: analysed_segment present",
              seg.get("from_cycle") is not None and seg.get("to_cycle") is not None, seg)
        with open(os.path.join(tmpdir, "report.html"), encoding="utf-8") as fh:
            report_html = fh.read()
        check("restart segmentation: red banner present in report.html",
              "restart detected" in report_html and "control-plane restart" in report_html)

        # 11. agent-restart segmentation (issue #420): the node-agent alone
        # restarts mid-run (cp_uptime_s never drops) — must still be
        # detected, segmented, and named as a node-agent restart, AND the
        # segmented verdict must correctly catch a leak that a naive
        # unsegmented fit misses (the asymmetry that is the point of #420).
        p, restart_cycles = _selftest_agent_restart_csv(tmpdir)
        rc = run_report(p)
        with open(os.path.join(tmpdir, "verdicts.json"), encoding="utf-8") as fh:
            vj = json.load(fh)
        run_meta = vj.get("_run", {})
        check("agent restart: restarted flag set from agent_uptime_s alone (cp never drops)",
              run_meta.get("restarted") is True, run_meta)
        break_reasons = run_meta.get("break_reasons", {})
        reasons_flat = [r for rs in break_reasons.values() for r in rs]
        check("agent restart: break_reasons names 'node-agent restart' (not control-plane)",
              any("node-agent restart" in r for r in reasons_flat)
              and not any("control-plane restart" in r for r in reasons_flat),
              reasons_flat)
        check("agent restart: segmented verdict for agent_rss_kb -> LEAK",
              vj.get("agent_rss_kb", {}).get("verdict") == "LEAK",
              vj.get("agent_rss_kb", {}).get("verdict"))
        with open(os.path.join(tmpdir, "report.html"), encoding="utf-8") as fh:
            report_html = fh.read()
        check("agent restart: banner present and names node-agent",
              "restart detected" in report_html and "node-agent restart" in report_html)

        # Control: the SAME data, fit naively (whole series, ignoring the
        # restart) via classify_gauge directly on the raw pointwise series —
        # must NOT also read LEAK, demonstrating why segmentation is needed
        # (the reset in the middle either kills the net trend or blows up
        # the residual noise enough to miss real per-lifetime leaking).
        naive_series = load_series(p)
        naive_v = classify_gauge(*naive_series["agent_rss_kb"], "agent_rss_kb")
        check("agent restart CONTROL: naive unsegmented fit does NOT read LEAK "
              "(segmentation is what catches this leak, not the naive fit)",
              naive_v["verdict"] != "LEAK", naive_v["verdict"])

        # 12. backward compat: a pre-#420 CSV (no agent_uptime_s column at
        # all) must still report-only cleanly — report-only mode over old
        # results must not break just because the column is now expected.
        p = _selftest_pre_420_csv(tmpdir)
        rc = run_report(p)
        check("pre-#420 CSV (no agent_uptime_s column): run_report still exits 0", rc == 0, f"rc={rc}")
        with open(os.path.join(tmpdir, "verdicts.json"), encoding="utf-8") as fh:
            vj = json.load(fh)
        check("pre-#420 CSV: not flagged restarted (no agent_uptime_s signal at all)",
              vj.get("_run", {}).get("restarted") is False, vj.get("_run"))
        check("pre-#420 CSV: cp_goroutines still classified normally",
              vj.get("cp_goroutines", {}).get("verdict") is not None)

        # 13. acceptance gate (fixes #2/#3/#6): 100-trial noisy leak / noisy
        # flat / noisy-tail leak simulations, per the reviewer's own numbers.
        leak_hits = 0
        for trial in range(100):
            p = _selftest_noisy_series_csv(tmpdir, "leak", trial, drift_per_cycle=1 * 1024 * 1024)
            series = load_series(p)
            v = classify_gauge(*series["cp_heap_inuse_bytes"], "cp_heap_inuse_bytes")
            if v["verdict"] == "LEAK":
                leak_hits += 1
        check("noisy linear leak (1MiB/cycle over GC sawtooth) -> LEAK in >=90/100 trials",
              leak_hits >= 90, f"{leak_hits}/100")

        flat_hits = 0
        for trial in range(100):
            p = _selftest_noisy_series_csv(tmpdir, "flat", trial, drift_per_cycle=0)
            series = load_series(p)
            v = classify_gauge(*series["cp_heap_inuse_bytes"], "cp_heap_inuse_bytes")
            if v["verdict"] == "LEAK":
                flat_hits += 1
        check("noisy flat (GC sawtooth, no drift) -> LEAK in <=2/100 trials (false-red bound)",
              flat_hits <= 2, f"{flat_hits}/100")

        not_plateau = 0
        for trial in range(100):
            p = _selftest_linear_leak_noisy_tail_csv(tmpdir, trial)
            series = load_series(p)
            v = classify_gauge(*series["cp_heap_inuse_bytes"], "cp_heap_inuse_bytes")
            if v["verdict"] != "PLATEAU":
                not_plateau += 1
        check("real linear leak w/ noisy tail -> NOT PLATEAU in >=95/100 trials",
              not_plateau >= 95, f"{not_plateau}/100")

    print()
    if failures:
        print(f"SELFTEST: {len(failures)} FAILURE(S): {failures}")
        return 1
    print("SELFTEST: ALL PASS")
    return 0


# ── CLI ─────────────────────────────────────────────────────────────────


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("csv", nargs="?", help="path to the soak CSV")
    ap.add_argument("--html", help="output HTML path (default: report.html next to the CSV)")
    ap.add_argument("--json", dest="json_path", help="output verdicts.json path (default: next to the CSV)")
    ap.add_argument("--params", help="params.json sidecar path (default: params.json next to the CSV)")
    ap.add_argument("--selftest", action="store_true", help="run the synthetic-CSV self-test and exit")
    args = ap.parse_args(argv)

    if args.selftest:
        return selftest()

    if not args.csv:
        ap.error("csv path is required unless --selftest is given")
    return run_report(args.csv, args.html, args.json_path, args.params)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
