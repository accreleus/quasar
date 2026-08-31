#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/validate.sh — `make validate` orchestrator (release-readiness P1).
# Spec: docs/design/plans/2026-08-07-validation-harness-spec.md
#
#   scripts/dx/validate.sh [LEVEL=ui|api|session|all] [TARGET=local|<base-url>]
#                           [--reuse-web] [KEEP=1]
#
# LEVEL (default ui):
#   ui       structural UI journeys (user + admin) in a headless browser
#   api      OpenAPI conformance only — wraps scripts/harness/run-apitest.sh UNCHANGED
#   session  ui journeys + the real session-launch/decode journey (needs a live
#            GPU stack; refuses to run against TARGET=local — see
#            scripts/validate/journeys/session-launch.mjs)
#   all      api, then ui (+ session if TARGET is not local). Against
#            TARGET=local the session journey is not meaningful (no GPU host
#            in the ephemeral stack) — it is recorded as SKIPPED in the report
#            (report.json "skipped", report.html's Skipped section), never
#            silently dropped.
#
# TARGET (default local):
#   local            self-boots an ephemeral Postgres + control-plane (go run,
#                    the scripts/harness/run-apitest.sh recipe generalized) serving a
#                    freshly built web/dist, with QUASAR_DEV_AGENT_AUTH=1, and
#                    seeds one bench app via the admin API so the UI journeys
#                    have real data to assert on (not just empty-state chrome
#                    — see seed_bench_app). Fully self-cleaning (KEEP=1 leaves
#                    it up for debugging). Publishes the control-plane on a
#                    dx_free_port-chosen port, NOT the worktree's usual
#                    DX_CP_PORT — that port is what `docker-compose.local.yml`
#                    (the persistent dev stack, `make up`) also binds, and the
#                    two stacks can legitimately be up at the same time.
#   <base-url>       validate a live stack, e.g. https://tower.local:18443.
#                    Requires QUASAR_DEV_AGENT_AUTH=1 there and the dev key via
#                    $QUASAR_DEV_AGENT_KEY (no local boot, no teardown, no
#                    seeding — this harness does not own that stack's data).
#
# --reuse-web   reuse an existing web/dist instead of rebuilding it (inner-loop
#               speedup). Default is a FRESH build every run: a stale dist
#               silently invalidates every UI journey's result against
#               whatever commit built it, which is worse than the extra ~10s.
# KEEP=1        leave the local ephemeral stack running after the run (debug).
#
# Report: deploy/results/validate-<UTC ts>/{report.html,report.json,shots/}.
# Exit non-zero if any journey fails (the report is still written).
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET_NAME=validate

# `make validate ARGS='--reuse-web'` delivers ARGS by ENVIRONMENT, not
# interpolated into the recipe line (#550). Note that $TARGET here is the
# validation TARGET knob (local|<url>), not a dx target name — hence
# $TARGET_NAME for the guard.
[ $# -gt 0 ] || { dx_env_argv "$TARGET_NAME" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

usage() { sed -n '3,38p' "$0" | sed 's/^# \{0,1\}//'; }

LEVEL="${LEVEL:-ui}"
TARGET="${TARGET:-local}"
KEEP="${KEEP:-0}"
REUSE_WEB=0

while [ $# -gt 0 ]; do
  case "$1" in
    --reuse-web) REUSE_WEB=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET_NAME" "unknown arg '$1' — see: scripts/dx/validate.sh --help" ;;
  esac
done

case "$LEVEL" in
  ui|api|session|all) ;;
  *) dx_guard "$TARGET_NAME" "LEVEL must be ui|api|session|all (got '$LEVEL')" ;;
esac

RUN_TS="$(dx_timestamp)"
RESULTS_DIR="$DX_ROOT/deploy/results/validate-${RUN_TS}"
mkdir -p "$RESULTS_DIR"
STATE_DIR="$RESULTS_DIR/state"
mkdir -p "$STATE_DIR"

COMMIT="$(git -C "$DX_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"

dx_info "run dir: $RESULTS_DIR"
dx_info "level=$LEVEL target=$TARGET commit=$COMMIT"

