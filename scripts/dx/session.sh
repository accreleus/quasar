#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/session.sh — read a LIVE session's observability surface from this
# workstation. One verb per question, one RESULT line per run, no prompts.
#
#   scripts/dx/session.sh <verb> [<sid>|latest]
#
#   list       every session the stack knows about (running first)
#   verdict    the session's Verdict: state, reason, tier, clock, falsifiers
#   metrics    the recent telemetry samples as a compact table
#   trace      GET /trace/window + /trace/events as a table of events
#   bundle     the raw bundle JSON, written to a file
#   capture    arm ONE bounded observation of the live session and write it out
#   logs       docker logs for the session's containers, over ssh
#   diagnose   the full structured analysis (the former `qdiag`)
#
# Knobs (all read from the environment, so `make session-<verb> KEY=value` works):
#   SID=<uuid>|latest   the session. `latest` = newest state=running.
#   HOST=<role|name>    the stack, from .claude/skills/_shared/hosts.json.
#   WINDOW=<from>,<to>  explicit unix-ms window (verdict/trace/bundle/diagnose).
#   JSON=1              machine-readable output; the RESULT fields are inside it.
#   SINCE=<10m|600s>    metrics/logs: how far back to look.
#   N=<rows>            metrics: how many samples to print (default 20).
#   GREP=<pattern>      logs: extra grep -E filter.
#   OUT=<path>          bundle: where to write (default .diagnostics/bundle-<sid>-<ts>.json).
#   KIND=<k>            capture: pipeline_dot|encoder_props|burst_stats|all.
#   OUT=<dir>           capture: directory to write into (default .diagnostics/).
#   WINDOWS=<n>         capture burst_stats: how many windows (1-40, default 20).
#   WINDOW_MS=<ms>      capture burst_stats: window length (100-1000, default 250).
#   CAPTURE_TIMEOUT_S=  capture: seconds to poll for the result (default 15).
#
# EXIT CODES
#   0  ok (including a verdict this tool does not recognise — that is DATA)
#   1  the session is degraded: the classifier returned a likely_* verdict
#   2  tool error — auth, unreachable, 404. Always names the next command.
#   3  usage error
#
# TLS: the workstation curls the host's `api_external`. The fleet certs are
# trusted here, so there is no -k by default. A host may set "tls_insecure": true
# in hosts.json, or you can pass QUASAR_CURL_INSECURE=1 for a one-off.
#
# AUTH: scripts/dx/admin_token.sh, THE ladder. Nothing here mints credentials.
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

VERB="${1:-help}"
shift || true
if [ $# -gt 0 ] && [ -n "${1:-}" ]; then SID="${SID:-$1}"; fi

SID="${SID:-}"
WINDOW="${WINDOW:-}"
JSON="${JSON:-}"
SINCE="${SINCE:-}"
GREP_PAT="${GREP:-}"
OUT_PATH="${OUT:-}"
ROWS="${N:-20}"
KIND="${KIND:-}"
WINDOWS="${WINDOWS:-}"
WINDOW_MS="${WINDOW_MS:-}"

usage() { awk 'NR>3 && !/^#/ {exit} NR>3 {sub(/^# ?/,""); print}' "${BASH_SOURCE[0]}" >&2; }

# ── The single terminal line ─────────────────────────────────────────────────
# RESULT status=<ok|degraded|failed|error> target=session-<verb> sid=<sid> host=<host> [k=v ...]
RESULT_EMITTED=0
result() { # result <status> [k=v ...]
  local status="$1"; shift || true
  RESULT_EMITTED=1
  # JSON=1 keeps stdout PURE JSON (the RESULT fields are inside the object);
  # the human RESULT line then goes to stderr so `| python3 -m json.tool` works.
  local out=1
  [ "${JSON:-}" = 1 ] && out=2
  {
    printf 'RESULT status=%s target=session-%s sid=%s host=%s' \
      "$status" "$VERB" "${SID:--}" "$DX_HOST"
    local kv
    for kv in "$@"; do printf ' %s' "$kv"; done
    printf '\n'
  } >&"$out"
  case "$status" in
    ok) exit 0 ;;
    degraded|failed) exit 1 ;;
    *) exit 2 ;;
  esac
}

# A tool error ALWAYS names the next command. `reason=` is one token so the line
# stays greppable; the human sentence goes on its own line above it.
err_out() { # err_out <reason-token> <sentence>
  printf 'ERROR %s\n' "$2" >&2
  result error "reason=$1"
}

usage_out() { printf 'usage: %s\n' "$1" >&2; usage; exit 3; }

case "$VERB" in
  help|-h|--help) usage; exit 0 ;;
  list|verdict|metrics|trace|bundle|capture|logs|diagnose) ;;
  *) usage_out "unknown verb '$VERB'" ;;
esac

dx_have curl  || err_out no-curl "curl is not on PATH. Next: install curl."
dx_have python3 || err_out no-python "python3 is not on PATH. Next: install python3 (every DX script needs it)."

# ── Host + API base ──────────────────────────────────────────────────────────
API_BASE=""
CURL_K=()
if [ "$DX_HOST" = "local" ]; then
  API_BASE="http://127.0.0.1:${DX_CP_PORT}"
