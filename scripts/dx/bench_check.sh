#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/bench_check.sh — the quasar-bench landing gate and review-status read.
#
#   bench_check.sh check  [BASE=<sha>] [WINDOW=impaired] [ARGS='--head <sha> --keys a,b']
#   bench_check.sh status [SPRINT=<slug>] [REPO=accreleus/quasar]
#   make bench-check      [BASE=<sha>] [WINDOW=impaired]
#   make bench-status     [SPRINT=<slug>]
#
# check — `qbench check` from the repo root: judges git HEAD against the nearest
# ancestor commit that has bench runs (or BASE), per scenario that ran at both.
# It posts nothing. Exit codes are qbench's own, passed straight through:
#
#   0  clean       nothing crossed its threshold in any scenario run at both commits
#   3  regressed   a scenario is regressed or mixed. A streaming-path change does
#                  not land until each regressed metric is explained or fixed
#                  (the quasar-bench-regressions skill walks it)
#   4  nothing comparable — NOT a pass. HEAD has no runs, no earlier commit has
#                  any, or no scenario ran at both. The landing summary must say so.
#   5  key missing or rejected — run `qbench doctor`
#   1  any other error (the message names the HTTP status and the server's reason)
#
# `make bench-check` fails on every non-zero exit (make itself then exits 2); the
# RESULT line's `result=` and `rc=` carry which one it was.
#
# status — what is waiting on you when you resume work: with SPRINT, that
# sprint report's review status and unresolved comments (`qbench sprint status`);
# without, the repo's recent sprint reports plus every commit report whose review
# is `changes_requested`. Read-only. Review status is the reviewer's to set,
# never yours.
#
# The server and key resolve the way qbench does: BENCH_URL / BENCH_KEY, else
# ${XDG_CONFIG_HOME:-~/.config}/qbench/{url,key}. No default server. The CLI is an
# installed `qbench`, else scripts/dx/vendor/qbench; QBENCH=<path> overrides.
#
# Exit (status): 0 ok, 1 error, 2 usage, 5 key.

set -uo pipefail

# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
set +e   # after common.sh: the gate must reach its own exit-code mapping

VERB="${1:-check}"
REPO="${REPO:-accreleus/quasar}"
dx_require_safe bench-check REPO "$REPO" '^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$' "It is an OWNER/NAME slug."

# No dx_bench_require here: qbench resolves the server and key by the same rules
# and reports a missing one as exit 5 with the next step, which is the contract.
dx_qbench
cd "$DX_ROOT" || dx_guard "bench-$VERB" "cannot cd to the repo root $DX_ROOT"

case "$VERB" in
  check)
    TARGET=bench-check
    args=(check --repo "$REPO")
    if [ -n "${BASE:-}" ]; then
      dx_require_safe "$TARGET" BASE "$BASE" "$DX_RE_REF" "It is a commit sha or ref."
      args+=(--base "$BASE")
    fi
    if [ -n "${WINDOW:-}" ]; then
      dx_require_safe "$TARGET" WINDOW "$WINDOW" '^[A-Za-z0-9_-]+$' \
        "It is a phase window name: impaired, baseline, recovery, observe, settled, …"
      args+=(--window "$WINDOW")
    fi
    dx_env_argv "$TARGET" ARGS
    args+=(${DX_ARGV[@]+"${DX_ARGV[@]}"})

    dx_info "qbench ${args[*]}   (HEAD $(git rev-parse --short=12 HEAD 2>/dev/null || echo '?'))"
    rc=0
    "${DX_QBENCH[@]}" "${args[@]}" || rc=$?
    printf '\n'
    case "$rc" in
      0) dx_pass bench-check "clean — quote the verdict text above in the landing summary, with its scope (which scenarios ran at both commits)"
         result=clean ;;
      3) dx_fail bench-check "REGRESSED — do not land this streaming-path change until each regressed metric is explained (harness mismatch, noise with evidence) or fixed; the quasar-bench-regressions skill walks it"
         result=regressed ;;
      4) printf '%s\n' \
           "WARN bench-check — ======================================================================" \
           "WARN bench-check — NOTHING COMPARABLE. This is NOT a pass. No verdict exists for this change." \
           "WARN bench-check — Post runs for this commit (every run carries --repo $REPO --commit <sha>)," \
           "WARN bench-check — or pass BASE=<sha> — and if you land anyway, the summary MUST say" \
           "WARN bench-check — \"qbench check: nothing comparable\" rather than implying it passed." \
           "WARN bench-check — ======================================================================" >&2
         result=nothing_comparable ;;
      5) dx_fail bench-check "the bench key is missing or rejected — run \`qbench doctor\`; on a rejected key, ask the operator for one (never retry blindly)"
         result=key ;;
      *) dx_fail bench-check "qbench check failed (rc $rc) — the message above names the HTTP status and the server's reason"
         result=error ;;
    esac
    printf 'RESULT status=%s target=bench-check repo=%s result=%s rc=%d\n' \
      "$([ "$rc" = 0 ] && echo ok || echo failed)" "$REPO" "$result" "$rc"
    exit "$rc"
    ;;
  status)
    TARGET=bench-status
    rc=0
    if [ -n "${SPRINT:-}" ]; then
      dx_require_safe "$TARGET" SPRINT "$SPRINT" '^[A-Za-z0-9._-]+$' "It is a sprint slug such as c15 or 2026-w38."
      "${DX_QBENCH[@]}" sprint status --repo "$REPO" --sprint "$SPRINT" || rc=$?
    else
      dx_info "sprint reports (newest first) — pass SPRINT=<slug> for one report's comments"
      "${DX_QBENCH[@]}" sprint list --repo "$REPO" --limit 10 || rc=$?
      printf '\n'
      dx_info "commit reports waiting on you (review_status=changes_requested)"
      [ "$rc" != 0 ] || "${DX_QBENCH[@]}" report list --repo "$REPO" --review-status changes_requested --limit 20 || rc=$?
    fi
    printf 'RESULT status=%s target=bench-status repo=%s rc=%d\n' \
      "$([ "$rc" = 0 ] && echo ok || echo failed)" "$REPO" "$rc"
    exit "$rc"
    ;;
  *)
    dx_guard bench-check "unknown verb '$VERB' (check|status)"
    ;;
esac
