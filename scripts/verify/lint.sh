#!/usr/bin/env bash
# lint.sh: the one CI entry point for pre-merge checks. Runs every scripts/verify/lint.d/*.sh
# in name order; a new check is a new file there, not an edit to ci.yml.
#
# A check passes when it exits 0 AND prints something. Fails closed: no checks, a check that
# is not executable, or a silent check is a failure.
set -uo pipefail   # not -e: every check runs, then the verdict

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lint.d"
shopt -s nullglob
checks=("$dir"/*.sh)
[ "${#checks[@]}" -gt 0 ] || { echo "FAIL lint: no checks in $dir" >&2; exit 1; }

log="$(mktemp)"
trap 'rm -f "$log"' EXIT
results=() failed=0
for check in "${checks[@]}"; do
  name="$(basename "$check" .sh)" why=""
  printf '\n== %s ==\n' "$name"
  if [ ! -x "$check" ]; then
    why="not executable"
  else
    "$check" </dev/null 2>&1 | tee "$log"
    rc=${PIPESTATUS[0]}
    if [ "$rc" -ne 0 ]; then
      why="exit $rc"
    elif ! grep -q '[^[:space:]]' "$log"; then
      why="printed nothing"
    fi
  fi
  if [ -z "$why" ]; then
    results+=("PASS $name")
  else
    results+=("FAIL $name ($why)")
    failed=$((failed + 1))
  fi
done

printf '\n== lint ==\n'
printf '%s\n' "${results[@]}"
echo "lint: ${#checks[@]} check(s), $failed failed"
[ "$failed" -eq 0 ]
