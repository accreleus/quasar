#!/usr/bin/env bash
# The two rules the preview manifest's tag lives by (#839; protocol/control-api.md
# "Platform-release preview channel"). The Images workflow calls both, so they are
# tested here rather than read off the YAML (scripts/release/test-preview-rules.sh).
#
#   tag EVENT GIT_REF BRANCH WANT_PREVIEW
#       Prints the tag this run publishes the preview manifest to, or nothing.
#       `preview` only on a push to develop. A dispatch with the `preview` input
#       ticked gets a test tag, `preview-<branch>`, which no control plane reads.
#       A dispatch on a tag that asks for one is refused.
#
#   forward CURRENT NEW
#       Exit 0 when the tag may move from CURRENT (the source_commit it names now;
#       "" = not published) to NEW: CURRENT is empty, or an ancestor of NEW, or NEW.
#       The tag only moves forward, so re-running an older run cannot rewind it.
#       Run inside the checkout; it needs the history (`fetch-depth: 0`).
#
# Exit codes: 0 yes / printed · 1 refused · 2 usage.
set -euo pipefail

usage() {
  echo "usage: preview-rules.sh tag EVENT GIT_REF BRANCH WANT_PREVIEW | forward CURRENT NEW" >&2
  exit 2
}

case "${1:-}" in
  tag)
    (($# == 5)) || usage
    event=$2 ref=$3 branch=$4 want=$5
    if [[ "$event" == push && "$ref" == refs/heads/develop ]]; then
      echo preview
    elif [[ "$event" == workflow_dispatch && "$want" == true ]]; then
      [[ "$ref" == refs/heads/* ]] || {
        echo "preview-rules: a test preview is published from a branch, not $ref" >&2
        exit 1
      }
      printf 'preview-%s\n' "$(printf '%s' "$branch" | tr -c 'A-Za-z0-9_.-' '-' | cut -c1-100)"
    fi
    ;;
  forward)
    (($# == 3)) || usage
    current=$2 new=$3
    [[ -n "$current" ]] || exit 0
    if ! git cat-file -e "${current}^{commit}" 2>/dev/null; then
      echo "preview-rules: the preview names $current, which is not in this checkout, so $new cannot be shown to be newer (was the branch rewritten?)" >&2
      exit 1
    fi
    rc=0
    git merge-base --is-ancestor "$current" "$new" || rc=$?
    case "$rc" in
      0) exit 0 ;;
      1) echo "preview-rules: refusing to move the preview back: it names $current, which is not an ancestor of $new (a re-run of an older run, or a rewritten branch). It only moves forward." >&2
         exit 1 ;;
      *) echo "preview-rules: cannot compare $current with $new" >&2
         exit 1 ;;
    esac
    ;;
  *) usage ;;
esac
