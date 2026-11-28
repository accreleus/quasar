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
db_url="${TEST_DATABASE_URL:-}"   # the DB case below uses it; every other case runs without
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
# The skip line counts only at the start of a line, outside code, with a real reason.
expect 0 '**proved**' "${fix[@]}" PR_BODY=$'```\nrevert-proof: skip quoted in an example\n```'
expect 0 '**proved**' "${fix[@]}" PR_BODY=$'    revert-proof: skip indented, so code'
expect 0 '**proved**' "${fix[@]}" PR_BODY='revert-proof: skip .'

# A renamed test file is not left behind on base (its helper would be declared twice).
fixture renamed
printf 'package calc\n\nimport "testing"\n\nfunc two() int { return 2 }\n\nfunc TestOld(t *testing.T) {\n\tif two() != 2 {\n\t\tt.Fatal("wrong")\n\t}\n}\n' > calc/old_test.go
commit; git switch -q base; git merge -q --ff-only pr; git switch -q pr
fixed; git mv calc/old_test.go calc/new_test.go
printf '\nfunc TestNew(t *testing.T) {\n\tif Add(two(), 2) != 4 {\n\t\tt.Fatal("wrong")\n\t}\n}\n' >> calc/new_test.go; commit
expect 0 '| `./calc TestNew` | pass | fail | **proved** |' "${fix[@]}"

# A test that does not pass on head is a loud "not evaluated", even beside a proved one.
fixture headbroken; fixed; gotest TestAdd 'Add(2, 2) != 4'; mkdir other
printf 'package other\n\nimport "testing"\n\nfunc TestBroken(t *testing.T) { undefinedThing() }\n' > other/other_test.go; commit
expect 1 'NOT EVALUATED: ./other TestBroken does not pass on head (compile)' "${fix[@]}" REVERT_PROOF_MODE=enforce

# Every database a run creates is dropped when it ends.
if [ -n "$db_url" ]; then
  cd "$work/proved"
  expect 0 '**proved**' "${fix[@]}" TEST_DATABASE_URL="$db_url"
  left="$(psql "$db_url" -X -At -c "SELECT count(*) FROM pg_database WHERE datname LIKE 'revert\_proof\_%'")"
  [ "$left" = 0 ] || fail "$left revert_proof database(s) left behind"
else
  [ -z "${CI:-}" ] || fail "no TEST_DATABASE_URL for the database case (CI must pass it)"
  echo "revert-proof test: SKIP the database case (no TEST_DATABASE_URL)"
fi

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
expect 1 "NOT EVALUATED: REVERT_PROOF_MODE must be warn or enforce, not 'enforced'" "${fix[@]}" REVERT_PROOF_MODE=enforced

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
  printf 'export function add(a: number, b: number): number { return a - b; }\n%s' "${3:-}" > web/src/add.ts
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

# A test helper that imports a symbol the fix adds does not compile on base either.
webfixture vhelper 'export function fixOnly(): number { return 4; }'; mkdir -p web/src/__tests__
printf 'import { fixOnly } from "../add";\nexport const four = (): number => fixOnly();\n' > web/src/__tests__/helper.ts
printf 'import { expect, test } from "vitest";\nimport { four } from "./__tests__/helper";\n\ntest("it", () => { expect(four()).toBe(4); });\n' > web/src/add.test.ts; commit
expect 0 '| `web/src/add.test.ts` | pass | compile | UNPROVED (compile)' "${web[@]}"
# A type error in one test file does not mark another test file.
webfixture vmixed 'export function sub(a: number, b: number): number { return a - b; }'
printf 'import { expect, test } from "vitest";\nimport { add } from "./add";\n\ntest("it", () => { expect(add(2, 2)).toBe(4); });\n' > web/src/two.test.ts
vitest sub 'sub(3, 1)' 2
expect 0 '| `web/src/two.test.ts` | pass | fail | **proved** |' "${web[@]}"
expect 0 '| `web/src/add.test.ts` | pass | compile | UNPROVED (compile)' "${web[@]}"
# A test file used as a helper (it imports a symbol the fix adds) marks its importers too.
webfixture vtesthelper 'export function fixOnly(): number { return 4; }'
printf 'import { expect, test } from "vitest";\nimport { fixOnly } from "./add";\nexport const four = (): number => fixOnly();\ntest("noop", () => { expect(1).toBe(1); });\n' > web/src/shared.test.ts
printf 'import { expect, test } from "vitest";\nimport { four } from "./shared.test";\n\ntest("it", () => { expect(four()).toBe(4); });\n' > web/src/add.test.ts; commit
expect 0 '| `web/src/add.test.ts` | pass | compile | UNPROVED (compile)' "${web[@]}"
# A timeout on base is not a proof.
webfixture vtimeout 'export const slow = (): Promise<number> => Promise.resolve(4);' 'export const slow = (): Promise<number> => new Promise(() => {});'
printf 'import { expect, test } from "vitest";\nimport { slow } from "./add";\n\ntest("it", async () => { expect(await slow()).toBe(4); }, 300);\n' > web/src/add.test.ts; commit
expect 0 '| `web/src/add.test.ts` | pass | timeout | UNPROVED: timed out on base' "${web[@]}"
# A test skipped on head proves nothing, whatever it does on base.
webfixture vskip
printf 'import { expect, test } from "vitest";\nimport { add } from "./add";\n\ntest.skipIf(add(2, 2) === 4)("it", () => { expect(add(2, 2)).toBe(4); });\n' > web/src/add.test.ts; commit
expect 0 '| `web/src/add.test.ts` | skip | fail | skipped, not evaluated |' "${web[@]}"

echo "revert-proof test: ok"
