#!/usr/bin/env python3
"""
session_diagnose.runner — turn ONE diagnostic bundle into a structured smoothness
analysis (network / encoder / client).

    python3 -m session_diagnose.runner --bundle <file.json> --sid <id> --host <host>
    python3 -m session_diagnose.runner --url https://host:8443 --sid <id> [--window a,b]

It mints NOTHING. `scripts/dx/session.sh diagnose` resolves the host, the admin
bearer (scripts/dx/admin_token.sh) and the window, fetches the bundle, and hands
this a FILE. The --url form exists for a direct call and reads the bearer from
$QUASAR_ADMIN_TOKEN — it never logs in and never reads deploy/.env. The old
runner did both, and its hardcoded ~/code/quasar path was wrong on every host in
the fleet.

The classifier's verdict is passed through VERBATIM. This module has no list of
valid verdicts: the control plane owns that vocabulary and grows it.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import ssl
import sys
import urllib.error
import urllib.request
from collections import Counter

_SSL_CTX = ssl._create_unverified_context()


# --------------------------------------------------------------------------- #
# Bundle acquisition                                                           #
# --------------------------------------------------------------------------- #

def fetch_bundle(url: str, sid: str, token: str, window: str | None) -> dict:
    path = f"{url.rstrip('/')}/v1/admin/sessions/{sid}/diagnostic-bundle"
    if window and "," in window:
        frm, to = window.split(",", 1)
        path += f"?from={frm}&to={to}"
    req = urllib.request.Request(path)
    req.add_header("authorization", "Bearer " + token)
    try:
        return json.load(urllib.request.urlopen(req, timeout=20, context=_SSL_CTX))
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")[:400]
        raise SystemExit(
            f"ERROR GET {path} -> HTTP {e.code}: {body}\n"
            f"Next: scripts/dx/admin_token.sh --host <host> --fresh"
        )
    except OSError as e:
        raise SystemExit(f"ERROR GET {path} unreachable: {e}\nNext: make status HOST=<host>")


# --------------------------------------------------------------------------- #
# Stats helpers                                                                #
# --------------------------------------------------------------------------- #

def pctile(values, p):
    if not values:
        return math.nan
    s = sorted(values)
    rank = (p / 100) * (len(s) - 1)
    lo = int(rank)
    hi = min(lo + 1, len(s) - 1)
    return s[lo] + (rank - lo) * (s[hi] - s[lo])


def _vals(pts):
    out = []
    for p in pts:
        v = p[1] if isinstance(p, list) else p.get("v", p.get("V"))
        if isinstance(v, (int, float)) and not isinstance(v, bool):
            out.append(float(v))
    return out


def series_stats(pts) -> dict:
    vals = _vals(pts)
    if not vals:
        return {"count": 0}
    return {
        "count": len(vals),
        "min": round(min(vals), 2),
        "p50": round(pctile(vals, 50), 2),
        "p95": round(pctile(vals, 95), 2),
        "max": round(max(vals), 2),
    }


# --------------------------------------------------------------------------- #
# Shape check — KEYS ONLY                                                      #
# --------------------------------------------------------------------------- #

REQUIRED_BUNDLE_KEYS = ["trace", "window", "clock", "series", "events",
                        "derived_windows", "classifier"]
DERIVED_WINDOWS_KEYS = ["hitches", "abr_downshifts", "encoder_saturation",
                        "likely_network_congestion"]


def validate_bundle(bundle: dict) -> list[str]:
    """Shape only. Returns warnings; raises ValueError on a missing required key.

    There is deliberately NO verdict-vocabulary check here.
    """
    warnings: list[str] = []
    for key in REQUIRED_BUNDLE_KEYS:
        if key not in bundle:
            raise ValueError(f"bundle missing required key '{key}'")

    cl = bundle["classifier"]
    if not isinstance(cl, dict):
        raise ValueError("classifier must be an object")
    for key in ("verdict", "evidence"):
        if key not in cl:
            raise ValueError(f"classifier missing required key '{key}'")
    if not isinstance(cl.get("verdict"), str) or not cl["verdict"]:
        raise ValueError("classifier.verdict must be a non-empty string")

    clock = bundle["clock"]
    if isinstance(clock, dict):
        if clock.get("unmeasured") is True:
            warnings.append("clock is unmeasured (no ping/pong round-trip)")
        elif "client_offset_ms" not in clock:
            warnings.append("clock present but missing client_offset_ms")
    elif clock is not None:
        warnings.append(f"clock has unexpected type {type(clock).__name__}")

    dw = bundle.get("derived_windows") or {}
    for key in DERIVED_WINDOWS_KEYS:
        if key not in dw:
            warnings.append(f"derived_windows missing key '{key}'")
    return warnings


# --------------------------------------------------------------------------- #
# Analysis + rendering                                                         #
# --------------------------------------------------------------------------- #

PRIORITY_SERIES = [
    "encoder.fps", "encoder.encode_ms", "encoder.encode_ms_p95",
    "encoder.bitrate_kbps", "abr.setpoint_kbps",
    "client.fps", "client.present_interval_sd_ms", "client.present_interval_p95_ms",
    "transport.rtt_ms", "transport.jitter_buffer_ms", "transport.packets_lost",
    "client.decode_ms", "client.glass_to_glass_ms",
]

# Long-form labels for the verdicts we happen to know about. An unlisted verdict
# prints verbatim — it is data, not an error.
VERDICT_LABELS = {
    "likely_network_congestion": "NETWORK CONGESTION",
    "likely_encoder_saturation": "ENCODER SATURATION",
    "likely_client_presentation_limit": "CLIENT PRESENTATION LIMIT",
    "nominal": "NOMINAL (healthy — no negative signal)",
    "indeterminate_client_hidden": "INDETERMINATE (the client tab was hidden)",
    "unknown": "UNKNOWN (insufficient signal)",
}


def analyse(bundle: dict, sid: str, host: str, warnings: list[str]) -> dict:
    cl = bundle["classifier"]
    dw = bundle.get("derived_windows") or {}
    ev = bundle.get("events") or []
    ser = bundle.get("series") or {}
    win = bundle.get("window") or {}
    meta = bundle.get("trace") or {}

    abr_downshifts = dw.get("abr_downshifts") or []
    abr_amplitude = None
    if abr_downshifts:
        sp = _vals(ser.get("abr.setpoint_kbps") or [])
        if sp:
            abr_amplitude = round(max(sp) - min(sp), 0)

    return {
        "session": {
            "id": sid,
            "host": host,
            "profile": meta.get("profile_id"),
            "host_id": meta.get("host_id"),
        },
        "window": win,
        "clock": bundle.get("clock"),
        "abr_mode": bundle.get("abr_mode"),
        "classifier": cl,
        "series": {name: series_stats(pts) for name, pts in ser.items()},
        "events": dict(Counter(e.get("type") for e in ev).most_common()),
        "derived_windows": {k: len(v or []) for k, v in dw.items()},
        "abr_sawtooth_amplitude_kbps": abr_amplitude,
        "warnings": warnings,
    }


def render_falsifiers(cl: dict) -> None:
    """The falsifier table — the numbers that would overturn the verdict.

    `holds` answers "does the data satisfy the condition the verdict relies on?",
    NOT "is this good". For a likely_* verdict the conditions that FIRED are the
    ones that hold. A falsifier with no samples shows a dash and its note: an
    absent measurement is never a passing one.

    Absent on a pre-ST-09 control plane, and silently skipped there.
    """
    falsifiers = cl.get("falsifiers") or []
    if not falsifiers:
        return
    print("")
    print("  FALSIFIERS  (ok = the data satisfies the condition the verdict relies on)")
    print("    %-3s %-32s %-9s %12s %-14s %6s"
          % ("", "name", "estimator", "value", "condition", "n"))
    for f in falsifiers:
        val = f.get("value")
        unit = f.get("unit") or ""
        if val is None:
            val_s = "-"
        elif unit in ("count", "bool", ""):
            val_s = "%g" % val
        else:
            val_s = "%g %s" % (val, unit)
        cond = "%s %g" % (f.get("op") or "?", f.get("threshold") or 0)
        note = ("  <- " + f["note"]) if f.get("note") else ""
        print("    %-3s %-32s %-9s %12s %-14s %6s%s"
              % ("ok" if f.get("holds") else "NO",
                 f.get("name") or "?", f.get("estimator") or "?", val_s, cond,
                 f.get("n") if f.get("n") is not None else "?", note))
    if cl.get("thresholds_version"):
        print("    thresholds: %s" % cl["thresholds_version"])


def render(a: dict) -> None:
    sep = "─" * 72
    cl = a["classifier"]
    win = a["window"]
    print(sep)
    print(f"QUASAR SESSION DIAGNOSIS  host={a['session']['host']}  session={a['session']['id']}")
    if a["session"].get("profile"):
        print(f"  profile: {a['session']['profile']}")
    if a.get("abr_mode"):
        print(f"  abr:     {a['abr_mode']}")
    if win:
        span = (int(win.get("to_ms") or 0) - int(win.get("from_ms") or 0)) / 1000
        print(f"  window:  {span:.0f}s  ({win.get('from_ms')} → {win.get('to_ms')})")
    print(sep)

    print("\nCLASSIFIER")
    print(f"  verdict:  {cl.get('verdict')}")
    # ST-09: the classifier is the Verdict VALUE. Everything below is read
    # defensively — a control plane that predates ST-09 returns only verdict +
    # evidence, and this runner must still work against it.
    if cl.get("reason"):
        print(f"  reason:   {cl['reason']}")
    if cl.get("evidence_tier"):
        print(f"  tier:     {cl['evidence_tier']}")
    clock = cl.get("clock")
    if isinstance(clock, dict) and clock.get("quality"):
        extra = ""
        if clock.get("offset_ms") is not None:
            extra += "  offset %s ms" % clock["offset_ms"]
        if clock.get("uncertainty_ms") is not None:
            extra += "  +/-%s ms" % clock["uncertainty_ms"]
        print(f"  clock:    {clock['quality']}{extra}")
    win = cl.get("window") or {}
    if win.get("n_host") is not None or win.get("n_client") is not None:
        print("  samples:  %s host / %s client" % (win.get("n_host", "?"), win.get("n_client", "?")))
    for line in (cl.get("evidence") or []):
        print(f"  evidence: {line}")

    print("\nSERIES  (metric: count  [min / p50 / p95 / max])")
    printed = set()
    for name in PRIORITY_SERIES + sorted(a["series"]):
        if name in printed or name not in a["series"]:
            continue
        printed.add(name)
        st = a["series"][name]
        if st["count"] == 0:
            print(f"  {name}: 0 points")
        else:
            print(f"  {name}: {st['count']}  "
                  f"[{st['min']} / {st['p50']} / {st['p95']} / {st['max']}]")

    print("\nEVENTS  (type: count)")
    if a["events"]:
        for t, c in sorted(a["events"].items(), key=lambda x: -x[1]):
            print(f"  {t}: {c}")
    else:
        print("  (none)")

    print("\nDERIVED WINDOWS  (name: count)")
    for k, c in a["derived_windows"].items():
        print(f"  {k}: {c}")
    if not a["derived_windows"]:
        print("  (none)")

    if a["abr_sawtooth_amplitude_kbps"] is not None:
        print(f"\nABR  setpoint sawtooth amplitude: "
              f"{a['abr_sawtooth_amplitude_kbps']:.0f} kbps "
              f"({a['derived_windows'].get('abr_downshifts', 0)} downshift(s))")

    if a["warnings"]:
        print("\nWARNINGS")
        for w in a["warnings"]:
            print(f"  ! {w}")

    verdict = cl.get("verdict") or "?"
    label = VERDICT_LABELS.get(verdict)
    if label is None:
        label = f"{verdict}  (a verdict this tool does not know — reported verbatim)"
    print(f"\n{sep}")
    print(f"BOTTOM LINE: {label}")
    if cl.get("reason"):
        print(f"  {cl['reason']}")
    render_falsifiers(cl)
    print(sep)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(prog="session_diagnose.runner")
    p.add_argument("--bundle", help="path to a diagnostic-bundle JSON file ('-' = stdin)")
    p.add_argument("--url", help="control-plane base URL (uses $QUASAR_ADMIN_TOKEN)")
    p.add_argument("--sid", default="?")
    p.add_argument("--host", default=os.environ.get("QDIAG_HOST", "?"))
    p.add_argument("--window", help="<from_ms>,<to_ms> (only with --url)")
    p.add_argument("--json", action="store_true")
    p.add_argument("--raw", action="store_true", help="print the bundle and stop")
    a = p.parse_args(argv)

    if a.bundle:
        src = sys.stdin if a.bundle == "-" else open(a.bundle, encoding="utf-8")
        try:
            bundle = json.load(src)
        except ValueError as e:
            print(f"ERROR the bundle is not JSON: {e}", file=sys.stderr)
            return 2
        finally:
            if src is not sys.stdin:
                src.close()
    elif a.url:
        token = os.environ.get("QUASAR_ADMIN_TOKEN", "").strip()
        if not token:
            print("ERROR --url needs $QUASAR_ADMIN_TOKEN. "
                  "Next: export QUASAR_ADMIN_TOKEN=\"$(scripts/dx/admin_token.sh --host <host>)\"",
                  file=sys.stderr)
            return 2
        bundle = fetch_bundle(a.url, a.sid, token, a.window)
    else:
        p.error("one of --bundle or --url is required")
        return 3

    if a.raw:
        print(json.dumps(bundle, indent=2, default=str))
        return 0

    try:
        warnings = validate_bundle(bundle)
    except ValueError as e:
        print(f"ERROR bundle shape: {e}", file=sys.stderr)
        return 2

    analysis = analyse(bundle, a.sid, a.host, warnings)
    if a.json:
        print(json.dumps(analysis, indent=2, default=str))
    else:
        render(analysis)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