else
  dx_resolve_remote "$DX_HOST" || err_out unknown-host \
    "HOST='$DX_HOST' is not a known role or host. Next: make session-list HOST=<role>, or add it to .claude/skills/_shared/hosts.json."
  API_BASE="${DX_REMOTE_API_EXTERNAL:-}"
  [ -n "$API_BASE" ] || err_out no-api \
    "host '$DX_REMOTE_NAME' has no api_external in hosts.json. Next: add \"api_external\" to its entry."
  if [ -n "${DX_REMOTE_TLS_INSECURE:-}" ]; then CURL_K=(-k); fi
fi
if [ "${QUASAR_CURL_INSECURE:-0}" = "1" ]; then CURL_K=(-k); fi
API_BASE="${API_BASE%/}"

# ── Auth ─────────────────────────────────────────────────────────────────────
TOKEN="$(QUASAR_ADMIN_TOKEN="${QUASAR_ADMIN_TOKEN:-${QSES_ADMIN_TOKEN:-}}" \
  bash "$DX_DIR/admin_token.sh" --host "$DX_HOST" --quiet)" || TOKEN=""
[ -n "$TOKEN" ] || err_out no-token \
  "no admin bearer for host=$DX_HOST (the ladder printed every tier it tried above). Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh"

# The bearer goes to curl through a 0600 header FILE, never argv: any local user
# can read another process's argv out of `ps auxww`.
HDR_FILE="$(mktemp)"; chmod 600 "$HDR_FILE"
printf 'Authorization: Bearer %s\n' "$TOKEN" > "$HDR_FILE"
RESP_FILE="$(mktemp)"
trap 'rm -f "$HDR_FILE" "$RESP_FILE"' EXIT

API_CODE=""
api_get() { # api_get <path> ; body lands in $RESP_FILE, status in $API_CODE
  # ${a[@]+"${a[@]}"} — macOS ships bash 3.2, where a plain "${a[@]}" on an
  # EMPTY array is an unbound-variable error under `set -u`.
  API_CODE="$(curl -sS ${CURL_K[@]+"${CURL_K[@]}"} -o "$RESP_FILE" -w '%{http_code}' --max-time 30 \
    -H @"$HDR_FILE" "$API_BASE$1" 2>/dev/null || printf '000')"
}

# Every non-200 names the next command. A 401 is the interesting one: the cached
# token outlived the stack that minted it, and `--fresh` is the whole fix.
api_or_die() { # api_or_die <path> <what>
  api_get "$1"
  case "$API_CODE" in
    200) return 0 ;;
    401|403) err_out unauthorized \
      "token rejected (HTTP $API_CODE) on $2. Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh" ;;
    404) err_out not-found \
      "404 on $2 — no such session on host=$DX_HOST (a stopped session KEEPS its row, so a 404 usually means the wrong stack). Next: make session-list HOST=$DX_HOST" ;;
    000) err_out unreachable \
      "no response from $API_BASE — the stack is down or unreachable. Next: make status HOST=$DX_HOST" ;;
    *) err_out "http-$API_CODE" "HTTP $API_CODE on $2. Next: make status HOST=$DX_HOST" ;;
  esac
}

# api_post is deliberately NOT wrapped in api_or_die: the capture verb has to
# read 202/404/409/422/501/503 apart from one another and name a DIFFERENT next
# command for each, which a shared "any non-200 is fatal" helper cannot do.
api_post() { # api_post <path> <json-body> ; body lands in $RESP_FILE, status in $API_CODE
  API_CODE="$(curl -sS ${CURL_K[@]+"${CURL_K[@]}"} -o "$RESP_FILE" -w '%{http_code}' --max-time 30 \
    -X POST -H @"$HDR_FILE" -H 'Content-Type: application/json' \
    --data "$2" "$API_BASE$1" 2>/dev/null || printf '000')"
}

# ── SID resolution ───────────────────────────────────────────────────────────
# There is no ?state= filter on GET /v1/admin/sessions — the filtering is ours,
# exactly as session_soak_driver.py's latest_running() does it.
resolve_latest() {
  api_or_die "/v1/admin/sessions?limit=100" "GET /v1/admin/sessions"
  local sid
  sid="$(python3 - "$RESP_FILE" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit(0)
items = [i for i in (body.get("items") or []) if i.get("state") == "running"]
items.sort(key=lambda i: i.get("created_at") or "", reverse=True)
print(items[0].get("id") if items else "")
PY
)"
  [ -n "$sid" ] || err_out no-running-session \
    "no session in state=running on host=$DX_HOST. Next: make session-list HOST=$DX_HOST (it shows every state), or launch one first."
  SID="$sid"
  printf 'sid: %s (newest running)\n' "$SID" >&2
}

need_sid() {
  # `[ x = y ] && f` as the LAST statement of a function returns 1 when the test
  # is false, and under `set -e` the caller then dies silently. Always an `if`.
  [ -n "$SID" ] || usage_out "$VERB needs SID=<uuid>|latest"
  # A session id is a UUID. It was never checked, and `session-logs` builds a
  # remote shell script around it (SID='<sid>' inside the snippet ssh executes),
  # so a `'` in SID= was command execution as the fleet ssh account. Validated
  # here, in the one gate every SID-taking verb already passes through — before
  # `latest` is resolved, so a malformed literal is rejected on sight...
  if [ "$SID" != latest ]; then
    dx_require_safe "$VERB" "SID" "$SID" "$DX_RE_SID" "A session id is a UUID."
  fi
  if [ "$SID" = latest ]; then
    resolve_latest
    # ...and again after, because resolve_latest reads the id back out of a
    # control-plane response. Server-generated, but it crosses a network before
    # it is spliced into that remote snippet, which is enough to re-check.
    dx_require_safe "$VERB" "resolved SID" "$SID" "$DX_RE_SID" \
      "The control plane returned a session id that is not a UUID."
  fi
  return 0
}

