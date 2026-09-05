#!/usr/bin/env bash
# Print one version's section body from CHANGELOG.md — the release notes for a
# `vX.Y.Z` tag, taken from the tree the tag points at so the notes and the images
# cannot come from different commits.
#
# Used by .github/workflows/images.yml twice: the `release-gate` job runs it
# BEFORE any build to refuse a tag whose section is missing or empty (a ~85 min
# node-agent build must not run for a release that cannot be published), and the
# `release` job runs it again to write the GitHub Release body.
#
# Exit codes: 0 printed a non-empty body · 1 the changelog is wrong (no such
# section, or the section is empty) · 2 usage error.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
file="$root/CHANGELOG.md"
version=""

usage() {
  cat <<'EOF'
usage: scripts/release/changelog-section.sh <version> [--file PATH]

Prints the body of the `## <version>` section of CHANGELOG.md — everything after
the heading line, up to the next `## ` heading or end of file — with surrounding
blank lines trimmed.

<version> is the tag without its leading `v` (0.2.0, 0.2.0-rc.1) and is matched
EXACTLY: a `## 0.2.0` heading does not satisfy 0.2.0-rc.1, so a prerelease tag
needs its own section. Accepted heading forms:

  ## 0.2.0
  ## 0.2.0 — 2026-08-01     (em dash, what 0.1.0 uses)
  ## 0.2.0 - 2026-08-01
  ## 0.2.0 (2026-08-01)

Exits 1 with a message on stderr when the section is missing or its body is
empty; exits 2 on a usage error.
EOF
}

while (($#)); do
  case "$1" in
    --file) file=${2:?--file needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    -*) usage >&2; exit 2 ;;
    *)
      [[ -z "$version" ]] || { usage >&2; exit 2; }
      version=$1; shift ;;
  esac
done

[[ -n "$version" ]] || { usage >&2; exit 2; }

python3 - "$file" "$version" <<'PY'
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
version = sys.argv[2]

if not path.is_file():
    print(f"changelog-section: no such changelog: {path}", file=sys.stderr)
    raise SystemExit(1)

# A version, not a label. `## Unreleased` tracks develop and is never a release
# body; refusing it here means a mistyped tag cannot publish work-in-progress
# notes. The full semver grammar is the validator's job (this only has to reject
# things that are obviously not a version), so the shape check stays loose.
if not re.fullmatch(r"[0-9][0-9A-Za-z.+-]*", version):
    print(f"changelog-section: {version!r} is not a version", file=sys.stderr)
    raise SystemExit(1)

# Heading grammar: `## <version>` then end of line, ` — <date>`, ` - <date>` or
# ` (<date>)`. Every date form requires WHITESPACE between the version and the
# separator, which is what stops `0.4.0` from matching a `## 0.4.0-rc.1` heading
# through the hyphen alternative: a prerelease is a different version and needs
# its own section. The bare form requires end of line for the same reason.
v = re.escape(version)
heading = re.compile(
    rf"^##[ \t]+{v}(?:[ \t]*$|[ \t]+[—\-][ \t]*\S|[ \t]+\(.*\)[ \t]*$)"
)
lines = path.read_text().splitlines()

start = None
for i, line in enumerate(lines):
    if heading.match(line):
        start = i + 1
        break

if start is None:
    print(
        f"changelog-section: no section for version {version} in {path}. "
        f"Add a `## {version} — YYYY-MM-DD` section with a non-empty body; the "
        "release workflow publishes it as the release notes.",
        file=sys.stderr,
    )
    raise SystemExit(1)

end = len(lines)
for i in range(start, len(lines)):
    if lines[i].startswith("## "):
        end = i
        break

body = "\n".join(lines[start:end]).strip("\n")
if not body.strip():
    print(
        f"changelog-section: section for version {version} in {path} is empty. "
        "A release cannot be published with no notes.",
        file=sys.stderr,
    )
    raise SystemExit(1)

print(body)
PY
