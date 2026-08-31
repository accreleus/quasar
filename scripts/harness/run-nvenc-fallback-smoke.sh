#!/usr/bin/env bash
# scripts/harness/run-nvenc-fallback-smoke.sh — fallback-NVENC smoke suite (#271
# remainder item, absorbed from #266). NVIDIA hosts default to Vulkan encode
# (QUASAR_ENCODER=vulkan, QUASAR_VULKAN_H264/_HEVC/_AV1 all ON) since
# 2026-08-12. Setting QUASAR_VULKAN_H264=0 makes a session borrow the vendor
# HW path (`nvcudah264enc`) via pipeline::resolve_effective_encoder instead —
# that per-session fallback is what protects a host from #489 (an NVIDIA
# driver NVENC teardown use-after-free) when Vulkan itself is disabled or
# unavailable. Because Vulkan is the default, the fallback path can rot
# silently. This harness proves it still works: three GATES, all required —
#
#   G1  effective encoder — a real session with QUASAR_VULKAN_H264=0 on the
#       agent container really resolves to the vendor NVENC path
#       (nvcudah264enc or nvh264enc, depending on the GStreamer version — see
#       G1's own comment below), not vulkanh264enc.
#       Proven from the agent's own `probe-encoder` report AND from the
#       structured "codec fallback: ..." log line the pipeline emits for a
#       real launch (node-agent/src/session/pipeline.rs) — not by inference.
#   G2  live decode — a real Chrome-for-Testing (CFT) WebRTC peer connects
#       and decodes the fallback session (frame count grows, never freezes).
#       A pipeline that reaches `running` is not proof; a decoding client is.
#   G3  clean teardown — the session tears down to a confirmed terminal
#       state, the agent container is still running afterward with an
#       unchanged restart count (no crash-loop), and the agent log window
#       around teardown carries no SIGSEGV/libnvcuvid/GLib-CRITICAL (the
#       #489 signature — node-agent/src/session/nvenc_defer.rs).
#
# This harness does NOT flip QUASAR_VULKAN_H264 itself — same convention as
# scripts/harness/run-codec-validate.sh's QUASAR_CAPTURE_BITSTREAM gate: the operator
# restarts the agent container with QUASAR_VULKAN_H264=0 BEFORE running this
# (env vars are read once by the long-lived agent process; a docker exec
# cannot inject env into an already-running PID 1, so there is nothing safe
# for this script to flip on your behalf). G1 fails loudly, with the exact
# fix, if that precondition isn't met.
#
# Runs ON THE HOST WHERE CFT LIVES (same rule as run-codec-validate.sh /
# run-spt06-certify.sh / run-soak-profile.sh) — reuses whatever
# `qses provision` already bootstrapped at /tmp/cft + /tmp/t8-driver.
#
# Usage:
#   scripts/harness/run-nvenc-fallback-smoke.sh --app Steam
#   scripts/harness/run-nvenc-fallback-smoke.sh --app "Quasar Bench: Ball" --secs 90
#
# Options:
#   --app <name>            REQUIRED app name (substring match, case-insensitive)
#   --secs 60                measurement window for the decode gate (default 60)
#   --api https://localhost:18443   control-plane base URL
#   --profile <id>           optional stream/launch profile_id override
#   --cp <container>         control-plane container (default: deploy-quasar-control-plane-1)
#   --agent <container>      node-agent container (default: deploy-quasar-node-agent-1)
#   --keep                   do NOT teardown a FAILED cell's session (debugging)
#   --email/--pass/--user    explicit register+login instead of the dev-gated
#                             throwaway identity (POST /v1/dev/agent-session,
#                             requires QUASAR_DEV_AGENT_AUTH=1 on the stack)
#   --launch-timeout N        seconds to wait for a session to reach running (default 90)
#   --teardown-timeout N      seconds to wait for a confirmed teardown (default 60)
#   --chrome/--pw-dir/--driver   CFT conventions (defaults match run-codec-validate.sh)
#
# Precondition (operator step, not automated here):
#   set QUASAR_VULKAN_H264=0 in deploy/.env (or export it before
#   `docker compose up -d --force-recreate quasar-node-agent`) and restart the
#   agent container BEFORE invoking this script. Restore it afterward the same
#   way to go back to the Vulkan default.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/harness/lib/harness.sh
source "$ROOT/scripts/harness/lib/harness.sh"

