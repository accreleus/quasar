#!/usr/bin/env bash
# scripts/harness/run-ref-recovery.sh — reference recovery bitstream check (#1348) for the
# patched Vulkan encoders. Per codec: encode a moving test pattern with num-anchors=3 while
# a script asks for anchors and recoveries (node-agent test
# session::pipeline::ref_recovery::tests::capture_for_harness), then check the capture with
# ref_recovery_check.py: the pruned stream (lost frames dropped) decodes with no error and
# bit-identical to the full stream, and every frame names the reference it should.
#
# Runs on a host with a Vulkan encode GPU, from the repo root:
#   scripts/harness/run-ref-recovery.sh <out-dir> [av1] [h264] [h265]
# Env:
#   IMAGE     dev image with the patched GStreamer (default quasar-agent-dev:latest)
#   GPU_ARGS  docker device args (default: --device /dev/dri;
#             NVIDIA: "--gpus all -e NVIDIA_DRIVER_CAPABILITIES=all")
#   FFMPEG    ffmpeg command for the checker (default: the linuxserver/ffmpeg image)
set -euo pipefail

out=$(realpath -m "${1:?usage: $0 <out-dir> [codec...]}")
shift
codecs=("$@")
[ ${#codecs[@]} -gt 0 ] || codecs=(av1 h264 h265)
image=${IMAGE:-quasar-agent-dev:latest}
read -r -a gpu <<<"${GPU_ARGS:---device /dev/dri}"
mkdir -p "$out"

for c in "${codecs[@]}"; do
  echo "== capture $c"
  docker run --rm "${gpu[@]}" \
    -v "$PWD":/workspace -v "$out":/out -w /workspace/node-agent \
    -e QUASAR_REF_RECOVERY_OUT=/out -e QUASAR_REF_RECOVERY_CODEC="$c" \
    "$image" sh -c "cargo test --lib session::pipeline::ref_recovery::tests::capture_for_harness \
      -- --ignored --exact --nocapture && chown -R $(id -u):$(id -g) /out"
done

rc=0
for c in "${codecs[@]}"; do
  python3 scripts/harness/ref_recovery_check.py "$out" "$c" || rc=1
done
exit "$rc"
