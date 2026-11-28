#!/usr/bin/env bash
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit
bash scripts/verify/test-shellcheck.sh || exit
exec bash scripts/verify/shellcheck.sh