# ── Defaults ─────────────────────────────────────────────────────────────
APP=""
SECS=60
API="https://localhost:18443"
PROFILE=""
CP_CONTAINER="deploy-quasar-control-plane-1"
AGENT_CONTAINER="deploy-quasar-node-agent-1"
KEEP=0
EMAIL=""
PASS_=""
USERNAME=""
LAUNCH_TIMEOUT="${QSES_LAUNCH_TIMEOUT:-90}"
TEARDOWN_TIMEOUT="${QSES_TEARDOWN_TIMEOUT:-60}"
CHROME="${CHROME:-/tmp/cft/chrome-linux64/chrome}"
PW_DIR="${PW_DIR:-/tmp/t8-driver}"
DRIVER="${DRIVER:-$ROOT/scripts/harness/peer-driver.mjs}"
PROBE_EVERY_MS=1000

usage() { sed -n '1,50p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --app) APP="$2"; shift 2 ;;
    --secs) SECS="$2"; shift 2 ;;
    --api) API="$2"; shift 2 ;;
    --profile) PROFILE="$2"; shift 2 ;;
    --cp) CP_CONTAINER="$2"; shift 2 ;;
    --agent) AGENT_CONTAINER="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    --email) EMAIL="$2"; shift 2 ;;
    --pass) PASS_="$2"; shift 2 ;;
    --user) USERNAME="$2"; shift 2 ;;
    --launch-timeout) LAUNCH_TIMEOUT="$2"; shift 2 ;;
    --teardown-timeout) TEARDOWN_TIMEOUT="$2"; shift 2 ;;
    --chrome) CHROME="$2"; shift 2 ;;
    --pw-dir) PW_DIR="$2"; shift 2 ;;
    --driver) DRIVER="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

harness_init "nvenc-fallback-smoke"
require curl python3 docker node

[ -n "$APP" ] || { fail "usage: --app is required (e.g. --app Steam)"; harness_report; }
[[ -x "$CHROME" ]] || { fail "CFT not found at $CHROME (run: qses provision)"; harness_report; }
[[ -d "$PW_DIR/node_modules/playwright-core" ]] || { fail "playwright-core missing in $PW_DIR (run: qses provision)"; harness_report; }
[[ -f "$DRIVER" ]] || { fail "driver not found: $DRIVER"; harness_report; }

harness_note "app" "$APP"
harness_note "secs" "$SECS"
harness_note "api" "$API"
harness_note "agent_container" "$AGENT_CONTAINER"

# ── G1a: confirm the precondition on the AGENT container's OWN live env
#    (read via /proc/1/environ — same technique run-codec-validate.sh uses
#    for QUASAR_CAPTURE_BITSTREAM — never this shell's env, which may be on a
#    different host in the normal remote-stack case) ─────────────────────
read_agent_env_var() {
  docker exec "$AGENT_CONTAINER" sh -c \
    "tr '\\0' '\\n' < /proc/1/environ | grep -m1 '^$1='" 2>/dev/null | cut -d= -f2- || true
}

VULKAN_H264_ENV="$(read_agent_env_var QUASAR_VULKAN_H264)"
case "$VULKAN_H264_ENV" in
  0|false|off)
    pass "precondition: QUASAR_VULKAN_H264=$VULKAN_H264_ENV on $AGENT_CONTAINER — Vulkan h264 disabled, fallback path is live"
    ;;
  *)
    fail "precondition: QUASAR_VULKAN_H264 on $AGENT_CONTAINER is '${VULKAN_H264_ENV:-<unset, defaults ON>}', not disabled. Set QUASAR_VULKAN_H264=0 in deploy/.env and restart the agent container (docker compose up -d --force-recreate quasar-node-agent), THEN re-run this harness. Not automated here — env vars are read once by the long-lived agent process, so a docker exec cannot inject this into the already-running PID 1."
    harness_report
    ;;
