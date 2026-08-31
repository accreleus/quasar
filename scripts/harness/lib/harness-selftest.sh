#!/usr/bin/env bash
# scripts/harness/lib/harness-selftest.sh — proves scripts/harness/lib/harness.sh works, standalone.
#
# No stack, no docker, no network — just bash + python3 + base64. Every
# scenario runs `harness.sh` in its own subshell/process (harness_report and
# the crash-safety trap both call `exit`, so a scenario has to be isolated or
# it would end this self-test early).
#
# Run:  bash scripts/harness/lib/harness-selftest.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
LIB="$ROOT/scripts/harness/lib/harness.sh"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/harness-selftest.XXXXXX")"
trap 'rm -rf "$WORKDIR"' EXIT

SELF_PASS=0
SELF_FAIL=0
ok()  { SELF_PASS=$((SELF_PASS + 1)); printf 'ok   %s\n' "$*"; }
bad() { SELF_FAIL=$((SELF_FAIL + 1)); printf 'FAIL %s\n' "$*" >&2; }

[ -f "$LIB" ] || { echo "FATAL: $LIB not found" >&2; exit 2; }

# ── 0. sourcing has no side effects ─────────────────────────────────────────
BEFORE=$( { ls "$ROOT/deploy/results" 2>/dev/null || true; } | wc -l | tr -d ' ')
( set -euo pipefail; source "$LIB" )
AFTER=$( { ls "$ROOT/deploy/results" 2>/dev/null || true; } | wc -l | tr -d ' ')
if [ "$BEFORE" = "$AFTER" ]; then
  ok "sourcing harness.sh has no side effects (deploy/results unchanged)"
else
  bad "sourcing harness.sh changed deploy/results ($BEFORE -> $AFTER files)"
fi

