#!/usr/bin/env bash
# scripts/harness/run-ref-recovery.sh — reference recovery bitstream check (#1348) for the
# patched Vulkan (default) or NVENC encoders. Per codec: encode a moving test pattern with num-anchors=3 while
# a script asks for anchors and recoveries (node-agent test
# session::pipeline::ref_recovery::tests::capture_for_harness), then check the capture with
# ref_recovery_check.py: the pruned stream (lost frames dropped) decodes with no error and
# bit-identical to the full stream, and every frame names the reference it should.
#
# Runs on a host with an encode GPU, from the repo root:
#   scripts/harness/run-ref-recovery.sh [--encoder vulkan|nvenc] <out-dir> [av1] [h264] [h265]
# A codec whose encoder element this GPU does not register is skipped, and said so.
# Env:
#   IMAGE     dev image with the patched GStreamer (default quasar-agent-dev:latest)
#   GPU_ARGS  docker device args (default: --device /dev/dri;
#             NVIDIA: "--gpus all -e NVIDIA_DRIVER_CAPABILITIES=all")
#   FFMPEG    ffmpeg command for the checker (default: the linuxserver/ffmpeg image)
#   QUASAR_REF_RECOVERY_{ANCHORS,GOP,SIZE,SLOTS}  passed to the capture (its doc comment)
set -euo pipefail

encoder=vulkan
if [ "${1:-}" = --encoder ]; then
  encoder=${2:?--encoder vulkan|nvenc}
  shift 2
fi
case "$encoder" in
  vulkan) prefix=vulkan ;;
  nvenc) prefix=nv ;; # CUDA mode: nvh264enc etc. on GStreamer 1.28
  *) echo "--encoder $encoder: vulkan or nvenc" >&2; exit 2 ;;
esac
out=$(realpath -m "${1:?usage: $0 [--encoder vulkan|nvenc] <out-dir> [codec...]}")
shift
codecs=("$@")
[ ${#codecs[@]} -gt 0 ] || codecs=(av1 h264 h265)
image=${IMAGE:-quasar-agent-dev:latest}
read -r -a gpu <<<"${GPU_ARGS:---device /dev/dri}"
mkdir -p "$out"

knobs=()
for k in ANCHORS GOP SIZE SLOTS; do
  v=QUASAR_REF_RECOVERY_$k
  [ -z "${!v:-}" ] || knobs+=(-e "$v=${!v}")
done

captured=()
for c in "${codecs[@]}"; do
  el=$prefix${c}enc
  echo "== capture $c ($el)"
  rc=0
  # exit 77: the element is not registered on this GPU
  docker run --rm "${gpu[@]}" \
    -v "$PWD":/workspace -v "$out":/out -w /workspace/node-agent \
    -e QUASAR_REF_RECOVERY_OUT=/out -e QUASAR_REF_RECOVERY_CODEC="$c" \
    -e QUASAR_REF_RECOVERY_ENCODER="$encoder" "${knobs[@]}" \
    "$image" sh -c "gst-inspect-1.0 --exists $el || exit 77; \
      cargo test --lib session::pipeline::ref_recovery::tests::capture_for_harness \
      -- --ignored --exact --nocapture && chown -R $(id -u):$(id -g) /out" || rc=$?
  case $rc in
    0) captured+=("$c") ;;
    77) echo "SKIP $c: this GPU registers no $el" ;;
    *) echo "FAIL $c: capture exited $rc"; exit "$rc" ;;
  esac
done

rc=0
for c in "${captured[@]}"; do
  python3 scripts/harness/ref_recovery_check.py "$out" "$c" || rc=1
done
[ ${#captured[@]} -gt 0 ] || { echo "FAIL: nothing captured"; rc=1; }
exit "$rc"
