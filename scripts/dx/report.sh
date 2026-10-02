#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/report.sh — publish completion reports and evidence to quasar-bench.
#
#   report.sh publish  REPORT=<file> TITLE=<text> [COMMIT=HEAD] [REPO=accreleus/quasar]
#                      [BRANCH=<auto>] [SUMMARY=<text>] [ISSUES="512 513"] [PRS="7"]
#                      [RUNS="<run-id> ..."] [TAGS="k=v ..."] [PIN=1]
#   report.sh attach   COMMIT=<sha> FILE=<path> [ROLE=screenshot|video|log|bundle|other]
#                      [CAPTION=<text>] [REPO=...]
#   report.sh url      COMMIT=<sha> [REPO=...]
#
# The report is keyed by REPO + COMMIT (the merge SHA). Re-publishing the same
# key replaces the body and keeps the attachments. The RESULT line carries the
# stable URL so it can be pasted into the commit body, the issue and memory.
#
# The server and key come from wherever qbench finds them: BENCH_URL / BENCH_KEY,
# else qbench's own config (${XDG_CONFIG_HOME:-~/.config}/qbench/{url,key}, which
# the bench server's install.sh writes). There is no built-in address and no
# host-derived fallback; with neither set this stops and says to run
# `qbench doctor`. HOST is not used.
#
# The CLI is an installed `qbench` when there is one, else the vendored copy
# (scripts/dx/vendor/qbench); QBENCH=<path> overrides both.
#
# Exit: 0 ok, 1 failed (RESULT line names why), 2 usage.

set -euo pipefail

# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

VERB="${1:-}"
[ -n "$VERB" ] || dx_guard report "usage: report.sh publish|attach|url (see header)"

ROOT="$(cd "$DX_DIR/.." && pwd)"
REPO="${REPO:-accreleus/quasar}"
COMMIT="${COMMIT:-HEAD}"

# resolve_commit — sets SHA (not a subshell: dx_guard must exit the script).
resolve_commit() {
  if [ "$REPO" = "accreleus/quasar" ]; then
    SHA="$(git -C "$ROOT" rev-parse --verify "$COMMIT^{commit}" 2>/dev/null || true)"
    [ -n "$SHA" ] || dx_guard report "COMMIT=$COMMIT is not a commit in this checkout"
  else
    # Another repo: the caller must hand us the full sha.
    case "$COMMIT" in
      *[!0-9a-f]*|'') dx_guard report "COMMIT must be a full 40-char sha for REPO=$REPO" ;;
    esac
    [ ${#COMMIT} -eq 40 ] || dx_guard report "COMMIT must be a full 40-char sha for REPO=$REPO"
    SHA="$COMMIT"
  fi
}

# bench_creds — BENCH_URL + BENCH_KEY from the env or qbench's config, or stop.
# `bench_creds url-only` needs just the server: `report url` makes no request.
bench_creds() {
  if [ "${1:-}" = url-only ]; then
    dx_bench_env || true
    [ -n "${BENCH_URL:-}" ] || dx_bench_require report
  else
    dx_bench_require report
  fi
  dx_qbench
}

case "$VERB" in
  publish)
    REPORT="${REPORT:-}"; TITLE="${TITLE:-}"
    [ -n "$REPORT" ] && [ -n "$TITLE" ] || dx_guard report "publish needs REPORT=<file> TITLE=<text>"
    [ -f "$REPORT" ] || dx_guard report "REPORT=$REPORT is not a file"
    resolve_commit
    bench_creds
    BRANCH="${BRANCH:-}"
    if [ -z "$BRANCH" ] && [ "$REPO" = "accreleus/quasar" ]; then
      BRANCH="$(git -C "$ROOT" branch --show-current 2>/dev/null || true)"
    fi
    case "$REPORT" in
      *.html|*.htm) MIME=text/html ;;
      *.md)         MIME=text/markdown ;;
      *)            MIME=text/plain ;;
    esac
    args=(report put --repo "$REPO" --commit "$SHA" --title "$TITLE" --body "$REPORT" --body-mime "$MIME")
    [ -z "$BRANCH" ] || args+=(--branch "$BRANCH")
    [ -z "${SUMMARY:-}" ] || args+=(--summary "$SUMMARY")
    for n in ${ISSUES:-}; do args+=(--issue "$n"); done
    for n in ${PRS:-}; do args+=(--pr "$n"); done
    for r in ${RUNS:-}; do args+=(--run "$r"); done
    for t in ${TAGS:-}; do args+=(--tag "$t"); done
    [ "${PIN:-0}" != 1 ] || args+=(--pin)
    if URL="$("${DX_QBENCH[@]}" "${args[@]}" 2>&1 | tail -n 1)"; then
      case "$URL" in
        http*) dx_pass report-publish "$URL" ;;
        *) dx_fail report-publish "$URL"; dx_result report-publish ;;
      esac
    else
      dx_fail report-publish "$URL"
      dx_result report-publish
    fi
    dx_result report-publish "repo=$REPO" "commit=${SHA:0:8}" "url=$URL"
    ;;
  attach)
    FILE="${FILE:-}"
    [ -n "$FILE" ] && [ "${COMMIT:-HEAD}" != "" ] || dx_guard report "attach needs COMMIT=<sha> FILE=<path>"
    [ -f "$FILE" ] || dx_guard report "FILE=$FILE is not a file"
    resolve_commit
    bench_creds
    ROLE="${ROLE:-}"
    if [ -z "$ROLE" ]; then
      case "$FILE" in
        *.png|*.jpg|*.jpeg|*.webp|*.gif) ROLE=screenshot ;;
        *.mp4|*.webm|*.mkv|*.mov)       ROLE=video ;;
        *.log|*.txt|*.jsonl)            ROLE=log ;;
        *.tar.gz|*.tgz|*.zip|*.json)    ROLE=bundle ;;
        *)                              ROLE=other ;;
      esac
    fi
    args=(report attach --repo "$REPO" --commit "$SHA" --file "$FILE" --role "$ROLE")
    [ -z "${CAPTION:-}" ] || args+=(--caption "$CAPTION")
    if OUT="$("${DX_QBENCH[@]}" "${args[@]}" 2>&1 | tail -n 1)"; then
      dx_pass report-attach "$(basename "$FILE") role=$ROLE"
    else
      dx_fail report-attach "$OUT"
      dx_result report-attach
    fi
    URL="$("${DX_QBENCH[@]}" report url --repo "$REPO" --commit "$SHA" 2>/dev/null || true)"
    dx_result report-attach "repo=$REPO" "commit=${SHA:0:8}" "role=$ROLE" "url=$URL"
    ;;
  url)
    resolve_commit
    bench_creds url-only
    URL="$("${DX_QBENCH[@]}" report url --repo "$REPO" --commit "$SHA")"
    dx_pass report-url "$URL"
    dx_result report-url "url=$URL"
    ;;
  *)
    dx_guard report "unknown verb '$VERB' (publish|attach|url)"
    ;;
esac
