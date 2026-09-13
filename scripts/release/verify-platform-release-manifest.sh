#!/usr/bin/env bash
# Verify a detached platform-release-manifest signature against one or more
# trusted public keys — the shell twin of the updater's verifier
# (control-plane/internal/updater/signature.go `VerifyManifestSignature`), for
# the release job's self-check and for an operator checking a release by hand.
#
# THE NORMATIVE VERIFIER IS THE UPDATER'S. This script must agree with it: any
# signature by any trusted key verifies the document, and the key_id is a label,
# never what makes a signature good.
#
# openssl, not a python crypto library: openssl is already required to sign, and
# a verifier that needs a pip install is one an operator cannot run.
#
# Schema: scripts/release/platform-release-signature.md
# Exit codes: 0 verified (prints PASS and the key) · 1 did not verify · 2 usage.
set -euo pipefail

manifest=""
signature=""
keys=()

usage() {
  cat <<'EOF'
usage: scripts/release/verify-platform-release-manifest.sh \
         --manifest  platform-release-manifest.json \
         --signature platform-release-manifest.json.sig \
         --public-key [key-id:]<base64 32-byte ed25519 public key>  (repeatable)

Verifies the detached signature over the manifest's exact bytes. Exits 0 and
prints the verifying key's label when ANY signature in the document is made by
ANY of the given keys; exits 1 otherwise.

--public-key takes exactly what QUASAR_UPDATER_TRUSTED_KEYS takes, so an
operator can paste the same string into both.
EOF
}

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?--manifest needs a path}; shift 2 ;;
    --signature) signature=${2:?--signature needs a path}; shift 2 ;;
    --public-key) keys+=("${2:?--public-key needs a value}"); shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

if [[ -z "$manifest" || -z "$signature" || ${#keys[@]} -eq 0 ]]; then
  echo "verify-platform-release-manifest: --manifest, --signature and at least one --public-key are required" >&2
  usage >&2
  exit 2
fi
fail() { echo "verify-platform-release-manifest: $*" >&2; exit 1; }

[[ -r "$manifest" ]] || fail "cannot read manifest $manifest"
[[ -r "$signature" ]] || fail "cannot read signature document $signature"

work="$(mktemp -d)"
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT

b64d() { # base64 -d, refusing anything that is not valid base64
  python3 -c 'import base64,sys; sys.stdout.buffer.write(base64.b64decode(sys.argv[1], validate=True))' "$1"
}

# One `key_id<TAB>base64signature` line per usable ed25519 entry. A document
# that is not the documented shape produces none, and the loop below fails.
entries="$work/entries.tsv"
python3 - "$signature" > "$entries" <<'PY'
import json
import sys
from pathlib import Path

try:
    doc = json.loads(Path(sys.argv[1]).read_bytes())
except ValueError as exc:
    raise SystemExit(f"the signature document is not JSON: {exc}")
if doc.get("format_version") != 1:
    raise SystemExit(f"signature document format_version {doc.get('format_version')!r} is not 1")
entries = doc.get("signatures")
if not isinstance(entries, list) or not entries:
    raise SystemExit("the document carries no signatures")
for entry in entries:
    if not isinstance(entry, dict) or entry.get("algorithm") != "ed25519":
        continue
    key_id = str(entry.get("key_id", ""))
    signature = str(entry.get("signature", ""))
    if "\t" in key_id or "\n" in key_id or not signature:
        continue
    print(f"{key_id}\t{signature}")
PY
[[ -s "$entries" ]] || fail "the document carries no usable ed25519 signature"

# An ed25519 SubjectPublicKeyInfo is a fixed 12-byte prefix plus the 32-byte
# key, so a raw key becomes a PEM openssl will read with no key-format tooling.
spki_prefix="MCowBQYDK2VwAyEA"

for raw_key in "${keys[@]}"; do
  label="${raw_key%%:*}"
  encoded="${raw_key#*:}"
  if [[ "$encoded" == "$raw_key" ]]; then label=""; fi
  material="$work/key.raw"
  b64d "$encoded" > "$material" 2>/dev/null || fail "--public-key ${label:-$raw_key} is not base64"
  size=$(wc -c < "$material")
  [[ "$size" -eq 32 ]] || fail "--public-key ${label:-$raw_key} is $size bytes, not the 32 of an ed25519 public key"

  pem="$work/key.pem"
  {
    echo "-----BEGIN PUBLIC KEY-----"
    echo "${spki_prefix}${encoded}"
    echo "-----END PUBLIC KEY-----"
  } > "$pem"
  openssl pkey -pubin -in "$pem" -noout 2>/dev/null \
    || fail "--public-key ${label:-$raw_key} is not a usable ed25519 public key"

  while IFS=$'\t' read -r entry_id entry_sig; do
    sig="$work/sig.bin"
    b64d "$entry_sig" > "$sig" 2>/dev/null || continue
    [[ "$(wc -c < "$sig")" -eq 64 ]] || continue
    if openssl pkeyutl -verify -rawin -pubin -inkey "$pem" \
        -sigfile "$sig" -in "$manifest" >/dev/null 2>&1; then
      echo "PASS $manifest: signed by ${label:-(unlabelled)} (document key_id ${entry_id:-none})"
      exit 0
    fi
  done < "$entries"
done

fail "no signature in the document was made by a given key"
