#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."/web || exit
[ -x node_modules/.bin/openapi-typescript ] || npm ci --no-audit --no-fund
source ../scripts/verify/web-checks.sh
schema_drift_check
echo "web/src/api/schema.d.ts matches protocol/openapi.yaml"
