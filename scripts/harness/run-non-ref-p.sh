#!/usr/bin/env bash
# scripts/harness/run-non-ref-p.sh — non-reference P bitstream check (`non-ref-p`; `nonref-p` on NVENC) (#1322) on the patched Vulkan
# (default) or NVENC H.264 encoder: encode a moving test pattern with every second P frame a
# non-reference frame, then check with non_ref_p_check.py that those frames have nal_ref_idc 0
# and that the stream without them decodes cleanly and bit-identical for every other frame.
#
# Runs on a host with an encode GPU, from the repo root:
#   scripts/harness/run-non-ref-p.sh [--encoder vulkan|nvenc] <out-dir>
# Env: IMAGE, GPU_ARGS, FFMPEG as in run-ref-recovery.sh.
set -euo pipefail

encoder=vulkan
if [ "${1:-}" = --encoder ]; then
  encoder=${2:?--encoder vulkan|nvenc}
  shift 2
fi
out=$(realpath -m "${1:?usage: $0 [--encoder vulkan|nvenc] <out-dir>}")
image=${IMAGE:-quasar-agent-dev:latest}
read -r -a gpu <<<"${GPU_ARGS:---device /dev/dri}"
case "$encoder" in
  vulkan) enc="vulkanupload ! vulkanh264enc non-ref-p=true idr-period=1000 num-ref-frames=1 b-frames=0" ;;
  nvenc) enc="nvh264enc nonref-p=true gop-size=1000 preset=p4 tune=ultra-low-latency rc-mode=cbr bframes=0" ;;
  *) echo "--encoder $encoder: vulkan or nvenc" >&2; exit 2 ;;
esac
mkdir -p "$out"

docker run --rm "${gpu[@]}" -v "$out":/out "$image" sh -c "\
  gst-launch-1.0 -q videotestsrc num-buffers=240 pattern=smpte horizontal-speed=3 \
    ! video/x-raw,format=NV12,width=1280,height=720,framerate=60/1 ! $enc \
    ! video/x-h264,profile=constrained-baseline ! h264parse config-interval=-1 \
    ! video/x-h264,stream-format=byte-stream,alignment=au ! filesink location=/out/h264.annexb \
  && chown -R $(id -u):$(id -g) /out"

python3 scripts/harness/non_ref_p_check.py "$out/h264.annexb"
