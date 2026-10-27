#!/usr/bin/env bash
# deploy/toolchain-hash.sh — compute the content tag for the quasar-gst-toolchain
# artefact image.
#
# WHAT THE TAG MEANS. `quasar-gst-toolchain:<hash>` is the patched GStreamer 1.28
# tree (/opt/gst), gst-interpipe, gst-plugins-rs, gst-wayland-display and the
# Rust/CUDA toolchain that produce them. It takes ~40 minutes to compile and it
# changes only when one of its INPUTS changes — a few times a month, on a pin
# bump. Node-agent code changes daily and does not touch it at all. So the tag
# is a hash over the inputs, and the build is skipped whenever a tag already
# exists in the registry:
#
#     tag="$(deploy/toolchain-hash.sh)"
#     docker manifest inspect ghcr.io/…/quasar-gst-toolchain:"$tag" \
#       && echo "reuse"  || echo "build it"
#
# WHAT IS HASHED, AND WHY EACH. Getting this set wrong is the only way this
# scheme can hurt you: a MISSING input means a stale toolchain is silently
# reused and the change you made never reaches the image.
#
#   1. The TOOLCHAIN keys of deploy/pins.env. The refs and versions that are
#      literally checked out and compiled.
#   2. Every file under deploy/patches/. The vulkan patch set is applied to the
#      GStreamer tree at build time; editing a patch must produce a new
#      toolchain. (The gwd patches under deploy/patches/vulkan/ are the authored
#      record and are NOT applied — the fork's develop branch carries them — but
#      they are hashed anyway: it costs one rebuild on a re-diff and removes the
#      need for anyone to remember which subset is live.)
#   3. The `toolchain` STAGE TEXT of deploy/Dockerfile.vulkan, byte for byte,
#      from its FROM line to the line before the next FROM. This is what catches
#      a change to the meson enable-set, the vulkan-headers floor check, the
#      static-archive strip, or the order of the build steps — none of which is
#      a "pin" but all of which change what /opt/gst contains.
#
# WHAT IS DELIBERATELY NOT HASHED: node-agent/ sources (they are compiled in the
# `build` stage, which sits ON TOP of the toolchain), the runtime/nv/dev stage
# text, DOCKER_VERSION, SAMPLY_VERSION. A change to any of those must NOT
# invalidate a 40-minute compile.
#
# STABILITY: the hash must be reproducible on a CI runner and on the devbox, so
# it is computed from file CONTENT only — never from mtimes, git SHAs, or the
# working directory. Neither sha256 front-end is universal: `sha256sum`
# (coreutils) is absent on macOS, and `shasum` (a perl script) is absent on a
# minimal Fedora — the first devbox run of this script died on exactly that. Both
# emit the same lowercase hex digest, so either will do; pick whichever exists.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PINS_FILE="${QUASAR_PINS_FILE:-$SCRIPT_DIR/pins.env}"
DOCKERFILE="${QUASAR_TOOLCHAIN_DOCKERFILE:-$SCRIPT_DIR/Dockerfile.vulkan}"
PATCH_DIR="$SCRIPT_DIR/patches"

# The pins that define the toolchain. Keep in step with the TOOLCHAIN section of
# deploy/pins.env — build-images.sh asserts every name here exists in that file.
TOOLCHAIN_PIN_KEYS=(
  QUASAR_BASE_IMAGE
  GST_VERSION
  GST_PLUGINS_RS_REF
  GST_INTERPIPE_REF
  GST_WAYLAND_DISPLAY_REPO
  GST_WAYLAND_DISPLAY_REF
  RUST_VERSION
  CARGO_C_VERSION
  RUSTUP_VERSION
  RUSTUP_INIT_SHA256_X86_64
  RUSTUP_INIT_SHA256_AARCH64
  CUDA_PKG_VERSION
)

