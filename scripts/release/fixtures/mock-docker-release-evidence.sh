#!/usr/bin/env bash
# Minimal Docker fixture for release artifact evidence contract tests.
set -euo pipefail

case "${1:-} ${2:-}" in
  "image inspect")
    if [[ "${3:-}" != "--format" ]]; then
      echo "unexpected image inspect arguments" >&2
      exit 2
    fi
    image=${5:?missing image}
    case "${4:-}" in
      '{{json .RepoDigests}}') printf '["%s"]\n' "$image" ;;
      '{{.Id}}') printf 'sha256:mock-image-id\n' ;;
      *) echo "unexpected inspect format: ${4:-}" >&2; exit 2 ;;
    esac
    ;;
  "image save")
    [[ "${3:-}" == "--output" ]] || { echo "missing --output" >&2; exit 2; }
    : >"${4:?missing archive output}"
    ;;
  run*)
    if [[ "${MOCK_DOCKER_FAIL_RUN:-}" == "1" ]]; then
      echo "mock scanner failure" >&2
      exit 91
    fi
    printf '{"bomFormat":"CycloneDX","components":[]}\n'
    ;;
  *)
    echo "unexpected Docker fixture call: $*" >&2
    exit 2
    ;;
esac
