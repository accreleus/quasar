#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/nightly_budget.sh — the nightly, unattended glass-to-glass budget
# run (docs/testing-bench-mode.md "The glass-to-glass budget (standing
# instrument)" -> "A nightly budget run").
#
#   scripts/dx/nightly_budget.sh                 # run once, now
#   scripts/dx/nightly_budget.sh --dry-run        # preflight checks only, print the plan, run nothing
#   make nightly-budget-install HOST=devbox       # installs the crontab line (03:30 daily)
#   make nightly-budget-run HOST=devbox           # trigger one run now
#   make nightly-budget-status HOST=devbox        # tail the log + print LAST_REGRESSION if any
#
# Designed to run AS the cron job, on the STACK HOST ITSELF (devbox), from
# whatever sha happens to be checked out there — it deliberately does NOT
# `git pull` / redeploy first (that is a mutation of the running stack, and a
# nightly cron is not the place for it). The deployed sha is only ever READ
# (`git rev-parse HEAD`) and recorded as a tag, never advanced.
#
# One run is: preflight checks (below), then ONE `bench_run.sh --bench-mode`
# 1080p60 h264 cell against `HOST=devbox-self` (this box, over a loopback ssh
# hop — see "HOST=devbox-self" below), then `bench_budget.py` against the
# pinned baseline `latency-budget/1080p60-h264-local`. Suite `nightly-budget`,
# scenario `1080p60-h264-local`, tags `nightly=1 git_quasar=<sha>`.
#
# HOST=devbox-self: bench_run.sh always launches its session through `qses`,
# and `qses` always shells out over ssh to whatever `--stack` names — even
# when the target IS the machine the caller is already running on (there is
# no "we're already here, skip the hop" path in qses). So a cron job running
# directly on the stack host still needs a working ssh hop to reach itself.
# A dedicated self-entry (e.g. `"<host>-self"`) is added to that host's own (untracked, machine-local)
# hosts.json: a loopback ssh_host 127.0.0.1 keyed by a dedicated ed25519
# keypair (NOT an interactive/agent-backed key) added to that host's own authorized_keys.
# Deliberately NOT named "local": common.sh's DX_HOST=local sentinel means
# "skip remote resolution entirely, this is the ephemeral
# docker-compose.local.yml dev stack" (a different stack, a different port) —
# bench_run.sh's own admin-API calls (`host_curl`) fall back to that stack's
# port whenever DX_HOST=local, which silently pointed every one of them at the
# wrong stack the first time this ran live (session launched and ran fine over
# the real ssh/qses path, but bench_run.sh's own polling for it timed out
# after 300s because it was asking the wrong port whether the session
# existed). A real, resolvable host name routes bench_run.sh through its
# normal remote-host code path (`dx_resolve_remote` -> `DX_REMOTE_API`)
# instead, which resolves correctly. `NIGHTLY_HOST` overrides the stack
# role/host for tests.
#
# Preflight (each one that fails makes this a clean SKIP, never a crash):
#   * BENCH_KEY readable  — from $NIGHTLY_BENCH_ENV's BENCH_API_KEYS=name:secret
#                            (default: $HOME/quasar-bench/deploy/.env,
#                            "harness" entry). Never copied into this repo.
#   * the stack is healthy — GET $NIGHTLY_HEALTH_URL/health == 200
#   * no session already running — `qses ls --stack=$NIGHTLY_HOST` (using the
#     stack's own per-boot dev-agent key, docker-exec'd fresh every run since
#     it rotates on every control-plane restart) reports no non-terminal
#     (starting|running|swapping) session
#   * this script is not already mid-run — a mkdir-based lock (portable: no
#     flock dependency), so a hung previous run never overlaps a new one
#
# On regression (bench_budget.py exits 1): $NIGHTLY_LOG_DIR/LAST_REGRESSION is
# (over)written with the reconciled table, and the log line below is the
# alert — there is no email/Slack wiring yet, this is meant to be read via
# Dozzle or `make nightly-budget-status`.
#
# Every run appends ONE line, always the last line written for that run:
#   NIGHTLY-BUDGET status=ok|regression|skipped|error run_id=<id|-> \
#     suite=<suite> scenario=<scenario> git_quasar=<sha> [reason=<...>]
#
# Logs: $NIGHTLY_LOG_DIR/<YYYY-MM-DD>.log, one file per UTC day, rotated to
# keep the last $NIGHTLY_KEEP_DAYS (default 30).
#
# Env overrides (all optional; every one exists so this is testable without
# real infra — see scripts/dx/tests/run.sh "nightly budget"):
#   NIGHTLY_HOST            stack role/host (default: devbox-self)
#   NIGHTLY_LOG_DIR         default: $HOME/quasar-nightly
#   NIGHTLY_KEEP_DAYS       default: 30
#   NIGHTLY_SUITE           default: nightly-budget
#   NIGHTLY_SCENARIO        default: 1080p60-h264-local
#   NIGHTLY_PROFILE         default: 1080p60
#   NIGHTLY_APP             default: Quasar Benchapp
#   NIGHTLY_SECS            default: 150
#   NIGHTLY_CODEC           default: h264
#   NIGHTLY_BASELINE        default: latency-budget/1080p60-h264-local
#   NIGHTLY_BENCH_ENV       default: $HOME/quasar-bench/deploy/.env
#   NIGHTLY_BENCH_URL       default: derived from BENCH_PORT in that file (or 9400)
#   NIGHTLY_HEALTH_URL      default: https://localhost:8443/health
#   NIGHTLY_API_URL         default: derived from NIGHTLY_HEALTH_URL (strip /health)
#   NIGHTLY_DEV_KEY         skip the docker-exec fetch and use this key verbatim
#   NIGHTLY_ADMIN_TOKEN     skip minting an admin bearer (scripts/dx/agentcreds.sh)
#                           and use this token verbatim — bench_run.sh needs one
#                           of its own (QSES_ADMIN_TOKEN) for its host-settings
#                           PATCH, distinct from the dev-agent key above
#   NIGHTLY_ADMIN_TTL       admin token ttl, default 30m
#   NIGHTLY_BENCH_RUN       default: scripts/dx/bench_run.sh (override for tests)
#   NIGHTLY_BENCH_BUDGET    default: "python3 scripts/dx/bench_budget.py" (override for tests)
#   NIGHTLY_QSES            default: .claude/skills/quasar-session/scripts/qses (override for tests)
#
# Exit: 0 always on a clean skip/ok/regression (this is a cron job — a
# non-zero exit would just generate cron's own mail noise, and the log line
# is the real signal). Non-zero only on a genuine usage error (--bad-flag).
set -uo pipefail  # deliberately NOT -e: every step decides its own fate and
                   # always reaches the final NIGHTLY-BUDGET line and lock release.

