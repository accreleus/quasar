#!/usr/bin/env bash
# Shared fail-closed helpers for local, immutable release-artifact evidence.
# This file is sourced by the two public release evidence commands.
set -euo pipefail

release_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# No tag component is allowed. A registry port is the only colon permitted
# before the immutable sha256 digest.
release_exact_digest_ref() {
  [[ "$1" =~ ^([a-z0-9][a-z0-9.-]*(:[0-9]+)?/)?([a-z0-9]+([._-][a-z0-9]+)*/)*[a-z0-9]+([._-][a-z0-9]+)*@sha256:[0-9a-f]{64}$ ]]
}

release_fail() { printf 'release artifact evidence: %s\n' "$*" >&2; exit 1; }

release_require_external_output() {
  local output=$1 output_dir
  [[ -n "$output" ]] || release_fail "--output is required"
  output_dir="$(cd "$(dirname "$output")" && pwd)" || release_fail "output directory does not exist"
  RELEASE_BUNDLE="$output_dir/$(basename "$output")"
  # `-e` misses dangling symlinks. A final evidence bundle is always a real
  # directory; any pre-existing filesystem entry is a collision.
  [[ ! -e "$RELEASE_BUNDLE" && ! -L "$RELEASE_BUNDLE" ]] \
    || release_fail "refusing to replace existing evidence bundle: $RELEASE_BUNDLE"
  case "$RELEASE_BUNDLE" in "$release_root"/*) release_fail "evidence output must be outside repository";; esac
}

release_require_local_image() {
  local image=$1 inspected
  release_exact_digest_ref "$image" || release_fail "image must be exact name@sha256:<64 lowercase hex>: $image"
  inspected="$(docker image inspect --format '{{json .RepoDigests}}' "$image" 2>/dev/null)" \
    || release_fail "candidate image is not present locally: $image"
  python3 - "$image" "$inspected" <<'PY' || exit 1
import json, sys
image, repo_digests = sys.argv[1:]
try:
    values = json.loads(repo_digests)
except json.JSONDecodeError:
    raise SystemExit("candidate image RepoDigests unavailable")
if image not in values:
    raise SystemExit("candidate image local RepoDigests does not contain exact requested digest")
PY
  RELEASE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$image")"
}

release_require_scanner() {
  local scanner=$1 inspected
  release_exact_digest_ref "$scanner" || release_fail "scanner image pin is malformed: $scanner"
  inspected="$(docker image inspect --format '{{json .RepoDigests}}' "$scanner" 2>/dev/null)" \
    || release_fail "scanner image is not present locally: $scanner"
  python3 - "$scanner" "$inspected" <<'PY' || exit 1
import json, sys
scanner, repo_digests = sys.argv[1:]
try:
    values = json.loads(repo_digests)
except json.JSONDecodeError:
    raise SystemExit("scanner image RepoDigests unavailable")
if scanner not in values:
    raise SystemExit("scanner image local RepoDigests does not contain exact requested digest")
PY
  RELEASE_SCANNER_ID="$(docker image inspect --format '{{.Id}}' "$scanner")"
}

release_save_image() {
  RELEASE_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/quasar-release-artifact.XXXXXX")"
  RELEASE_ARCHIVE="$RELEASE_TMPDIR/image.tar"
  RELEASE_CONTAINER_TMP="$RELEASE_TMPDIR/container-tmp"
  mkdir "$RELEASE_CONTAINER_TMP"
  chmod 1777 "$RELEASE_CONTAINER_TMP"
  trap 'rm -rf "$RELEASE_TMPDIR" "${RELEASE_STAGE:-}"' EXIT
  docker image save --output "$RELEASE_ARCHIVE" "$RELEASE_IMAGE"
  chmod 0644 "$RELEASE_ARCHIVE"
}

release_prepare_evidence_files() {
  local output_dir base
  output_dir="$(dirname "$RELEASE_BUNDLE")"
  base="$(basename "$RELEASE_BUNDLE")"
  # mktemp creates mode 0700. Nothing becomes visible at destination until
  # both reports are complete and validated.
  RELEASE_STAGE="$(mktemp -d "$output_dir/.${base}.staging.XXXXXX")"
  RELEASE_EVIDENCE="$RELEASE_STAGE/evidence.json"
  RELEASE_PROVENANCE="$RELEASE_STAGE/provenance.json"
}

release_validate_json() {
  python3 -m json.tool "$1" >/dev/null \
    || release_fail "scanner did not emit valid JSON"
}

release_write_provenance() {
  local kind=$1
  python3 - "$kind" "$RELEASE_IMAGE" "$RELEASE_IMAGE_ID" "$RELEASE_SCANNER" \
    "$RELEASE_SCANNER_ID" "$RELEASE_EVIDENCE" "$RELEASE_PROVENANCE" <<'PY'
import datetime as dt
import hashlib
import json
import sys
from pathlib import Path

kind, candidate, candidate_id, scanner, scanner_id, evidence, destination = sys.argv[1:]
digest = hashlib.sha256(Path(evidence).read_bytes()).hexdigest()
payload = {
    "schema_version": 1,
    "kind": kind,
    "generated_at_utc": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat(),
    "candidate_image": candidate,
    "candidate_image_id": candidate_id,
    "scanner_image": scanner,
    "scanner_image_id": scanner_id,
    "network": "disabled",
    "candidate_source": "local Docker image archive",
    "evidence_sha256": digest,
}
Path(destination).write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
PY
}

release_publish_bundle() {
  [[ -s "$RELEASE_EVIDENCE" && -s "$RELEASE_PROVENANCE" ]] \
    || release_fail "evidence bundle is incomplete"
  release_validate_json "$RELEASE_EVIDENCE"
  release_validate_json "$RELEASE_PROVENANCE"
  # Directory rename must be atomic and no-replace. Darwin supplies
  # renamex_np(RENAME_EXCL); glibc supplies renameat2(RENAME_NOREPLACE).
  # Failing closed is safer than a fallback that can replace an empty target.
  python3 - "$RELEASE_STAGE" "$RELEASE_BUNDLE" <<'PY' || release_fail "evidence bundle destination collision or atomic no-replace rename unavailable"
import ctypes
import errno
import os
import sys

source, destination = (os.fsencode(value) for value in sys.argv[1:])
try:
    os.lstat(destination)
except FileNotFoundError:
    pass
else:
    raise SystemExit("destination already exists")

libc = ctypes.CDLL(None, use_errno=True)
if hasattr(libc, "renamex_np"):
    # macOS: RENAME_EXCL. This fails rather than replacing any target entry.
    result = libc.renamex_np(source, destination, 0x00000004)
elif hasattr(libc, "renameat2"):
    # Linux: AT_FDCWD, AT_FDCWD, RENAME_NOREPLACE.
    result = libc.renameat2(-100, source, -100, destination, 1)
else:
    raise SystemExit("atomic no-replace directory rename unsupported")
if result:
    error = ctypes.get_errno()
    if error == errno.EEXIST:
        raise SystemExit("destination collision")
    raise SystemExit(os.strerror(error))
PY
  RELEASE_STAGE=""
}
