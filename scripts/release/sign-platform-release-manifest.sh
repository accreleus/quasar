#!/usr/bin/env bash
# Write the detached signature for a platform release manifest —
# `platform-release-manifest.json.sig`, published beside the manifest as a
# second GitHub Release asset.
#
# Schema and rationale: scripts/release/platform-release-signature.md.
# The verifier is the updater (control-plane/internal/updater/signature.go);
# this script and that file are the two halves of one format.
#
# THE PRIVATE KEY NEVER TOUCHES THE REPOSITORY. It is read from the environment
# ($QUASAR_RELEASE_SIGNING_KEY, a PKCS#8 PEM, from a CI secret) or from a file
# outside the tree, written to a mode-0600 temporary file for openssl, and
# removed on every exit path.
#
# Signing is OPTIONAL: with no key configured, the release job skips this step
# and publishes an unsigned release, exactly as before.
#
# Exit codes: 0 wrote a valid signature · 1 bad input or a key that will not
# sign · 2 usage.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
verifier="$root/scripts/release/verify-platform-release-manifest.sh"

manifest=""
output=""
key_file=""
key_id=""
append=""

usage() {
  cat <<'EOF'
usage: scripts/release/sign-platform-release-manifest.sh \
         --manifest platform-release-manifest.json \
         --output   platform-release-manifest.json.sig \
         --key-id   <label, e.g. quasar-release-2026> \
         [--key-file PATH]   (default: $QUASAR_RELEASE_SIGNING_KEY, a PKCS#8 PEM)
         [--append PATH]     (an existing .sig whose signatures are carried over)

Signs the manifest's exact bytes with an ed25519 key and writes a detached
signature document (format_version 1). The output is verified with the matching
public key before this script exits 0, so an unverifiable signature is never
left behind.

--append is the key-rotation path: it carries the signatures already in PATH
into the new document, so one release can be signed by the outgoing and the
incoming key at once. Entries whose key_id equals --key-id are replaced.

Generate a key with scripts/release/new-release-signing-key.sh. Operator
procedure, including the CI secret to create: docs/upgrading.md.
EOF
}

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?--manifest needs a path}; shift 2 ;;
    --output) output=${2:?--output needs a path}; shift 2 ;;
    --key-file) key_file=${2:?--key-file needs a path}; shift 2 ;;
    --key-id) key_id=${2:?--key-id needs a value}; shift 2 ;;
    --append) append=${2:?--append needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

for required in manifest output key_id; do
  if [[ -z "${!required}" ]]; then
    echo "sign-platform-release-manifest: missing --${required//_/-}" >&2
    usage >&2
    exit 2
  fi
done

if [[ ! -r "$manifest" ]]; then
  echo "sign-platform-release-manifest: cannot read $manifest" >&2
  exit 1
fi
# The label rides in the published document and is matched against an
# operator's configuration, so keep it to something both can spell.
if [[ ! "$key_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]]; then
  echo "sign-platform-release-manifest: --key-id must be 1-64 chars of [A-Za-z0-9._-], got '$key_id'" >&2
  exit 1
fi

work="$(mktemp -d)"
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT

key_pem="$work/key.pem"
( umask 077; : > "$key_pem" )
if [[ -n "$key_file" ]]; then
  [[ -r "$key_file" ]] || { echo "sign-platform-release-manifest: cannot read $key_file" >&2; exit 1; }
  cat "$key_file" > "$key_pem"
elif [[ -n "${QUASAR_RELEASE_SIGNING_KEY:-}" ]]; then
  printf '%s\n' "$QUASAR_RELEASE_SIGNING_KEY" > "$key_pem"
else
  echo "sign-platform-release-manifest: no signing key (set \$QUASAR_RELEASE_SIGNING_KEY or pass --key-file)" >&2
  exit 1
fi

if ! grep -q 'BEGIN PRIVATE KEY' "$key_pem"; then
  echo "sign-platform-release-manifest: the signing key is not a PKCS#8 PEM ('-----BEGIN PRIVATE KEY-----'); see scripts/release/new-release-signing-key.sh" >&2
  exit 1
fi
if [[ "$(openssl pkey -in "$key_pem" -noout -text 2>/dev/null | head -1)" != *ED25519* ]]; then
  echo "sign-platform-release-manifest: the signing key is not an ed25519 key" >&2
  exit 1
fi

# -rawin: ed25519 signs the message itself, never a digest of it. Passing a
# pre-hashed input here would produce a signature the verifier cannot check.
if ! openssl pkeyutl -sign -rawin -inkey "$key_pem" -in "$manifest" -out "$work/sig.bin"; then
  echo "sign-platform-release-manifest: openssl could not sign $manifest" >&2
  exit 1
fi
signature="$(base64 -w0 < "$work/sig.bin" 2>/dev/null || base64 < "$work/sig.bin" | tr -d '\n')"
public_key="$(openssl pkey -in "$key_pem" -pubout -outform DER \
  | tail -c 32 | { base64 -w0 2>/dev/null || base64 | tr -d '\n'; })"

python3 - "$output" "$key_id" "$signature" "$append" <<'PY'
import json
import sys
from pathlib import Path

output, key_id, signature, append = sys.argv[1:5]

entries = []
if append:
    prior = json.loads(Path(append).read_text())
    if prior.get("format_version") != 1:
        raise SystemExit(f"sign-platform-release-manifest: {append} is not a format_version 1 document")
    # Replaced, not duplicated: re-signing with the same label supersedes.
    entries = [e for e in prior.get("signatures", []) if e.get("key_id") != key_id]

entries.append({"algorithm": "ed25519", "key_id": key_id, "signature": signature})

out = Path(output)
out.parent.mkdir(parents=True, exist_ok=True)
out.write_text(json.dumps({"format_version": 1, "signatures": entries}, indent=2) + "\n")
print(f"wrote {out} ({len(entries)} signature(s))")
PY

# Never leave an unverifiable signature behind: the file may exist only if the
# public key half of the key that just signed it verifies it.
if ! "$verifier" --manifest "$manifest" --signature "$output" --public-key "$key_id:$public_key" >/dev/null; then
  echo "sign-platform-release-manifest: the signature just written does not verify; removing $output" >&2
  rm -f "$output"
  exit 1
fi

echo "signed $manifest as $output (key_id $key_id)"
echo "public key (base64, for QUASAR_UPDATER_TRUSTED_KEYS): $key_id:$public_key"
