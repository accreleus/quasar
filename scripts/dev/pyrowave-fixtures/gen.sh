#!/usr/bin/env bash
# Regenerate (or --check) the golden PyroWave fixtures in testdata/pyrowave/ (#1053).
#
#   scripts/dev/pyrowave-fixtures/gen.sh [--check]
#
# Everything runs inside the platform image's toolchain role (quasar-dev, built from
# deploy/Dockerfile.vulkan --target dev), which carries the pinned libpyrowave-shared in /opt/gst.
# The encoder is Vulkan compute and needs a real GPU: lavapipe cannot force the subgroup sizes
# it needs. The source goes in over stdin and the results come back over stdout as a tar, so the
# Docker daemon may be a remote one (a lab host's context) as well as the local one.
#
#   DOCKER    docker command           (default: docker)           e.g. "docker -c <lab-context>"
#   GPU_ARGS  how the container sees the GPU (default: none)      e.g. "--device nvidia.com/gpu=all"
#                                                                  or "--device /dev/dri/renderD128"
#   NVIDIA_VOLUME  docker volume the node agent provisioned the NVIDIA userspace into (an NVIDIA host
#             with CDI: its ICD and libraries are not in the image). Adds what deploy/validate-image.sh sets.
#   IMAGE     the toolchain image       (default: quasar-dev:latest; build it with `dev.sh image`
#                                        on a tree whose Dockerfile.vulkan carries the pins)
#
# --check regenerates into a temp directory and fails if any file differs from the committed ones
# (the manifest is compared without its encoder_device record, so a GPU that reproduces every byte
# passes). Without it the files in testdata/pyrowave/ are replaced.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
out="${OUT:-$repo/testdata/pyrowave}"
DOCKER="${DOCKER:-docker}"
IMAGE="${IMAGE:-quasar-dev:latest}"
check=0; [ "${1:-}" = "--check" ] && check=1

pin() { sed -n "s/^ARG $1=\\([0-9a-f]\\{40\\}\\)\$/\\1/p" "$repo/deploy/Dockerfile.vulkan"; }
pyrowave_ref="$(pin PYROWAVE_REF)"; granite_ref="$(pin GRANITE_REF)"
[ -n "$pyrowave_ref" ] && [ -n "$granite_ref" ] || { echo "no PYROWAVE_REF/GRANITE_REF in deploy/Dockerfile.vulkan" >&2; exit 1; }
have="$($DOCKER image inspect "$IMAGE" --format '{{index .Config.Labels "org.quasar.pins.pyrowave"}}' 2>/dev/null || true)"
[ "$have" = "$pyrowave_ref" ] || { echo "$IMAGE carries PyroWave '${have:-none}', Dockerfile.vulkan pins $pyrowave_ref: rebuild the image" >&2; exit 1; }

gpu_args=(${GPU_ARGS:-})
if [ -n "${NVIDIA_VOLUME:-}" ]; then
  d=/opt/quasar/nvidia-driver
  gpu_args+=(-v "$NVIDIA_VOLUME:$d:ro" -e "LD_LIBRARY_PATH=$d/lib64"
    -e "__EGL_VENDOR_LIBRARY_DIRS=$d/glvnd/egl_vendor.d:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d"
    -e "__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=$d/egl_external_platform.d:/usr/share/egl/egl_external_platform.d"
    -e "VK_ADD_DRIVER_FILES=$d/vulkan/icd.d/nvidia_icd.json")
fi

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
tar -C "$here" -cf - pyrowave_fixtures.c manifest.py | $DOCKER run --rm -i "${gpu_args[@]}" --entrypoint bash "$IMAGE" -c '
  set -eu
  mkdir /tmp/src /tmp/out; tar -C /tmp/src -xf -
  export PKG_CONFIG_PATH=/opt/gst/lib64/pkgconfig LD_LIBRARY_PATH=/opt/gst/lib64:${LD_LIBRARY_PATH:-}
  gcc -O2 -Wall -o /tmp/pwfix /tmp/src/pyrowave_fixtures.c $(pkg-config --cflags --libs pyrowave-shared) -lvulkan >&2
  /tmp/pwfix /tmp/out >&2
  python3 -I /tmp/src/manifest.py /tmp/out '"$pyrowave_ref $granite_ref"' >&2
  rm /tmp/out/device.json /tmp/out/budgets.tsv
  tar -C /tmp/out -cf - .' | tar --no-same-owner -C "$tmp" -xf -
[ -s "$tmp/manifest.json" ] || { echo "the generator produced nothing" >&2; exit 1; }

if [ "$check" = 1 ]; then
  diff -r --exclude=manifest.json --exclude=README.md "$out" "$tmp"
  diff <(jq 'del(.encoder_device)' "$out/manifest.json") <(jq 'del(.encoder_device)' "$tmp/manifest.json")
  echo "pyrowave fixtures: reproduced byte for byte ($(find "$tmp" -type f | wc -l) files)"
else
  mkdir -p "$out"; find "$out" -type f ! -name README.md -delete; cp "$tmp"/* "$out"/
  echo "pyrowave fixtures: wrote $(find "$out" -type f ! -name README.md | wc -l) files, $(du -sk "$out" | cut -f1) KiB, to ${out#"$repo"/}"
fi