# ── Local ephemeral stack state (only used when TARGET=local) ────────────────
NET="qval-net-${QUASAR_INSTANCE}"
PG="qval-pg-${QUASAR_INSTANCE}"
CP="qval-cp-${QUASAR_INSTANCE}"
GO_IMAGE="golang:1.26"
ADMIN_EMAIL="admin@quasar.local"
ADMIN_PASS="adminpassword123"
# Set the instant the network exists — every resource created after this point
# (postgres, the control-plane container) must be torn down on any exit path,
# INCLUDING one that fails mid-boot before that resource itself is confirmed
# up. Setting this only after the control-plane container started (as a
# previous revision did) left an orphaned network+postgres on every postgres-
# or control-plane-boot failure (MAJOR 4, adversarial review).
STACK_UP=0
RUN_STATE_DIR="$RESULTS_DIR/stack-run"
CP_PORT=""

teardown_local_stack() {
  [ "$STACK_UP" = "1" ] || return 0
  if [ "$KEEP" = "1" ]; then
    dx_info "KEEP=1 — leaving local stack up ($PG, $CP on $NET, port ${CP_PORT:-?})"
    return 0
  fi
  docker rm -f "$CP" "$PG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  # The container holding the key is gone, but the key FILE (a live secret
  # while the stack was up) is still sitting on disk under the results dir —
  # shred it once it can no longer authenticate anything.
  rm -f "$RUN_STATE_DIR/dev-agent-key" 2>/dev/null || true
}

cleanup() {
  teardown_local_stack
}
trap cleanup EXIT

# ── Web build (LEVEL != api needs a served web/dist) ──────────────────────────
node_major() {
  local v
  v="$(node --version 2>/dev/null | sed 's/^v//')"
  echo "${v%%.*}"
}

build_web() {
  local dist="$DX_ROOT/web/dist"
  # Default is a FRESH build every run (MAJOR 6, adversarial review): a stale
  # dist silently validates against whatever commit built it, not the commit
  # this run reports — and the report's "commit" field would then be a lie.
  # --reuse-web is the explicit, named opt-out for the inner loop.
  if [ "$REUSE_WEB" = "1" ] && [ -d "$dist" ] && [ -n "$(ls -A "$dist" 2>/dev/null)" ]; then
    dx_pass web-build "--reuse-web: existing web/dist reused (may not match commit $COMMIT)"
    return 0
  fi
  local start end elapsed
  start=$(date +%s)
  # npm ci, not npm install: install can rewrite package-lock.json (e.g. on a
  # dependency range bump upstream), silently mutating a committed lockfile as
  # a side effect of running a validation harness (MAJOR 6 / repo review #12).
  # ci is also the correct verb for a reproducible, from-lockfile build.
  if dx_have node && [ "$(node_major)" -ge 22 ] 2>/dev/null && dx_have npm; then
    dx_info "building web/dist with host node $(node --version)"
    ( cd "$DX_ROOT/web" && npm ci && npm run build )
  else
    dx_info "host node missing/too old — building web/dist in node:22"
    docker run --rm -v "$DX_ROOT/web":/w -w /w node:22 sh -c 'npm ci && npm run build'
  fi
  end=$(date +%s)
  elapsed=$((end - start))
  if [ -d "$dist" ] && [ -n "$(ls -A "$dist" 2>/dev/null)" ]; then
    dx_pass web-build "web/dist built in ${elapsed}s"
  else
    dx_fail web-build "web/dist is empty after build"
    dx_result "$TARGET_NAME"
  fi
}

