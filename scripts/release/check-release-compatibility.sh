#!/usr/bin/env bash
# The release-time check (ADR 0008): refuse a format-2 release that could leave
# an owned machine unmanageable after a one-step update. Offline; every input is
# a file (scripts/release/collect-release-compatibility-inputs.sh gathers them in
# CI).
#
# Refused, every reason printed, when:
#   (a) the candidate recovery actor's window for control-plane, node-agent or
#       recovery-actor does not contain the recipe revision of the candidate's own
#       image for that role;
#   (b) for c in node-agent, recovery-actor: a known release r with
#       floor_c <= r.version <= candidate.version has a c image whose revision is
#       outside the candidate actor's c window; or the previous release's
#       control-plane revision is outside its control-plane window (restoring a
#       pre-update control plane, ADR 0008 Rule B);
#   (c) a floor orders above the previous release, so a machine that release left
#       current would be below the floor after the update.
# Known releases are the previously published format-2 manifests; format-1 ones
# are ignored, because no owned install predates the first format-2 release. The
# previous release is the known one ordering highest strictly below the
# candidate; with none, (b) has nothing to check beyond (a) and (c) passes.
#
# Exit codes: 0 compatible (prints PASS) · 1 refused or bad input · 2 usage.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
validator="$root/scripts/release/validate-platform-release-manifest.sh"

manifest=""
known=""
recipes=""
windows=""

usage() {
  cat <<'EOF'
usage: scripts/release/check-release-compatibility.sh \
         --manifest platform-release-manifest.v2.json \
         --known DIR \
         --recipes recipes.json \
         --actor-windows actor-windows.json

  --manifest       the candidate format-2 manifest
  --known          previously published manifests (*.json); files whose
                   format_version is not 2 are ignored
  --recipes        {"<image>@<digest>": <org.quasar.recipe>, ...} for every image
                   the candidate and the known format-2 manifests name
  --actor-windows  the candidate recovery actor's `quasar-recovery recipes` output,
                   {"format_version":1,"windows":{"<role>":{"from":N,"to":M},...}}

Prints every refusal on stderr and exits 1; prints PASS and exits 0.
EOF
}

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?--manifest needs a path}; shift 2 ;;
    --known) known=${2:?--known needs a directory}; shift 2 ;;
    --recipes) recipes=${2:?--recipes needs a path}; shift 2 ;;
    --actor-windows) windows=${2:?--actor-windows needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

for required in manifest known recipes windows; do
  if [[ -z "${!required}" ]]; then
    echo "check-release-compatibility: missing a required flag (--${required/windows/actor-windows})" >&2
    usage >&2
    exit 2
  fi
done

if ! "$validator" "$manifest" --expect-format 2 >/dev/null; then
  echo "check-release-compatibility: the candidate $manifest is not a valid format-2 manifest" >&2
  exit 1
fi

python3 - "$validator" "$manifest" "$known" "$recipes" "$windows" <<'PY'
import json
import re
import subprocess
import sys
from pathlib import Path

validator, manifest_path, known_dir, recipes_path, windows_path = sys.argv[1:6]
errors = []

ROLES = ("control-plane", "node-agent", "postgres", "recovery-actor")
FLOORED = ("node-agent", "recovery-actor")
# Same grammar and precedence as validate-platform-release-manifest.sh.
SEMVER = re.compile(
    r"(?P<core>(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))"
    r"(?:-(?P<pre>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)"
    r"(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?"
)


def semver_key(version):
    match = SEMVER.fullmatch(version)
    core = tuple(int(part) for part in match.group("core").split("."))
    pre = match.group("pre")
    if pre is None:
        return core, (1,)
    ids = tuple((0, int(i), "") if i.isdigit() else (1, 0, i) for i in pre.split("."))
    return core, (0, ids)


def is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def done():
    if errors:
        for error in errors:
            print(f"check-release-compatibility: {error}", file=sys.stderr)
        print(f"check-release-compatibility: REFUSED ({len(errors)} reason(s))", file=sys.stderr)
        raise SystemExit(1)


def load_json(path, what):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError, UnicodeDecodeError) as exc:
        errors.append(f"{what} {path} is unreadable: {exc}")
        return None


def refs(doc):
    return {c["name"]: f'{c["image"]}@{c["digest"]}' for c in doc["components"]}


def floors(doc):
    return {f["name"]: f["version"] for f in doc["floor"]}


candidate = load_json(manifest_path, "candidate manifest")
done()
version = candidate["version"]

# The known releases.
known = []
known_path = Path(known_dir)
if not known_path.is_dir():
    errors.append(f"--known is not a directory: {known_dir}")
