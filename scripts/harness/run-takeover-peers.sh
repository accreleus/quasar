#!/usr/bin/env bash
# run-takeover-peers.sh — real Chrome pages against the agent's encode pipeline, no GPU
# host and no control plane (#535).
#
# The host is the `browser_host` test in node-agent (`session::pipeline::takeover_tests`):
# the production encode pipeline on a test source and software encode, behind a stand-in
# for the signaling relay (last attach wins, the displaced socket gets 4410). The pages are
# takeover-peers/peer.html, driven by takeover-peers/driver.mjs.
#
# What it shows that the in-process Rust tests cannot: a browser's own certificate, and
# the SCTP abort a page sends when it unloads.
#
# Usage:
#   PW_IMAGE=<image> PW_MODULES=<dir> bash scripts/harness/run-takeover-peers.sh [scenario...]
#
# Scenarios (default: all):
#   takeover     a second browser opens a session the first is streaming
#   connecting   the same, while the first page has answered and not connected
#   reload       the first page navigates away and back in the same tab
#   samepeer     the streaming page asks for an ICE restart on its own socket
#   reattach     the streaming page replaces its signaling socket and keeps its peers
#   spa-takeover the takeover again with the REAL web client (a built web/dist, /v1
#                mocked, microphone granted): input channel, microphone, tracks, banner
#   spa-restart  the same, with an ICE-restart request landing between the two offers
#
# Env:
#   PW_IMAGE    image with a real Chrome (H.264) and xvfb-run. Playwright's bundled
#               Chromium has no H.264 and will not decode this stream.
#   PW_MODULES  directory holding node_modules/playwright-core
#   CHROME      Chrome inside PW_IMAGE (default /usr/bin/google-chrome)
#   IMAGE       agent dev image (default quasar-agent-dev:latest)
#   PORT        signaling port on loopback (default 18535)
#   SPA_DIST    a built web/dist for the spa-* scenarios (default: built here with
#               node:22 into .diagnostics/takeover-peers/dist)
#
# Exit 0 = every check of every scenario passed. Output and logs go under
# .diagnostics/takeover-peers/.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="${IMAGE:-quasar-agent-dev:latest}"
PORT="${PORT:-18535}"
: "${PW_IMAGE:?set PW_IMAGE to an image with a real Chrome and xvfb-run}"
: "${PW_MODULES:?set PW_MODULES to a directory holding node_modules/playwright-core}"
OUT="$ROOT/.diagnostics/takeover-peers"
HOST_NAME="takeover-host-$PORT"
[ $# -gt 0 ] || set -- takeover connecting reload samepeer reattach spa-takeover spa-restart
SPA_DIST="${SPA_DIST:-$OUT/dist}"

# Built from a copy, so the worktree gets no node_modules.
build_spa() {
  [ -f "$SPA_DIST/index.html" ] && return 0
  docker run --rm -v "$ROOT":/src:ro -v "$OUT":/out node:22 sh -c '
    mkdir -p /b/deploy && cp -r /src/web /b/web && cp /src/deploy/enroll-host.sh /b/deploy/ &&
    cd /b/web && rm -rf node_modules dist && npm ci >/dev/null 2>&1 &&
    npx vite build --outDir /out/dist --emptyOutDir >/dev/null && chown -R '"$(id -u):$(id -g)"' /out/dist'
}

mkdir -p "$OUT"
trap 'docker rm -f "$HOST_NAME" >/dev/null 2>&1 || true' EXIT

failed=0
for scenario in "$@"; do
  docker rm -f "$HOST_NAME" >/dev/null 2>&1 || true
  host_env=()
  driver=(peers/driver.mjs "$scenario" "ws://127.0.0.1:$PORT" "file:///w/peers/peer.html")
  case "$scenario" in
    spa-*)
      if ! build_spa; then
        echo "FAIL — $scenario: the web client did not build"
        failed=1
        continue
      fi
      host_env=(-e QUASAR_TAKEOVER_MIC=1)
      [ "$scenario" = spa-restart ] && host_env+=(-e QUASAR_TAKEOVER_RESTART_BETWEEN=1)
      driver=(peers/driver-spa.mjs "${scenario#spa-}" "ws://127.0.0.1:$PORT" /w/dist)
      ;;
  esac
  # One session per scenario. Host networking: ICE runs between two containers.
  docker run -d --name "$HOST_NAME" --network host --ulimit core=0 \
    -v "$ROOT":/workspace -w /workspace/node-agent \
    ${CARGO_TARGET_VOLUME:+-v "$CARGO_TARGET_VOLUME":/target -e CARGO_TARGET_DIR=/target} \
    -e QUASAR_TAKEOVER_WS="127.0.0.1:$PORT" -e QUASAR_TAKEOVER_SECS=120 "${host_env[@]}" \
    "$IMAGE" cargo test -p quasar-node-agent --lib takeover_tests::browser_host -- --ignored --nocapture >/dev/null
  # To a file, then grep: `docker logs | grep -q` dies of SIGPIPE under pipefail.
  listening() { docker logs "$HOST_NAME" >"$OUT/$scenario.host.log" 2>&1; grep -q "HOST listening" "$OUT/$scenario.host.log"; }
  for _ in $(seq 1 600); do
    listening && break
    [ "$(docker inspect -f '{{.State.Running}}' "$HOST_NAME")" = true ] || break
    sleep 2
  done
  if ! listening; then
    echo "FAIL — $scenario: the browser host did not start (see $OUT/$scenario.host.log)"
    failed=1
    continue
  fi
  docker run --rm --network host -e CHROME="${CHROME:-/usr/bin/google-chrome}" \
    -v "$PW_MODULES/node_modules":/w/node_modules:ro \
    -v "$ROOT/scripts/harness/takeover-peers":/w/peers:ro -v "$SPA_DIST":/w/dist:ro -w /w \
    "$PW_IMAGE" sh -c 'xvfb-run -a -s "-screen 0 1280x800x24" node "$@"' _ "${driver[@]}" \
    >"$OUT/$scenario.json" 2>"$OUT/$scenario.driver.log" || true
  docker logs -t "$HOST_NAME" >"$OUT/$scenario.host.log" 2>&1 || true
  # What the host did, next to what the pages saw.
  replaced="$(grep -c "HOST replaced" "$OUT/$scenario.host.log" || true)"
  # Microphone RTP reaching the host on a transport built after the first takeover.
  mic_after="$(sed -n '/HOST replaced the audio transport/,$p' "$OUT/$scenario.host.log" | grep -c "microphone: inbound track linked" || true)"
  if python3 - "$OUT/$scenario.json" "$scenario" "$replaced" "$mic_after" "$OUT/$scenario.host.log" <<'PY'
