#!/usr/bin/env bash
# Validate a platform release manifest against the schema documented in
# scripts/release/platform-release-manifest.md — every field, every grammar, and
# NO key the schema does not name, so the format cannot drift silently into
# something the control plane's release detection cannot read.
#
# Dispatches on `format_version`: 1 (platform-release-manifest.json, two
# components; already-published releases carry it) or 2
# (platform-release-manifest.v2.json, three components and a floor;
# protocol/control-api.md "Release manifest format 2"). With --preview it validates
# the preview manifest instead (format 2 with `version` null, `prerelease` false and
# an optional `commits`; control-api.md "Platform-release preview channel"); without
# it a null version and a `commits` key stay invalid, as on every release asset.
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
expect_format=""
expect_version=""
expect_control=""
expect_agent=""
expect_recovery=""
preview=0

usage() {
  cat <<'EOF'
usage: scripts/release/validate-platform-release-manifest.sh PATH \
         [--expect-format N] [--expect-version V] \
         [--expect-control-digest D] [--expect-agent-digest D] \
         [--expect-recovery-digest D] [--preview]

Validates a platform release manifest, format 1 (platform-release-manifest.json)
or format 2 (platform-release-manifest.v2.json), by its format_version. The
optional --expect-* flags assert equality against values the caller already
knows (the workflow passes the format, the tag's version and the three
build-job digests), which is what proves the manifest describes THIS run's
artifacts and not a stale file. --expect-recovery-digest on a format-1 manifest
is an error: format 1 has no recovery-actor component.

--preview validates the preview manifest (the document at the `preview` tag of
quasar-release-manifest): format 2, `version` null, `prerelease` false, and an
optional `commits` list. It cannot be combined with --expect-version or
--expect-format 1. Without --preview a null version or a `commits` key is invalid.

Prints every error on stderr and exits 1; prints PASS and exits 0 when valid.
Schema: scripts/release/platform-release-manifest.md
EOF
}

while (($#)); do
  case "$1" in
    --expect-format) expect_format=${2:?--expect-format needs a value}; shift 2 ;;
    --expect-version) expect_version=${2:?--expect-version needs a value}; shift 2 ;;
    --expect-control-digest) expect_control=${2:?--expect-control-digest needs a value}; shift 2 ;;
    --expect-agent-digest) expect_agent=${2:?--expect-agent-digest needs a value}; shift 2 ;;
    --expect-recovery-digest) expect_recovery=${2:?--expect-recovery-digest needs a value}; shift 2 ;;
    --preview) preview=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage >&2; exit 2 ;;
    *)
      [[ -z "$path" ]] || { usage >&2; exit 2; }
      path=$1; shift ;;
  esac
done

[[ -n "$path" ]] || { usage >&2; exit 2; }
case "$expect_format" in
  ''|1|2) ;;
  *) echo "validate-platform-release-manifest: --expect-format must be 1 or 2, got '$expect_format'" >&2
     exit 2 ;;
esac
if ((preview)) && [[ -n "$expect_version" || "$expect_format" == 1 ]]; then
  echo "validate-platform-release-manifest: --preview has no version and is format 2" >&2
  exit 2
fi

python3 - "$path" "$expect_format" "$expect_version" "$expect_control" "$expect_agent" \
  "$expect_recovery" "$preview" <<'PY'
import datetime as dt
import json
import re
import sys
import unicodedata
from pathlib import Path

path = Path(sys.argv[1])
(expect_format, expect_version, expect_control, expect_agent,
 expect_recovery) = sys.argv[2:7]
preview = sys.argv[7] == "1"
errors = []

# Per format, in order: component name and the last path element of its image.
# Another artifact is a format_version bump, not a lenient validator.
COMPONENTS = {
    1: (
        ("control-plane", "quasar-control-plane"),
        ("node-agent", "quasar-node-agent"),
    ),
    2: (
        ("control-plane", "quasar-control-plane"),
        ("node-agent", "quasar-node-agent"),
        ("recovery-actor", "quasar-recovery"),
    ),
}
TOP_KEYS = {
    1: ["format_version", "version", "prerelease", "source_commit",
        "built_at", "schema_version", "components"],
    2: ["format_version", "version", "prerelease", "source_commit",
        "built_at", "schema_version", "components", "floor"],
}
COMPONENT_KEYS = ["name", "image", "digest"]
# Format 2's floor entries, in order.
FLOOR = ("node-agent", "recovery-actor")
FLOOR_KEYS = ["name", "version"]
# The preview manifest's optional commit list (amendment 48).
COMMIT_KEYS = ["commit", "subject"]
MAX_COMMITS = 500
MAX_SUBJECT = 200
# What a one-line subject may not hold, as the control plane's parser refuses it:
# control characters (C0, DEL, C1), U+2028/U+2029 and the Bidi_Control set.
BIDI = {0x061C, 0x200E, 0x200F, *range(0x202A, 0x202F), *range(0x2066, 0x206A)}


def unsafe_subject_char(c):
    return unicodedata.category(c) in ("Cc", "Zl", "Zp") or ord(c) in BIDI

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


def semver_key(match):
    """Sort key with SemVer 2.0.0 precedence (semver.org section 11)."""
    core = tuple(int(part) for part in match.group("core").split("."))
    pre = match.group("pre")
    if pre is None:
        return core, (1,)
    ids = tuple((0, int(i), "") if i.isdigit() else (1, 0, i) for i in pre.split("."))
    return core, (0, ids)


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

# An unknown format_version is reported, and the rest is checked as format 1 so
# every other error is reported with it.
format_version = doc.get("format_version")
if is_int(format_version) and format_version in COMPONENTS:
    fmt = format_version
