#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/e2e_post.sh — put a suite run on quasar-bench (make e2e-post).
#
#   make e2e-post BUNDLE=<dir> HOST=<role> [DRY_RUN=1]
#
# Thin, like scripts/dx/e2e_collect.sh: scripts/validate/stream/post.mjs validates the
# bundle, takes the commit under test from the bundle's builds.json and posts one run per
# scenario through scripts/dx/bench_submit.py. Posting is local to the bench server, so
# HOST is only the role name that pairs the runs, never a stack this verb reaches.
# The bench server and key stay where bench_submit.py finds them (BENCH_URL / BENCH_KEY,
# else qbench's config); this script never reads, prints or passes either.
#
# Exit: 0 every scenario posted (or planned with DRY_RUN=1), 1 the bundle is not valid,
# the build has no commit, or a post failed, 2 usage.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
POST="$ROOT/scripts/validate/stream/post.mjs"

usage() { # usage <reason-token> <sentence>
  printf 'e2e-post: %s\n' "$2" >&2
  echo "usage: make e2e-post BUNDLE=<bundle-dir> HOST=<role> [DRY_RUN=1]" >&2
  printf 'RESULT status=error target=e2e-post reason=%s\n' "$1"
  exit 2
}

[ -n "${BUNDLE:-}" ] || usage no-bundle "BUNDLE=<dir> is required. Next: make e2e-collect BUNDLE=.diagnostics/e2e/<run>"
[ -d "$BUNDLE" ] || usage no-bundle "'$BUNDLE' is not a directory. A bundle zip is not read here: unzip it first."
[ -n "${HOST:-}" ] || usage no-host "HOST=<role> is required: it pairs the runs, and the bundle holds no host name."
command -v node >/dev/null 2>&1 || usage no-node "node is not on PATH (the poster is Node 22 and has no dependencies)."

args=("$BUNDLE" --host "$HOST")
[ "${DRY_RUN:-0}" = 1 ] && args+=(--dry-run)
exec node "$POST" "${args[@]}"
