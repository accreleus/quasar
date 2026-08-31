#!/usr/bin/env python3
"""Fold quasar-benchapp + bench-mode output into a bench run directory.

bench_submit.py already knows how to post `<RUNDIR>/metrics.jsonl` as samples and
`<RUNDIR>/trace.json`'s `events[]` as events. Rather than teach it three new file
formats, this script normalises them into those two files, in place:

  1. `app/**/frames.jsonl`  — one JSON object per PRESENTED frame from the app,
     at 60 Hz (~4.9 MB per 12 minutes). Far too dense to post raw, so it is
     aggregated into ONE `source:"app"` sample per wall-clock second.

  2. `app/**/events.jsonl`  — the app's own discrete events (startup, input,
     command). Emitted as `app.event`, payload = the record verbatim.

  3. `bench-windows.json`   — the `window.__qBench.windows()` readout lifted out
     of the peer driver's result. Each already IS a one-second aggregate, so it
     becomes one `source:"browser"` sample, plus an `app.event`-free
     `bench.window` event carrying the same payload for drill-down.

Everything joins on `host_time_ms` / `ts_unix_ms` — both CLOCK_REALTIME unix ms
on the STACK host, the same domain as the agent's `session_metrics.ts_unix_ms`,
so no new clock machinery is needed (bring-up doc §8.1).

Why `app.event` and not `harness.mark`: bench_submit.py MINTS `harness.mark`
itself, with a `{phase, edge}` payload, to tell the server where the baseline /
impaired / settled / recovery windows are. Feeding an app's labelled marks into
that same type would corrupt phase derivation. App marks stay `app.event` with
`payload.mark` set; they are drill-down, not phase boundaries.

Idempotent: rows this script appends are tagged and stripped on a re-run.
"""

import argparse
import json
import os
import sys

# Marks a row/event as ours so a second run replaces rather than duplicates.
STAMP_KEY = "_bench_app_samples"


def _p(values, q):
    """Nearest-rank percentile over a list of floats; None when empty."""
    if not values:
        return None
    s = sorted(values)
    idx = max(0, min(len(s) - 1, int(len(s) * q + 0.999999) - 1))
    return s[idx]


def read_jsonl(path):
    out = []
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    out.append(json.loads(line))
                except json.JSONDecodeError:
                    continue  # a truncated last line is normal on a killed app
    except OSError:
        return []
    return out


def find_files(root, name):
    hits = []
    for base, _dirs, files in os.walk(root):
        if name in files:
            hits.append(os.path.join(base, name))
    return sorted(hits)


