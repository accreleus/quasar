#!/usr/bin/env bash
# Print the org release keys the recovery actor trusts by default, one
# `key-id:base64` per line, read from DEFAULT_TRUSTED_KEYS in the actor's source
# so a key rotation edits one place. The release job verifies every signature it
# publishes against these (docs/upgrading.md "Signing platform releases").
#
# usage: scripts/release/org-release-keys.sh [signature.rs]
# Exit codes: 0 printed at least one key · 1 found none.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
src="${1:-$repo/node-agent/crates/quasar-recovery/src/trust/signature.rs}"

# The constant's string literal may sit on the line after its declaration.
raw="$(awk '/const DEFAULT_TRUSTED_KEYS/ { f = 1 }
  f && match($0, /"[^"]*"/) { print substr($0, RSTART + 1, RLENGTH - 2); exit }' "$src")"

keys="$(printf '%s' "$raw" | tr ',' '\n' | sed 's/^ *//; s/ *$//')"
if [[ -z "$keys" ]] || grep -qvE '^[A-Za-z0-9._-]{1,64}:[A-Za-z0-9+/]{43}=$' <<<"$keys"; then
  echo "org-release-keys: DEFAULT_TRUSTED_KEYS in $src is missing or not a key-id:base64 list: '$raw'" >&2
  exit 1
fi
printf '%s\n' "$keys"
