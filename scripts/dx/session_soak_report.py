#!/usr/bin/env python3
"""scripts/dx/session_soak_report.py — the analysis half of `make session-soak`.

Reads a soak output directory (session.json / steps.jsonl / metrics.jsonl /
trace.json, as written by scripts/dx/session_soak.sh from the driver's NDJSON)
and writes summary.json + REPORT.md next to them.

python3 stdlib only, and no f-strings beyond 3.6 — it runs on this workstation
(macOS) AND on a fleet host (Linux) with whatever python3 is there.

Usage:
  session_soak_report.py --dir .diagnostics/soak/<run>
  session_soak_report.py --dir <run> --stdout      # print REPORT.md, write nothing
"""

import argparse
import json
import os
import sys

# Steady state: the first STEADY_SKIP_MS of a dwell is the retarget transient
# (IDR, encoder reconfigure, jitter-buffer refill) and is deliberately excluded
# from every per-step mean. The boundary analysis is where that window is judged.
STEADY_SKIP_MS = 3000
BOUNDARY_MS = 5000

AGENT_KEYS = ["fps", "bitrate_kbps", "encode_ms", "encode_ms_p95"]
BROWSER_KEYS = [
    "fps", "present_fps", "present_interval_sd_ms", "decode_ms", "rtt_ms",
    "jitter_buffer_ms", "bitrate_kbps",
]
# Monotonic counters: reported as a delta across the window, never a mean.
COUNTER_KEYS = ["frames_dropped", "packets_lost"]


# ── io ───────────────────────────────────────────────────────────────────────
def read_jsonl(path):
    out = []
    if not os.path.exists(path):
        return out
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                out.append(json.loads(line))
            except ValueError:
                continue
    return out


def read_json(path, default=None):
    if not os.path.exists(path):
        return default
    try:
        with open(path) as f:
            return json.load(f)
    except ValueError:
        return default


# ── stats ────────────────────────────────────────────────────────────────────
def mean(xs):
    xs = [x for x in xs if isinstance(x, (int, float))]
    return sum(xs) / float(len(xs)) if xs else None


def pct(xs, q):
    xs = sorted(x for x in xs if isinstance(x, (int, float)))
    if not xs:
        return None
    i = int(round((len(xs) - 1) * q))
    return xs[i]


def fmt(v, nd=1):
    if v is None:
        return "-"
    if isinstance(v, float):
        return ("%%.%df" % nd) % v
    return str(v)


def window(samples, source, t0, t1):
    return [s for s in samples
            if s.get("source") == source
            and isinstance(s.get("ts_unix_ms"), (int, float))
            and t0 <= s["ts_unix_ms"] <= t1]


def series(win, key):
    return [(s.get("metrics") or {}).get(key) for s in win]


def counter_delta(win, key):
    vals = [v for v in series(win, key) if isinstance(v, (int, float))]
    if len(vals) < 2:
        return None
    # session_metrics counters are cumulative per session; a reset (agent
    # restart) would read negative, so clamp rather than report nonsense.
    d = vals[-1] - vals[0]
    return d if d >= 0 else None


def agent_size(win, wkey, hkey, alt_w=None, alt_h=None):
    """Last (w,h) reported in the window. Accepts both the agent's own
    stream_width/height naming and the API's external_width/height."""
    for s in reversed(win):
        m = s.get("metrics") or {}
        w = m.get(wkey, m.get(alt_w) if alt_w else None)
        h = m.get(hkey, m.get(alt_h) if alt_h else None)
        if w and h:
            return (int(w), int(h))
    return None


def ladder_steps(trace):
    """The agent's own `abr.ladder.step` events, in time order. This is the whole
    point of an observe run: the harness did nothing, so every step here is the
    host's ABR ladder acting on its own."""
    out = []
    for ev in ((trace or {}).get("events") or []):
        # The trace names an event in `type` (the wire key from
        # control-plane/internal/session/trace_handler.go Type field).
        if ev.get("type") != "abr.ladder.step":
            continue
        p = ev.get("payload") or {}
        out.append({
            "ts_unix_ms": ev.get("ts_unix_ms"),
            "rung": p.get("rung"), "from": p.get("from"), "to": p.get("to"),
            "reason": p.get("reason"), "setpoint_kbps": p.get("setpoint_kbps"),
        })
    out.sort(key=lambda s: s["ts_unix_ms"] or 0)
    return out


