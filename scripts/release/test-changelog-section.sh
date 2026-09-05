#!/usr/bin/env bash
# Offline contract test for scripts/release/changelog-section.sh. The tag-push
# release lane (.github/workflows/images.yml, release-gate) refuses a tag whose
# changelog section is missing or empty BEFORE any build starts, so the two
# refusals below are load-bearing, not cosmetic: they are the whole gate.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo/scripts/release/changelog-section.sh"
fixtures="$repo/scripts/release/fixtures/changelogs"

fail() { echo "changelog-section test: $*" >&2; exit 1; }

extract() { # extract <version> [file]
  "$script" "$1" --file "${2:-$fixtures/sections.md}"
}

reject() { # reject <version> <file> <expected stderr fragment>
  local output
  if output=$("$script" "$1" --file "$2" 2>&1); then
    fail "unexpected success for $1 in $2: $output"
  fi
  [[ "$output" == *"$3"* ]] || fail "missing rejection '$3' for $1: $output"
}

test -x "$script" || fail "changelog-section.sh not executable"

# --help is a contract too: the workflow's failure message points a maintainer at it.
"$script" --help >/dev/null || fail "--help must exit 0"

# Every accepted heading form yields its own body, and only its own body.
[[ "$(extract 0.3.0)" == "### Added

- Em-dash date form, the shape 0.1.0 uses.

### Fixed

- Blank lines around this section must be trimmed off the extracted body." ]] \
  || fail "em-dash section body wrong: $(extract 0.3.0)"

[[ "$(extract 0.2.0)" == "Hyphen date form." ]] || fail "hyphen date form not matched"
[[ "$(extract 0.2.0-rc.1)" == "Parenthesised date form, and a prerelease carrying its own section." ]] \
  || fail "parenthesised date form not matched"
[[ "$(extract 0.1.5)" == "Bare heading, no date at all." ]] || fail "bare heading not matched"

# The last section ends at end of file, not at a heading that is not there.
[[ "$(extract 0.1.3)" == "Last section in the file; the extractor stops at end of file, not at a heading
that is not there." ]] || fail "final section body wrong: $(extract 0.1.3)"

# The body is trimmed: no leading or trailing blank line survives.
body="$(extract 0.3.0)"
[[ "$body" != $'\n'* && "$body" != *$'\n' ]] || fail "body not trimmed"

# A version is matched EXACTLY. `## 0.2.0` must not satisfy tag 0.2.0-rc.1 (a
# prerelease needs its own section) and `## 0.2.0-rc.1` must not satisfy 0.2.0.
[[ "$(extract 0.2.0)" != *"prerelease"* ]] || fail "0.2.0 matched the prerelease section"
[[ "$(extract 0.2.0-rc.1)" != *"Hyphen"* ]] || fail "0.2.0-rc.1 matched the release section"
reject 0.3 "$fixtures/sections.md" "no section for version 0.3"
# The prefix case with nothing else to match: `## 0.4.0-rc.1` is not a 0.4.0
# section, even when it is the only heading whose text starts with 0.4.0.
reject 0.4.0 "$fixtures/prerelease-only.md" "no section for version 0.4.0"
[[ "$(extract 0.4.0-rc.1 "$fixtures/prerelease-only.md")" == "The candidate's own notes." ]] \
  || fail "prerelease-only section body wrong"

# A missing section fails, and says which file and version it looked for.
reject 9.9.9 "$fixtures/sections.md" "no section for version 9.9.9"
reject 9.9.9 "$fixtures/sections.md" "sections.md"
reject 0.3.0 "$fixtures/unreleased-only.md" "no section for version 0.3.0"

# `## Unreleased` is not a version and must never be extractable as one: the
# release notes for vX.Y.Z can only ever be that version's own section.
reject Unreleased "$fixtures/sections.md" "not a version"

# An empty section is refused separately from a missing one: a tag whose notes
# are a bare heading would publish a release with no body.
reject 0.1.4 "$fixtures/sections.md" "section for version 0.1.4"
reject 0.1.4 "$fixtures/sections.md" "is empty"

# The repo's own CHANGELOG is the real input. 0.1.0 is released and dated, so it
# must extract non-empty — this is what catches a heading-format drift here.
[[ -n "$("$script" 0.1.0)" ]] || fail "repo CHANGELOG 0.1.0 section empty"
[[ -n "$("$script" 0.1.0 --file "$repo/CHANGELOG.md")" ]] || fail "--file on repo CHANGELOG failed"

# Argument handling: unknown flag and missing version are usage errors (exit 2),
# distinct from the exit-1 "the changelog is wrong" failures above.
if "$script" 0.1.0 --nonsense >/dev/null 2>&1; then fail "unknown flag accepted"; fi
"$script" 0.1.0 --nonsense >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "unknown flag must exit 2"
"$script" >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "missing version must exit 2"
if "$script" 0.1.0 --file "$fixtures/does-not-exist.md" >/dev/null 2>&1; then
  fail "missing changelog file accepted"
fi

echo "Changelog section extraction contract: PASS"
