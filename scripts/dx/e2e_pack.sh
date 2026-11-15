#!/usr/bin/env bash
# make e2e-pack: writes the client half of the end-to-end suite as one archive (docs/testing-e2e-suite.md).
# env: OUT (optional: the archive's path without its extension; makes OUT.tar.gz and OUT.zip).
# Ends with one RESULT line; exit 0 ok, 1 otherwise.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v node >/dev/null 2>&1 || { echo "e2e-pack: node is not installed" >&2; echo "RESULT status=error target=e2e-pack tar= zip= files=0"; exit 2; }
args=()
[ -z "${OUT:-}" ] || args+=(--out "$OUT")
exec node "$ROOT/scripts/validate/stream/pack.mjs" "${args[@]}"
