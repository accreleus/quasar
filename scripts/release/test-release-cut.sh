#!/usr/bin/env bash
# Offline contract test for scripts/release/release-cut.sh (#109). Two halves:
#
#   1. The pure --transform mode — fixture files, no git involved — including
#      feeding its output into scripts/release/changelog-section.sh, the SAME
#      extractor the release-gate job in .github/workflows/images.yml uses, so
#      the two cannot silently drift apart.
#   2. Every git-dependent refusal, exercised against a throwaway repo + a
#      throwaway BARE "origin" under mktemp. Nothing here ever touches this
#      repo's real remote, real main, or real tags.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo/scripts/release/release-cut.sh"
section_script="$repo/scripts/release/changelog-section.sh"
fixtures="$repo/scripts/release/fixtures/changelogs"

fail() { echo "release-cut test: $*" >&2; exit 1; }

test -x "$script" || fail "release-cut.sh not executable"
"$script" --help >/dev/null || fail "--help must exit 0"
"$script" >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "no arguments must exit 2 (usage error)"

# release-cut.sh cuts on main and pushes; it must never merge or otherwise
# touch develop — verified structurally, since the script has no branch logic
# beyond checking it is already on main.
# The header may explain that develop is out of scope; no git INVOCATION may
# ever name it as a ref (checkout/merge/push target).
! grep -Eq '^\s*git .*develop' "$script" || fail "release-cut.sh must never run a git command against develop"

## ── Part 1: the pure transform, no git ──────────────────────────────────────

transform() { # transform <version> <date> [<file>]
  "$script" --transform --version "$1" --date "$2" < "${3:-$fixtures/sections.md}"
}

out="$(transform 0.4.0 2026-09-05)"
[[ "$out" == *$'## Unreleased\n\n## 0.4.0 — 2026-09-05\n\n### Added\n\n- Something not yet released.'* ]] \
  || fail "happy-path transform did not move the Unreleased body under a fresh heading: $out"
[[ "$out" == *'## 0.3.0'* ]] || fail "later sections must survive the rewrite untouched"
[[ "$out" != *'0.4.0-rc.1'* ]] || fail "sanity: fixture drifted (unexpected prerelease heading)"

# Pure function of its inputs: same version/date/file in, byte-identical out.
[[ "$(transform 0.4.0 2026-09-05)" == "$(transform 0.4.0 2026-09-05)" ]] \
  || fail "--transform is not deterministic"

