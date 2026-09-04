#!/usr/bin/env bash
# scripts/release/release-cut.sh — cut a release in one command (#109).
#
# `make release VERSION=x.y.z` (or a direct `--version` invocation) moves the
# CHANGELOG.md `## Unreleased` section into a dated `## X.Y.Z — YYYY-MM-DD`
# section directly below a fresh empty `## Unreleased`, commits that on `main`,
# tags the commit `vX.Y.Z` (annotated), and pushes the commit and the tag —
# which is the one automatic trigger for the tag-push release lane
# (.github/workflows/images.yml, `release-gate` job, #108). This script never
# merges `develop` and never touches it.
#
# The changelog rewrite is a PURE function of the file, exposed as its own
# mode so it is fixture-testable with no git at all:
#
#   scripts/release/release-cut.sh --transform --version X.Y.Z --date YYYY-MM-DD \
#     < CHANGELOG.md > new-CHANGELOG.md
#
# That mode reads the changelog on stdin and writes the rewritten changelog on
# stdout; it refuses (exit 1, one-line reason on stderr) if there is no
# `## Unreleased` heading or its body is empty. The default (cut) mode runs
# every refusal check below, then calls the same transform to produce the
# content it writes.
#
# Refusals (each exits non-zero with a one-line reason on stderr), in cheapest
# -first order:
#   - not on `main`
#   - working tree not clean
#   - VERSION is not strict semver X.Y.Z[-prerelease] (semver.org core; no `v`
#     prefix, no build metadata — the same grammar the release-gate job and
#     platform-release-manifest validator use)
#   - HEAD is not exactly `origin/main` (fetched fresh — refuses both "behind"
#     and "ahead of unpushed" mismatches)
#   - VERSION is not strictly newer than the newest existing `v*` tag (semver
#     precedence, prereleases sort below their release)
#   - the `## Unreleased` section is missing or empty
#
# `--dry-run` runs every refusal check (including the fetch — the diff it
# prints must be trustworthy) and then prints the changelog diff and the git
# commands it would run, without writing, committing, tagging or pushing
# anything.
#
# Exit codes: 0 success (or a clean --dry-run) · 1 a refusal, reason on stderr
# · 2 usage error.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
changelog="$root/CHANGELOG.md"

# semver.org 2.0.0 core, optional prerelease part, no build metadata — the same
# grammar as the release-gate job in .github/workflows/images.yml and
# scripts/release/validate-platform-release-manifest.sh. Kept in sync by hand;
# the fixture test compares it against the workflow's copy so drift is caught.
semver_re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?$'

usage() {
  cat <<'EOF'
usage:
  scripts/release/release-cut.sh --version X.Y.Z [--dry-run]
  scripts/release/release-cut.sh --transform --version X.Y.Z --date YYYY-MM-DD

VERSION and DRY_RUN may also come from the environment (make release
VERSION=x.y.z [DRY_RUN=1]) — the Makefile passes no caller-settable text into
recipe lines, per #550.

Cut mode (default): refuses unless the repo is on `main` with a clean tree that
matches origin/main, VERSION is strict semver and strictly newer than the
newest existing `v*` tag, and CHANGELOG.md's `## Unreleased` section is
non-empty. On success it moves that section into a dated `## X.Y.Z —
YYYY-MM-DD` section under a fresh empty `## Unreleased`, commits
"chore(release): X.Y.Z", tags `vX.Y.Z` (annotated) and pushes both. Never
merges or touches `develop`.

--dry-run runs every check, then prints the changelog diff and the git
commands that would run — nothing is written, committed, tagged or pushed.

--transform is the pure changelog rewrite with no git involved: it reads a
changelog on stdin and writes the rewritten changelog on stdout, so it is
fixture-testable. --date defaults to today (UTC) if omitted.
EOF
}

mode="cut"
version="${VERSION:-}"
dry_run=false
case "${DRY_RUN:-0}" in
  1 | true | yes) dry_run=true ;;
esac
transform_date=""

while (($#)); do
  case "$1" in
    --transform) mode=transform; shift ;;
    --version)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      version=$2
      shift 2
      ;;
    --date)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      transform_date=$2
      shift 2
      ;;
    --dry-run) dry_run=true; shift ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

[[ -n "$version" ]] || {
  usage >&2
  exit 2
}

if ! [[ "$version" =~ $semver_re ]]; then
  echo "release-cut: '$version' is not strict semver X.Y.Z[-prerelease] (no leading v, no build metadata)" >&2
  exit 1
fi

