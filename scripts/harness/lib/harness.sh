# shellcheck shell=bash
# scripts/harness/lib/harness.sh — reusable pass/fail/skip/report core for scripts/harness/run-*.sh
# and scripts/harness/checks/*.sh harnesses.
#
# Every harness in this tree used to reimplement the same counter+report block
# under a different name (TOTAL_PASS, ST_PASS, PASS_COUNT — see run-p3-multihost.sh,
# run-p5-home.sh, run-st-trace.sh). This is the extraction (#383 spec §7.2). New
# scripts use it; existing harnesses are not rewritten here (out of scope, and
# they are green) but can adopt it incrementally.
#
# Sourcing this file has NO side effects (no directories created, no traps
# installed, no output) — only bash variable/function definitions. All state
# is set up by harness_init.
#
# Usage:
#   source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"
#   harness_init "my-check"
#   require curl python3
#   pass "thing worked"
#   fail "thing broke"
#   skip "thing not applicable here"
#   harness_note "api" "$API"
#   harness_report        # prints "PASS: n FAIL: n SKIP: n", writes the JSON
#                          # report, and exits (1 if any FAIL, else 0)
#
# API:
#   harness_init <name>   counters, results path (deploy/results/<name>-<ts>.json),
#                         crash-safety EXIT trap (see below)
#   pass  <msg>           colorized (when stdout is a tty) + counted
#   fail  <msg>           colorized + counted (stderr)
#   skip  <msg>           colorized + counted SEPARATELY — a skip is not a pass,
#                         and the report shows it as its own bucket
#   require <cmd...>      fail fast (exit 127) with a clear message if any of the
#                         given commands is not on PATH
#   harness_note <k> <v>  arbitrary key/value recorded into the JSON report's
#                         "notes" object (last write wins for a repeated key)
#   harness_report [rc]   prints the summary line, writes the JSON report, and
#                         exits with the greater of `rc` (default: $? as seen
#                         on harness_report's own first line) and (1 if
#                         HARNESS_FAIL > 0 else 0) — i.e. a crash already in
#                         flight is never silently downgraded to a PASS exit.
#                         Idempotent — a second call is a no-op (returns 0).
#                         IMPORTANT: if your own cleanup function runs other
#                         commands before calling harness_report (see "Cleanup
#                         trap" below), $? by the time harness_report actually
#                         runs is whatever THOSE commands returned, not the
#                         crash that triggered the trap. Capture `local rc=$?`
#                         on cleanup's OWN first line and pass it explicitly:
#                         `harness_report "$rc"`.
#
# Cleanup trap: harness_init installs its own `trap ... EXIT` as a crash safety
# net — if the script dies without ever calling harness_report (an uncaught
# `set -e` trip, a `die`, Ctrl-C), the trap still writes whatever was recorded
# so far and exits non-zero if anything had failed, matching harness_report's
# own contract.
#
# If your script needs ADDITIONAL teardown (stop sessions it launched, restore
# a DB mutation, remove a temp app), define your own cleanup function and
# `trap cleanup EXIT` AFTER calling harness_init — bash allows only one EXIT
# trap, so yours replaces this one. Call `harness_report` as the LAST line of
# your own cleanup function to still get the summary line + JSON report +
# correct exit code (harness_report is idempotent, so this is always safe even
# if something upstream already called it explicitly).
#
# CRASH SAFETY under a custom trap: capture `local rc=$?` as the very FIRST
# line of your cleanup function (before any other command runs and clobbers
# $?), then call `harness_report "$rc"` at the end — NOT a bare
# `harness_report`. A bare call sees whatever your own cleanup's last command
# returned, which is virtually never the crash's exit status, so a `set -e`
# trip mid-script with zero recorded FAILs would otherwise print
# "PASS: n FAIL: 0" and exit 0 — a silently-passing crash.
#
# Portable: bash 3.2 (macOS) and bash 5 (Linux). No associative arrays, no
# `local -n`, no `${var,,}`, no `mapfile`. Requires python3 (JSON) and base64
# (both already required across every existing deploy/*.sh harness).

# ── State (declared at source time so pass/fail/skip never see an unset var
#    under `set -u`, even if called before harness_init by mistake) ───────────
HARNESS_NAME=""
HARNESS_TS=""
HARNESS_TS_ISO=""
HARNESS_ROOT=""
HARNESS_RESULTS_FILE=""
HARNESS_PASS=0
HARNESS_FAIL=0
HARNESS_SKIP=0
HARNESS_COLOR=0
HARNESS_CHECK_RESULTS=()
HARNESS_CHECK_MESSAGES=()
HARNESS_NOTE_KEYS=()
HARNESS_NOTE_VALUES=()
_HARNESS_INITED=0
_HARNESS_REPORTED=0

