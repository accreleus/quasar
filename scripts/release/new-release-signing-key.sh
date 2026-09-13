#!/usr/bin/env bash
# Generate a release signing key pair for an operator to install.
#
# NOTHING THIS WRITES MAY ENTER THE REPOSITORY. The script refuses to write
# inside the working tree, writes the private key at mode 0600, and prints the
# public half — the only half that is ever configured or published.
#
# The operator runs this once, by hand, on a machine they trust. No CI job runs
# it: a key a pipeline generated is a key nobody controls.
#
# Full procedure, including the CI secret to create and how to rotate:
# docs/upgrading.md §"Signing platform releases".
#
# Exit codes: 0 wrote a key pair · 1 refused · 2 usage.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

out=""
key_id=""

usage() {
  cat <<'EOF'
usage: scripts/release/new-release-signing-key.sh --out PATH --key-id LABEL

Writes an ed25519 private key (PKCS#8 PEM) to PATH, mode 0600, and prints:

  * the public key as `key-id:base64`, for QUASAR_UPDATER_TRUSTED_KEYS on every
    host that should trust it, and
  * the exact `gh secret set` command for the release pipeline.

PATH must be outside the repository. Pick a label that will still mean something
at the next rotation, e.g. quasar-release-2026.
EOF
}

while (($#)); do
  case "$1" in
    --out) out=${2:?--out needs a path}; shift 2 ;;
    --key-id) key_id=${2:?--key-id needs a value}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

if [[ -z "$out" || -z "$key_id" ]]; then
  echo "new-release-signing-key: --out and --key-id are required" >&2
  usage >&2
  exit 2
fi
if [[ ! "$key_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]]; then
  echo "new-release-signing-key: --key-id must be 1-64 chars of [A-Za-z0-9._-]" >&2
  exit 1
fi
if [[ -e "$out" ]]; then
  echo "new-release-signing-key: $out already exists; refusing to overwrite a key" >&2
  exit 1
fi

mkdir -p "$(dirname "$out")"
abs_out="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
case "$abs_out" in
  "$root"/*|"$root")
    echo "new-release-signing-key: $abs_out is inside the repository at $root." >&2
    echo "A signing key must never be committable. Write it somewhere outside the tree." >&2
    exit 1 ;;
esac

umask 077
openssl genpkey -algorithm ed25519 -out "$out"
chmod 600 "$out"
public_key="$(openssl pkey -in "$out" -pubout -outform DER \
  | tail -c 32 | { base64 -w0 2>/dev/null || base64 | tr -d '\n'; })"

cat <<EOF

Wrote the private key to $out (mode 0600). Back it up somewhere you would be
willing to restore a release from; there is no recovery from losing it, only a
rotation to a new one.

Public key — add to QUASAR_UPDATER_TRUSTED_KEYS on every host, comma-separated
alongside any key you are rotating away from:

  $key_id:$public_key

Release pipeline — create the repository secret holding the PRIVATE key:

  gh secret set QUASAR_RELEASE_SIGNING_KEY --repo <owner/name> < $out
  gh variable set QUASAR_RELEASE_SIGNING_KEY_ID --repo <owner/name> --body '$key_id'

Then delete nothing from your backup, and never paste the private key into an
issue, a log, or this repository.
EOF
