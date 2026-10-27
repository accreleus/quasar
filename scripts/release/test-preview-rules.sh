#!/usr/bin/env bash
# Offline table test for scripts/release/preview-rules.sh: which tag a run writes
# the preview manifest to, and that the tag only moves forward. A throwaway git
# repo; no docker, no network.
set -euo pipefail
rules="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/preview-rules.sh"
fail() { echo "preview rules test: $*" >&2; exit 1; }

# ── tag EVENT GIT_REF BRANCH WANT_PREVIEW ─────────────────────────────────────
tag_is() { # tag_is <want> <args...>
  local want=$1 got
  shift
  got="$(bash "$rules" tag "$@")" || fail "tag $* failed"
  [[ "$got" == "$want" ]] || fail "tag $*: got '$got', want '$want'"
}
tag_is preview push refs/heads/develop develop ""
tag_is preview push refs/heads/develop develop false
tag_is "" push refs/heads/main main ""
tag_is "" push refs/tags/v0.5.1 v0.5.1 ""
tag_is "" workflow_dispatch refs/heads/develop develop false
tag_is preview-develop workflow_dispatch refs/heads/develop develop true
tag_is preview-ci-839-preview-builds workflow_dispatch refs/heads/ci/839-preview-builds ci/839-preview-builds true
tag_is "preview-a-b_c.d" workflow_dispatch "refs/heads/a@b_c.d" "a@b_c.d" true
long="$(printf 'x%.0s' {1..140})"
tag_is "preview-$(printf 'x%.0s' {1..100})" workflow_dispatch "refs/heads/$long" "$long" true
# A dispatch can never write the real tag, and a tag ref cannot ask for a preview.
out="$(bash "$rules" tag workflow_dispatch refs/tags/v0.5.1 v0.5.1 true 2>&1)" && fail "a tag dispatch got a preview"
[[ "$out" == *"published from a branch"* ]] || fail "tag dispatch message: $out"
rc=0; bash "$rules" tag push >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "tag with too few arguments must exit 2, got $rc"

# ── forward CURRENT NEW ───────────────────────────────────────────────────────
repo="$(mktemp -d /tmp/quasar-preview-rules.XXXXXX)"
trap 'rm -rf "$repo"' EXIT
cd "$repo"
g() { git -c user.name=t -c user.email=t@example.invalid -c init.defaultBranch=develop -c commit.gpgsign=false "$@"; }
g init -q .
g commit -q --allow-empty -m A; a="$(git rev-parse HEAD)"
g commit -q --allow-empty -m B; b="$(git rev-parse HEAD)"
g checkout -q -b side "$a"
g commit -q --allow-empty -m C; c="$(git rev-parse HEAD)"

bash "$rules" forward "" "$b" || fail "an unpublished tag must accept any commit"
bash "$rules" forward "$a" "$b" || fail "a newer commit must move the tag"
bash "$rules" forward "$b" "$b" || fail "the same commit must be accepted (a re-run of the newest run)"
refused() { # refused <fragment> CURRENT NEW
  local out
  out="$(bash "$rules" forward "$2" "$3" 2>&1)" && fail "forward $2 $3 was accepted"
  [[ "$out" == *"$1"* ]] || fail "forward $2 $3: missing '$1': $out"
}
refused "refusing to move the preview back" "$b" "$a"
refused "refusing to move the preview back" "$c" "$b"
refused "not in this checkout" "$(printf 'e%.0s' {1..40})" "$b"

echo "Preview rules: PASS"