OSCILLATION_WINDOW_MS = 60000


def oscillations(steps):
    """A-B-A on the same rung kind inside 60 s (spec D8 pass criterion 1). Returns the
    offending triples; any hit FAILS the run — pumping is the single failure mode this
    whole hysteresis design exists to prevent."""
    bad = []
    for kind in {s["rung"] for s in steps}:
        seq = [s for s in steps if s["rung"] == kind]
        for i in range(len(seq) - 2):
            a, b, c = seq[i], seq[i + 1], seq[i + 2]
            if a["to"] == c["to"] and a["to"] != b["to"] \
               and (c["ts_unix_ms"] - a["ts_unix_ms"]) <= OSCILLATION_WINDOW_MS:
                bad.append([a, b, c])
    return bad


def step_timings(steps, t0_ms, impair_ms=None, clear_ms=None):
    """time-to-first-step (from the impairment, or from run start) and
    time-to-recover (from the clear to the step that returns to rung 0)."""
    first = next((s for s in steps if s["reason"] in ("engage", "emergency")), None)
    recovered = next((s for s in steps if s["to"] == 0 and s["reason"] == "recover"), None)
    ref = impair_ms or t0_ms
    return {
        "time_to_first_step_s": round((first["ts_unix_ms"] - ref) / 1000.0, 1) if first and ref else None,
        "time_to_recover_s": round((recovered["ts_unix_ms"] - (clear_ms or ref)) / 1000.0, 1)
                              if recovered and (clear_ms or ref) else None,
    }


def marks_impair_clear(marks):
    """The sequencer (abr_ladder_netem.sh) writes marks.jsonl with the real
    wall-clock instant of every `qnetem sender <level>` / `sender-clear` it issues —
    ground truth for when the network actually changed, which the soak driver has
    no way to know on its own (it never touches qnetem). Anchor time-to-first-step
    on the FIRST impair mark and time-to-recover on the LAST clear mark: the first
    impairment applied and the final recovery, bracketing the whole run."""
    imp = [m for m in (marks or []) if m.get("mark") == "impair"]
    clr = [m for m in (marks or []) if m.get("mark") == "clear"]
    impair_ms = imp[0]["ts_unix_ms"] if imp else None
    clear_ms = clr[-1]["ts_unix_ms"] if clr else None
    return impair_ms, clear_ms