else:
    for path in sorted(known_path.glob("*.json")):
        doc = load_json(path, "known manifest")
        if doc is None:
            continue
        if not isinstance(doc, dict) or doc.get("format_version") != 2:
            print(f"check-release-compatibility: ignoring {path.name} (not format 2)")
            continue
        result = subprocess.run([validator, str(path), "--expect-format", "2"],
                                capture_output=True, text=True)
        if result.returncode != 0:
            errors.append(f"known manifest {path} is not a valid format-2 manifest:\n"
                          + result.stderr.rstrip())
            continue
        if doc["version"] == version:
            errors.append(f"known manifest {path} has the candidate's own version {version}: "
                          "a published release cannot be published again")
            continue
        if any(k["version"] == doc["version"] for _, k in known):
            errors.append(f"known manifest {path} repeats version {doc['version']}")
            continue
        known.append((path, doc))

# The recipe labels.
recipes = load_json(recipes_path, "--recipes")
if recipes is not None:
    if not isinstance(recipes, dict):
        errors.append(f"--recipes {recipes_path} must be a JSON object of image@digest -> revision")
        recipes = None
    else:
        for ref, rev in recipes.items():
            if not is_int(rev) or rev < 1:
                errors.append(f"--recipes: {ref} has recipe revision {rev!r}, "
                              "not a positive integer")

# The candidate actor's windows.
windows = {}
doc = load_json(windows_path, "--actor-windows")
if doc is not None:
    shape = f"--actor-windows {windows_path}"
    if not isinstance(doc, dict) or sorted(doc) != ["format_version", "windows"]:
        errors.append(f"{shape} must be exactly {{\"format_version\", \"windows\"}}")
    elif doc["format_version"] != 1 or not is_int(doc["format_version"]):
        errors.append(f"{shape}: format_version must be 1, got {doc['format_version']!r}")
    elif not isinstance(doc["windows"], dict):
        errors.append(f"{shape}: windows must be an object")
    else:
        for role, window in doc["windows"].items():
            if role not in ROLES:
                errors.append(f"{shape}: unknown role {role!r}")
            elif (not isinstance(window, dict) or sorted(window) != ["from", "to"]
                  or not is_int(window["from"]) or not is_int(window["to"])
                  or not 1 <= window["from"] <= window["to"]):
                errors.append(f"{shape}: {role} must be {{\"from\": N, \"to\": M}} with "
                              f"1 <= N <= M, got {window!r}")
            else:
                windows[role] = (window["from"], window["to"])
done()


def revision(ref):
    rev = recipes.get(ref)
    if rev is None:
        errors.append(f"no recipe revision for {ref} in --recipes "
                      "(its org.quasar.recipe label)")
    return rev


def outside(role, rev):
    """A refusal fragment when `rev` is outside the actor's `role` window, else None."""
    if rev is None:
        return None
    if role not in windows:
        return f"the candidate recovery actor carries no {role} recipe"
    low, high = windows[role]
    if low <= rev <= high:
        return None
    return (f"revision {rev} is outside the candidate recovery actor's {role} window "
            f"{low}..{high}")


below = [doc for _, doc in known if semver_key(doc["version"]) < semver_key(version)]
previous = max(below, key=lambda d: semver_key(d["version"]), default=None)
own = refs(candidate)
floor = floors(candidate)

# (a) The candidate's own images.
for role in ("control-plane", "node-agent", "recovery-actor"):
    why = outside(role, revision(own[role]))
    if why:
        errors.append(f"(a) {version} {role} image {own[role]}: {why}")

# (b) Every known release the floor still covers, and the previous control plane.
for role in FLOORED:
    for _, doc in known:
        if not (semver_key(floor[role]) <= semver_key(doc["version"]) <= semver_key(version)):
            continue
        ref = refs(doc)[role]
        why = outside(role, revision(ref))
        if why:
            errors.append(f"(b) the {role} floor {floor[role]} covers release {doc['version']}, "
                          f"whose {role} image {ref} needs a recipe the candidate actor "
                          f"cannot render: {why}")
if previous is not None:
    ref = refs(previous)["control-plane"]
    why = outside("control-plane", revision(ref))
    if why:
        errors.append(f"(b) restoring the previous release {previous['version']}'s control "
                      f"plane {ref} would fail: {why}")

# (c) The floor against the previous release.
if previous is not None:
    for role in FLOORED:
        if semver_key(floor[role]) > semver_key(previous["version"]):
            errors.append(f"(c) the {role} floor {floor[role]} orders above the previous "
                          f"release {previous['version']}: a {role} that release left "
                          "current would be below the floor after the update")

done()
print(f"PASS {version}: {len(known)} known format-2 release(s), previous "
      f"{previous['version'] if previous else 'none'}")
PY
