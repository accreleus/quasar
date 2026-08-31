#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/homes_gc.sh — run ONE throwaway-home sweep on a host, now (#500).
#
#   make homes-gc HOST=devbox ARGS='--dry-run'
#
# The node-agent sweeps by itself at startup and every 24 h; this is the
# operator's "reclaim it now" / "show me what would go" handle. It execs the
# agent's own `quasar-node-agent homes-gc` subcommand inside the running agent
# container, so it uses exactly the code path, knobs and guards the timer does —
# this script contains no deletion logic of its own and never touches the
# filesystem directly.
#
# Only ephemeral `agent-<8hex>-<8hex>` homes that nothing has mounted and that
# are past QUASAR_HOMES_GC_RETENTION_HOURS are removed; real users' homes and
# `templates` are never candidates. See docs/configuration.md.
#
# HOST is required and must be TYPED (this deletes data on a remote host, so it
# gets the same guard as up/down/rebuild). ARGS may carry `--dry-run`.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

# `make homes-gc ARGS='--dry-run'` delivers ARGS by ENVIRONMENT, not
# interpolated into the recipe line (#550).
[ $# -gt 0 ] || { dx_env_argv homes-gc ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

DRY_RUN=0
for a in "$@"; do
  case "$a" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help)
      sed -n '3,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) dx_guard homes-gc "unknown argument '$a' (only --dry-run is accepted)" ;;
  esac
done

dx_require_host_scope homes-gc

if [ "$DX_HOST" = "local" ]; then
  dx_guard homes-gc \
    "the local stack runs no node-agent — target a fleet host: make homes-gc HOST=devbox"
fi

AGENT_SVC="quasar-node-agent"

# Resolve the agent container through compose (the project name and container
# suffix differ per host, so never hard-code `deploy-quasar-node-agent-1`).
CID="$(dx_ssh_remote "cd '$DX_REMOTE_DIR/deploy' && docker compose $(dx_remote_compose_args) ps -q $AGENT_SVC" 2>/dev/null | tr -d '\r' | head -1 || true)"
if [ -z "$CID" ]; then
  dx_fail homes-gc "no running $AGENT_SVC container on $DX_REMOTE_NAME — bring the stack up first"
  dx_result homes-gc
fi

EXEC_ENV=()
if [ "$DRY_RUN" = "1" ]; then
  EXEC_ENV=(-e QUASAR_HOMES_GC_DRY_RUN=1)
  dx_info "$DX_REMOTE_NAME homes-gc: DRY RUN — nothing will be deleted"
fi
# The sweep is gated on QUASAR_HOMES_GC; a host that has it off should still be
# able to run a manual pass, so force it on for this one-shot invocation only.
EXEC_ENV+=(-e QUASAR_HOMES_GC=1)

OUT="$(dx_ssh_remote "docker exec ${EXEC_ENV[*]} '$CID' quasar-node-agent homes-gc $([ "$DRY_RUN" = 1 ] && echo --dry-run) 2>&1" || true)"
printf '%s\n' "$OUT"

if printf '%s' "$OUT" | grep -q 'homes-gc: sweep'; then
  SUMMARY="$(printf '%s' "$OUT" | grep 'homes-gc: sweep' | tail -1)"
  dx_pass homes-gc "${SUMMARY#*homes-gc: }"
else
  dx_fail homes-gc "the sweep produced no summary line — see the output above"
fi

dx_result homes-gc "dry_run=$DRY_RUN"
