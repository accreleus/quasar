#!/usr/bin/env bash
set -euo pipefail

browser=""
for candidate in chromium chromium-browser; do
  if command -v "$candidate" >/dev/null 2>&1; then
    browser="$candidate"
    break
  fi
done

if [ -z "$browser" ]; then
  echo "chromium executable not found" >&2
  exit 127
fi

export XDG_CACHE_HOME="${XDG_CACHE_HOME:-/tmp/quasar-diagnostics-cache}"
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-/tmp/quasar-diagnostics-config}"
LOG_FILE="${QUASAR_DIAGNOSTICS_LOG:-/run/quasar-agent/quasar-diagnostics-app.log}"

mkdir -p "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME"
touch "$LOG_FILE" 2>/dev/null || LOG_FILE=/tmp/quasar-diagnostics-app.log

{
  echo "starting diagnostics app"
  echo "browser=$browser"
  echo "XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-}"
  echo "WAYLAND_DISPLAY=${WAYLAND_DISPLAY:-}"
  echo "QUASAR_DIAGNOSTICS_URL=${QUASAR_DIAGNOSTICS_URL:-file:///opt/quasar/diagnostics/quasar-stream-diagnostics.html}"
  ls -l /dev/dri 2>/dev/null || true
} >>"$LOG_FILE" 2>&1

# GPU selection: this Fedora Chromium's ANGLE OpenGL-ES backend
# (`--use-angle=opengles`) and the `--use-gl=egl-angle` implementation both make
# the browser launch its GPU child with `--use-gl=disabled`; because the build's
# allowed-implementations list is hardware-`egl-angle`-only (no software entry),
# that child is rejected ("Requested GL implementation (gl=none,angle=none) not
# found ... Exiting GPU process") and Chromium crash-loops the GPU process, then
# falls back to CPU rasterization (~12 fps). The ANGLE *desktop-GL* backend
# (`--use-gl=angle --use-angle=gl`) keeps the GPU process alive on real hardware
# on both AMD and NVIDIA hosts. Verified on Tower (RTX 5090 + AMD iGPU): with the
# NVIDIA glvnd EGL vendor pinned below, ANGLE reports
# "ANGLE (NVIDIA GeForce RTX 5090 ...)" and the GPU process stays up.
# (History: prior fixes only toggled --use-gl while keeping the poisoned
# --use-angle=opengles, so every attempt still produced gl=none.)
if [ "${QUASAR_DIAGNOSTICS_PREFER_NVIDIA:-auto}" != "0" ] \
   && [ -e /usr/share/glvnd/egl_vendor.d/10_nvidia.json ] \
   && grep -qs 'DRIVER=nvidia' /sys/class/drm/renderD*/device/uevent 2>/dev/null; then
  # Pin ANGLE's EGL to the NVIDIA vendor so the discrete GPU drives rendering
  # instead of a co-present Mesa iGPU (glvnd would otherwise default to Mesa).
  export __NV_PRIME_RENDER_OFFLOAD=1
  export __GLX_VENDOR_LIBRARY_NAME=nvidia
  export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
  echo "diagnostics: pinned ANGLE EGL to NVIDIA vendor" >>"$LOG_FILE"
fi

browser_args=(
  --ozone-platform=wayland \
  --ozone-platform-hint=wayland \
  --enable-features=UseOzonePlatform \
  --use-gl=angle \
  --use-angle=gl \
  --kiosk \
  --start-fullscreen \
  --no-first-run \
  --no-default-browser-check \
  --disable-dev-shm-usage \
  --disable-session-crashed-bubble \
  --disable-infobars \
  --disable-translate \
  --autoplay-policy=no-user-gesture-required \
  --user-data-dir=/tmp/quasar-diagnostics-profile \
  --no-sandbox \
  "${QUASAR_DIAGNOSTICS_URL:-file:///opt/quasar/diagnostics/quasar-stream-diagnostics.html}"
)

if [ "${QUASAR_DIAGNOSTICS_SOFTWARE_RENDERING:-0}" = "1" ]; then
  browser_args=(
    --disable-gpu
    --disable-vulkan
    --disable-gpu-compositing
    --disable-software-rasterizer=false
    "${browser_args[@]}"
  )
fi

set +e
if command -v dbus-run-session >/dev/null 2>&1; then
  dbus-run-session -- "$browser" "${browser_args[@]}" >>"$LOG_FILE" 2>&1
else
  "$browser" "${browser_args[@]}" >>"$LOG_FILE" 2>&1
fi
status=$?
set -e

echo "browser exited with status=$status" >>"$LOG_FILE"

if [ "${QUASAR_DIAGNOSTICS_KEEPALIVE_ON_EXIT:-0}" = "1" ]; then
  echo "keeping diagnostics container alive after browser exit" >>"$LOG_FILE"
  tail -f /dev/null
fi

exit "$status"