# ── Boot the local ephemeral stack (pg + control-plane, WEB_ROOT + dev-auth) ──
boot_local_stack() {
  dx_have docker || { dx_fail docker "not on PATH — cannot boot the local stack"; dx_result "$TARGET_NAME"; }
  docker info >/dev/null 2>&1 || { dx_fail docker "daemon not reachable"; dx_result "$TARGET_NAME"; }

  build_web

  mkdir -p "$RUN_STATE_DIR"

  # dx_free_port, not DX_CP_PORT: DX_CP_PORT is this worktree's fixed port for
  # docker-compose.local.yml (`make up`), which may legitimately already be
  # running — this ephemeral stack must not fight it for the port (MAJOR 5,
  # adversarial review). Picked once and reused for both the container publish
  # and every health/API URL below.
  CP_PORT="$(dx_free_port "$DX_TESTDB_PORT_HINT")" || {
    dx_fail port "no free port found near $DX_TESTDB_PORT_HINT"
    dx_result "$TARGET_NAME"
  }
  dx_info "chosen control-plane port: 127.0.0.1:${CP_PORT}"

  docker network create "$NET" >/dev/null 2>&1 || true
  # From here on, ANY exit (including a failure a few lines down before the
  # control-plane container itself exists) must tear this down — set the flag
  # at the earliest point there is something to tear down, not after the last
  # container in the sequence starts (MAJOR 4).
  STACK_UP=1
  docker rm -f "$PG" "$CP" >/dev/null 2>&1 || true

  dx_info "[1/3] postgres"
  docker run -d --name "$PG" --network "$NET" \
    -e POSTGRES_USER=quasar -e POSTGRES_PASSWORD=quasar -e POSTGRES_DB=quasar \
    postgres:16 >/dev/null
  local ready=0
  for _ in $(seq 1 30); do
    if docker exec "$PG" pg_isready -U quasar >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  if [ "$ready" = "1" ]; then dx_pass postgres "ready"; else
    dx_fail postgres "never became ready — docker logs $PG"; dx_result "$TARGET_NAME"
  fi

  dx_info "[2/3] control-plane (go run, WEB_ROOT + dev-auth)"
  # RUN_STATE_DIR is bind-mounted at /run/quasar so the dev-agent key lands on
  # the host directly — no docker exec needed to read it (mint_identities
  # below passes it to agentcreds.sh via $QUASAR_DEV_AGENT_KEY, bypassing its
  # own docker-exec discovery, which assumes a compose service name this
  # ad-hoc stack does not have).
  docker run -d --name "$CP" --network "$NET" \
    -p "127.0.0.1:${CP_PORT}:8080" \
    -v "$DX_ROOT/control-plane":/w \
    -v "$DX_ROOT/web/dist":/app/web:ro \
    -v "$RUN_STATE_DIR":/run/quasar \
    -w /w \
    -e DATABASE_URL="postgres://quasar:quasar@$PG:5432/quasar?sslmode=disable" \
    -e ENROLLMENT_TOKEN=validate-enroll-token \
    -e BOOTSTRAP_ADMIN_EMAIL="$ADMIN_EMAIL" \
    -e BOOTSTRAP_ADMIN_USERNAME=admin \
    -e BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PASS" \
    -e REGISTRATION_MODE=open \
    -e LISTEN_ADDR=":8080" \
    -e QUASAR_TLS=off \
    -e QUASAR_WEB_ROOT=/app/web \
    -e QUASAR_DEV_AGENT_AUTH=1 \
    -e QUASAR_DEV_AGENT_KEY_PATH=/run/quasar/dev-agent-key \
    "$GO_IMAGE" sh -c 'go run ./cmd/quasar-control' >/dev/null

  dx_info "waiting for control-plane /health (compiling)..."
  local up=0
  for _ in $(seq 1 120); do
    if curl -sf -o /dev/null "http://127.0.0.1:${CP_PORT}/health" 2>/dev/null; then up=1; break; fi
    sleep 2
  done
  if [ "$up" != "1" ]; then
    dx_fail control-plane "never came up — logs:"
    docker logs "$CP" 2>&1 | tail -60 >&2 || true
    dx_result "$TARGET_NAME"
  fi
  dx_pass control-plane "up on 127.0.0.1:${CP_PORT}"

  dx_info "[3/3] waiting for dev-agent key file"
  local key_ready=0
  for _ in $(seq 1 30); do
    if [ -s "$RUN_STATE_DIR/dev-agent-key" ]; then key_ready=1; break; fi
    sleep 1
  done
  if [ "$key_ready" != "1" ]; then
    dx_fail dev-agent-key "$RUN_STATE_DIR/dev-agent-key never appeared — was QUASAR_DEV_AGENT_AUTH honored?"
    dx_result "$TARGET_NAME"
  fi
  dx_pass dev-agent-key "present"
}

