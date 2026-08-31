#!/usr/bin/env bash
# run-st-trace.sh — ST-08 acceptance harness: end-to-end session-trace validation.
#
# One command → structured diagnostic report proving the Observability v2 pipeline
# works on a real H.264 browser decode:
#   1. Launch "NV Stress: Colour Ripple" (falls back to "Quasar Bench: Colour Ripple")
#   2. Drive a real headless Chrome-for-Testing peer (H.264 decode check)
#   3. Let ABR + adaptive playout act (mild netem to trigger events)
#   4. Pull GET /v1/admin/sessions/{id}/diagnostic-bundle
#   5. Assert the bundle contains (on a single timeline):
#        - agent encode metrics (fps / encode_ms / setpoint in series)
#        - browser presentation metrics (present_fps / present_interval_sd_ms)
#        - ≥1 abr.retarget event AND ≥1 playout.changed event
#        - a clock offset with stated uncertainty OR {"unmeasured": true}
#        - a classifier verdict from the current closed set with evidence
#   6. Emit a structured report. PASS/FAIL clearly at the bottom.
#
# Usage:
#   bash scripts/harness/run-st-trace.sh [app-name-or-id] [--shape <profile>]
#
# App name examples (tries in order, uses first found + enabled):
#   "NV Stress: Colour Ripple"    — known-good torture decode app (default try #1)
#   "Quasar Bench: Colour Ripple" — benchmark-seeded fallback (default try #2)
#
# Network shaping (applied mid-session to drive ABR + playout events):
#   --shape mild      — 20ms delay ±2ms, 0.5% loss, 5Mbps rate (default; recommended)
#   --shape moderate  — 40ms delay ±10ms, 1% loss, 3.5Mbps rate
#   --shape clean     — no shaping (ABR/playout events may not fire → warnings only)
#
# Prerequisites (all on hermes):
#   - Live stack up: qstack up
#   - Colour Ripple app seeded: qstack p4-bench (seeds "Quasar Bench: Colour Ripple")
#   - Chrome-for-Testing at /tmp/cft/chrome-linux64/chrome (T8 dep, already present)
#   - Playwright installed: /tmp/t8-driver/node_modules present
#
# Run:
#   bash scripts/harness/run-st-trace.sh
#   bash scripts/harness/run-st-trace.sh "Quasar Bench: Colour Ripple" --shape moderate
#
# Or from the Mac via qstack:
#   qstack st-trace
#   qstack st-trace "Quasar Bench: Colour Ripple"
#
# Exit 0 = all assertions PASS. Non-zero = one or more FAIL (or infra failure).
# The session is ALWAYS deleted on exit (trap EXIT).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
set -a; . "$ROOT/deploy/.env"; set +a

# ── Arg parsing ──────────────────────────────────────────────────────────────
APP_NAME_ARG=""
SHAPE="mild"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --shape) SHAPE="$2"; shift 2 ;;
    *) APP_NAME_ARG="$1"; shift ;;
  esac
done

API="${API:-http://localhost:${CONTROL_PORT:-8080}}"
ADMIN_EMAIL="${BOOTSTRAP_ADMIN_EMAIL:?}"
ADMIN_PASS="${BOOTSTRAP_ADMIN_PASSWORD:?}"

# Chrome-for-Testing (T8 dep, same path as the other peer-driven harnesses)
CHROME="${CHROME:-/tmp/cft/chrome-linux64/chrome}"
PW_DIR="${PW_DIR:-/tmp/t8-driver}"
# Reuse the peer-driver.mjs Playwright runner (same browser driver pattern)
RUNNER="$ROOT/scripts/harness/peer-driver.mjs"

# Measurement parameters — longer than p4 to ensure events accumulate
WARMUP="${WARMUP:-12}"      # seconds before measurement (let netem + ABR act)
SECS="${SECS:-45}"          # measurement window: long enough for ABR to retarget + playout to change
CONNECT_TIMEOUT_MS="${CONNECT_TIMEOUT_MS:-55000}"
BUNDLE_WINDOW_MINS="${BUNDLE_WINDOW_MINS:-5}"  # diagnostic-bundle window (minutes)

# Results dir
RESULTS_DIR="$ROOT/deploy/results"
mkdir -p "$RESULTS_DIR"
TS=$(date -u '+%Y%m%dT%H%M%SZ')
REPORT_FILE="$RESULTS_DIR/st-trace-${SHAPE}-${TS}.json"

# ── Colour helpers ────────────────────────────────────────────────────────────
col_green='\033[0;32m'; col_red='\033[0;31m'; col_cyan='\033[0;36m'
col_yellow='\033[0;33m'; col_reset='\033[0m'
say()  { printf "\n${col_cyan}== %s ==${col_reset}\n" "$*"; }
pass() { printf "${col_green}  PASS${col_reset} %s\n" "$*"; ((ST_PASS++)) || true; }
fail() { printf "${col_red}  FAIL${col_reset} %s\n" "$*" >&2; ((ST_FAIL++)) || true; }
warn() { printf "${col_yellow}  WARN${col_reset} %s\n" "$*"; }
die()  { echo "FATAL: $*" >&2; exit 1; }

