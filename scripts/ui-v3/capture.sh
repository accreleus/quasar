#!/usr/bin/env bash
# scripts/ui-v3/capture.sh — re-runnable screenshot pass over every UI v3 route.
#
# Serves the working tree's SPA with vite, logs in as the operator, injects the
# bearer into the SPA's own localStorage keys, and drives headless Chrome over
# CDP to screenshot every route in scripts/ui-v3/routes.json at 1440x900.
# Optionally renders the design_handoff_v3 mocks with the same browser and the
# same viewport, so the two sets are pixel-comparable.
#
#   scripts/ui-v3/capture.sh                     # app + mock, default stack
#   scripts/ui-v3/capture.sh --mode app          # app only
#   scripts/ui-v3/capture.sh --only admin-audit  # one route (regex over route ids)
#   scripts/ui-v3/capture.sh --width 1024 --only admin-fleet-jobs   # a narrower viewport
#   scripts/ui-v3/capture.sh --base https://127.0.0.1:8443   # the deployed SPA
#   scripts/ui-v3/capture.sh --api https://<your-host-ip>:8443 --out /tmp/ui-v3-capture
#
# `--mode session` is the live half: it launches a REAL session through the
# SPA's own Play button, screenshots the loader phase by phase, proves the
# WebRTC path with getStats(), walks the HUD (panes, docks, content presets,
# hidden), ends the session through the HUD exit and captures the summary. It
# always tears the session down and leaves nothing active.
#
#   scripts/ui-v3/capture.sh --mode session --base https://127.0.0.1:8443 \
#     --out /tmp/ui-v3-capture --app "Quasar Bench: Ball"     # -> $OUT/session/
#   scripts/ui-v3/capture.sh --mode mock --only "^(loader|hud)-" \
#     --out /tmp/ui-v3-capture/session                        # -> $OUT/mock/
#
# Output: $OUT/{app,mock,session}/<id>.png (+ .full.png when the page is taller
# than the viewport), $OUT/manifest-{app,mock,session}.json (per-route console
# errors, failed requests, blank/still-loading flags; for session, the phase
# list, the getStats evidence and the teardown check), $OUT/{vite,chrome}.log.
#
# It starts vite, a TLS bridge and Chrome itself and kills only those three on
# exit. Nothing else on the box is touched, and no container is restarted.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
WEB="$ROOT/web"
CHROME="${CHROME:-$HOME/.hermes/chrome/root/opt/google/chrome/chrome}"

OUT="/tmp/ui-v3-capture"
API="https://127.0.0.1:8443"
# Empty = serve the working tree with vite. Set it (or --base) to capture a
# deployed build instead — the control plane serves the SPA on its own origin.
BASE="${BASE_URL:-}"
MODE="both"
ONLY=""
APP=""
KEEP=0
WIDTH=1440
HEIGHT=900

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) OUT="$2"; shift 2 ;;
    --api) API="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --mode) MODE="$2"; shift 2 ;;
    --only) ONLY="$2"; shift 2 ;;
    --app) APP="$2"; shift 2 ;;
    --email) LOGIN_EMAIL="$2"; shift 2 ;;
    --chrome) CHROME="$2"; shift 2 ;;
    --width) WIDTH="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,35p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[[ -x "$CHROME" ]] || { echo "chrome not found at $CHROME (override with --chrome)" >&2; exit 1; }
mkdir -p "$OUT"

# --- ports -------------------------------------------------------------------
free_port() { python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
}
VITE_PORT="$(free_port)"
BRIDGE_PORT="$(free_port)"
CDP_PORT="$(free_port)"

VITE_PID=""; BRIDGE_PID=""; CHROME_PID=""; PROFILE=""
cleanup() {
  # Only ever the three processes this script started.
  for pid in "$CHROME_PID" "$VITE_PID" "$BRIDGE_PID"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  sleep 1
  [[ -n "$PROFILE" && -d "$PROFILE" ]] && rm -rf "$PROFILE" 2>/dev/null
  return 0
}
trap cleanup EXIT

wait_http() { # url attempts
  for _ in $(seq 1 "${2:-60}"); do
    curl -sk -o /dev/null "$1" && return 0
    sleep 0.5
  done
  return 1
}

# --- control-plane login -----------------------------------------------------
# Credentials default to deploy/.env (BOOTSTRAP_ADMIN_*); QUASAR_EMAIL /
# QUASAR_PASSWORD (or --email) override them, for a stack whose operator account
# is not the bootstrap one. The token is written to a 0600 file under $OUT, and
# neither the password nor the token is ever echoed.
EMAIL="${LOGIN_EMAIL:-${QUASAR_EMAIL:-$(grep -E '^BOOTSTRAP_ADMIN_EMAIL=' "$ROOT/deploy/.env" | tail -1 | cut -d= -f2-)}}"
PASSWORD="${QUASAR_PASSWORD:-$(grep -E '^BOOTSTRAP_ADMIN_PASSWORD=' "$ROOT/deploy/.env" | tail -1 | cut -d= -f2-)}"
[[ -n "$EMAIL" && -n "$PASSWORD" ]] || { echo "BOOTSTRAP_ADMIN_EMAIL/PASSWORD missing from $ROOT/deploy/.env" >&2; exit 1; }