DX_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/dx/common.sh
source "$DX_DIR/common.sh"

usage() { sed -n '3,84p' "$0" | sed 's/^# \{0,1\}//'; }

DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'usage: nightly_budget.sh [--dry-run]\n' >&2; exit 2 ;;
  esac
done

NIGHTLY_HOST="${NIGHTLY_HOST:-devbox-self}"
NIGHTLY_LOG_DIR="${NIGHTLY_LOG_DIR:-$HOME/quasar-nightly}"
NIGHTLY_KEEP_DAYS="${NIGHTLY_KEEP_DAYS:-30}"
NIGHTLY_SUITE="${NIGHTLY_SUITE:-nightly-budget}"
NIGHTLY_SCENARIO="${NIGHTLY_SCENARIO:-1080p60-h264-local}"
NIGHTLY_PROFILE="${NIGHTLY_PROFILE:-1080p60}"
NIGHTLY_APP="${NIGHTLY_APP:-Quasar Benchapp}"
NIGHTLY_SECS="${NIGHTLY_SECS:-150}"
NIGHTLY_CODEC="${NIGHTLY_CODEC:-h264}"
NIGHTLY_BASELINE="${NIGHTLY_BASELINE:-latency-budget/1080p60-h264-local}"
NIGHTLY_BENCH_ENV="${NIGHTLY_BENCH_ENV:-$HOME/quasar-bench/deploy/.env}"
NIGHTLY_HEALTH_URL="${NIGHTLY_HEALTH_URL:-https://localhost:8443/health}"
NIGHTLY_API_URL="${NIGHTLY_API_URL:-${NIGHTLY_HEALTH_URL%/health}}"
NIGHTLY_BENCH_RUN="${NIGHTLY_BENCH_RUN:-$DX_DIR/bench_run.sh}"
NIGHTLY_BENCH_BUDGET="${NIGHTLY_BENCH_BUDGET:-python3 $DX_DIR/bench_budget.py}"
NIGHTLY_QSES="${NIGHTLY_QSES:-$DX_ROOT/.claude/skills/quasar-session/scripts/qses}"