import json, sys
path, scenario, replaced, mic_after, host_log = sys.argv[1:6]
try:
    result = json.load(open(path))
    checks = result["checks"]
except Exception as e:
    print(f"FAIL — {scenario}: no driver result ({e})"); sys.exit(1)
# Bus errors up to the end of the checks. After it the page that owns the session
# closes, and the host seeing that is the peer-disconnect rule, not a defect.
done = result.get("done", "9")
errors = sum(1 for line in open(host_log, errors="replace")
             if "HOST bus error" in line and line.split(" ", 1)[0] < done)
# A new peer gets one new transport per PC; the same peer never does. The spa
# scenarios end with a third page, a plain resume, taking over once more.
want = {"takeover": 2, "connecting": 2, "reload": 2, "spa-takeover": 4, "spa-restart": 4}.get(scenario, 0)
if scenario == "spa-restart":
    absorbed = sum(1 for line in open(host_log, errors="replace") if "before the transport's first offer" in line)
    checks["the restart request that beat the new offer was absorbed"] = absorbed >= 1
checks[f"the host replaced {want} transports"] = int(replaced) == want
if scenario.startswith("spa-"):
    checks["the microphone reaches the host on the new transport"] = int(mic_after) >= 1
if scenario != "reload":
    # On a reload the unloading page aborts SCTP on the old transport before the new
    # page attaches; that one is the page leaving, as it is today.
    checks["no error reached the session's bus"] = int(errors) == 0
bad = [name for name, ok in checks.items() if not ok]
for name, ok in checks.items():
    print(f"{'PASS' if ok else 'FAIL'} — {scenario}: {name}")
sys.exit(1 if bad else 0)
PY
  then :; else failed=1; fi
done
[ "$failed" = 0 ] && echo "RESULT status=ok target=takeover-peers" || echo "RESULT status=failed target=takeover-peers"
exit "$failed"
