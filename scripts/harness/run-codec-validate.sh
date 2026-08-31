#!/usr/bin/env bash
# scripts/harness/run-codec-validate.sh — M2 codec-parameterised validation harness
# (vulkanav1enc campaign, spec docs/design/plans/2026-08-21-vulkanav1enc-implementation-spec.md §7,
# G3/G4/G8). Doubles as the h264/h265 regression net: run it against every
# armed codec after ANY encode-path change, not just AV1 work.
#
# Per codec cell:
#   1. launch a session with a forced codec override ("stream":{"codec":<c>})
#   2. drive a REAL Chrome-for-Testing (CFT) WebRTC peer against it for --secs
#      seconds in the driver's HOLD mode (scripts/harness/peer-driver.mjs --hold),
#      which polls totalVideoFrames every second and flags a stalled decode
#      (frame counter frozen >3s) as decodeFailed — this harness reuses that
#      output rather than reimplementing freeze detection.
#      PASS requires: totalVideoFrames non-decreasing across every sample,
#      final count > 0, and zero decodeFailed=true samples (G3).
#   3. decoderImplementation (NullVideoDecoder check): the driver does not
#      currently expose it — SKIPped with an explicit note, never faked.
#   4. IF the node-agent container was launched with QUASAR_CAPTURE_BITSTREAM
#      (or the legacy QUASAR_CAPTURE_H264 alias) set BEFORE this session was
#      created, its bitstream-capture pad probe
#      (node-agent/src/session/pipeline/probes.rs::attach_bitstream_capture)
#      writes the raw encoded stream to that path inside the container. This
#      harness copies the file out and, only if it actually appeared, runs a
#      strict decode (`ffmpeg -err_detect +explode`) requiring the file
#      decode cleanly end-to-end (G4). ffmpeg is not assumed present anywhere
#      — it runs inside `quasar-agent-dev:latest` (built via scripts/dev/dev.sh image),
#      and if that image lacks ffmpeg the sub-check SKIPs with an explicit
#      note instead of silently passing.
#
#   h265 is validated ONLY when explicitly listed in --codecs, and even then
#   its cell SKIPs (not fails, not silently excluded) — Chrome-on-Linux has no
#   HEVC hardware decoder, so a real browser peer can never confirm decode for
#   it; this is a browser limitation, not a signal about the encode path.
#
# Runs ON THE HOST WHERE CFT LIVES (the same rule scripts/harness/run-spt06-certify.sh
# and scripts/harness/run-soak-profile.sh follow) — reuses whatever `qses provision`
# already bootstrapped at /tmp/cft + /tmp/t8-driver, and talks to docker
# directly (no ssh hop of its own).
#
# Usage:
#   scripts/harness/run-codec-validate.sh --app Steam [options...]
#
# Options:
#   --app <name>            REQUIRED app name (substring match, case-insensitive)
#   --codecs h264,av1       comma list of h264|h265|av1 (default: h264,av1)
#   --secs 60               measurement window per codec cell (default 60)
#   --api https://localhost:18443   control-plane base URL
#   --profile <id>          optional stream/launch profile_id override
#   --cp <container>        control-plane container (default: deploy-quasar-control-plane-1)
#   --agent <container>     node-agent container (default: deploy-quasar-node-agent-1)
#   --dev-image <tag>       image ffmpeg runs in for the strict decode gate (default: quasar-agent-dev:latest)
#   --keep                  do NOT teardown a FAILED cell's session (debugging)
#   --email/--pass/--user   explicit register+login instead of the dev-gated
#                           throwaway identity (POST /v1/dev/agent-session,
#                           requires QUASAR_DEV_AGENT_AUTH=1 on the stack)
#   --launch-timeout N      seconds to wait for a session to reach running (default 90)
#   --teardown-timeout N    seconds to wait for a confirmed teardown (default 60)
#   --chrome/--pw-dir/--driver   CFT conventions (defaults match run-spt06-certify.sh)
#
# Enabling the ffmpeg strict-decode gate (G4):
#   set QUASAR_CAPTURE_BITSTREAM=/tmp/quasar-capture-<codec>.bin (any writable
#   in-container path) on the node-agent CONTAINER's environment (compose env,
#   or an admin host override) BEFORE this harness launches a session, and
#   restart the agent so it picks it up. This harness does not set the env
#   itself — see the harness-NOTES.md doc note (destined for
#   docs/configuration.md) for the full knob writeup.
#
# Examples:
#   scripts/harness/run-codec-validate.sh --app Steam
#   scripts/harness/run-codec-validate.sh --app Steam --codecs h264,h265,av1 --secs 120
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/harness/lib/harness.sh
source "$ROOT/scripts/harness/lib/harness.sh"