# ── 1. mixed pass/fail/skip: counts, exit 1, valid JSON ─────────────────────
RESULTS_DIR="$WORKDIR/results-1"
set +e
OUT=$(HARNESS_RESULTS_DIR="$RESULTS_DIR" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-mixed"
  pass "one"
  pass "two"
  fail "three"
  skip "four"
  harness_note "example_key" "example_value"
  harness_report
')
RC=$?
set -e
[ "$RC" -eq 1 ] && ok "mixed scenario exits 1 (had a FAIL)" || bad "mixed scenario exit=$RC (want 1)"
if printf '%s\n' "$OUT" | grep -qE '^PASS: 2 FAIL: 1 SKIP: 1$'; then
  ok "summary line 'PASS: 2 FAIL: 1 SKIP: 1' present"
else
  bad "summary line missing/wrong. Got:
$OUT"
fi

JSONF=$(ls "$RESULTS_DIR"/selftest-mixed-*.json 2>/dev/null | head -1 || true)
if [ -n "$JSONF" ]; then
  ok "JSON report written: $JSONF"
  if python3 - "$JSONF" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d["counts"] == {"pass": 2, "fail": 1, "skip": 1}, d["counts"]
assert d["name"] == "selftest-mixed", d["name"]
results = [c["result"] for c in d["checks"]]
assert results == ["pass", "pass", "fail", "skip"], results
messages = [c["message"] for c in d["checks"]]
assert messages == ["one", "two", "three", "four"], messages
assert d["notes"].get("example_key") == "example_value", d["notes"]
assert "timestamp" in d and d["timestamp"]
PY
  then
    ok "JSON is valid and counts/checks/notes match"
  else
    bad "JSON validation failed"
  fi
else
  bad "no JSON report file found in $RESULTS_DIR"
fi

# ── 2. all-pass scenario exits 0 ────────────────────────────────────────────
RESULTS_DIR2="$WORKDIR/results-2"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR2" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-allpass"
  pass "a"
  pass "b"
  harness_report
' >/dev/null
RC2=$?
set -e
[ "$RC2" -eq 0 ] && ok "all-pass scenario exits 0" || bad "all-pass scenario exit=$RC2 (want 0)"

# ── 3. skip-only scenario exits 0 (a skip is not a fail) ────────────────────
RESULTS_DIR3="$WORKDIR/results-3"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR3" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-skiponly"
  skip "unavailable"
  harness_report
' >/dev/null
RC3=$?
set -e
[ "$RC3" -eq 0 ] && ok "skip-only scenario exits 0 (skip != fail)" || bad "skip-only scenario exit=$RC3 (want 0)"
JSONF3=$(ls "$RESULTS_DIR3"/selftest-skiponly-*.json 2>/dev/null | head -1 || true)
if [ -n "$JSONF3" ] && python3 -c "
import json, sys
d = json.load(open('$JSONF3'))
assert d['counts'] == {'pass': 0, 'fail': 0, 'skip': 1}, d['counts']
" 2>/dev/null; then
  ok "skip-only JSON counts are {pass:0, fail:0, skip:1}"
else
  bad "skip-only JSON counts wrong or file missing ($JSONF3)"
fi

# ── 4. crash safety net: script dies without calling harness_report ─────────
RESULTS_DIR4="$WORKDIR/results-4"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR4" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-crash"
  pass "before crash"
  false   # trips set -e -> the EXIT trap must still write the report
' >/dev/null 2>&1
RC4=$?
set -e
JSONF4=$(ls "$RESULTS_DIR4"/selftest-crash-*.json 2>/dev/null | head -1 || true)
[ "$RC4" -ne 0 ] && ok "crashed script exits non-zero (rc=$RC4)" || bad "crashed script exit=$RC4 (want non-zero)"
if [ -n "$JSONF4" ]; then
  ok "crash safety net wrote a report anyway: $JSONF4"
else
  bad "no report written after an uncaught crash"
fi

# ── 5. auto-report via natural end (no explicit harness_report call) ────────
# 5a: a recorded FAIL with no explicit harness_report call must still exit 1.
RESULTS_DIR5A="$WORKDIR/results-5a"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR5A" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-autofail"
  pass "ok one"
  fail "recorded but never reported explicitly"
' >/dev/null
RC5A=$?
set -e
[ "$RC5A" -eq 1 ] && ok "natural end with a recorded FAIL still exits 1 (auto-report escalates)" \
  || bad "natural end with a FAIL exit=$RC5A (want 1)"

# 5b: natural end with only PASSes exits 0.
RESULTS_DIR5B="$WORKDIR/results-5b"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR5B" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-autopass"
  pass "ok one"
  pass "ok two"
' >/dev/null
RC5B=$?
set -e
[ "$RC5B" -eq 0 ] && ok "natural end with only PASSes exits 0 (no false escalation)" \
  || bad "natural end with only PASSes exit=$RC5B (want 0)"

# ── 6. require() fails fast on a missing dependency ──────────────────────────
REQ_OUT="$WORKDIR/require.out"
set +e
bash -c '
  source "'"$LIB"'"
  require this-command-does-not-exist-anywhere-xyz
  echo "UNREACHABLE"
' >"$REQ_OUT" 2>&1
RC6=$?
set -e
if [ "$RC6" -ne 0 ] && ! grep -q UNREACHABLE "$REQ_OUT"; then
  ok "require() exits non-zero and short-circuits on a missing command"
else
  bad "require() did not fail fast (rc=$RC6): $(cat "$REQ_OUT")"
fi

# ── 7. results path respects a caller-supplied name and default location ────
RESULTS_DIR7="$WORKDIR/results-7"
HARNESS_RESULTS_DIR="$RESULTS_DIR7" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-naming"
  pass "x"
  harness_report
' >/dev/null 2>&1 || true
if ls "$RESULTS_DIR7"/selftest-naming-*.json >/dev/null 2>&1; then
  ok "results file is named <name>-<ts>.json under HARNESS_RESULTS_DIR"
else
  bad "results file not found with the expected naming convention"
fi

# ── 8. crash under a custom EXIT trap must not report PASS/exit 0 ───────────
# This is the #383-review finding: a script installs its own `trap cleanup
# EXIT` (as harness.sh's own doc block tells authors to do for extra
# teardown), `cleanup` runs several commands that reset $?, and only THEN
# calls harness_report. A bare `harness_report` at that point would see the
# wrong (zero) $? and silently report PASS. The documented fix is for cleanup
# to capture `local rc=$?` on ITS OWN first line and pass it through:
# `harness_report "$rc"`.
RESULTS_DIR8="$WORKDIR/results-8"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR8" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-cleanup-crash"
  cleanup() {
    local rc=$?
    # busywork that changes $? before harness_report is reached, simulating a
    # real cleanup (session teardown, DB restore, etc.) — proves the FIX does
    # not depend on harness_report happening to be the first thing that runs.
    true
    false || true
    harness_report "$rc"
  }
  trap cleanup EXIT
  pass "before crash"
  false   # trips set -e -> cleanup -> harness_report "$rc" must preserve rc=1
' >/dev/null 2>&1
RC8=$?
set -e
[ "$RC8" -ne 0 ] && ok "crash under a custom EXIT trap exits non-zero even with zero recorded FAILs (rc=$RC8)" \
  || bad "crash under a custom EXIT trap exit=$RC8 (want non-zero) — a mid-script crash was silently reported PASS"
JSONF8=$(ls "$RESULTS_DIR8"/selftest-cleanup-crash-*.json 2>/dev/null | head -1 || true)
if [ -n "$JSONF8" ] && python3 -c "
import json, sys
d = json.load(open('$JSONF8'))
assert d['counts'] == {'pass': 1, 'fail': 0, 'skip': 0}, d['counts']
" 2>/dev/null; then
  ok "crash report has zero recorded FAILs (proving the non-zero exit came from the preserved crash rc, not a FAIL count)"
else
  bad "crash report missing or counts unexpected ($JSONF8)"
fi

# ── 9. harness_report with no argument still defaults to $? (unchanged
#    behavior for the common case: called directly, not from a custom trap) ─
RESULTS_DIR9="$WORKDIR/results-9"
set +e
HARNESS_RESULTS_DIR="$RESULTS_DIR9" bash -c '
  set -euo pipefail
  source "'"$LIB"'"
  harness_init "selftest-report-noarg"
  pass "one"
  fail "two"
  harness_report
' >/dev/null 2>&1
RC9=$?
set -e
[ "$RC9" -eq 1 ] && ok "harness_report with no arg still exits 1 on a recorded FAIL (default-arg path unaffected by the fix)" \
  || bad "harness_report with no arg exit=$RC9 (want 1)"

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "harness-selftest: PASS=$SELF_PASS FAIL=$SELF_FAIL"
[ "$SELF_FAIL" -eq 0 ]
