#!/usr/bin/env bash
# Validate a platform release manifest against the schema documented in
# scripts/release/platform-release-manifest.md — every field, every grammar, and
# NO key the schema does not name, so the format cannot drift silently into
# something the control plane's release detection cannot read.
#
# Run twice on every release: once by the generator on its own output, once by
# the `release` job in .github/workflows/images.yml (with the --expect-* flags,
# which tie the manifest to the digests the build jobs actually produced) before
# the asset is uploaded to the GitHub Release.
#
# Reports EVERY error, not just the first — fixing a manifest one CI run per
# field is the failure mode this avoids.
#
# Exit codes: 0 valid (prints PASS) · 1 invalid or unreadable · 2 usage error.
set -euo pipefail

path=""
expect_version=""
expect_control=""
expect_agent=""

usage() {
  cat <<'EOF'
usage: scripts/release/validate-platform-release-manifest.sh PATH \
         [--expect-version V] [--expect-control-digest D] [--expect-agent-digest D]

Validates a platform-release-manifest.json. The optional --expect-* flags assert
equality against values the caller already knows (the workflow passes the tag's
version and the two build-job digests), which is what proves the manifest
describes THIS run's artifacts and not a stale file.

Prints every error on stderr and exits 1; prints PASS and exits 0 when valid.
Schema: scripts/release/platform-release-manifest.md
EOF
}

while (($#)); do
  case "$1" in
    --expect-version) expect_version=${2:?--expect-version needs a value}; shift 2 ;;
    --expect-control-digest) expect_control=${2:?--expect-control-digest needs a value}; shift 2 ;;
    --expect-agent-digest) expect_agent=${2:?--expect-agent-digest needs a value}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    -*) usage >&2; exit 2 ;;
    *)
      [[ -z "$path" ]] || { usage >&2; exit 2; }
      path=$1; shift ;;
  esac
done

[[ -n "$path" ]] || { usage >&2; exit 2; }

python3 - "$path" "$expect_version" "$expect_control" "$expect_agent" <<'PY'
import datetime as dt
import json
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
expect_version, expect_control, expect_agent = sys.argv[2:5]
errors = []

# The two components, in this order, with the image name each one must carry.
# Exactly these: a manifest naming a third artifact is a format change and needs
# a format_version bump, not a lenient validator.
COMPONENTS = (
    ("control-plane", "quasar-control-plane"),
    ("node-agent", "quasar-node-agent"),
)
TOP_KEYS = ["format_version", "version", "prerelease", "source_commit",
            "built_at", "schema_version", "components"]
COMPONENT_KEYS = ["name", "image", "digest"]

# semver.org 2.0.0, prerelease part optional, build metadata deliberately NOT
# accepted: `+` is not a legal character in a Docker tag, so a version carrying
# it could never appear as the `:X.Y.Z` tag the release promotes.
SEMVER = re.compile(
    r"(?P<core>(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))"
    r"(?:-(?P<pre>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)"
    r"(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?"
)
# A registry reference with NO tag and NO digest: consumers compose `image@digest`
# themselves (ADR 0001 — a tag is never an identity in the release path). The
# optional leading component is a registry host, whose colon is a port.
IMAGE = re.compile(
    r"(?:[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/)?"
    r"(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*"
    r"[a-z0-9]+(?:[._-][a-z0-9]+)*"
)
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
COMMIT = re.compile(r"[0-9a-f]{40}")
BUILT_AT = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z")


def is_int(value):
    # bool is an int subclass in Python and `true` is not a schema_version.
    return isinstance(value, int) and not isinstance(value, bool)


def report():
    if errors:
        for error in errors:
            print(f"platform-release-manifest: {error}", file=sys.stderr)
        print(f"platform-release-manifest: {path} is INVALID ({len(errors)} error(s))",
              file=sys.stderr)
        raise SystemExit(1)
    print("PASS")
    raise SystemExit(0)


if not path.is_file():
    errors.append(f"no such manifest: {path}")
    report()

try:
    doc = json.loads(path.read_text())
except (ValueError, UnicodeDecodeError) as exc:
    errors.append(f"{path} is not valid JSON: {exc}")
    report()