esac

RESTART_COUNT_BEFORE="$(docker inspect "$AGENT_CONTAINER" --format '{{.RestartCount}}' 2>/dev/null || echo -1)"
harness_note "agent_restart_count_before" "$RESTART_COUNT_BEFORE"

# ── G1b: probe-encoder — the SAME resolve_effective_encoder path a real
#    session builds through, no hand-typed gst-launch (probe-encoder rule,
#    see run-codec-validate.sh's own comment on why this matters). ────────
PROBE_OUT="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-probe.XXXXXX")"
if ! docker exec "$AGENT_CONTAINER" quasar-node-agent probe-encoder \
      --codec h264 --seconds 2 --json >"$PROBE_OUT" 2>"$PROBE_OUT.err"; then
  fail "G1: probe-encoder failed — $(tail -3 "$PROBE_OUT.err" 2>/dev/null | tr '\n' ' ')"
else
  PROBE_FACTORY=$(python3 -c 'import sys,json; print(json.load(open(sys.argv[1])).get("encoder_factory") or "-")' "$PROBE_OUT" 2>/dev/null) || PROBE_FACTORY="-"
  harness_note "g1_probe_encoder_factory" "$PROBE_FACTORY"
  # node-agent/src/session/pipeline/encoders.rs:360-370 — GStreamer >=1.26 (this
  # image is 1.28.4) renamed the CUDA NVENC element from nvcudah264enc back to
  # nvh264enc; the agent tries nvcudah264enc first (pre-1.26 naming), then
  # nvh264enc. Both are the real vendor CUDA/NVENC path — only vulkanh264enc
  # (or anything else) means the fallback did NOT engage.
  case "$PROBE_FACTORY" in
    nvcudah264enc|nvh264enc)
      pass "G1: probe-encoder resolves h264 to $PROBE_FACTORY (fallback path, not vulkanh264enc)"
      ;;
    *)
      fail "G1: probe-encoder resolved h264 to '$PROBE_FACTORY', expected nvcudah264enc or nvh264enc — the fallback path did NOT engage even though QUASAR_VULKAN_H264=$VULKAN_H264_ENV. Check for a missing nvcuda plugin / broken image (see the encoder-element-missing-fallback warn in agent logs)."
      ;;
  esac
fi

# ── auth (same shape as run-codec-validate.sh) ─────────────────────────
TOK=""
login() {
  if [ -n "$EMAIL" ]; then
    local reg_user="${USERNAME:-${EMAIL%%@*}}"
    curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/auth/register" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS_\",\"username\":\"$reg_user\"}" >/dev/null 2>&1 || true
    TOK=$(curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/auth/login" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS_\"}" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null) || TOK=""
    return
  fi
  local key="${QUASAR_DEV_AGENT_KEY:-}"
  if [ -z "$key" ]; then
    key=$(docker exec "$CP_CONTAINER" cat /run/quasar/dev-agent-key 2>/dev/null) || key=""
  fi
  if [ -z "$key" ]; then
    echo "FATAL: no dev-agent key — set QUASAR_DEV_AGENT_AUTH=1 on the stack (key lands in the CP log and /run/quasar/dev-agent-key), export QUASAR_DEV_AGENT_KEY, or pass --email/--pass" >&2
    TOK=""
    return
  fi
  local hdrfile
  hdrfile=$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-devkey-hdr.XXXXXX")
  chmod 600 "$hdrfile"
  printf 'X-Quasar-Dev-Key: %s\n' "$key" > "$hdrfile"
  TOK=$(curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/dev/agent-session" \
    -H @"$hdrfile" -H 'Content-Type: application/json' \
    -d '{"role":"user","ttl_seconds":28800}' \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null) || TOK=""
  rm -f "$hdrfile"
}

API_CURL_CODE_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-api-code.XXXXXX")"
api_code() { cat "$API_CURL_CODE_FILE" 2>/dev/null; }
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
  printf '%s' "$code" > "$API_CURL_CODE_FILE"
  echo "$body"
}