def app_samples(frames):
    """Aggregate per-frame records into one sample per wall-clock second."""
    buckets = {}
    for f in frames:
        ts = f.get("host_time_ms")
        if not isinstance(ts, (int, float)):
            continue
        sec = int(ts // 1000)
        b = buckets.setdefault(sec, {"cpu": [], "gpu": [], "late": 0, "n": 0,
                                     "scene": None, "load": None, "fps_target": None,
                                     "imin": None, "imax": None,
                                     "submit_present": [], "render": [], "repaint": []})
        b["n"] += 1
        cpu = f.get("cpu_frame_ms")
        gpu = f.get("gpu_frame_ms")
        if isinstance(cpu, (int, float)):
            b["cpu"].append(float(cpu))
        if isinstance(gpu, (int, float)):
            b["gpu"].append(float(gpu))
        if f.get("presented_late") is True:
            b["late"] += 1
        if isinstance(f.get("scene_id"), (int, float)):
            b["scene"] = f["scene_id"]
        if isinstance(f.get("load"), (int, float)):
            b["load"] = f["load"]
        if isinstance(f.get("fps_target"), (int, float)):
            b["fps_target"] = f["fps_target"]
        # The app -> compositor stage of the latency budget. `host_time_ms` is
        # stamped just BEFORE render (it has to be, to reach that frame's
        # pixels), so `present_ms - host_time_ms` is the whole app-side head of
        # every glass-to-glass number the marker produces. It was 13.80 ms in
        # T1 and quoted as one unexplained lump; `commit_ms` (benchapp) splits
        # it by OWNER, which is the only form in which it is actionable:
        #   render  = commit_ms  - host_time_ms  -> the app's own render+submit
        #   repaint = present_ms - commit_ms     -> waiting for the compositor
        # Both stay absent (not zero) on a compositor without wp_presentation or
        # a benchapp too old to emit commit_ms.
        present = f.get("present_ms")
        commit = f.get("commit_ms")
        if isinstance(present, (int, float)) and not isinstance(present, bool):
            b["submit_present"].append(float(present) - float(ts))
            if isinstance(commit, (int, float)) and not isinstance(commit, bool):
                b["repaint"].append(float(present) - float(commit))
        if isinstance(commit, (int, float)) and not isinstance(commit, bool):
            b["render"].append(float(commit) - float(ts))
        # I7: keep the app's own presented INDEX RANGE for the second. Without
        # it the browser's `missing_indices` count is unattributable — an index
        # the browser never displayed may simply never have been presented by
        # the app, and reducing frames.jsonl to counts threw away the only
        # evidence that could tell the two apart.
        idx = f.get("frame_index")
        if isinstance(idx, (int, float)) and not isinstance(idx, bool):
            idx = int(idx)
            b["imin"] = idx if b["imin"] is None else min(b["imin"], idx)
            b["imax"] = idx if b["imax"] is None else max(b["imax"], idx)

    rows = []
    for sec in sorted(buckets):
        b = buckets[sec]
        m = {
            # fps is deliberately unprefixed: it is directly comparable to the
            # agent's own fps, and `source` already keeps the two series apart.
            "fps": b["n"],
            "frames": b["n"],
            "presented_late": b["late"],
        }
        cpu95 = _p(b["cpu"], 0.95)
        gpu95 = _p(b["gpu"], 0.95)
        if cpu95 is not None:
            m["cpu_frame_ms_p95"] = round(cpu95, 3)
        if gpu95 is not None:
            m["gpu_frame_ms_p95"] = round(gpu95, 3)
        if b["scene"] is not None:
            m["scene_id"] = b["scene"]
        if b["load"] is not None:
            m["load"] = b["load"]
        if b["fps_target"] is not None:
            m["fps_target"] = b["fps_target"]
        # p50 as well as p95: the SHAPE is the diagnosis here. A compositor
        # repaint wait is roughly uniform over one refresh interval, so its p50
        # sits near half a frame and its p95 near a whole one; a constant cost
        # has p50 ~= p95. Reporting only p95 (as the cpu/gpu keys do) would make
        # those two indistinguishable.
        for key, name in (("submit_present", "submit_to_present_ms"),
                          ("render", "render_ms"),
                          ("repaint", "repaint_wait_ms")):
            p50 = _p(b[key], 0.5)
            p95 = _p(b[key], 0.95)
            if p50 is not None:
                m[name.replace("_ms", "_p50_ms")] = round(p50, 3)
            if p95 is not None:
                m[name.replace("_ms", "_p95_ms")] = round(p95, 3)
        if b["imin"] is not None:
            m["frame_index_min"] = b["imin"]
            m["frame_index_max"] = b["imax"]
            # What the app SUBMITTED across this second's index range. Compare
            # against the browser sample's `missing_indices` for the same second:
            #   app_index_span - frames  = indices the app itself never presented
            #   missing_indices - that   = loss downstream of the app
            m["app_index_span"] = b["imax"] - b["imin"] + 1
        rows.append({
            "kind": "metric", "source": "app",
            "ts_unix_ms": sec * 1000, "metrics": m, STAMP_KEY: True,
        })
    return rows


def app_events(records):
    out = []
    for r in records:
        ts = r.get("host_time_ms") or r.get("ts_unix_ms")
        if not isinstance(ts, (int, float)):
            continue
        out.append({
            "ts_unix_ms": int(ts),
            "type": "app.event",
            "payload": dict(r, source="app"),
            STAMP_KEY: True,
        })
    return out


def window_ts(w, prev_ts, fallback_ms):
    """Pick a UNIQUE, monotonic join timestamp for one bench window.

    Preference order (each candidate must be STRICTLY GREATER than the
    previous window's assigned timestamp — samples upsert on `(run, source,
    ts)`, so a duplicate or out-of-order stamp silently overwrites an earlier
    sample and quasar-bench's `expected_count` guard rolls the whole submit
    back, #529):

    1. `last_host_time_ms` — the last decoded frame's own marker timestamp. It
       is the STACK HOST's CLOCK_REALTIME, read out of the pixels, so it needs
       no clock offset and shares a domain with the agent's `ts_unix_ms` and the
       app's `frames.jsonl` exactly. Under impairment a window can decode only
       a frame or two, and two consecutive windows can end on the SAME decoded
       frame — the collision #529 was filed against.
    2. `t_end_host_ms` — the window's close time carried across by the page's
       ST-05 offset. Correct, but inherits the offset's uncertainty. Monotonic
       by construction (each window's own wall-clock close), so it resolves
       the case above.
    3. `t_end_ms` — the window's close on the BROWSER's clock. Used only when
       the offset was never measured; the run is then flagged. Also monotonic
       by construction.

    If every candidate still fails to clear `prev_ts` (all three windows'
    natural timestamps collide, or the clock went backwards), the best
    candidate (1, if present, else the fallback ordinal) is nudged forward by
    1ms from `prev_ts` and the caller is told so it can flag the sample rather
    than pretend the 1ms gap is real data.

    There is deliberately no fourth *natural* option before the nudge. The old
    ordinal stamping (`launch_epoch + i x 1000`) is what this replaced: it
    fabricated a timeline that looked plausible and joined wrongly, with the
    error growing across the run. `fallback_ms` exists only for a legacy
    readout that carries no timestamps at all, and the caller warns loudly
    when it is used.

    Returns (ts_ms, source_label, nudged_bool).
    """
    candidates = []
    for key in ("last_host_time_ms", "t_end_host_ms", "t_end_ms"):
        v = w.get(key)
        if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0:
            candidates.append((int(v), key))
    for v, key in candidates:
        if prev_ts is None or v > prev_ts:
            return v, key, False
    # Every natural candidate collided with (or preceded) the prior window's
    # assigned timestamp. Nudge forward by 1ms from whichever natural value we
    # would otherwise have used (the highest-preference one, if any), so the
    # series stays strictly increasing and the collision is visible rather than
    # silently overwriting an earlier sample.
    base_ts, base_key = candidates[0] if candidates else (int(fallback_ms), "fallback_ordinal")
    nudged_ts = (prev_ts + 1) if prev_ts is not None else base_ts
    return nudged_ts, base_key, True


# Payload keys renamed 2026-08-18. An instrument older than the fold still emits
# the left-hand names; without this map the fold looks for the new key, finds
# nothing, and silently omits the metric ENTIRELY — which is exactly what
# happened to 10 of the 12 runs in suite `opt-t1`, whose drop counts are absent
# from the service even though the runs themselves are fine. A rename that lands
# between the deployed bundle and the fold is a data hole, not a cosmetic change.
LEGACY_KEYS = {
    "dropped": "missing_indices",   # never meant "pipeline loss" — see aggregate.ts
    "crc_fail": "undecoded",        # also counted "no marker present at all"
}


def normalise_window(w):
    """Rename legacy payload keys forward. Returns (window, legacy_key_count)."""
    hits = 0
    out = dict(w)
    for old_key, new_key in LEGACY_KEYS.items():
        if old_key in out and new_key not in out:
            out[new_key] = out.pop(old_key)
            hits += 1
    return out, hits


def bench_rows(windows, fallback_t0_ms, window_ms=1000):
    """One `browser` sample + one `bench.window` event per bench-mode window."""
    samples, events = [], []
    sources = {}
    legacy = 0
    nudged_count = 0
    prev_ts = None
    for i, w in enumerate(windows):
        if not isinstance(w, dict):
            continue
        w, hits = normalise_window(w)
        legacy += hits
        ts, src, nudged = window_ts(w, prev_ts, fallback_t0_ms + i * window_ms)
        prev_ts = ts
        sources[src] = sources.get(src, 0) + 1
        if nudged:
            nudged_count += 1
        m = {}
        for key in ("n", "decoded", "undecoded", "no_image",
                    "missing_indices", "duplicated", "reordered",
                    "g2g_p50_ms", "g2g_p95_ms", "g2g_max_ms", "render_w", "render_h",
                    "offset_ms", "i2p_missed"):
            v = w.get(key)
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                # `n`/`decoded` are too generic to share a namespace with the
                # SPA's own browser stats keys; everything else is unambiguous.
                name = ("bench_" + key) if key in ("n", "decoded") else key
                m[name] = v
        # `stage_*` — the per-stage latency split (web/src/bench/stages.ts).
        # Carried by PREFIX rather than by name, deliberately: the list above is
        # an allow-list, and an instrument that adds a stage key the fold has
        # never heard of would otherwise drop it silently — the exact failure
        # that lost the drop counts of 10 of the 12 runs in suite `opt-t1` (see
        # LEGACY_KEYS above). The prefix is already namespaced, so nothing can
        # collide with the SPA's own browser keys. Nulls are skipped: a stage the
        # browser could not measure must be ABSENT from the series, never 0.
        for key, v in w.items():
            if not (isinstance(key, str) and key.startswith("stage_")):
                continue
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                m[key] = v
        i2p = [x for x in (w.get("i2p_ms") or []) if isinstance(x, (int, float))]
        if i2p:
            m["i2p_p50_ms"] = _p(i2p, 0.5)
            m["i2p_max_ms"] = max(i2p)
            m["i2p_n"] = len(i2p)
        # offset_unknown is the honesty flag: a g2g computed without a clock
        # offset is a raw clock difference, not a latency. Carry it as 0/1.
        m["offset_unknown"] = 1 if w.get("offset_unknown") else 0
        # #529: nudged==1 means this window's natural timestamp collided with
        # (or preceded) the prior window's and was bumped 1ms forward to stay
        # unique/monotonic for the upsert key — a flag, not a real gap.
        if nudged:
            m["ts_nudged"] = 1
        samples.append({
            "kind": "metric", "source": "browser",
            "ts_unix_ms": ts, "metrics": m, STAMP_KEY: True,
        })
        events.append({
            "ts_unix_ms": ts, "type": "bench.window",
            "payload": dict(w, source="browser", ts_source=src, ts_nudged=nudged), STAMP_KEY: True,
        })
    return samples, events, sources, legacy, nudged_count


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dir", required=True, help="bench run directory")
    ap.add_argument("--app-dir", default=None, help="default <dir>/app")
    ap.add_argument("--bench-windows", default=None, help="default <dir>/bench-windows.json")
    ap.add_argument("--t0-ms", type=int, default=0,
                    help="epoch ms of the first bench window (run launch time)")
    args = ap.parse_args()

    rundir = args.dir
    if not os.path.isdir(rundir):
        print(f"bench_app_samples: no such directory: {rundir}", file=sys.stderr)
        return 2
    appdir = args.app_dir or os.path.join(rundir, "app")
    winpath = args.bench_windows or os.path.join(rundir, "bench-windows.json")

    new_samples, new_events = [], []

    frames = []
    for path in find_files(appdir, "frames.jsonl"):
        frames.extend(read_jsonl(path))
    if frames:
        new_samples.extend(app_samples(frames))

    evrecords = []
    for path in find_files(appdir, "events.jsonl"):
        evrecords.extend(read_jsonl(path))
    if evrecords:
        new_events.extend(app_events(evrecords))

    windows = []
    blob = None
    if os.path.exists(winpath):
        try:
            blob = json.load(open(winpath, "r", encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            blob = None
        if isinstance(blob, dict):
            windows = blob.get("windows") or []
        elif isinstance(blob, list):
            windows = blob
    ts_sources = {}
    legacy_keys = 0
    nudged_count = 0
    if windows:
        # Only ever reached by a readout produced before the windows carried
        # their own clock; every current window has t_end_ms at minimum.
        t0 = args.t0_ms
        if not t0:
            t0 = int(frames[0]["host_time_ms"]) if frames and isinstance(
                frames[0].get("host_time_ms"), (int, float)) else 0
        smp, evs, ts_sources, legacy_keys, nudged_count = bench_rows(windows, t0)
        new_samples.extend(smp)
        new_events.extend(evs)

    # ── merge (idempotent: drop anything we wrote on a previous run) ──────────
    metrics_path = os.path.join(rundir, "metrics.jsonl")
    existing = [r for r in read_jsonl(metrics_path) if not r.get(STAMP_KEY)]
    if new_samples:
        rows = existing + new_samples
        rows.sort(key=lambda r: r.get("ts_unix_ms") or 0)
        with open(metrics_path, "w", encoding="utf-8") as fh:
            for r in rows:
                fh.write(json.dumps(r, separators=(",", ":")) + "\n")

    trace_path = os.path.join(rundir, "trace.json")
    if new_events:
        trace = {}
        if os.path.exists(trace_path):
            try:
                trace = json.load(open(trace_path, "r", encoding="utf-8")) or {}
            except (OSError, json.JSONDecodeError):
                trace = {}
        if not isinstance(trace, dict):
            trace = {}
        # MERGE, never replace: session_soak.sh already wrote this file and
        # bench_submit.py reads every event out of it.
        kept = [e for e in (trace.get("events") or []) if not e.get(STAMP_KEY)]
        trace["events"] = sorted(kept + new_events, key=lambda e: e.get("ts_unix_ms") or 0)
        with open(trace_path, "w", encoding="utf-8") as fh:
            json.dump(trace, fh, separators=(",", ":"))

    # ── the per-frame ring (bench-frames.json) ───────────────────────────────
    # Written as a run ARTIFACT, never as samples: 5000 records at 60 Hz is a
    # timeline, not a metric series. It is what makes per-frame drop attribution
    # reconstructable from a submitted run instead of only from a live page.
    ring = []
    if isinstance(blob, dict):
        ring = blob.get("frames") or []
        if isinstance(ring, dict):  # __qBench.dump() shape
            ring = ring.get("frames") or []
    if ring:
        with open(os.path.join(rundir, "bench-frames.json"), "w", encoding="utf-8") as fh:
            json.dump({"frames": ring}, fh, separators=(",", ":"))

    if legacy_keys:
        print("bench_app_samples: NOTE — %d legacy payload key(s) renamed forward "
              "(the instrument predates the fold); the metrics are present under "
              "their current names" % legacy_keys, file=sys.stderr)
    ts_note = ",".join(f"{k}={v}" for k, v in sorted(ts_sources.items())) or "none"
    if ts_sources.get("fallback_ordinal"):
        print("bench_app_samples: WARNING — %d window(s) carried no timestamp and were "
              "stamped by ordinal; their position on the timeline is NOT trustworthy "
              "(stale instrument?)" % ts_sources["fallback_ordinal"], file=sys.stderr)
    if nudged_count:
        # #529: under impairment, consecutive windows can share the same
        # natural join key (a barely-decoding receiver ends two windows on the
        # same frame). window_ts() falls back through t_end_host_ms/t_end_ms
        # first, and only nudges +1ms when even those collide — this is that
        # count. Every nudged sample carries metrics.ts_nudged=1 /
        # payload.ts_nudged=true for drill-down; the series stays unique and
        # monotonic so the submit's expected_count check no longer rolls the
        # whole run back over one duplicated millisecond.
        print("bench_app_samples: NOTE — %d bench window(s) had a colliding join "
              "timestamp and were nudged 1ms forward to stay unique (see "
              "ts_nudged in the samples/events)" % nudged_count, file=sys.stderr)
    print(f"app_frames={len(frames)} app_samples={len(new_samples)} "
          f"app_events={len(evrecords)} bench_windows={len(windows)} "
          f"bench_frames={len(ring)} window_ts={ts_note} legacy_keys={legacy_keys} "
          f"nudged={nudged_count} events_written={len(new_events)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
