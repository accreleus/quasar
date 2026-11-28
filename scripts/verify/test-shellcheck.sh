#!/usr/bin/env bash
# Table test for scripts/verify/shellcheck.sh, on a copy of it in a planted repo.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() { echo "shellcheck test: $*" >&2; exit 1; }

mkdir -p "$work/scripts/verify" "$work/site/src/data"
cp "$repo/scripts/verify/shellcheck.sh" "$work/scripts/verify/"
git -C "$work" init -q
baseline() { printf '# header\n%s' "$1" > "$work/scripts/verify/shellcheck.baseline"; }
renders() { # renders <body of the one generated script; empty writes none>
  printf 'import { writeFileSync } from "node:fs";\nconst b = %s;\nif (b) writeFileSync(process.argv[2] + "/g.sh", "#!/bin/sh\\n" + b + "\\n");\n' \
    "'$1'" > "$work/site/src/data/write-fixtures.mjs"
}
expect() { # expect <exit> <text it must print>
  local out rc=0
  git -C "$work" add -A
  out="$(bash "$work/scripts/verify/shellcheck.sh" 2>&1)" || rc=$?
  [ "$rc" -eq "$1" ] || fail "exit $rc, want $1: $out"
  grep -qF -- "$2" <<<"$out" || fail "missing '$2' in: $out"
}

renders 'echo ok'; baseline ''
expect 0 "no new findings"
# An extensionless script is found by its shebang; `cd` with no `|| exit` is SC2164.
printf '#!/usr/bin/env bash\ncd /nowhere\n' > "$work/hook"
expect 1 "> hook SC2164 1"
baseline $'hook SC2164 1\n'; expect 0 "no new findings"
printf 'cd /elsewhere\n' >> "$work/hook"; expect 1 "> hook SC2164 2"      # a second one in a baselined file
printf '#!/usr/bin/env bash\ntrue\n' > "$work/hook"; expect 1 "< hook SC2164 1"  # the baseline only shrinks
baseline ''
renders 'cd /nowhere'; expect 1 "g.sh:2:1:"
renders ''; expect 1 "wrote no scripts"
renders 'echo ok'
printf '#!/bin/sh\nexit 125\n' > "$work/docker"; chmod +x "$work/docker"
PATH="$work:$PATH" expect 1 "shellcheck did not run (exit 125)"
echo "shellcheck test: ok"
