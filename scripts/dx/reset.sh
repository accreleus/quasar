#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/reset.sh — tear down THIS worktree's local stack.
#
#   make reset CONFIRM=reset          containers + networks, volumes PRESERVED
#   make reset CONFIRM=reset-data     the above, plus this instance's volumes
#
# Guards, in order:
#   1. CONFIRM must be reset or reset-data (rc 2 otherwise)
#   2. Any non-local HOST is refused UNCONDITIONALLY — there is no "reset a
#      fleet host" verb here, and there never will be. A fleet host is
#      redeployed with deploy/redeploy.sh, deliberately and by a human.
#   3. Scope is `docker compose -p $QUASAR_INSTANCE` and nothing else, so this
#      cannot reach another worktree's stack or the fleet's named volumes.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=reset

# Guard 2 first: even a correct CONFIRM must not look like it could hit a
# remote host. Checked before dx_require_local so this refuses ANY non-local
# HOST value — known or not, without needing hosts.json to resolve it.
if [ "$DX_HOST" != "local" ]; then
  dx_guard "$TARGET" "reset NEVER targets a remote host. Redeploy it with deploy/redeploy.sh instead."
fi
dx_require_local "$TARGET"

CONFIRM="${CONFIRM:-}"
case "$CONFIRM" in
  reset|reset-data) ;;
  "")
    dx_guard "$TARGET" \
      "refusing to reset without confirmation — re-run as: make reset CONFIRM=reset (add CONFIRM=reset-data to also delete this instance's volumes)" ;;
  *)
    dx_guard "$TARGET" \
      "CONFIRM='$CONFIRM' is not valid — use CONFIRM=reset or CONFIRM=reset-data" ;;
esac

if ! dx_have docker || ! docker info >/dev/null 2>&1; then
  dx_fail docker "daemon not reachable — nothing was reset"
  dx_result "$TARGET"
fi

dx_info "scope: compose project '$QUASAR_INSTANCE' only (this worktree)"

if [ "$CONFIRM" = "reset-data" ]; then
  if dx_local_compose down --remove-orphans --volumes; then
    dx_pass reset "project $QUASAR_INSTANCE removed INCLUDING its named volumes"
  else
    dx_fail reset "compose down --volumes failed"
  fi
else
  if dx_local_compose down --remove-orphans; then
    dx_pass reset "project $QUASAR_INSTANCE removed (volumes preserved)"
  else
    dx_fail reset "compose down failed"
  fi
fi

# The ephemeral test database is instance-scoped too; a reset should not leave
# one behind from a killed make test-db. Since #466, an unpinned `make
# test-db` names its container "qpg-${QUASAR_INSTANCE}-<pid>-<rand>" (not the
# bare "qpg-${QUASAR_INSTANCE}") so concurrent runs don't collide — sweep by
# prefix to still catch an orphan left by a SIGKILLed run.
readarray -t STALE_TESTDB < <(docker ps -aq --filter "name=^qpg-${QUASAR_INSTANCE}" 2>/dev/null || true)
if [ "${#STALE_TESTDB[@]}" -gt 0 ]; then
  if docker rm -f "${STALE_TESTDB[@]}" >/dev/null 2>&1; then
    dx_pass testdb "removed ${#STALE_TESTDB[@]} leftover ephemeral postgres container(s) matching qpg-${QUASAR_INSTANCE}*"
  else
    dx_warn testdb "found leftover ephemeral postgres container(s) matching qpg-${QUASAR_INSTANCE}* but could not remove all of them"
  fi
fi

dx_result "$TARGET" "confirm=$CONFIRM"
