#!/usr/bin/env bash
# revert-proof.sh: does a fix pull request's new test catch the bug it fixes?
#
#   PR_TITLE=... PR_BODY=... scripts/verify/revert-proof.sh <base> <head>
#
# Runs the tests the change adds or edits twice: on <head>, and on <base>'s product code
# with <head>'s test files laid over it. A test that passes on head and fails on base is
# PROVED. One that passes on base too does not catch the bug (UNPROVED). One that does
# not compile on base (it uses a symbol the fix adds) proves nothing either and is counted
# apart, never as proved.
#
# Scope (v1): Go test functions in changed *_test.go files, and changed vitest files under
# web/. Rust tests live inline next to the code they test, so Rust test files (tests/,
# tests.rs, *_test(s).rs) are listed but not run. Without TEST_DATABASE_URL the Go DB tests
# skip, and a skipped test is "not evaluated", never proved.
#
# Exempt: a title that is not a conventional `fix`, a change with no test edits, a change to
# tests only (no product file but Markdown), and a PR body line `revert-proof: skip <reason>`
# (at the start of a line, outside fenced code, with a reason of 10 characters or more).
#
# Environment:
#   REVERT_PROOF_MODE          warn (default: always exit 0) or enforce (exit 1 when nothing
#                              is proved or the run could not be evaluated)
#   TEST_DATABASE_URL          optional Postgres server; every run gets its own database
#   REVERT_PROOF_NODE_MODULES  web/node_modules to use for the vitest runs (needed for those)
#   GITHUB_STEP_SUMMARY        the report is appended there as well as printed
set -uo pipefail   # not -e: a failing test run is data, not an error

mode="${REVERT_PROOF_MODE:-warn}"
title="${PR_TITLE:-}" body="${PR_BODY:-}"
title="${title//$'\r'/}" body="${body//$'\r'/}"   # GitHub bodies come with CRLF
base_ref="${1:-}" head_ref="${2:-}"
rows=() errors=() notes=()
proved=0 unproved=0 compile=0 notevald=0

report() { # report <verdict line>: print the report, then exit by mode
  local out
  out="$(
    echo "## Revert proof"
    for e in "${errors[@]}"; do echo; echo "> **NOT EVALUATED: $e**"; done
    echo; echo "$1"
    if [ "${#rows[@]}" -gt 0 ]; then
      echo; echo "| test | head | base | verdict |"; echo "|---|---|---|---|"
      printf '%s\n' "${rows[@]}"
    fi
    for n in "${notes[@]}"; do echo; echo "- $n"; done
    echo; echo "Mode: $mode."
  )"
  echo "$out"
  [ -n "${GITHUB_STEP_SUMMARY:-}" ] && echo "$out" >> "$GITHUB_STEP_SUMMARY"
  for e in "${errors[@]}"; do echo "::error title=revert proof not evaluated::$e"; done
  [ "$mode" = enforce ] || exit 0
  [ "${#errors[@]}" -eq 0 ] || exit 1
  [ "$2" = ok ] || [ "$proved" -gt 0 ] || exit 1
  exit 0
}
fail_tool() { errors+=("$1"); report "Could not evaluate this change." fail; }

# A typo in the mode (say `enforced`) must not read as a green check: fail closed.
case "$mode" in warn|enforce) ;; *) bad="$mode" mode=enforce; fail_tool "REVERT_PROOF_MODE must be warn or enforce, not '$bad'";; esac
[ -n "$base_ref" ] && [ -n "$head_ref" ] || fail_tool "usage: revert-proof.sh <base> <head>"

if ! grep -qE '^fix(\([^)]*\))?!?:' <<<"$title"; then
  report "Exempt: the title is not a \`fix\` (\`$title\`)." ok
fi
# At the start of a line, outside fenced code, with a reason of 10 characters or more.
skip="$(awk '/^(```|~~~)/ { fence = !fence; next } !fence' <<<"$body" | sed -nE 's/^revert-proof: skip ([^[:space:]].{9,})$/\1/p' | head -1)"
[ -z "$skip" ] || report "Exempt: \`revert-proof: skip\` in the PR body: $skip" ok

for tool in git jq; do command -v "$tool" >/dev/null || fail_tool "$tool is not installed"; done
repo="$(git rev-parse --show-toplevel 2>/dev/null)" || fail_tool "not inside a git repository"
cd "$repo" || fail_tool "cannot enter $repo"
base="$(git rev-parse --verify -q "$base_ref^{commit}")" || fail_tool "unknown base $base_ref"
head="$(git rev-parse --verify -q "$head_ref^{commit}")" || fail_tool "unknown head $head_ref"

