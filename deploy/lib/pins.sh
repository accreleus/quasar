#!/usr/bin/env bash
# pins.sh — read deploy/pins.env from a shell script. Source it; do not copy the
# values it holds.
#
# WHY THIS EXISTS. deploy/pins.env is the source of truth for every third-party
# pin, and build-images.sh asserts that deploy/Dockerfile.vulkan's global ARG
# defaults agree with it. The DEPLOY SCRIPTS were never part of that agreement:
# build-images.sh and redeploy.sh each carried their own `${QUASAR_BASE_IMAGE:-
# <literal>}` fallback, so the same ref existed as three independent strings with
# nothing tying them together. They drifted exactly as you would expect — the
# org move to accretion-io updated one copy per commit, and a redeploy from a
# checkout that had one copy but not the other died with a raw
#
#   ERROR: failed to solve: ... load metadata for ghcr.io/<org>/quasar-base:<tag>
#   ... 401 Unauthorized
#
# from BuildKit, minutes into a build, naming a ref that appears in no error
# message the operator can act on. Both scripts now read the value from
# pins.env through this file, so there is one definition and the drift is not
# expressible.
#
# The Dockerfiles cannot source shell, so their `ARG QUASAR_BASE_IMAGE=` defaults
# stay separate literals — that is what build-images.sh's check_pins_agree (for
# Dockerfile.vulkan) and check_base_arg_agrees (for Dockerfile.control.prod)
# exist to police. Every other consumer sources this.

# Resolve pins.env relative to THIS file, not the caller: redeploy.sh runs from
# the repo root, build-images.sh from anywhere, and both may be invoked by an
# absolute path.
QUASAR_PINS_FILE="${QUASAR_PINS_FILE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/pins.env}"

# pin_value <KEY> — print the value of KEY from pins.env, or fail loudly.
#
# The parse matches deploy/toolchain-hash.sh's: strict KEY=VALUE, first match
# wins, everything after the first `=` is the value (GST_WAYLAND_DISPLAY_REPO is
# a URL and contains none, but a future pin might). pins.env is deliberately
# boring so this stays a grep rather than a shell `source` — sourcing it would
# also dump nine unrelated pins into the caller's environment, where compose and
# docker would then see them.
pin_value() { # pin_value <KEY>
  local key="${1:?pin_value: KEY required}" value
  [ -f "$QUASAR_PINS_FILE" ] || {
    printf 'pins: missing %s (the pin source of truth)\n' "$QUASAR_PINS_FILE" >&2
    return 2
  }
  value="$(grep -E "^${key}=" "$QUASAR_PINS_FILE" | head -1 | cut -d= -f2-)"
  [ -n "$value" ] || {
    printf 'pins: %s is not set in %s\n' "$key" "$QUASAR_PINS_FILE" >&2
    return 2
  }
  printf '%s\n' "$value"
}

# quasar_base_image — the base image every Quasar image builds FROM.
#
# An explicit QUASAR_BASE_IMAGE in the environment wins, which is how a release
# build pins an exact base digest (scripts/release/release-preflight.sh REQUIRES a
# name@sha256:<hex> form there and rejects a tag). With nothing set, the answer
# is pins.env's QUASAR_BASE_IMAGE — currently the moving `:latest` tag, which
# quasar-images publishes from its `stable` branch on every reviewed merge.
quasar_base_image() {
  printf '%s\n' "${QUASAR_BASE_IMAGE:-$(pin_value QUASAR_BASE_IMAGE)}"
}
