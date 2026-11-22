#!/usr/bin/env bash
# Offline contract test for the release-signature half: keygen, signing,
# verification, and every shape the verifier must refuse. No docker, no network.
#
# Every key here is generated INTO A TEMPORARY DIRECTORY at run time and removed
# on exit. No key, public or private, is ever committed.
#
# The recovery actor's verifier (node-agent/crates/quasar-recovery/src/trust/signature.rs, pinned by
# testdata/recovery/trust-vectors) is the normative one; this proves the shell
# producer writes what the shell verifier accepts.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
keygen="$repo/scripts/release/new-release-signing-key.sh"
sign="$repo/scripts/release/sign-platform-release-manifest.sh"
verify="$repo/scripts/release/verify-platform-release-manifest.sh"
orgkeys="$repo/scripts/release/org-release-keys.sh"

work="$(mktemp -d /tmp/quasar-release-signature-test.XXXXXX)"
trap 'rm -rf "$work"' EXIT

fail() { echo "release signature test: $*" >&2; exit 1; }

reject() { # reject <expected fragment> <cmd...>
  local expected=$1
  shift
  local output
  if output=$("$@" 2>&1); then
    fail "unexpected success: $* ($output)"
  fi
  [[ "$output" == *"$expected"* ]] || fail "missing rejection '$expected': $output"
}

for script in "$keygen" "$sign" "$verify"; do
  test -x "$script" || fail "$(basename "$script") not executable"
  "$script" --help >/dev/null || fail "$(basename "$script") --help must exit 0"
done
test -x "$orgkeys" || fail "org-release-keys.sh not executable"

# ── Keygen ────────────────────────────────────────────────────────────────────
"$keygen" --out "$work/a.pem" --key-id quasar-test-a > "$work/a.out"
[[ -s "$work/a.pem" ]] || fail "keygen wrote no key"
[[ "$(stat -c '%a' "$work/a.pem")" == 600 ]] || fail "the private key must be mode 0600"
key_a="$(grep -o 'quasar-test-a:[A-Za-z0-9+/=]*' "$work/a.out" | head -1)"
[[ -n "$key_a" ]] || fail "keygen printed no public key"
grep -q 'gh secret set QUASAR_RELEASE_SIGNING_KEY --env release' "$work/a.out" \
  || fail "keygen must print the CI secret command, scoped to the release environment"

# A key inside the repository is a key that can be committed.
reject "inside the repository" "$keygen" --out "$repo/scripts/release/leaked.pem" --key-id x
test ! -e "$repo/scripts/release/leaked.pem" || fail "keygen wrote inside the repo"
reject "refusing to overwrite" "$keygen" --out "$work/a.pem" --key-id quasar-test-a

"$keygen" --out "$work/b.pem" --key-id quasar-test-b > "$work/b.out"
key_b="$(grep -o 'quasar-test-b:[A-Za-z0-9+/=]*' "$work/b.out" | head -1)"

