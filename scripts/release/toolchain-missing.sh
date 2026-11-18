#!/usr/bin/env bash
# Is the quasar-gst-toolchain image for the CURRENT pins and the CURRENT base published?
# The nightly toolchain refresh (.github/workflows/toolchain-refresh.yml) asks this to
# decide whether to build; agent-live.yml derives the same tag the same way (#1092).
#
# Prints key=value lines (the workflow appends them to $GITHUB_OUTPUT):
#   image=<registry>/quasar-gst-toolchain:<hash>
#   base_ref=<base image by digest>
#   build=true|false        true only when the registry ANSWERED "no such tag".
# Run it from the repo root. Needs REGISTRY_NS (e.g. ghcr.io/accreleus/quasar).
#
# QUASAR_BASE_DIGEST pins the base digest the tag is worked out for, instead of asking the
# registry what `quasar-base:latest` is now. The refresh's recheck passes the digest its
# `check` job recorded, so a base rebuild between the two jobs cannot make it ask about a
# tag this run will not build (#1092). Set but empty (a `check` output that went missing)
# is an ERROR, never a fallback to the live base: falling back would ask about a different
# tag than this run is about to build.
#
# A registry that cannot be read is an ERROR (exit 1), never "missing": a hiccup must not
# read as "absent" and cost a 40-minute build. Only the registry's own "manifest unknown"
# / "name unknown" / "<ref>: not found" counts as missing.
set -euo pipefail
: "${REGISTRY_NS:?REGISTRY_NS is required}"

base="$(grep -E '^QUASAR_BASE_IMAGE=' deploy/pins.env | head -1 | cut -d= -f2-)"
[ -n "$base" ] || { echo "deploy/pins.env declares no QUASAR_BASE_IMAGE" >&2; exit 1; }

digest=""
if [ "${QUASAR_BASE_DIGEST+set}" = set ]; then
  digest="${QUASAR_BASE_DIGEST#*@}"
  [ -n "$digest" ] || {
    echo "::error::QUASAR_BASE_DIGEST is set but empty: the recheck has no recorded base digest to work out its tag from (a missing check output is an error, not a live lookup)." >&2
    exit 1
  }
else
  if ! digest="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$base")"; then
    echo "::error::could not read the base image $base from the registry, so the toolchain tag cannot be worked out (the error is above). Re-run if the registry was unreachable." >&2
    exit 1
  fi
fi
printf '%s\n' "$digest" | grep -Eqx 'sha256:[0-9a-f]{64}' || {
  echo "::error::unexpected digest for $base: $digest (need sha256: followed by 64 lowercase hex digits)" >&2
  exit 1
}

tag="$(QUASAR_BASE_DIGEST="$digest" bash deploy/toolchain-hash.sh)"
image="$REGISTRY_NS/quasar-gst-toolchain:$tag"

build=false
if ! said="$(docker manifest inspect "$image" 2>&1 >/dev/null)"; then
  case "$said" in
    # No docker at all is not a missing tag (a shell says `docker: not found`).
    *"command not found"* | *"docker: not found"*) echo "::error::cannot run docker: ${said//$'\n'/ }" >&2; exit 1 ;;
    *"manifest unknown"* | *"name unknown"* | *"no such manifest"* | *": not found"*) build=true ;;
    *) echo "::error::could not tell whether $image is published; the registry said: ${said//$'\n'/ }" >&2; exit 1 ;;
  esac
fi

echo "image=$image"
echo "base_ref=$(printf '%s' "$base" | sed 's/:[^:/]*$//')@${digest}"
echo "build=$build"