# ── The pure transform ───────────────────────────────────────────────────────
#
# Reads a changelog on stdin, writes the rewritten changelog on stdout. Refuses
# (exit 1, reason on stderr) if there is no `## Unreleased` heading or its body
# is empty — this is the ONE place either check happens; cut mode below calls
# it rather than re-implementing the check.
transform() { # transform <version> <date>
  local version="$1" date_arg="$2" code
  # The transform reads the changelog on the CALLER's stdin — `python3 -`
  # would consume that same stdin to receive the script itself via this
  # heredoc, so the script is built as a string with `cat` (whose heredoc is
  # its own, separate stdin) and run with `python3 -c`, leaving the function's
  # stdin untouched for the actual changelog.
  code="$(cat <<'PY'
import re
import sys

version, date = sys.argv[1], sys.argv[2]
text = sys.stdin.read()
lines = text.split("\n")

# Exact heading only: `## Unreleased` never carries a date.
heading_re = re.compile(r"^##[ \t]+Unreleased[ \t]*$")
start = None
for i, line in enumerate(lines):
    if heading_re.match(line):
        start = i
        break
if start is None:
    print("release-cut: CHANGELOG.md has no '## Unreleased' heading", file=sys.stderr)
    raise SystemExit(1)

# The section runs to the next top-level `## ` heading (not `### `, whose third
# character is `#` rather than a space) or end of file.
end = len(lines)
for j in range(start + 1, len(lines)):
    if re.match(r"^## ", lines[j]):
        end = j
        break

body = lines[start + 1 : end]
b0, b1 = 0, len(body)
while b0 < b1 and body[b0].strip() == "":
    b0 += 1
while b1 > b0 and body[b1 - 1].strip() == "":
    b1 -= 1
body = body[b0:b1]

if not body:
    print("release-cut: the '## Unreleased' section is empty", file=sys.stderr)
    raise SystemExit(1)

new_heading = f"## {version} — {date}"
out = lines[: start + 1] + [""] + [new_heading] + [""] + body + [""] + lines[end:]
sys.stdout.write("\n".join(out))
PY
)"
  python3 -c "$code" "$version" "$date_arg"
}

if [[ "$mode" == transform ]]; then
  [[ -n "$transform_date" ]] || transform_date="$(date -u +%F)"
  transform "$version" "$transform_date"
  exit 0
fi

# ── Cut mode: the refusal checks, cheapest first ────────────────────────────

fail() {
  echo "release-cut: $*" >&2
  exit 1
}

branch="$(git -C "$root" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
[[ "$branch" == "main" ]] || fail "must be run on main (currently on '${branch:-detached HEAD}')"

if [[ -n "$(git -C "$root" status --porcelain)" ]]; then
  fail "working tree is not clean; commit or stash changes first"
fi

# Explicit refspec, not a bare `git fetch origin`: a tag checkout or a worktree
# is not guaranteed to already carry an origin/main remote-tracking ref, and
# comparing against one that does not exist would fail for the wrong reason —
# same discipline as the release-gate job in .github/workflows/images.yml.
git -C "$root" fetch --quiet --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
local_head="$(git -C "$root" rev-parse HEAD)"
origin_head="$(git -C "$root" rev-parse refs/remotes/origin/main)"
[[ "$local_head" == "$origin_head" ]] || fail "HEAD ($local_head) does not match origin/main ($origin_head); pull or push first"

# Strictly newer than the newest existing v* tag, by semver precedence (a
# prerelease sorts below its own release, per semver.org). No existing tag at
# all trivially passes.
mapfile -t existing_tags < <(git -C "$root" tag -l 'v*' | sed 's/^v//')
newest_offender=""
if ! newest_offender="$(python3 - "$version" "${existing_tags[@]+"${existing_tags[@]}"}" <<'PY'
import re
import sys

SEMVER = re.compile(
    r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-([0-9A-Za-z.-]+))?$"
)


def key(v):
    m = SEMVER.match(v)
    core = tuple(int(x) for x in m.group(1, 2, 3))
    pre = m.group(4)
    if pre is None:
        # No prerelease outranks every prerelease of the same core.
        return (core, (1,))
    parts = []
    for ident in pre.split("."):
        if ident.isdigit():
            parts.append((0, int(ident)))
        else:
            parts.append((1, ident))
    return (core, (0, tuple(parts)))


version = sys.argv[1]
tags = [t for t in sys.argv[2:] if SEMVER.match(t)]
if not tags:
    raise SystemExit(0)
newest = max(tags, key=key)
if key(version) > key(newest):
    raise SystemExit(0)
print(newest)
raise SystemExit(1)
PY
)"; then
  fail "$version is not strictly newer than the newest existing tag v$newest_offender"
fi

today="$(date -u +%F)"
if ! new_content="$(transform "$version" "$today" < "$changelog")"; then
  exit 1
fi

commit_msg="chore(release): $version"
tag="v$version"

if "$dry_run"; then
  echo "release-cut: --dry-run — nothing below is executed"
  echo
  echo "── changelog diff ──"
  diff -u "$changelog" <(printf '%s\n' "$new_content") || true
  echo
  echo "── commands ──"
  echo "git -C '$root' add CHANGELOG.md"
  echo "git -C '$root' commit -m '$commit_msg'"
  echo "git -C '$root' tag -a '$tag' -m '$commit_msg'"
  echo "git -C '$root' push origin HEAD:refs/heads/main '$tag'"
  exit 0
fi

printf '%s\n' "$new_content" > "$changelog"
git -C "$root" add CHANGELOG.md
git -C "$root" commit --quiet -m "$commit_msg"
git -C "$root" tag -a "$tag" -m "$commit_msg"
git -C "$root" push origin HEAD:refs/heads/main "$tag"

echo "release-cut: cut and pushed $tag"
