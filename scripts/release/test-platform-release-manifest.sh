#!/usr/bin/env bash
# Offline contract test for the platform release manifest: the generator (format
# 2 only), the validator (formats 1 and 2), and every shape the validator must
# refuse. Fixtures only — no docker, no network, no registry.
#
# The manifest is what the control plane reads to learn a release exists
# (scripts/release/platform-release-manifest.md), so a silently-drifted field is
# a broken update path on every instance. Every rejection below is a drift the
# validator has to catch before the asset is uploaded.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
generate="$repo/scripts/release/generate-platform-release-manifest.sh"
validate="$repo/scripts/release/validate-platform-release-manifest.sh"
fixtures="$repo/scripts/release/fixtures/manifests"
migrations="$repo/scripts/release/fixtures/migrations"

control_digest="sha256:$(printf 'a%.0s' {1..64})"
agent_digest="sha256:$(printf 'b%.0s' {1..64})"
recovery_digest="sha256:$(printf 'd%.0s' {1..64})"
commit="$(printf 'c%.0s' {1..40})"
ns="ghcr.io/accreleus/quasar"

work="$(mktemp -d /tmp/quasar-platform-manifest-test.XXXXXX)"
trap 'rm -rf "$work"' EXIT

fail() { echo "platform release manifest test: $*" >&2; exit 1; }

reject() { # reject <script> <expected fragment> <args...>
  local script=$1 expected=$2
  shift 2
  local output
  if output=$("$script" "$@" 2>&1); then
    fail "unexpected success: $(basename "$script") $*"
  fi
  [[ "$output" == *"$expected"* ]] || fail "missing rejection '$expected': $output"
}

test -x "$generate" || fail "generator not executable"
test -x "$validate" || fail "validator not executable"
"$generate" --help >/dev/null || fail "generator --help must exit 0"
"$validate" --help >/dev/null || fail "validator --help must exit 0"

# ── Format 1: the validator on a known-good manifest ─────────────────────────
out="$("$validate" "$fixtures/valid.json")"
[[ "$out" == *PASS* ]] || fail "valid manifest did not report PASS: $out"
"$validate" "$fixtures/valid-stable.json" >/dev/null || fail "valid stable manifest rejected"

# The --expect-* flags are how the workflow ties the manifest to the digests the
# build jobs produced; they must assert equality, not merely presence.
"$validate" "$fixtures/valid.json" \
  --expect-version 0.2.0-rc.1 \
  --expect-control-digest "$control_digest" \
  --expect-agent-digest "$agent_digest" >/dev/null || fail "matching --expect-* rejected"
reject "$validate" "expected version '0.2.0'" "$fixtures/valid.json" --expect-version 0.2.0
reject "$validate" "expected control-plane digest" "$fixtures/valid.json" \
  --expect-control-digest "$agent_digest"
reject "$validate" "expected node-agent digest" "$fixtures/valid.json" \
  --expect-agent-digest "$control_digest"
"$validate" "$fixtures/valid.json" --expect-format 1 >/dev/null || fail "format 1 rejected --expect-format 1"
reject "$validate" "expected format_version 2" "$fixtures/valid.json" --expect-format 2
reject "$validate" "expected recovery-actor digest" "$fixtures/valid.json" \
  --expect-recovery-digest "$recovery_digest"

# ── Every refusal, one fixture each ───────────────────────────────────────────
reject "$validate" "format_version" "$fixtures/bad-format-version.json"
reject "$validate" "version" "$fixtures/bad-version.json"
reject "$validate" "build metadata" "$fixtures/build-metadata-version.json"
reject "$validate" "prerelease" "$fixtures/prerelease-mismatch.json"
reject "$validate" "source_commit" "$fixtures/bad-source-commit.json"
reject "$validate" "built_at" "$fixtures/bad-built-at.json"
reject "$validate" "schema_version" "$fixtures/bad-schema-version.json"
reject "$validate" "schema_version" "$fixtures/string-schema-version.json"
reject "$validate" "unknown top-level key: channel" "$fixtures/unknown-top-key.json"
reject "$validate" "missing top-level key: schema_version" "$fixtures/missing-key.json"
reject "$validate" "components[0].name" "$fixtures/component-order.json"
reject "$validate" "components[1].name" "$fixtures/component-name.json"
reject "$validate" "unknown key: role" "$fixtures/component-extra-key.json"
reject "$validate" "missing key: digest" "$fixtures/component-missing-key.json"
reject "$validate" "must carry no tag" "$fixtures/tagged-image.json"
reject "$validate" "must carry no tag" "$fixtures/digest-in-image.json"
reject "$validate" "quasar-node-agent" "$fixtures/wrong-image-name.json"
reject "$validate" "digest" "$fixtures/bad-digest.json"
reject "$validate" "digest" "$fixtures/uppercase-digest.json"
reject "$validate" "exactly 2 components" "$fixtures/extra-component.json"
reject "$validate" "exactly 2 components" "$fixtures/one-component.json"
reject "$validate" "not valid JSON" "$fixtures/not-json.json"
reject "$validate" "JSON object" "$fixtures/not-an-object.json"
reject "$validate" "no such manifest" "$work/absent.json"

