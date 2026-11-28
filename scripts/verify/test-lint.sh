#!/usr/bin/env bash
# Table test for scripts/verify/lint.sh, on a copy of it with planted checks.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp "$repo/scripts/verify/lint.sh" "$work/"
fail() { echo "lint test: $*" >&2; exit 1; }

plant() { printf '#!/bin/sh\n%s\n' "$2" > "$work/lint.d/$1.sh"; chmod +x "$work/lint.d/$1.sh"; }
reset() { rm -rf "$work/lint.d"; mkdir "$work/lint.d"; }
expect() { # expect <exit> <line it must print>
  local out rc=0
  out="$(bash "$work/lint.sh" 2>&1)" || rc=$?
  [ "$rc" -eq "$1" ] || fail "exit $rc, want $1: $out"
  grep -qxF -- "$2" <<<"$out" || fail "missing line '$2' in: $out"
}

expect 1 "FAIL lint: no checks in $work/lint.d"   # no directory
reset; expect 1 "FAIL lint: no checks in $work/lint.d"
plant a 'echo ok'; expect 0 "PASS a"
plant b 'echo broken; exit 3'; plant c 'echo ok'
expect 1 "FAIL b (exit 3)"
expect 1 "PASS c"                                 # a failure does not stop the rest
reset; plant a 'true'; expect 1 "FAIL a (printed nothing)"
reset; plant a 'echo ok'; chmod -x "$work/lint.d/a.sh"; expect 1 "FAIL a (not executable)"
reset; plant a 'no-such-tool-here'; expect 1 "FAIL a (exit 127)"
echo "lint test: ok"
