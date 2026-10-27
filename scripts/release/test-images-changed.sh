#!/usr/bin/env bash
# Table test for scripts/release/images-changed.sh (#867): changed paths -> artefacts.
# Wrong in one direction a landing ships a stale image; in the other it rebuilds for nothing.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$repo/scripts/release/images-changed.sh"
fail() { echo "images-changed test: $*" >&2; exit 1; }

# decide <paths...> -> "cp,na,rec" subset, "-" for nothing
decide() {
  local out
  out="$(printf '%s\n' "$@" | bash "$gate")"
  grep -q '^control_plane=true' <<<"$out" && printf 'cp,'
  grep -q '^node_agent=true' <<<"$out" && printf 'na,'
  grep -q '^recovery=true' <<<"$out" && printf 'rec,'
  return 0
}
expect() { # expect <want> <paths...>
  local want="$1" got; shift
  got="$(decide "$@")"; got="${got%,}"; got="${got:--}"
  [ "$got" = "$want" ] || fail "[$*] -> $got, want $want"
}

expect -        # nothing changed
expect - docs/architecture-and-plan.md CHANGELOG.md README.md
expect - protocol .github/workflows/ci.yml scripts/release/images-changed.sh deploy/redeploy.sh deploy/docker-compose.yml
expect cp web/src/App.tsx
expect cp web/src/App.tsx docs/a.md
expect cp control-plane/internal/auth/auth.go control-plane/migrations/0001_x.up.sql
expect cp deploy/Dockerfile.control.prod
expect cp deploy/enroll-host.sh
expect cp deploy/control-entrypoint.sh
expect na,rec node-agent/src/main.rs
expect na,rec node-agent/crates/quasar-recovery/src/lib.rs
expect na deploy/Dockerfile.vulkan
expect na deploy/patches/vulkan/vkh264.patch
expect na deploy/audio/quasar-session.conf
expect rec deploy/Dockerfile.recovery
expect cp,na,rec deploy/pins.env
expect cp,na,rec deploy/lib/recipe-revision.sh
expect cp,na,rec .dockerignore
expect cp,na,rec .github/workflows/images.yml
expect cp,na,rec web/a.ts node-agent/b.rs
expect cp,na control-plane/a.go deploy/Dockerfile.vulkan
bash "$gate" --all </dev/null 2>/dev/null && fail "an unknown flag must be refused"

# Drift guard: every COPY source of an artefact's Dockerfile must reach that artefact.
covered() { # covered <dockerfile> <want-token>
  local src p
  while IFS= read -r src; do
    for p in "${src%/}" "${src%/}/probe"; do
      [[ "$(decide "$p")" == *"$2,"* ]] && continue 2
    done
    fail "$1: COPY source '$src' does not reach $2"
  done < <(awk '/^COPY / && !/--from=/ {
      n = 0; for (i = 2; i <= NF; i++) if ($i !~ /^--/) a[n++] = $i
      for (i = 0; i < n - 1; i++) print a[i]; delete a }' "$repo/$1")
}
covered deploy/Dockerfile.control.prod cp
covered deploy/Dockerfile.vulkan na
covered deploy/Dockerfile.recovery rec
echo "PASS images-changed: paths map to the artefacts they feed"