# ── Format 2 ──────────────────────────────────────────────────────────────────
# valid-v2.json is a copy of the fixture the Go tests read.
shared="$repo/testdata/release/platform-release-manifest.v2.json"
cmp -s "$shared" "$fixtures/valid-v2.json" || fail "valid-v2.json differs from $shared"
out="$("$validate" "$shared" --expect-format 2)"
[[ "$out" == *PASS* ]] || fail "valid format-2 manifest did not report PASS: $out"
"$validate" "$fixtures/valid-v2.json" \
  --expect-format 2 \
  --expect-version 0.4.0 \
  --expect-control-digest "$control_digest" \
  --expect-agent-digest "$agent_digest" \
  --expect-recovery-digest "$recovery_digest" >/dev/null || fail "matching format-2 --expect-* rejected"
reject "$validate" "expected format_version 1" "$fixtures/valid-v2.json" --expect-format 1
reject "$validate" "expected recovery-actor digest" "$fixtures/valid-v2.json" \
  --expect-recovery-digest "$agent_digest"
reject "$validate" "exactly 3 components" "$fixtures/v2-two-components.json"
reject "$validate" "components[1].name must be 'node-agent'" "$fixtures/v2-component-order.json"
reject "$validate" "components[2].image must name 'quasar-recovery'" "$fixtures/v2-wrong-recovery-image.json"
reject "$validate" "missing top-level key: floor" "$fixtures/v2-floor-missing.json"
reject "$validate" "floor must hold exactly 2 entries" "$fixtures/v2-floor-one-entry.json"
reject "$validate" "floor must be an array" "$fixtures/v2-floor-object.json"
reject "$validate" "floor[0].name must be 'node-agent'" "$fixtures/v2-floor-order.json"
reject "$validate" "floor[0].version must be strict semver" "$fixtures/v2-floor-leading-v.json"
reject "$validate" "floor[1].version must not carry semver build metadata" \
  "$fixtures/v2-floor-build-metadata.json"
reject "$validate" "floor[1] missing key: version" "$fixtures/v2-floor-missing-version.json"
reject "$validate" "floor[0] unknown key: since" "$fixtures/v2-floor-unknown-key.json"
reject "$validate" "floor[0].version '0.4.1' orders above the release version" \
  "$fixtures/v2-floor-above-version.json"
# SemVer precedence, not string order: rc.1.1 is above rc.1.
reject "$validate" "floor[1].version '0.4.0-rc.1.1' orders above" \
  "$fixtures/v2-floor-above-prerelease.json"
reject "$validate" "unknown top-level key: channel" "$fixtures/v2-unknown-top-key.json"
# A floor key on a format-1 manifest is unknown, not ignored.
python3 - "$fixtures/valid.json" "$work/v1-with-floor.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["floor"] = []
open(sys.argv[2], "w").write(json.dumps(d))
PY
reject "$validate" "unknown top-level key: floor" "$work/v1-with-floor.json"
reject "$validate" "must be 1 or 2" "$fixtures/valid-v2.json" --expect-format 3

# Every error is reported, not just the first: a maintainer fixing one field at a
# time per CI run is the failure mode this avoids.
two="$("$validate" "$fixtures/two-errors.json" 2>&1 || true)"
[[ "$two" == *"format_version"* && "$two" == *"source_commit"* ]] \
  || fail "validator stopped at the first error: $two"