# ── analysis ─────────────────────────────────────────────────────────────────
def analyse(sess, steps, metrics, trace=None, marks=None, stream_verdict=None):
    launch = sess.get("launch") or [0, 0]
    is_observe = sess.get("profile") == "observe"
    rows = []
    for st in steps:
        t0, t1 = st.get("t_start_ms"), st.get("t_end_ms")
        if not t0 or not t1:
            continue
        s0 = t0 + STEADY_SKIP_MS
        aw = window(metrics, "agent", s0, t1)
        bw = window(metrics, "browser", s0, t1)
        sd = [v for v in series(bw, "present_interval_sd_ms") if isinstance(v, (int, float))]

        row = {
            "index": st.get("index"),
            "label": st.get("label"),
            "target": [st.get("target_w"), st.get("target_h")],
            "patch_code": st.get("patch_code"),
            "patch_ms": st.get("patch_ms"),
            "echo_ms": st.get("echo_ms"),
            "echoed": st.get("echoed"),
            "dwell_s": st.get("dwell_s"),
            "t_start_ms": t0,
            "t_end_ms": t1,
            "n_agent": len(aw),
            "n_browser": len(bw),
            "agent": dict((k, mean(series(aw, k))) for k in AGENT_KEYS),
            "browser": dict((k, mean(series(bw, k))) for k in BROWSER_KEYS),
            "present_sd_p95": pct(sd, 0.95),
            "agent_frames_dropped_delta": counter_delta(aw, "frames_dropped"),
            "browser_frames_dropped_delta": counter_delta(bw, "frames_dropped"),
            "packets_lost_delta": counter_delta(bw, "packets_lost"),
            "agent_external": agent_size(aw, "stream_width", "stream_height",
                                         "external_width", "external_height"),
            "agent_render": agent_size(aw, "render_width", "render_height"),
            "render_before": st.get("render_before"),
            "render_after": st.get("render_after"),
        }

        # Boundary: the 5 s either side of the PATCH instant.
        pre_b = window(metrics, "browser", t0 - BOUNDARY_MS, t0 - 1)
        post_b = window(metrics, "browser", t0 + 1, t0 + BOUNDARY_MS)
        row["boundary"] = {
            "pre_sd_mean": mean(series(pre_b, "present_interval_sd_ms")),
            "post_sd_mean": mean(series(post_b, "present_interval_sd_ms")),
            "pre_dropped_delta": counter_delta(pre_b, "frames_dropped"),
            "post_dropped_delta": counter_delta(post_b, "frames_dropped"),
            "n_pre": len(pre_b),
            "n_post": len(post_b),
        }
        rows.append(row)

    # ── internal-untouched verdict ───────────────────────────────────────────
    # The soak never sends render_*, and since the 2026-08-16 independent-axes
    # amendment the agent never rewrites the render size on a stream step either
    # (the old internal<=external clamp is gone). So the rule is binary: the
    # observed internal size must be IDENTICAL across every step. Per contract
    # the render_* readback is present only when non-default — absent means
    # "at the launch size" — so a missing readback is folded to `launch`, not
    # treated as unknown.
    if is_observe:
        # An observe run scripts no external-size steps at all, so the step-based
        # internal-untouched check (which needs `rows`) has nothing to look at by
        # design — that is not the same thing as "unproven", it is "not applicable".
        internal = {
            "verdict": "N/A",
            "notes": ["observe profile: zero PATCHes were sent, so the step-based "
                      "internal-untouched check does not apply to this run."],
            "violations": [],
            "observed": [],
        }
        steps_failed = []
    else:
        internal = {"verdict": "PASS", "notes": [], "violations": [], "observed": []}
        prev_render = None
        have_launch = list(launch) != [0, 0]
        for row in rows:
            obs = row["agent_render"] or row["render_after"]
            if not obs and have_launch:
                obs = list(launch)
            if obs:
                row_obs = list(obs)
                internal["observed"].append({"step": row["index"], "render": row_obs})
                if prev_render and row_obs != list(prev_render):
                    internal["verdict"] = "FAIL"
                    msg = ("step %s: internal moved %dx%d -> %dx%d on a stream-only update "
                           "(render must never follow the external size)"
                           % (row["index"], prev_render[0], prev_render[1],
                              row_obs[0], row_obs[1]))
                    internal["notes"].append(msg)
                    internal["violations"].append(msg)
                prev_render = tuple(row_obs)
        if not internal["observed"]:
            internal["verdict"] = "UNKNOWN"
            internal["notes"].append(
                "no render_width/height readback and no launch size known — internal-untouched "
                "is UNPROVEN.")

        steps_failed = [r for r in rows if not (r["echoed"] and r["patch_code"] == 202)]

    # ── the agent's own ABR ladder, from the trace (T3 events) ──────────────────
    # Present on any run, but this is the whole POINT of an observe run: the
    # harness issued no PATCHes, so every step here is the host acting on its own.
    ladder = ladder_steps(trace)
    osc = oscillations(ladder)
    ref_t0 = sess.get("started_at_ms")
    if not ref_t0 and rows:
        ref_t0 = rows[0]["t_start_ms"]
    if not ref_t0 and metrics:
        ref_t0 = metrics[0].get("ts_unix_ms")
    impair_ms, clear_ms = marks_impair_clear(marks)
    timings = step_timings(ladder, ref_t0, impair_ms, clear_ms)

    overall = "PASS"
    if steps_failed or internal["verdict"] == "FAIL":
        overall = "FAIL"
    elif internal["verdict"] == "UNKNOWN":
        overall = "DEGRADED"
    if osc:
        # An A-B-A inside the hysteresis window is pumping — it FAILS the run
        # outright regardless of anything else this analysis found.
        overall = "FAIL"

    return {
        "session": sess,
        "launch": launch,
        "is_observe": is_observe,
        "steps": rows,
        "internal": internal,
        "steps_failed": [r["index"] for r in steps_failed],
        "ladder_steps": ladder,
        "oscillations": osc,
        "timings": timings,
        "marks": marks or [],
        "overall": overall,
        "n_samples": len(metrics),
        "n_agent_samples": len([m for m in metrics if m.get("source") == "agent"]),
        "n_browser_samples": len([m for m in metrics if m.get("source") == "browser"]),
        "stream_verdict": stream_verdict or {},
    }