# ── Seed one bench app (LOCAL stack only — see MAJOR 7, adversarial review) ──
# Table shells (thead, and even the `empty` message) render regardless of
# data, so a UI journey that only checks for a <table> or an <h1> passes just
# as well on a totally broken data fetch as on a real one. Seeding one known
# app via the real admin API — the same path an operator would use — gives
# admin-apps and user-login-library a real row/tile to assert on by name.
# Uses the freshly minted admin identity's own bearer token (from
# mint_identities' admin storage-state, not a separate bootstrap-admin login)
# so this exercises the same POST /v1/apps path #399-minted tooling already
# exists for. Keep BENCH_APP_NAME in sync with scripts/validate/lib/seed.mjs.
BENCH_APP_NAME="validate-bench"
seed_bench_app() {
  local base_url="$1" admin_state="$2"
  local admin_token
  admin_token="$(python3 - "$admin_state" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
for origin in data.get("origins", []):
    for kv in origin.get("localStorage", []):
        if kv["name"] == "quasar.auth.token":
            print(kv["value"])
            raise SystemExit(0)
PY
)"
  if [ -z "$admin_token" ]; then
    dx_fail seed-app "could not extract the admin bearer token from $admin_state"
    dx_result "$TARGET_NAME"
  fi

  local resp_file body http_code
  resp_file="$(mktemp)"
  body='{"name":"'"$BENCH_APP_NAME"'","description":"seeded by make validate — safe to delete","runtime_spec":{"image":"ghcr.io/games-on-whales/xfce:edge","args":[],"env":{},"gpu":true}}'
  # Token goes via a header file, same convention agentcreds.sh uses for the
  # dev key — never in argv.
  local hdr_file; hdr_file="$(mktemp)"; chmod 600 "$hdr_file"
  printf 'Authorization: Bearer %s\n' "$admin_token" > "$hdr_file"
  http_code="$(curl -sS -k -o "$resp_file" -w '%{http_code}' --max-time 10 \
    -X POST "$base_url/v1/apps" \
    -H @"$hdr_file" -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null || true)"
  rm -f "$hdr_file"
  if [ "$http_code" != "201" ]; then
    dx_fail seed-app "POST /v1/apps returned $http_code (want 201): $(cat "$resp_file")"
    rm -f "$resp_file"
    dx_result "$TARGET_NAME"
  fi
  rm -f "$resp_file"
  dx_pass seed-app "seeded '$BENCH_APP_NAME' via POST /v1/apps for the bench-data journeys"
}

# ── Mint identities ────────────────────────────────────────────────────────────
# The dev key is passed via the QUASAR_DEV_AGENT_KEY env var (agentcreds.sh
# already reads it — see its key-resolution order), NEVER via --key: argv is
# visible to any local user via `ps`/`/proc/<pid>/cmdline`, which is exactly
# the leak commit ded82c93 closed one layer down (agentcreds.sh's own curl
# call uses a header FILE for this reason). An env var set on the immediate
# child's environment (`VAR=val cmd`, not `export`) does not appear in argv
# and is only readable via /proc/<pid>/environ by the same UID or root.
mint_identities() {
  local url="$1" key="$2"
  local user_state="$STATE_DIR/user.json" admin_state="$STATE_DIR/admin.json"
  if ! QUASAR_DEV_AGENT_KEY="$key" bash "$DX_DIR/agentcreds.sh" --role user --url "$url" \
        --storage-state "$user_state" >/dev/null; then
    dx_fail agent-creds "failed to mint a user identity — see stderr above"
    dx_result "$TARGET_NAME"
  fi
  if ! QUASAR_DEV_AGENT_KEY="$key" bash "$DX_DIR/agentcreds.sh" --role admin --url "$url" \
        --storage-state "$admin_state" >/dev/null; then
    dx_fail agent-creds "failed to mint an admin identity — see stderr above"
    dx_result "$TARGET_NAME"
  fi
  dx_pass agent-creds "minted user + admin storage-states"
  USER_STATE="$user_state"
  ADMIN_STATE="$admin_state"
}

