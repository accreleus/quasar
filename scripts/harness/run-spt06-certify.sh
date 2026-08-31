#!/usr/bin/env bash
# SPT-06 — REAL encoder certification: a script-orchestrated bench that drives a
# live Chrome-for-Testing (CFT) WebRTC peer per (rung × bitrate) cell so frames
# actually flow + decode, then has the control plane measure true encode
# performance and derive a verdict.
#
# WHY a peer is mandatory: webrtcbin gates frame flow until a peer connects.
# Without one the encode pipeline stalls (fps≈0, ~1 metrics sample) and EVERY
# cell falsely reads `unsafe` — which mis-caps a perfectly capable host. This
# script runs the SAME CFT + playwright mechanism `qses` uses (the peer always
# lives on this host; the API may be local or a remote stack).
#
# Run this ON THE HOST WHERE CFT LIVES (the hermes dev/CI box). CFT is bootstrapped
# by `qses provision` at /tmp/cft + /tmp/t8-driver; this script reuses them.
#
# Usage:
#   ./scripts/harness/run-spt06-certify.sh <host_id> [api_base] [admin_token] [encoder] [opts]
#
# A CELL IS A RUNG × BITRATE (migration 0041). Encode cost is codec-dependent, so
# a certification verdict is keyed on the stream profile (rung) that was actually
# streamed, not on the launch profile the user picks. PROFILES therefore names
# LAUNCH profiles and the control plane expands each into its rungs; the returned
# cell plan is what this script iterates, so a multi-codec chain gets every codec
# measured instead of whichever one the bench happened to pick. Naming a rung id
# directly in PROFILES also works (certify one codec of one chain).
#
# Options (env or trailing KEY=VAL):
#   PROFILES="1080p60 720p60"   space-separated launch-profile (or rung) ids
#                                                             (default: 1080p60 720p60 720p30)
#   BITRATES="8000"             space-separated kbps           (default: 4000 6000 8000 12000)
#   WINDOW_SECS=30              measurement window per cell    (default: 30)
#   WARMUP_SECS=6              warmup skipped before the window (driver-side; default: 6)
#   GPU_INDEX=0                 gpu index to certify           (default: 0)
#   CHROME=/tmp/cft/chrome-linux64/chrome
#   PW_DIR=/tmp/t8-driver
#   DRIVER=scripts/harness/peer-driver.mjs
#
# Examples:
#   ./scripts/harness/run-spt06-certify.sh <uuid> http://localhost:8080 "$ADMIN" va
#   PROFILES=1080p60 BITRATES=8000 ./scripts/harness/run-spt06-certify.sh <uuid> http://localhost:8080 "$ADMIN" va
set -euo pipefail

