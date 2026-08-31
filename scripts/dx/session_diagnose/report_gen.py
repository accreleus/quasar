#!/usr/bin/env python3
"""
_qdiag_report_gen.py — implementation of qdiag-report (see
docs/design/plans/2026-07-21-ir-period-experiment.md §6b).

Generates a single self-contained HTML report (inline CSS/SVG/JS, no CDN, no
external assets) from a set of qdiag-sample-shaped run files plus their
sidecar `<stem>.meta.json` cell-metadata files (written by
qdiag-ir-experiment). Renders correctly even when a cell's metadata marks it
failed, or its run file is missing/degenerate — failed cells are shown, not
dropped.

Sections (in order), each with a stable anchor id for the offline test suite:
  #summary                 — plain-language write-up rendered from --summary <file.md>
  #verdict-table            — one row per matrix cell, colour-coded vs the control row
  #graphs                   — inline SVG bar charts + per-cell time-series strips
  #configuration-appendix    — full cell metadata, verbatim
  #limitations               — quality-not-measured / WiFi / single-pass caveats
"""

import argparse
import html
import json
import math
import re
import sys
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
import validators as V  # noqa: E402

ACCENT = "#3b6bd6"
ACCENT_DARK = "#1f3f8f"
GREEN = "#1f8a4c"
RED = "#c0392b"
GREY = "#8a8f98"

SERIES_NAMES = {
    "present_sd": "client.present_interval_sd_ms",
    "encode_ms": "encoder.encode_ms",
    "fps": "encoder.fps",
    "rtt": "network.rtt_ms",
    "setpoint": "abr.setpoint_kbps",
}


# --------------------------------------------------------------------------- #
# Loading                                                                      #
# --------------------------------------------------------------------------- #

def load_cell(run_path: Path, cfg: dict) -> dict:
    stem = run_path.name[:-5] if run_path.name.endswith(".json") else run_path.stem
    meta_path = run_path.with_name(stem + ".meta.json")
    meta = None
    if meta_path.exists():
        try:
            meta = json.loads(meta_path.read_text())
        except Exception:
            meta = None

    run_data = None
    warnings = []
    try:
        run_data = json.loads(run_path.read_text())
    except Exception as e:
        warnings.append(f"run file did not parse: {e}")

    m = re.match(r"ir-(?P<row>[^-]+(?:-[^-]+)?)-(?P<col>[a-zA-Z0-9]+)-", run_path.name)
    row_id = (meta or {}).get("row") or (m.group("row") if m else "?")
    col_id = (meta or {}).get("col") or (m.group("col") if m else "?")

    status = (meta or {}).get("status", "ok" if run_data else "unknown")

    bundles = []
    if run_data:
        for s in run_data.get("samples", []):
            b = s.get("bundle")
            if b:
                bundles.append((s.get("ts_unix_s"), b))
        if run_data.get("final_bundle") and not bundles:
            bundles.append((None, run_data["final_bundle"]))
        try:
            cwarn = V.validate_run_completeness(run_data, cfg)
            warnings.extend(cwarn)
        except Exception as e:
            warnings.append(str(e))

    return {
        "row": row_id,
        "col": col_id,
        "cell_id": f"ir-{row_id}-{col_id}",
        "status": status,
        "meta": meta,
        "run_path": run_path,
        "bundles": bundles,
        "warnings": warnings,
    }


# --------------------------------------------------------------------------- #
# Metric extraction                                                            #
# --------------------------------------------------------------------------- #

def series_points(bundles, name):
    pts = []
    for ts, b in bundles:
        for p in (b.get("series") or {}).get(name, []):
            if isinstance(p, list) and len(p) >= 2:
                pts.append((p[0], p[1]))
            elif isinstance(p, dict):
                v = p.get("v", p.get("V"))
                t = p.get("t", p.get("T", ts))
                if v is not None:
                    pts.append((t, v))
    return pts


def pctl(values, p):
    if not values:
        return None
    return round(V.percentile(values, p), 2)


