#!/usr/bin/env bash
# scripts/harness/run-soak-profile.sh — PROF-03 soak-and-diff leak detector.
#
# Cycles session launch -> stream -> teardown N times against a LIVE Quasar
# stack, sampling a fixed metric set after every teardown (control-plane
# runtime/pool via the PROF-01 debug listener, RSS/fd/thread counts via
# /proc, DB row counts, node-agent RSS/threads/fds/VRAM). Every cycle is
# appended to a CSV as it completes, then scripts/harness/lib/soak_report.py turns the
# CSV into leak/plateau/flat verdicts + an HTML report.
#
# WHY CYCLING BEATS UPTIME: a single long-lived session's RSS can be flat
# while every NEW session leaks a fixed amount (a per-session ref cycle, a
# per-session goroutine that never exits, ...) — that only shows up as a
# staircase across many launch/teardown cycles, never within one session's
# lifetime. See docs/profiling-soak.md for the full rationale and operator
# procedure.
#
# Runs ON THE STACK HOST (Tower or hermes) with docker + psql + curl + wget
# reachable and python3 on PATH — it drives docker exec / curl against the
# live compose stack directly. It does not ssh anywhere itself.
#
# Usage:
#   bash scripts/harness/run-soak-profile.sh --app 'Steam' [options...]
#   bash scripts/harness/run-soak-profile.sh --report-only deploy/results/soak-.../soak.csv
#
# See docs/profiling-soak.md for the full CLI reference, CSV schema, and
# verdict vocabulary.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/harness/lib/harness.sh
source "$ROOT/scripts/harness/lib/harness.sh"

# ── Defaults (every one of these is a CLI knob — no hidden constants) ──────
DURATION="2h"
CYCLES=""                    # empty = unlimited (duration governs)
HOLD=90
SETTLE=10
APP=""
PROFILE=""
# Optional per-session codec override (h264|h265|av1) — the POST /v1/sessions
# diagnostic override, added for D-5 (#395) per-codec soaks. Empty = server-side
# auto resolution (h264 floor on a headless launch, which has no decode probe).
CODEC=""
API="https://localhost:18443"
CP_CONTAINER="deploy-quasar-control-plane-1"
AGENT_CONTAINER="deploy-quasar-node-agent-1"
PG_CONTAINER="deploy-quasar-postgres-1"
OUT_DIR=""
GST_LEAKS=0
MAX_CONSECUTIVE_FAILURES=5
# Default auth is a throwaway identity minted via the dev-gated
# POST /v1/dev/agent-session (#399) — requires QUASAR_DEV_AGENT_AUTH=1 on the
# stack. --email/--pass switch to an explicit register+login instead. There is
# deliberately NO committed default credential anymore.
EMAIL=""
PASS_=""
USERNAME=""
REPORT_ONLY_CSV=""
# --launch-timeout / --teardown-timeout (finding #15): promoted from the
# QSES_LAUNCH_TIMEOUT / QSES_TEARDOWN_TIMEOUT env vars to real flags; the env
# vars remain a fallback default so nothing already using them breaks.
LAUNCH_TIMEOUT="${QSES_LAUNCH_TIMEOUT:-90}"
TEARDOWN_TIMEOUT="${QSES_TEARDOWN_TIMEOUT:-60}"

usage() {
  sed -n '1,40p' "$0" | sed 's/^# \{0,1\}//'
}

# ── Arg parsing ──────────────────────────────────────────────────────────
while [ $# -gt 0 ]; do
  case "$1" in
    --duration) DURATION="$2"; shift 2 ;;
    --cycles) CYCLES="$2"; shift 2 ;;
    --hold) HOLD="$2"; shift 2 ;;
    --settle) SETTLE="$2"; shift 2 ;;
    --app) APP="$2"; shift 2 ;;
    --profile) PROFILE="$2"; shift 2 ;;
    --codec) CODEC="$2"; shift 2 ;;
    --api) API="$2"; shift 2 ;;
    --cp) CP_CONTAINER="$2"; shift 2 ;;
    --agent) AGENT_CONTAINER="$2"; shift 2 ;;
    --pg) PG_CONTAINER="$2"; shift 2 ;;
    --out) OUT_DIR="$2"; shift 2 ;;
    --gst-leaks) GST_LEAKS=1; shift ;;
    --max-consecutive-failures) MAX_CONSECUTIVE_FAILURES="$2"; shift 2 ;;
    --email) EMAIL="$2"; shift 2 ;;
    --pass) PASS_="$2"; shift 2 ;;
    --user) USERNAME="$2"; shift 2 ;;
    --launch-timeout) LAUNCH_TIMEOUT="$2"; shift 2 ;;
    --teardown-timeout) TEARDOWN_TIMEOUT="$2"; shift 2 ;;
    --report-only) REPORT_ONLY_CSV="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

# ── report-only shortcut: no soaking, just run the reporter ───────────────
if [ -n "$REPORT_ONLY_CSV" ]; then
  harness_init "soak-profile-report-only"
  require python3
  if [ ! -f "$REPORT_ONLY_CSV" ]; then
    fail "csv not found: $REPORT_ONLY_CSV"
    harness_report
  fi
  pass "csv found: $REPORT_ONLY_CSV"
  harness_note "csv" "$REPORT_ONLY_CSV"
  if python3 "$ROOT/scripts/harness/lib/soak_report.py" "$REPORT_ONLY_CSV"; then
    pass "report generated"
  else
    fail "soak_report.py exited nonzero"
  fi
  harness_report
fi

[ -n "$APP" ] || { echo "--app is required (e.g. --app Steam — Tower's known-good app)" >&2; exit 2; }

if [ -z "$OUT_DIR" ]; then
  OUT_DIR="$ROOT/deploy/results/soak-$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$OUT_DIR"
CSV_PATH="$OUT_DIR/soak.csv"
PARAMS_PATH="$OUT_DIR/params.json"

