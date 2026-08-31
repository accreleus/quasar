#!/usr/bin/env python3
"""Assertions for the bench_app_samples.py fold (scripts/dx/tests/run.sh).

Prints "OK", or a `; `-joined list of problems. Lives in its own file rather
than a heredoc because run.sh already nests one level of heredoc here.
"""
import json
import os
import sys

d = sys.argv[1]
rows = [json.loads(l) for l in open(os.path.join(d, "metrics.jsonl")) if l.strip()]
tr = json.load(open(os.path.join(d, "trace.json")))
problems = []

agent = [r for r in rows if r["source"] == "agent"]
app = [r for r in rows if r["source"] == "app"]
brow = [r for r in rows if r["source"] == "browser"]
if len(agent) != 1:
    problems.append("pre-existing agent sample count %d (fold must not eat it)" % len(agent))
if len(app) != 2:
    problems.append("app samples %d (want one per wall-clock second)" % len(app))
if len(brow) != 2:
    problems.append("browser samples %d (want one per bench window, INCLUDING the empty one)" % len(brow))

if app:
    m = app[0]["metrics"]
    if m.get("fps") != 2:
        problems.append("second-1 fps %r" % m.get("fps"))
    if m.get("presented_late") != 1:
        problems.append("presented_late %r" % m.get("presented_late"))
    if m.get("scene_id") != 3:
        problems.append("scene_id %r" % m.get("scene_id"))
    for k in ("cpu_frame_ms_p95", "gpu_frame_ms_p95", "load"):
        if k not in m:
            problems.append("app sample missing %s" % k)
    # I7: the app's own presented index range must survive the fold, or the
    # browser's missing_indices count is unattributable.
    if m.get("frame_index_min") != 0 or m.get("frame_index_max") != 1:
        problems.append("app frame_index range %r..%r" % (m.get("frame_index_min"), m.get("frame_index_max")))
    if m.get("app_index_span") != 2:
        problems.append("app_index_span %r" % m.get("app_index_span"))
    # The app -> compositor stage, split by owner. Without this the head of the
    # latency budget is a single unexplained lump (T1 §2: 13.80 ms).
    for k, want in (("submit_to_present_p50_ms", 10.0), ("submit_to_present_p95_ms", 16.0),
                    ("render_p50_ms", 2.0), ("render_p95_ms", 3.0),
                    ("repaint_wait_p50_ms", 8.0), ("repaint_wait_p95_ms", 13.0)):
        if m.get(k) != want:
            problems.append("app %s %r (want %r)" % (k, m.get(k), want))

if len(app) > 1:
    # Second 2's frame carries no present_ms/commit_ms — an older benchapp, or a
    # compositor without wp_presentation. Those keys must be ABSENT, never 0: a
    # 0 ms repaint wait would read as "the compositor answered instantly".
    m2 = app[1]["metrics"]
    for k in ("submit_to_present_p50_ms", "render_p50_ms", "repaint_wait_p50_ms"):
        if k in m2:
            problems.append("unmeasured app stage folded in as %r" % m2.get(k))

if brow:
    m = brow[0]["metrics"]
    for k in ("g2g_p50_ms", "g2g_p95_ms", "missing_indices", "duplicated", "undecoded",
              "i2p_p50_ms", "i2p_missed", "no_image"):
        if k not in m:
            problems.append("browser sample missing %s" % k)
    if m.get("i2p_p50_ms") != 44:
        problems.append("i2p_p50_ms %r" % m.get("i2p_p50_ms"))
    if m.get("offset_unknown") != 0:
        problems.append("offset_unknown %r" % m.get("offset_unknown"))
    # The `stage_*` latency split must reach the series. It is carried by PREFIX,
    # not by an allow-list, so a new stage key added to the instrument arrives
    # without a fold change — the point being that a stage cannot go missing the
    # way the renamed drop keys did in suite opt-t1.
    for k in ("stage_host_to_receive_p50_ms", "stage_receive_to_present_p50_ms",
              "stage_decode_p50_ms", "stage_wait_queue_p50_ms",
              "stage_present_to_display_p50_ms", "stage_jb_mean_ms",
              "stage_render_queue_derived_p50_ms", "stage_n"):
        if k not in m:
            problems.append("browser sample missing %s" % k)
    if m.get("stage_host_to_receive_p50_ms") != 21.4:
        problems.append("stage_host_to_receive_p50_ms %r" % m.get("stage_host_to_receive_p50_ms"))
    # A stage the browser could not measure must be ABSENT from the numeric
    # series, never folded in as 0 — a 0 ms assembly time is a claim, not a gap.
    if "stage_assembly_mean_ms" in m:
        problems.append("null stage key folded in as a number: %r" % m.get("stage_assembly_mean_ms"))
    # C1: the window's own clock, not launch_epoch + ordinal. The first window
    # decoded, so its last decoded frame's marker host_time_ms is the join key.
    if brow[0]["ts_unix_ms"] != 1800000000950:
        problems.append("window 1 stamped %r (want last_host_time_ms 1800000000950)" % brow[0]["ts_unix_ms"])
if len(brow) > 1:
    # The empty window decoded nothing, so it falls back to t_end_host_ms —
    # still its OWN clock, never an ordinal.
    if brow[1]["ts_unix_ms"] != 1800000002003:
        problems.append("empty window stamped %r (want t_end_host_ms 1800000002003)" % brow[1]["ts_unix_ms"])
    if brow[1]["metrics"].get("bench_n") != 0:
        problems.append("empty window must still be a sample with bench_n=0")
    if brow[1]["metrics"].get("i2p_missed") != 1:
        problems.append("empty-window i2p_missed %r" % brow[1]["metrics"].get("i2p_missed"))

kinds = {}
for e in tr.get("events") or []:
    kinds[e["type"]] = kinds.get(e["type"], 0) + 1
if kinds.get("abr.retarget") != 1:
    problems.append("pre-existing trace event lost — the fold must MERGE, not replace")
if kinds.get("app.event") != 2:
    problems.append("app.event count %r" % kinds.get("app.event"))
if kinds.get("bench.window") != 2:
    problems.append("bench.window count %r" % kinds.get("bench.window"))
if kinds.get("harness.mark"):
    problems.append("app marks must NOT be harness.mark — that type is bench_submit's phase boundary")

# The per-frame ring must be written out as an artifact (opt-t1: without it no
# per-frame timeline is reconstructable from a submitted run).
ring_path = os.path.join(d, "bench-frames.json")
if not os.path.exists(ring_path):
    problems.append("bench-frames.json not written")
else:
    ring = json.load(open(ring_path)).get("frames") or []
    if len(ring) != 1 or ring[0].get("frame_index") != 7:
        problems.append("bench-frames.json ring %r" % ring)

print("OK" if not problems else "; ".join(problems))