window_qs() { # prints ?from=..&to=.. or ''
  [ -n "$WINDOW" ] || return 0
  case "$WINDOW" in
    *,*) printf '?from=%s&to=%s' "${WINDOW%%,*}" "${WINDOW##*,}" ;;
    *) usage_out "WINDOW must be <from_ms>,<to_ms> (got '$WINDOW')" ;;
  esac
}

since_ms() { # prints a unix-ms floor for $SINCE, or 0
  local raw="${1:-}" n
  [ -n "$raw" ] || { printf '0'; return 0; }
  case "$raw" in
    *h) n="${raw%h}"; n=$(( n * 3600 )) ;;
    *m) n="${raw%m}"; n=$(( n * 60 )) ;;
    *s) n="${raw%s}" ;;
    *) n="$raw" ;;
  esac
  case "$n" in ''|*[!0-9]*) usage_out "SINCE must be Nh/Nm/Ns or seconds (got '$raw')" ;; esac
  printf '%s' "$(( ( $(date -u +%s) - n ) * 1000 ))"
}

# ── Verbs ────────────────────────────────────────────────────────────────────
case "$VERB" in

list)
  api_or_die "/v1/admin/sessions?limit=100" "GET /v1/admin/sessions"
  COUNT_FILE="$(mktemp)"
  JSON="$JSON" HOSTV="$DX_HOST" COUNT_FILE="$COUNT_FILE" python3 - "$RESP_FILE" <<'PY'
import json, os, sys
body = json.load(open(sys.argv[1]))
items = body.get("items") or []
rank = {"running": 0, "starting": 1, "pending": 1}
items.sort(key=lambda i: (rank.get(i.get("state"), 9), i.get("created_at") or ""))
if os.environ.get("JSON") == "1":
    print(json.dumps({"status": "ok", "target": "session-list", "sid": "-",
                      "host": os.environ["HOSTV"], "sessions": items},
                     indent=2, default=str))
else:
    if not items:
        print("(no sessions on this stack)")
    else:
        print("%-38s %-10s %-22s %-14s %s" % ("SESSION", "STATE", "APP", "HOST", "CREATED"))
        for i in items:
            print("%-38s %-10s %-22s %-14s %s" % (
                i.get("id", "?"), i.get("state", "?"),
                (i.get("app_name") or "?")[:22], (i.get("host_name") or "?")[:14],
                i.get("created_at") or "?"))
open(os.environ["COUNT_FILE"], "w").write(str(len(items)))

PY
  COUNT="$(cat "$COUNT_FILE" 2>/dev/null || echo 0)"; rm -f "$COUNT_FILE"
  result ok "count=$COUNT"
  ;;

verdict|bundle|diagnose)
  need_sid

  # ST-09: `verdict` prefers the dedicated Verdict read. It is a fraction of the
  # bundle's size and carries the same value, built by the same function on the
  # server, so the two can never disagree.
  #
  # A 404 on it is ambiguous — a control plane that predates the route, or a
  # session this stack has never heard of — so we do NOT decide here: we fall
  # through to the bundle, whose own 404 handling names the right next command.
  # That fallback is also what lets this script keep working against a host that
  # has not been redeployed yet.
  VERDICT_SRC=bundle
  if [ "$VERB" = verdict ]; then
    api_get "/v1/admin/sessions/$SID/verdict$(window_qs)"
    case "$API_CODE" in
      200) VERDICT_SRC=verdict-route ;;
      404) : ;;
      401|403) err_out unauthorized \
        "token rejected (HTTP $API_CODE) on GET /v1/admin/sessions/$SID/verdict. Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh" ;;
      000) err_out unreachable \
        "no response from $API_BASE — the stack is down or unreachable. Next: make status HOST=$DX_HOST" ;;
      *) err_out "http-$API_CODE" \
        "GET /v1/admin/sessions/$SID/verdict returned HTTP $API_CODE. Next: make session-bundle SID=$SID HOST=$DX_HOST" ;;
    esac
  fi

  if [ "$VERDICT_SRC" = bundle ]; then
    api_or_die "/v1/admin/sessions/$SID/diagnostic-bundle$(window_qs)" \
      "GET /v1/admin/sessions/$SID/diagnostic-bundle"
  fi

  if [ "$VERB" = bundle ]; then
    if [ -z "$OUT_PATH" ]; then
      mkdir -p "$DX_DIAG_DIR"
      chmod 700 "$DX_DIAG_DIR" 2>/dev/null || true
      OUT_PATH="$DX_DIAG_DIR/bundle-${SID}-$(dx_timestamp).json"
    else
      mkdir -p "$(dirname "$OUT_PATH")"
    fi
    cp "$RESP_FILE" "$OUT_PATH"
    chmod 600 "$OUT_PATH" 2>/dev/null || true
    printf '%s\n' "$OUT_PATH"
    result ok "out=$OUT_PATH"
  fi

  if [ "$VERB" = diagnose ]; then
    # The analysis is a pure function of the bundle. The runner is fed a FILE and
    # mints nothing: credentials, host resolution and the window all happened above.
    ANALYSIS_RC=0
    PYTHONPATH="$DX_DIR${PYTHONPATH:+:$PYTHONPATH}" QDIAG_HOST="$DX_HOST" \
      python3 -m session_diagnose.runner \
        --bundle "$RESP_FILE" --sid "$SID" --host "$DX_HOST" \
        ${JSON:+--json} || ANALYSIS_RC=$?
    if [ "$ANALYSIS_RC" != 0 ]; then
      err_out analysis-failed \
        "the bundle analyser exited $ANALYSIS_RC. Next: make session-bundle SID=$SID HOST=$DX_HOST and inspect the raw JSON."
    fi
  fi

  # verdict/diagnose both end on the Verdict. It is the whole body of the
  # /verdict read and the `classifier` key of a bundle — one shape, two places.
  VERDICT_LINE="$(python3 - "$RESP_FILE" <<'PY'