# ── LEVEL=api: wrap scripts/harness/run-apitest.sh UNCHANGED ───────────────────────────
run_api_level() {
  local api_results="$RESULTS_DIR/api-results.json"
  dx_info "=== LEVEL=api: scripts/harness/run-apitest.sh ==="
  if RESULTS_JSON="$api_results" KEEP="${KEEP}" bash "$DX_ROOT/scripts/harness/run-apitest.sh"; then
    dx_pass api-conformance "scripts/harness/run-apitest.sh green"
  else
    dx_fail api-conformance "scripts/harness/run-apitest.sh failed — see $api_results"
  fi
}

# ── LEVEL=ui / session: run the Node/Playwright journeys ─────────────────────
run_node_level() {
  local base_url="$1" node_level="$2" skipped_note="${3:-}"

  local validate_dir="$DX_ROOT/scripts/validate"
  # npm ci, not npm install (MAJOR 6 / repo review #12 — same rationale as
  # build_web's npm ci: never let running a validation harness silently
  # rewrite a committed lockfile). Re-run whenever node_modules is missing —
  # ci always starts from a clean install.
  if [ ! -d "$validate_dir/node_modules" ]; then
    dx_info "installing scripts/validate node deps (npm ci — first run, caches thereafter)"
    ( cd "$validate_dir" && npm ci )
  fi

  local browsers_cache="${PLAYWRIGHT_BROWSERS_PATH:-$validate_dir/.cache/playwright}"
  export PLAYWRIGHT_BROWSERS_PATH="$browsers_cache"
  # Check the ACTUAL chromium executable Playwright would launch, not just
  # "the cache dir is non-empty" — a Playwright version bump changes the
  # expected revision subdirectory, so an old, non-empty cache dir from a
  # prior npm version would previously be treated as "already installed" and
  # `chromium.launch()` would then fail on a missing executable (MAJOR 6).
  local chromium_exe
  chromium_exe="$(cd "$validate_dir" && node -e "process.stdout.write(require('playwright').chromium.executablePath())" 2>/dev/null || true)"
  if [ -z "$chromium_exe" ] || [ ! -x "$chromium_exe" ]; then
    dx_info "installing Playwright chromium into $browsers_cache (missing/mismatched executable)"
    ( cd "$validate_dir" && npx playwright install chromium )
  fi

  dx_info "=== LEVEL=$node_level journeys against $base_url ==="
  set +e
  node "$validate_dir/runner.mjs" \
    --level "$node_level" \
    --target "$TARGET" \
    --base-url "$base_url" \
    --results-dir "$RESULTS_DIR" \
    --user-state "$USER_STATE" \
    --admin-state "$ADMIN_STATE" \
    --commit "$COMMIT" \
    --skipped "$skipped_note"
  local rc=$?
  set -e

  if [ ! -f "$RESULTS_DIR/report.json" ]; then
    dx_fail report "runner.mjs did not write $RESULTS_DIR/report.json"
    return 1
  fi

  # Fold each journey's verdict into the dx PASS/FAIL tally so the terminal
  # RESULT line and exit code reflect the real per-journey outcome.
  local summary
  summary="$(python3 - "$RESULTS_DIR/report.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    report = json.load(f)
for j in report.get("journeys", []):
    # Collapse embedded newlines (Playwright timeout messages carry multi-line
    # "Call log:" bodies) — this is a one-record-per-line TSV protocol.
    reason = " / ".join(j.get("reason", "").splitlines())
    print(f"{j['verdict']}\t{j['name']}\t{reason}")
    # A journey can PASS its assertions yet leave the world dirty (e.g.
    # session-launch failing to reach a terminal teardown state leaks a GPU
    # slot). Those land in warnings; surface each as a WARN record so a
    # leaked session degrades the run rather than reporting a clean status=ok.
    for w in j.get("warnings", []):
        print(f"WARN\t{j['name']}\t{' / '.join(str(w).splitlines())}")
PY
)"
  local saw_fail=0
  if [ -n "$summary" ]; then
    while IFS=$'\t' read -r verdict name reason; do
      [ -n "$verdict" ] || continue
      if [ "$verdict" = "PASS" ]; then
        dx_pass "journey:$name" "ok"
      elif [ "$verdict" = "WARN" ]; then
        dx_warn "journey:$name" "$reason"
      else
        dx_fail "journey:$name" "$reason"
        saw_fail=1
      fi
    done <<< "$summary"
  fi

  # Belt-and-braces: a non-zero runner exit with no per-journey FAIL recorded
  # (e.g. it crashed before finishing a journey) must still fail the run.
  if [ "$rc" -ne 0 ] && [ "$saw_fail" -eq 0 ]; then
    dx_fail runner "scripts/validate/runner.mjs exited $rc with no journey verdict recorded"
  fi

  return "$rc"
}

