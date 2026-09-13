#!/usr/bin/env bash
# Offline contract test for the release-signature half: keygen, signing,
# verification, and every shape the verifier must refuse. No docker, no network.
#
# Every key here is generated INTO A TEMPORARY DIRECTORY at run time and removed
# on exit. No key, public or private, is ever committed.
#
# The Go verifier (control-plane/internal/updater/signature_test.go) is the
# normative one; this proves the shell producer writes what it accepts.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
keygen="$repo/scripts/release/new-release-signing-key.sh"
sign="$repo/scripts/release/sign-platform-release-manifest.sh"
verify="$repo/scripts/release/verify-platform-release-manifest.sh"

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

# ── Keygen ────────────────────────────────────────────────────────────────────
"$keygen" --out "$work/a.pem" --key-id quasar-test-a > "$work/a.out"
[[ -s "$work/a.pem" ]] || fail "keygen wrote no key"
[[ "$(stat -c '%a' "$work/a.pem")" == 600 ]] || fail "the private key must be mode 0600"
key_a="$(grep -o 'quasar-test-a:[A-Za-z0-9+/=]*' "$work/a.out" | head -1)"
[[ -n "$key_a" ]] || fail "keygen printed no public key"
grep -q 'gh secret set QUASAR_RELEASE_SIGNING_KEY' "$work/a.out" \
  || fail "keygen must print the CI secret command"

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

echo "PASS release signature contract (keygen, sign, verify, rotation, refusals)"