import json, sys
try:
    b = json.load(open(sys.argv[1]))
except Exception as e:
    print("PARSE\t%s" % e); raise SystemExit(0)
v = b.get("classifier") if isinstance(b.get("classifier"), dict) else b
w = v.get("window") or b.get("window") or {}
print("%s\t%s\t%s" % (v.get("verdict") or "", w.get("from_ms") or 0, w.get("to_ms") or 0))
PY
)"
  case "$VERDICT_LINE" in
    PARSE*) err_out bad-bundle \
      "the diagnostic response did not parse as JSON (${VERDICT_LINE#PARSE	}). Next: make session-bundle SID=$SID HOST=$DX_HOST and look at the file." ;;
  esac
  VERDICT="$(printf '%s' "$VERDICT_LINE" | cut -f1)"
  WFROM="$(printf '%s' "$VERDICT_LINE" | cut -f2)"
  WTO="$(printf '%s' "$VERDICT_LINE" | cut -f3)"
  [ -n "$VERDICT" ] || err_out no-verdict \
    "the response carries no verdict. Next: make session-bundle SID=$SID HOST=$DX_HOST and check the control-plane version."

  # The verdict vocabulary is the CONTROL PLANE's, not ours. A string this script
  # has never heard of is DATA: print it, say so, and exit 0. The 2026-08-22
  # incident was a stale four-string enum here turning a healthy `nominal`
  # session into exit 2, which is what sent an agent to psql.
  NOTE=""
  case "$VERDICT" in
    nominal|unknown|indeterminate_client_hidden) STATUS=ok ;;
    likely_network_congestion|likely_encoder_saturation|likely_client_presentation_limit) STATUS=degraded ;;
    *) STATUS=ok
       NOTE="verdict '$VERDICT' is not one this tool knows — reporting it verbatim, not failing on it" ;;
  esac

  if [ "$VERB" = verdict ]; then
    VERDICT="$VERDICT" STATUS="$STATUS" NOTE="$NOTE" SIDV="$SID" HOSTV="$DX_HOST" \
      SRC="$VERDICT_SRC" WFROM="$WFROM" WTO="$WTO" JSONV="${JSON:-}" \
      python3 - "$RESP_FILE" <<'PY'
import json, os, sys

body = json.load(open(sys.argv[1]))
# One shape, two places: the /verdict body IS the value; a bundle carries it
# under `classifier`.
v = body.get("classifier") if isinstance(body.get("classifier"), dict) else body
win = v.get("window") or body.get("window") or {}
falsifiers = v.get("falsifiers") or []

if os.environ.get("JSONV") == "1":
    # The Verdict verbatim, plus the RESULT fields, so a JSON consumer never has
    # to parse the terminal line.
    out = dict(v)
    out.update({
        "status": os.environ["STATUS"], "target": "session-verdict",
        "sid": os.environ["SIDV"], "host": os.environ["HOSTV"],
        "source": os.environ["SRC"],
    })
    if isinstance(body.get("classifier"), dict):
        # Came from the bundle fallback, so these are available for free.
        out["abr_mode"] = body.get("abr_mode")
        out["derived_windows"] = {k: len(x or []) for k, x in (body.get("derived_windows") or {}).items()}
    if os.environ.get("NOTE"):
        out["note"] = os.environ["NOTE"]
    print(json.dumps(out, indent=2, default=str))
    raise SystemExit(0)