ST_PASS=0; ST_FAIL=0

# ── Preflight ─────────────────────────────────────────────────────────────────
say "preflight"
[ -f "$CHROME" ] || die "Chrome-for-Testing not found at $CHROME. Run: qses provision"
[ -d "$PW_DIR/node_modules" ] || die "Playwright node_modules not at $PW_DIR. Run: cd $PW_DIR && npm install playwright-core@1.60.0"
[ -f "$RUNNER" ] || die "Playwright runner not found at $RUNNER"
curl -fs "$API/health" >/dev/null 2>&1 || die "Control-plane not healthy at $API — is the stack up? (try: qstack up)"
pass "all prerequisites present"

# ── Network shaping ───────────────────────────────────────────────────────────
# Uses the quasar-netem helper container (alpine + iproute2, NET_ADMIN, host network)
# when present — same approach as run-as06-abr-netem.sh. Falls back to sudo tc.
# Only shapes UDP on lo (WebRTC media) via prio+u32 filter so the control-plane TCP
# (DB / API / WS) stays unimpaired — a flat rate on all lo starves stack control traffic.
TC_APPLIED=false
TC_DEV="${TC_DEV:-lo}"
NETEM_IMG="${NETEM_IMG:-quasar-netem}"
USE_TC_HELPER=false
docker image inspect "$NETEM_IMG" >/dev/null 2>&1 && USE_TC_HELPER=true

netem_sh() {  # run an arbitrary tc snippet as root with NET_ADMIN on host network
  if [ "$USE_TC_HELPER" = "true" ]; then
    docker run --rm --network host --cap-add NET_ADMIN "$NETEM_IMG" sh -c "$1"
  else
    sudo sh -c "$1"
  fi
}

tc_cleanup() {
  # Unconditionally attempt deletion — handles stale rules from previous runs
  # even when TC_APPLIED=false (fresh invocation). The del fails silently if
  # no root qdisc exists, so this is safe to call at any point.
  local had_rule="$TC_APPLIED"
  netem_sh "tc qdisc del dev $TC_DEV root" >/dev/null 2>&1 || true
  TC_APPLIED=false
  [ "$had_rule" = "true" ] && echo "  netem removed" >&2 || true
}

# Shape UDP-only on lo (protocol 17) into netem band; leave TCP alone.
# Mirrors the run-as06-abr-netem.sh prio+u32 recipe.
tc_apply_udp() {
  local netem_args="$1"
  tc_cleanup  # remove any stale rules
  netem_sh "
    tc qdisc add dev $TC_DEV root handle 1: prio &&
    tc qdisc add dev $TC_DEV parent 1:3 handle 30: netem $netem_args &&
    tc filter add dev $TC_DEV parent 1: protocol ip u32 match ip protocol 17 0xff flowid 1:3
  " 2>/dev/null && TC_APPLIED=true || true
}

apply_netem() {
  case "$SHAPE" in
    mild)
      tc_apply_udp "delay 20ms 2ms loss 0.5% rate 5000kbit"
      ;;
    moderate)
      tc_apply_udp "delay 40ms 10ms loss 1% rate 3500kbit"
      ;;
    clean)
      true  # no shaping
      ;;
    *)
      warn "unknown shape '$SHAPE' — treating as clean"
      SHAPE=clean
      ;;
  esac
}

# ── Cleanup trap ─────────────────────────────────────────────────────────────
SID=""
ADMIN_TOK=""
cleanup() {
  echo "" >&2
  say "cleanup"
  tc_cleanup
  if [ -n "$SID" ] && [ -n "$ADMIN_TOK" ]; then
    HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
      "$API/v1/sessions/$SID" -H "Authorization: Bearer $ADMIN_TOK")
    if [ "$HTTP_CODE" = "202" ] || [ "$HTTP_CODE" = "200" ] || \
       [ "$HTTP_CODE" = "204" ] || [ "$HTTP_CODE" = "404" ]; then
      pass "session $SID stopped (HTTP $HTTP_CODE)"
    else
      fail "session stop returned HTTP $HTTP_CODE — may have leaked"
    fi
  fi
}
trap cleanup EXIT

# ── 0. Login ─────────────────────────────────────────────────────────────────
say "0. login"
ADMIN_TOK=$(curl -fs -X POST "$API/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])") \
  || die "admin login failed"
pass "admin token obtained"

TEST_EMAIL="st-harness@quasar.local"
TEST_PASS="STHarness123!"
curl -fs -X POST "$API/v1/auth/register" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$TEST_EMAIL\",\"username\":\"stharness\",\"password\":\"$TEST_PASS\"}" \
  >/dev/null 2>&1 || true
USER_TOK=$(curl -fs -X POST "$API/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$TEST_EMAIL\",\"password\":\"$TEST_PASS\"}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])") \
  || die "user login failed"
pass "user token obtained"

# ── 1. Resolve app ────────────────────────────────────────────────────────────
say "1. resolve app"
APPS_JSON=$(curl -fs "$API/v1/admin/apps" -H "Authorization: Bearer $ADMIN_TOK")

