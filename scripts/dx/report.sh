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
# Credentials, in order: $BENCH_URL + $BENCH_KEY if set; otherwise the bench
# service's own deploy/.env on HOST (BENCH_API_KEYS=name:secret, first key),
# read over ssh like nightly_budget.sh does. There is no built-in bench address:
# export $QUASAR_BENCH_URL (a deployment's own stable DNS name) so published
# report links survive — a LAN IP pasted into a commit body rots. With it unset,
# or unreachable, the URL is derived as http://<host>:9400 with a WARN.
#
# Exit: 0 ok, 1 failed (RESULT line names why), 2 usage.

set -euo pipefail

# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

VERB="${1:-}"
[ -n "$VERB" ] || dx_guard report "usage: report.sh publish|attach|url (see header)"

DX="$DX_DIR"
ROOT="$(cd "$DX_DIR/.." && pwd)"
QBENCH="$DX/vendor/qbench"
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

# bench_creds — exports BENCH_URL + BENCH_KEY or fails with the next step.
bench_creds() {
  if [ -n "${BENCH_URL:-}" ] && [ -n "${BENCH_KEY:-}" ]; then
    return 0
  fi
  [ "$DX_HOST" != "local" ] || dx_guard report \
    "BENCH_URL + BENCH_KEY are unset and HOST=local has no bench service. Export them, or HOST=<role> to read the service's deploy/.env over ssh"
  dx_resolve_remote "$DX_HOST" || dx_guard report "unknown host '$DX_HOST'"
  local env_file="${BENCH_ENV_FILE:-\$HOME/quasar-bench/deploy/.env}"
  # Spliced into the remote grep UNQUOTED — it has to be, so the remote shell
  # expands the default's leading $HOME. That makes validation the only defence:
  # allow that one expansion, refuse every other shell metacharacter.
  dx_require_safe report "BENCH_ENV_FILE" "$env_file" "$DX_RE_REMOTE_PATH" \
    "It is a path on $DX_HOST, optionally starting with a literal \$HOME."
  local line
  line="$(dx_ssh_remote "grep -h '^BENCH_API_KEYS=' $env_file 2>/dev/null | head -1" || true)"
  [ -n "$line" ] || {
    dx_fail bench-creds "no BENCH_API_KEYS in $env_file on $DX_HOST. Next: export BENCH_URL + BENCH_KEY"
    dx_result report
  }
  local keys="${line#BENCH_API_KEYS=}"
  keys="${keys%%,*}"
  export BENCH_KEY="${keys#*:}"
  if [ -z "${BENCH_URL:-}" ]; then
    # No built-in address: the stable name is a per-deployment fact.
    local stable="${QUASAR_BENCH_URL:-}"
    if [ -n "$stable" ] && curl -fsS -m 5 -o /dev/null "$stable/v1/health" 2>/dev/null; then
      export BENCH_URL="$stable"
    else
      # ssh_alias hosts resolve no DX_REMOTE_HOST; ask ssh what the alias points at.
      local h="${DX_REMOTE_HOST:-}"
      [ -n "$h" ] || h="$(ssh -G "${DX_REMOTE_SSH_ALIAS}" 2>/dev/null | awk '/^hostname /{print $2}')"
      [ -n "$h" ] || dx_guard report "no QUASAR_BENCH_URL and cannot derive the bench host for $DX_HOST; export BENCH_URL"
      if [ -n "$stable" ]; then
        dx_warn bench-url "$stable is unreachable; falling back to the LAN address (published links will rot)"
      else
        dx_warn bench-url "QUASAR_BENCH_URL is unset; using the LAN address (published links will rot)"
      fi
      export BENCH_URL="http://${h}:${BENCH_PORT:-9400}"
    fi
  fi
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
    if URL="$(python3 "$QBENCH" "${args[@]}" 2>&1 | tail -n 1)"; then
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
    if OUT="$(python3 "$QBENCH" "${args[@]}" 2>&1 | tail -n 1)"; then
      dx_pass report-attach "$(basename "$FILE") role=$ROLE"
    else
      dx_fail report-attach "$OUT"
      dx_result report-attach
    fi
    URL="$(python3 "$QBENCH" --url "${BENCH_URL}" report url --repo "$REPO" --commit "$SHA" 2>/dev/null || true)"
    dx_result report-attach "repo=$REPO" "commit=${SHA:0:8}" "role=$ROLE" "url=$URL"
    ;;
  url)
    resolve_commit
    bench_creds
    URL="$(python3 "$QBENCH" --url "${BENCH_URL}" report url --repo "$REPO" --commit "$SHA")"
    dx_pass report-url "$URL"
    dx_result report-url "url=$URL"
    ;;
  *)
    dx_guard report "unknown verb '$VERB' (publish|attach|url)"
    ;;
esac
