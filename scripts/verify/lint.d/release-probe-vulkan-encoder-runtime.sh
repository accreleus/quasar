#!/usr/bin/env bash
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit
exec bash scripts/release/test-probe-vulkan-encoder-runtime.sh
