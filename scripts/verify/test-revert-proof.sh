#!/usr/bin/env bash
# Offline self-test for scripts/verify/revert-proof.sh, on throwaway fixture repositories:
# a fix whose test fails on base (proved), one whose test passes on base (unproved), one
# whose test does not compile on base (unproved, compile), the exemptions, and a tooling
# failure that must say so rather than pass. Needs git, jq and go. The vitest cases need a
# web/node_modules (REVERT_PROOF_NODE_MODULES, else the repo's web/node_modules); without
# one they are skipped, except under CI, where that is a failure.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo/scripts/verify/revert-proof.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() { echo "revert-proof test: $*" >&2; exit 1; }
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
unset GITHUB_STEP_SUMMARY TEST_DATABASE_URL

# fixture <name>: a repo whose base commit has the bug (Add subtracts)
fixture() {
  local d="$work/$1"
  mkdir -p "$d/calc" && cd "$d"
  git init -q -b base .
  printf 'module example.invalid/fx\n\ngo 1.22\n' > go.mod
  printf 'package calc\n\nfunc Add(a, b int) int { return a - b }\n' > calc/calc.go
  git add -A && git commit -qm base
  git switch -q -c pr
}
fixed() { printf 'package calc\n\nfunc Add(a, b int) int { return a + b }\n%s' "${1:-}" > calc/calc.go; }
gotest() { printf 'package calc\n\nimport "testing"\n\nfunc %s(t *testing.T) {\n\tif %s {\n\t\tt.Fatal("wrong")\n\t}\n}\n' "$1" "$2" > "calc/${3:-calc}_test.go"; }
commit() { git add -A && git commit -qm pr; }
# expect <exit> <text it must print> [env...]: run the script on base..pr
expect() {
  local want="$1" text="$2" out rc=0
  shift 2
  out="$(env "$@" bash "$script" base pr 2>&1)" || rc=$?
  [ "$rc" -eq "$want" ] || fail "$(basename "$PWD"): exit $rc, want $want: $out"
  grep -qF -- "$text" <<<"$out" || fail "$(basename "$PWD"): missing '$text' in: $out"
}
fix=(PR_TITLE="fix(calc): add adds")

fixture proved; fixed; gotest TestAdd 'Add(2, 2) != 4'; commit
expect 0 '| `./calc TestAdd` | pass | fail | **proved** |' "${fix[@]}"
expect 0 'Proved 1, unproved 0' "${fix[@]}" REVERT_PROOF_MODE=enforce

fixture unproved; fixed; gotest TestZero 'Add(0, 0) != 0'; commit
expect 0 '| `./calc TestZero` | pass | pass | UNPROVED: passes on base' "${fix[@]}"
expect 1 'nothing here shows the fix is tested' "${fix[@]}" REVERT_PROOF_MODE=enforce

fixture compile; fixed 'func Sub(a, b int) int { return a - b }'; gotest TestSub 'Sub(3, 1) != 2'; commit
expect 0 '| `./calc TestSub` | pass | compile | UNPROVED (compile)' "${fix[@]}"
expect 1 'unproved (does not compile on base) 1' "${fix[@]}" REVERT_PROOF_MODE=enforce

# Only the edited test runs: an untouched test in the same file is not in the table.
fixture edited; gotest TestOld 'Add(0, 0) != 0'; commit; git switch -q base; git merge -q --ff-only pr; git switch -q pr
fixed; printf '\nfunc TestNew(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal("wrong")\n\t}\n}\n' >> calc/calc_test.go; commit
expect 0 'Proved 1, unproved 0' "${fix[@]}"
out="$(env "${fix[@]}" bash "$script" base pr)"; ! grep -q TestOld <<<"$out" || fail "an unedited test was run: $out"

cd "$work/proved"
expect 0 'Exempt: the title is not a `fix`' PR_TITLE="refactor(calc): tidy" REVERT_PROOF_MODE=enforce
expect 0 'Exempt: `revert-proof: skip` in the PR body: characterises old behaviour' "${fix[@]}" \
  PR_BODY=$'Some text.\r\nrevert-proof: skip characterises old behaviour\r\n' REVERT_PROOF_MODE=enforce
fixture notests; fixed; commit
expect 0 'No new tests' "${fix[@]}" REVERT_PROOF_MODE=enforce
fixture testsonly; gotest TestZero 'Add(0, 0) != 0'; echo note > NOTES.md; commit
expect 0 'Exempt: tests only' "${fix[@]}" REVERT_PROOF_MODE=enforce

# A run that cannot evaluate says so, and fails under enforce.
cd "$work/proved"
nogo="$work/nogo"; mkdir -p "$nogo"
for t in bash git jq tar mktemp grep sed awk dirname env cat head tail rm mkdir cp ln; do ln -sf "$(command -v "$t")" "$nogo/$t"; done
expect 0 'NOT EVALUATED: go is not installed' "${fix[@]}" PATH="$nogo"
expect 1 '::error title=revert proof not evaluated::go is not installed' "${fix[@]}" PATH="$nogo" REVERT_PROOF_MODE=enforce
expect 0 'NOT EVALUATED: REVERT_PROOF_MODE must be warn or enforce' "${fix[@]}" REVERT_PROOF_MODE=strict

# --- vitest ---
nm="${REVERT_PROOF_NODE_MODULES:-$repo/web/node_modules}"
if [ ! -x "$nm/.bin/vitest" ] || [ ! -x "$nm/.bin/tsc" ]; then
  [ -z "${CI:-}" ] || fail "no vitest/tsc in $nm (CI must run npm ci in web/ first)"
  echo "revert-proof test: SKIP vitest cases (no $nm; run npm ci in web/)"
  echo "revert-proof test: ok (Go only)"
  exit 0
fi
webfixture() {
  local d="$work/$1"
  mkdir -p "$d/web/src" && cd "$d"
  git init -q -b base .
  printf '{"compilerOptions":{"strict":true,"noEmit":true,"skipLibCheck":true,"target":"ES2022","module":"ESNext","moduleResolution":"bundler"},"include":["src"]}\n' > web/tsconfig.json
  printf 'export function add(a: number, b: number): number { return a - b; }\n' > web/src/add.ts
  git add -A && git commit -qm base
  git switch -q -c pr
  printf 'export function add(a: number, b: number): number { return a + b; }\n%s' "${2:-}" > web/src/add.ts
}
vitest() { printf 'import { expect, test } from "vitest";\nimport { %s } from "./add";\n\ntest("it", () => { expect(%s).toBe(%s); });\n' "$1" "$2" "$3" > web/src/add.test.ts; commit; }
web=("${fix[@]}" REVERT_PROOF_NODE_MODULES="$nm")

webfixture vproved; vitest add 'add(2, 2)' 4
expect 0 '| `web/src/add.test.ts` | pass | fail | **proved** |' "${web[@]}"
webfixture vunproved; vitest add 'add(0, 0)' 0
expect 0 '| `web/src/add.test.ts` | pass | pass | UNPROVED' "${web[@]}"
webfixture vcompile 'export function sub(a: number, b: number): number { return a - b; }'; vitest sub 'sub(3, 1)' 2
expect 0 '| `web/src/add.test.ts` | pass | compile | UNPROVED (compile)' "${web[@]}"
expect 0 'NOT EVALUATED: vitest files changed but REVERT_PROOF_NODE_MODULES' "${fix[@]}" REVERT_PROOF_NODE_MODULES=

echo "revert-proof test: ok"