# ── The generator ─────────────────────────────────────────────────────────────
floor_fixture() { # floor_fixture <path> <node-agent floor> <recovery-actor floor>
  printf 'package buildinfo\n\nconst FloorNodeAgent = "%s"\nconst FloorRecoveryActor = "%s"\n' \
    "$2" "$3" > "$1"
}
floor_fixture "$work/floor.go" 0.4.0-0 0.4.0-0

gen_base=(
  --source-commit "$commit"
  --built-at 2026-10-01T12:00:00Z
  --control-digest "$control_digest"
  --agent-digest "$agent_digest"
  --recovery-digest "$recovery_digest"
  --registry-ns "$ns"
  --migrations-dir "$migrations"
  --floor-source "$work/floor.go"
)
gen() { # gen <output> <version> [overrides...]; a later flag overrides gen_base
  local output=$1 version=$2
  shift 2
  "$generate" "${gen_base[@]}" --version "$version" --output "$output" "$@"
}

gen "$work/doc.json" 0.4.0 >/dev/null || fail "generator failed on the documented inputs"
"$validate" "$work/doc.json" --expect-format 2 --expect-version 0.4.0 \
  --expect-control-digest "$control_digest" --expect-agent-digest "$agent_digest" \
  --expect-recovery-digest "$recovery_digest" >/dev/null \
  || fail "generated manifest failed validation"

# The documented shape, field for field and in key order at every level.
# schema_version is compared separately: the fixture migrations top out at 0012.
python3 - "$fixtures/valid-v2.json" "$work/doc.json" <<'PY'
import json
import sys

want, got = (json.load(open(p)) for p in sys.argv[1:])
want.pop("schema_version")
assert got.pop("schema_version") == 12, "schema_version must be max NNNN over the *.up.sql files"
assert json.dumps(want) == json.dumps(got), f"generated manifest differs from the documented shape: {got}"
PY
grep -q '^  "format_version": 2,$' "$work/doc.json" || fail "generated manifest is not 2-space indented format 2"
[[ "$(tail -c 1 "$work/doc.json" | od -An -tx1 | tr -d ' ')" == "0a" ]] || fail "generated manifest has no trailing newline"

# prerelease is derived from the version, never asserted by the caller.
gen "$work/pre.json" 0.4.0-rc.1 >/dev/null || fail "generator failed on a prerelease version"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["prerelease"] is True, d' "$work/pre.json"
gen "$work/stable.json" 1.4.0 >/dev/null || fail "generator failed on a stable version"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["prerelease"] is False, d' "$work/stable.json"

# The generator validates its own output before exiting 0, so a bad input can
# never leave a published-looking file behind.
reject "$generate" "digest" "${gen_base[@]}" --version 0.4.0 --control-digest sha256:abcd \
  --output "$work/bad-digest.json"
test ! -e "$work/bad-digest.json" || fail "generator wrote a manifest for a bad digest"
reject "$generate" "--recovery-digest" "${gen_base[@]}" --version 0.4.0 \
  --recovery-digest "sha256:$(printf 'D%.0s' {1..64})" --output "$work/bad-recovery.json"
test ! -e "$work/bad-recovery.json" || fail "generator wrote a manifest for a bad recovery digest"

reject "$generate" "source-commit" "${gen_base[@]}" --version 0.4.0 --source-commit deadbeef \
  --output "$work/bad-commit.json"
reject "$generate" "version" "${gen_base[@]}" --version v0.4.0 --output "$work/bad-version.json"
reject "$generate" "built-at" "${gen_base[@]}" --version 0.4.0 \
  --built-at "2026-09-04T12:00:00+00:00" --output "$work/bad-built-at.json"
reject "$generate" "built-at" "${gen_base[@]}" --version 0.4.0 \
  --built-at "2026-09-04 12:00:00Z" --output "$work/bad-built-at2.json"
reject "$generate" "registry-ns" "${gen_base[@]}" --version 0.4.0 \
  --registry-ns "ghcr.io/accreleus/quasar:latest" --output "$work/bad-ns.json"
reject "$generate" "migrations" "${gen_base[@]}" --version 0.4.0 \
  --migrations-dir "$work/nowhere" --output "$work/no-migrations.json"
mkdir -p "$work/empty-migrations"
reject "$generate" "no migrations" "${gen_base[@]}" --version 0.4.0 \
  --migrations-dir "$work/empty-migrations" --output "$work/empty.json"