TOKEN_FILE="$OUT/.token"
if [[ "$MODE" != "mock" ]]; then
  (umask 077; : > "$TOKEN_FILE")
  curl -sk -X POST "$API/v1/auth/login" -H 'content-type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "$EMAIL" "$PASSWORD")" \
    | python3 -c 'import sys, json; d = json.load(sys.stdin); sys.stdout.write(d["access_token"])' > "$TOKEN_FILE" \
    || { echo "login against $API failed" >&2; exit 1; }
  [[ -s "$TOKEN_FILE" ]] || { echo "login against $API returned no access_token" >&2; exit 1; }
  echo "logged in as $EMAIL at $API"
fi

# --- servers -----------------------------------------------------------------
if [[ "$MODE" != "mock" ]]; then
  # vite cannot proxy to a self-signed HTTPS origin (its proxy hard-sets
  # rejectUnauthorized), so /v1 goes through a plain-HTTP bridge instead of
  # editing web/vite.config.ts.
  node "$HERE/tlsbridge.mjs" --port "$BRIDGE_PORT" --target "$API" >"$OUT/bridge.log" 2>&1 &
  BRIDGE_PID=$!
  wait_http "http://127.0.0.1:$BRIDGE_PORT/health" 40 || { echo "tls bridge did not come up; see $OUT/bridge.log" >&2; exit 1; }

  # node on vite's own bin, not npx: npx forks a shell that forks node, and
  # killing the wrapper leaves the dev server running on exit.
  if [[ -z "$BASE" ]]; then
  ( cd "$WEB" && QUASAR_CONTROL_ORIGIN="http://127.0.0.1:$BRIDGE_PORT" \
      exec node "$WEB/node_modules/vite/bin/vite.js" --port "$VITE_PORT" --strictPort --host 127.0.0.1 \
      >"$OUT/vite.log" 2>&1 ) &
  VITE_PID=$!
  wait_http "http://127.0.0.1:$VITE_PORT/" 80 || { echo "vite did not come up; see $OUT/vite.log" >&2; exit 1; }
  BASE="http://127.0.0.1:$VITE_PORT"
  echo "vite on $BASE (api -> $API via bridge :$BRIDGE_PORT)"
  else
    wait_http "$BASE/" 20 || { echo "$BASE is not serving" >&2; exit 1; }
    echo "capturing the build served at $BASE (api reads via bridge :$BRIDGE_PORT)"
  fi
fi

# --- browser -----------------------------------------------------------------
# No --disable-gpu: the v3 glass surfaces (backdrop-filter) need the compositor.
PROFILE="$(mktemp -d /tmp/ui-v3-chrome-XXXXXX)"
# The session pass needs a page that can start playing on its own and can be
# granted a microphone without a prompt; neither flag changes how anything
# renders, so they are harmless on the route passes too.
MEDIA_FLAGS=(--autoplay-policy=no-user-gesture-required
             --use-fake-ui-for-media-stream
             --use-fake-device-for-media-stream)
"$CHROME" \
  --headless=new \
  --no-sandbox \
  --hide-scrollbars \
  --window-size="$WIDTH,$HEIGHT" \
  --force-device-scale-factor=1 \
  --remote-debugging-port="$CDP_PORT" \
  --remote-allow-origins='*' \
  --user-data-dir="$PROFILE" \
  --no-first-run --no-default-browser-check --disable-background-networking \
  --allow-file-access-from-files \
  --ignore-certificate-errors \
  --enable-logging=stderr --v=0 \
  "${MEDIA_FLAGS[@]}" \
  about:blank >"$OUT/chrome.log" 2>&1 &
CHROME_PID=$!
wait_http "http://127.0.0.1:$CDP_PORT/json/version" 40 || { echo "chrome devtools did not come up; see $OUT/chrome.log" >&2; exit 1; }

run_mode() {
  local mode="$1"; shift
  echo "--- capturing $mode"
  node "$HERE/capture.mjs" \
    --cdp "http://127.0.0.1:$CDP_PORT" \
    --out "$OUT" \
    --mode "$mode" \
    --base "$BASE" \
    --api "http://127.0.0.1:$BRIDGE_PORT" \
    --token-file "$TOKEN_FILE" \
    --mock-dir "$ROOT/design_handoff_v3/screens" \
    --routes "$HERE/routes.json" \
    --width "$WIDTH" --height "$HEIGHT" \
    ${APP:+--app "$APP"} \
    ${ONLY:+--only "$ONLY"}
}

if [[ "$MODE" == "app" || "$MODE" == "both" ]]; then run_mode app; fi
if [[ "$MODE" == "mock" || "$MODE" == "both" ]]; then run_mode mock; fi
if [[ "$MODE" == "session" ]]; then run_mode session; fi

if [[ "$KEEP" == 1 ]]; then
  echo "--keep: vite pid $VITE_PID on :$VITE_PORT, chrome pid $CHROME_PID on :$CDP_PORT — kill them yourself"
  trap - EXIT
fi

echo "done. evidence in $OUT"
