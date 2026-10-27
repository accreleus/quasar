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

# The script must say all three, well-formed: the workflow reads "any =true" from it, so a
# short or garbled answer must not read as "nothing changed".
out="$(printf '%s\n' docs/a.md | bash "$gate")"
[ "$(grep -c -E '^(control_plane|node_agent|recovery)=(true|false)$' <<<"$out")" -eq 3 ] || fail "malformed output: $out"
# ...and the workflow must not swallow a failure of the script (it fails closed).
! grep -n 'images-changed.sh.*||' "$repo/.github/workflows/images.yml" || fail "images.yml masks a failing images-changed.sh"

# Drift guard: every COPY/ADD source (and bind-mount source) of an artefact's Dockerfile,
# continuation lines joined, must reach that artefact.
sources() { # sources < dockerfile -> one build-context path per line
  sed -e ':a' -e '/\\$/{N;s/\\\n[[:space:]]*/ /;ba' -e '}' | awk '
    /^(COPY|ADD) / && !/--from=/ {
      n = 0; for (i = 2; i <= NF; i++) if ($i !~ /^--/) a[n++] = $i
      for (i = 0; i < n - 1; i++) print a[i]; delete a }
    /^RUN / { for (i = 2; i <= NF; i++) if ($i ~ /^--mount=type=bind/ && $i !~ /from=/ && match($i, /source=[^,]+/))
      print substr($i, RSTART + 7, RLENGTH - 7) }'
}
covered() { # covered <dockerfile> <want-token>
  local src p
  while IFS= read -r src; do
    for p in "${src%/}" "${src%/}/probe"; do
      [[ "$(decide "$p")" == *"$2,"* ]] && continue 2
    done
    fail "$1: build source '$src' does not reach $2"
  done < <(sources < "$repo/$1")
}
probe="$(printf 'COPY a.patch \\\n     deploy/newdir/x.conf \\\n     /tmp/\nADD deploy/added.tgz /x\nRUN --mount=type=bind,source=deploy/bound,target=/b true\nCOPY --from=build /o /p\n')"
[ "$(sources <<<"$probe" | tr '\n' ' ')" = "a.patch deploy/newdir/x.conf deploy/added.tgz deploy/bound " ] || fail "sources() misses a continuation, ADD or bind-mount source: $(sources <<<"$probe" | tr '\n' ' ')"
covered deploy/Dockerfile.control.prod cp
covered deploy/Dockerfile.vulkan na
covered deploy/Dockerfile.recovery rec
echo "PASS images-changed: paths map to the artefacts they feed"