# Build candidate list: explicit arg first, then "NV Stress: Colour Ripple",
# then "Quasar Bench: Colour Ripple" (both may be seeded; use first enabled match).
CANDIDATES=()
[ -n "$APP_NAME_ARG" ] && CANDIDATES+=("$APP_NAME_ARG")
CANDIDATES+=("NV Stress: Colour Ripple" "Quasar Bench: Colour Ripple" "Colour Ripple")

APP_ID=""
RESOLVED_NAME=""
for CANDIDATE in "${CANDIDATES[@]}"; do
  RESULT=$(echo "$APPS_JSON" | python3 -c "
import sys, json
apps = json.load(sys.stdin).get('items', [])
name = '''$CANDIDATE'''
# Exact name match (enabled)
for a in apps:
    if a['name'] == name and a.get('enabled'):
        print(a['id'] + '|' + a['name']); sys.exit(0)
# UUID match
for a in apps:
    if a['id'] == name:
        print(a['id'] + '|' + a['name']); sys.exit(0)
# Case-insensitive substring (enabled)
nl = name.lower()
for a in apps:
    if nl in a['name'].lower() and a.get('enabled'):
        print(a['id'] + '|' + a['name']); sys.exit(0)
" 2>/dev/null || echo "")
  if [ -n "$RESULT" ]; then
    APP_ID="${RESULT%%|*}"
    RESOLVED_NAME="${RESULT##*|}"
    break
  fi
done

[ -n "$APP_ID" ] || die "No Colour Ripple app found (tried: ${CANDIDATES[*]}). Run: qstack p4-bench (seeds 'Quasar Bench: Colour Ripple')"
pass "resolved: $RESOLVED_NAME ($APP_ID)"

# ── 2. Launch session ─────────────────────────────────────────────────────────
say "2. launch session"
LAUNCH=$(curl -fs -X POST "$API/v1/sessions" \
  -H "Authorization: Bearer $USER_TOK" \
  -H 'Content-Type: application/json' \
  -d "{\"app_id\":\"$APP_ID\"}") || die "launch failed"
SID=$(echo "$LAUNCH" | python3 -c "import sys,json; print(json.load(sys.stdin)['session']['id'])")
SIG_URL=$(echo "$LAUNCH" | python3 -c "import sys,json; print(json.load(sys.stdin)['signaling']['url'])")
SIG_TOKEN=$(echo "$LAUNCH" | python3 -c "import sys,json; print(json.load(sys.stdin)['signaling']['token'])")
[ -n "$SID" ] || die "no session id in launch response"
pass "session $SID launched"

# ── 3. Wait for running ───────────────────────────────────────────────────────
say "3. wait for state=running"
STATE=""
for _ in $(seq 1 30); do
  sleep 2
  STATE=$(curl -fs "$API/v1/sessions/$SID" \
    -H "Authorization: Bearer $USER_TOK" \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['session']['state'])")
  [ "$STATE" = "running" ] && break
  [ "$STATE" = "failed" ] && break
  echo "  state=$STATE..."
done
[ "$STATE" = "running" ] || die "session did not reach running (state=$STATE) — cannot proceed"
HOST_ID=$(curl -fs "$API/v1/sessions/$SID" \
  -H "Authorization: Bearer $USER_TOK" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['session']['host_id'])")
pass "session running (host: $HOST_ID)"

# ── 4. Schedule network shaping (applied MID-SESSION after decode) ────────────
# Following the AS-06 design: browser connects clean, netem is applied during the
# warmup window (before measurement), so decode succeeds and ABR fires under load.
# A background subshell waits NETEM_AFTER_S seconds (just enough for ICE + decode),
# then applies netem. This matches the "connect clean, then degrade" AS-06 recipe.
NETEM_AFTER_S="${NETEM_AFTER_S:-8}"  # wait for ICE + first frames, then shape
say "4. network shaping: $SHAPE (applied ${NETEM_AFTER_S}s after launch — mid-session)"
NETEM_BG_PID=""
case "$SHAPE" in
  mild|moderate)
    if [ "$USE_TC_HELPER" = "true" ] || true; then
      # Background process: wait NETEM_AFTER_S seconds then apply netem.
      # The Playwright runner has a CONNECT_TIMEOUT then WARMUP window, so
      # netem fires while the browser is decoding during warmup.
      (
        sleep "$NETEM_AFTER_S"
        apply_netem
        if [ "$TC_APPLIED" = "true" ]; then
          case "$SHAPE" in
            mild)     echo "[netem-bg] applied: 20ms delay ±2ms, 0.5% loss (UDP-only, driving ABR)" >&2 ;;
            moderate) echo "[netem-bg] applied: 40ms delay ±10ms, 1% loss (UDP-only, driving ABR)" >&2 ;;
          esac
        else
          echo "[netem-bg] WARN: tc netem failed — ABR/playout events may not fire" >&2
        fi
      ) &
      NETEM_BG_PID=$!
      pass "netem background timer armed (fires in ${NETEM_AFTER_S}s, after decode)"
    fi
    ;;
  clean)
    echo "  shape=clean: no netem (ABR/playout events may not fire in the measurement window)"
    warn "Using --shape mild or moderate is strongly recommended to guarantee abr.retarget + playout.changed events"
    ;;
