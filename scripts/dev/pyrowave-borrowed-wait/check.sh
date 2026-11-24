#!/usr/bin/env bash
# #1663: build PyroWave at the pinned refs the way deploy/Dockerfile.vulkan does, with the vendored
# Granite patch (or without it: --unpatched), and run borrowed_wait.cpp on lavapipe in the dev image.
# Passes when the library makes no vkDeviceWaitIdle call on a borrowed device and the Khronos
# validation layer reports nothing (no object freed while Granite's own work is in flight).
# No GPU needed; needs network (clones PyroWave and Granite, installs the validation layer).
#
#   scripts/dev/pyrowave-borrowed-wait/check.sh [--unpatched]     IMAGE defaults to quasar-agent-dev:latest
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
apply=1; [ "${1:-}" = "--unpatched" ] && apply=0
# shellcheck disable=SC1090
. <(grep -E '^(PYROWAVE_REF|GRANITE_REF)=' "$repo/deploy/pins.env")

docker run --rm -i -e PYROWAVE_REF="$PYROWAVE_REF" -e GRANITE_REF="$GRANITE_REF" -e APPLY="$apply" \
  -e VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/lvp_icd.x86_64.json -e VK_LOADER_DRIVERS_DISABLE= \
  -v "$repo/deploy/patches/pyrowave:/patches:ro" -v "$here:/check:ro" \
  "${IMAGE:-quasar-agent-dev:latest}" bash -euo pipefail -c '
cd /tmp
git clone -q https://github.com/Themaister/pyrowave.git && cd pyrowave && git checkout -q "$PYROWAVE_REF"
bash checkout_granite.sh >/dev/null 2>&1
[ "$(git -C Granite rev-parse HEAD)" = "$GRANITE_REF" ]
if [ "$APPLY" = 1 ]; then for p in /patches/*.patch; do git -C Granite apply -v "$p"; done; fi
cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/tmp/pw -DCMAKE_INSTALL_LIBDIR=lib64 >/dev/null
cmake --build build --target pyrowave-shared pyrowave-device-validation >/dev/null
cmake --install build >/dev/null
g++ -std=c++17 -O1 -I/tmp/pw/include /check/borrowed_wait.cpp -L/tmp/pw/lib64 -lpyrowave-shared -lvulkan -o /tmp/borrowed_wait
dnf install -y -q vulkan-validation-layers >/dev/null
rc=0; LD_LIBRARY_PATH=/tmp/pw/lib64 VK_INSTANCE_LAYERS=VK_LAYER_KHRONOS_validation /tmp/borrowed_wait >/tmp/out 2>&1 || rc=$?
cat /tmp/out
# VUID-vkCmdDraw-None-09600 is the decoder fragment path on lavapipe, unpatched too. Dropping the
# wait in the patch shows as VUID-vkDestroyBuffer-buffer-00922 (destroyed while in use).
if grep -o "Validation Error: \[ [^ ]*" /tmp/out | grep -v VUID-vkCmdDraw-None-09600; then echo "FAIL: validation errors"; exit 1; fi
exit $rc'