# ── observations / optimisation candidates ───────────────────────────────────
def observations(a):
    out = []
    rows = a["steps"]
    launch = a["launch"]

    if a["n_browser_samples"] == 0:
        out.append("**browser telemetry absent** — no `source=browser` samples arrived. Is the tab "
                   "foregrounded and still on the session page? A backgrounded tab stops posting "
                   "stats, and every browser column below is therefore empty.")
    if a["n_agent_samples"] == 0:
        out.append("**agent telemetry absent** — no `source=agent` samples. The node agent is not "
                   "reporting `session_metrics` for this session.")

    for r in rows:
        i, tw, th = r["index"], r["target"][0], r["target"][1]
        if r["echo_ms"] is not None and r["echo_ms"] > 1000:
            out.append("step %d (%dx%d): echo latency **%.0f ms** — the client-visible size took "
                       "over a second to follow the PATCH. Candidate: shorten the agent's "
                       "retarget→renegotiate path." % (i, tw, th, r["echo_ms"]))
        if not r["echoed"]:
            out.append("step %d (%dx%d): **no echo** within the timeout (PATCH %s) — the external "
                       "size never reached the target." % (i, tw, th, r["patch_code"]))

    # Bitrate must actually fall when the pixel count does — otherwise the resize
    # bought nothing on the wire, which is the whole point of a degrade ladder.
    base = None
    for r in rows:
        if r["target"] == list(launch) and r["agent"].get("bitrate_kbps"):
            base = r["agent"]["bitrate_kbps"]
            break
    for r in rows:
        br = r["agent"].get("bitrate_kbps")
        if base and br and r["target"][1] <= 720 and br > base * 0.75:
            out.append("step %d (%dx%d): agent bitrate **%.0f kbps** vs %.0f kbps at launch — only "
                       "%.0f%% lower at <=720p, expected >=25%%. Candidate: the encoder bitrate "
                       "target is not following the retarget."
                       % (r["index"], r["target"][0], r["target"][1], br, base,
                          (1 - br / base) * 100.0))

    # Encode cost must fall with pixels.
    prev_px, prev_enc = None, None
    for r in rows:
        px = r["target"][0] * r["target"][1]
        enc = r["agent"].get("encode_ms")
        if prev_px and prev_enc and enc and px < prev_px * 0.9 and enc > prev_enc * 0.95:
            out.append("step %d (%dx%d): encode_ms **%.2f ms** did not fall despite %.0f%% fewer "
                       "pixels (was %.2f ms). Candidate: the encoder is not actually being fed the "
                       "smaller frame." % (r["index"], r["target"][0], r["target"][1], enc,
                                           (1 - px / float(prev_px)) * 100.0, prev_enc))
        if enc:
            prev_px, prev_enc = px, enc

    for r in rows:
        b = r["boundary"]
        if b["pre_sd_mean"] and b["post_sd_mean"] and b["post_sd_mean"] > 2 * b["pre_sd_mean"]:
            out.append("step %d (%dx%d): present σ **%.1f → %.1f ms** across the transition "
                       "(>2x). Candidate: the retarget IDR is landing as a visible hitch."
                       % (r["index"], r["target"][0], r["target"][1],
                          b["pre_sd_mean"], b["post_sd_mean"]))
        pd = b["post_dropped_delta"]
        if pd is not None and pd > 5:
            out.append("step %d (%dx%d): **%d frames dropped** in the 5 s after the step "
                       "(%s before). Candidate: decode-side burst at the resolution change."
                       % (r["index"], r["target"][0], r["target"][1], pd,
                          fmt(b["pre_dropped_delta"], 0)))

    if a["internal"]["verdict"] == "FAIL":
        out.extend("**internal moved** — " + n for n in a["internal"]["violations"])

    if not out:
        out.append("Nothing tripped a rule. Every step echoed, encode cost and bitrate tracked the "
                   "pixel count, and no boundary produced a σ spike or a frame-drop burst.")
    return out


