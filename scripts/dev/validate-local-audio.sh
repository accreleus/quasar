#!/usr/bin/env bash
#
# validate-local-audio.sh — one-command PASS/FAIL/SKIP validator for the Quasar
# local-audio (console-mode) PulseAudio sidecar. Replaces the ~15 manual probes
# used during the 2026-07-14 local-audio debugging session. Run ON the Tower host
# (or via qnv sh), e.g.:
#   qnv sh 'scripts/dev/validate-local-audio.sh'
#   qnv sh 'scripts/dev/validate-local-audio.sh --capture 5'
#
# Discovers the running pulse sidecar container (name prefix `quasar-pulse-`) and
# checks, per item:
#   1. session socket dir mode 0755 (not 0700), and NO cookie file is published
#      (paths: /run/quasar-agent/pulse-<sid>/{,native})
#   2. non-root auth works (uid 99 = nobody, gid 100 = users — the game-container
#      identity) against unix:<dir>/native. No PULSE_COOKIE: the socket grants
#      anonymous auth (`auth-anonymous=1`) — cookie auth was removed because
#      pressure-vessel (Proton/Steam) remaps the cookie and silently denies every
#      sandboxed client
#   3. sink quasar_output exists and is the default sink; default source is
#      quasar_mic_src (deliberate — default-source apps record the mic; the
#      agent's own capture pins device=quasar_output.monitor explicitly); the
#      microphone devices (sink quasar_mic + source quasar_mic_src) exist
#   4. zero "Denied access" lines in the sidecar's logs over the last 5 minutes
#   5. [optional --capture N] captures N seconds from quasar_output.monitor via
#      parec inside the sidecar and reports per-second max amplitude (measurement
#      only — never plays audio back)
#   6. node-agent container is running, and (SKIP if no console session is live)
#      has an open FD on an ALSA pcm device
#
# Exits non-zero if any check FAILs. A missing sidecar SKIPs sidecar-dependent
# checks rather than failing the whole run (there may legitimately be no console
# session active).
set -euo pipefail

CAPTURE_SECONDS=0
while [ $# -gt 0 ]; do
  case "$1" in
    --capture)
      CAPTURE_SECONDS="${2:?--capture requires a seconds value}"
      shift 2
      ;;
    --capture=*)
      CAPTURE_SECONDS="${1#*=}"
      shift
      ;;
    *)
      echo "usage: $0 [--capture N]" >&2
      exit 2
      ;;
  esac
done

FAIL_COUNT=0
pass() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
skip() { printf 'SKIP: %s\n' "$*"; }

pactl_sidecar() {
  # pactl_sidecar <native-sock> <args...>
  # No PULSE_COOKIE: the sidecar socket grants anonymous auth (auth-anonymous=1,
  # see node-agent/src/session/audio.rs). Cookie auth is long gone.
  local sock="$1"
  shift 1
  docker exec -u 99:100 -e HOME=/tmp "$PULSE_CONTAINER" \
    pactl -s "unix:${sock}" "$@"
}

PULSE_CONTAINER="$(docker ps --filter 'name=quasar-pulse-' --format '{{.Names}}' 2>/dev/null | head -n1 || true)"

if [ -z "$PULSE_CONTAINER" ]; then
  skip "no running quasar-pulse-* sidecar found — skipping all sidecar checks (no console session live?)"
  skip "session socket dir / cookie permissions (no sidecar)"
  skip "non-root pactl auth (no sidecar)"
  skip "sink/source defaults (no sidecar)"
  skip "sidecar log 'Denied access' scan (no sidecar)"
  if [ "$CAPTURE_SECONDS" -gt 0 ]; then
    skip "amplitude capture (no sidecar)"
  fi