# Colors resolved once, at source time, based on whether stdout is a tty.
if [ -t 1 ]; then
  _harness_c_green='\033[0;32m'
  _harness_c_red='\033[0;31m'
  _harness_c_yellow='\033[0;33m'
  _harness_c_cyan='\033[0;36m'
  _harness_c_reset='\033[0m'
else
  _harness_c_green=''
  _harness_c_red=''
  _harness_c_yellow=''
  _harness_c_cyan=''
  _harness_c_reset=''
fi

# harness_init <name> — set up counters, the results-file path, and the crash
# safety net. Call this before any pass/fail/skip/harness_note/harness_report.
harness_init() {
  HARNESS_NAME="${1:?harness_init requires a name}"
  HARNESS_TS="$(date -u +%Y%m%dT%H%M%SZ)"
  HARNESS_TS_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  HARNESS_PASS=0
  HARNESS_FAIL=0
  HARNESS_SKIP=0
  HARNESS_CHECK_RESULTS=()
  HARNESS_CHECK_MESSAGES=()
  HARNESS_NOTE_KEYS=()
  HARNESS_NOTE_VALUES=()
  _HARNESS_INITED=1
  _HARNESS_REPORTED=0

  local lib_dir
  lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  HARNESS_ROOT="$(cd "$lib_dir/../../.." && pwd)"
  # HARNESS_RESULTS_DIR may already be set by the caller (e.g. a self-test
  # pointing it at a scratch dir) — an explicit env value always wins.
  HARNESS_RESULTS_DIR="${HARNESS_RESULTS_DIR:-$HARNESS_ROOT/deploy/results}"
  HARNESS_RESULTS_FILE="$HARNESS_RESULTS_DIR/${HARNESS_NAME}-${HARNESS_TS}.json"

  [ -t 1 ] && HARNESS_COLOR=1 || HARNESS_COLOR=0

  trap _harness_autoreport EXIT
}

# require <cmd...> — fail fast (clear message, exit 127) if any command is
# missing from PATH. Intended for the top of a script, before any real work.
require() {
  local missing="" c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
  done
  if [ -n "$missing" ]; then
    printf 'FATAL: missing required command(s):%s\n' "$missing" >&2
    exit 127
  fi
}

pass() {
  local msg="$*"
  HARNESS_PASS=$((HARNESS_PASS + 1))
  HARNESS_CHECK_RESULTS+=("pass")
  HARNESS_CHECK_MESSAGES+=("$msg")
  if [ "$HARNESS_COLOR" = "1" ]; then
    printf "${_harness_c_green}  PASS${_harness_c_reset} %s\n" "$msg"
  else
    printf '  PASS %s\n' "$msg"
  fi
}

fail() {
  local msg="$*"
  HARNESS_FAIL=$((HARNESS_FAIL + 1))
  HARNESS_CHECK_RESULTS+=("fail")
  HARNESS_CHECK_MESSAGES+=("$msg")
  if [ "$HARNESS_COLOR" = "1" ]; then
    printf "${_harness_c_red}  FAIL${_harness_c_reset} %s\n" "$msg" >&2
  else
    printf '  FAIL %s\n' "$msg" >&2
  fi
}

skip() {
  local msg="$*"
  HARNESS_SKIP=$((HARNESS_SKIP + 1))
  HARNESS_CHECK_RESULTS+=("skip")
  HARNESS_CHECK_MESSAGES+=("$msg")
  if [ "$HARNESS_COLOR" = "1" ]; then
    printf "${_harness_c_yellow}  SKIP${_harness_c_reset} %s\n" "$msg"
  else
    printf '  SKIP %s\n' "$msg"
  fi
}

# harness_note <key> <value> — record an arbitrary key/value into the JSON
# report's "notes" object. Repeated keys: last write wins.
harness_note() {
  local k="$1" v="$2"
  HARNESS_NOTE_KEYS+=("$k")
  HARNESS_NOTE_VALUES+=("$v")
}

# _harness_b64 <string> — base64-encode with no embedded newlines, portable
# across GNU and BSD base64 (avoids `base64 -w0`, which BSD/macOS lacks).
_harness_b64() {
  printf '%s' "$1" | base64 | tr -d '\n'
}