mkdir -p "$NIGHTLY_LOG_DIR"

# ── log rotation: keep the last N daily files ────────────────────────────────
find "$NIGHTLY_LOG_DIR" -maxdepth 1 -name '????-??-??.log' -mtime "+$NIGHTLY_KEEP_DAYS" -delete 2>/dev/null || true

LOG="$NIGHTLY_LOG_DIR/$(date -u +%F).log"
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >> "$LOG"; }

SHA="$(git -C "$DX_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"

finish() { # finish <status> [k=v ...]
  local status="$1"; shift || true
  local line="NIGHTLY-BUDGET status=$status run_id=${RUN_ID:--} suite=$NIGHTLY_SUITE scenario=$NIGHTLY_SCENARIO git_quasar=$SHA"
  local kv; for kv in "$@"; do line="$line $kv"; done
  log "$line"
  printf '%s\n' "$line"
  exit 0
}

RUN_ID=""

# ── lock: one nightly run at a time, portable (mkdir is atomic everywhere) ──
LOCK_DIR="$NIGHTLY_LOG_DIR/.run.lock"
if [ "$DRY" != 1 ]; then
  if ! mkdir "$LOCK_DIR" 2>/dev/null; then
    log "skip: reason=nightly-lock-held (another run has $LOCK_DIR)"
    finish skipped "reason=nightly-lock-held"
  fi
  trap 'rmdir "$LOCK_DIR" 2>/dev/null || true' EXIT INT TERM
fi

log "start host=$NIGHTLY_HOST suite=$NIGHTLY_SUITE scenario=$NIGHTLY_SCENARIO git_quasar=$SHA dry_run=$DRY"

# ── preflight 1: BENCH_KEY readable ──────────────────────────────────────────
BENCH_KEY=""
if [ -n "${NIGHTLY_BENCH_KEY:-}" ]; then
  BENCH_KEY="$NIGHTLY_BENCH_KEY"
elif [ -r "$NIGHTLY_BENCH_ENV" ]; then
  # BENCH_API_KEYS=name:secret[,name2:secret2,...] — take the first pair's
  # secret half (bench_run.sh/bench_submit.py tolerate the raw name:secret
  # form too, but resolving it here means the preflight itself can tell
  # "file present but no key" apart from "key present").
  raw="$(sed -n 's/^BENCH_API_KEYS=//p' "$NIGHTLY_BENCH_ENV" | head -1)"
  raw="${raw%%,*}"
  BENCH_KEY="${raw#*:}"
fi
if [ -z "$BENCH_KEY" ]; then
  log "skip: reason=bench-key-unavailable ($NIGHTLY_BENCH_ENV BENCH_API_KEYS unreadable or empty)"
  finish skipped "reason=bench-key-unavailable"
fi

BENCH_URL="${NIGHTLY_BENCH_URL:-}"
if [ -z "$BENCH_URL" ] && [ -r "$NIGHTLY_BENCH_ENV" ]; then
  port="$(sed -n 's/^BENCH_PORT=//p' "$NIGHTLY_BENCH_ENV" | head -1)"
  BENCH_URL="http://localhost:${port:-9400}"