HOST_ID="${1:?Usage: $0 <host_id> [api_base] [admin_token] [encoder]}"
API="${2:-http://localhost:8080}"
TOKEN="${3:-${QUASAR_ADMIN_TOKEN:-}}"
ENCODER="${4:-va}"
shift $(( $# < 4 ? $# : 4 )) || true
for kv in "$@"; do export "$kv"; done

: "${PROFILES:=1080p60 720p60 720p30}"
: "${BITRATES:=4000 6000 8000 12000}"
# Agent metrics heartbeat is ~5s, so a longer window => more samples for a robust
# p95. 35s nets ~6-7 post-warmup samples per cell.
: "${WINDOW_SECS:=35}"
: "${WARMUP_SECS:=6}"
: "${GPU_INDEX:=0}"
: "${CHROME:=/tmp/cft/chrome-linux64/chrome}"
: "${PW_DIR:=/tmp/t8-driver}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${DRIVER:=$SCRIPT_DIR/peer-driver.mjs}"

if [[ -z "$TOKEN" ]]; then
  echo "ERROR: admin token required (arg 3 or QUASAR_ADMIN_TOKEN env var)" >&2
  exit 1
fi
[[ -x "$CHROME" ]] || { echo "ERROR: CFT not found at $CHROME (run: qses provision)" >&2; exit 1; }
[[ -d "$PW_DIR/node_modules/playwright-core" ]] || { echo "ERROR: playwright-core missing in $PW_DIR (run: qses provision)" >&2; exit 1; }
[[ -f "$DRIVER" ]] || { echo "ERROR: driver not found: $DRIVER" >&2; exit 1; }

BEARER="Authorization: Bearer $TOKEN"
jget() { python3 -c "import sys,json; print(json.load(sys.stdin)$1)"; }

# --- open a script-driven run (reserves the one-per-host lock, returns the plan) ---
PROFILES_JSON=$(printf '%s\n' $PROFILES | python3 -c 'import sys,json;print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
BITRATES_JSON=$(printf '%s\n' $BITRATES | python3 -c 'import sys,json;print(json.dumps([int(l) for l in sys.stdin if l.strip()]))')

echo "==> Opening certification run: host=$HOST_ID encoder=$ENCODER gpu=$GPU_INDEX"
echo "    profiles=[$PROFILES]  bitrates=[$BITRATES]  window=${WINDOW_SECS}s"
RUN_RESP=$(curl -sf -X POST -H 'Content-Type: application/json' -H "$BEARER" \
  -d "{\"encoder\":\"$ENCODER\",\"gpu_index\":$GPU_INDEX,\"profiles\":$PROFILES_JSON,\"bitrates_kbps\":$BITRATES_JSON}" \
  "$API/v1/admin/hosts/$HOST_ID/encoder-certification/runs")
RUN_ID=$(echo "$RUN_RESP" | jget "['run_id']")
TOTAL=$(echo "$RUN_RESP" | jget "['total_pts']")
echo "    run_id=$RUN_ID  total_cells=$TOTAL"

# The CELL PLAN is authoritative (0041): the control plane expanded each launch
# profile into its rungs, so iterating PROFILES × BITRATES here would silently
# miss every non-h264 rung of a multi-codec chain. Emit one
# "<launch profile> <rung> <codec> <bitrate>" line per planned cell.
CELLS_TMP=$(mktemp)
printf '%s' "$RUN_RESP" | python3 -c '
import sys, json
for c in json.load(sys.stdin).get("cells", []):
    print(c["profile_id"], c["stream_profile_id"], c.get("codec", "h264"), c["bitrate_kbps"])
' > "$CELLS_TMP"

# Always close the run + print results on exit.
finish() {
  echo ""
  echo "==> Closing run $RUN_ID"
  curl -sf -X POST -H "$BEARER" \
    "$API/v1/admin/hosts/$HOST_ID/encoder-certification/runs/$RUN_ID/complete" >/dev/null 2>&1 || true
  echo ""
  echo "==> Certification results for host $HOST_ID:"
  CERT_RESP=$(curl -sf -H "$BEARER" "$API/v1/admin/hosts/$HOST_ID/encoder-certification" 2>/dev/null || true)
  [[ -n "${CERT_RESP// }" ]] || CERT_RESP='{}'
  CERT_TMP=$(mktemp)
  printf '%s' "$CERT_RESP" > "$CERT_TMP"
  python3 - "$CERT_TMP" <<'PYEOF'
import sys, json
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
except Exception:
    print("  (could not read cert results)"); sys.exit(0)
certs = data.get("certifications", [])
if not certs:
    print("  (no certification rows)"); sys.exit(0)
print(f"  {'Rung':<22} {'Bitrate':>9} {'Verdict':>8} {'P95 ms':>8} {'P50 ms':>8} {'FPS':>6} {'Drops':>7} {'N':>4}")
print(f"  {'-'*22} {'-'*9} {'-'*8} {'-'*8} {'-'*8} {'-'*6} {'-'*7} {'-'*4}")
for c in certs:
    rung = c.get("stream_profile_id") or c["profile_id"]
    print(f"  {rung:<22} {c['bitrate_kbps']:>8}k {c['verdict']:>8} "
          f"{c['encode_ms_p95']:>8.2f} {c['encode_ms_p50']:>8.2f} "
          f"{c['output_fps']:>6.1f} {c['drop_rate']:>7.3f} {c['sample_count']:>4}")
PYEOF
  rm -f "$CERT_TMP" "$CELLS_TMP"
}
trap finish EXIT

# --- drive each cell: launch → CFT peer drives frames → finalize -------------------
CELL=0
# stdin is FD 3 so the playwright driver inside the loop cannot swallow the plan.
while read -r prof rung codec br <&3; do
  [[ -n "$prof" ]] || continue
    CELL=$((CELL+1))
    echo ""
    echo "==> [$CELL/$TOTAL] cell rung=$rung codec=$codec profile=$prof bitrate=${br}k"

    LAUNCH=$(curl -sf -X POST -H 'Content-Type: application/json' -H "$BEARER" \
      -d "{\"gpu_index\":$GPU_INDEX,\"encoder\":\"$ENCODER\",\"profile_id\":\"$prof\",\"stream_profile_id\":\"$rung\",\"bitrate_kbps\":$br}" \
      "$API/v1/admin/hosts/$HOST_ID/encoder-certification/cells") || {
        echo "    launch FAILED (skipping cell)" >&2; continue; }
    SID=$(echo "$LAUNCH" | jget "['session_id']")
    SIG_URL=$(echo "$LAUNCH" | jget "['signaling_url']")
    SIG_TOKEN=$(echo "$LAUNCH" | jget "['signaling_token']")
    BUDGET=$(echo "$LAUNCH" | jget "['budget_ms']")
    echo "    session=$SID  budget=${BUDGET}ms  driving CFT peer for ${WINDOW_SECS}s..."

    # Drive a REAL CFT peer so frames flow + decode for the measurement window.
    # The driver holds the peer connected for SECS, then exits; agent metrics
    # accumulate in session_metrics meanwhile.
    DRES=$(SPA_URL=$API SID=$SID SIG_URL=$SIG_URL SIG_TOKEN=$SIG_TOKEN AUTH_TOKEN=$TOKEN \
      APP_NAME='Quasar Stream Diagnostics' CHROME=$CHROME PW_DIR=$PW_DIR \
      WARMUP=$WARMUP_SECS SECS=$WINDOW_SECS CONNECT_TIMEOUT_MS=45000 \
      node "$DRIVER" 2>/dev/null | tail -1 || echo '{}')
    echo "$DRES" | python3 -c 'import sys,json
try:
  d=json.load(sys.stdin); lw=d.get("lightweight") or {}
  print("    peer:", "FAIL "+str(d.get("error")) if "error" in d else
        "OK fps=%s res=%s rtt=%sms"%(lw.get("fps"),lw.get("resolution"),lw.get("rtt_ms")))
except Exception: print("    peer: (no result json)")' || true

    # Finalize: control plane reads agent metrics → DeriveVerdict → upsert → teardown.
    FIN=$(curl -s -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' -H "$BEARER" \
      -d "{\"gpu_index\":$GPU_INDEX,\"encoder\":\"$ENCODER\",\"profile_id\":\"$prof\",\"stream_profile_id\":\"$rung\",\"bitrate_kbps\":$br,\"run_id\":\"$RUN_ID\"}" \
      "$API/v1/admin/hosts/$HOST_ID/encoder-certification/cells/$SID/finalize")
    CODE=$(echo "$FIN" | tail -1); BODY=$(echo "$FIN" | sed '$d')
    if [[ "$CODE" == "200" ]]; then
      echo "$BODY" | python3 -c 'import sys,json
d=json.load(sys.stdin)
print("    VERDICT=%s  p95=%.2fms p50=%.2fms fps=%.1f drops=%.3f n=%d live_write=%s"%(
  d["verdict"],d["encode_ms_p95"],d["encode_ms_p50"],d["output_fps"],d["drop_rate"],
  d["sample_count"],d["live_write_stable"]))'
    else
      echo "    finalize HTTP $CODE — $BODY (no cert row written)" >&2
    fi
done 3< "$CELLS_TMP"