print("session: %s   host: %s" % (os.environ["SIDV"], os.environ["HOSTV"]))
frm, to = int(win.get("from_ms") or 0), int(win.get("to_ms") or 0)
if to:
    span = "window:  %ss  (%s -> %s)" % ((to - frm) // 1000, frm, to)
    n_host, n_client = win.get("n_host"), win.get("n_client")
    if n_host is not None or n_client is not None:
        span += "   samples: %s host / %s client" % (n_host if n_host is not None else "?",
                                                     n_client if n_client is not None else "?")
    print(span)
print("verdict: %s" % (v.get("verdict") or "?"))
if v.get("reason"):
    print("reason:  %s" % v["reason"])
if v.get("evidence_tier"):
    print("tier:    %s" % v["evidence_tier"])
clock = v.get("clock")
if isinstance(clock, dict) and clock.get("quality"):
    line = "clock:   %s" % clock["quality"]
    if clock.get("offset_ms") is not None:
        line += "  offset %s ms" % clock["offset_ms"]
    if clock.get("uncertainty_ms") is not None:
        line += "  ±%s ms" % clock["uncertainty_ms"]
    # applied is the load-bearing half: a measured clock that was never applied
    # means the two timelines were compared as if they agreed.
    if clock.get("applied") is not None:
        line += "  %s" % ("applied" if clock["applied"] else "NOT applied")
    if clock.get("age_ms") is not None:
        line += "  age %ss" % (int(clock["age_ms"]) // 1000)
    print(line)
warm = win.get("warmup_excluded_ms")
if warm:
    print("warm-up: first %ss of the window excluded from hitch detection and the host-fps floor"
          % (int(warm) // 1000))
# Only ever present on a bundle, and only when this control plane dropped
# something. Silence means nothing was rejected.
ing = body.get("ingest")
if isinstance(ing, dict) and ing.get("rejected_ts"):
    line = "ingest:  %s client point(s) DROPPED for an implausible ts_unix_ms" % ing["rejected_ts"]
    if ing.get("last_rejected_reason"):
        line += " (last: %s, %s)" % (ing.get("last_rejected_ts_unix_ms"), ing["last_rejected_reason"])
    print(line)

for line in (v.get("evidence") or []):
    print("  evidence: %s" % line)

if falsifiers:
    # The falsifiers ARE the argument: these are the numbers that would overturn
    # the verdict. `holds` answers "does the data satisfy the condition the
    # verdict relies on" — for a likely_* verdict the conditions that FIRED are
    # the ones that hold, so a ✓ is not "good", it is "this leg stands".
    print("")
    print("falsifiers  (✓ = the data satisfies the condition the verdict relies on)")
    hdr = "  %-1s %-32s %-9s %12s %-14s %6s" % ("", "name", "estimator", "value", "condition", "n")
    print(hdr)
    print("  " + "-" * (len(hdr) - 2))
    for f in falsifiers:
        val = f.get("value")
        val_s = "-" if val is None else ("%g" % val)
        unit = f.get("unit") or ""
        if val is not None and unit not in ("count", "bool", ""):
            val_s = "%s %s" % (val_s, unit)
        cond = "%s %g" % (f.get("op") or "?", f.get("threshold") or 0)
        print("  %-1s %-32s %-9s %12s %-14s %6s%s" % (
            "✓" if f.get("holds") else "✗",
            f.get("name") or "?", f.get("estimator") or "?", val_s, cond,
            f.get("n") if f.get("n") is not None else "?",
            ("  — " + f["note"]) if f.get("note") else ""))
    if v.get("thresholds_version"):
        print("  thresholds: %s" % v["thresholds_version"])

if os.environ.get("NOTE"):
    print("NOTE: %s" % os.environ["NOTE"])
PY
  fi

  if [ -n "$NOTE" ]; then
    result "$STATUS" "verdict=$VERDICT" "reason=unrecognised-verdict"
  fi
  result "$STATUS" "verdict=$VERDICT"
  ;;

metrics)
  need_sid
  api_or_die "/v1/admin/sessions/$SID/metrics?limit=200" \
    "GET /v1/admin/sessions/$SID/metrics"
  FLOOR="$(since_ms "$SINCE")"
  COUNT_FILE="$(mktemp)"
  JSON="$JSON" FLOOR="$FLOOR" ROWS="$ROWS" SIDV="$SID" HOSTV="$DX_HOST" \
    ROOTV="$DX_ROOT" COUNT_FILE="$COUNT_FILE" python3 - "$RESP_FILE" <<'PY'
import datetime, json, os, sys

body = json.load(open(sys.argv[1]))
items = body.get("items") or []
floor = int(os.environ.get("FLOOR") or 0)
if floor:
    items = [i for i in items if (i.get("ts_unix_ms") or 0) >= floor]
items.sort(key=lambda i: i.get("ts_unix_ms") or 0)
rows = int(os.environ.get("ROWS") or 20)
if rows > 0:
    items = items[-rows:]

if os.environ.get("JSON") == "1":
    print(json.dumps({"status": "ok", "target": "session-metrics",
                      "sid": os.environ["SIDV"], "host": os.environ["HOSTV"],
                      "samples": items}, indent=2, default=str))
else:
    def g(m, *names):
        for n in names:
            v = m.get(n)
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                return round(float(v), 2)
        return None

    def f(v):
        return "-" if v is None else ("%g" % v)

    def pct(m):
        # present_beat_fraction is a 0..1 share; a table reads better as a
        # percentage, and "-" when the client predates the key.
        v = g(m, "present_beat_fraction")
        return "-" if v is None else ("%g%%" % round(v * 100, 1))

    # PRES_FPS_MED + BEAT replace the old PRES_SD column. present_fps is fps
    # from the MEAN interval and misreads a source-fps == display-Hz session by
    # 10-25% (2026-08-22); the median does not, and BEAT says how much of the
    # window is the inherent vsync beat. SD is still in --json.
    # Column headers carry the UNIT from the metric manifest
    # (docs/session-trace/metrics.json), not a hand-typed guess: the manifest is
    # the one place that says what each key is measured in, and a table that
    # states a unit it did not read is a table that can be wrong.
    UNITS = {}
    try:
        import pathlib
        _root = pathlib.Path(os.environ["ROOTV"])
        _man = json.loads((_root / "docs/session-trace/metrics.json").read_text())
        for _e in _man["metrics"]:
            UNITS.setdefault(_e["key"], _e["unit"])
    except Exception:
        pass  # the manifest is a nicety here; a missing one must not break the verb

    def hdr(label, key):
        # Suffix the unit unless the column name already carries it (FPS, and
        # PRES_FPS_MED, would otherwise read "FPS(fps)").
        u = UNITS.get(key)
        if not u or u.lower() in label.lower():
            return label
        return "%s(%s)" % (label, u)

    COLS = "%-8s %-7s %7s %11s %11s %14s %12s %6s"
    print(COLS % (
        "TIME", "SOURCE", hdr("FPS", "fps"), hdr("ENC_P95", "encode_ms_p95"),
        hdr("KBPS", "bitrate_kbps"), hdr("SETPOINT", "abr_setpoint_kbps"),
        hdr("PRES_FPS_MED", "present_fps_median"), "BEAT"))
    for i in items:
        m = i.get("metrics") or {}
        ts = i.get("ts_unix_ms") or 0
        hhmmss = (datetime.datetime.fromtimestamp(
            ts / 1000, datetime.timezone.utc).strftime("%H:%M:%S") if ts else "?")
        print(COLS % (
            hhmmss, (i.get("source") or "?")[:7],
            f(g(m, "fps", "present_fps")),
            f(g(m, "encode_ms_p95", "encode_ms")),
            f(g(m, "bitrate_kbps")),
            f(g(m, "abr_setpoint_kbps")),
            f(g(m, "present_fps_median")),
            pct(m)))
    if not items:
        print("(no samples in range — a session that just launched has none yet)")
open(os.environ["COUNT_FILE"], "w").write(str(len(items)))

PY
  SHOWN="$(cat "$COUNT_FILE" 2>/dev/null || echo 0)"; rm -f "$COUNT_FILE"
  result ok "samples=$SHOWN"
  ;;

trace)
  need_sid
  QS="$(window_qs)"
  api_or_die "/v1/admin/sessions/$SID/trace/window$QS" \
    "GET /v1/admin/sessions/$SID/trace/window"
  cp "$RESP_FILE" "$RESP_FILE.window"
  api_or_die "/v1/admin/sessions/$SID/trace/events$QS" \
    "GET /v1/admin/sessions/$SID/trace/events"
  COUNT_FILE="$(mktemp)"
  JSON="$JSON" SIDV="$SID" HOSTV="$DX_HOST" COUNT_FILE="$COUNT_FILE" \
    python3 - "$RESP_FILE.window" "$RESP_FILE" <<'PY'
import json, os, sys
win = json.load(open(sys.argv[1]))
evs = json.load(open(sys.argv[2])).get("events") or []
w = win.get("window") or {}
if os.environ.get("JSON") == "1":
    print(json.dumps({"status": "ok", "target": "session-trace",
                      "sid": os.environ["SIDV"], "host": os.environ["HOSTV"],
                      "window": w,
                      "series_names": sorted((win.get("series") or {}).keys()),
                      "events": evs}, indent=2, default=str))
else:
    span = (int(w.get("to_ms") or 0) - int(w.get("from_ms") or 0)) / 1000.0
    print("window: %.0fs  (%s -> %s)" % (span, w.get("from_ms"), w.get("to_ms")))
    names = sorted((win.get("series") or {}).keys())
    print("series: %d  (%s%s)" % (len(names), ", ".join(names[:6]),
                                  ", ..." if len(names) > 6 else ""))
    print()
    if not evs:
        print("(no trace events in this window)")
    else:
        # The salient key FIRST, then the rest. A raw dump truncated at 90 chars
        # dropped exactly the fields a reader is scanning for — `reason` on an
        # encoder.stall, `rejected_count` on an sdp.answer_applied, `code` on a
        # host.xid — because they sit at the tail of a long payload.
        LEAD = ("reason", "rejected_count", "code", "trigger", "phase", "profile",
                "kind", "state", "vendor")
        print("%-15s %-8s %-28s %s" % ("TS_UNIX_MS", "SOURCE", "TYPE", "PAYLOAD"))
        for e in evs:
            pl = e.get("payload")
            body = ""
            if isinstance(pl, dict):
                head = ["%s=%s" % (k, json.dumps(pl[k], default=str))
                        for k in LEAD if k in pl]
                rest = {k: v for k, v in pl.items() if k not in LEAD}
                body = " ".join(head)
                if rest:
                    body = (body + " " if body else "") + json.dumps(
                        rest, separators=(",", ":"), default=str)
            elif pl is not None:
                body = json.dumps(pl, separators=(",", ":"), default=str)
            if len(body) > 90:
                body = body[:87] + "..."
            print("%-15s %-8s %-28s %s" % (
                e.get("ts_unix_ms"), (e.get("source") or "?")[:8],
                (e.get("type") or "?")[:28], body))
open(os.environ["COUNT_FILE"], "w").write(str(len(evs)))

PY
  EVENTS="$(cat "$COUNT_FILE" 2>/dev/null || echo 0)"; rm -f "$COUNT_FILE" "$RESP_FILE.window"
  result ok "events=$EVENTS"
  ;;

capture)
  need_sid
  [ -n "$KIND" ] || usage_out "capture needs KIND=pipeline_dot|encoder_props|burst_stats|all"
  case "$KIND" in
    pipeline_dot|encoder_props|burst_stats|all) ;;
    *) usage_out "KIND must be pipeline_dot|encoder_props|burst_stats|all (got '$KIND')" ;;
  esac

  CAP_DIR="${OUT_PATH:-$DX_DIAG_DIR}"
  mkdir -p "$CAP_DIR"
  chmod 700 "$CAP_DIR" 2>/dev/null || true

  # The result arrives on the trace lane, not in the POST's response: the 202
  # means ARMED. This is the poll deadline — the agent's own wall-clock budget
  # (10 s) plus 5 s of slack for the WS hop and the synchronous insert. A knob
  # only because the self-suite exercises the timeout path and should not spend
  # 15 s doing it.
  CAP_DEADLINE_S="${CAPTURE_TIMEOUT_S:-15}"
  CAP_SUMMARY=""
  CAP_N=0
  CAP_LAST_ID=""

  do_capture() { # do_capture <kind> ; writes a file, appends to $CAP_SUMMARY
    local kind="$1" body params cap_id waited out_path
    params=""
    if [ "$kind" = burst_stats ] && { [ -n "$WINDOWS" ] || [ -n "$WINDOW_MS" ]; }; then
      params=",\"params\":{\"windows\":${WINDOWS:-20},\"window_ms\":${WINDOW_MS:-250}}"
    fi
    body="{\"kind\":\"$kind\"$params}"

    api_post "/v1/admin/sessions/$SID/capture" "$body"
    case "$API_CODE" in
      202) : ;;
      401|403) err_out unauthorized \
        "token rejected (HTTP $API_CODE) on POST /v1/admin/sessions/$SID/capture. Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh" ;;
      404) err_out not-found \
        "404 on POST .../capture — no such session on host=$DX_HOST. Next: make session-list HOST=$DX_HOST" ;;
      409) err_out capture-busy \
        "the host refused the capture (HTTP 409): a capture is already in flight for this session, or it is not running. Captures are single-flight — they are never queued. Next: wait a few seconds and re-run, or make session-list HOST=$DX_HOST to check the state." ;;
      422) err_out capture-kind \
        "the agent will not capture kind='$kind' on this session (HTTP 422) — it does not know the kind, or cannot do it here yet. Next: make session-capture SID=$SID HOST=$DX_HOST KIND=pipeline_dot" ;;
      501) err_out capture-unsupported \
        "this host's node-agent predates on-demand captures (HTTP 501) — it never acked, so retrying will not help. Next: make rebuild HOST=$DX_HOST" ;;
      503) err_out agent-not-connected \
        "the session's host has no live agent connection (HTTP 503). Next: make status HOST=$DX_HOST" ;;
      000) err_out unreachable \
        "no response from $API_BASE — the stack is down or unreachable. Next: make status HOST=$DX_HOST" ;;
      *) err_out "http-$API_CODE" "HTTP $API_CODE on POST .../capture. Next: make status HOST=$DX_HOST" ;;
    esac

    cap_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("capture_id") or "")' "$RESP_FILE")"
    [ -n "$cap_id" ] || err_out no-capture-id \
      "the control plane accepted the capture but returned no capture_id. Next: make session-bundle SID=$SID HOST=$DX_HOST and read captures[] directly."
    printf 'armed %s: capture_id=%s\n' "$kind" "$cap_id" >&2

    # 404 while it is in flight is the POLL SIGNAL, not an error.
    waited=0
    while :; do
      api_get "/v1/admin/sessions/$SID/captures/$cap_id"
      case "$API_CODE" in
        200) break ;;
        404) : ;;
        401|403) err_out unauthorized \
          "token rejected (HTTP $API_CODE) polling the capture. Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh" ;;
        000) err_out unreachable \
          "no response from $API_BASE while polling. Next: make status HOST=$DX_HOST" ;;
        *) err_out "http-$API_CODE" "HTTP $API_CODE polling the capture. Next: make status HOST=$DX_HOST" ;;
      esac
      waited=$(( waited + 1 ))
      if [ "$waited" -gt $(( CAP_DEADLINE_S * 2 )) ]; then
        err_out capture-timeout \
          "capture $cap_id (kind=$kind) never reported within ${CAP_DEADLINE_S}s. The agent acked, so it armed — the result was lost or the agent is wedged. Next: make session-logs SID=$SID HOST=$DX_HOST GREP=capture"
      fi
      sleep 0.5
    done

    # Decode where the bytes are, not where a filename suggests: the payload is
    # gzip+base64 for text kinds and inline JSON for small ones.
    out_path="$(CAP_DIR="$CAP_DIR" SIDV="$SID" KINDV="$kind" CAPID="$cap_id" \
      TS="$(dx_timestamp)" python3 - "$RESP_FILE" <<'PY'
