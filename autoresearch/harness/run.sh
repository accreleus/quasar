#!/usr/bin/env bash
# Frozen autoresearch harness entry point. Run from the repo root:
#
#   bash autoresearch/harness/run.sh <label> [--only perf|polish|shots] [--guards] [--runs N]
#
# 1. builds web/ in node:22 (npm ci + vite build --manifest; the manifest flag is
#    the harness-local override, web/vite.config.ts is untouched),
# 2. serves the build over the static mock (server.mjs) and measures it inside the
#    Playwright image (measure.mjs) -> autoresearch/harness/out/<label>.json,
# 3. with --guards, runs `make test-web` (typecheck, vitest incl. the design lint,
#    production build) -> out/<label>.guards ("pass"/"fail").
# One container at a time.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
label=${1:?usage: run.sh <label> [--ref <kept-label>] [--only perf|polish|shots] [--guards] [--runs N]}; shift
only=""; guards=0; runs=7; ref=""
while [ $# -gt 0 ]; do
  case "$1" in
    --only) only=$2; shift 2 ;;
    --guards) guards=1; shift ;;
    --ref) ref=$2; shift 2 ;;
    --runs) runs=$2; shift 2 ;;
    *) echo "unknown arg $1" >&2; exit 2 ;;
  esac
done

PW_IMAGE=mcr.microsoft.com/playwright:v1.56.0-noble
H=autoresearch/harness
mkdir -p "$H/out"

# The operator-busy check (operator-local, optional): wait at most 20 minutes.
busy=${OWNER_BUSY:-$HOME/.config/quasar-lab/owner-busy.sh}
if [ -x "$busy" ]; then
  for _ in 1 2 3 4; do "$busy" >/dev/null 2>&1 || break; echo "operator busy; waiting 5 min" >&2; sleep 300; done
fi

[ -d "$H/node_modules/playwright" ] || docker run --rm -v "$PWD/$H":/h -w /h "$PW_IMAGE" \
  sh -c "npm install --no-audit --no-fund >/dev/null && chown -R $(id -u):$(id -g) /h"

docker volume create ar-web-nm >/dev/null
docker run --rm -v "$PWD":/w -v ar-web-nm:/w/web/node_modules -w /w/web node:22 sh -c \
  "npm ci --no-audit --no-fund >/dev/null 2>&1 && npx vite build --manifest --logLevel error && chown -R $(id -u):$(id -g) dist"

rm -rf "$H/out/$label.dist" && cp -r web/dist "$H/out/$label.dist"
ref_srv=""; ref_args=""
if [ -n "$ref" ]; then
  [ -d "$H/out/$ref.dist" ] || { echo "no build kept for ref $ref" >&2; exit 2; }
  ref_srv="node server.mjs --dist /w/$H/out/$ref.dist --port 4901 2>/dev/null &"
  ref_args="--ref-base http://127.0.0.1:4901"
fi

# Timing means nothing on a busy machine: wait (at most 10 min) for 1-min load < 4,
# and record the load the run started at.
for _ in $(seq 60); do awk '{exit !($1 < 4)}' /proc/loadavg && break; sleep 10; done
cut -d' ' -f1 /proc/loadavg >"$H/out/$label.load"

imp_args=(); imp_mount=()
IMP=${IMPECCABLE_BIN:-$HOME/.claude/skills/impeccable/scripts/bin/linux-x64/impeccable}
if [ -x "$IMP" ] && { [ -z "$only" ] || [ "$only" = polish ]; }; then
  imp_mount=(-v "$IMP":/opt/impeccable:ro); imp_args=(--impeccable /opt/impeccable)
fi
docker run --rm --ipc=host --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD":/w "${imp_mount[@]}" -w /w/$H "$PW_IMAGE" sh -c '
  export IMPECCABLE_BROWSER=/w/autoresearch/harness/chrome-nosandbox.sh
  node server.mjs --dist /w/web/dist --port 4900 2>out/'"$label"'.server.log &
  '"$ref_srv"'
  sleep 1
  node measure.mjs --dist /w/web/dist --out out/'"$label"'.json --runs '"$runs"' '"${only:+--only $only}"' '"${imp_args[*]}"' '"$ref_args"'
  status=$?
  mkdir -p out/'"$label"'.shots && [ -d out/shots ] && cp -r out/shots/. out/'"$label"'.shots/ && rm -rf out/shots
  exit $status'

if [ "$guards" = 1 ]; then
  if make test-web >"$H/out/$label.guards.log" 2>&1; then
    echo pass >"$H/out/$label.guards"
  else
    # A vitest-only failure (this machine is shared; tests time out under load) gets
    # ONE re-run of just the failing files. Typecheck/build/drift failures stay failures.
    log=$(sed 's/\x1b\[[0-9;]*m//g' "$H/out/$label.guards.log")
    files=$(printf '%s\n' "$log" | grep -oE '^ FAIL  src/[^ ]+' | awk '{print $2}' | sort -u | tr '\n' ' ')
    if printf '%s\n' "$log" | grep -q '^FAIL — web unit tests$' && [ -n "$files" ] && \
       docker run --rm -v "$PWD":/w -v ar-web-nm:/w/web/node_modules -w /w/web node:22 npx vitest run $files >>"$H/out/$label.guards.log" 2>&1; then
      echo "pass (flake re-run: $files)" >"$H/out/$label.guards"
    else
      echo fail >"$H/out/$label.guards"
    fi
  fi
  echo "guards: $(cat "$H/out/$label.guards") (log $H/out/$label.guards.log)"
fi