# ── Defaults ─────────────────────────────────────────────────────────────
APP=""
CODECS="h264,av1"
SECS=60
API="https://localhost:18443"
PROFILE=""
CP_CONTAINER="deploy-quasar-control-plane-1"
AGENT_CONTAINER="deploy-quasar-node-agent-1"
DEV_IMAGE="quasar-agent-dev:latest"
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

usage() { sed -n '1,66p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --app) APP="$2"; shift 2 ;;
    --codecs) CODECS="$2"; shift 2 ;;
    --secs) SECS="$2"; shift 2 ;;
    --api) API="$2"; shift 2 ;;
    --profile) PROFILE="$2"; shift 2 ;;
    --cp) CP_CONTAINER="$2"; shift 2 ;;
    --agent) AGENT_CONTAINER="$2"; shift 2 ;;
    --dev-image) DEV_IMAGE="$2"; shift 2 ;;
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

harness_init "codec-validate"
require curl python3 docker node

[ -n "$APP" ] || { fail "usage: --app is required (e.g. --app Steam)"; harness_report; }
[[ -x "$CHROME" ]] || { fail "CFT not found at $CHROME (run: qses provision)"; harness_report; }
[[ -d "$PW_DIR/node_modules/playwright-core" ]] || { fail "playwright-core missing in $PW_DIR (run: qses provision)"; harness_report; }
[[ -f "$DRIVER" ]] || { fail "driver not found: $DRIVER"; harness_report; }

harness_note "app" "$APP"
harness_note "codecs" "$CODECS"
harness_note "secs" "$SECS"
harness_note "api" "$API"

# ── auth (same shape as run-soak-profile.sh's login/api_curl — subshell-loss
#    lesson: HTTP status + session id travel via FILES, never via a variable
#    assigned inside a $()-captured function, see LAUNCH_SID_FILE below) ────
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
  hdrfile=$(mktemp "${TMPDIR:-/tmp}/codec-validate-devkey-hdr.XXXXXX")
  chmod 600 "$hdrfile"
  printf 'X-Quasar-Dev-Key: %s\n' "$key" > "$hdrfile"
  TOK=$(curl -k -fs --max-time 20 --connect-timeout 5 -X POST "$API/v1/dev/agent-session" \
    -H @"$hdrfile" -H 'Content-Type: application/json' \
    -d '{"role":"user","ttl_seconds":28800}' \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null) || TOK=""
  rm -f "$hdrfile"
}

API_CURL_CODE_FILE="$(mktemp "${TMPDIR:-/tmp}/codec-validate-api-code.XXXXXX")"
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
APP_ID_FILE="$(mktemp "${TMPDIR:-/tmp}/codec-validate-appid.XXXXXX")"
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

# ── session id travels via a FILE (soak-harness lesson: launch_session was
#    invoked as SID=$(launch_session ...); a variable set INSIDE that subshell
#    never reaches the caller, which silently orphaned a session every cycle
#    on Tower/2026-07-31. Same class as API_CURL_CODE_FILE above — never
#    reintroduce a "capture the id via command substitution from a function
#    that assigns a global" pattern in this file.) ──────────────────────────
LAUNCH_SID_FILE="$(mktemp "${TMPDIR:-/tmp}/codec-validate-sid.XXXXXX")"
launch_sid() { cat "$LAUNCH_SID_FILE" 2>/dev/null; }