def cell_metrics(cell):
    bundles = cell["bundles"]
    if not bundles:
        return {}
    last_bundle = bundles[-1][1]
    dw = last_bundle.get("derived_windows") or {}
    verdicts = [b.get("classifier", {}).get("verdict", "unknown") for _, b in bundles]

    def vals(name):
        return [v for _, v in series_points(bundles, name)]

    return {
        "present_sd_p50": pctl(vals(SERIES_NAMES["present_sd"]), 50),
        "present_sd_p95": pctl(vals(SERIES_NAMES["present_sd"]), 95),
        "encode_ms_p50": pctl(vals(SERIES_NAMES["encode_ms"]), 50),
        "encode_ms_p95": pctl(vals(SERIES_NAMES["encode_ms"]), 95),
        "fps_p10": pctl(vals(SERIES_NAMES["fps"]), 10),
        "fps_p50": pctl(vals(SERIES_NAMES["fps"]), 50),
        "rtt_p95": pctl(vals(SERIES_NAMES["rtt"]), 95),
        "hitches": len(dw.get("hitches", [])),
        "freezes": len(dw.get("hitches", [])),
        "abr_downshifts": len(dw.get("abr_downshifts", [])),
        "verdict": max(set(verdicts), key=verdicts.count) if verdicts else "unknown",
    }


# --------------------------------------------------------------------------- #
# Colour-coding vs control (noise band ±10% or ±1ms, whichever larger)         #
# --------------------------------------------------------------------------- #

def compare_to_control(value, control_value, cfg):
    if value is None or control_value is None:
        return "grey", "no data"
    ir = cfg["ir_experiment"]
    band = max(control_value * ir["noise_band_pct"], ir["noise_band_abs_ms"])
    delta = value - control_value
    if abs(delta) <= band:
        return "grey", f"within noise (Δ{delta:+.2f}ms, band ±{band:.2f}ms)"
    if delta < 0:
        return "green", f"better by {abs(delta):.2f}ms"
    return "red", f"worse by {delta:.2f}ms"


# --------------------------------------------------------------------------- #
# Markdown-lite renderer (headings, bold, lists, paragraphs — no external libs)#
# --------------------------------------------------------------------------- #

def render_markdown_lite(text: str) -> str:
    lines = text.splitlines()
    out = []
    in_list = False

    def close_list():
        nonlocal in_list
        if in_list:
            out.append("</ul>")
            in_list = False

    def inline(s: str) -> str:
        s = html.escape(s)
        s = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", s)
        s = re.sub(r"(?<!\*)\*([^*]+?)\*(?!\*)", r"<em>\1</em>", s)
        return s

    paragraph = []

    def flush_paragraph():
        if paragraph:
            out.append("<p>" + " ".join(paragraph) + "</p>")
            paragraph.clear()

    for raw in lines:
        line = raw.rstrip()
        if not line.strip():
            flush_paragraph()
            close_list()
            continue
        heading = re.match(r"^(#{1,6})\s+(.*)$", line)
        if heading:
            flush_paragraph()
            close_list()
            level = len(heading.group(1))
            out.append(f"<h{level}>{inline(heading.group(2))}</h{level}>")
            continue
        item = re.match(r"^[-*]\s+(.*)$", line)
        if item:
            flush_paragraph()
            if not in_list:
                out.append("<ul>")
                in_list = True
            out.append(f"<li>{inline(item.group(1))}</li>")
            continue
        close_list()
        paragraph.append(inline(line.strip()))

    flush_paragraph()
    close_list()
    return "\n".join(out)


# --------------------------------------------------------------------------- #
# SVG helpers (clean inline SVG, one accent palette, direct series labels,    #
# labelled axes with units, netem-applied moment as a vertical marker)        #
# --------------------------------------------------------------------------- #