if not isinstance(doc, dict):
    errors.append(f"{path} is not a JSON object")
    report()

for key in TOP_KEYS:
    if key not in doc:
        errors.append(f"missing top-level key: {key}")
for key in doc:
    if key not in TOP_KEYS:
        errors.append(f"unknown top-level key: {key} "
                      "(adding a key is a format_version bump, not a new key)")

if "format_version" in doc and doc["format_version"] != 1:
    errors.append(f"format_version must be the integer 1, got {doc['format_version']!r}")

version = doc.get("version")
parsed = None
if not isinstance(version, str):
    errors.append(f"version must be a string, got {version!r}")
elif "+" in version:
    errors.append(f"version must not carry semver build metadata: {version!r}")
elif not (parsed := SEMVER.fullmatch(version)):
    errors.append(f"version must be strict semver X.Y.Z[-prerelease], got {version!r}")

prerelease = doc.get("prerelease")
if not isinstance(prerelease, bool):
    errors.append(f"prerelease must be a boolean, got {prerelease!r}")
elif parsed is not None and prerelease != bool(parsed.group("pre")):
    errors.append(
        f"prerelease is {prerelease} but version {version!r} "
        f"{'has' if parsed.group('pre') else 'has no'} a prerelease part"
    )

commit = doc.get("source_commit")
if not isinstance(commit, str) or not COMMIT.fullmatch(commit):
    errors.append(f"source_commit must be 40 lowercase hex characters, got {commit!r}")

built_at = doc.get("built_at")
if not isinstance(built_at, str) or not BUILT_AT.fullmatch(built_at):
    errors.append(f"built_at must be RFC3339 UTC as YYYY-MM-DDTHH:MM:SSZ, got {built_at!r}")
else:
    try:
        dt.datetime.strptime(built_at, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        errors.append(f"built_at is not a real instant: {built_at!r} ({exc})")

schema_version = doc.get("schema_version")
if not is_int(schema_version) or schema_version < 1:
    errors.append(
        f"schema_version must be a positive integer (the highest control-plane "
        f"migration embedded at this commit), got {schema_version!r}"
    )

components = doc.get("components")
if not isinstance(components, list):
    errors.append(f"components must be an array, got {components!r}")
elif len(components) != len(COMPONENTS):
    errors.append(
        f"components must hold exactly {len(COMPONENTS)} components "
        f"({', '.join(name for name, _ in COMPONENTS)}), got {len(components)}"
    )
else:
    for index, (component, (want_name, want_image)) in enumerate(zip(components, COMPONENTS)):
        where = f"components[{index}]"
        if not isinstance(component, dict):
            errors.append(f"{where} must be an object, got {component!r}")
            continue
        for key in COMPONENT_KEYS:
            if key not in component:
                errors.append(f"{where} missing key: {key}")
        for key in component:
            if key not in COMPONENT_KEYS:
                errors.append(f"{where} unknown key: {key}")

        name = component.get("name")
        if name != want_name:
            errors.append(f"{where}.name must be {want_name!r}, got {name!r} "
                          "(the two components are fixed, in this order)")

        image = component.get("image")
        if not isinstance(image, str) or not IMAGE.fullmatch(image):
            errors.append(
                f"{where}.image must be a registry reference and must carry no tag "
                f"and no digest, got {image!r}"
            )
        elif image.rsplit("/", 1)[-1] != want_image:
            errors.append(f"{where}.image must name {want_image!r}, got {image!r}")

        digest = component.get("digest")
        if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
            errors.append(
                f"{where}.digest must be sha256: plus 64 lowercase hex characters, "
                f"got {digest!r}"
            )

# The --expect-* assertions: the caller already knows these, so a mismatch means
# the manifest describes something other than what this run built.
if expect_version and version != expect_version:
    errors.append(f"expected version {expect_version!r}, manifest has {version!r}")

if isinstance(components, list):
    by_name = {c.get("name"): c for c in components if isinstance(c, dict)}
    for flag, name in ((expect_control, "control-plane"), (expect_agent, "node-agent")):
        if not flag:
            continue
        got = (by_name.get(name) or {}).get("digest")
        if got != flag:
            errors.append(f"expected {name} digest {flag!r}, manifest has {got!r}")

report()
PY
