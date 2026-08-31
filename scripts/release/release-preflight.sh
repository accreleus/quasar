#!/usr/bin/env bash
# Record, do not invent, release-candidate identities.  This is deliberately
# local-only: publishing, signing, scanning and SBOM creation remain separate
# release gates with their own verifiable artifacts.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
manifest="$root/scripts/release/release-manifest.json"
output=""
require_artifact_images=0

usage() {
  cat <<'EOF'
usage: scripts/release/release-preflight.sh [--output PATH] [--require-artifact-images]

Writes JSON release-input evidence to stdout, or PATH. Exit non-zero only when
the manifest no longer matches declared source pins or a declared input is missing.
Unpinned image bases and unavailable SBOM/scan/signing tools are reported, never passed.
EOF
}

while (($#)); do
  case "$1" in
    --output) output=${2:?--output needs a path}; shift 2 ;;
    --require-artifact-images) require_artifact_images=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

if [[ -n "$output" ]]; then
  output="$(cd "$(dirname "$output")" && pwd)/$(basename "$output")"
fi

python3 - "$root" "$manifest" "$output" "$require_artifact_images" <<'PY'
import datetime as dt
import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

root = Path(sys.argv[1])
manifest_path = Path(sys.argv[2])
output = sys.argv[3]
require_artifact_images = sys.argv[4] == "1"
manifest = json.loads(manifest_path.read_text())
errors = []

def run(args, cwd=root):
    try:
        p = subprocess.run(args, cwd=cwd, text=True, stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, timeout=20, check=False)
        return p.stdout.strip() if p.returncode == 0 else None
    except (OSError, subprocess.TimeoutExpired):
        return None

def command_identity(command):
    executable = shutil.which(command)
    if not executable:
        return {"available": False}
    version = run([command, "--version"])
    if version is None:
        version = run([command, "version"])
    return {"available": True, "path": executable, "version": version}