# _harness_write_report — build deploy/results/<name>-<ts>.json from the
# recorded checks/notes and print the summary line. Does NOT exit. Idempotent
# guard lives in the two public callers (harness_report / _harness_autoreport).
_harness_write_report() {
  _HARNESS_REPORTED=1

  printf 'PASS: %s FAIL: %s SKIP: %s\n' "$HARNESS_PASS" "$HARNESS_FAIL" "$HARNESS_SKIP"

  local dataf
  dataf="$(mktemp "${TMPDIR:-/tmp}/harness-data.XXXXXX")"
  local i=0
  while [ "$i" -lt "${#HARNESS_CHECK_RESULTS[@]}" ]; do
    printf 'CHECK\t%s\t%s\n' "${HARNESS_CHECK_RESULTS[$i]}" "$(_harness_b64 "${HARNESS_CHECK_MESSAGES[$i]}")" >>"$dataf"
    i=$((i + 1))
  done
  i=0
  while [ "$i" -lt "${#HARNESS_NOTE_KEYS[@]}" ]; do
    printf 'NOTE\t%s\t%s\n' "$(_harness_b64 "${HARNESS_NOTE_KEYS[$i]}")" "$(_harness_b64 "${HARNESS_NOTE_VALUES[$i]}")" >>"$dataf"
    i=$((i + 1))
  done

  mkdir -p "$(dirname "$HARNESS_RESULTS_FILE")"
  python3 - "$dataf" "$HARNESS_RESULTS_FILE" "$HARNESS_NAME" "$HARNESS_TS_ISO" \
    "$HARNESS_PASS" "$HARNESS_FAIL" "$HARNESS_SKIP" <<'PYEOF'
import sys, json, base64

dataf, outf, name, ts, p, f, s = sys.argv[1:8]
checks = []
notes = {}
with open(dataf, encoding="utf-8") as fh:
    for line in fh:
        line = line.rstrip("\n")
        if not line:
            continue
        parts = line.split("\t")
        if parts[0] == "CHECK" and len(parts) == 3:
            result, msg_b64 = parts[1], parts[2]
            msg = base64.b64decode(msg_b64).decode("utf-8", "replace")
            checks.append({"result": result, "message": msg})
        elif parts[0] == "NOTE" and len(parts) == 3:
            k = base64.b64decode(parts[1]).decode("utf-8", "replace")
            v = base64.b64decode(parts[2]).decode("utf-8", "replace")
            notes[k] = v

report = {
    "name": name,
    "timestamp": ts,
    "counts": {"pass": int(p), "fail": int(f), "skip": int(s)},
    "checks": checks,
    "notes": notes,
}
with open(outf, "w", encoding="utf-8") as out:
    json.dump(report, out, indent=2)
    out.write("\n")
PYEOF
  rm -f "$dataf"
  echo "report: $HARNESS_RESULTS_FILE"
}

# harness_report [rc] — the definitive verdict for a script that ran to
# completion. `rc` defaults to $? as observed on this function's OWN first
# line (so a plain top-level `harness_report` call behaves as before); pass it
# explicitly when calling from a custom cleanup trap that ran other commands
# first (see "Cleanup trap" above) so a crash's exit status survives those
# intervening commands instead of being silently discarded.
#
# Exits with the greater of `rc` and (1 if HARNESS_FAIL > 0 else 0) — matching
# _harness_autoreport's contract: a nonzero rc already in flight is preserved
# (never downgraded to 0), and a zero rc is escalated to 1 if anything failed.
# Idempotent: a second call just returns 0 (it does not re-print, re-write, or
# exit again).
harness_report() {
  local rc="${1:-$?}"
  if [ "$_HARNESS_REPORTED" = "1" ]; then
    return 0
  fi
  _harness_write_report
  if [ "$rc" -eq 0 ] && [ "$HARNESS_FAIL" -gt 0 ]; then
    rc=1
  fi
  exit "$rc"
}

# _harness_autoreport — the EXIT-trap safety net installed by harness_init.
# If harness_report was never called, write the report now. Unlike
# harness_report, this PRESERVES a nonzero exit code that was already in
# flight (a real crash), only escalating to 1 when the script would otherwise
# exit 0 despite recorded FAILs (a script that just forgot to call
# harness_report explicitly).
_harness_autoreport() {
  local rc=$?
  if [ "$_HARNESS_REPORTED" != "1" ]; then
    _harness_write_report
    if [ "$rc" -eq 0 ] && [ "$HARNESS_FAIL" -gt 0 ]; then
      rc=1
    fi
  fi
  exit "$rc"
}