if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 | cut -d' ' -f1; }
else
  echo "toolchain-hash: neither sha256sum nor shasum is available" >&2; exit 2
fi

[ -f "$PINS_FILE" ]   || { echo "toolchain-hash: missing $PINS_FILE" >&2; exit 2; }
[ -f "$DOCKERFILE" ]  || { echo "toolchain-hash: missing $DOCKERFILE" >&2; exit 2; }

# The base's IDENTITY, not the string that names it. `quasar-base:latest` is a moving
# tag: rebuilt upstream, its content changes but the tag does not, so the hash still hits
# and the 40-minute compile is skipped against a base that is no longer the one it was
# built on — the "a MISSING input silently reuses a stale toolchain" failure this file
# exists to prevent. A digest reference is already an identity; a tag needs
# QUASAR_BASE_DIGEST, which the publisher resolves once and passes in. With neither, hash
# the tag as before and SAY SO: refusing would break every offline build.
base_image_identity() {
  local ref="$1"
  case "$ref" in
    *@sha256:*) printf '%s' "$ref"; return ;;
  esac
  if [ -n "${QUASAR_BASE_DIGEST:-}" ]; then
    printf '%s@%s' "${ref%%:*}" "${QUASAR_BASE_DIGEST#*@}"
    return
  fi
  echo "toolchain-hash: WARNING — $ref is a mutable tag and QUASAR_BASE_DIGEST is unset, so a rebuilt base does NOT change this tag and a stale toolchain will be reused. Resolve it with: docker buildx imagetools inspect --format '{{.Manifest.Digest}}' $ref" >&2
  printf '%s' "$ref"
}

emit_inputs() {
  # 1. toolchain pins, in the fixed order above (not sorted by the shell's
  #    locale, which differs between macOS and Fedora).
  local k v
  for k in "${TOOLCHAIN_PIN_KEYS[@]}"; do
    v="$(grep -E "^${k}=" "$PINS_FILE" | head -1 | cut -d= -f2-)"
    [ -n "$v" ] || { echo "toolchain-hash: $k is not set in $PINS_FILE" >&2; exit 2; }
    [ "$k" = QUASAR_BASE_IMAGE ] && v="$(base_image_identity "$v")"
    printf 'pin %s=%s\n' "$k" "$v"
  done

  # 2. every patch file, by content, in byte-sorted path order.
  if [ -d "$PATCH_DIR" ]; then
    local f
    while IFS= read -r f; do
      printf 'patch %s %s\n' "${f#"$REPO_ROOT/"}" "$(sha < "$f")"
    done < <(find "$PATCH_DIR" -type f -name '*.patch' | LC_ALL=C sort)
  fi

  # 3. the toolchain stage text, verbatim. awk prints from the toolchain FROM
  #    line up to (not including) the next FROM at column 0.
  printf 'stage %s\n' "$(
    awk '
      /^FROM .* AS toolchain$/ { inside = 1 }
      inside && /^FROM / && !/AS toolchain$/ { exit }
      inside { print }
    ' "$DOCKERFILE" | sha
  )"
}

INPUTS="$(emit_inputs)"

case "${1:-}" in
  --explain)
    # Human-readable: what went into the tag. Used by build-images.sh -v and by
    # anyone asking "why did the toolchain rebuild?".
    printf '%s\n' "$INPUTS"
    printf -- '---\ntag: %s\n' "$(printf '%s\n' "$INPUTS" | sha | cut -c1-16)"
    ;;
  ''|--tag)
    # 16 hex chars: 64 bits of collision resistance over a set of inputs that
    # changes a few times a month, and short enough to read in a `docker images`
    # listing. Prefixed nowhere — the tag IS the hash, so an image's identity is
    # legible without a lookup.
    printf '%s\n' "$INPUTS" | sha | cut -c1-16
    ;;
  *)
    echo "usage: toolchain-hash.sh [--tag|--explain]" >&2; exit 64 ;;
esac
