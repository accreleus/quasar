#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/e2e_collect.sh — the host half of a run bundle (make e2e-collect).
#
#   make e2e-collect BUNDLE=<dir> [HOST=<role|name>] [RESUME=1]
#
# Resolves the stack exactly as the session-* verbs do (HOST -> api_external in
# .claude/skills/_shared/hosts.json), takes the admin bearer from the one ladder
# (scripts/dx/admin_token.sh) and hands it to scripts/validate/stream/collect.mjs
# through the ENVIRONMENT, never on argv. Read-only: nothing on the host changes.
# A bundle that already has a host half is refused (exit 2) unless RESUME=1, which finishes a
# collection this machine started.
#
# Exit: 0 every session collected, 1 the bundle is not valid, 2 usage / no bearer /
# wrong host / a bundle that already has a host half, 3 incomplete (a session the stack no longer holds; the reason is in
# that scenario's host-half.json).
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

BUNDLE="${BUNDLE:-}"
COLLECT="$DX_ROOT/scripts/validate/stream/collect.mjs"

result() { # result <status> [k=v ...]
  local status="$1"; shift || true
  printf 'RESULT status=%s target=e2e-collect' "$status"
  local kv
  for kv in "$@"; do printf ' %s' "$kv"; done
  printf '\n'
}

die() { # die <reason-token> <sentence>
  printf 'ERROR %s\n' "$2" >&2
  result error "reason=$1"
  exit 2
}

[ -n "$BUNDLE" ] || die no-bundle "BUNDLE=<dir> is required. Next: make e2e-collect BUNDLE=.diagnostics/e2e/<run>"
[ -d "$BUNDLE" ] || die no-bundle "'$BUNDLE' is not a directory. A bundle zip is not read here: unzip it first."
dx_have node || die no-node "node is not on PATH (the collector is Node 22 and has no dependencies)."

API_BASE=""
if [ "$DX_HOST" = "local" ]; then
  API_BASE="http://127.0.0.1:${DX_CP_PORT}"
else
  dx_resolve_remote "$DX_HOST" || die unknown-host \
    "HOST='$DX_HOST' is not a known role or host. Next: make session-list HOST=<role>, or add it to .claude/skills/_shared/hosts.json."
  API_BASE="${DX_REMOTE_API_EXTERNAL:-}"
  [ -n "$API_BASE" ] || die no-api \
    "host '$DX_REMOTE_NAME' has no api_external in hosts.json. Next: add \"api_external\" to its entry."
fi
API_BASE="${API_BASE%/}"

TOKEN="$(QUASAR_ADMIN_TOKEN="${QUASAR_ADMIN_TOKEN:-${QSES_ADMIN_TOKEN:-}}" \
  bash "$DX_DIR/admin_token.sh" --host "$DX_HOST" --quiet)" || TOKEN=""
[ -n "$TOKEN" ] || die no-token \
  "no admin bearer for host=$DX_HOST (the ladder printed every tier it tried above). Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh"

ARGS=()
[ "${RESUME:-}" = 1 ] && ARGS+=(--resume)
QUASAR_ADMIN_TOKEN="$TOKEN" exec node "$COLLECT" "$BUNDLE" --base-url "$API_BASE" "${ARGS[@]}"