fi
BENCH_URL="${BENCH_URL:-http://localhost:9400}"

# ── preflight 2: stack healthy ───────────────────────────────────────────────
if [ "$DRY" != 1 ]; then
  code="$(curl -k -sS -o /dev/null -w '%{http_code}' --max-time 10 "$NIGHTLY_HEALTH_URL" 2>/dev/null || true)"
  if [ "$code" != "200" ]; then
    log "skip: reason=stack-unhealthy ($NIGHTLY_HEALTH_URL -> ${code:-unreachable})"
    finish skipped "reason=stack-unhealthy"
  fi
fi

# ── preflight 3: no session already running ──────────────────────────────────
if [ "$DRY" != 1 ]; then
  DEV_KEY="${NIGHTLY_DEV_KEY:-}"
  if [ -z "$DEV_KEY" ]; then
    CPC="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 'quasar-control-plane' || true)"
    if [ -n "$CPC" ]; then
      DEV_KEY="$(docker exec "$CPC" cat /run/quasar/dev-agent-key 2>/dev/null || true)"
    fi
  fi
  if [ -z "$DEV_KEY" ]; then
    log "skip: reason=dev-agent-key-unavailable (no quasar-control-plane container, or /run/quasar/dev-agent-key unreadable — needs QUASAR_DEV_AGENT_AUTH=1)"
    finish skipped "reason=dev-agent-key-unavailable"
  fi
  ls_out="$(cd "$DX_ROOT" && QSES_PEER_ROLE="$NIGHTLY_HOST" QSES_DEV_KEY="$DEV_KEY" \
    bash "$NIGHTLY_QSES" ls --stack="$NIGHTLY_HOST" 2>&1)"
  # `qses ls` prints "<id> <state> <app-prefix>" per session after its own
  # PASS/RESULT lines — a non-terminal state means a session is live.
  if printf '%s\n' "$ls_out" | grep -Eq '^[0-9a-f-]+ (starting|running|swapping)( |$)'; then
    log "skip: reason=session-already-running"
    log "$ls_out"
    finish skipped "reason=session-already-running"
  fi

  # bench_run.sh needs a real admin BEARER token of its own (QSES_ADMIN_TOKEN)
  # for its host-settings PATCH (arming/restoring latency_probe) — the
  # dev-agent KEY above is a different credential (it lets qses mint identities
  # on the stack's behalf, but bench_run.sh's own host_curl calls want the
  # token directly). Minted fresh every run, short TTL, never logged.
  ADMIN_TOKEN="${NIGHTLY_ADMIN_TOKEN:-}"
  if [ -z "$ADMIN_TOKEN" ]; then
    creds_json="$(cd "$DX_ROOT" && bash "$DX_DIR/agentcreds.sh" --role admin --ttl "${NIGHTLY_ADMIN_TTL:-30m}" \
      --url "$NIGHTLY_API_URL" --key "$DEV_KEY" 2>>"$LOG")" || true
    ADMIN_TOKEN="$(printf '%s' "$creds_json" | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin)["quasar.auth.token"])
except Exception:
    pass' 2>/dev/null || true)"
  fi
  if [ -z "$ADMIN_TOKEN" ]; then
    log "skip: reason=admin-token-mint-failed"
    finish skipped "reason=admin-token-mint-failed"
  fi
fi

if [ "$DRY" = 1 ]; then
  cat <<PLAN