# The floor source: exactly one well-formed line per constant; a floor may equal
# the version but not order above it.
reject "$generate" "--floor-source is not a file" "${gen_base[@]}" --version 0.4.0 \
  --floor-source "$work/nowhere.go" --output "$work/no-floor.json"
printf 'package buildinfo\n\nconst FloorNodeAgent = "0.3.0"\n' > "$work/floor-one.go"
reject "$generate" "exactly one line 'const FloorRecoveryActor" "${gen_base[@]}" --version 0.4.0 \
  --floor-source "$work/floor-one.go" --output "$work/floor-one.json"
test ! -e "$work/floor-one.json" || fail "generator wrote a manifest with a missing floor line"
printf 'const FloorNodeAgent = "0.3.0"\nconst FloorNodeAgent = "0.3.1"\nconst FloorRecoveryActor = "0.3.0"\n' \
  > "$work/floor-twice.go"
reject "$generate" "(found 2)" "${gen_base[@]}" --version 0.4.0 \
  --floor-source "$work/floor-twice.go" --output "$work/floor-twice.json"
floor_fixture "$work/floor-v.go" v0.3.0 0.3.0
reject "$generate" "FloorNodeAgent must be strict semver" "${gen_base[@]}" --version 0.4.0 \
  --floor-source "$work/floor-v.go" --output "$work/floor-v.json"
floor_fixture "$work/floor-build.go" 0.3.0 0.3.0+b1
reject "$generate" "FloorRecoveryActor must be strict semver" "${gen_base[@]}" --version 0.4.0 \
  --floor-source "$work/floor-build.go" --output "$work/floor-build.json"
floor_fixture "$work/floor-above.go" 0.4.1 0.3.0
reject "$generate" "orders above the release version" "${gen_base[@]}" --version 0.4.0 \
  --floor-source "$work/floor-above.go" --output "$work/floor-above.json"
test ! -e "$work/floor-above.json" || fail "generator left a manifest whose floor is above its version"
floor_fixture "$work/floor-equal.go" 0.4.0-rc.1 0.4.0-rc.1
gen "$work/floor-equal.json" 0.4.0-rc.1 --floor-source "$work/floor-equal.go" >/dev/null \
  || fail "a floor equal to the version was refused"

# The defaulted --migrations-dir and --floor-source read the real control plane.
gen_default="$work/default-sources.json"
"$generate" --version 9.9.9 --source-commit "$commit" --built-at 2026-09-04T12:00:00Z \
  --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --recovery-digest "$recovery_digest" --registry-ns "$ns" --output "$gen_default" >/dev/null \
  || fail "generator failed with the default migrations dir and floor source"
expected_schema="$(find "$repo/control-plane/migrations" -name '*.up.sql' -printf '%f\n' \
  | cut -d_ -f1 | sort -n | tail -1 | sed 's/^0*//')"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["schema_version"] == int(sys.argv[2]), (d["schema_version"], sys.argv[2])' \
  "$gen_default" "$expected_schema"
floor_go="$repo/control-plane/internal/buildinfo/floor.go"
want_agent_floor="$(sed -n -E 's/^const FloorNodeAgent = "([^"]*)"$/\1/p' "$floor_go")"
want_actor_floor="$(sed -n -E 's/^const FloorRecoveryActor = "([^"]*)"$/\1/p' "$floor_go")"
[[ -n "$want_agent_floor" && -n "$want_actor_floor" ]] || fail "floor.go no longer has the two floor lines"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["floor"] == [{"name": "node-agent", "version": sys.argv[2]}, {"name": "recovery-actor", "version": sys.argv[3]}], d["floor"]' \
  "$gen_default" "$want_agent_floor" "$want_actor_floor"

# A missing --recovery-digest is a usage error, like every other required flag.
missing_rc=0
"$generate" --version 0.4.0 --source-commit "$commit" --built-at 2026-09-04T12:00:00Z \
  --control-digest "$control_digest" --agent-digest "$agent_digest" --registry-ns "$ns" \
  --output "$work/no-recovery.json" >/dev/null 2>&1 || missing_rc=$?
[[ $missing_rc -eq 2 ]] || fail "generator without --recovery-digest must exit 2, got $missing_rc"

# Usage errors are exit 2, distinct from the exit-1 "the inputs are wrong".
"$generate" --nonsense >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "generator unknown flag must exit 2"
"$validate" >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "validator with no path must exit 2"

echo "Platform release manifest contract: PASS"