login
if [ -z "$TOK" ]; then
  fail "auth: could not mint a token (see FATAL above)"
  harness_report
fi
pass "auth: token minted"

# ── resolve app id ───────────────────────────────────────────────────────
APP_ID_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-appid.XXXXXX")"
resolve_app_id() {
  local body
  body=$(api_curl GET "/v1/apps")
  [ "$(api_code)" = "200" ] || return 1
  echo "$body" | QSES_APP="$APP" python3 -c '
import os, sys, json
name = os.environ["QSES_APP"].lower()
d = json.load(sys.stdin)
items = d.get("apps", d.get("items", []))
for a in items:
    if name in a["name"].lower():
        print(a["id"]); break
' > "$APP_ID_FILE"
}
resolve_app_id || true
APP_ID="$(cat "$APP_ID_FILE" 2>/dev/null)"
if [ -z "$APP_ID" ]; then
  fail "app resolve: no app matching '$APP' (HTTP $(api_code))"
  harness_report
fi
pass "app resolved: $APP_ID"
harness_note "app_id" "$APP_ID"

# ── session id travels via a FILE (soak-harness lesson — see run-codec-validate.sh) ─
LAUNCH_SID_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-sid.XXXXXX")"
launch_sid() { cat "$LAUNCH_SID_FILE" 2>/dev/null; }
LAUNCH_AT_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-launch-at.XXXXXX")"