plan
  host      $NIGHTLY_HOST
  app       $NIGHTLY_APP
  profile   $NIGHTLY_PROFILE
  suite     $NIGHTLY_SUITE
  scenario  $NIGHTLY_SCENARIO
  codec     $NIGHTLY_CODEC
  secs      $NIGHTLY_SECS
  baseline  $NIGHTLY_BASELINE
  bench_url $BENCH_URL
  log       $LOG
  would run: HOST=$NIGHTLY_HOST $NIGHTLY_BENCH_RUN --app '$NIGHTLY_APP' --profile $NIGHTLY_PROFILE --secs $NIGHTLY_SECS --bench-mode --codec $NIGHTLY_CODEC --peer local --suite $NIGHTLY_SUITE --scenario $NIGHTLY_SCENARIO --tag nightly=1 --tag git_quasar=$SHA
  would run: $NIGHTLY_BENCH_BUDGET --run <id> --suite $NIGHTLY_SUITE --scenario $NIGHTLY_SCENARIO --baseline '$NIGHTLY_BASELINE'
PLAN
  log "dry-run: preflight passed, nothing launched"
  finish ok "dry_run=1"
fi

# ── the run ──────────────────────────────────────────────────────────────────
RUN_LOG="$(mktemp "${TMPDIR:-/tmp}/nightly-budget.XXXXXX")"
# shellcheck disable=SC2064
trap "rm -f '$RUN_LOG'; rmdir '$LOCK_DIR' 2>/dev/null || true" EXIT INT TERM

BENCH_RUN_RC=0
HOST="$NIGHTLY_HOST" BENCH_URL="$BENCH_URL" BENCH_KEY="$BENCH_KEY" \
  QSES_DEV_KEY="${NIGHTLY_DEV_KEY:-${DEV_KEY:-}}" \
  QSES_ADMIN_TOKEN="${NIGHTLY_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}" \
  bash "$NIGHTLY_BENCH_RUN" \
    --app "$NIGHTLY_APP" --profile "$NIGHTLY_PROFILE" --secs "$NIGHTLY_SECS" \
    --bench-mode --codec "$NIGHTLY_CODEC" --peer local \
    --suite "$NIGHTLY_SUITE" --scenario "$NIGHTLY_SCENARIO" \
    --tag "nightly=1" --tag "git_quasar=$SHA" \
    > "$RUN_LOG" 2>&1 || BENCH_RUN_RC=$?

cat "$RUN_LOG" >> "$LOG"

RUN_ID="$(sed -n 's/.*RESULT .*run_id=\([0-9A-Za-z-]*\).*/\1/p' "$RUN_LOG" | tail -1)"

if [ -z "$RUN_ID" ]; then
  log "error: bench_run.sh rc=$BENCH_RUN_RC produced no run id — see the log above"
  finish error "reason=bench-run-failed bench_run_rc=$BENCH_RUN_RC"
fi

# ── gate: bench_budget.py against the pinned baseline ────────────────────────
BUDGET_OUT="$(mktemp "${TMPDIR:-/tmp}/nightly-budget-table.XXXXXX")"
BUDGET_RC=0
BENCH_URL="$BENCH_URL" BENCH_KEY="$BENCH_KEY" \
  $NIGHTLY_BENCH_BUDGET --run "$RUN_ID" --suite "$NIGHTLY_SUITE" --scenario "$NIGHTLY_SCENARIO" \
    --baseline "$NIGHTLY_BASELINE" \
    > "$BUDGET_OUT" 2>&1 || BUDGET_RC=$?

cat "$BUDGET_OUT" >> "$LOG"

case "$BUDGET_RC" in
  0)
    rm -f "$BUDGET_OUT"
    finish ok
    ;;
  1)
    { printf 'nightly-budget REGRESSION — %s\n' "$(date -u +%FT%TZ)"
      printf 'run_id=%s suite=%s scenario=%s git_quasar=%s\n\n' "$RUN_ID" "$NIGHTLY_SUITE" "$NIGHTLY_SCENARIO" "$SHA"
      cat "$BUDGET_OUT"
    } > "$NIGHTLY_LOG_DIR/LAST_REGRESSION"
    rm -f "$BUDGET_OUT"
    finish regression
    ;;
  *)
    rm -f "$BUDGET_OUT"
    finish error "reason=budget-script-failed budget_rc=$BUDGET_RC"
    ;;
esac