# ── Dispatch ───────────────────────────────────────────────────────────────────
NODE_RC=0

if [ "$LEVEL" = api ] || [ "$LEVEL" = all ]; then
  run_api_level
fi

if [ "$LEVEL" = ui ] || [ "$LEVEL" = session ] || [ "$LEVEL" = all ]; then
  BASE_URL=""
  KEY=""
  IS_LOCAL=0

  if [ "$TARGET" = local ]; then
    IS_LOCAL=1
    boot_local_stack
    BASE_URL="http://127.0.0.1:${CP_PORT}"
    KEY="$(tr -d '\r\n' < "$RUN_STATE_DIR/dev-agent-key")"
  else
    BASE_URL="${TARGET%/}"
    KEY="${QUASAR_DEV_AGENT_KEY:-}"
    if [ -z "$KEY" ]; then
      dx_fail dev-agent-key "\$QUASAR_DEV_AGENT_KEY is unset — TARGET=$TARGET needs the live stack's per-boot dev key. Fetch it with: ssh <host> 'docker compose exec -T quasar-control-plane cat /run/quasar/dev-agent-key' (or the stack's compose service name), then re-run with QUASAR_DEV_AGENT_KEY=<key> make validate LEVEL=$LEVEL TARGET=$TARGET"
      dx_result "$TARGET_NAME"
    fi
  fi

  mint_identities "$BASE_URL" "$KEY"

  if [ "$IS_LOCAL" = "1" ]; then
    seed_bench_app "$BASE_URL" "$ADMIN_STATE"
  fi

  # ui|api never touch the session journey. session always does (including
  # against TARGET=local — it then refuses itself with a clear message, see
  # scripts/validate/journeys/session-launch.mjs, rather than being silently
  # skipped). all includes it only when there is a real remote target — the
  # local ephemeral stack has no GPU host, so a session-launch attempt there
  # would be meaningless, not merely refusing; that case is recorded as a
  # SKIPPED journey in the report (MINOR 10) instead of quietly narrowing
  # LEVEL=all's node-side journey set with no trace of why.
  NODE_LEVEL="ui"
  SKIPPED_NOTE=""
  if [ "$LEVEL" = session ]; then
    NODE_LEVEL="session"
  elif [ "$LEVEL" = all ]; then
    if [ "$TARGET" != local ]; then
      NODE_LEVEL="session"
    else
      SKIPPED_NOTE="session-launch=LEVEL=all narrowed to the ui journeys against TARGET=local — no GPU host in the ephemeral stack; rerun with LEVEL=session TARGET=<live-gpu-stack> to exercise it"
    fi
  fi

  run_node_level "$BASE_URL" "$NODE_LEVEL" "$SKIPPED_NOTE" || NODE_RC=$?
fi

# ── Storage-state hygiene: delete on overall PASS, retain (named) on FAIL ─────
if [ "$DX_FAIL_N" -eq 0 ] && [ "$NODE_RC" -eq 0 ]; then
  rm -f "$STATE_DIR"/*.json 2>/dev/null || true
  dx_info "storage-state files deleted (overall PASS)"
else
  dx_info "storage-state files retained for debugging under $STATE_DIR (overall FAIL)"
fi

dx_info "report: $RESULTS_DIR/report.html"
# level=$NODE_LEVEL reports what the node runner ACTUALLY ran (MINOR 10) —
# for LEVEL=api this stays unset/empty since the node runner never launches;
# NODE_LEVEL is only assigned inside the ui/session/all branch above.
dx_result "$TARGET_NAME" "level=$LEVEL" "ran=${NODE_LEVEL:-$LEVEL}" "target=$TARGET" "report=$RESULTS_DIR/report.html"