# ── duration parsing: SECS | Nh | Nm ───────────────────────────────────────
parse_duration_secs() {
  local d="$1"
  case "$d" in
    *h) echo $(( ${d%h} * 3600 )) ;;
    *m) echo $(( ${d%m} * 60 )) ;;
    ''|*[!0-9]*) echo "invalid --duration: $d (want SECS, Nh, or Nm)" >&2; return 1 ;;
    *) echo "$d" ;;
  esac
}
DURATION_SECS="$(parse_duration_secs "$DURATION")" || exit 2

harness_init "soak-profile"
require curl docker python3 date

# HAVE_TIMEOUT (finding #5): checked once at startup, never per-cycle. When
# absent, docker-exec sampling calls run unwrapped (best effort) rather than
# failing the whole run over a missing coreutils package.
if command -v timeout >/dev/null 2>&1; then
  HAVE_TIMEOUT=1
else
  HAVE_TIMEOUT=0
  echo "WARNING: coreutils 'timeout' not found on PATH — docker exec sampling calls will run unwrapped (no 15s cap); a wedged docker daemon can still hang this run" >&2
fi

# t15 <cmd...> — run a command under `timeout 15`, or bare if unavailable.
t15() {
  if [ "$HAVE_TIMEOUT" = 1 ]; then
    timeout 15 "$@"
  else
    "$@"
  fi
}

START_EPOCH=$(date -u +%s)
START_ISO=$(date -u +%Y-%m-%dT%H:%M:%SZ)
END_EPOCH=$(( START_EPOCH + DURATION_SECS ))

harness_note "app" "$APP"
harness_note "profile" "${PROFILE:-<none>}"
harness_note "codec" "${CODEC:-<auto>}"
harness_note "duration" "$DURATION"
harness_note "hold" "$HOLD"
harness_note "settle" "$SETTLE"
harness_note "api" "$API"
harness_note "out_dir" "$OUT_DIR"
harness_note "csv" "$CSV_PATH"
harness_note "gst_leaks" "$GST_LEAKS"
harness_note "launch_timeout" "$LAUNCH_TIMEOUT"
harness_note "teardown_timeout" "$TEARDOWN_TIMEOUT"

# ── state used by cleanup ───────────────────────────────────────────────
TOK=""
CUR_SID=""
CYCLE_COUNT=0
CSV_HEADER_WRITTEN=0
ABORTED_REASON=""
AGENT_TRACERS_ARMED=0

# agent_uptime_s (issue #420) is appended at the END of the column list —
# never inserted mid-schema — so report-only mode over an OLD CSV (written
# before this column existed) still parses cleanly: soak_report.py reads by
# header name, and a missing trailing column just means the field is absent
# from that CSV's fieldnames, not a width mismatch.
CSV_COLUMNS="cycle,ts_iso,cycle_ok,session_id,launch_to_running_s,cp_goroutines,cp_heap_inuse_bytes,cp_heap_objects,cp_heap_alloc_bytes,cp_sys_bytes,cp_num_gc,cp_rss_kb,cp_fds,cp_uptime_s,pool_acquired,pool_idle,pool_total,pool_empty_acquire_count,db_sessions,db_session_metrics,db_trace_events,db_auth_tokens,db_session_tokens,db_admin_activity,agent_rss_kb,agent_threads,agent_fds,vram_used_mb,gst_alive,agent_uptime_s"
CSV_FIELD_COUNT=$(awk -F, '{print NF}' <<<"$CSV_COLUMNS")

ensure_csv_header() {
  if [ "$CSV_HEADER_WRITTEN" = 0 ]; then
    echo "$CSV_COLUMNS" > "$CSV_PATH"
    CSV_HEADER_WRITTEN=1
  fi
}

# join_csv_row <field...> — commas between args, no trailing separator issues
# (an empty arg still yields an empty field, never dropped, which is how a
# "missing" sample shows up as an empty cell rather than a shifted column).
join_csv_row() {
  local IFS=,
  echo "$*"
}

# write_csv_row <cycle> <ts> <cycle_ok> <session_id> <launch_to_running_s> <sample_csv>
# Assembles the full row and structurally guards against the column-count
# bug class from finding #1 (a sample fragment emitting the wrong number of
# fields silently shifts every later column). On a field-count mismatch:
# log an error and write an all-empty-sample row of the CORRECT width
# instead, preserving cycle/ts/cycle_ok (forced to 0) — never append the
# malformed row itself.
write_csv_row() {
  local cycle="$1" ts="$2" cycle_ok="$3" sid="$4" l2r="$5" sample="$6"
  local row="${cycle},${ts},${cycle_ok},${sid},${l2r},${sample}"
  local field_count
  field_count=$(awk -F, '{print NF}' <<<"$row")
  if [ "$field_count" != "$CSV_FIELD_COUNT" ]; then
    echo "ERROR: cycle $cycle: assembled CSV row has $field_count field(s), expected $CSV_FIELD_COUNT — writing an empty-sample row (cycle_ok=0) instead to preserve column alignment" >&2
    local n_rest=$((CSV_FIELD_COUNT - 3))
    local rest="" i
    for ((i = 0; i < n_rest; i++)); do
      rest="${rest},"
    done
    row="${cycle},${ts},0${rest}"
  fi
  echo "$row" >> "$CSV_PATH"
}