# ── ascii timeline ───────────────────────────────────────────────────────────
def timeline(a, metrics, cols=48):
    rows = a["steps"]
    if not rows:
        return "(no steps)"
    t0 = rows[0]["t_start_ms"]
    t1 = rows[-1]["t_end_ms"]
    span = max(t1 - t0, 1)
    bucket = span / float(cols)

    def ext_at(t):
        for r in rows:
            if r["t_start_ms"] <= t <= r["t_end_ms"]:
                return "%dx%d" % (r["target"][0], r["target"][1])
        return "-"

    lines = ["    t(s)  external      fps   σ(ms)  " + "-" * 20]
    for c in range(cols):
        b0 = t0 + int(c * bucket)
        b1 = t0 + int((c + 1) * bucket)
        bw = window(metrics, "browser", b0, b1)
        f = mean(series(bw, "fps"))
        sd = mean(series(bw, "present_interval_sd_ms"))
        bar = ""
        if f is not None:
            bar = "#" * max(1, min(20, int(round(f / 3.0))))
        lines.append("  %6.0f  %-11s %5s  %6s  %s"
                     % ((b0 - t0) / 1000.0, ext_at(b0), fmt(f, 0), fmt(sd, 1), bar))
    lines.append("    (bar = browser fps / 3, 20 cols == 60 fps)")
    return "\n".join(lines)