# --- split the change into test files and product files ---
kind() { # kind <path> -> go | web | webgpu | rust | data | product
  case "$1" in
    *.webgpu.test.ts) echo webgpu ;;
    *_test.go) echo go ;;
    web/*.test.ts|web/*.test.tsx|web/*/__tests__/*) echo web ;;
    */testdata/*|testdata/*) echo data ;;
    *.rs) case "$1" in */tests/*|*/tests.rs|*_tests.rs|*_test.rs) echo rust ;; *) echo product ;; esac ;;
    *) echo product ;;
  esac
}
changed="$(git diff --name-only --no-renames "$base" "$head")" || fail_tool "git diff $base $head failed"
test_files=() deleted_tests=() go_files=() web_files=() rust_files=() other_test=() products=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  k="$(kind "$f")"
  if [ "$k" = product ]; then case "$f" in *.md) ;; *) products=$((products + 1)) ;; esac; continue; fi
  git cat-file -e "$head:$f" 2>/dev/null || { deleted_tests+=("$f"); continue; }   # proves nothing; gone from base too
  test_files+=("$f")
  case "$k" in
    go) go_files+=("$f") ;;
    web) case "$f" in web/*.test.ts|web/*.test.tsx) web_files+=("$f") ;; *) other_test+=("$f") ;; esac ;;
    rust) rust_files+=("$f") ;;
    webgpu) other_test+=("$f") ;;
  esac
done <<<"$changed"

[ "${#rust_files[@]}" -eq 0 ] || notes+=("Rust test files changed but not run (v1 covers Go and vitest; Rust tests are inline): ${rust_files[*]}")
[ "${#other_test[@]}" -eq 0 ] || notes+=("Test files with no runner here (WebGPU, __tests__ helpers): ${other_test[*]}")
if [ "${#go_files[@]}" -eq 0 ] && [ "${#web_files[@]}" -eq 0 ]; then
  report "No new tests: the change adds or edits no Go or vitest test." ok
fi
grep -q '^control-plane/migrations/' <<<"$changed" && notes+=("The change adds or edits a migration: a DB test that fails on base may fail on the missing schema rather than on the bug.")
# Base and head run the same product code, so every test would pass on both (a flake fix).
[ "$products" -gt 0 ] || report "Exempt: tests only, no product change to revert." ok

# --- which Go tests the change adds or edits ---
funcs() { grep -oE '^func Test[A-Za-z0-9_]*\(([A-Za-z_]+ )?\*testing\.T\)' | sed -E 's/^func ([^(]*)\(.*/\1/'; }
body_of() { awk -v f="$1" 'index($0, "func " f "(") == 1 { on = 1 } on { print } on && /^}/ { exit }'; }
declare -A go_tests=()   # "<module dir>|<package dir relative to it>" -> "TestA|TestB"
for f in "${go_files[@]}"; do
  new="$(git show "$head:$f")" old="$(git show "$base:$f" 2>/dev/null || true)"
  picked=()
  for t in $(funcs <<<"$new"); do
    [ "$(body_of "$t" <<<"$new")" = "$(body_of "$t" <<<"$old")" ] || picked+=("$t")
  done
  # Only helpers or tables changed: every test in the file may depend on them.
  [ "${#picked[@]}" -gt 0 ] || mapfile -t picked < <(funcs <<<"$new")
  [ "${#picked[@]}" -gt 0 ] || continue
  dir="$(dirname "$f")" mod="$dir"
  while [ "$mod" != . ] && ! git cat-file -e "$head:$mod/go.mod" 2>/dev/null; do mod="$(dirname "$mod")"; done
  git cat-file -e "$head:$mod/go.mod" 2>/dev/null || fail_tool "no go.mod above $f"
  rel="${dir#"$mod"/}"; [ "$mod" != . ] || rel="$dir"; [ "$dir" != "$mod" ] || rel=.
  key="$mod|$rel"
  for t in "${picked[@]}"; do go_tests[$key]="${go_tests[$key]:+${go_tests[$key]}|}$t"; done
done