# launch_codec_session <codec> — on success, writes the session id to
# LAUNCH_SID_FILE and returns 0; on failure returns 1 and leaves the file
# empty. Never uses `SID=$(launch_codec_session ...)`.
launch_codec_session() {
  local codec="$1" launch_json body sid tries st
  : > "$LAUNCH_SID_FILE"
  launch_json="{\"app_id\":\"$APP_ID\",\"stream\":{\"codec\":\"$codec\"}"
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
  # Signaling coordinates are NOT embedded in the session resource — mint a
  # fresh single-use envelope for the live session (the same flow a reconnecting
  # client uses): POST /v1/sessions/{id}/signaling-token -> SignalingEnvelope.
  SIG_URL_FILE="$(mktemp "${TMPDIR:-/tmp}/codec-validate-sig.XXXXXX")"
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

# teardown_session <sid> — best-effort; returns 0 only when a terminal state
# was CONFIRMED (404, or state stopped/failed). A DELETE answering 404 is not
# by itself proof (see soak-script lesson above) — poll and confirm.
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

# ── ffmpeg availability in the dev image — probed ONCE, cached to a file, so
#    every cell reuses the same verdict instead of re-probing (and so a SKIP
#    note is written exactly once). "docker run --rm" failing to even start
#    (image missing) is also treated as ffmpeg-absent, not a hard error — a
#    missing dev image just widens the SKIP, it doesn't crash the harness.
FFMPEG_PROBE_FILE="$(mktemp "${TMPDIR:-/tmp}/codec-validate-ffmpeg-probe.XXXXXX")"
probe_ffmpeg() {
  if docker run --rm "$DEV_IMAGE" sh -c 'command -v ffmpeg' >/dev/null 2>&1; then
    echo "yes" > "$FFMPEG_PROBE_FILE"
  else
    echo "no" > "$FFMPEG_PROBE_FILE"
  fi
}
probe_ffmpeg
FFMPEG_AVAILABLE="$(cat "$FFMPEG_PROBE_FILE" 2>/dev/null)"
if [ "$FFMPEG_AVAILABLE" != "yes" ]; then
  harness_note "ffmpeg_in_dev_image" "absent — G4 strict-decode sub-check will SKIP for every cell"
fi

CAPTURE_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/codec-validate-capture.XXXXXX")"

# resolve_capture_path — reads QUASAR_CAPTURE_BITSTREAM (falling back to the
# legacy QUASAR_CAPTURE_H264 alias) from the AGENT container's own live
# environment via /proc/1/environ, so the harness sees exactly what the
# running agent process was actually started with (not this shell's env,
# which is on a different host in the normal remote-stack case).
resolve_capture_path() {
  docker exec "$AGENT_CONTAINER" sh -c \
    "tr '\\0' '\\n' < /proc/1/environ | grep -m1 '^QUASAR_CAPTURE_BITSTREAM=' | cut -d= -f2- ; \
     tr '\\0' '\\n' < /proc/1/environ | grep -m1 '^QUASAR_CAPTURE_H264=' | cut -d= -f2-" \
    2>/dev/null | grep -v '^$' | head -1 || true
}

# ── per-codec cells ──────────────────────────────────────────────────────
OLDIFS="$IFS"; IFS=','
# shellcheck disable=SC2206
CODEC_LIST=($CODECS)
IFS="$OLDIFS"
[ "${#CODEC_LIST[@]}" -gt 0 ] || { fail "--codecs produced no tokens (got '$CODECS')"; harness_report; }

# ── negotiation pre-flight: `probe-encoder`, not a hand-typed gst-launch ──
#
# On 2026-08-22 a bare `gst-launch-1.0 ... ! vulkanh265enc ! ...` probe negotiated
# `profile=main-444` and the result was read as an NVIDIA driver regression. Production
# pins `profile=main` through the bitstream chain's capsfilter; the probe did not, because
# the harness and the product shared no code. `quasar-node-agent probe-encoder` builds the
# encoder branch through the SAME resolution, the SAME element builder and the SAME output
# capsfilter a session uses, so a disagreement between it and a live session is a real
# defect rather than a difference in how the two were typed.
#
# This runs for EVERY requested codec, including h265 — the encode side is exactly what a
# browser peer cannot tell us about (see the HEVC-headless rule in
# docs/testing-bench-mode.md).
probe_encoder_cell() {
  local codec="$1" out rc profile factory
  out="$CAPTURE_WORKDIR/$codec-probe-encoder.json"
  if ! docker exec "$AGENT_CONTAINER" quasar-node-agent probe-encoder \
        --codec "$codec" --seconds 2 --json >"$out" 2>"$out.err"; then
    rc=$?
    fail "$codec: probe-encoder failed (rc=$rc) — $(tail -3 "$out.err" 2>/dev/null | tr '\n' ' ')"
    return 1
  fi
  factory=$(python3 -c 'import sys,json; print(json.load(open(sys.argv[1])).get("encoder_factory") or "-")' "$out" 2>/dev/null) || factory="-"
  profile=$(python3 -c 'import sys,json; print(json.load(open(sys.argv[1])).get("profile") or "-")' "$out" 2>/dev/null) || profile="-"
  harness_note "${codec}_encoder_factory" "$factory"
  harness_note "${codec}_negotiated_profile" "$profile"
  # h264/h265 must pin a profile; av1 carries no profile caps field at all, which is
  # correct and not a finding.
  case "$codec" in
    h264|h265)
      if [ "$profile" = "-" ]; then
        fail "$codec: probe-encoder negotiated NO profile ($factory) — the output capsfilter did not pin one"
        return 1
      fi
      pass "$codec: encode branch negotiates profile=$profile on $factory"
      ;;
    av1)
      pass "$codec: encode branch negotiates on $factory (av1 has no profile caps field)"
      ;;
  esac
  return 0
}

