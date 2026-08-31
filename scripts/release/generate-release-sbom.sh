#!/usr/bin/env bash
# Produce CycloneDX JSON for a local immutable release candidate without
# exposing a Docker socket or allowing scanner network access.
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-supply-chain-lib.sh"

# Verified with `docker buildx imagetools inspect anchore/syft:v1.41.0` on
# 2026-07-13. This is the immutable multi-platform index digest, not a tag.
RELEASE_SCANNER="anchore/syft@sha256:046f0c3b14e4451a1d0cd2f367ce3ba3bd653cbd23a7749556f46592d0281a0d"
RELEASE_IMAGE=""

usage() {
  cat <<'EOF'
usage: scripts/release/generate-release-sbom.sh --image name@sha256:<digest> --output /external/path/sbom-bundle

Candidate and scanner must already exist in local Docker image store. Command never
pulls, publishes, mounts Docker socket, or enables network. It atomically creates the
new <output>/ bundle containing evidence.json and provenance.json. Existing output is rejected.
EOF
}

while (($#)); do
  case "$1" in
    --image) RELEASE_IMAGE=${2:?--image needs a value}; shift 2 ;;
    --output) requested_output=${2:?--output needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

release_require_external_output "${requested_output:-}"
release_require_local_image "$RELEASE_IMAGE"
release_require_scanner "$RELEASE_SCANNER"
release_save_image
release_prepare_evidence_files

docker run --rm --network none --read-only --cap-drop=ALL \
  --security-opt no-new-privileges -e HOME=/tmp -e SYFT_CHECK_FOR_APP_UPDATE=false \
  -v "$RELEASE_CONTAINER_TMP:/tmp:rw" \
  -v "$RELEASE_ARCHIVE:/input/image.tar:ro" \
  "$RELEASE_SCANNER" scan "docker-archive:/input/image.tar" -o cyclonedx-json \
  >"$RELEASE_EVIDENCE"
release_validate_json "$RELEASE_EVIDENCE"
release_write_provenance sbom-cyclonedx-json
release_publish_bundle
printf 'SBOM evidence bundle: %s\n' "$RELEASE_BUNDLE"