# --- the two trees: head, and base product code with head's test files ---
work="$(cd "$(mktemp -d)" && pwd -P)" || fail_tool "mktemp failed"   # vitest reports real paths
dbn=0
cleanup() {
  rm -rf "$work"
  for ((n = 1; n <= dbn; n++)); do psql "$TEST_DATABASE_URL" -X -q -c "DROP DATABASE IF EXISTS revert_proof_$$_$n" >/dev/null 2>&1; done
}
trap cleanup EXIT
mkdir -p "$work/head" "$work/base" "$work/out"
git archive "$head" | tar -x -C "$work/head" || fail_tool "git archive $head failed"
git archive "$base" | tar -x -C "$work/base" || fail_tool "git archive $base failed"
for f in "${test_files[@]}"; do
  mkdir -p "$work/base/$(dirname "$f")" && git show "$head:$f" > "$work/base/$f" || fail_tool "cannot lay $f over base"
done
for f in "${deleted_tests[@]}"; do rm -f "$work/base/$f"; done   # a renamed test must not stay twice
# Submodules (protocol/) are not in an archive: both trees get the checkout's copy.
# shortcut: a PR that moves a submodule pin is run with head's submodule on both sides.
while IFS= read -r sub; do
  [ -d "$repo/$sub" ] || continue
  for side in head base; do mkdir -p "$work/$side/$sub" && cp -a "$repo/$sub/." "$work/$side/$sub/"; done
done < <(git ls-tree -r "$head" | awk '$2 == "commit" { print $4 }')

fresh_db() { # fresh_db: sets db to the URL of a new empty database (empty without a server)
  db=""
  [ -n "${TEST_DATABASE_URL:-}" ] || return 0
  local url="${TEST_DATABASE_URL%%\?*}" q="" name
  case "$TEST_DATABASE_URL" in *\?*) q="?${TEST_DATABASE_URL#*\?}" ;; esac
  dbn=$((dbn + 1)) name="revert_proof_$$_$dbn"   # not in a subshell: cleanup counts on dbn
  PGOPTIONS=--client-min-messages=warning psql "$TEST_DATABASE_URL" -X -q -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS $name" -c "CREATE DATABASE $name" >/dev/null || return 1
  db="${url%/*}/$name$q"
}

# go_result <json> <test> -> pass | fail | timeout | skip | compile | none
go_result() {
  local a
  a="$(jq -rs --arg t "$2" '[.[] | select(.Test == $t and (.Action | test("^(pass|fail|skip)$")))] | last | .Action // ""' "$1" 2>/dev/null)"
  # A timeout panics the whole binary: no fail event for the test, only its output.
  if jq -e -s --arg t "$2" 'any(.[]; .Test == $t and ((.Output // "") | contains("test timed out")))' "$1" >/dev/null 2>&1; then a=timeout; fi
  if [ -z "$a" ]; then
    if grep -qE '"Action":"build-fail"|\[(build|setup) failed\]' "$1" "${1%.json}.err"; then a=compile; else a=none; fi
  fi
  echo "$a"
}