for codec in "${CODEC_LIST[@]}"; do
  case "$codec" in
    h264|h265|av1) ;;
    *) fail "cell $codec: unknown codec (must be h264|h265|av1)"; continue ;;
  esac

  echo ""
  echo "==> cell codec=$codec"

  probe_encoder_cell "$codec" || true

  if [ "$codec" = "h265" ]; then
    skip "$codec: Chrome-on-Linux has no HEVC hardware decoder — a real browser peer can never confirm decode (browser limitation, not an encode-path signal). The encode side was checked by probe-encoder above."
    continue
  fi

  if ! launch_codec_session "$codec"; then
    fail "$codec: launch/reach-running failed"
    continue
  fi
  SID="$(launch_sid)"
  SIG_URL="$(sed -n '1p' "$SIG_URL_FILE" 2>/dev/null)"
  SIG_TOKEN="$(sed -n '2p' "$SIG_URL_FILE" 2>/dev/null)"
  echo "    session=$SID"

  if [ -z "$SIG_URL" ] || [ -z "$SIG_TOKEN" ]; then
    fail "$codec: session response carried no signaling.url/token"
    [ "$KEEP" = 1 ] || teardown_session "$SID" || true
    continue
  fi

  # Drive the CFT peer in HOLD mode: one JSON line per probe carrying
  # totalVideoFrames + decodeFailed. This is the SAME field the driver's
  # normal single-shot mode derives deltaFrames from at :828 — HOLD mode just
  # exposes the per-sample series instead of a single before/after delta, so
  # the freeze check does not need to reimplement anything.
  HOLD_OUT="$CAPTURE_WORKDIR/$codec-hold.jsonl"
  : > "$HOLD_OUT"
  set +e
  SPA_URL="$API" SID="$SID" SIG_URL="$SIG_URL" SIG_TOKEN="$SIG_TOKEN" AUTH_TOKEN="$TOK" \
    APP_NAME="$APP" CHROME="$CHROME" PW_DIR="$PW_DIR" CONNECT_TIMEOUT_MS=45000 \
    node "$DRIVER" --hold "$SECS" --probe-every "$PROBE_EVERY_MS" --sid "$SID" \
    >"$HOLD_OUT" 2>"$CAPTURE_WORKDIR/$codec-hold.stderr"
  DRIVER_RC=$?
  set -e

  if [ "$DRIVER_RC" != 0 ] || [ ! -s "$HOLD_OUT" ]; then
    fail "$codec: CFT driver failed to connect/decode (rc=$DRIVER_RC) — see $CAPTURE_WORKDIR/$codec-hold.stderr"
    [ "$KEEP" = 1 ] || teardown_session "$SID" || true
    continue
  fi

  VERDICT_FILE="$CAPTURE_WORKDIR/$codec-verdict.json"
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

  harness_note "${codec}_final_frames" "$V_FRAMES"
  harness_note "${codec}_samples" "$V_SAMPLES"

  if [ "$V_OK" = "True" ]; then
    pass "$codec: live decode — $V_SAMPLES samples, frames→$V_FRAMES, monotonic, 0 freezes"
  else
    fail "$codec: live decode — $V_REASON ($V_SAMPLES samples, frames→$V_FRAMES)"
  fi

  # decoderImplementation (NullVideoDecoder check, G3): the driver
  # (scripts/harness/peer-driver.mjs) does not currently surface a decoder
  # implementation string anywhere in its HOLD or single-shot JSON — not in
  # `lightweight`, not in the HOLD sample shape. Recorded as an explicit SKIP
  # rather than assumed/faked.
  skip "$codec: decoderImplementation check — driver does not expose it (see harness-NOTES.md); confirm manually via chrome://webrtc-internals if needed"

  # G4 strict decode gate, only if a capture actually appeared.
  CAP_PATH="$(resolve_capture_path)"
  if [ -z "$CAP_PATH" ]; then
    skip "$codec: strict-decode (G4) — QUASAR_CAPTURE_BITSTREAM/QUASAR_CAPTURE_H264 not set on $AGENT_CONTAINER; set it BEFORE launching to enable this sub-check"
  elif ! docker exec "$AGENT_CONTAINER" test -f "$CAP_PATH" 2>/dev/null; then
    skip "$codec: strict-decode (G4) — capture path $CAP_PATH configured but no file appeared (probe never attached or session produced no buffers)"
  elif [ "$FFMPEG_AVAILABLE" != "yes" ]; then
    skip "$codec: strict-decode (G4) — ffmpeg absent from $DEV_IMAGE; capture at $CAP_PATH left in place, not verified"
  else
    LOCAL_CAP="$CAPTURE_WORKDIR/$codec-capture.bin"
    if docker cp "$AGENT_CONTAINER:$CAP_PATH" "$LOCAL_CAP" 2>/dev/null && [ -s "$LOCAL_CAP" ]; then
      FFMPEG_LOG="$CAPTURE_WORKDIR/$codec-ffmpeg.log"
      if docker run --rm -v "$CAPTURE_WORKDIR:/cap" "$DEV_IMAGE" \
          ffmpeg -v error -err_detect +explode -i "/cap/$codec-capture.bin" -f null - \
          >"$FFMPEG_LOG" 2>&1; then
        pass "$codec: strict-decode (G4) — $CAP_PATH decoded 100% clean ($(wc -c <"$LOCAL_CAP") bytes)"
      else
        fail "$codec: strict-decode (G4) — ffmpeg -err_detect +explode rejected $CAP_PATH; see $FFMPEG_LOG"
      fi
    else
      fail "$codec: strict-decode (G4) — docker cp of $CAP_PATH from $AGENT_CONTAINER failed or produced an empty file"
    fi
  fi

  if [ "$V_OK" = "True" ] || [ "$KEEP" != 1 ]; then
    teardown_session "$SID" || echo "    WARNING: teardown of $SID not confirmed within ${TEARDOWN_TIMEOUT}s" >&2
  else
    echo "    --keep: leaving FAILED session $SID up for debugging" >&2
  fi
done

harness_report