# The output feeds into the SAME extraction the release-gate job uses
# (scripts/release/changelog-section.sh) — this is the drift-proofing the
# ticket asked for: no separately-maintained copy of the heading grammar.
cut_tmp="$(mktemp "${TMPDIR:-/tmp}/quasar-release-cut-fixture.XXXXXX")"
transform 0.4.0 2026-09-05 > "$cut_tmp"
section_out="$("$section_script" 0.4.0 --file "$cut_tmp")"
rm -f "$cut_tmp"
[[ "$section_out" == "### Added

- Something not yet released." ]] || fail "release-gate's own extractor could not read the cut section: $section_out"

reject_transform() { # reject_transform <file> <expected>
  local file=$1 expected=$2 out
  if out=$("$script" --transform --version 0.4.0 --date 2026-09-05 < "$file" 2>&1); then
    fail "unexpected success transforming $file: $out"
  fi
  [[ "$out" == *"$expected"* ]] || fail "missing rejection '$expected' for $file: $out"
}

reject_transform "$fixtures/no-unreleased.md" "no '## Unreleased' heading"
reject_transform "$fixtures/unreleased-empty.md" "is empty"

# Bad semver is refused before either mode does any work.
bad_out="$("$script" --transform --version v1.0 --date 2026-09-05 < "$fixtures/sections.md" 2>&1)" && fail "leading-v version must be refused"
[[ "$bad_out" == *"not strict semver"* ]] || fail "bad semver message missing: $bad_out"

"$script" --transform --date 2026-09-05 < "$fixtures/sections.md" >/dev/null 2>&1 \
  || [[ $? -eq 2 ]] || fail "--transform with no --version must exit 2"

"$script" --transform --version 0.4.0 --nonsense < "$fixtures/sections.md" >/dev/null 2>&1 \
  || [[ $? -eq 2 ]] || fail "an unknown flag must exit 2"

## ── Part 2: git-dependent refusals, throwaway repo + throwaway origin ──────

gitwork="$(mktemp -d "${TMPDIR:-/tmp}/quasar-release-cut-work.XXXXXX")"
gitremote="$(mktemp -d "${TMPDIR:-/tmp}/quasar-release-cut-remote.XXXXXX")"
cleanup() { rm -rf "$gitwork" "$gitremote"; }
trap cleanup EXIT

git init --quiet --bare "$gitremote"
git init --quiet "$gitwork"
git -C "$gitwork" config user.email "release-cut-test@example.invalid"
git -C "$gitwork" config user.name "release-cut test"
git -C "$gitwork" remote add origin "$gitremote"

mkdir -p "$gitwork/scripts/release"
cp "$script" "$gitwork/scripts/release/release-cut.sh"
cp "$fixtures/unreleased-only.md" "$gitwork/CHANGELOG.md"

git -C "$gitwork" checkout -b main --quiet
git -C "$gitwork" add -A
git -C "$gitwork" commit --quiet -m "init"
git -C "$gitwork" push --quiet -u origin main
git -C "$gitwork" tag -a v0.1.0 -m v0.1.0
git -C "$gitwork" push --quiet origin v0.1.0

run() { (cd "$gitwork" && bash scripts/release/release-cut.sh "$@"); }

reject_cut() { # reject_cut <expected> <args...>
  local expected=$1 out
  shift
  if out=$(run "$@" 2>&1); then
    fail "unexpected success for release-cut $*: $out"
  fi
  [[ "$out" == *"$expected"* ]] || fail "missing rejection '$expected' for release-cut $*: $out"
}

# Not on main.
git -C "$gitwork" checkout -b feature --quiet
reject_cut "must be run on main" --version 0.2.0 --dry-run
git -C "$gitwork" checkout main --quiet
git -C "$gitwork" branch -D feature --quiet

# Dirty tree.
echo "dirty" > "$gitwork/untracked.txt"
reject_cut "working tree is not clean" --version 0.2.0 --dry-run
rm -f "$gitwork/untracked.txt"

# Bad semver.
reject_cut "not strict semver" --version v0.2.0 --dry-run

# Equal to the newest existing tag: caught by the explicit exists-check, with
# its own clearer reason, before the precedence check even runs.
reject_cut "v0.1.0 already exists (local or remote tag)" --version 0.1.0 --dry-run

# A prerelease of an ALREADY-RELEASED version is not newer either.
reject_cut "is not strictly newer than the newest existing tag v0.1.0" --version 0.1.0-rc.1 --dry-run

# A tag that exists ONLY on origin (this clone never fetched it locally) must
# still be seen: `git tag -l` alone would report no tags at all here, which is
# exactly the defect this guards against — a clone with an empty local tag
# list must not treat that as "no releases yet".
git -C "$gitwork" tag -d v0.1.0 >/dev/null
[[ -z "$(git -C "$gitwork" tag -l v0.1.0)" ]] || fail "setup: local tag delete did not take"

# (a) remote-only tag, VERSION not newer than it -> refused.
reject_cut "is not strictly newer than the newest existing tag v0.1.0" --version 0.1.0-beta --dry-run

# (b) VERSION equals a tag that exists only on origin -> refused.
reject_cut "v0.1.0 already exists (local or remote tag)" --version 0.1.0 --dry-run

# Restore the local tag for the rest of this test's flow.
git -C "$gitwork" fetch --quiet origin 'refs/tags/v0.1.0:refs/tags/v0.1.0'

# HEAD behind/ahead of origin/main (an unpushed local commit).
echo "local only" > "$gitwork/local.txt"
git -C "$gitwork" add local.txt
git -C "$gitwork" commit --quiet -m "unpushed"
reject_cut "does not match origin/main" --version 0.2.0 --dry-run
git -C "$gitwork" reset --hard --quiet HEAD~1

# An unreachable origin must refuse with a one-line reason, not a raw git
# fatal-error dump — this is what "refuse with a one-line reason if ls-remote
# fails" also depends on: the fetch that runs first has the same discipline.
git -C "$gitwork" remote set-url origin "$gitremote-does-not-exist"
reject_cut "could not fetch origin/main" --version 0.2.0 --dry-run
git -C "$gitwork" remote set-url origin "$gitremote"


# --dry-run performs every check but mutates nothing: still clean, no new tag.
dry_out="$(run --version 0.2.0 --dry-run)"
[[ "$dry_out" == *"## 0.2.0 — "* ]] || fail "dry-run must print the changelog diff: $dry_out"
[[ "$dry_out" == *"git -C"*"push origin HEAD:refs/heads/main 'v0.2.0'"* ]] || fail "dry-run must print the push command: $dry_out"
[[ -z "$(git -C "$gitwork" status --porcelain)" ]] || fail "--dry-run must not touch the working tree"
[[ -z "$(git -C "$gitwork" tag -l v0.2.0)" ]] || fail "--dry-run must not create a tag"

# The real cut: happy path end to end against the throwaway origin.
run --version 0.2.0 >/dev/null

grep -q '^## 0.2.0 — ' "$gitwork/CHANGELOG.md" || fail "real cut did not write the dated section"
grep -Fxq '## Unreleased' "$gitwork/CHANGELOG.md" || fail "real cut must leave a fresh '## Unreleased' heading"
[[ "$(git -C "$gitwork" log -1 --format=%s main)" == "chore(release): 0.2.0" ]] || fail "commit message wrong"
[[ "$(git -C "$gitwork" cat-file -t v0.2.0)" == "tag" ]] || fail "v0.2.0 must be an annotated tag object, not lightweight"
[[ "$(git -C "$gitremote" rev-parse main)" == "$(git -C "$gitwork" rev-parse main)" ]] || fail "commit was not pushed to origin"
[[ "$(git -C "$gitremote" tag -l v0.2.0)" == "v0.2.0" ]] || fail "tag was not pushed to origin"

# The Unreleased section the real cut just emptied must now refuse a second cut.
reject_cut "is empty" --version 0.3.0 --dry-run

echo "release-cut contract: PASS"