esac

# ── 5. Run Playwright browser ─────────────────────────────────────────────────
say "5. drive headless Chrome-for-Testing"
echo "  app:    $RESOLVED_NAME"
echo "  warmup: ${WARMUP}s | measure: ${SECS}s | timeout: ${CONNECT_TIMEOUT_MS}ms"

PW_STDERR_LOG="/tmp/st-pw-stderr-$$.log"
set +e
BROWSER_JSON=$(
  SPA_URL="$API" \
  SID="$SID" \
  SIG_URL="$SIG_URL" \
  SIG_TOKEN="$SIG_TOKEN" \
  AUTH_TOKEN="$USER_TOK" \
  APP_NAME="$RESOLVED_NAME" \
  CHROME="$CHROME" \
  PW_DIR="$PW_DIR" \
  WARMUP="$WARMUP" \
  SECS="$SECS" \
  CONNECT_TIMEOUT_MS="$CONNECT_TIMEOUT_MS" \
  node "$RUNNER" 2>"$PW_STDERR_LOG"
)
PW_EXIT=$?
set -e
cat "$PW_STDERR_LOG" >&2
rm -f "$PW_STDERR_LOG"

# Wait for and reap the netem background timer
[ -n "$NETEM_BG_PID" ] && wait "$NETEM_BG_PID" 2>/dev/null || true

if [ "$PW_EXIT" -ne 0 ] && [ -z "$BROWSER_JSON" ]; then
  BROWSER_JSON='{"error":"playwright_exit","message":"runner exited non-zero with no output","lightweight":null,"deep_trace":"unavailable"}'
fi

H264_DECODED=true
if echo "$BROWSER_JSON" | python3 -c "import sys,json; d=json.load(sys.stdin); exit(0 if 'error' not in d else 1)" 2>/dev/null; then
  pass "H.264 decoded — Keystone 1 PASS"
else
  H264_DECODED=false
  fail "H.264 decode failed — Keystone 1 FAILED (check avahi-daemon on Linux host for mDNS ICE)"
fi

# ── 6. Remove netem (session still alive for bundle fetch) ────────────────────
say "6. remove netem"
tc_cleanup

# Give the browser-side events a moment to flush via POST /v1/sessions/{id}/trace/events
# (the SPA posts on visibility-change / interval; wait a bit for the last events)
sleep 3

# ── 7. Pull diagnostic bundle ─────────────────────────────────────────────────
say "7. pull diagnostic-bundle"
BUNDLE_JSON=$(curl -fs \
  "$API/v1/admin/sessions/$SID/diagnostic-bundle?window=${BUNDLE_WINDOW_MINS}m" \
  -H "Authorization: Bearer $ADMIN_TOK" 2>/dev/null || echo '{}')