import base64, gzip, json, os, sys

c = json.load(open(sys.argv[1]))
enc = c.get("encoding")
ctype = c.get("content_type") or ""
ext = ".dot" if "graphviz" in ctype else ".json"
if enc == "gzip+base64":
    blob = gzip.decompress(base64.b64decode(c.get("data") or ""))
elif enc == "json":
    blob = json.dumps(c.get("json"), indent=2, default=str).encode()
    ext = ".json"
else:
    # An encoding this tool has never seen is DATA, not a failure: write the
    # whole event out verbatim so nothing an agent reported is lost.
    blob = json.dumps(c, indent=2, default=str).encode()
    ext = ".json"

path = os.path.join(os.environ["CAP_DIR"],
                    "capture-%s-%s-%s%s" % (os.environ["SIDV"], os.environ["KINDV"],
                                            os.environ["TS"], ext))
with open(path, "wb") as fh:
    fh.write(blob)
os.chmod(path, 0o600)
sys.stderr.write("bytes=%s truncated=%s duration_ms=%s%s\n" % (
    c.get("bytes"), str(bool(c.get("truncated"))).lower(), c.get("duration_ms"),
    ("  error=%s" % c["error"]) if c.get("error") else ""))
print(path)
PY
)" || err_out decode-failed \
      "could not decode capture $cap_id. Next: make session-bundle SID=$SID HOST=$DX_HOST and read captures[] out of the JSON."

    printf '%s\n' "$out_path"
    chmod 600 "$out_path" 2>/dev/null || true

    # Graphviz if it is here; the .dot is the artifact either way.
    case "$out_path" in
      *.dot)
        if dx_have dot; then
          if dot -Tsvg "$out_path" -o "${out_path%.dot}.svg" 2>/dev/null; then
            chmod 600 "${out_path%.dot}.svg" 2>/dev/null || true
            printf '%s\n' "${out_path%.dot}.svg"
          fi
        else
          printf 'note: graphviz `dot` is not on PATH, so no .svg was rendered. Next: brew install graphviz\n' >&2
        fi
        ;;
    esac

    CAP_N=$(( CAP_N + 1 ))
    CAP_LAST_ID="$cap_id"
    CAP_SUMMARY="$(CAPS="$CAP_SUMMARY" python3 - "$RESP_FILE" "$kind" <<'PY'