def sha256(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()

def docker_args(path):
    values = {}
    for line in path.read_text().splitlines():
        match = re.match(r"\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)(?:=(\S+))?\s*$", line)
        if match:
            values[match.group(1)] = match.group(2)
    return values

def base_status(image):
    if "@sha256:" in image:
        return "digest-pinned"
    if "${" in image:
        return "indirect-or-unpinned"
    return "tag-unpinned"

def exact_digest_ref(image):
    # Docker permits `repo:tag@digest`, but a release candidate must not carry
    # a mutable tag component. The optional colon below is a registry port only.
    repository = r"(?:[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/)?(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*"
    return bool(image and re.fullmatch(repository + r"@sha256:[0-9a-f]{64}", image))

for target in manifest["supported_targets"]:
    for rel in target["compose"]:
        if not (root / rel).is_file():
            errors.append(f"missing compose input for {target['name']}: {rel}")

dockerfile_report = []
for artifact in manifest["artifacts"]:
    if "dockerfile" not in artifact:
        continue
    path = root / artifact["dockerfile"]
    if not path.is_file():
        errors.append(f"missing Dockerfile: {artifact['dockerfile']}")
        continue
    args = docker_args(path)
    bases = []
    stages = set()
    for line in path.read_text().splitlines():
        match = re.match(r"\s*FROM\s+([^\s]+)(?:\s+AS\s+([^\s]+))?", line, re.IGNORECASE)
        if not match:
            continue
        raw = match.group(1)
        # Environment wins over the ARG default, mirroring `docker build
        # --build-arg`: build-images.sh always passes QUASAR_BASE_IMAGE
        # explicitly, so the committed :latest default is a dev convenience,
        # not the release identity. Strict mode separately REQUIRES the env
        # to be an exact digest reference (see release_base_image below), so
        # the FROM lines below resolve digest-pinned on a real candidate.
        resolved = re.sub(
            r"\$\{([^}]+)\}",
            lambda m: os.environ.get(m.group(1)) or args.get(m.group(1)) or m.group(0),
            raw,
        )
        status = "internal-stage" if resolved in stages else base_status(resolved)
        if require_artifact_images and status not in ("digest-pinned", "internal-stage"):
            errors.append(
                f"release Dockerfile base must be digest-pinned: {artifact['dockerfile']}:{raw} ({status})"
            )
        bases.append({"raw": raw, "resolved": resolved, "pin_status": status})
        if match.group(2):
            stages.add(match.group(2))
    dockerfile_report.append({
        "artifact": artifact["name"], "path": artifact["dockerfile"],
        "sha256": sha256(path), "target": artifact.get("target"), "bases": bases,
    })

# deploy/pins.env is the single source of truth for these pins (2026-08-20 --
# they used to live as ARG defaults in deploy/Dockerfile.vulkan, duplicated in
# a build stage and a runtime stage). Reading the Dockerfile here directly is
# what broke: pins.env's keys are declared ONCE at global scope in the
# Dockerfile, and every stage that needs one re-declares it BARE (`ARG NAME`,
# no default) to inherit that default -- a plain "last ARG line wins" scan
# (which is what this script used to do) picks up one of those bare
# re-declarations instead and reports None for every pin. build-images.sh's
# check_pins_agree already asserts the Dockerfile's global defaults match
# pins.env on every build, so this script does not need to re-parse the
# Dockerfile at all -- it reads pins.env directly, via the one canonical
# single-key reader (deploy/lib/pins.sh's pin_value), so there is still only
# one KEY=VALUE parser for pins.env's grep-friendly format, not a second one
# copy-pasted in here.
pins_lib = root / "deploy" / "lib" / "pins.sh"
pins_file = root / "deploy" / "pins.env"
if not pins_lib.is_file():
    errors.append(f"missing pins reader: {pins_lib.relative_to(root)}")
if not pins_file.is_file():
    errors.append(f"missing pin source of truth: {pins_file.relative_to(root)}")

def pins_env_value(key):
    return run(["bash", "-c",
                f"source {shlex.quote(str(pins_lib))} && pin_value {shlex.quote(key)}"])

pin_report = []
for expected in manifest["upstream_pins"]:
    key = expected["arg"]
    observed = pins_env_value(key)
    matches = observed == expected["value"]
    if not matches:
        errors.append(
            f"pin drift: release-manifest.json upstream_pins records {key}="
            f"{expected['value']!r} but deploy/pins.env has {key}={observed!r} -- "
            "deploy/pins.env is the source of truth (build-images.sh's "
            "check_pins_agree enforces deploy/Dockerfile.vulkan matches it at "
            "build time); update release-manifest.json to match pins.env."
        )
    status = "git-commit-pinned" if re.fullmatch(r"[0-9a-f]{7,40}", expected["value"]) else "version-pinned"
    pin_report.append({
        **expected,
        "source": "deploy/pins.env",
        "observed": observed,
        "matches_manifest": matches,
        "pin_status": status,
    })

patch_report = []
for rel in manifest["patches"]:
    path = root / rel
    if not path.is_file():
        errors.append(f"missing patch: {rel}")
        continue
    patch_report.append({"path": rel, "sha256": sha256(path)})

# The legacy list is evidence too: an entry naming a file that no longer exists
# is a stale claim about the tree, so stat every one instead of copying the
# array into the report verbatim.
legacy_report = []
for rel in manifest["legacy_non_release_dockerfiles"]:
    exists = (root / rel).is_file()
    if not exists:
        errors.append(f"legacy_non_release_dockerfiles names a missing file: {rel}")
    legacy_report.append({"path": rel, "exists": exists})

external = []
for artifact in manifest["artifacts"]:
    if artifact.get("external"):
        external.append({"artifact": artifact["name"], "image": artifact["image"],
                         "pin_status": base_status(artifact["image"])})

release_images = []
for env_name, artifact in (("QUASAR_CONTROL_IMAGE", "quasar-control"),
                           ("QUASAR_AGENT_IMAGE", "quasar-agent")):
    image = os.environ.get(env_name)
    item = {"artifact": artifact, "environment": env_name, "image": image}
    if image:
        item["pin_status"] = "digest-pinned" if exact_digest_ref(image) else base_status(image)
        item["exact_digest_reference"] = exact_digest_ref(image)
        if require_artifact_images and not item["exact_digest_reference"]:
            errors.append(f"{env_name} must be exact name@sha256:<64 lowercase hex>, got {image}")
    else:
        item["pin_status"] = "missing"
        if require_artifact_images:
            errors.append(f"missing required release artifact image: {env_name}")
    release_images.append(item)

# The quasar-base family is a build INPUT, not a published artifact, but a
# candidate's identity is incomplete without the exact base its artifacts were
# built FROM (${QUASAR_BASE_IMAGE} in both release Dockerfiles).
base_image = os.environ.get("QUASAR_BASE_IMAGE")
release_base_image = {"environment": "QUASAR_BASE_IMAGE", "image": base_image}
if base_image:
    release_base_image["exact_digest_reference"] = exact_digest_ref(base_image)
    release_base_image["pin_status"] = (
        "digest-pinned" if release_base_image["exact_digest_reference"] else base_status(base_image)
    )
    if require_artifact_images and not release_base_image["exact_digest_reference"]:
        errors.append(f"QUASAR_BASE_IMAGE must be exact name@sha256:<64 lowercase hex>, got {base_image}")
else:
    release_base_image["pin_status"] = "missing"
    if require_artifact_images:
        errors.append(
            "missing required release base image: QUASAR_BASE_IMAGE "
            "(the exact quasar-base digest the release artifacts were built FROM)"
        )

protocol = root / "protocol"
output_inside_repo = bool(output and Path(output).resolve().is_relative_to(root.resolve()))
if require_artifact_images and output_inside_repo:
    errors.append("strict preflight output must be outside repository; exact release identity cannot write an unignored repo file")
super_dirty = (run(["git", "status", "--porcelain"]) or "").splitlines()
protocol_recorded = run(["git", "submodule", "status", "--", "protocol"])
protocol_head = run(["git", "rev-parse", "HEAD"], protocol) if protocol.is_dir() else None
protocol_dirty = (run(["git", "status", "--porcelain"], protocol) or "").splitlines() if protocol.is_dir() else []
tree_entry = run(["git", "ls-tree", "HEAD", "--", "protocol"])
expected_protocol_head = tree_entry.split()[2] if tree_entry and len(tree_entry.split()) >= 3 else None
protocol_initialized = bool(protocol_recorded and not protocol_recorded.startswith("-"))
protocol_matches_superproject = bool(protocol_initialized and protocol_head and expected_protocol_head == protocol_head)
exact_source_identity = bool(not super_dirty and protocol_initialized and protocol_matches_superproject and not protocol_dirty)
if require_artifact_images and not exact_source_identity:
    if super_dirty:
        errors.append("superproject tree dirty; exact release identity requires clean tree")
    if not protocol_initialized:
        errors.append("protocol submodule uninitialized; exact release identity requires initialized submodule")
    elif not protocol_matches_superproject:
        errors.append("protocol submodule revision differs from superproject pin")
    if protocol_dirty:
        errors.append("protocol submodule dirty; exact release identity requires clean submodule")
source = {
    "revision": run(["git", "rev-parse", "HEAD"]),
    "describe": run(["git", "describe", "--always", "--dirty", "--tags"]),
    "dirty_paths": super_dirty,
    "exact_identity": exact_source_identity,
    "exact_identity_requirement": "clean superproject plus initialized, pin-matched, clean protocol submodule",
    "protocol_submodule": {
        "recorded": protocol_recorded,
        "expected_head": expected_protocol_head,
        "head": protocol_head,
        "initialized": protocol_initialized,
        "matches_superproject": protocol_matches_superproject,
        "dirty_paths": protocol_dirty,
        "describe": run(["git", "describe", "--always", "--dirty", "--tags"], protocol) if protocol.is_dir() else None,
    },
}

driver = {"nvidia_smi": command_identity("nvidia-smi")}
if driver["nvidia_smi"].get("available"):
    query = run(["nvidia-smi", "--query-gpu=name,driver_version,uuid", "--format=csv,noheader"])
    driver["gpus"] = query.splitlines() if query else []

tools = {name: command_identity(name) for name in (
    "docker", "git", "python3", "syft", "trivy", "cosign", "oras"
)}
if tools["docker"].get("available"):
    tools["docker"]["compose_version"] = run(["docker", "compose", "version"])
    tools["docker"]["buildx_version"] = run(["docker", "buildx", "version"])

report = {
    "schema_version": 1,
    "generated_at_utc": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat(),
    "manifest": {"path": str(manifest_path.relative_to(root)), "sha256": sha256(manifest_path)},
    "pins_env": {"path": str(pins_file.relative_to(root)), "sha256": sha256(pins_file) if pins_file.is_file() else None},
    "output": {"path": output or None, "inside_repository": output_inside_repo},
    "result": "PASS" if not errors else "FAIL",
    "errors": errors,
    "source": source,
    "supported_targets": manifest["supported_targets"],
    "dockerfiles": dockerfile_report,
    "external_images": external,
    "release_artifact_images": release_images,
    "release_base_image": release_base_image,
    "upstream_pins": pin_report,
    "patches": patch_report,
    "legacy_non_release_dockerfiles": legacy_report,
    "tools": tools,
    "driver": driver,
    "release_evidence_required": manifest["required_release_evidence"],
    "unavailable_release_gates": [
        name for name in ("syft", "trivy", "cosign") if not tools[name].get("available")
    ],
}
encoded = json.dumps(report, indent=2, sort_keys=True) + "\n"
if output and not (require_artifact_images and output_inside_repo):
    Path(output).write_text(encoded)
else:
    print(encoded, end="")
sys.exit(0 if not errors else 1)
PY
