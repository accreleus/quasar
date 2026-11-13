#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/e2e_compare.sh — what changed between two run bundles (make e2e-compare).
#
#   make e2e-compare A=<bundle-dir> B=<bundle-dir> [OUT=<dir>]
#
# Thin, like scripts/dx/e2e_post.sh: scripts/validate/stream/compare.mjs validates both bundles,
# says what differed about the runs and then what the results show, and writes COMPARE.md and
# compare.json into OUT (default: a new directory beside B). Nothing here reaches a host, a bench
# server or a token: two directories are all this verb takes, and it can run on any machine that
# has Node 22.
#
# Exit: 0 both bundles are valid and the comparison was written, 1 a bundle is not valid,
# 2 usage, or a comparison that cannot be written (OUT exists, or is inside a bundle).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPARE="$ROOT/scripts/validate/stream/compare.mjs"

usage() { # usage <reason-token> <sentence>
  printf 'e2e-compare: %s\n' "$2" >&2
  echo "usage: make e2e-compare A=<bundle-dir> B=<bundle-dir> [OUT=<dir>]" >&2
  printf 'RESULT status=error target=e2e-compare reason=%s\n' "$1"
  exit 2
}

[ -n "${A:-}" ] || usage no-a "A=<dir> is required: the bundle to compare from (the baseline)."
[ -n "${B:-}" ] || usage no-b "B=<dir> is required: the bundle to compare to."
[ -d "${A}" ] || usage no-a "'${A}' is not a directory. A bundle zip is not read here: unzip it first."
[ -d "${B}" ] || usage no-b "'${B}' is not a directory. A bundle zip is not read here: unzip it first."
command -v node >/dev/null 2>&1 || usage no-node "node is not on PATH (the comparison is Node 22 and has no dependencies)."

args=("${A}" "${B}")
[ -n "${OUT:-}" ] && args+=(--out "${OUT}")
exec node "$COMPARE" "${args[@]}"