launch_session() {
  local launch_json body sid tries st
  : > "$LAUNCH_SID_FILE"
  date -u +%Y-%m-%dT%H:%M:%SZ > "$LAUNCH_AT_FILE"
  launch_json="{\"app_id\":\"$APP_ID\",\"stream\":{\"codec\":\"h264\"}"
  [ -n "$PROFILE" ] && launch_json="$launch_json,\"profile_id\":\"$PROFILE\""
  launch_json="$launch_json}"

  body=$(api_curl POST "/v1/sessions" -H 'Content-Type: application/json' -d "$launch_json")
  if [ "$(api_code)" != "201" ]; then
    echo "    launch failed: HTTP $(api_code) — $body" >&2
    return 1
  fi
  sid=$(echo "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["session"]["id"])' 2>/dev/null) || sid=""
  [ -n "$sid" ] || { echo "    launch: no session id in 201 response" >&2; return 1; }
  printf '%s' "$sid" > "$LAUNCH_SID_FILE"

  tries=$(( (LAUNCH_TIMEOUT + 1) / 2 ))
  st=""
  for _ in $(seq 1 "$tries"); do
    sleep 2
    body=$(api_curl GET "/v1/sessions/$sid")
    [ "$(api_code)" = "200" ] || continue
    st=$(echo "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["session"]["state"])' 2>/dev/null) || st=""
    [ "$st" = "running" ] && break
    [ "$st" = "failed" ] && break
  done
  if [ "$st" != "running" ]; then
    echo "    session did not reach running (state=${st:-unknown})" >&2
    return 1
  fi
  SIG_URL_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-sig.XXXXXX")"
  body=$(api_curl POST "/v1/sessions/$sid/signaling-token")
  if [ "$(api_code)" != "201" ]; then
    echo "    signaling-token mint failed: HTTP $(api_code)" >&2
    : > "$SIG_URL_FILE"
    return 0
  fi
  echo "$body" | python3 -c '
import sys, json
sig = json.load(sys.stdin).get("signaling") or {}
print(sig.get("url", ""))
print(sig.get("token", ""))
' > "$SIG_URL_FILE" 2>/dev/null || : > "$SIG_URL_FILE"
  return 0
}

teardown_session() {
  local sid="$1" tries st body
  [ -n "$sid" ] || return 0
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

if ! launch_session; then
  fail "launch/reach-running failed"
  harness_report
fi
SID="$(launch_sid)"
LAUNCH_AT="$(cat "$LAUNCH_AT_FILE" 2>/dev/null)"
SIG_URL="$(sed -n '1p' "$SIG_URL_FILE" 2>/dev/null)"
SIG_TOKEN="$(sed -n '2p' "$SIG_URL_FILE" 2>/dev/null)"
harness_note "session_id" "$SID"
echo "    session=$SID"

if [ -z "$SIG_URL" ] || [ -z "$SIG_TOKEN" ]; then
  fail "session response carried no signaling.url/token"
  [ "$KEEP" = 1 ] || teardown_session "$SID" || true
  harness_report
fi

# ── G1c: grep the pipeline's OWN "codec fallback" log line for THIS launch
#    (node-agent/src/session/pipeline.rs) — independent structured evidence
#    that resolve_effective_encoder actually chose nvcudah264enc for this
#    session, not just that probe-encoder (a separate CLI invocation) agrees.
FALLBACK_LOG_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-launch-log.XXXXXX")"
docker logs "$AGENT_CONTAINER" --since "$LAUNCH_AT" 2>&1 | grep "codec fallback" > "$FALLBACK_LOG_FILE" || true
if grep -qE "element=(nvcudah264enc|nvh264enc)" "$FALLBACK_LOG_FILE"; then
  pass "G1: agent log for session $SID carries \"codec fallback ... element=<nvenc factory>\" ($(grep -cE "element=(nvcudah264enc|nvh264enc)" "$FALLBACK_LOG_FILE") line(s))"
  harness_note "g1_fallback_log_line" "$(head -1 "$FALLBACK_LOG_FILE")"
elif [ -s "$FALLBACK_LOG_FILE" ]; then
  fail "G1: agent log carries a 'codec fallback' line for this launch but it does not name nvcudah264enc/nvh264enc: $(head -1 "$FALLBACK_LOG_FILE")"
else
  fail "G1: no 'codec fallback' log line found in $AGENT_CONTAINER since $LAUNCH_AT for session $SID — cannot independently confirm the resolved encoder from agent logs (probe-encoder's own G1b result above is a separate, narrower signal)"
fi

# ── G2: live decode — CFT peer in HOLD mode, same freeze/monotonic check as
#    run-codec-validate.sh's live-decode gate. ─────────────────────────────
CAPTURE_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/nvenc-fallback-capture.XXXXXX")"
HOLD_OUT="$CAPTURE_WORKDIR/hold.jsonl"
: > "$HOLD_OUT"
set +e
SPA_URL="$API" SID="$SID" SIG_URL="$SIG_URL" SIG_TOKEN="$SIG_TOKEN" AUTH_TOKEN="$TOK" \
  APP_NAME="$APP" CHROME="$CHROME" PW_DIR="$PW_DIR" CONNECT_TIMEOUT_MS=45000 \
  node "$DRIVER" --hold "$SECS" --probe-every "$PROBE_EVERY_MS" --sid "$SID" \
  >"$HOLD_OUT" 2>"$CAPTURE_WORKDIR/hold.stderr"
DRIVER_RC=$?
set -e

if [ "$DRIVER_RC" != 0 ] || [ ! -s "$HOLD_OUT" ]; then
  fail "G2: CFT driver failed to connect/decode (rc=$DRIVER_RC) — see $CAPTURE_WORKDIR/hold.stderr"
else
  VERDICT_FILE="$CAPTURE_WORKDIR/verdict.json"
  python3 - "$HOLD_OUT" "$VERDICT_FILE" <<'PYEOF'
import sys, json

inf, outf = sys.argv[1], sys.argv[2]
samples = []
with open(inf) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            samples.append(json.loads(line))
        except ValueError:
            continue

result = {"samples": len(samples), "monotonic": True, "final_frames": 0,
          "freeze_count": 0, "ok": False, "reason": ""}
if not samples:
    result["reason"] = "no samples parsed from HOLD output"
else:
    last = -1
    for s in samples:
        f = s.get("totalVideoFrames", 0)
        if f < last:
            result["monotonic"] = False
        last = f
        if s.get("decodeFailed"):
            result["freeze_count"] += 1
    result["final_frames"] = last
    result["ok"] = result["monotonic"] and last > 0 and result["freeze_count"] == 0
    if not result["ok"]:
        reasons = []
        if not result["monotonic"]:
            reasons.append("totalVideoFrames not monotonic")
        if last <= 0:
            reasons.append("final frame count is 0")
        if result["freeze_count"] > 0:
            reasons.append(f"{result['freeze_count']} decodeFailed sample(s) (stalled decode/freeze)")
        result["reason"] = "; ".join(reasons)

with open(outf, "w") as f:
    json.dump(result, f)
print(json.dumps(result))
PYEOF
  V_OK=$(python3 -c "import json;print(json.load(open('$VERDICT_FILE'))['ok'])")
  V_FRAMES=$(python3 -c "import json;print(json.load(open('$VERDICT_FILE'))['final_frames'])")
  V_SAMPLES=$(python3 -c "import json;print(json.load(open('$VERDICT_FILE'))['samples'])")
  V_REASON=$(python3 -c "import json;print(json.load(open('$VERDICT_FILE'))['reason'])")

  harness_note "g2_final_frames" "$V_FRAMES"
  harness_note "g2_samples" "$V_SAMPLES"

  if [ "$V_OK" = "True" ]; then
    pass "G2: live decode on the fallback (vendor NVENC) path — $V_SAMPLES samples, frames→$V_FRAMES, monotonic, 0 freezes"
  else
    fail "G2: live decode — $V_REASON ($V_SAMPLES samples, frames→$V_FRAMES)"
  fi
fi

# ── G3: clean teardown ──────────────────────────────────────────────────
if [ "$KEEP" = 1 ]; then
  skip "G3: teardown — --keep set, leaving session $SID up for debugging"
else
  TEARDOWN_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if teardown_session "$SID"; then
    pass "G3a: session $SID reached a confirmed terminal state within ${TEARDOWN_TIMEOUT}s"
  else
    fail "G3a: teardown of $SID not confirmed within ${TEARDOWN_TIMEOUT}s"
  fi

  # give the deferred-teardown drain (nvenc_defer.rs) and any crash a moment
  # to actually happen before we inspect container state / logs.
  sleep 5

  RUNNING="$(docker inspect "$AGENT_CONTAINER" --format '{{.State.Running}}' 2>/dev/null || echo false)"
  RESTART_COUNT_AFTER="$(docker inspect "$AGENT_CONTAINER" --format '{{.RestartCount}}' 2>/dev/null || echo -1)"
  harness_note "agent_restart_count_after" "$RESTART_COUNT_AFTER"
  if [ "$RUNNING" = "true" ] && [ "$RESTART_COUNT_AFTER" = "$RESTART_COUNT_BEFORE" ]; then
    pass "G3b: $AGENT_CONTAINER still running post-teardown, restart count unchanged ($RESTART_COUNT_BEFORE) — no crash/crash-loop"
  else
    fail "G3b: $AGENT_CONTAINER health changed post-teardown (running=$RUNNING, restart count $RESTART_COUNT_BEFORE -> $RESTART_COUNT_AFTER) — looks like the #489 UAF or an equivalent crash on the fallback path"
  fi

  TEARDOWN_LOG_FILE="$(mktemp "${TMPDIR:-/tmp}/nvenc-fallback-teardown-log.XXXXXX")"
  docker logs "$AGENT_CONTAINER" --since "$TEARDOWN_AT" 2>&1 > "$TEARDOWN_LOG_FILE" || true
  if grep -qiE 'segv|libnvcuvid|glib-.*-critical|panicked at' "$TEARDOWN_LOG_FILE"; then
    fail "G3c: #489 signature (SIGSEGV/libnvcuvid/GLib-CRITICAL/panic) found in $AGENT_CONTAINER log during teardown window — $(grep -iE 'segv|libnvcuvid|glib-.*-critical|panicked at' "$TEARDOWN_LOG_FILE" | head -1)"
  else
    pass "G3c: no #489 signature (SIGSEGV/libnvcuvid/GLib-CRITICAL/panic) in $AGENT_CONTAINER log during teardown window"
  fi
fi

harness_report
