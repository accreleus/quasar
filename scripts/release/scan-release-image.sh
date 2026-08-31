#!/usr/bin/env bash
# Produce an offline Trivy JSON scan for a local immutable release candidate.
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-supply-chain-lib.sh"

# Verified with `docker buildx imagetools inspect aquasec/trivy:0.69.3` on
# 2026-07-13. This is the immutable multi-platform index digest, not a tag.
RELEASE_SCANNER="aquasec/trivy@sha256:bcc376de8d77cfe086a917230e818dc9f8528e3c852f7b1aff648949b6258d1c"
RELEASE_IMAGE=""
TRIVY_CACHE_DIR=""

usage() {
  cat <<'EOF'
usage: scripts/release/scan-release-image.sh --image name@sha256:<digest> --cache-dir /approved/trivy-cache --output /external/path/trivy-bundle

Candidate and scanner must already exist in local Docker image store. Cache directory
must contain an approved current Trivy DB. Command runs offline and fails if DB is
missing or unusable; cache freshness must be attested by its supplying process. It never
pulls, publishes, mounts Docker socket, or enables network. It atomically creates the new
<output>/ bundle containing evidence.json and provenance.json; existing output is rejected.
EOF
}

while (($#)); do
  case "$1" in
    --image) RELEASE_IMAGE=${2:?--image needs a value}; shift 2 ;;
    --cache-dir) TRIVY_CACHE_DIR=${2:?--cache-dir needs a path}; shift 2 ;;
    --output) requested_output=${2:?--output needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

release_require_external_output "${requested_output:-}"
release_require_local_image "$RELEASE_IMAGE"
release_require_scanner "$RELEASE_SCANNER"
[[ -d "$TRIVY_CACHE_DIR" ]] || release_fail "--cache-dir must be an existing approved Trivy cache directory"
TRIVY_CACHE_DIR="$(cd "$TRIVY_CACHE_DIR" && pwd)"
release_save_image
release_prepare_evidence_files

docker run --rm --network none --read-only --cap-drop=ALL \
  --security-opt no-new-privileges -v "$RELEASE_CONTAINER_TMP:/tmp:rw" \
  -v "$RELEASE_ARCHIVE:/input/image.tar:ro" \
  -v "$TRIVY_CACHE_DIR:/root/.cache/trivy:ro" \
  "$RELEASE_SCANNER" image --input /input/image.tar --offline-scan --skip-db-update \
  --skip-java-db-update --format json >"$RELEASE_EVIDENCE"
release_validate_json "$RELEASE_EVIDENCE"
release_write_provenance vulnerability-scan-trivy-json
release_publish_bundle
printf 'Vulnerability evidence bundle: %s\n' "$RELEASE_BUNDLE"
