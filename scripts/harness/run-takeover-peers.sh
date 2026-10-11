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
#   takeover    a second browser opens a session the first is streaming
#   connecting  the same, while the first page has answered and not connected
#   reload      the first page navigates away and back in the same tab
#   samepeer    the streaming page asks for an ICE restart on its own socket
#   reattach    the streaming page replaces its signaling socket and keeps its peers
#
# Env:
#   PW_IMAGE    image with a real Chrome (H.264) and xvfb-run. Playwright's bundled
#               Chromium has no H.264 and will not decode this stream.
#   PW_MODULES  directory holding node_modules/playwright-core
#   CHROME      Chrome inside PW_IMAGE (default /usr/bin/google-chrome)
#   IMAGE       agent dev image (default quasar-agent-dev:latest)
#   PORT        signaling port on loopback (default 18535)
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
[ $# -gt 0 ] || set -- takeover connecting reload samepeer reattach

mkdir -p "$OUT"
trap 'docker rm -f "$HOST_NAME" >/dev/null 2>&1 || true' EXIT

failed=0
for scenario in "$@"; do
  docker rm -f "$HOST_NAME" >/dev/null 2>&1 || true
  # One session per scenario. Host networking: ICE runs between two containers.
  docker run -d --name "$HOST_NAME" --network host --ulimit core=0 \
    -v "$ROOT":/workspace -w /workspace/node-agent \
    ${CARGO_TARGET_VOLUME:+-v "$CARGO_TARGET_VOLUME":/target -e CARGO_TARGET_DIR=/target} \
    -e QUASAR_TAKEOVER_WS="127.0.0.1:$PORT" -e QUASAR_TAKEOVER_SECS=90 \
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
    -v "$ROOT/scripts/harness/takeover-peers":/w/peers:ro -w /w \
    "$PW_IMAGE" sh -c 'xvfb-run -a -s "-screen 0 1280x800x24" node peers/driver.mjs "$@"' _ \
    "$scenario" "ws://127.0.0.1:$PORT" "file:///w/peers/peer.html" \
    >"$OUT/$scenario.json" 2>"$OUT/$scenario.driver.log" || true
  docker logs -t "$HOST_NAME" >"$OUT/$scenario.host.log" 2>&1 || true
  # What the host did, next to what the pages saw.
  replaced="$(grep -c "HOST replaced" "$OUT/$scenario.host.log" || true)"
  errors="$(grep -c "HOST bus error" "$OUT/$scenario.host.log" || true)"
  if python3 - "$OUT/$scenario.json" "$scenario" "$replaced" "$errors" <<'PY'
import json, sys
path, scenario, replaced, errors = sys.argv[1:5]
try:
    checks = json.load(open(path))["checks"]
except Exception as e:
    print(f"FAIL — {scenario}: no driver result ({e})"); sys.exit(1)
# A new peer gets one new transport per PC; the same peer never does.
want = {"takeover": 2, "connecting": 2, "reload": 2}.get(scenario, 0)
checks[f"the host replaced {want} transports"] = int(replaced) == want
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