# ── report ───────────────────────────────────────────────────────────────────
def render(a, metrics, trace, run_dir):
    s = a["session"]
    L = a["launch"]
    o = []
    o.append("# Soak — adaptive external (stream) resolution")
    o.append("")
    o.append("Generated by `scripts/dx/session_soak_report.py` from `%s`." % run_dir)
    o.append("")
    o.append("| | |")
    o.append("|---|---|")
    o.append("| Session | `%s` |" % s.get("id"))
    o.append("| App | %s |" % (s.get("app_name") or s.get("app_id") or "?"))
    o.append("| Host | %s |" % (s.get("host_name") or s.get("host_id") or "?"))
    o.append("| Codec | %s |" % (s.get("codec") or "?"))
    o.append("| Launch size | %sx%s @ %s fps, %s kbps |"
             % (L[0], L[1], s.get("fps"), s.get("bitrate_kbps")))
    o.append("| Ladder | %s |" % " > ".join("%dx%d" % (r[0], r[1]) for r in (s.get("rungs") or [])))
    o.append("| Profile / duration | %s / %ss |" % (s.get("profile"), s.get("duration_s")))
    o.append("| external_resize_supported | %s |" % s.get("external_resize_supported"))
    o.append("| Samples | %d total (%d agent, %d browser) |"
             % (a["n_samples"], a["n_agent_samples"], a["n_browser_samples"]))
    o.append("| Trace | %s |" % ("captured" if trace else "absent"))
    # ST-09: the STREAM's health is the control plane's Verdict, not this
    # report's. `overall` below answers a different question — did the resize
    # ladder behave — and the two must not be confused, so the stream verdict is
    # named as such and attributed. Absent when the control plane predates the
    # /verdict route.
    sv = a.get("stream_verdict") or {}
    if sv.get("verdict"):
        row = sv["verdict"]
        if sv.get("evidence_tier"):
            row += " (evidence: %s" % sv["evidence_tier"]
            clock = sv.get("clock") or {}
            if clock.get("quality"):
                row += ", clock %s" % clock["quality"]
            row += ")"
        o.append("| Stream verdict (control plane) | %s |" % row)
    o.append("")
    o.append("## Verdict: **%s**" % a["overall"])
    o.append("")
    o.append("This is the RESIZE-LADDER verdict: every step echoed, nothing internal moved, "
             "no oscillation. Stream health is a separate question and a separate authority — "
             "the control plane's Verdict, in the header above.")
    if sv.get("reason"):
        o.append("")
        o.append("> Stream verdict: %s" % sv["reason"])
    o.append("")
    if a.get("is_observe"):
        o.append("This run issued zero PATCHes; every step below was the host's ABR ladder.")
        if a.get("oscillations"):
            o.append("**PUMPING DETECTED** — see Ladder steps below; that alone fails this run.")
    elif a["steps_failed"]:
        o.append("Failed steps: %s" % ", ".join(str(i) for i in a["steps_failed"]))
    else:
        o.append("Every step echoed the target external size and returned `202`.")
    o.append("")
    o.append("**Internal untouched: %s.** The soak sends only `stream_width`/`stream_height`; "
             "`render_*` is never written." % a["internal"]["verdict"])
    for n in a["internal"]["notes"]:
        o.append("- %s" % n)
    o.append("")

    o.append("## Ladder steps")
    o.append("")
    if a.get("is_observe"):
        o.append("This run issued zero PATCHes; every step in the table below is the host's "
                 "ABR ladder acting on its own.")
        o.append("")
    ladder = a.get("ladder_steps") or []
    if not ladder:
        o.append("No `abr.ladder.step` trace events were found. Either the ABR resolution ladder "
                 "never engaged during this run, or this host's node-agent predates the ladder "
                 "trace events / metrics keys (an older agent) — degrading gracefully here, not "
                 "a failure.")
    else:
        t0 = ladder[0]["ts_unix_ms"] or 0
        o.append("| t+s | rung | from → to | reason | setpoint kbps |")
        o.append("|---|---|---|---|---|")
        for s in ladder:
            ts = s.get("ts_unix_ms") or 0
            o.append("| %s | %s | %s → %s | %s | %s |"
                     % (fmt((ts - t0) / 1000.0, 1), s.get("rung"), s.get("from"), s.get("to"),
                        s.get("reason"), fmt(s.get("setpoint_kbps"), 0)))
        o.append("")
        timings = a.get("timings") or {}
        o.append("time to first step: %s s" % fmt(timings.get("time_to_first_step_s"), 1))
        o.append("")
        o.append("time to recover: %s s" % fmt(timings.get("time_to_recover_s"), 1))
        osc = a.get("oscillations") or []
        if osc:
            o.append("")
            o.append("**PUMPING DETECTED** — an A-B-A on the same rung inside %ds:"
                     % (OSCILLATION_WINDOW_MS // 1000))
            for triple in osc:
                o.append("- %s: %s"
                         % (triple[0].get("rung"),
                            " -> ".join("%s@%ss" % (t.get("to"), fmt(((t.get("ts_unix_ms") or 0) - t0) / 1000.0, 1))
                                        for t in triple)))
    o.append("")

    o.append("## Impairment marks")
    o.append("")
    marks = a.get("marks") or []
    if not marks:
        o.append("No `marks.jsonl` — this run was not driven by `abr_ladder_netem.sh` (or ran "
                 "before the marks-based timing fix), so time-to-first-step/time-to-recover above "
                 "fall back to the run's own start time rather than the actual qnetem apply/clear "
                 "instants.")
    else:
        o.append("The wall-clock instant of every `qnetem sender <level>` / `sender-clear` the "
                 "sequencer issued. `time to first step`/`time to recover` above are anchored on "
                 "the FIRST `impair` mark and the LAST `clear` mark here, not on the run's own "
                 "start time.")
        o.append("")
        mt0 = marks[0].get("ts_unix_ms") or 0
        o.append("| t+s | mark | level |")
        o.append("|---|---|---|")
        for m in marks:
            mts = m.get("ts_unix_ms") or 0
            o.append("| %s | %s | %s |" % (fmt((mts - mt0) / 1000.0, 1), m.get("mark"),
                                            m.get("level") or "-"))
    o.append("")

    o.append("## Steps")
    o.append("")
    o.append("Means are steady-state: the first %d s of every dwell is excluded."
             % (STEADY_SKIP_MS / 1000))
    o.append("")
    o.append("| # | target | label | PATCH | PATCH ms | echo ms | a.fps | a.kbps | a.enc ms "
             "| b.fps | b.pfps | b.σ ms | b.σ p95 | b.drop Δ | rtt | jb ms | dec ms | pkt lost Δ |")
    o.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in a["steps"]:
        ag, br = r["agent"], r["browser"]
        o.append("| %s | %dx%d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |"
                 % (r["index"], r["target"][0], r["target"][1], r["label"],
                    r["patch_code"], fmt(r["patch_ms"], 0),
                    fmt(r["echo_ms"], 0) if r["echoed"] else "**TIMEOUT**",
                    fmt(ag.get("fps")), fmt(ag.get("bitrate_kbps"), 0), fmt(ag.get("encode_ms"), 2),
                    fmt(br.get("fps")), fmt(br.get("present_fps")),
                    fmt(br.get("present_interval_sd_ms"), 1), fmt(r["present_sd_p95"], 1),
                    fmt(r["browser_frames_dropped_delta"], 0), fmt(br.get("rtt_ms"), 0),
                    fmt(br.get("jitter_buffer_ms"), 0), fmt(br.get("decode_ms"), 2),
                    fmt(r["packets_lost_delta"], 0)))
    o.append("")

    o.append("## Boundary analysis")
    o.append("")
    o.append("The %d s AFTER each PATCH vs the %d s BEFORE it (browser samples)."
             % (BOUNDARY_MS / 1000, BOUNDARY_MS / 1000))
    o.append("")
    o.append("| # | target | σ before | σ after | σ ratio | dropped before | dropped after | n pre/post |")
    o.append("|---|---|---|---|---|---|---|---|")
    for r in a["steps"]:
        b = r["boundary"]
        ratio = "-"
        if b["pre_sd_mean"] and b["post_sd_mean"]:
            ratio = "%.2fx" % (b["post_sd_mean"] / b["pre_sd_mean"])
        o.append("| %s | %dx%d | %s | %s | %s | %s | %s | %d/%d |"
                 % (r["index"], r["target"][0], r["target"][1],
                    fmt(b["pre_sd_mean"], 1), fmt(b["post_sd_mean"], 1), ratio,
                    fmt(b["pre_dropped_delta"], 0), fmt(b["post_dropped_delta"], 0),
                    b["n_pre"], b["n_post"]))
    o.append("")

    o.append("## Timeline")
    o.append("")
    o.append("```")
    o.append(timeline(a, metrics))
    o.append("```")
    o.append("")

    o.append("## Observations / optimisation candidates")
    o.append("")
    for line in observations(a):
        o.append("- %s" % line)
    o.append("")
    o.append("---")
    o.append("")
    o.append("Raw: `steps.jsonl`, `metrics.jsonl`, `trace.json`, `summary.json`, `raw.ndjson`.")
    return "\n".join(o) + "\n"


def main(argv):
    p = argparse.ArgumentParser()
    p.add_argument("--dir", required=True)
    p.add_argument("--stdout", action="store_true")
    a = p.parse_args(argv)
    d = a.dir

    sess = read_json(os.path.join(d, "session.json"), {}) or {}
    steps = read_jsonl(os.path.join(d, "steps.jsonl"))
    metrics = read_jsonl(os.path.join(d, "metrics.jsonl"))
    trace = read_json(os.path.join(d, "trace.json"), None)
    # The control plane's Verdict for the run window (ST-09). Absent on an older
    # stack; the report then simply omits the row rather than inventing a
    # stream-health opinion of its own.
    stream_verdict = read_json(os.path.join(d, "verdict.json"), None)
    marks = read_jsonl(os.path.join(d, "marks.jsonl"))  # written by abr_ladder_netem.sh, if any

    analysis = analyse(sess, steps, metrics, trace, marks, stream_verdict)
    analysis["observations"] = observations(analysis)
    report = render(analysis, metrics, trace, d)

    if a.stdout:
        sys.stdout.write(report)
        return 0
    with open(os.path.join(d, "summary.json"), "w") as f:
        json.dump(analysis, f, indent=2, sort_keys=True)
    with open(os.path.join(d, "REPORT.md"), "w") as f:
        f.write(report)
    sys.stdout.write("%s\n" % analysis["overall"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