def svg_grouped_bar(title: str, rows: list, values_by_row: dict, unit: str, control_row: str, cfg) -> str:
    """values_by_row: {row_id: value_or_None} for a single metric, single column."""
    w, h = 480, 220
    pad_l, pad_r, pad_t, pad_b = 50, 20, 30, 40
    plot_w, plot_h = w - pad_l - pad_r, h - pad_t - pad_b
    vals = [v for v in values_by_row.values() if v is not None]
    vmax = max(vals) * 1.15 if vals else 1.0
    vmax = vmax or 1.0
    n = len(rows)
    bar_w = plot_w / max(n, 1) * 0.6
    slot_w = plot_w / max(n, 1)

    bars = []
    labels = []
    for i, row_id in enumerate(rows):
        v = values_by_row.get(row_id)
        x = pad_l + i * slot_w + (slot_w - bar_w) / 2
        control_v = values_by_row.get(control_row)
        color, _reason = compare_to_control(v, control_v, cfg) if row_id != control_row else ("grey", "control")
        fill = {"green": GREEN, "red": RED, "grey": GREY}.get(color, ACCENT)
        if row_id == control_row:
            fill = ACCENT_DARK
        bar_h = (v / vmax) * plot_h if v is not None else 0
        y = pad_t + plot_h - bar_h
        bars.append(
            f'<rect x="{x:.1f}" y="{y:.1f}" width="{bar_w:.1f}" height="{bar_h:.1f}" fill="{fill}" rx="2"/>'
        )
        val_label = f"{v:.1f}" if v is not None else "—"
        bars.append(
            f'<text x="{x + bar_w/2:.1f}" y="{y - 6:.1f}" text-anchor="middle" '
            f'font-size="11" fill="#2a2f3a">{val_label}</text>'
        )
        labels.append(
            f'<text x="{x + bar_w/2:.1f}" y="{pad_t + plot_h + 16:.1f}" text-anchor="middle" '
            f'font-size="11" fill="#2a2f3a">{html.escape(row_id)}</text>'
        )

    axis = (
        f'<line x1="{pad_l}" y1="{pad_t + plot_h}" x2="{pad_l + plot_w}" y2="{pad_t + plot_h}" '
        f'stroke="#c7ccd4" stroke-width="1"/>'
        f'<line x1="{pad_l}" y1="{pad_t}" x2="{pad_l}" y2="{pad_t + plot_h}" stroke="#c7ccd4" stroke-width="1"/>'
    )

    return (
        f'<svg viewBox="0 0 {w} {h}" xmlns="http://www.w3.org/2000/svg" role="img" '
        f'aria-label="{html.escape(title)}">'
        f'<text x="{pad_l}" y="16" font-size="13" font-weight="600" fill="#1a1d24">{html.escape(title)} '
        f'<tspan font-weight="400" fill="#6b7280">({html.escape(unit)})</tspan></text>'
        f'{axis}{"".join(bars)}{"".join(labels)}</svg>'
    )