else:
    fmt = 1
    if "format_version" in doc:
        errors.append(f"format_version must be the integer 1 or 2, got {format_version!r}")

if expect_format and format_version != int(expect_format):
    errors.append(f"expected format_version {int(expect_format)}, manifest has {format_version!r}")

for key in TOP_KEYS[fmt]:
    if key not in doc:
        errors.append(f"missing top-level key: {key}")
if preview and fmt != 2:
    errors.append("the preview manifest is format_version 2")
for key in doc:
    if key == "commits" and preview:
        continue
    if key not in TOP_KEYS[fmt]:
        errors.append(f"unknown top-level key: {key} "
                      "(adding a key is a format_version bump, not a new key)")

version = doc.get("version")
parsed = None
if preview:
    if version is not None:
        errors.append(f"the preview manifest's version must be null, got {version!r}")
elif not isinstance(version, str):
    errors.append(f"version must be a string, got {version!r}")
elif "+" in version:
    errors.append(f"version must not carry semver build metadata: {version!r}")
elif not (parsed := SEMVER.fullmatch(version)):
    errors.append(f"version must be strict semver X.Y.Z[-prerelease], got {version!r}")

prerelease = doc.get("prerelease")
if not isinstance(prerelease, bool):
    errors.append(f"prerelease must be a boolean, got {prerelease!r}")
elif preview and prerelease:
    errors.append("the preview manifest's prerelease must be false")
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
want_components = COMPONENTS[fmt]
if not isinstance(components, list):
    errors.append(f"components must be an array, got {components!r}")
elif len(components) != len(want_components):
    errors.append(
        f"components must hold exactly {len(want_components)} components "
        f"({', '.join(name for name, _ in want_components)}), got {len(components)}"
    )
else:
    for index, (component, (want_name, want_image)) in enumerate(zip(components, want_components)):
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
                          f"(the {len(want_components)} components are fixed, in this order)")

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

# A floor has the top-level version's grammar and must not order above it.
if fmt == 2 and "floor" in doc:
    floor = doc["floor"]
    if not isinstance(floor, list):
        errors.append(f"floor must be an array, got {floor!r}")
    elif len(floor) != len(FLOOR):
        errors.append(
            f"floor must hold exactly {len(FLOOR)} entries "
            f"({', '.join(FLOOR)}), got {len(floor)}"
        )
    else:
        for index, (entry, want_name) in enumerate(zip(floor, FLOOR)):
            where = f"floor[{index}]"
            if not isinstance(entry, dict):
                errors.append(f"{where} must be an object, got {entry!r}")
                continue
            for key in FLOOR_KEYS:
                if key not in entry:
                    errors.append(f"{where} missing key: {key}")
            for key in entry:
                if key not in FLOOR_KEYS:
                    errors.append(f"{where} unknown key: {key}")

            name = entry.get("name")
            if name != want_name:
                errors.append(f"{where}.name must be {want_name!r}, got {name!r} "
                              "(the floor names node-agent, then recovery-actor)")

            floor_version = entry.get("version")
            floor_parsed = None
            if not isinstance(floor_version, str):
                errors.append(f"{where}.version must be a string, got {floor_version!r}")
            elif "+" in floor_version:
                errors.append(f"{where}.version must not carry semver build metadata: "
                              f"{floor_version!r}")
            elif not (floor_parsed := SEMVER.fullmatch(floor_version)):
                errors.append(f"{where}.version must be strict semver X.Y.Z[-prerelease], "
                              f"got {floor_version!r}")
            if (floor_parsed is not None and parsed is not None
                    and semver_key(floor_parsed) > semver_key(parsed)):
                errors.append(
                    f"{where}.version {floor_version!r} orders above the release version "
                    f"{version!r} (a floor may not be newer than the release declaring it)"
                )

# `commits`: only the preview manifest may carry it (the key loop above refuses it
# on a release asset). Same bounds as the control plane's ParsePreviewManifest: a
# control, separator or bidi character in a subject makes the whole document invalid.
if preview and "commits" in doc:
    commits = doc["commits"]
    if not isinstance(commits, list):
        errors.append(f"commits must be an array, got {commits!r}")
    elif len(commits) > MAX_COMMITS:
        errors.append(f"commits holds {len(commits)} entries, at most {MAX_COMMITS}")
    else:
        for index, entry in enumerate(commits):
            where = f"commits[{index}]"
            if not isinstance(entry, dict) or sorted(entry) != sorted(COMMIT_KEYS):
                errors.append(f"{where} must be exactly {{commit, subject}}, got {entry!r}")
                continue
            if not isinstance(entry["commit"], str) or not COMMIT.fullmatch(entry["commit"]):
                errors.append(f"{where}.commit must be 40 lowercase hex characters, "
                              f"got {entry['commit']!r}")
            subject = entry["subject"]
            if (not isinstance(subject, str) or len(subject) > MAX_SUBJECT
                    or any(unsafe_subject_char(c) for c in subject)):
                errors.append(f"{where}.subject must be one line of at most {MAX_SUBJECT} "
                              f"characters with no control, separator or bidi character, got {subject!r}")

# The --expect-* assertions: the caller already knows these, so a mismatch means
# the manifest describes something other than what this run built.
if expect_version and version != expect_version:
    errors.append(f"expected version {expect_version!r}, manifest has {version!r}")

if isinstance(components, list):
    by_name = {c.get("name"): c for c in components if isinstance(c, dict)}
    for flag, name in ((expect_control, "control-plane"), (expect_agent, "node-agent"),
                       (expect_recovery, "recovery-actor")):
        if not flag:
            continue
        got = (by_name.get(name) or {}).get("digest")
        if got != flag:
            errors.append(f"expected {name} digest {flag!r}, manifest has {got!r}")

report()
PY
