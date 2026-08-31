#!/usr/bin/env bash
# Local release-preflight contract test. Uses a detached clean worktree because
# strict evidence correctly rejects the caller's dirty tree.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
worktree="$(mktemp -d /tmp/quasar-release-preflight-test.XXXXXX)"
evidence_dir="$(mktemp -d /tmp/quasar-release-evidence-test.XXXXXX)"
digest="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

cleanup() {
  git -C "$repo" worktree remove --force "$worktree" >/dev/null 2>&1 || true
  git -C "$repo" worktree prune >/dev/null 2>&1 || true
  rm -rf "$evidence_dir"
}
trap cleanup EXIT

fail() { echo "release preflight test: $*" >&2; exit 1; }

git -C "$repo" worktree add --detach "$worktree" HEAD >/dev/null
git -C "$worktree" submodule update --init protocol >/dev/null

# The manifest must inventory only patch inputs the candidate Dockerfile applies.
# A stale manifest entry blocks strict preflight with a generic missing-file error
# even though it is not an image input; a missing applied patch must instead be
# caught here before any candidate evidence is accepted.
python3 - "$worktree" <<'PY'
import json
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
manifest = json.loads((root / "scripts/release/release-manifest.json").read_text())
dockerfile = (root / "deploy/Dockerfile.vulkan").read_text()
applied = {
    f"deploy/patches/vulkan/{name}"
    for line in dockerfile.splitlines()
    for name in re.findall(r"^\s*&&\s*git apply(?:\s+--[A-Za-z0-9-]+)*\s+/tmp/([^/\s]+\.patch)(?:\s+\\)?$", line)
}
declared = set(manifest["patches"])
assert declared == applied, (
    "manifest patch inventory differs from Dockerfile.vulkan git apply inputs: "
    f"declared-only={sorted(declared - applied)}, applied-only={sorted(applied - declared)}"
)
missing = sorted(rel for rel in declared if not (root / rel).is_file())
assert not missing, f"manifest declares absent patch inputs: {missing}"
PY

valid_base="ghcr.io/accreleus/quasar-base@sha256:$digest"

preflight() {
  QUASAR_CONTROL_IMAGE="$1" QUASAR_AGENT_IMAGE="$2" QUASAR_BASE_IMAGE="$valid_base" \
    "$worktree/scripts/release/release-preflight.sh" --require-artifact-images --output "$3"
}

valid_control="registry.example/quasar-control@sha256:$digest"
valid_vulkan="registry.example:5000/quasar/vulkan@sha256:$digest"
valid_out="$evidence_dir/valid.json"
preflight "$valid_control" "$valid_vulkan" "$valid_out"
python3 - "$valid_out" <<'PY'
import json, sys
report = json.load(open(sys.argv[1]))
assert report["result"] == "PASS", report["errors"]
assert report["source"]["exact_identity"] is True, report["source"]
assert report["output"]["inside_repository"] is False, report["output"]
assert all(image["exact_digest_reference"] for image in report["release_artifact_images"])
assert report["release_base_image"]["exact_digest_reference"] is True, report["release_base_image"]
# With the base digest supplied, every ${QUASAR_BASE_IMAGE} FROM must have
# resolved digest-pinned — no release Dockerfile base may stay tag-unpinned.
for df in report["dockerfiles"]:
    for base in df["bases"]:
        assert base["pin_status"] in ("digest-pinned", "internal-stage"), (df["path"], base)
PY

# Strict preflight without the base identity must fail: the artifacts' FROM
# lineage would be unrecorded and the committed :latest default is mutable.
if QUASAR_CONTROL_IMAGE="$valid_control" QUASAR_AGENT_IMAGE="$valid_vulkan" \
    "$worktree/scripts/release/release-preflight.sh" --require-artifact-images \
    --output "$evidence_dir/nobase.json" >/dev/null 2>&1; then
  fail "unexpected strict success without QUASAR_BASE_IMAGE"
fi
python3 -c 'import json,sys; report=json.load(open(sys.argv[1])); assert report["result"] == "FAIL" and any("QUASAR_BASE_IMAGE" in e for e in report["errors"]), report["errors"]' "$evidence_dir/nobase.json"

# A tag-form base is mutable metadata, exactly like a tagged artifact image.
if QUASAR_CONTROL_IMAGE="$valid_control" QUASAR_AGENT_IMAGE="$valid_vulkan" \
    QUASAR_BASE_IMAGE="ghcr.io/accreleus/quasar-base:latest" \
    "$worktree/scripts/release/release-preflight.sh" --require-artifact-images \
    --output "$evidence_dir/tagbase.json" >/dev/null 2>&1; then
  fail "unexpected strict success with tag-form QUASAR_BASE_IMAGE"
fi
python3 -c 'import json,sys; report=json.load(open(sys.argv[1])); assert report["result"] == "FAIL" and any("QUASAR_BASE_IMAGE must be exact" in e for e in report["errors"]), report["errors"]' "$evidence_dir/tagbase.json"

reject() {
  local control=$1 vulkan=$2 output=$3 needle=$4 actual payload
  if actual=$(preflight "$control" "$vulkan" "$output" 2>&1); then
    fail "unexpected strict success: $control / $vulkan"
  fi
  payload=$actual
  [[ -s "$output" ]] && payload=$(<"$output")
  printf '%s' "$payload" | python3 -c 'import json,sys; report=json.load(sys.stdin); needle=sys.argv[1]; assert report["result"] == "FAIL" and any(needle in e for e in report["errors"]), report["errors"]' "$needle"
}

# A Docker-valid tag-plus-digest is still mutable metadata, so reject it.
reject "registry.example/quasar-control:1@sha256:$digest" "$valid_vulkan" \
  "$evidence_dir/tagged.json" "QUASAR_CONTROL_IMAGE must be exact"
reject "registry.example/quasar-control@sha256:abcd" "$valid_vulkan" \
  "$evidence_dir/malformed.json" "QUASAR_CONTROL_IMAGE must be exact"

# Every strict output under candidate repository is rejected and must not write.
inside="$worktree/strict-preflight-inside.json"
reject "$valid_control" "$valid_vulkan" "$inside" "strict preflight output must be outside repository"
test ! -e "$inside" || fail "strict in-repo output was written"

# Mutable release bases are rejected even when the candidate artifact names are
# exact digest references. The preflight must cover the whole build graph, not
# merely the images supplied by the operator.
python3 - "$worktree/deploy/Dockerfile.control.prod" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = "FROM node@sha256:a25c9934ff6382cd4f08b6bc26c82bf4ea69b1e6f8dabfb2ead457374127c365 AS web-builder"
assert old in text
path.write_text(text.replace(old, "FROM node:22 AS web-builder", 1))
PY
reject "$valid_control" "$valid_vulkan" "$evidence_dir/unpinned-base.json" \
  "release Dockerfile base must be digest-pinned"

echo "Release preflight strict contract: PASS"
