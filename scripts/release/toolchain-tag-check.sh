#!/usr/bin/env bash
# Does this checkout still hash to the toolchain tag the `check` job published a decision for? (#1092)
#
#   toolchain-tag-check.sh IMAGE BASE_REF
#     IMAGE     <registry>/quasar-gst-toolchain:<tag>, from toolchain-missing.sh
#     BASE_REF  the base by digest, <image>@sha256:<hex>, from the same run
#
# The tag is content-addressed and never rebuilt, so an image built from a tree that hashes
# to another tag would sit under the wrong one for good. The build job runs this on the
# commit it checked out, immediately before it builds.
set -euo pipefail
(($# == 2)) || { echo "usage: toolchain-tag-check.sh IMAGE BASE_REF" >&2; exit 2; }
image=$1 base=$2
case "$base" in *@sha256:*) ;; *) echo "::error::$base is not a digest reference" >&2; exit 1 ;; esac
got="$(QUASAR_BASE_DIGEST="${base##*@}" bash "$(dirname "${BASH_SOURCE[0]}")/../../deploy/toolchain-hash.sh")"
if [ "$got" != "${image##*:}" ]; then
  echo "::error::this checkout hashes to toolchain tag $got, but the check job decided on ${image##*:}. The pins, patches or toolchain stage changed in between; building would publish the wrong content under that tag. Re-run." >&2
  exit 1
fi
echo "toolchain tag ${got} matches the checkout"