import json, os, sys
c = json.load(open(sys.argv[1]))
prev = os.environ.get("CAPS") or ""
one = "%s:bytes=%s,truncated=%s,duration_ms=%s" % (
    sys.argv[2], c.get("bytes"), str(bool(c.get("truncated"))).lower(), c.get("duration_ms"))
print((prev + " " + one).strip())
PY
)"
  }

  if [ "$KIND" = all ]; then
    # Sequential, deliberately: captures are single-flight per session, so a
    # parallel fan-out would 409 two of its own three requests.
    do_capture pipeline_dot
    do_capture encoder_props
    do_capture burst_stats
    result ok "kinds=3" "captures=$CAP_SUMMARY"
  else
    do_capture "$KIND"
    result ok "capture_id=$CAP_LAST_ID" "$CAP_SUMMARY"
  fi
  ;;

logs)
  need_sid
  [ "$DX_HOST" != "local" ] || err_out local-unsupported \
    "session-logs reads containers on a GPU host; the local stack has no node-agent. Next: make session-logs SID=$SID HOST=<role>"
  SINCE_ARG="${SINCE:-10m}"
  # Also spliced into the remote snippet below, as SINCE='<value>'.
  dx_require_safe "$VERB" "SINCE" "$SINCE_ARG" "$DX_RE_SINCE" \
    "SINCE is a docker --since value: a duration (10m, 2h30m) or an RFC3339 timestamp."
  # The container names are the agent's: node-agent/src/session/container.rs
  # SESSION_NAME_PREFIX (+ the -g<N> generation override in run()) and
  # audio.rs PULSE_NAME_PREFIX.
  # The EGL/GL/smithay filter is the same one qses uses — without it the agent's
  # renderer chatter buries every line you came for.
  # C9 log spans: the session runner thread stamps `session{id=<sid> host=<node>}`
  # on every line it emits (text format), or nests the id under a `session` span
  # object when QUASAR_LOG_FORMAT=json; the agent's own control loop is outside
  # that span and instead carries `session_id=<sid>` or names the id in prose.
  # One SID filter has to catch all of them — hence the alternation, with a bare
  # -id arm last so a prose mention is never silently dropped. Kept on one line
  # and asserted by scripts/dx/tests/run.sh (session:logs-sid-filter).
  # shellcheck disable=SC2016
  SNIP='SID='"$SID"'
