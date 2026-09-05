#!/usr/bin/env bash
# Offline contract test for the platform release manifest: the generator, the
# validator, and every shape the validator must refuse. Fixtures only — no
# docker, no network, no registry.
#
# The manifest is what the control plane reads to learn a release exists
# (scripts/release/platform-release-manifest.md), so a silently-drifted field is
# a broken updater on every instance. Every rejection below is a drift the
# validator has to catch before the asset is uploaded.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
generate="$repo/scripts/release/generate-platform-release-manifest.sh"
validate="$repo/scripts/release/validate-platform-release-manifest.sh"
fixtures="$repo/scripts/release/fixtures/manifests"
migrations="$repo/scripts/release/fixtures/migrations"

control_digest="sha256:$(printf 'a%.0s' {1..64})"
agent_digest="sha256:$(printf 'b%.0s' {1..64})"
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

# ── The validator on a known-good manifest ────────────────────────────────────
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

# Every error is reported, not just the first: a maintainer fixing one field at a
# time per CI run is the failure mode this avoids.
two="$("$validate" "$fixtures/two-errors.json" 2>&1 || true)"
[[ "$two" == *"format_version"* && "$two" == *"source_commit"* ]] \
  || fail "validator stopped at the first error: $two"

# ── The generator ─────────────────────────────────────────────────────────────
gen() { # gen <output> <version> [extra args...]
  local output=$1 version=$2
  shift 2
  "$generate" \
    --version "$version" \
    --source-commit "$commit" \
    --built-at 2026-09-04T12:00:00Z \
    --control-digest "$control_digest" \
    --agent-digest "$agent_digest" \
    --registry-ns "$ns" \
    --migrations-dir "$migrations" \
    --output "$output" "$@"
}

gen "$work/pre.json" 0.2.0-rc.1 >/dev/null || fail "generator failed on a prerelease version"
"$validate" "$work/pre.json" --expect-version 0.2.0-rc.1 \
  --expect-control-digest "$control_digest" --expect-agent-digest "$agent_digest" >/dev/null \
  || fail "generated prerelease manifest failed validation"

# The documented shape, field for field and in key order: the fixture is what
# the schema doc shows. schema_version is compared separately because the fixture
# migrations dir tops out at 0012 while the doc's example says 74.
python3 - "$fixtures/valid.json" "$work/pre.json" <<'PY'
import json
import sys

want, got = (json.load(open(p)) for p in sys.argv[1:])
want.pop("schema_version")
assert got.pop("schema_version") == 12, "schema_version must be max NNNN over the *.up.sql files"
assert list(want) == list(got), f"key order drifted: {list(got)}"
assert want == got, f"generated manifest differs from the documented shape: {got}"
PY
grep -q '^  "format_version": 1,$' "$work/pre.json" || fail "generated manifest is not 2-space indented"
[[ "$(tail -c 1 "$work/pre.json" | xxd -p)" == "0a" ]] || fail "generated manifest has no trailing newline"

# prerelease is derived from the version, never asserted by the caller.
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["prerelease"] is True, d' "$work/pre.json"
gen "$work/stable.json" 1.4.0 >/dev/null || fail "generator failed on a stable version"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["prerelease"] is False, d' "$work/stable.json"

# The generator validates its own output before exiting 0, so a bad input can
# never leave a published-looking file behind.
reject "$generate" "digest" --version 0.2.0 --source-commit "$commit" \
  --built-at 2026-09-04T12:00:00Z --control-digest sha256:abcd --agent-digest "$agent_digest" \
  --registry-ns "$ns" --migrations-dir "$migrations" --output "$work/bad-digest.json"
test ! -e "$work/bad-digest.json" || fail "generator wrote a manifest for a bad digest"

reject "$generate" "source-commit" --version 0.2.0 --source-commit deadbeef \
  --built-at 2026-09-04T12:00:00Z --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --registry-ns "$ns" --migrations-dir "$migrations" --output "$work/bad-commit.json"
reject "$generate" "version" --version v0.2.0 --source-commit "$commit" \
  --built-at 2026-09-04T12:00:00Z --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --registry-ns "$ns" --migrations-dir "$migrations" --output "$work/bad-version.json"
reject "$generate" "built-at" --version 0.2.0 --source-commit "$commit" \
  --built-at "2026-09-04T12:00:00+00:00" --control-digest "$control_digest" \
  --agent-digest "$agent_digest" --registry-ns "$ns" --migrations-dir "$migrations" \
  --output "$work/bad-built-at.json"
reject "$generate" "built-at" --version 0.2.0 --source-commit "$commit" \
  --built-at "2026-09-04 12:00:00Z" --control-digest "$control_digest" \
  --agent-digest "$agent_digest" --registry-ns "$ns" --migrations-dir "$migrations" \
  --output "$work/bad-built-at2.json"
reject "$generate" "registry-ns" --version 0.2.0 --source-commit "$commit" \
  --built-at 2026-09-04T12:00:00Z --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --registry-ns "ghcr.io/accreleus/quasar:latest" --migrations-dir "$migrations" \
  --output "$work/bad-ns.json"
reject "$generate" "migrations" --version 0.2.0 --source-commit "$commit" \
  --built-at 2026-09-04T12:00:00Z --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --registry-ns "$ns" --migrations-dir "$work/nowhere" --output "$work/no-migrations.json"
mkdir -p "$work/empty-migrations"
reject "$generate" "no migrations" --version 0.2.0 --source-commit "$commit" \
  --built-at 2026-09-04T12:00:00Z --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --registry-ns "$ns" --migrations-dir "$work/empty-migrations" --output "$work/empty.json"

# Defaulted --migrations-dir reads the real control plane, which is the number a
# real release carries. It must be a positive integer and match the tree.
gen_default="$work/default-migrations.json"
"$generate" --version 0.2.0 --source-commit "$commit" --built-at 2026-09-04T12:00:00Z \
  --control-digest "$control_digest" --agent-digest "$agent_digest" \
  --registry-ns "$ns" --output "$gen_default" >/dev/null || fail "generator failed with the default migrations dir"
expected_schema="$(find "$repo/control-plane/migrations" -name '*.up.sql' -printf '%f\n' \
  | cut -d_ -f1 | sort -n | tail -1 | sed 's/^0*//')"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["schema_version"] == int(sys.argv[2]), (d["schema_version"], sys.argv[2])' \
  "$gen_default" "$expected_schema"

# Usage errors are exit 2, distinct from the exit-1 "the inputs are wrong".
"$generate" --nonsense >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "generator unknown flag must exit 2"
"$validate" >/dev/null 2>&1 || [[ $? -eq 2 ]] || fail "validator with no path must exit 2"

echo "Platform release manifest contract: PASS"
