#!/usr/bin/env bash
# scripts/dx/codec_validate.sh — DX façade over scripts/harness/run-codec-validate.sh
# (M2 codec-parameterised validation harness, vulkanav1enc spec §7).
#
# Thin: this script's only job is resolving HOST -> an API base the same way
# session_soak.sh / bench_run.sh already do, then handing everything else
# straight to scripts/harness/run-codec-validate.sh — no logic is duplicated here.
#
# Local: runs scripts/harness/run-codec-validate.sh directly against this worktree's
#   instance-scoped stack (http://127.0.0.1:$DX_CP_PORT). Requires CFT +
#   playwright already provisioned (`qses provision`) and docker reachable on
#   THIS machine.
# Remote (HOST=<role-or-host>): scripts/harness/run-codec-validate.sh must run ON that
#   host (it drives a local CFT peer + `docker exec`/`docker cp` against the
#   stack's own containers — same rule run-spt06-certify.sh and
#   run-soak-profile.sh follow), so this delegates the ENTIRE invocation over
#   ssh rather than proxying individual calls. The remote checkout at
#   DX_REMOTE_DIR must already carry this script (i.e. be at least this
#   commit) — codec-validate does not ship itself over the wire.
#
# Usage:
#   make codec-validate ARGS='--app Steam'
#   make codec-validate ARGS='--app Steam --codecs h264,h265,av1 --secs 120' HOST=devbox
#
# All flags are scripts/harness/run-codec-validate.sh's own — see its header (--app is
# required; --codecs/--secs/--profile/--keep/... are optional). This wrapper
# adds no flags of its own beyond ARGS passthrough.
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=codec-validate

# `make codec-validate ARGS='--app Steam --codecs h264,av1'` delivers ARGS by
# ENVIRONMENT, not interpolated into the recipe line (#550). Every token is
# shape-checked before it reaches the printf %q remote command built below.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

dx_require_host_scope "$TARGET"

if [ "$DX_HOST" = local ]; then
  dx_info "codec-validate: running locally against http://127.0.0.1:$DX_CP_PORT"
  if bash "$DX_ROOT/scripts/harness/run-codec-validate.sh" --api "http://127.0.0.1:$DX_CP_PORT" "$@"; then
    dx_pass codec-validate "scripts/harness/run-codec-validate.sh green"
  else
    dx_fail codec-validate "scripts/harness/run-codec-validate.sh failed — see output above"
  fi
else
  [ -n "${DX_REMOTE_DIR:-}" ] || dx_guard "$TARGET" "HOST=$DX_HOST resolved no remote dir (see .claude/skills/_shared/hosts.json)"
  RARGS=""
  for a in "$@"; do RARGS="$RARGS $(printf '%q' "$a")"; done
  dx_info "codec-validate: delegating to $DX_REMOTE_NAME over ssh (CFT + docker must live there)"
  API_ARG=""
  [ -n "${DX_REMOTE_API:-}" ] && API_ARG="--api $(printf '%q' "$DX_REMOTE_API")"
  if dx_ssh_remote "cd '$DX_REMOTE_DIR' && bash scripts/harness/run-codec-validate.sh $API_ARG$RARGS"; then
    dx_pass codec-validate "scripts/harness/run-codec-validate.sh green on $DX_REMOTE_NAME"
  else
    dx_fail codec-validate "scripts/harness/run-codec-validate.sh failed on $DX_REMOTE_NAME — see output above"
  fi
fi

dx_result "$TARGET"
