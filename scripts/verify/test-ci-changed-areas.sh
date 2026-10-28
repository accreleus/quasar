#!/usr/bin/env bash
# Table test for scripts/verify/ci-changed-areas.sh (#919): changed paths -> CI test areas.
# Wrong in one direction a pull request skips a test it breaks (develop's push run still
# catches it); in the other it runs a job for nothing.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
areas="$repo/scripts/verify/ci-changed-areas.sh"
fail() { echo "ci-changed-areas test: $*" >&2; exit 1; }

decide() { # decide <paths...> -> "go,web,rust,site" subset
  local out
  out="$(printf '%s\n' "$@" | bash "$areas")"
  for a in go web rust site; do grep -q "^$a=true" <<<"$out" && printf '%s,' "$a"; done
  return 0
}
expect() { # expect <want> <paths...>
  local want="$1" got; shift
  got="$(decide "$@")"; got="${got%,}"; got="${got:--}"
  [ "$got" = "$want" ] || fail "[$*] -> $got, want $want"
}

expect -        # nothing changed
expect - CHANGELOG.md README.md docs/architecture-and-plan.md design_handoff_v3/README.md scripts/dev/dev.sh
expect go control-plane/internal/session/launcher.go
expect go control-plane/migrations/0108_x.up.sql
expect go scripts/verify/go-test-parallel.sh
expect web web/src/App.tsx
expect web docs/upgrading.md
expect rust node-agent/src/main.rs
expect rust node-agent/Cargo.lock
expect site site/src/content/docs/index.mdx
expect go,site protocol
expect go,site protocol/openapi.yaml
expect go,web web/index.html
expect go,web web/src/lib/metrics.generated.json
expect go,web,rust docs/session-trace/metrics.json
expect web,rust docs/configuration.md
expect go,rust control-plane/internal/buildinfo/recipe.go
expect go,rust node-agent/crates/quasar-recovery/src/journal.rs
expect go,web,rust,site testdata/enroll-host/pins.json
expect go,web,rust,site deploy/enroll-host.sh
expect go,web,rust,site .github/workflows/ci.yml
expect go,web,rust,site scripts/verify/ci-apt-install.sh
expect go,web,rust,site .gitmodules
expect go,web web/src/App.tsx control-plane/a.go CHANGELOG.md
bash "$areas" --all </dev/null 2>/dev/null && fail "an unknown flag must be refused"

# All four, well-formed: ci.yml reads each line, so a short answer must not read as "skip".
out="$(printf '%s\n' docs/a.md | bash "$areas")"
[ "$(grep -c -E '^(go|web|rust|site)=(true|false)$' <<<"$out")" -eq 4 ] || fail "malformed output: $out"
echo "ci-changed-areas test: ok"