verdict() { # verdict <name> <head> <base>
  local v
  case "$2/$3" in
    pass/fail) v="**proved**"; proved=$((proved + 1)) ;;
    pass/pass) v="UNPROVED: passes on base, so it does not catch the bug"; unproved=$((unproved + 1)) ;;
    pass/compile) v="UNPROVED (compile): does not compile on base"; compile=$((compile + 1)) ;;
    pass/timeout) v="UNPROVED: timed out on base"; unproved=$((unproved + 1)) ;;
    skip/*|pass/skip) v="skipped, not evaluated"; notevald=$((notevald + 1)) ;;
    pass/none|none/*) v="no result (tooling)"; errors+=("$1 gave no result"); notevald=$((notevald + 1)) ;;
    *) v="not evaluated: $2 on head"; errors+=("$1 does not pass on head ($2)"); notevald=$((notevald + 1)) ;;
  esac
  rows+=("| \`$1\` | $2 | $3 | $v |")
}

if [ "${#go_tests[@]}" -gt 0 ]; then
  command -v go >/dev/null || fail_tool "go is not installed"
  [ -z "${TEST_DATABASE_URL:-}" ] || command -v psql >/dev/null || fail_tool "TEST_DATABASE_URL is set but psql is not installed"
  i=0
  for key in $(printf '%s\n' "${!go_tests[@]}" | sort); do   # keys hold no spaces
    i=$((i + 1)) mod="${key%%|*}" pkg="./${key#*|}"
    for side in head base; do
      fresh_db || fail_tool "could not create a test database"
      (cd "$work/$side/$mod" && TEST_DATABASE_URL="$db" go test -json -count=1 -p 1 -timeout 10m \
        -run "^(${go_tests[$key]})\$" "$pkg/" > "$work/out/go-$i-$side.json" 2> "$work/out/go-$i-$side.err")
    done
    IFS='|' read -r -a names <<<"${go_tests[$key]}"
    for t in "${names[@]}"; do
      verdict "$mod/${key#*|} $t" "$(go_result "$work/out/go-$i-head.json" "$t")" "$(go_result "$work/out/go-$i-base.json" "$t")"
    done
  done
  [ -n "${TEST_DATABASE_URL:-}" ] || notes+=("No TEST_DATABASE_URL: Go DB tests skip and are reported as not evaluated.")
fi

# --- vitest ---
# vitest_result <json> <file> <base type errors: yes|no> <tree> -> pass | fail | timeout | skip | compile | none
vitest_result() {
  local abs="$4/$2"
  if [ "$3" = yes ]; then echo compile; return; fi
  jq -r --arg f "$abs" '
    [.testResults[]? | select(.name == $f)] | first |
    if . == null then "none"
    elif (.assertionResults | length) == 0 then (if .status == "failed" then "compile" else "none" end)
    elif any(.assertionResults[]; .status == "failed" and any(.failureMessages[]?; test("timed out"))) then "timeout"
    elif any(.assertionResults[]; .status == "failed") then "fail"
    elif all(.assertionResults[]; .status | test("^(pending|skipped|todo)$")) then "skip"
    else "pass" end' "$1" 2>/dev/null || echo none
}
if [ "${#web_files[@]}" -gt 0 ]; then
  nm="${REVERT_PROOF_NODE_MODULES:-}"
  if [ -z "$nm" ] || [ ! -d "$nm" ]; then
    fail_tool "vitest files changed but REVERT_PROOF_NODE_MODULES does not name a web/node_modules"
  elif grep -qx 'web/package-lock.json' <<<"$changed"; then
    for f in "${web_files[@]}"; do rows+=("| \`$f\` | - | - | not evaluated: the change moves web dependencies |"); notevald=$((notevald + 1)); done
  else
    rel=("${web_files[@]#web/}") tsc_errors="" err_files=() shared_error=no
    for side in head base; do
      ln -s "$(cd "$nm" && pwd)" "$work/$side/web/node_modules"
      (cd "$work/$side/web" && node_modules/.bin/vitest run --reporter=json --outputFile="$work/out/vitest-$side.json" "${rel[@]}" \
        > "$work/out/vitest-$side.log" 2>&1)
      [ -s "$work/out/vitest-$side.json" ] || fail_tool "vitest wrote no report on $side: $(tail -5 "$work/out/vitest-$side.log" | tr "\n" " ")"
    done
    # A test that imports a symbol the fix adds still runs under vitest (types are stripped),
    # so the typecheck is what tells "does not compile" apart from a real failure. Base's own
    # product code typechecks (CI holds develop to it), so a type error comes from the
    # overlaid tests. One in a *.test.ts(x) file marks that file; one anywhere else (a
    # __tests__ helper any test may import) marks every vitest row of this change.
    if ! tsc_errors="$(cd "$work/base/web" && node_modules/.bin/tsc -b --noEmit 2>&1)"; then
      mapfile -t err_files < <(sed -nE 's#^([^(]+)\([0-9]+,[0-9]+\): error TS.*#web/\1#p' <<<"$tsc_errors" | sort -u)
      [ "${#err_files[@]}" -gt 0 ] || fail_tool "the base typecheck failed without a TS error: $(tail -3 <<<"$tsc_errors" | tr '\n' ' ')"
      for e in "${err_files[@]}"; do case "$e" in *.test.ts|*.test.tsx) ;; *) shared_error=yes ;; esac; done
      notes+=("Base with this change's tests does not typecheck in: ${err_files[*]}")
    fi
    for f in "${web_files[@]}"; do
      typed=$shared_error
      printf '%s\n' "${err_files[@]}" | grep -qxF "$f" && typed=yes
      verdict "$f" "$(vitest_result "$work/out/vitest-head.json" "$f" no "$work/head")" \
        "$(vitest_result "$work/out/vitest-base.json" "$f" "$typed" "$work/base")"
    done
  fi
fi

line="Proved $proved, unproved $unproved, unproved (does not compile on base) $compile, not evaluated $notevald."
[ "$proved" -gt 0 ] || line="$line No added or edited test fails on base: nothing here shows the fix is tested."
report "$line" fail
