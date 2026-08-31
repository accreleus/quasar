#!/usr/bin/env bash
# GPU-attached, local-only release probe for GStreamer's dynamic Vulkan encoders.
#
# `vulkanh264enc` is not a static plugin feature.  GStreamer registers it only
# while scanning a real Vulkan device that advertises H.264 Video Encode.  Do
# not use a GPU-free `gst-inspect` result to judge this image.
set -euo pipefail

image="${QUASAR_VULKAN_IMAGE:-}"
output=""
render_node=""
require_h265=0

usage() {
  cat <<'EOF'
usage: scripts/release/probe-vulkan-encoder-runtime.sh --image NAME@sha256:DIGEST --render-node /dev/dri/renderDNNN --output DIR [--require-h265]

Runs a local, disposable GPU/CDI-equivalent container only.  It never pulls,
builds, publishes, deploys, or changes the running Compose stack.

The image must be an exact name@sha256:<64 lowercase hex> reference already
present locally. `--render-node` is an existing host DRM render node and is
passed into the disposable container. H.264 registration and a bounded H.264
encoder initialization are required. H.265 is recorded PASS or SKIP because
Quasar's browser release path is H.264; --require-h265 turns a missing H.265
capability into failure.

This is a registration/init probe, not streaming acceptance: GStreamer does
not expose a stable mapping from its selected Vulkan device index to this DRM
node. A session must still prove selected-node identity, source frames, RTP,
browser decode, and sustained lifecycle behavior.
EOF
}

while (($#)); do
  case "$1" in
    --image) image=${2:?--image needs value}; shift 2 ;;
    --render-node) render_node=${2:?--render-node needs value}; shift 2 ;;
    --output) output=${2:?--output needs directory}; shift 2 ;;
    --require-h265) require_h265=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

[[ -n "$image" ]] || { echo "FAIL: missing --image (or QUASAR_VULKAN_IMAGE)" >&2; exit 2; }
[[ -n "$output" ]] || { echo "FAIL: missing --output" >&2; exit 2; }
[[ -n "$render_node" ]] || { echo "FAIL: missing --render-node" >&2; exit 2; }
[[ "$render_node" =~ ^/dev/dri/renderD[0-9]+$ ]] || {
  echo "FAIL: --render-node must be /dev/dri/renderDNNN, got $render_node" >&2; exit 2;
}

# Bash regex is POSIX ERE, not PCRE: keep every group capturing here.
repo='([a-z0-9][a-z0-9.-]*(:[0-9]+)?/)?([a-z0-9]+([._-][a-z0-9]+)*/)*[a-z0-9]+([._-][a-z0-9]+)*'
if ! [[ "$image" =~ ^${repo}@sha256:[0-9a-f]{64}$ ]]; then
  echo "FAIL: --image must be exact name@sha256:<64 lowercase hex>, got $image" >&2
  exit 2
fi

mkdir -p "$output"
output="$(cd "$output" && pwd)"
report="$output/report.txt"

fail() {
  if [[ -f "$report" ]]; then
    printf 'result=FAIL\nreason=%s\n' "$1" >>"$report"
  else
    printf 'result=FAIL\nreason=%s\nimage=%s\n' "$1" "$image" >"$report"
  fi
  echo "FAIL: $1" >&2
  exit 1
}

command -v docker >/dev/null || fail "docker command unavailable"
if [[ "${QUASAR_PROBE_TEST_SKIP_RENDER_NODE_CHECK:-}" != 1 && ! -c "$render_node" ]]; then
  fail "selected render node is not a host character device: $render_node"
fi
docker image inspect "$image" >"$output/local-image-inspect.json" 2>"$output/local-image-inspect.stderr" || \
  fail "candidate digest is not present locally; refusing pull"

# Each invocation has a new registry so a prior no-GPU cache cannot suppress
# per-device dynamic feature registration. These flags retain required GPU/CDI,
# selected DRM node, NVIDIA capability, and seccomp conditions while isolating
# probe from service: no volumes, no host network, `docker run --rm`.
run_gpu() {
  local name=$1 command=$2
  docker run --rm --pull=never --network none --gpus all --device "$render_node" \
    --security-opt seccomp=unconfined \
    -e NVIDIA_DRIVER_CAPABILITIES=all \
    -e QUASAR_RENDER_NODE="$render_node" \
    -e GST_REGISTRY="/tmp/quasar-vulkan-probe-${name}-$$.bin" \
    -e GST_DEBUG='GST_PLUGIN_LOADING:4,vulkan*:6' \
    --entrypoint /bin/bash "$image" -lc "$command"
}

{
  echo "image=$image"
  echo "probe=local-disposable-gpu-runtime"
  echo "render_node=$render_node"
  echo "scope=registration-and-init-only; selected Vulkan-to-DRM identity unverified"
  echo "selected_gpu_identity=UNVERIFIED"
  echo "h264=UNKNOWN"
  echo "h264_init=UNKNOWN"
  echo "h265=UNKNOWN"
} >"$report"

run_gpu diagnostics '
  set -o pipefail
  uname -m
  echo "QUASAR_RENDER_NODE=$QUASAR_RENDER_NODE"
  ls -l "$QUASAR_RENDER_NODE"
  gst-inspect-1.0 --version
  gst-inspect-1.0 vulkan
' >"$output/diagnostics.log" 2>&1 || fail "Vulkan runtime diagnostic failed; see diagnostics.log"

if run_gpu h264 'gst-inspect-1.0 vulkanh264enc' >"$output/vulkanh264enc.log" 2>&1; then
  sed -i.bak 's/^h264=UNKNOWN$/h264=PASS/' "$report" && rm -f "$report.bak"
else
  sed -i.bak 's/^h264=UNKNOWN$/h264=FAIL/' "$report" && rm -f "$report.bak"
  fail "vulkanh264enc not registered on attached GPU; see vulkanh264enc.log and diagnostics.log"
fi

if run_gpu h264-init '
  timeout -k 5s 30s gst-launch-1.0 -q \
    videotestsrc num-buffers=1 ! video/x-raw,format=NV12,width=128,height=72,framerate=30/1 ! \
    vulkanupload ! vulkanh264enc ! fakesink sync=false
' >"$output/vulkanh264enc-init.log" 2>&1; then
  sed -i.bak 's/^h264_init=UNKNOWN$/h264_init=PASS/' "$report" && rm -f "$report.bak"
else
  sed -i.bak 's/^h264_init=UNKNOWN$/h264_init=FAIL/' "$report" && rm -f "$report.bak"
  fail "vulkanh264enc failed bounded initialization; see vulkanh264enc-init.log and diagnostics.log"
fi

if run_gpu h265 'gst-inspect-1.0 vulkanh265enc' >"$output/vulkanh265enc.log" 2>&1; then
  sed -i.bak 's/^h265=UNKNOWN$/h265=PASS/' "$report" && rm -f "$report.bak"
else
  if ((require_h265)); then
    sed -i.bak 's/^h265=UNKNOWN$/h265=FAIL/' "$report" && rm -f "$report.bak"
    fail "vulkanh265enc not registered on attached GPU; --require-h265 was set; see vulkanh265enc.log"
  fi
  sed -i.bak 's/^h265=UNKNOWN$/h265=SKIP (GPU did not register optional H.265 Vulkan Video encode)/' "$report" && rm -f "$report.bak"
fi

printf 'result=PASS\n' >>"$report"
echo "Vulkan runtime encoder probe: PASS ($report)"
