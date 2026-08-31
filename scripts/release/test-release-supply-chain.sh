#!/usr/bin/env bash
# Contract test uses a mock Docker CLI; it never pulls or scans an image.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sbom="$repo/scripts/release/generate-release-sbom.sh"
scan="$repo/scripts/release/scan-release-image.sh"
fixture="$repo/scripts/release/fixtures/mock-docker-release-evidence.sh"
digest="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

fail() { echo "release supply-chain test: $*" >&2; exit 1; }
reject() {
  local script=$1 expected=$2
  shift 2
  local output
  if output=$("$script" "$@" 2>&1); then
    fail "unexpected success: $script $*"
  fi
  [[ "$output" == *"$expected"* ]] || fail "missing rejection '$expected': $output"
}

test -x "$sbom" || fail "SBOM command not executable"
test -x "$scan" || fail "scan command not executable"
test -x "$fixture" || fail "mock Docker fixture not executable"
grep -Fq 'anchore/syft@sha256:046f0c3b14e4451a1d0cd2f367ce3ba3bd653cbd23a7749556f46592d0281a0d' "$sbom" || fail "Syft digest missing"
grep -Fq 'aquasec/trivy@sha256:bcc376de8d77cfe086a917230e818dc9f8528e3c852f7b1aff648949b6258d1c' "$scan" || fail "Trivy digest missing"
! grep -Eq 'docker (pull|login|push)' "$sbom" "$scan" || fail "evidence commands must not pull/login/push"
grep -Fq -- '--network none' "$sbom" || fail "SBOM network must be disabled"
grep -Fq -- '--network none' "$scan" || fail "scan network must be disabled"
! grep -Fq '/var/run/docker.sock' "$sbom" "$scan" || fail "evidence commands must not mount Docker socket"

# Reject before Docker is consulted. This keeps test local/offline.
reject "$sbom" 'image must be exact' --image "registry.example/quasar:1@sha256:$digest" --output /tmp/evidence.json
reject "$scan" 'image must be exact' --image "registry.example/quasar@sha256:abcd" --cache-dir /tmp --output /tmp/evidence.json
reject "$sbom" 'evidence output must be outside repository' --image "registry.example/quasar@sha256:$digest" --output "$repo/sbom.json"
reject "$scan" 'evidence output must be outside repository' --image "registry.example/quasar@sha256:$digest" --cache-dir /tmp --output "$repo/scan.json"

existing_dir="$(mktemp -d /tmp/quasar-release-evidence-existing.XXXXXX)"
mock_dir="$(mktemp -d /tmp/quasar-release-evidence-mock.XXXXXX)"
output_dir="$(mktemp -d /tmp/quasar-release-evidence-output.XXXXXX)"
trap 'rm -rf "$existing_dir" "$mock_dir" "$output_dir"' EXIT
ln -s "$fixture" "$mock_dir/docker"
export PATH="$mock_dir:$PATH"
touch "$existing_dir/existing.json"
reject "$sbom" 'refusing to replace existing evidence' --image "registry.example/quasar@sha256:$digest" --output "$existing_dir/existing.json"
test -f "$existing_dir/existing.json" || fail "existing evidence removed"

candidate="registry.example/quasar@sha256:$digest"
sbom_bundle="$output_dir/sbom"
"$sbom" --image "$candidate" --output "$sbom_bundle" >/dev/null
test -d "$sbom_bundle" || fail "SBOM bundle not published"
test -s "$sbom_bundle/evidence.json" || fail "SBOM evidence missing"
test -s "$sbom_bundle/provenance.json" || fail "SBOM provenance missing"
python3 - "$candidate" "$sbom_bundle/evidence.json" "$sbom_bundle/provenance.json" <<'PY'
import hashlib
import json
import sys
candidate, evidence, provenance = sys.argv[1:]
receipt = json.load(open(provenance))
assert receipt["candidate_image"] == candidate
assert receipt["evidence_sha256"] == hashlib.sha256(open(evidence, "rb").read()).hexdigest()
PY

cache_dir="$output_dir/cache"
mkdir "$cache_dir"
scan_bundle="$output_dir/scan"
"$scan" --image "$candidate" --cache-dir "$cache_dir" --output "$scan_bundle" >/dev/null
test -s "$scan_bundle/evidence.json" || fail "scan evidence missing"
test -s "$scan_bundle/provenance.json" || fail "scan provenance missing"

# Existing directory and dangling symlink must reject before scanner work. No
# stage directory or partial output may survive either collision.
collision="$output_dir/collision"
mkdir "$collision"
touch "$collision/keep"
reject "$sbom" 'refusing to replace existing evidence bundle' --image "$candidate" --output "$collision"
test -f "$collision/keep" || fail "collision target changed"
dangling="$output_dir/dangling"
ln -s /no/such/release-evidence "$dangling"
reject "$scan" 'refusing to replace existing evidence bundle' --image "$candidate" --cache-dir "$cache_dir" --output "$dangling"
test -L "$dangling" || fail "dangling target changed"

failed="$output_dir/failed"
if MOCK_DOCKER_FAIL_RUN=1 "$sbom" --image "$candidate" --output "$failed" >/dev/null 2>&1; then
  fail "mock scanner failure unexpectedly published bundle"
fi
test ! -e "$failed" && test ! -L "$failed" || fail "failed scan published destination"
if find "$output_dir" -maxdepth 1 -name '.*.staging.*' -print -quit | grep -q .; then
  fail "staging directory orphaned"
fi

echo "Release supply-chain contract: PASS"
