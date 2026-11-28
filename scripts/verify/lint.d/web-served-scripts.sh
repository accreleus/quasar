#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."/web || exit
[ -x node_modules/.bin/vite ] || npm ci --no-audit --no-fund
npm run build
source ../scripts/verify/web-checks.sh
served_scripts_check
echo "web/dist serves enroll-host.sh and not prepare-host.sh"
