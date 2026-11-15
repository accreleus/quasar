#!/usr/bin/env bash
# scripts/release/toolchain-tag-check.sh: a matching tag passes, any other fails, and so
# does a base that is not a digest. Uses the real toolchain-hash.sh on this checkout.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
chk="$root/scripts/release/toolchain-tag-check.sh"
fail() { echo "toolchain-tag-check test: $*" >&2; exit 1; }
base="ghcr.io/example/quasar-base@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
tag="$(QUASAR_BASE_DIGEST="${base##*@}" bash "$root/deploy/toolchain-hash.sh")"
ns=ghcr.io/example/quasar/quasar-gst-toolchain
bash "$chk" "$ns:$tag" "$base" >/dev/null || fail "the matching tag was refused"
out="$(bash "$chk" "$ns:0000000000000000" "$base" 2>&1)" && fail "a different tag was accepted"
grep -q "$tag" <<<"$out" || fail "the refusal does not name the checkout's tag: $out"
# A moved base digest is a different tag too.
other="ghcr.io/example/quasar-base@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
bash "$chk" "$ns:$tag" "$other" >/dev/null 2>&1 && fail "a different base digest was accepted"
bash "$chk" "$ns:$tag" "ghcr.io/example/quasar-base:latest" >/dev/null 2>&1 && fail "a base that is not a digest was accepted"
echo "PASS toolchain-tag-check: the matching tag passes; a different tag, a moved base or a tag base fails"
