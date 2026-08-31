#!/usr/bin/env bash
# Offline contract tests. Mock Docker proves grammar, no-pull image lookup,
# H.264 failure semantics, and optional-vs-required H.265 behavior.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$root/scripts/release/probe-vulkan-encoder-runtime.sh"
tmp="$(mktemp -d /tmp/quasar-vulkan-probe-test.XXXXXX)"
digest="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
image="registry.example:5000/quasar/vulkan@sha256:$digest"

cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"
if [[ "$1 $2" == "image inspect" ]]; then
  [[ "${MOCK_IMAGE_PRESENT:-1}" == 1 ]] || exit 1
  echo '{"Id":"mock"}'
  exit 0
fi
if [[ "$1" == run ]]; then
  command=${*: -1}
  case "$command" in
    *vulkanh264enc*) [[ "${MOCK_H264:-1}" == 1 ]] && echo H264 || { echo 'No such element or plugin' >&2; exit 1; } ;;
    *vulkanh265enc*) [[ "${MOCK_H265:-1}" == 1 ]] && echo H265 || { echo 'No such element or plugin' >&2; exit 1; } ;;
    *) echo diagnostics ;;
  esac
  exit 0
fi
exit 99
EOF
chmod +x "$tmp/bin/docker"

run() {
  local name=$1; shift
  PATH="$tmp/bin:$PATH" MOCK_DOCKER_LOG="$tmp/$name.docker.log" \
    QUASAR_PROBE_TEST_SKIP_RENDER_NODE_CHECK=1 "$@"
}

# Grammar failure must reach no Docker action.
if run grammar "$script" --image quasar-node-agent:latest --render-node /dev/dri/renderD128 --output "$tmp/grammar"; then
  echo 'tag unexpectedly accepted' >&2; exit 1
fi
test ! -e "$tmp/grammar.docker.log"

run h264_only env MOCK_H264=1 MOCK_H265=0 "$script" --image "$image" --render-node /dev/dri/renderD128 --output "$tmp/h264-only"
grep -Fx 'h264=PASS' "$tmp/h264-only/report.txt"
grep -Fx 'h264_init=PASS' "$tmp/h264-only/report.txt"
grep -Fx 'h265=SKIP (GPU did not register optional H.265 Vulkan Video encode)' "$tmp/h264-only/report.txt"
grep -F -- '--pull=never --network none --gpus all --device /dev/dri/renderD128' "$tmp/h264_only.docker.log" >/dev/null
grep -F -- '-e QUASAR_RENDER_NODE=/dev/dri/renderD128' "$tmp/h264_only.docker.log" >/dev/null
grep -Fx 'scope=registration-and-init-only; selected Vulkan-to-DRM identity unverified' "$tmp/h264-only/report.txt"

if run missing_h264 env MOCK_H264=0 MOCK_H265=0 "$script" --image "$image" --render-node /dev/dri/renderD128 --output "$tmp/missing-h264"; then
  echo 'missing H264 unexpectedly accepted' >&2; exit 1
fi
grep -Fx 'h264=FAIL' "$tmp/missing-h264/report.txt"
grep -F 'vulkanh264enc not registered on attached GPU' "$tmp/missing-h264/report.txt"

if run require_h265 env MOCK_H264=1 MOCK_H265=0 "$script" --image "$image" --render-node /dev/dri/renderD128 --output "$tmp/require-h265" --require-h265; then
  echo 'missing required H265 unexpectedly accepted' >&2; exit 1
fi
grep -Fx 'h265=FAIL' "$tmp/require-h265/report.txt"

run h265_pass env MOCK_H264=1 MOCK_H265=1 "$script" --image "$image" --render-node /dev/dri/renderD128 --output "$tmp/h265-pass"
grep -Fx 'h265=PASS' "$tmp/h265-pass/report.txt"

if run missing_image env MOCK_IMAGE_PRESENT=0 "$script" --image "$image" --render-node /dev/dri/renderD128 --output "$tmp/missing-image"; then
  echo 'missing local image unexpectedly accepted' >&2; exit 1
fi
grep -F 'candidate digest is not present locally; refusing pull' "$tmp/missing-image/report.txt"
test "$(wc -l <"$tmp/missing_image.docker.log")" -eq 1

run image_env env QUASAR_VULKAN_IMAGE="$image" MOCK_H264=1 MOCK_H265=1 "$script" --render-node /dev/dri/renderD128 --output "$tmp/image-env"
grep -Fx "image=$image" "$tmp/image-env/report.txt"

echo 'Vulkan runtime encoder probe contract: PASS'