# ── Signing and verification ──────────────────────────────────────────────────
manifest="$work/platform-release-manifest.json"
cat > "$manifest" <<'JSON'
{
  "format_version": 1,
  "version": "0.9.0",
  "prerelease": false,
  "source_commit": "cccccccccccccccccccccccccccccccccccccccc",
  "built_at": "2026-09-08T00:00:00Z",
  "schema_version": 80,
  "components": [
    { "name": "control-plane",
      "image": "ghcr.io/accreleus/quasar/quasar-control-plane",
      "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
    { "name": "node-agent",
      "image": "ghcr.io/accreleus/quasar/quasar-node-agent",
      "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }
  ]
}
JSON

sig="$manifest.sig"
"$sign" --manifest "$manifest" --output "$sig" --key-id quasar-test-a --key-file "$work/a.pem" >/dev/null
[[ -s "$sig" ]] || fail "signing wrote no document"
# The Go verifier decodes with DisallowUnknownFields, so an extra key here is a
# document every host would refuse. Key sets, not just values.
python3 - "$sig" <<'PY' || fail "the signature document is not the documented shape"
import json
import sys

doc = json.load(open(sys.argv[1]))
assert set(doc) == {"format_version", "signatures"}, doc
assert doc["format_version"] == 1, doc
assert len(doc["signatures"]) == 1, doc
for entry in doc["signatures"]:
    assert set(entry) == {"algorithm", "key_id", "signature"}, entry
    assert entry["algorithm"] == "ed25519", entry
PY

out="$("$verify" --manifest "$manifest" --signature "$sig" --public-key "$key_a")"
[[ "$out" == *PASS* ]] || fail "a genuine signature did not verify: $out"

# The key comes from the environment when no --key-file is given: that is how
# the release job passes a CI secret.
QUASAR_RELEASE_SIGNING_KEY="$(cat "$work/a.pem")" \
  "$sign" --manifest "$manifest" --output "$work/env.sig" --key-id quasar-test-a >/dev/null
"$verify" --manifest "$manifest" --signature "$work/env.sig" --public-key "$key_a" >/dev/null \
  || fail "a signature made from \$QUASAR_RELEASE_SIGNING_KEY did not verify"

# ── The three refusals that matter ────────────────────────────────────────────
# Tampered manifest: one byte of a digest.
sed 's/sha256:bbbb/sha256:cbbb/' "$manifest" > "$work/tampered.json"
reject "no signature in the document was made by a given key" \
  "$verify" --manifest "$work/tampered.json" --signature "$sig" --public-key "$key_a"

# Wrong key: a real, well-formed key that did not sign this.
reject "no signature in the document was made by a given key" \
  "$verify" --manifest "$manifest" --signature "$sig" --public-key "$key_b"

# The label buys nothing: relabelling the document as the trusted key must not
# make an untrusted signature verify.
"$sign" --manifest "$manifest" --output "$work/mislabelled.sig" \
  --key-id quasar-test-a --key-file "$work/b.pem" >/dev/null
reject "no signature in the document was made by a given key" \
  "$verify" --manifest "$manifest" --signature "$work/mislabelled.sig" --public-key "$key_a"

# ── Rotation: one release signed by both keys ─────────────────────────────────
"$sign" --manifest "$manifest" --output "$work/both.sig" \
  --key-id quasar-test-b --key-file "$work/b.pem" --append "$sig" >/dev/null
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert len(d["signatures"])==2, d' "$work/both.sig" \
  || fail "--append must carry the prior signature over"
for key in "$key_a" "$key_b"; do
  "$verify" --manifest "$manifest" --signature "$work/both.sig" --public-key "$key" >/dev/null \
    || fail "a dual-signed release must verify under $key"
done
# Re-signing with the same label supersedes rather than duplicating.
"$sign" --manifest "$manifest" --output "$work/resigned.sig" \
  --key-id quasar-test-b --key-file "$work/b.pem" --append "$work/both.sig" >/dev/null
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert len(d["signatures"])==2, d' "$work/resigned.sig" \
  || fail "re-signing with the same key_id must replace, not append"

# ── Malformed inputs ──────────────────────────────────────────────────────────
echo '{"format_version":2,"signatures":[]}' > "$work/future.sig"
reject "is not 1" "$verify" --manifest "$manifest" --signature "$work/future.sig" --public-key "$key_a"
echo '{"format_version":1,"signatures":[]}' > "$work/empty.sig"
reject "carries no signatures" "$verify" --manifest "$manifest" --signature "$work/empty.sig" --public-key "$key_a"
echo 'not json' > "$work/garbage.sig"
reject "not JSON" "$verify" --manifest "$manifest" --signature "$work/garbage.sig" --public-key "$key_a"
reject "not base64" "$verify" --manifest "$manifest" --signature "$sig" --public-key "k:!!!!"
reject "not the 32" "$verify" --manifest "$manifest" --signature "$sig" --public-key "k:$(printf 'short' | base64)"
reject "cannot read manifest" "$verify" --manifest "$work/absent.json" --signature "$sig" --public-key "$key_a"

# A key that is not a signing key at all, and no key at all.
echo "not a pem" > "$work/bogus.pem"
reject "not a PKCS#8 PEM" "$sign" --manifest "$manifest" --output "$work/x.sig" \
  --key-id k --key-file "$work/bogus.pem"
reject "no signing key" env -u QUASAR_RELEASE_SIGNING_KEY \
  "$sign" --manifest "$manifest" --output "$work/x.sig" --key-id k
test ! -e "$work/x.sig" || fail "a failed signing must leave no document behind"

# ── The org key the release job pins ─────────────────────────────────────────
# It must find the real constant, or every tag release fails at its signature check.
org_keys="$("$orgkeys")" || fail "org-release-keys.sh found no key in signature.rs"
[[ "$org_keys" == quasar-release-*:* ]] || fail "unexpected org key: $org_keys"
# A rotation's two-key literal, on the line after the declaration, gives one line per key.
printf 'pub(crate) const DEFAULT_TRUSTED_KEYS: &str =\n    "%s, %s";\n' "$key_a" "$key_b" > "$work/two.rs"
[[ "$("$orgkeys" "$work/two.rs")" == "$key_a"$'\n'"$key_b" ]] || fail "a two-key default must print both keys"
printf 'pub(crate) const DEFAULT_TRUSTED_KEYS: &str = "%s,k:short";\n' "$key_a" > "$work/bad.rs"
reject "not a key-id:base64 list" "$orgkeys" "$work/bad.rs"
# The job's check: a release signed by a key the actor does not compile in fails.
mapfile -t org_args < <(printf -- '--public-key\n%s\n' $org_keys)
reject "no signature in the document was made by a given key" \
  "$verify" --manifest "$manifest" --signature "$sig" "${org_args[@]}"

# ── The release job's signing step, run as the workflow has it ───────────────
step="$work/sign-step.sh"
python3 - "$repo/.github/workflows/images.yml" > "$step" <<'PY'
import sys
lines = open(sys.argv[1]).read().splitlines()
start = next(i for i, l in enumerate(lines) if l.strip() == "- name: Sign the platform release manifest")
run = next(i for i in range(start, len(lines)) if lines[i].strip() == "run: |")
indent = len(lines[run + 1]) - len(lines[run + 1].lstrip())
for l in lines[run + 1:]:
    if l.strip() and len(l) - len(l.lstrip()) < indent:
        break
    print(l[indent:])
PY
grep -q 'org-release-keys.sh' "$step" || fail "could not extract the signing step from images.yml"
run_step() { # run_step <repo> [key pem file]; prints the step's output, returns its status
  local pem="" out="$work/gh-output"
  [[ -n "${2:-}" ]] && pem="$(cat "$2")"
  : > "$out"
  (cd "$repo" && env -i PATH="$PATH" HOME="$HOME" RUNNER_TEMP="$work" GITHUB_OUTPUT="$out" \
    GITHUB_STEP_SUMMARY="$work/summary" MANIFEST="$work/step-manifest.json" REPO="$1" \
    KEY_ID=quasar-test-a QUASAR_RELEASE_SIGNING_KEY="$pem" bash -euo pipefail "$step" 2>&1)
}
cp "$manifest" "$work/step-manifest.json"
# An org tag release with no key fails; a fork's publishes unsigned with a warning.
reject "no QUASAR_RELEASE_SIGNING_KEY secret in the release environment" run_step accreleus/quasar
out="$(run_step someone/fork)" || fail "a fork's release with no key must still publish: $out"
[[ "$out" == *"::warning::"* ]] && grep -qx 'path=' "$work/gh-output" \
  || fail "a fork with no key must warn and output no signature"
# An org release signed by a key the actor does not compile in fails the pin; a fork's passes.
reject "does not verify under DEFAULT_TRUSTED_KEYS" run_step accreleus/quasar "$work/a.pem"
out="$(run_step someone/fork "$work/a.pem")" || fail "a fork signing with its own key must publish: $out"
grep -qx "path=$work/step-manifest.json.sig" "$work/gh-output" || fail "a signed fork release must output its .sig"

echo "PASS release signature contract (keygen, sign, verify, rotation, refusals)"