def svg_timeseries(title: str, cell, netem_delay_s: float, cfg) -> str:
    """Per-cell time-series strip: setpoint, fps, present-σ, with the netem-applied
    moment (start_time + netem_delay_s) marked as a vertical line."""
    w, h = 480, 180
    pad_l, pad_r, pad_t, pad_b = 45, 15, 20, 25
    plot_w, plot_h = w - pad_l - pad_r, h - pad_t - pad_b
    bundles = cell["bundles"]

    series_defs = [
        ("present_sd", "present σ (ms)", ACCENT),
        ("fps", "fps", GREEN),
        ("setpoint", "setpoint (kbps)", RED),
    ]

    all_ts = []
    normed = {}
    for key, _label, _color in series_defs:
        pts = series_points(bundles, SERIES_NAMES[key])
        if not pts:
            normed[key] = []
            continue
        ts0 = min(t for t, _ in pts if t is not None) if any(t is not None for t, _ in pts) else 0
        vs = [v for _, v in pts]
        vmax = max(vs) or 1.0
        normed[key] = [((t or 0) - ts0, v / vmax) for t, v in pts]
        all_ts.extend(t for t, _ in normed[key])

    tmax = max(all_ts) if all_ts else 1.0
    tmax = tmax or 1.0

    def xy(t, v_norm):
        x = pad_l + (t / tmax) * plot_w
        y = pad_t + plot_h - v_norm * plot_h
        return x, y

    paths = []
    legend = []
    for i, (key, label, color) in enumerate(series_defs):
        pts = normed[key]
        if not pts:
            continue
        d = "M " + " L ".join(f"{x:.1f},{y:.1f}" for x, y in (xy(t, v) for t, v in pts))
        paths.append(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="2"/>')
        legend.append(
            f'<text x="{pad_l + i*140}" y="{h-4}" font-size="10" fill="{color}">{html.escape(label)}</text>'
        )

    marker = ""
    if netem_delay_s is not None and tmax > 0:
        mx = pad_l + min(netem_delay_s / tmax, 1.0) * plot_w
        marker = (
            f'<line x1="{mx:.1f}" y1="{pad_t}" x2="{mx:.1f}" y2="{pad_t + plot_h}" '
            f'stroke="#c0392b" stroke-width="1.5" stroke-dasharray="4,3"/>'
            f'<text x="{mx+3:.1f}" y="{pad_t+10:.1f}" font-size="9" fill="#c0392b">netem applied</text>'
        )

    axis = (
        f'<line x1="{pad_l}" y1="{pad_t + plot_h}" x2="{pad_l + plot_w}" y2="{pad_t + plot_h}" '
        f'stroke="#c7ccd4" stroke-width="1"/>'
        f'<text x="{pad_l}" y="{h-4}" font-size="9" fill="#6b7280"></text>'
    )

    return (
        f'<svg viewBox="0 0 {w} {h}" xmlns="http://www.w3.org/2000/svg" role="img" '
        f'aria-label="{html.escape(title)} time series">'
        f'<text x="{pad_l}" y="12" font-size="11" font-weight="600" fill="#1a1d24">{html.escape(title)}'
        f' <tspan font-weight="400" fill="#6b7280">(x-axis: seconds since cell start)</tspan></text>'
        f'{axis}{marker}{"".join(paths)}{"".join(legend)}</svg>'
    )


def svg_box_summary(title: str, cells: list) -> str:
    """encode_ms box summary: p05-p95 range bar + p50 tick per cell."""
    w = 560
    row_h = 26
    pad_l, pad_r, pad_t = 140, 30, 30
    plot_w = w - pad_l - pad_r
    h = pad_t + row_h * max(len(cells), 1) + 10

    all_p95 = [c["metrics"].get("encode_ms_p95") for c in cells if c["metrics"].get("encode_ms_p95") is not None]
    vmax = (max(all_p95) * 1.15) if all_p95 else 1.0
    vmax = vmax or 1.0

    rows = []
    for i, c in enumerate(cells):
        y = pad_t + i * row_h
        p50 = c["metrics"].get("encode_ms_p50")
        p95 = c["metrics"].get("encode_ms_p95")
        label = f'{c["row"]}/{c["col"]}'
        rows.append(
            f'<text x="4" y="{y+16}" font-size="10" fill="#2a2f3a">{html.escape(label)}</text>'
        )
        if p50 is None or p95 is None:
            rows.append(f'<text x="{pad_l}" y="{y+16}" font-size="10" fill="{GREY}">no data</text>')
            continue
        x0 = pad_l
        x1 = pad_l + (p95 / vmax) * plot_w
        xmid = pad_l + (p50 / vmax) * plot_w
        rows.append(f'<rect x="{x0:.1f}" y="{y+4}" width="{(x1-x0):.1f}" height="12" fill="{ACCENT}" opacity="0.35"/>')
        rows.append(f'<line x1="{xmid:.1f}" y1="{y+2}" x2="{xmid:.1f}" y2="{y+18}" stroke="{ACCENT_DARK}" stroke-width="2"/>')
        rows.append(f'<text x="{x1+4:.1f}" y="{y+15}" font-size="9" fill="#6b7280">p50={p50:.1f} p95={p95:.1f}ms</text>')

    return (
        f'<svg viewBox="0 0 {w} {h}" xmlns="http://www.w3.org/2000/svg" role="img" '
        f'aria-label="{html.escape(title)}">'
        f'<text x="4" y="14" font-size="12" font-weight="600" fill="#1a1d24">{html.escape(title)} '
        f'<tspan font-weight="400" fill="#6b7280">(ms, bar=p50-p95 range)</tspan></text>'
        f'{"".join(rows)}</svg>'
    )


# --------------------------------------------------------------------------- #
# HTML assembly                                                               #
# --------------------------------------------------------------------------- #

CSS = f"""
:root {{ --accent: {ACCENT}; --accent-dark: {ACCENT_DARK}; --green: {GREEN}; --red: {RED}; --grey: {GREY}; }}
* {{ box-sizing: border-box; }}
body {{ font-family: -apple-system, "Segoe UI", Helvetica, Arial, sans-serif; margin: 0; padding: 0;
        background: #f5f6f8; color: #1a1d24; line-height: 1.5; }}
header {{ background: var(--accent-dark); color: #fff; padding: 24px 32px; }}
header h1 {{ margin: 0 0 4px 0; font-size: 22px; }}
header p {{ margin: 0; opacity: 0.85; font-size: 13px; }}
nav.toc {{ background: #fff; border-bottom: 1px solid #dde1e7; padding: 10px 32px; font-size: 13px; }}
nav.toc a {{ margin-right: 16px; color: var(--accent-dark); text-decoration: none; font-weight: 600; }}
main {{ max-width: 1040px; margin: 0 auto; padding: 24px 32px 64px; }}
section {{ background: #fff; border: 1px solid #e3e6ea; border-radius: 8px; padding: 20px 24px; margin-bottom: 24px; }}
section h2 {{ margin-top: 0; font-size: 17px; border-bottom: 2px solid var(--accent); padding-bottom: 6px; }}
table {{ border-collapse: collapse; width: 100%; font-size: 13px; }}
th, td {{ border: 1px solid #e3e6ea; padding: 6px 10px; text-align: left; }}
th {{ background: #f0f2f6; }}
.badge {{ display: inline-block; padding: 2px 8px; border-radius: 10px; font-size: 11px; font-weight: 600; color: #fff; }}
.badge-green {{ background: var(--green); }}
.badge-red {{ background: var(--red); }}
.badge-grey {{ background: var(--grey); }}
.badge-failed {{ background: var(--red); }}
.cell-failed {{ opacity: 0.7; background: #fdf1ef; }}
.graph-grid {{ display: flex; flex-wrap: wrap; gap: 16px; }}
.graph-grid > div {{ flex: 0 0 auto; }}
pre.meta {{ background: #f7f8fa; border: 1px solid #e3e6ea; padding: 10px; font-size: 12px; overflow-x: auto; }}
.limitations li {{ margin-bottom: 6px; }}
"""


def build_verdict_table(cells, cfg):
    ir = cfg["ir_experiment"]
    control_row = ir["control_row"]
    rows_html = []
    for c in cells:
        m = c["metrics"]
        p95 = m.get("present_sd_p95")
        failed = c["status"] == "failed"
        control_cell = next(
            (x for x in cells if x["row"] == control_row and x["col"] == c["col"]), None
        )
        control_p95 = (control_cell or {}).get("metrics", {}).get("present_sd_p95")
        if failed:
            color, reason = "red", "cell failed"
        elif c["row"] == control_row:
            color, reason = "grey", "control"
        else:
            color, reason = compare_to_control(p95, control_p95, cfg)
        badge_class = {"green": "badge-green", "red": "badge-red", "grey": "badge-grey"}[color]
        row_class = ' class="cell-failed"' if failed else ""
        rows_html.append(
            f"<tr{row_class}>"
            f"<td>{html.escape(c['row'])}</td><td>{html.escape(c['col'])}</td>"
            f"<td>{'FAILED' if failed else (f'{p95:.1f}' if p95 is not None else '—')}</td>"
            f"<td>{m.get('hitches', '—')}</td>"
            f"<td>{html.escape(str(m.get('verdict', '—')))}</td>"
            f'<td><span class="badge {badge_class}">{html.escape(reason)}</span></td>'
            f"</tr>"
        )
    return (
        '<table><thead><tr><th>Row</th><th>Column</th><th>present σ p95 (ms)</th>'
        '<th>Freezes</th><th>Classifier verdict</th><th>vs control</th></tr></thead>'
        f"<tbody>{''.join(rows_html)}</tbody></table>"
    )


def build_graphs(cells, cfg):
    ir = cfg["ir_experiment"]
    rows = list(ir["rows"].keys())
    cols = list(ir["cols"].keys())
    control_row = ir["control_row"]

    parts = ['<div class="graph-grid">']
    for col_id in cols:
        col_cells = {c["row"]: c for c in cells if c["col"] == col_id}
        sd_vals = {r: (col_cells[r]["metrics"].get("present_sd_p95") if r in col_cells else None) for r in rows}
        freeze_vals = {r: (col_cells[r]["metrics"].get("freezes") if r in col_cells else None) for r in rows}
        parts.append(f"<div>{svg_grouped_bar(f'present σ p95 — {col_id}', rows, sd_vals, 'ms', control_row, cfg)}</div>")
        parts.append(f"<div>{svg_grouped_bar(f'freeze count — {col_id}', rows, freeze_vals, 'count', control_row, cfg)}</div>")
    parts.append("</div>")

    parts.append('<div class="graph-grid">')
    for c in cells:
        if not c["bundles"]:
            continue
        cell_title = f"{c['row']} x {c['col']}"
        parts.append(
            f"<div>{svg_timeseries(cell_title, c, ir['netem_apply_delay_s'], cfg)}</div>"
        )
    parts.append("</div>")

    parts.append(svg_box_summary("encode_ms per cell", cells))
    return "".join(parts)


def build_configuration_appendix(cells):
    blocks = []
    for c in cells:
        meta = c["meta"] or {"note": "no sidecar metadata file found"}
        blocks.append(
            f"<h3>{html.escape(c['cell_id'])} "
            f'<span class="badge {"badge-failed" if c["status"] == "failed" else "badge-grey"}">{html.escape(c["status"])}</span></h3>'
            f"<pre class=\"meta\">{html.escape(json.dumps(meta, indent=2, default=str))}</pre>"
        )
    return "".join(blocks)


LIMITATIONS_HTML = """
<ul class="limitations">
  <li><strong>Picture quality was not measured.</strong> No PSNR/VMAF tap exists in this
      harness; the intra-refresh overhead hypothesis is assessed indirectly (if a
      periodic row matches the continuous row on recovery metrics, its lower
      steady-state intra fraction is a strict win by construction) and directly by
      a follow-up human eyeball pass — not by any number in this report.</li>
  <li><strong>A wireless LAN link on the peer host.</strong> Cross-host "clean" cells then carry real
      ambient jitter; treat small clean-column deltas with proportionate skepticism.</li>
  <li><strong>Single pass.</strong> Each cell in this report is one ~120s sample, not an
      average of repeated runs. A marginal verdict should be re-run before it is
      treated as a decision input.</li>
</ul>
"""


def main(argv):
    p = argparse.ArgumentParser(prog="qdiag-report")
    p.add_argument("runs", nargs="+", help="run file(s), e.g. qdiag-runs/ir-*.json")
    p.add_argument("--out", required=True)
    p.add_argument("--summary", default=None, help="markdown-lite file rendered into the summary section")
    args = p.parse_args(argv)

    cfg = V.load_config()
    V.validate_ir_experiment_config(cfg)

    run_paths = [Path(x) for x in args.runs if not x.endswith(".meta.json")]
    # A missing FILE is an argument error (typo/garbage token), not a "failed
    # cell" — failed cells are marked via their sidecar meta and still render.
    missing = [str(p) for p in run_paths if not p.is_file()]
    if missing:
        print(f"qdiag-report: no such run file(s): {', '.join(missing)}", file=sys.stderr)
        return 2
    cells = []
    for rp in run_paths:
        c = load_cell(rp, cfg)
        c["metrics"] = cell_metrics(c)
        cells.append(c)

    summary_html = "<p><em>No --summary provided.</em></p>"
    if args.summary:
        summary_text = Path(args.summary).read_text()
        summary_html = render_markdown_lite(summary_text)

    n_ok = sum(1 for c in cells if c["status"] != "failed")
    n_failed = sum(1 for c in cells if c["status"] == "failed")

    body = f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>IR-period experiment report</title>
<style>{CSS}</style>
</head>
<body>
<header>
  <h1>IR-period experiment: rolling intra-refresh overhead vs recovery</h1>
  <p>{len(cells)} cell(s) — {n_ok} ok / {n_failed} failed. Generated offline from run files + metadata.</p>
</header>
<nav class="toc">
  <a href="#summary">Summary</a>
  <a href="#verdict-table">Verdict table</a>
  <a href="#graphs">Graphs</a>
  <a href="#configuration-appendix">Configuration appendix</a>
  <a href="#limitations">Limitations</a>
</nav>
<main>
  <section id="summary">
    <h2>Plain-language summary</h2>
    {summary_html}
  </section>
  <section id="verdict-table">
    <h2>Verdict table</h2>
    {build_verdict_table(cells, cfg)}
  </section>
  <section id="graphs">
    <h2>Graphs</h2>
    {build_graphs(cells, cfg)}
  </section>
  <section id="configuration-appendix">
    <h2>Configuration appendix</h2>
    {build_configuration_appendix(cells)}
  </section>
  <section id="limitations">
    <h2>Limitations</h2>
    {LIMITATIONS_HTML}
  </section>
</main>
</body>
</html>
"""

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(body)
    print(f"qdiag-report: wrote {out_path} ({len(cells)} cells, {n_failed} failed)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