else
  echo "found sidecar: ${PULSE_CONTAINER}"

  SID="${PULSE_CONTAINER#quasar-pulse-}"
  SOCK_DIR="/run/quasar-agent/pulse-${SID}"
  NATIVE_SOCK="${SOCK_DIR}/native"

  # --- 1. permissions ---
  DIR_MODE="$(docker exec "$PULSE_CONTAINER" stat -c '%a' "$SOCK_DIR" 2>/dev/null || echo "")"
  if [ "$DIR_MODE" = "755" ]; then
    pass "session socket dir ${SOCK_DIR} mode 0755"
  else
    fail "session socket dir ${SOCK_DIR} mode is '${DIR_MODE:-missing}', expected 0755"
  fi

  # The sidecar must publish NO cookie: the socket is anonymous-auth, and a
  # stray cookie would mean someone re-introduced the auth mode that breaks
  # every Proton/pressure-vessel client.
  if docker exec "$PULSE_CONTAINER" test -e "${SOCK_DIR}/pulse-cookie" 2>/dev/null; then
    fail "unexpected cookie at ${SOCK_DIR}/pulse-cookie (socket is auth-anonymous=1)"
  else
    pass "no cookie published (socket grants anonymous auth)"
  fi

  # --- 2. non-root auth ---
  if pactl_sidecar "$NATIVE_SOCK" info >/dev/null 2>&1; then
    pass "non-root (uid 99:100) pactl auth against unix:${NATIVE_SOCK}"
  else
    fail "non-root (uid 99:100) pactl auth against unix:${NATIVE_SOCK}"
  fi

  # --- 3. sink/source defaults + microphone devices ---
  SINK_LIST="$(pactl_sidecar "$NATIVE_SOCK" list short sinks 2>/dev/null || true)"
  if echo "$SINK_LIST" | grep -q 'quasar_output'; then
    pass "sink quasar_output exists"
  else
    fail "sink quasar_output not found"
  fi

  # Microphone capture: the agent plays decoded client mic audio into this sink.
  # Baked into the sidecar argv unconditionally, so it must exist on EVERY
  # session — silent unless that session negotiated a mic m-line.
  if echo "$SINK_LIST" | grep -q 'quasar_mic'; then
    pass "sink quasar_mic exists (microphone feed)"
  else
    fail "sink quasar_mic not found (microphone feed sink missing)"
  fi

  SOURCE_LIST="$(pactl_sidecar "$NATIVE_SOCK" list short sources 2>/dev/null || true)"
  # The remapped capture source the app container records from (PULSE_SOURCE).
  # It must be a real source, not just the monitor — Steam and many games hide
  # monitor-class sources in their microphone pickers.
  if echo "$SOURCE_LIST" | grep -q 'quasar_mic_src'; then
    pass "source quasar_mic_src exists (remapped microphone capture device)"
  else
    fail "source quasar_mic_src not found (module-remap-source missing)"
  fi

  DEFAULT_SINK="$(pactl_sidecar "$NATIVE_SOCK" get-default-sink 2>/dev/null || echo "")"
  if [ "$DEFAULT_SINK" = "quasar_output" ]; then
    pass "default sink is quasar_output"
  else
    fail "default sink is '${DEFAULT_SINK:-unknown}', expected quasar_output"
  fi

  # The default SOURCE is quasar_mic_src: a remap-source outranks monitors
  # regardless of load order (proven live 2026-08-02), and that is the desired
  # routing — an app reading the default source records the microphone. Nothing
  # on the capture side relies on the default: both agent pulsesrcs pin
  # `device=quasar_output.monitor` explicitly (the real regression guard).
  DEFAULT_SOURCE="$(pactl_sidecar "$NATIVE_SOCK" get-default-source 2>/dev/null || echo "")"
  if [ "$DEFAULT_SOURCE" = "quasar_mic_src" ]; then
    pass "default source is quasar_mic_src (mic routing for default-source apps)"
  else
    fail "default source is '${DEFAULT_SOURCE:-unknown}', expected quasar_mic_src"
  fi

  # --- 4. Denied access scan ---
  DENIED_COUNT="$(docker logs --since 5m "$PULSE_CONTAINER" 2>&1 | grep -c 'Denied access' || true)"
  DENIED_COUNT="${DENIED_COUNT:-0}"
  if [ "$DENIED_COUNT" -eq 0 ]; then
    pass "zero 'Denied access' lines in sidecar logs (last 5m)"
  else
    fail "${DENIED_COUNT} 'Denied access' line(s) in sidecar logs (last 5m)"
  fi

  # --- 5. optional amplitude capture (measurement only, no playback) ---
  if [ "$CAPTURE_SECONDS" -gt 0 ]; then
    echo "capturing ${CAPTURE_SECONDS}s from quasar_output.monitor (measurement only, no playback)..."
    CAPTURE_OUT="$(docker exec -u 99:100 -e HOME=/tmp "$PULSE_CONTAINER" \
      timeout "$((CAPTURE_SECONDS + 5))" python3 - "$CAPTURE_SECONDS" "$NATIVE_SOCK" <<'PYEOF' 2>/dev/null || true
import struct
import subprocess
import sys

seconds = int(sys.argv[1])
sock = sys.argv[2]
rate = 44100
channels = 2
bytes_per_sample = 2
chunk_bytes = rate * channels * bytes_per_sample

proc = subprocess.Popen(
    [
        "parec",
        "-s", f"unix:{sock}",
        "-d", "quasar_output.monitor",
        "--format=s16le",
        f"--rate={rate}",
        f"--channels={channels}",
        "--raw",
    ],
    stdout=subprocess.PIPE,
)

for sec in range(seconds):
    buf = b""
    while len(buf) < chunk_bytes:
        data = proc.stdout.read(chunk_bytes - len(buf))
        if not data:
            break
        buf += data
    if not buf:
        break
    n = len(buf) // 2
    samples = struct.unpack(f"<{n}h", buf[: n * 2])
    peak = max(abs(s) for s in samples) if samples else 0
    print(f"second {sec + 1}: max_amplitude={peak}")

proc.terminate()
PYEOF
)"
    if [ -n "$CAPTURE_OUT" ]; then
      echo "$CAPTURE_OUT"
      pass "captured ${CAPTURE_SECONDS}s amplitude report from quasar_output.monitor"
    else
      fail "amplitude capture from quasar_output.monitor produced no output"
    fi
  fi
fi

# --- 6. node-agent container + ALSA pcm FD (SKIP gracefully if no live session) ---
AGENT_CONTAINER="$(docker ps --filter 'name=quasar-node-agent' --format '{{.Names}}' 2>/dev/null | head -n1 || true)"
if [ -z "$AGENT_CONTAINER" ]; then
  fail "no running quasar-node-agent container found"
else
  pass "node-agent container running (${AGENT_CONTAINER})"

  AGENT_PID="$(docker inspect --format '{{.State.Pid}}' "$AGENT_CONTAINER")"
  PCM_FD_COUNT="$(find "/proc/${AGENT_PID}/fd" -lname '*snd/pcm*' 2>/dev/null | wc -l | tr -d ' ')"
  PCM_FD_COUNT="${PCM_FD_COUNT:-0}"
  if [ "$PCM_FD_COUNT" -gt 0 ]; then
    pass "node-agent (pid ${AGENT_PID}) has an open FD on an ALSA pcm device (console session live)"
  else
    skip "no ALSA pcm FD on node-agent (pid ${AGENT_PID}) — no console session currently live"
  fi
fi

echo "---"
if [ "$FAIL_COUNT" -eq 0 ]; then
  echo "RESULT: PASS (0 failures)"
  exit 0
else
  echo "RESULT: FAIL (${FAIL_COUNT} failures)"
  exit 1
fi