# Validate we got a real bundle (not empty/error)
BUNDLE_OK=$(echo "$BUNDLE_JSON" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print('ok' if 'trace' in d and 'series' in d else 'empty')
except Exception as e:
    print('parse_error:' + str(e))
" 2>/dev/null || echo "parse_error")

if [ "$BUNDLE_OK" = "ok" ]; then
  pass "diagnostic-bundle received (valid shape)"
else
  fail "diagnostic-bundle response invalid: $BUNDLE_OK"
  echo "  raw (first 500 chars): ${BUNDLE_JSON:0:500}" >&2
fi

# ── 8. Assert bundle contents ─────────────────────────────────────────────────
say "8. assert bundle contents"

# Write bundle to temp file to avoid shell interpolation issues in python3
BUNDLE_TMP="/tmp/st-bundle-$$.json"
printf '%s' "$BUNDLE_JSON" > "$BUNDLE_TMP"

ASSERT_EXIT=0
python3 - "$BUNDLE_TMP" "$SHAPE" <<'PYEOF' || ASSERT_EXIT=$?
import json, sys, os

bundle_file = sys.argv[1]
shape = sys.argv[2]
col_green = '\033[0;32m'
col_red   = '\033[0;31m'
col_yellow = '\033[0;33m'
col_reset = '\033[0m'

pass_count = 0
fail_count = 0

def chk(label, ok, detail=''):
    global pass_count, fail_count
    if ok:
        print(f"{col_green}  PASS{col_reset} {label}" + (f" ({detail})" if detail else ''))
        pass_count += 1
    else:
        print(f"{col_red}  FAIL{col_reset} {label}" + (f" — {detail}" if detail else ''), file=sys.stderr)
        fail_count += 1

def warn(msg):
    print(f"{col_yellow}  WARN{col_reset} {msg}")

try:
    with open(bundle_file) as f:
        b = json.load(f)
except Exception as e:
    print(f"{col_red}  FAIL{col_reset} bundle JSON parse error: {e}", file=sys.stderr)
    sys.exit(1)

# ── A. Agent encode metrics in series ────────────────────────────────────────
series = b.get('series', {})

encode_ms_pts = series.get('encoder.encode_ms', [])
fps_pts       = series.get('encoder.fps', [])
setpoint_pts  = series.get('abr.setpoint_kbps', [])
stage_required = {
    'source.fps': 'source/compositor cadence',
    'compositor.fps': 'compositor output cadence',
    'compositor.pts_delta_p95_ms': 'compositor PTS tail',
    'interpipe.queue_level_max': 'interpipe occupancy',
    'interpipe.queue_dwell_p95_ms': 'interpipe dwell tail',
    'interpipe.queue_drops': 'interpipe drops',
    'encoder.encode_ms_p50': 'encode p50',
    'encoder.encode_ms_p95': 'encode p95',
    'rtp.fps': 'RTP frame cadence',
    'rtp.bitrate_kbps': 'RTP wire bitrate',
}

chk('agent series: encoder.encode_ms present',
    len(encode_ms_pts) > 0,
    f"{len(encode_ms_pts)} points")
chk('agent series: encoder.fps present',
    len(fps_pts) > 0,
    f"{len(fps_pts)} points")
chk('agent series: abr.setpoint_kbps present',
    len(setpoint_pts) > 0,
    f"{len(setpoint_pts)} points; latest={setpoint_pts[-1]['v'] if setpoint_pts else 'n/a'}kbps")
for name, label in stage_required.items():
    points = series.get(name, [])
    chk(f'stage series: {label} ({name}) present', len(points) > 0, f"{len(points)} points")

# Sanity: fps > 0
if fps_pts:
    mean_fps = sum(p['v'] for p in fps_pts) / len(fps_pts)
    chk('agent fps > 0 (real encode happening)',
        mean_fps > 0,
        f"mean={mean_fps:.1f}")

# ── B. Browser presentation metrics ──────────────────────────────────────────
present_sd_pts  = series.get('client.present_interval_sd_ms', [])
present_fps_pts = series.get('client.present_fps', []) or series.get('client.fps', [])
client_stage_required = {
    'client.fps': 'receive/decode cadence',
    'client.decode_ms': 'decode time',
    'client.frames_dropped': 'receiver drops',
    'client.present_fps': 'presentation cadence',
    'client.present_interval_p95_ms': 'presentation interval tail',
    'client.freeze_count': 'presentation freezes',
    'client.display_refresh_hz': 'display refresh',
    'client.glass_to_glass_ms': 'glass-to-glass latency',
    'transport.rtt_ms': 'RTT',
    'transport.jitter_buffer_ms': 'jitter buffer',
    'transport.packets_lost': 'packet loss',
}

chk('browser series: client.present_interval_sd_ms present',
    len(present_sd_pts) > 0,
    f"{len(present_sd_pts)} points")
if not present_sd_pts:
    warn("client.present_interval_sd_ms missing — browser may not have posted trace events; "
         "check POST /v1/sessions/{id}/trace/events path (ST-04)")
for name, label in client_stage_required.items():
    points = series.get(name, [])
    chk(f'client stage: {label} ({name}) present', len(points) > 0, f"{len(points)} points")

# present_fps is optional (may be in lightweight browser telemetry, not a mandatory series field)
if present_fps_pts:
    pass  # nice to have, logged below
else:
    warn("client.present_fps series absent — expected from browser trace events (ST-04); "
         "harness will still PASS on the mandatory fields")

# ── C. Events: ≥1 abr.retarget AND ≥1 playout.changed ───────────────────────
events = b.get('events', [])
abr_retarget_events   = [e for e in events if e.get('type') == 'abr.retarget']
playout_changed_events = [e for e in events if e.get('type') == 'playout.changed']

events_required = shape != 'clean'
for label, found in (
    ('abr.retarget event (agent-sourced)', abr_retarget_events),
    ('playout.changed event (browser-sourced)', playout_changed_events),
):
    detail = f"{len(found)} found" + (
        f"; first: {found[0].get('payload', {})} at ts={found[0].get('ts_unix_ms')}"
        if found else '')
    if events_required:
        chk(f'≥1 {label}', len(found) >= 1, detail)
    else:
        chk(f'{label} optional for clean steady-state', True, detail)

if events_required and not abr_retarget_events:
    warn("abr.retarget absent — did ABR fire? Ensure QUASAR_ABR != 0, netem shaping was applied, "
         "and the agent is on ship/ST-08 code. Check: qstack logs quasar-node-agent | grep ABR")
if events_required and not playout_changed_events:
    warn("playout.changed absent — browser may not have posted it (ST-04 path) or adaptive playout "
         "controller (AS-05) didn't trigger in the measurement window. Try --shape moderate.")

# ── D. Clock: either measured or explicit {unmeasured:true} ──────────────────
clock = b.get('clock', {})
if isinstance(clock, dict):
    if 'unmeasured' in clock and clock['unmeasured'] is True:
        chk('clock: explicit {"unmeasured":true} (clock sync never succeeded — ST-05)',
            True, 'unmeasured=true is the correct sentinel (not null, not 0)')
    elif 'client_offset_ms' in clock and 'uncertainty_ms' in clock:
        offset_ms = clock['client_offset_ms']
        uncertainty_ms = clock['uncertainty_ms']
        chk('clock: measured offset with stated uncertainty (ST-05)',
            True,
            f"client_offset_ms={offset_ms:.2f}ms uncertainty={uncertainty_ms:.2f}ms "
            f"measured_at={clock.get('measured_at', 'n/a')}")
        chk('clock: uncertainty_ms is non-negative',
            uncertainty_ms >= 0,
            f"uncertainty={uncertainty_ms:.2f}ms")
    else:
        chk('clock: present and well-formed', False,
            f"unexpected shape: {clock}")
else:
    chk('clock: present as dict', False, f"got {type(clock).__name__}: {clock!r}")

# ── E. Classifier verdict ─────────────────────────────────────────────────────
classifier = b.get('classifier', {})
verdict = classifier.get('verdict', '')
evidence = classifier.get('evidence', [])

VALID_VERDICTS = {
    'likely_encoder_saturation',
    'likely_network_congestion',
    'likely_client_presentation_limit',
    'nominal',
    'indeterminate_client_hidden',
    'unknown',
}
chk('classifier verdict is one of the valid v1 verdicts (ST-06)',
    verdict in VALID_VERDICTS,
    f"verdict='{verdict}'")
chk('classifier evidence non-empty list (ST-06)',
    isinstance(evidence, list) and len(evidence) > 0,
    f"{len(evidence)} evidence item(s): {evidence[:2]}")

# ── Summary ───────────────────────────────────────────────────────────────────
print('')
print(f"  bundle assertions: {pass_count} PASS, {fail_count} FAIL")

# Exit code drives the outer script's final PASS/FAIL tally
sys.exit(0 if fail_count == 0 else 1)
PYEOF

rm -f "$BUNDLE_TMP"

if [ "$ASSERT_EXIT" -eq 0 ]; then
  pass "all bundle assertions PASS"
else
  fail "one or more bundle assertions FAIL (see above)"
fi

# ── 9. Collect agent metrics for the report ───────────────────────────────────
say "9. collect agent metrics"
AGENT_METRICS_JSON=$(curl -fs \
  "$API/v1/admin/sessions/$SID/metrics?source=agent&limit=15" \
  -H "Authorization: Bearer $ADMIN_TOK" 2>/dev/null || echo '{"items":[]}')

# ── 10. Assemble structured report ────────────────────────────────────────────
say "10. assemble report"
PW_JSON_TMP="/tmp/st-browser-$$.json"
AGENT_JSON_TMP="/tmp/st-agent-$$.json"
BUNDLE_JSON_TMP="/tmp/st-bundle2-$$.json"
printf '%s' "$BROWSER_JSON" > "$PW_JSON_TMP"
printf '%s' "$AGENT_METRICS_JSON" > "$AGENT_JSON_TMP"
printf '%s' "$BUNDLE_JSON" > "$BUNDLE_JSON_TMP"

python3 - \
  "$PW_JSON_TMP" "$AGENT_JSON_TMP" "$BUNDLE_JSON_TMP" \
  "$TS" "$RESOLVED_NAME" "$APP_ID" "$SID" "${HOST_ID:-unknown}" \
  "$SHAPE" "$SECS" "$WARMUP" "$H264_DECODED" "$ASSERT_EXIT" \
  > "$REPORT_FILE" <<'PYEOF'
import json, sys

pw_json_file     = sys.argv[1]
agent_json_file  = sys.argv[2]
bundle_json_file = sys.argv[3]
ts               = sys.argv[4]
app_name         = sys.argv[5]
app_id           = sys.argv[6]
session_id       = sys.argv[7]
host_id          = sys.argv[8]
shape            = sys.argv[9]
secs             = int(sys.argv[10])
warmup           = int(sys.argv[11])
h264_decoded     = sys.argv[12] == 'true'
bundle_assertions_passed = int(sys.argv[13]) == 0

with open(pw_json_file) as f:
    browser = json.load(f)
with open(agent_json_file) as f:
    agent_raw = json.load(f)
with open(bundle_json_file) as f:
    bundle = json.load(f)

# Agent metric summary from session_metrics endpoint
enc_ms_vals, fps_vals, bitrate_vals = [], [], []
for item in agent_raw.get('items', []):
    m = item.get('metrics', {})
    if isinstance(m, str): m = json.loads(m)
    if 'encode_ms'    in m: enc_ms_vals.append(m['encode_ms'])
    if 'fps'          in m: fps_vals.append(m['fps'])
    if 'bitrate_kbps' in m: bitrate_vals.append(m['bitrate_kbps'])

def avg(lst): return round(sum(lst)/len(lst), 2) if lst else None

lw = browser.get('lightweight') or {}
series = bundle.get('series', {})
events = bundle.get('events', [])

def values(name):
    return sorted(float(p['v']) for p in series.get(name, []) if isinstance(p.get('v'), (int, float)))
def percentile(vals, q):
    if not vals: return None
    return round(vals[min(len(vals)-1, max(0, int((len(vals)-1)*q + 0.5)))], 3)
def summary(name):
    vals = values(name)
    return {'points': len(vals), 'p50': percentile(vals, .50), 'p95': percentile(vals, .95),
            'min': round(vals[0], 3) if vals else None, 'max': round(vals[-1], 3) if vals else None}

abr_events     = [e for e in events if e.get('type') == 'abr.retarget']
playout_events = [e for e in events if e.get('type') == 'playout.changed']

report = {
    'meta': {
        'harness': 'ST-08',
        'timestamp': ts,
        'app_name': app_name,
        'app_id': app_id,
        'session_id': session_id,
        'host_id': host_id,
        'network_shape': shape,
        'measurement_window_secs': secs,
        'warmup_secs': warmup,
        'h264_decoded': h264_decoded,
        'bundle_assertions_passed': bundle_assertions_passed,
    },
    # Keystone 1: real H.264 browser decode
    'keystone_1_h264': h264_decoded,
    'keystone_2_bundle_passed': bundle_assertions_passed,
    # Keystone 2: diagnostic bundle passes all assertions
    'keystone_2_bundle': {
        'series_keys': list(series.keys()),
        'encode_ms_points':    len(series.get('encoder.encode_ms', [])),
        'fps_points':          len(series.get('encoder.fps', [])),
        'setpoint_points':     len(series.get('abr.setpoint_kbps', [])),
        'present_sd_points':   len(series.get('client.present_interval_sd_ms', [])),
        'abr_retarget_events': len(abr_events),
        'playout_changed_events': len(playout_events),
        'adaptation_events_required': shape != 'clean',
        'abr_retarget_first':  abr_events[0].get('payload') if abr_events else None,
        'playout_changed_first': playout_events[0].get('payload') if playout_events else None,
        'clock':               bundle.get('clock', {}),
        'classifier_verdict':  bundle.get('classifier', {}).get('verdict'),
        'classifier_evidence': bundle.get('classifier', {}).get('evidence', []),
    },
    'stage_budget': {name: summary(name) for name in [
        'source.fps', 'compositor.fps', 'compositor.pts_delta_p50_ms', 'compositor.pts_delta_p95_ms',
        'interpipe.queue_level_max', 'interpipe.queue_dwell_p50_ms',
        'interpipe.queue_dwell_p95_ms', 'interpipe.queue_drops',
        'encoder.encode_ms_p50', 'encoder.encode_ms_p95', 'encoder.fps',
        'rtp.fps', 'rtp.bitrate_kbps', 'transport.rtt_ms',
        'transport.jitter_buffer_ms', 'transport.packets_lost', 'client.decode_ms',
        'client.fps', 'client.frames_dropped', 'client.present_fps',
        'client.present_interval_sd_ms', 'client.present_interval_p95_ms',
        'client.freeze_count', 'client.display_refresh_hz', 'client.glass_to_glass_ms',
    ]},
    # Browser lightweight telemetry (from Playwright runner)
    'browser_lightweight': {
        'fps':                    lw.get('fps'),
        'rtt_ms':                 lw.get('rtt_ms'),
        'jitter_buffer_ms':       lw.get('jitter_buffer_ms'),
        'decode_ms':              lw.get('decode_ms'),
        'packets_lost':           lw.get('packets_lost'),
        'present_fps':            lw.get('present_fps'),
        'present_interval_sd_ms': lw.get('present_interval_sd_ms'),
        'resolution':             lw.get('resolution'),
    },
    # Negotiation/wire-timing evidence retained for abs-capture-time diagnosis.
    'browser_timing_diagnostics': browser.get('timing_diagnostics'),
    # Agent metrics from session_metrics endpoint (summary)
    'agent_metrics': {
        'encode_ms_mean':   avg(enc_ms_vals),
        'fps_mean':         avg(fps_vals),
        'bitrate_kbps_mean': avg(bitrate_vals),
        'sample_count':     len(agent_raw.get('items', [])),
    },
    'bounds': [
        'Observability v2 pipeline: agent→CP→DB→bundle (ST-02 to ST-06)',
        'Browser events posted via POST /v1/sessions/{id}/trace/events (ST-04)',
        'Clock alignment from ping/pong sync (ST-05); "unmeasured":true is valid',
        'Classifier is observational only — no automatic action (ST-06)',
        'ABR/playout events require netem shaping (--shape mild) to fire reliably',
        f"Measurement window: {warmup}s warmup + {secs}s measurement",
    ],
}
print(json.dumps(report, indent=2))
PYEOF

rm -f "$PW_JSON_TMP" "$AGENT_JSON_TMP" "$BUNDLE_JSON_TMP"
pass "report written: $REPORT_FILE"

# ── 11. Human summary ─────────────────────────────────────────────────────────
say "11. summary"
python3 - "$REPORT_FILE" <<'PYEOF'
import json, sys
r = json.load(open(sys.argv[1]))
meta = r['meta']
k2   = r['keystone_2_bundle']
ag   = r['agent_metrics']
lw   = r['browser_lightweight']
events_required = k2.get('adaptation_events_required', True)

col_g = '\033[0;32m'; col_r = '\033[0;31m'; col_c = '\033[0;36m'
col_y = '\033[0;33m'; col_x = '\033[0m'

def fmt(v, unit='ms'): return f"{v:.1f}{unit}" if v is not None else '--'
def yn(v): return (col_g + 'YES' + col_x) if v else (col_r + 'NO' + col_x)

clock = k2.get('clock', {})
if isinstance(clock, dict) and clock.get('unmeasured'):
    clock_str = '{"unmeasured":true}'
elif isinstance(clock, dict) and 'client_offset_ms' in clock:
    clock_str = f"offset={clock['client_offset_ms']:.2f}ms ±{clock['uncertainty_ms']:.2f}ms"
else:
    clock_str = str(clock)

verdict = k2.get('classifier_verdict') or '--'
evidence = k2.get('classifier_evidence') or []

lines = [
    '',
    '┌─ ST-08 Observability v2 Trace Validation Report ───────────────────────────',
    f"│ App:         {meta.get('app_name')}",
    f"│ Session:     {meta.get('session_id')}",
    f"│ Host:        {meta.get('host_id')}",
    f"│ Shape:       {meta.get('network_shape')}",
    f"│ H.264 dec:   {yn(meta.get('h264_decoded'))} (Keystone 1)",
    '├─ Diagnostic bundle — series ────────────────────────────────────────────────',
    f"│ encoder.encode_ms:          {k2['encode_ms_points']} points (agent series)",
    f"│ encoder.fps:                {k2['fps_points']} points",
    f"│ abr.setpoint_kbps:          {k2['setpoint_points']} points",
    f"│ client.present_interval_σ:  {k2['present_sd_points']} points (browser series)",
    '├─ Diagnostic bundle — events ────────────────────────────────────────────────',
    f"│ abr.retarget events:        {k2['abr_retarget_events']} "
        f"({'required: ' + yn(k2['abr_retarget_events'] >= 1) if events_required else 'optional (clean)'}); payload={k2.get('abr_retarget_first')}",
    f"│ playout.changed events:     {k2['playout_changed_events']} "
        f"({'required: ' + yn(k2['playout_changed_events'] >= 1) if events_required else 'optional (clean)'}); payload={k2.get('playout_changed_first')}",
    '├─ Diagnostic bundle — clock alignment ───────────────────────────────────────',
    f"│ clock:  {clock_str}",
    '├─ Diagnostic bundle — classifier ───────────────────────────────────────────',
    f"│ verdict:   {verdict}",
]
for ev in evidence[:4]:
    lines.append(f"│ evidence:  {ev}")
lines += [
    '├─ Browser lightweight telemetry ─────────────────────────────────────────────',
    f"│ fps:            {fmt(lw.get('fps'), 'fps')}   (agent mean: {fmt(ag.get('fps_mean'), 'fps')})",
    f"│ rtt:            {fmt(lw.get('rtt_ms'))}",
    f"│ jitter-buf:     {fmt(lw.get('jitter_buffer_ms'))}",
    f"│ present σ:      {fmt(lw.get('present_interval_sd_ms'))}",
    f"│ encode_ms mean: {fmt(ag.get('encode_ms_mean'))}  ({ag['sample_count']} samples from session_metrics)",
    f"│ resolution:     {lw.get('resolution') or '--'}",
    '├─ Keystones ─────────────────────────────────────────────────────────────────',
    f"│ K1 H.264 decode:         {yn(meta.get('h264_decoded'))}",
    f"│ K2 encode metrics:       {yn(k2['encode_ms_points'] > 0 and k2['fps_points'] > 0)}",
    f"│ K2 browser present σ:    {yn(k2['present_sd_points'] > 0)}",
    f"│ K2 abr.retarget event:   {yn(k2['abr_retarget_events'] >= 1) if events_required else 'N/A (clean)'}",
    f"│ K2 playout.changed event:{yn(k2['playout_changed_events'] >= 1) if events_required else 'N/A (clean)'}",
    f"│ K2 clock (measured|unmeasured): {yn(isinstance(clock, dict) and ('unmeasured' in clock or 'client_offset_ms' in clock))}",
    f"│ K2 classifier verdict:   {yn(verdict in {'likely_encoder_saturation','likely_network_congestion','likely_client_presentation_limit','nominal','indeterminate_client_hidden','unknown'})}",
    '├─ Bounds ────────────────────────────────────────────────────────────────────',
]
print('\n'.join(lines))
for b in r.get('bounds', []):
    print(f"│ • {b}")
print(f"└─ Report: {sys.argv[1]}")
PYEOF

# ── 12. Final verdict ─────────────────────────────────────────────────────────
say "final verdict"
echo ""
if [ "$ST_FAIL" -eq 0 ] && [ "$ASSERT_EXIT" -eq 0 ]; then
  printf "${col_green}RESULT: PASS — all ${ST_PASS} checks passed${col_reset}\n"
  printf "${col_green}        Observability v2 pipeline proven end-to-end on a real H.264 decode.${col_reset}\n"
  exit 0
else
  TOTAL_FAIL=$((ST_FAIL))
  if [ "$ASSERT_EXIT" -ne 0 ]; then
    TOTAL_FAIL=$((TOTAL_FAIL + 1))
  fi
  printf "${col_red}RESULT: FAIL — ${TOTAL_FAIL} check(s) failed (${ST_PASS} passed)${col_reset}\n"
  printf "${col_red}        See FAIL lines above. Check WARN lines for diagnostic hints.${col_reset}\n"
  exit 1
fi
