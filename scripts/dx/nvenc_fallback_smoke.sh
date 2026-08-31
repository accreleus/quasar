#!/usr/bin/env bash
# scripts/dx/nvenc_fallback_smoke.sh — DX façade over
# scripts/harness/run-nvenc-fallback-smoke.sh (fallback-NVENC smoke, #271 remainder
# item absorbed from #266).
#
# Thin: this script's only job is resolving HOST -> an API base the same way
# codec_validate.sh / session_soak.sh already do, then handing everything
# else straight to scripts/harness/run-nvenc-fallback-smoke.sh — no logic is
# duplicated here.
#
# Local: runs scripts/harness/run-nvenc-fallback-smoke.sh directly against this
#   worktree's instance-scoped stack (http://127.0.0.1:$DX_CP_PORT).
# Remote (HOST=<role-or-host>): scripts/harness/run-nvenc-fallback-smoke.sh must run ON
#   that host (it drives a local CFT peer + `docker exec`/`docker logs`/
#   `docker inspect` against the stack's own containers — same rule
#   codec_validate.sh follows), so this delegates the ENTIRE invocation over
#   ssh rather than proxying individual calls. The remote checkout at
#   DX_REMOTE_DIR must already carry this script (i.e. be at least this
#   commit).
#
# NOTE: this suite requires an NVIDIA GPU host with QUASAR_VULKAN_H264=0
# already set on the running node-agent container (deploy/.env +
# `docker compose up -d --force-recreate quasar-node-agent`, THEN run this —
# see scripts/harness/run-nvenc-fallback-smoke.sh's header for why it can't flip that
# itself). devbox is the only such host — see CLAUDE.md.
#
# Usage:
#   make nvenc-fallback-smoke ARGS='--app "Quasar Bench: Ball"' HOST=devbox
#
# All flags are scripts/harness/run-nvenc-fallback-smoke.sh's own — see its header
# (--app is required; --secs/--profile/--keep/... are optional). This wrapper
# adds no flags of its own beyond ARGS passthrough.
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=nvenc-fallback-smoke

# `make nvenc-fallback-smoke ARGS='--app Steam'` delivers ARGS by ENVIRONMENT,
# not interpolated into the recipe line (#550). Every token is shape-checked
# before it reaches the printf %q remote command built below.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

dx_require_host_scope "$TARGET"

if [ "$DX_HOST" = local ]; then
  dx_info "nvenc-fallback-smoke: running locally against http://127.0.0.1:$DX_CP_PORT"
  if bash "$DX_ROOT/scripts/harness/run-nvenc-fallback-smoke.sh" --api "http://127.0.0.1:$DX_CP_PORT" "$@"; then
    dx_pass nvenc-fallback-smoke "scripts/harness/run-nvenc-fallback-smoke.sh green"
  else
    dx_fail nvenc-fallback-smoke "scripts/harness/run-nvenc-fallback-smoke.sh failed — see output above"
  fi
else
  [ -n "${DX_REMOTE_DIR:-}" ] || dx_guard "$TARGET" "HOST=$DX_HOST resolved no remote dir (see .claude/skills/_shared/hosts.json)"
  RARGS=""
  for a in "$@"; do RARGS="$RARGS $(printf '%q' "$a")"; done
  dx_info "nvenc-fallback-smoke: delegating to $DX_REMOTE_NAME over ssh (CFT + docker + the NVIDIA GPU must live there)"
  API_ARG=""
  [ -n "${DX_REMOTE_API:-}" ] && API_ARG="--api $(printf '%q' "$DX_REMOTE_API")"
  if dx_ssh_remote "cd '$DX_REMOTE_DIR' && bash scripts/harness/run-nvenc-fallback-smoke.sh $API_ARG$RARGS"; then
    dx_pass nvenc-fallback-smoke "scripts/harness/run-nvenc-fallback-smoke.sh green on $DX_REMOTE_NAME"
  else
    dx_fail nvenc-fallback-smoke "scripts/harness/run-nvenc-fallback-smoke.sh failed on $DX_REMOTE_NAME — see output above"
  fi
fi

dx_result "$TARGET"
