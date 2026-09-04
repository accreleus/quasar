#!/usr/bin/env bash
# Write the platform release manifest — the machine-readable half of a stable
# platform release, published beside the human-readable notes so the two cannot
# disagree (CONTEXT.md, "Release manifest").
#
# It INVENTS nothing: every value is either passed in by the release job (the
# version from the tag, the digests from the build jobs, the commit and the
# shared built_at from the run) or read from the tagged tree (schema_version,
# the highest embedded control-plane migration). The one derived field is
# `prerelease`, which follows from the version by semver's own rule.
#
# The output is validated by validate-platform-release-manifest.sh before this
# script exits 0, so a bad input never leaves a published-looking file behind.
#
# Schema: scripts/release/platform-release-manifest.md
# Exit codes: 0 wrote a valid manifest · 1 bad input or invalid output · 2 usage.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
validator="$root/scripts/release/validate-platform-release-manifest.sh"

version=""
source_commit=""
built_at=""
control_digest=""
agent_digest=""
registry_ns=""
migrations_dir="$root/control-plane/migrations"
output=""

usage() {
  cat <<'EOF'
usage: scripts/release/generate-platform-release-manifest.sh \
         --version X.Y.Z[-prerelease] \
         --source-commit <40 hex> \
         --built-at YYYY-MM-DDTHH:MM:SSZ \
         --control-digest sha256:<64 hex> \
         --agent-digest sha256:<64 hex> \
         --registry-ns ghcr.io/accreleus/quasar \
         [--migrations-dir control-plane/migrations] \
         --output PATH

Writes platform-release-manifest.json (format_version 1) and validates it before
exiting 0. `schema_version` is computed as the highest NNNN over the migrations
directory's NNNN_*.up.sql files; `prerelease` is true iff --version carries a
prerelease part. The component images are <registry-ns>/quasar-control-plane and
<registry-ns>/quasar-node-agent, tag-free — consumers compose image@digest.

Schema and field grammar: scripts/release/platform-release-manifest.md
EOF
}

while (($#)); do
  case "$1" in
    --version) version=${2:?--version needs a value}; shift 2 ;;
    --source-commit) source_commit=${2:?--source-commit needs a value}; shift 2 ;;
    --built-at) built_at=${2:?--built-at needs a value}; shift 2 ;;
    --control-digest) control_digest=${2:?--control-digest needs a value}; shift 2 ;;
    --agent-digest) agent_digest=${2:?--agent-digest needs a value}; shift 2 ;;
    --registry-ns) registry_ns=${2:?--registry-ns needs a value}; shift 2 ;;
    --migrations-dir) migrations_dir=${2:?--migrations-dir needs a path}; shift 2 ;;
    --output) output=${2:?--output needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

for required in version source_commit built_at control_digest agent_digest registry_ns output; do
  if [[ -z "${!required}" ]]; then
    echo "generate-platform-release-manifest: missing --${required//_/-}" >&2
    usage >&2
    exit 2
  fi
done

python3 - "$version" "$source_commit" "$built_at" "$control_digest" "$agent_digest" \
  "$registry_ns" "$migrations_dir" "$output" <<'PY'
import json
import re
import sys
from pathlib import Path

(version, source_commit, built_at, control_digest, agent_digest,
 registry_ns, migrations_dir, output) = sys.argv[1:9]
errors = []

SEMVER = re.compile(
    r"(?P<core>(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))"
    r"(?:-(?P<pre>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)"
    r"(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?"
)
# The namespace is the image reference minus its last path element, so it obeys
# the same grammar and must carry no tag: appending `/quasar-control-plane` to a
# tagged namespace would produce something that is not a reference at all.
NAMESPACE = re.compile(
    r"(?:[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/)?"
    r"(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*"
    r"[a-z0-9]+(?:[._-][a-z0-9]+)*"
)

parsed = SEMVER.fullmatch(version)
if "+" in version:
    errors.append(f"--version must not carry semver build metadata: {version!r}")
elif not parsed:
    errors.append(
        f"--version must be strict semver X.Y.Z[-prerelease] with no leading 'v', "
        f"got {version!r}"
    )
if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
    errors.append(f"--source-commit must be 40 lowercase hex characters, got {source_commit!r}")
if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", built_at):
    errors.append(f"--built-at must be RFC3339 UTC as YYYY-MM-DDTHH:MM:SSZ, got {built_at!r}")
for flag, value in (("--control-digest", control_digest), ("--agent-digest", agent_digest)):
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", value):
        errors.append(f"{flag} must be sha256: plus 64 lowercase hex characters, got {value!r}")
if not NAMESPACE.fullmatch(registry_ns):
    errors.append(
        f"--registry-ns must be a tag-free registry namespace "
        f"(e.g. ghcr.io/accreleus/quasar), got {registry_ns!r}"
    )

# schema_version is ADR 0002's vocabulary: the DB migration version the control
# plane in this release will run forward to on boot, and therefore the number a
# host must not roll BELOW. It is not the manifest's own format_version.
schema_version = None
path = Path(migrations_dir)
if not path.is_dir():
    errors.append(f"--migrations-dir is not a directory: {migrations_dir}")
else:
    versions = []
    for entry in path.glob("*.up.sql"):
        match = re.match(r"(\d+)_", entry.name)
        if match:
            versions.append(int(match.group(1)))
    if not versions:
        errors.append(f"no migrations found in {migrations_dir} (expected NNNN_*.up.sql)")
    else:
        schema_version = max(versions)

if errors:
    for error in errors:
        print(f"generate-platform-release-manifest: {error}", file=sys.stderr)
    raise SystemExit(1)

manifest = {
    "format_version": 1,
    "version": version,
    "prerelease": bool(parsed.group("pre")),
    "source_commit": source_commit,
    "built_at": built_at,
    "schema_version": schema_version,
    "components": [
        {"name": "control-plane",
         "image": f"{registry_ns}/quasar-control-plane",
         "digest": control_digest},
        {"name": "node-agent",
         "image": f"{registry_ns}/quasar-node-agent",
         "digest": agent_digest},
    ],
}

out = Path(output)
out.parent.mkdir(parents=True, exist_ok=True)
# Insertion order, not sorted: the field order is part of the documented shape,
# and 2-space indent keeps the asset readable in the GitHub Release UI.
out.write_text(json.dumps(manifest, indent=2) + "\n")
print(f"wrote {out}")
PY

# Never publish an unvalidated manifest, and never leave one behind: the same
# validator the workflow runs decides whether this file may exist at all.
if ! "$validator" "$output" \
    --expect-version "$version" \
    --expect-control-digest "$control_digest" \
    --expect-agent-digest "$agent_digest" >/dev/null; then
  echo "generate-platform-release-manifest: generated manifest failed validation; removing $output" >&2
  rm -f "$output"
  exit 1
fi