# ── login / API helpers ─────────────────────────────────────────────────
# Every curl call in this script carries --max-time 20 --connect-timeout 5
# (finding #5) — a wedged control plane must never hang an 8h run.
login() {
  if [ -n "$EMAIL" ]; then
    # --user unset: derive a username from the email local part (registration
    # requires one; no committed default identity exists anymore).
    local reg_user="${USERNAME:-${EMAIL%%@*}}"
    curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/auth/register" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS_\",\"username\":\"$reg_user\"}" >/dev/null 2>&1 || true
    TOK=$(curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/auth/login" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS_\"}" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null) || TOK=""
    return
  fi
  # Default: mint a throwaway auto-reaped identity (#399). Key from the env or
  # the CP container; a 401 mid-run re-enters here and mints a fresh identity,
  # so the 8h TTL cap can never strand a long soak.
  local key="${QUASAR_DEV_AGENT_KEY:-}"
  if [ -z "$key" ]; then
    key=$(docker exec "$CP_CONTAINER" cat /run/quasar/dev-agent-key 2>/dev/null) || key=""
  fi
  if [ -z "$key" ]; then
    echo "FATAL: no dev-agent key — set QUASAR_DEV_AGENT_AUTH=1 on the stack (key lands in the CP log and /run/quasar/dev-agent-key), export QUASAR_DEV_AGENT_KEY, or pass --email/--pass" >&2
    TOK=""
    return
  fi
  # The dev key goes to curl via a header FILE, never argv — a literal
  # -H "X-Quasar-Dev-Key: $key" is visible to any local user via `ps`.
  # Global (not local) so cleanup() can remove it if we die mid-mint.
  DEVKEY_HDR_FILE=$(mktemp "${TMPDIR:-/tmp}/soak-devkey-hdr.XXXXXX")
  local hdrfile="$DEVKEY_HDR_FILE"
  chmod 600 "$hdrfile"
  printf 'X-Quasar-Dev-Key: %s\n' "$key" > "$hdrfile"
  TOK=$(curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/dev/agent-session" \
    -H @"$hdrfile" -H 'Content-Type: application/json' \
    -d '{"role":"user","ttl_seconds":28800}' \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null) || TOK=""
  rm -f "$hdrfile"
}

# api_curl <METHOD> <path> [extra curl args...] — the ONE place every authed
# call goes through (finding #10). Retries exactly once after a re-login on
# a 401 (24h token expiry or an invalid token). Prints the response body.
#
# The HTTP status travels via a FILE, not a variable: every caller captures
# the body with `body=$(api_curl ...)`, which runs api_curl in a SUBSHELL, so
# a variable assigned inside it never reaches the caller. That exact bug made
# get_app_id compare an empty status against 200 and report "app not found"
# on a perfect 200 response (caught live on Tower, 2026-07-31 — both review
# passes missed it because it only bites through the $() boundary). Read the
# status with api_code, never the variable.
API_CURL_CODE=""
API_CURL_CODE_FILE="$(mktemp "${TMPDIR:-/tmp}/soak-api-code.XXXXXX")"
api_code() { cat "$API_CURL_CODE_FILE" 2>/dev/null; }

# Same subshell-loss class, second instance (both caught live on Tower):
# launch_session is invoked as L2R=$(launch_session ...), so a CUR_SID assigned
# inside it dies with the subshell — the caller then saw SID="" every cycle,
# session_id was blank in the CSV, and teardown_session "" DELETEd /v1/sessions/
# (404), which the poll "confirmed" as terminal — so the harness NEVER tore a
# session down itself; every cycle's session survived until the next cycle's 409
# home_in_use handler deleted it, and the LAST cycle's session simply leaked.
# The session id therefore travels by file too.
LAUNCH_SID_FILE="$(mktemp "${TMPDIR:-/tmp}/soak-launch-sid.XXXXXX")"
launch_sid() { cat "$LAUNCH_SID_FILE" 2>/dev/null; }
api_curl() {
  local method="$1" path="$2" resp code body
  shift 2
  resp=$(curl -k -s --max-time 20 --connect-timeout 5 -w '\n%{http_code}' -X "$method" "$API$path" \
    -H "Authorization: Bearer $TOK" "$@")
  code=$(echo "$resp" | tail -1); body=$(echo "$resp" | sed '$d')
  if [ "$code" = "401" ]; then
    login
    resp=$(curl -k -s --max-time 20 --connect-timeout 5 -w '\n%{http_code}' -X "$method" "$API$path" \
      -H "Authorization: Bearer $TOK" "$@")
    code=$(echo "$resp" | tail -1); body=$(echo "$resp" | sed '$d')
  fi
  API_CURL_CODE="$code"
  printf '%s' "$code" > "$API_CURL_CODE_FILE"
  echo "$body"
}

get_app_id() {
  local body
  body=$(api_curl GET "/v1/apps")
  [ "$(api_code)" = "200" ] || { echo ""; return 1; }
  echo "$body" | QSES_APP="$APP" python3 -c '
import os, sys, json
name = os.environ["QSES_APP"].lower()
d = json.load(sys.stdin)
items = d.get("apps", d.get("items", []))
for a in items:
    if name in a["name"].lower():
        print(a["id"]); break
'
}

# launch_session <app_id> -> sets CUR_SID, prints launch_to_running_s or empty
# on failure (writes nothing to stdout on hard failure; caller checks $?).
launch_session() {
  local app_id="$1" launch_json body sid t0 t1 st tries
  : > "$LAUNCH_SID_FILE"
  launch_json="{\"app_id\":\"$app_id\""
  [ -n "$PROFILE" ] && launch_json="$launch_json,\"profile_id\":\"$PROFILE\""
  [ -n "$CODEC" ] && launch_json="$launch_json,\"stream\":{\"codec\":\"$CODEC\"}"
  launch_json="$launch_json}"

  body=$(api_curl POST "/v1/sessions" -H 'Content-Type: application/json' -d "$launch_json")

  if [ "$(api_code)" = "409" ]; then
    # home_in_use: a prior session still holds storage — find + delete the
    # blocking session id (field session_id in the error JSON) and treat
    # this cycle as a failure; the NEXT cycle gets a clean home.
    local blocker
    blocker=$(echo "$body" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); print(d.get("session_id","") or d.get("error",{}).get("session_id",""))
except Exception:
    print("")' 2>/dev/null) || blocker=""
    if [ -n "$blocker" ]; then
      api_curl DELETE "/v1/sessions/$blocker" >/dev/null || true
    fi
    echo "409 home_in_use (blocker=${blocker:-none})" >&2
    return 1
  fi

  if [ "$(api_code)" != "201" ]; then
    echo "launch failed: HTTP $(api_code)" >&2
    return 1
  fi

  sid=$(echo "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["session"]["id"])' 2>/dev/null) || sid=""
  [ -n "$sid" ] || { echo "launch: no session id in 201 response" >&2; return 1; }
  CUR_SID="$sid"                          # subshell-local; real handoff is the file
  printf '%s' "$sid" > "$LAUNCH_SID_FILE"

  t0=$(date -u +%s)
  st=""
  tries=$(( (LAUNCH_TIMEOUT + 1) / 2 ))
  for _ in $(seq 1 "$tries"); do
    sleep 2
    body=$(api_curl GET "/v1/sessions/$CUR_SID")
    [ "$(api_code)" = "200" ] || continue
    st=$(echo "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["session"]["state"])' 2>/dev/null) || st=""
    [ "$st" = "running" ] && break
    [ "$st" = "failed" ] && break
  done
  t1=$(date -u +%s)

  if [ "$st" != "running" ]; then
    echo "session did not reach running (state=${st:-unknown})" >&2
    return 1
  fi
  echo "$(( t1 - t0 ))"
  return 0
}

# teardown_session <sid> — returns 0 only when a terminal state was CONFIRMED
# (404, or state stopped/failed); returns 1 if the poll loop gave up without
# confirming (finding #4 — the caller must not clear CUR_SID on a 1 return,
# so the EXIT trap keeps retrying).
teardown_session() {
  local sid="$1" tries st body
  # An empty sid is a caller bug, and DELETE /v1/sessions/ answering 404 must
  # NOT read as "confirmed terminal" — that is exactly how the subshell-lost
  # sid masqueraded as clean teardown while every session leaked.
  [ -n "$sid" ] || { echo "teardown_session called with empty sid" >&2; return 1; }
  api_curl DELETE "/v1/sessions/$sid" >/dev/null || true
  tries=$(( (TEARDOWN_TIMEOUT + 1) / 2 ))
  for _ in $(seq 1 "$tries"); do
    sleep 2
    body=$(api_curl GET "/v1/sessions/$sid")
    [ "$(api_code)" = "404" ] && return 0
    [ "$(api_code)" = "200" ] || continue
    st=$(echo "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["session"]["state"])' 2>/dev/null) || st=""
    case "$st" in stopped|failed) return 0 ;; esac
  done
  return 1
}

# ── sampling helpers — every one of these must yield an EMPTY string on
# failure, never "0" (a fabricated zero would look like a real cliff to the
# report's classifier). ─────────────────────────────────────────────────

sample_cp_runtime() {
  local json
  json=$(t15 docker exec "$CP_CONTAINER" wget -qO- http://127.0.0.1:6060/debug/quasar/runtime 2>/dev/null) || { echo ",,,,,"; return; }
  echo "$json" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(",,,,,")
    sys.exit(0)
fields = ["goroutines","heap_inuse_bytes","heap_objects","heap_alloc_bytes","sys_bytes","num_gc"]
print(",".join(str(d[f]) if f in d and d[f] is not None else "" for f in fields))
' 2>/dev/null || echo ",,,,,"
}

sample_cp_pool() {
  local json
  json=$(t15 docker exec "$CP_CONTAINER" wget -qO- http://127.0.0.1:6060/debug/quasar/pool 2>/dev/null) || { echo ",,,"; return; }
  echo "$json" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(",,,")
    sys.exit(0)
fields = ["acquired_conns","idle_conns","total_conns","empty_acquire_count"]
print(",".join(str(d[f]) if f in d and d[f] is not None else "" for f in fields))
' 2>/dev/null || echo ",,,"
}

sample_cp_rss_kb() {
  t15 docker exec "$CP_CONTAINER" sh -c "grep VmRSS /proc/1/status 2>/dev/null" 2>/dev/null \
    | awk '{print $2}' || true
}

sample_cp_fds() {
  t15 docker exec "$CP_CONTAINER" sh -c "ls /proc/1/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]' || true
}

# sample_cp_uptime_s — seconds since the CP container's process 1 started,
# computed on the HARNESS host from `docker inspect .State.StartedAt`
# (finding #9). A DECREASE between consecutive samples means the container
# restarted — that's the signal soak_report.py segments on, not the absolute
# value itself.
sample_cp_uptime_s() {
  local started
  started=$(t15 docker inspect --format '{{.State.StartedAt}}' "$CP_CONTAINER" 2>/dev/null) || { echo ""; return; }
  [ -n "$started" ] || { echo ""; return; }
  QSES_STARTED_AT="$started" python3 -c '
import os, sys, datetime
s = os.environ.get("QSES_STARTED_AT", "").strip()
try:
    if "." in s:
        base, rest = s.split(".", 1)
        digits = ""
        tz = ""
        for i, ch in enumerate(rest):
            if ch.isdigit():
                digits += ch
            else:
                tz = rest[i:]
                break
        digits = (digits + "000000")[:6]
        s = f"{base}.{digits}{tz or 'Z'}"
    s = s.replace("Z", "+0000")
    fmt = "%Y-%m-%dT%H:%M:%S.%f%z" if "." in s else "%Y-%m-%dT%H:%M:%S%z"
    dt = datetime.datetime.strptime(s, fmt)
    now = datetime.datetime.now(datetime.timezone.utc)
    print(int((now - dt).total_seconds()))
except Exception:
    print("")
' 2>/dev/null || echo ""
}

# sample_agent_uptime_s — same idea as sample_cp_uptime_s (finding #9) but
# for AGENT_CONTAINER (issue #420): the D-5 chain showed a Tower 04:01 backup
# restarting the whole stack silently reset agent fd/RSS baselines with no
# segmentation banner, because only cp_uptime_s was ever sampled. Computed
# the same way (docker inspect .State.StartedAt on the HARNESS host, whatever
# host that is — Tower or hermes, reached the same way the agent's RSS/fd
# samples already are, i.e. local docker exec/inspect against AGENT_CONTAINER,
# no separate ssh hop). A DECREASE between consecutive samples means the
# agent container restarted — soak_report.py segments on it exactly like
# cp_uptime_s. Empty string (the harness-wide missing-sample sentinel, never
# a fabricated "0") on any inspect failure.
sample_agent_uptime_s() {
  local started
  started=$(t15 docker inspect --format '{{.State.StartedAt}}' "$AGENT_CONTAINER" 2>/dev/null) || { echo ""; return; }
  [ -n "$started" ] || { echo ""; return; }
  QSES_STARTED_AT="$started" python3 -c '
import os, sys, datetime
s = os.environ.get("QSES_STARTED_AT", "").strip()
try:
    if "." in s:
        base, rest = s.split(".", 1)
        digits = ""
        tz = ""
        for i, ch in enumerate(rest):
            if ch.isdigit():
                digits += ch
            else:
                tz = rest[i:]
                break
        digits = (digits + "000000")[:6]
        s = f"{base}.{digits}{tz or 'Z'}"
    s = s.replace("Z", "+0000")
    fmt = "%Y-%m-%dT%H:%M:%S.%f%z" if "." in s else "%Y-%m-%dT%H:%M:%S%z"
    dt = datetime.datetime.strptime(s, fmt)
    now = datetime.datetime.now(datetime.timezone.utc)
    print(int((now - dt).total_seconds()))
except Exception:
    print("")
' 2>/dev/null || echo ""
}

sample_db_count() {
  local table="$1"
  t15 docker exec "$PG_CONTAINER" psql -U quasar -d quasar -tAc "select count(*) from $table" 2>/dev/null \
    | tr -d '[:space:]' || true
}

# agent_pid — pgrep against a suffix match (finding #8): the dev stack runs
# /workspace/node-agent/target/release/quasar-node-agent, not the baked-image
# path, so an anchored full-path pattern silently missed it. Takes the FIRST
# match if pgrep somehow returns more than one.
agent_pid() {
  t15 docker exec "$AGENT_CONTAINER" sh -c "pgrep -f 'quasar-node-agent\$' | head -1" 2>/dev/null || true
}

sample_agent_rss_kb() {
  local pid="$1"
  [ -n "$pid" ] || return 0
  t15 docker exec "$AGENT_CONTAINER" sh -c "grep VmRSS /proc/$pid/status 2>/dev/null" 2>/dev/null \
    | awk '{print $2}' || true
}

sample_agent_threads() {
  local pid="$1"
  [ -n "$pid" ] || return 0
  t15 docker exec "$AGENT_CONTAINER" sh -c "grep Threads /proc/$pid/status 2>/dev/null" 2>/dev/null \
    | awk '{print $2}' || true
}

sample_agent_fds() {
  local pid="$1"
  [ -n "$pid" ] || return 0
  t15 docker exec "$AGENT_CONTAINER" sh -c "ls /proc/$pid/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]' || true
}

sample_vram_mb() {
  local nv amd
  nv=$(t15 docker exec "$AGENT_CONTAINER" nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits 2>/dev/null | head -1 | tr -d '[:space:]') || nv=""
  if [ -n "$nv" ]; then
    echo "$nv"
    return
  fi
  # AMD fallback (sysfs, bytes -> MiB). UNTESTED — no AMD host currently
  # reachable for validation; see docs/profiling-soak.md.
  amd=$(t15 docker exec "$AGENT_CONTAINER" sh -c \
    'cat /sys/class/drm/card*/device/mem_info_vram_used 2>/dev/null' 2>/dev/null \
    | awk '{s+=$1} END{if (NR>0) printf "%.0f", s/1024/1024}') || amd=""
  echo "$amd"
}

# sample_gst_alive — only ever emits a number when GST_LEAKS is on AND the
# agent's env confirmed GST_TRACERS armed at startup (AGENT_TRACERS_ARMED);
# otherwise empty, never a fabricated "0" (finding #7 — the old
# `grep -c ... || echo "0"` emitted "0\n0" AND fabricated a zero when the
# tracer wasn't even armed, since grep -c prints its own 0 count and still
# exits 1 on no match).
# usr1_caught — 1 (success) iff the agent process actually catches SIGUSR1:
# SigCgt in /proc/<pid>/status has the signal-10 bit set (mask 0x200, bit
# index 9). AGENT_TRACERS_ARMED only proves GST_TRACERS was in the env at
# startup — NOT that the leaks tracer loaded and installed its handler. If
# nothing catches SIGUSR1 the default disposition is TERMINATE (exit 138, no
# kernel record), so an unguarded `docker kill -s USR1` would silently kill
# and restart the agent on EVERY sample and masquerade as the crash being
# chased (#429). Checked per-sample against the current pid (the agent may
# restart mid-run and come back with/without the tracer).
usr1_caught() {
  local pid="$1" mask
  [ -n "$pid" ] || return 1
  mask=$(t15 docker exec "$AGENT_CONTAINER" sh -c "awk '/^SigCgt/{print \$2}' /proc/$pid/status" 2>/dev/null | tr -d '[:space:]') || return 1
  [ -n "$mask" ] || return 1
  [ $(( 0x$mask >> 9 & 1 )) -eq 1 ]
}

sample_gst_alive() {
  local since="$1" pid="$2" cnt
  [ "$GST_LEAKS" = 1 ] || { echo ""; return; }
  [ "$AGENT_TRACERS_ARMED" = 1 ] || { echo ""; return; }
  if ! usr1_caught "$pid"; then
    # Once-only warn travels by marker file — take_sample runs inside $(), so
    # a plain variable flag would be lost with the subshell (same class as
    # LAUNCH_SID_FILE above).
    if [ ! -f "$OUT_DIR/.usr1-not-caught-warned" ]; then
      echo "WARNING: agent pid ${pid:-<unresolved>} does not catch SIGUSR1 (SigCgt bit 10 clear) — leaks tracer handler not installed; skipping docker kill -s USR1 (it would TERMINATE the agent), gst_alive will be empty" >&2
      : > "$OUT_DIR/.usr1-not-caught-warned"
    fi
    echo ""
    return
  fi
  t15 docker kill -s USR1 "$AGENT_CONTAINER" >/dev/null 2>&1 || { echo ""; return; }
  sleep 1
  # Post-kill liveness check: if the pid vanished right after our USR1, the
  # harness itself almost certainly killed the agent — flag it loudly so the
  # leg's restart is never misread as a crash bug.
  if [ -n "$pid" ] && ! t15 docker exec "$AGENT_CONTAINER" sh -c "[ -d /proc/$pid ]" >/dev/null 2>&1; then
    {
      echo "############################################################"
      echo "# node-agent pid $pid DIED immediately after docker kill -s"
      echo "# USR1 from this harness (SIGUSR1 default disposition ="
      echo "# terminate). This restart is HARNESS-INDUCED, not a crash"
      echo "# bug — gst_alive sampling disabled for the rest of the run."
      echo "############################################################"
    } >&2
    : > "$OUT_DIR/.usr1-not-caught-warned"
    echo ""
    return
  fi
  cnt=$(t15 docker logs --since "$since" "$AGENT_CONTAINER" 2>&1 | grep -c 'object-alive' || true)
  cnt=$(echo "$cnt" | tr -d '[:space:]')
  echo "$cnt"
}

# ── one full sample of all series (used for baseline + every cycle) ───────
take_sample() {
  local cycle_start_iso="$1"
  local cp_rt cp_pool cp_rss cp_fds cp_uptime
  local db_sessions db_metrics db_trace db_auth db_stok db_admin
  local apid a_rss a_threads a_fds vram gst a_uptime

  cp_rt=$(sample_cp_runtime)
  cp_pool=$(sample_cp_pool)
  cp_rss=$(sample_cp_rss_kb)
  cp_fds=$(sample_cp_fds)
  cp_uptime=$(sample_cp_uptime_s)

  db_sessions=$(sample_db_count sessions)
  db_metrics=$(sample_db_count session_metrics)
  db_trace=$(sample_db_count session_trace_events)
  db_auth=$(sample_db_count auth_tokens)
  db_stok=$(sample_db_count session_tokens)
  db_admin=$(sample_db_count admin_activity)

  # take_sample runs inside $(), so plain variable flags do not survive it
  # (the same subshell-loss class as LAUNCH_SID_FILE above) — marker files in
  # OUT_DIR carry the once-only warn + resolved flags instead.
  apid=$(agent_pid)
  if [ -n "$apid" ]; then
    : > "$OUT_DIR/.agent-pid-resolved"
  elif [ ! -f "$OUT_DIR/.agent-pid-warned" ]; then
    echo "WARNING: node-agent PID not resolved via pgrep in $AGENT_CONTAINER — agent_rss_kb/agent_threads/agent_fds will be empty for affected cycles" >&2
    : > "$OUT_DIR/.agent-pid-warned"
  fi
  a_rss=$(sample_agent_rss_kb "$apid")
  a_threads=$(sample_agent_threads "$apid")
  a_fds=$(sample_agent_fds "$apid")
  vram=$(sample_vram_mb)
  gst=$(sample_gst_alive "$cycle_start_iso" "$apid")
  a_uptime=$(sample_agent_uptime_s)

  # agent_uptime_s (issue #420) is appended at the END, matching its position
  # in CSV_COLUMNS — see the comment there on why append-only preserves
  # report-only backward compat with old CSVs.
  echo "$cp_rt,$cp_rss,$cp_fds,$cp_uptime,$cp_pool,$db_sessions,$db_metrics,$db_trace,$db_auth,$db_stok,$db_admin,$a_rss,$a_threads,$a_fds,$vram,$gst,$a_uptime"
}

# write_params_json (finding #17) — build via python3's json.dump so
# quotes/backslashes in --app, --profile, or the abort reason can never
# break the JSON (the old heredoc did plain string interpolation). python3
# is already a hard prerequisite (require curl docker python3 date above),
# but keep a naive-with-escaping fallback for defense in depth.
write_params_json() {
  if command -v python3 >/dev/null 2>&1; then
    QSES_PARAMS_PATH="$PARAMS_PATH" \
    QSES_P_DURATION="$DURATION" QSES_P_HOLD="$HOLD" QSES_P_SETTLE="$SETTLE" \
    QSES_P_APP="$APP" QSES_P_PROFILE="${PROFILE:-}" QSES_P_API="$API" \
    QSES_P_HOST="$(hostname 2>/dev/null || echo unknown)" QSES_P_START="$START_ISO" \
    QSES_P_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)" QSES_P_CYCLES="$CYCLE_COUNT" \
    QSES_P_MAXFAIL="$MAX_CONSECUTIVE_FAILURES" QSES_P_ABORTED="$ABORTED_REASON" \
    QSES_P_LAUNCH_TIMEOUT="$LAUNCH_TIMEOUT" QSES_P_TEARDOWN_TIMEOUT="$TEARDOWN_TIMEOUT" \
    QSES_P_TRACERS_ARMED="$AGENT_TRACERS_ARMED" QSES_P_AGENT_PID_RESOLVED="$( [ -f "$OUT_DIR/.agent-pid-resolved" ] && echo 1 || echo 0 )" \
    QSES_P_HAVE_TIMEOUT="$HAVE_TIMEOUT" \
    python3 -c '
import json, os

def b(v):
    return v == "1"

data = {
    "duration": os.environ["QSES_P_DURATION"],
    "hold": int(os.environ["QSES_P_HOLD"]),
    "settle": int(os.environ["QSES_P_SETTLE"]),
    "app": os.environ["QSES_P_APP"],
    "profile": os.environ["QSES_P_PROFILE"],
    "api": os.environ["QSES_P_API"],
    "host": os.environ["QSES_P_HOST"],
    "start": os.environ["QSES_P_START"],
    "end": os.environ["QSES_P_END"],
    "cycles": int(os.environ["QSES_P_CYCLES"]),
    "max_consecutive_failures": int(os.environ["QSES_P_MAXFAIL"]),
    "aborted_reason": os.environ["QSES_P_ABORTED"],
    "launch_timeout_s": int(os.environ["QSES_P_LAUNCH_TIMEOUT"]),
    "teardown_timeout_s": int(os.environ["QSES_P_TEARDOWN_TIMEOUT"]),
    "agent_tracers_armed": b(os.environ["QSES_P_TRACERS_ARMED"]),
    "agent_pid_resolved": b(os.environ["QSES_P_AGENT_PID_RESOLVED"]),
    "coreutils_timeout_available": b(os.environ["QSES_P_HAVE_TIMEOUT"]),
}
with open(os.environ["QSES_PARAMS_PATH"], "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
'
  else
    local esc_app esc_profile esc_reason
    esc_app=$(printf '%s' "$APP" | sed 's/\\/\\\\/g; s/"/\\"/g')
    esc_profile=$(printf '%s' "${PROFILE:-}" | sed 's/\\/\\\\/g; s/"/\\"/g')
    esc_reason=$(printf '%s' "$ABORTED_REASON" | sed 's/\\/\\\\/g; s/"/\\"/g')
    local tracers_armed_json="false" pid_resolved_json="false" have_timeout_json="false"
    [ "$AGENT_TRACERS_ARMED" = 1 ] && tracers_armed_json="true"
    [ -f "$OUT_DIR/.agent-pid-resolved" ] && pid_resolved_json="true"
    [ "$HAVE_TIMEOUT" = 1 ] && have_timeout_json="true"
    cat > "$PARAMS_PATH" <<EOF
{
  "duration": "$DURATION",
  "hold": $HOLD,
  "settle": $SETTLE,
  "app": "$esc_app",
  "profile": "$esc_profile",
  "api": "$API",
  "host": "$(hostname 2>/dev/null || echo unknown)",
  "start": "$START_ISO",
  "end": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "cycles": $CYCLE_COUNT,
  "max_consecutive_failures": $MAX_CONSECUTIVE_FAILURES,
  "aborted_reason": "$esc_reason",
  "launch_timeout_s": $LAUNCH_TIMEOUT,
  "teardown_timeout_s": $TEARDOWN_TIMEOUT,
  "agent_tracers_armed": $tracers_armed_json,
  "agent_pid_resolved": $pid_resolved_json,
  "coreutils_timeout_available": $have_timeout_json
}
EOF
  fi
}

# ── cleanup / crash safety ─────────────────────────────────────────────
cleanup() {
  local rc=$?
  # A Ctrl-C mid-mint must not strand the per-boot dev key in /tmp.
  [ -n "${DEVKEY_HDR_FILE:-}" ] && rm -f "$DEVKEY_HDR_FILE"
  # Stop any in-flight session this run launched.
  if [ -n "$CUR_SID" ] && [ -n "$TOK" ]; then
    if teardown_session "$CUR_SID"; then
      CUR_SID=""
    else
      echo "EXIT trap: teardown of $CUR_SID did not confirm a terminal state — leaving it for a manual check" >&2
    fi
  fi

  write_params_json

  if [ -f "$CSV_PATH" ]; then
    if command -v python3 >/dev/null 2>&1; then
      if python3 "$ROOT/scripts/harness/lib/soak_report.py" "$CSV_PATH"; then
        pass "report generated from $CSV_PATH"
      else
        fail "soak_report.py exited nonzero"
      fi
    else
      echo "python3 not found — run this elsewhere:" >&2
      echo "  python3 $ROOT/scripts/harness/lib/soak_report.py $CSV_PATH" >&2
      skip "report generation (no python3 on this host)"
    fi
  else
    skip "report generation (no CSV was written — $CSV_PATH missing)"
  fi

  if [ -n "$ABORTED_REASON" ]; then
    fail "run aborted: $ABORTED_REASON"
  else
    pass "completed $CYCLE_COUNT cycle(s)"
  fi
  harness_note "cycles_completed" "$CYCLE_COUNT"
  harness_note "params_json" "$PARAMS_PATH"

  harness_report "$rc"
}
trap cleanup EXIT

# ── setup checks ─────────────────────────────────────────────────────────
if t15 docker exec "$CP_CONTAINER" wget -qO- http://127.0.0.1:6060/debug/quasar/runtime >/dev/null 2>&1; then
  pass "debug listener reachable in $CP_CONTAINER"
else
  fail "debug listener NOT reachable in $CP_CONTAINER (is PROF-01 deployed and the container up?)"
  ABORTED_REASON="debug listener unreachable"
  exit 1
fi

for c in "$CP_CONTAINER" "$AGENT_CONTAINER" "$PG_CONTAINER"; do
  if t15 docker inspect "$c" >/dev/null 2>&1; then
    pass "container present: $c"
  else
    fail "container missing: $c"
    ABORTED_REASON="container missing: $c"
    exit 1
  fi
done

# Tracer-armed check (finding #7) — once per run, not per cycle. Only
# meaningful with --gst-leaks; without it we still probe so params.json
# always records the fact for later reference.
AGENT_TRACER_ENV=$(t15 docker exec "$AGENT_CONTAINER" printenv GST_TRACERS 2>/dev/null) || AGENT_TRACER_ENV=""
# The leaks tracer only installs a SIGUSR1 handler when GST_LEAKS_TRACER_SIG=1.
# Without it, sample_gst_alive's `docker kill -s USR1` hits the DEFAULT signal
# disposition and TERMINATES the agent (exit 138, no dmesg, no core — a perfect
# #429 lookalike). So "armed" requires BOTH env vars, not just GST_TRACERS.
AGENT_TRACER_SIG=$(t15 docker exec "$AGENT_CONTAINER" printenv GST_LEAKS_TRACER_SIG 2>/dev/null) || AGENT_TRACER_SIG=""
if [ -n "$AGENT_TRACER_ENV" ] && [ "$AGENT_TRACER_SIG" = 1 ]; then
  AGENT_TRACERS_ARMED=1
  pass "agent GST_TRACERS armed: $AGENT_TRACER_ENV (SIG=1)"
else
  AGENT_TRACERS_ARMED=0
  if [ "$GST_LEAKS" = 1 ]; then
    if [ -n "$AGENT_TRACER_ENV" ]; then
      echo "WARNING: --gst-leaks requested but GST_LEAKS_TRACER_SIG!=1 on $AGENT_CONTAINER — sending USR1 would KILL the agent (no handler installed); gst_alive will be empty for every cycle this run" >&2
    else
      echo "WARNING: --gst-leaks requested but $AGENT_CONTAINER has no GST_TRACERS in its env — gst_alive will be empty for every cycle this run" >&2
    fi
  fi
fi

login
if [ -n "$TOK" ]; then
  pass "login ok (${EMAIL:-dev-minted identity})"
else
  fail "login failed (${EMAIL:-dev-mint}) against $API — with --email the user must exist and be entitled (registration is closed by default); the default dev-mint path needs QUASAR_DEV_AGENT_AUTH=1 on the stack; see docs/profiling-soak.md prerequisites"
  ABORTED_REASON="login failed"
  exit 1
fi

APP_ID=$(get_app_id) || true
if [ -n "$APP_ID" ]; then
  pass "app resolved: $APP -> $APP_ID"
else
  fail "app not found: $APP"
  ABORTED_REASON="app not found: $APP"
  exit 1
fi

# ── baseline sample (cycle 0), BEFORE the first launch ─────────────────
ensure_csv_header
BASELINE_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
BASELINE_SAMPLE=$(take_sample "$BASELINE_TS")
write_csv_row 0 "$BASELINE_TS" 1 "" "" "$BASELINE_SAMPLE"
echo "cycle 0/baseline: sampled before first launch"

# A cycle that runs long past its expected budget (hold + launch_timeout +
# teardown_timeout + slack) is itself a failure signal (finding #5) — every
# individual curl/docker-exec call is now time-boxed, so nothing can hang
# forever, but a slow-but-not-hung control plane could still eat the whole
# run one cycle at a time without this.
MAX_CYCLE_SECS=$(( HOLD + LAUNCH_TIMEOUT + TEARDOWN_TIMEOUT + 120 ))

# ── main soak loop ──────────────────────────────────────────────────────
CONSECUTIVE_FAILURES=0
CYCLE=0
while :; do
  NOW=$(date -u +%s)
  [ "$NOW" -lt "$END_EPOCH" ] || { echo "duration budget reached"; break; }
  if [ -n "$CYCLES" ] && [ "$CYCLE" -ge "$CYCLES" ]; then
    echo "cycle budget reached ($CYCLES)"
    break
  fi
  CYCLE=$((CYCLE + 1))
  CYCLE_T0=$(date -u +%s)
  CYCLE_START_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)

  CYCLE_OK=1
  L2R=""
  SID=""
  LAUNCH_ERR_FILE="${TMPDIR:-/tmp}/soak-launch-err.$$"
  set +e
  L2R=$(launch_session "$APP_ID" 2>"$LAUNCH_ERR_FILE")
  LAUNCH_RC=$?
  set -e
  LAUNCH_ERR=""
  [ -f "$LAUNCH_ERR_FILE" ] && LAUNCH_ERR=$(cat "$LAUNCH_ERR_FILE" 2>/dev/null)
  rm -f "$LAUNCH_ERR_FILE"
  # The sid comes back via file (see LAUNCH_SID_FILE above) — CUR_SID assigned
  # inside the $() subshell never reaches this scope. Re-establish CUR_SID here
  # so the EXIT trap sees what the launch actually created.
  SID="$(launch_sid)"
  CUR_SID="$SID"
  if [ "$LAUNCH_RC" -ne 0 ]; then
    CYCLE_OK=0
    L2R=""
    echo "cycle $CYCLE: launch FAILED: $LAUNCH_ERR"
    # A session id may have been obtained even though the launch ultimately
    # failed (e.g. it never reached running) — that session is untracked
    # unless torn down here (finding #4). Never clear CUR_SID until teardown
    # confirms a terminal state; if teardown itself gives up, leave CUR_SID
    # set so the EXIT trap retries it.
    if [ -n "$SID" ]; then
      if teardown_session "$SID"; then
        echo "cycle $CYCLE: torn down failed-launch session $SID"
        CUR_SID=""
      else
        echo "cycle $CYCLE: teardown of failed-launch session $SID did not confirm terminal state — leaving CUR_SID set for the EXIT trap to retry" >&2
      fi
    else
      CUR_SID=""
    fi
  else
    sleep "$HOLD"
    if teardown_session "$SID"; then
      CUR_SID=""
    else
      echo "cycle $CYCLE: teardown did not confirm terminal state (continuing) — leaving CUR_SID set for the EXIT trap to retry" >&2
    fi
  fi
  sleep "$SETTLE"

  SAMPLE=$(take_sample "$CYCLE_START_TS")

  CYCLE_ELAPSED=$(( $(date -u +%s) - CYCLE_T0 ))
  if [ "$CYCLE_ELAPSED" -gt "$MAX_CYCLE_SECS" ]; then
    echo "cycle $CYCLE: took ${CYCLE_ELAPSED}s (> ${MAX_CYCLE_SECS}s budget: hold+launch_timeout+teardown_timeout+120) — treating as failed" >&2
    CYCLE_OK=0
  fi

  write_csv_row "$CYCLE" "$CYCLE_START_TS" "$CYCLE_OK" "$SID" "$L2R" "$SAMPLE"
  CYCLE_COUNT=$CYCLE

  # headline numbers for the one-line-per-cycle log (best-effort parse; blank
  # if a field was empty).
  # Field indices below follow take_sample's fixed emission order (see
  # CSV_COLUMNS, offset by the 5 identifier columns written separately):
  # 1 cp_goroutines ... 7 cp_rss_kb ... 9 cp_uptime_s ... 20 agent_rss_kb ...
  # 23 vram_used_mb.
  CP_GOROUTINES=$(echo "$SAMPLE" | cut -d, -f1)
  CP_RSS=$(echo "$SAMPLE" | cut -d, -f7)
  AGENT_RSS=$(echo "$SAMPLE" | cut -d, -f20)
  VRAM=$(echo "$SAMPLE" | cut -d, -f23)
  echo "cycle $CYCLE/${CYCLES:-inf}: ok=$CYCLE_OK sid=${SID:-none} l2r=${L2R:-?}s goroutines=${CP_GOROUTINES:-?} cp_rss_kb=${CP_RSS:-?} agent_rss_kb=${AGENT_RSS:-?} vram_mb=${VRAM:-?}"

  if [ "$CYCLE_OK" = 1 ]; then
    CONSECUTIVE_FAILURES=0
  else
    CONSECUTIVE_FAILURES=$((CONSECUTIVE_FAILURES + 1))
    if [ "$CONSECUTIVE_FAILURES" -ge "$MAX_CONSECUTIVE_FAILURES" ]; then
      ABORTED_REASON="$CONSECUTIVE_FAILURES consecutive cycle failures (>= --max-consecutive-failures $MAX_CONSECUTIVE_FAILURES)"
      echo "$ABORTED_REASON" >&2
      exit 1
    fi
  fi
done

exit 0