SID_FILTER_RE="session\{id=$SID|session_id=\"?$SID|\"id\":\"$SID\"|$SID"
SINCE='"$SINCE_ARG"'
# The app container carries a per-generation suffix (quasar-sess-<sid>-g<N>):
# a swap runs the old and new generation side by side, so match by PREFIX and
# show every generation. The pulse sidecar has no generation.
FOUND=0
for c in $(docker ps -a --format "{{.Names}}" | grep -E "^quasar-(sess|pulse)-$SID(-g[0-9]+)?$" | sort); do
  FOUND=1
  echo "===== $c ====="
  docker logs --since "$SINCE" "$c" 2>&1 | tail -n 400
done
[ "$FOUND" = 1 ] || echo "===== quasar-sess-$SID-g* / quasar-pulse-$SID (no such containers) ====="
AGENT=$(docker ps --filter name=node-agent --format "{{.Names}}" | head -n 1)
if [ -n "$AGENT" ]; then
  echo "===== $AGENT (filtered to $SID) ====="
  docker logs --since "$SINCE" "$AGENT" 2>&1 \
    | grep -avE "EGL|GL_|smithay|egl_context|renderer_gles|pci.ids|Supported" \
    | grep -aE "$SID_FILTER_RE" | tail -n 400
else
  echo "===== node-agent (no running container found) ====="
fi'
  if [ -n "$GREP_PAT" ]; then
    dx_ssh_remote "$SNIP" | grep -aE "$GREP_PAT|^===== " || true
  else
    dx_ssh_remote "$SNIP" || err_out ssh-failed \
      "ssh to $DX_REMOTE_NAME failed. Next: make status HOST=$DX_HOST (a host with a Dozzle MCP endpoint reads container logs without an ssh hop)."
  fi
  printf 'note: where the target host exposes a Dozzle MCP endpoint (recorded in _shared/hosts.json) it reads these same logs without an ssh hop.\n' >&2
  result ok "since=$SINCE_ARG"
  ;;

esac

[ "$RESULT_EMITTED" = 1 ] || result failed "reason=no-path-taken"
