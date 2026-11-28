#!/usr/bin/env bash
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit
exec bash scripts/verify/test-control-entrypoint.sh
