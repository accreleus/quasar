#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/engine_suite.sh — the runtime's behavioural suite against real engines (#408).
#
#   make test-engines         build the suite in the devtools container, then run it here
#   make test-engines-build   build only: a libc-only binary to copy to a lab host
#
# The suite runs on the engine's own host as the invoking user, so bind-mount sources,
# device nodes and file owners are the host's. Targets come from
# QUASAR_ENGINE_SUITE_TARGETS ("<mode>=<socket> ..."); unset, every standard socket this
# user can reach is targeted as the mode its path conventionally means, and the suite's
# identity case fails one that is not. A capability the host cannot give is declared in
# QUASAR_ENGINE_SUITE_LACKS or its case fails. docs/testing-engine-suite.md has both.
#
# It creates and removes only uniquely named, run-labelled containers and volumes.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

MODE="${1:-run}"
case "$MODE" in
  run)   dx_require_local test-engines ;;
  build) dx_require_local test-engines-build ;;
  *) printf 'usage: %s [run|build]\n' "$0" >&2; exit 2 ;;
esac

BIN="${ENGINE_SUITE_BIN:-$DX_ROOT/.diagnostics/engine-suite/engine-suite}"
if [ -z "${ENGINE_SUITE_BIN:-}" ]; then
  bash "$DX_ROOT/scripts/verify.sh" engine-suite-build
fi
if [ "$MODE" = build ]; then
  dx_info "engine suite: $BIN"
  exit 0
fi
[ -x "$BIN" ] || { printf 'FAIL test-engines — no suite binary at %s\n' "$BIN" >&2; exit 1; }

if [ -z "${QUASAR_ENGINE_SUITE_TARGETS:-}" ]; then
  runtime_dir="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
  found=""
  reachable() { [ -S "$2" ] && [ -r "$2" ] && [ -w "$2" ] && found="$found $1=$2"; return 0; }
  reachable docker-rootful /var/run/docker.sock
  reachable docker-rootless "$runtime_dir/docker.sock"
  reachable podman-rootful /run/podman/podman.sock
  reachable podman-rootless "$runtime_dir/podman/podman.sock"
  if [ -z "$found" ]; then
    printf 'FAIL test-engines — no engine socket is reachable; set QUASAR_ENGINE_SUITE_TARGETS\n' >&2
    exit 2
  fi
  QUASAR_ENGINE_SUITE_TARGETS="${found# }"
  export QUASAR_ENGINE_SUITE_TARGETS
  dx_info "targets: $QUASAR_ENGINE_SUITE_TARGETS"
fi
exec "$BIN"
