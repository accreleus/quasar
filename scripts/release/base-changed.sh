#!/usr/bin/env bash
# Has quasar-base moved since the published preview's node-agent image was built? (#1092)
#
#   base-changed.sh AGENT_IMAGE BASE_REF
#     AGENT_IMAGE  the node-agent image@digest the preview manifest names
#     BASE_REF     the base this run resolved, <image>@sha256:<hex>
#
# The Images gate calls it on a push to develop. Before the nightly toolchain refresh
# (toolchain-refresh.yml) an unpublished toolchain made that gate build every image, and a
# rebuilt base reached the preview that way. The refresh publishes the toolchain first, so
# this is the check that is left: the agent image's `org.quasar.pins.base` label names the
# base it was built FROM.
#
# Prints rebuild=true|false and why=<reason>. Rebuild is the safe answer whenever the
# comparison cannot be made because something is ABSENT (no image, no label, a label or
# a base that is not a digest). A registry that cannot be read is an ERROR (exit 1):
# an outage must neither skip a needed build nor force an unneeded one.
set -euo pipefail
(($# == 2)) || { echo "usage: base-changed.sh AGENT_IMAGE BASE_REF" >&2; exit 2; }
agent=$1 base=$2

say() { echo "rebuild=$1"; echo "why=$2"; exit 0; }

want="${base##*@}"
[[ "$base" == *@sha256:* ]] || say true "this run's base ($base) is not a digest, so it cannot be shown unchanged"

if ! got="$(docker buildx imagetools inspect --format '{{index .Image.Config.Labels "org.quasar.pins.base"}}' "$agent" 2>&1)"; then
  case "$got" in
    *"manifest unknown"* | *"name unknown"* | *"no such manifest"* | *"not found"*)
      say true "the preview's agent image $agent is not in the registry" ;;
    *) echo "::error::could not read the preview's agent image $agent; the registry said: ${got//$'\n'/ }" >&2; exit 1 ;;
  esac
fi

case "$got" in
  '' | '<no value>') say true "the preview's agent image carries no org.quasar.pins.base label" ;;
  *@sha256:*) ;;
  *) say true "the preview's agent image was built on $got, which is not a digest" ;;
esac
have="${got##*@}"
if [[ "$have" == "$want" ]]; then
  say false "the base is unchanged ($want)"
fi
say true "the base moved: the preview's agent image was built on $have, this run resolved $want"
