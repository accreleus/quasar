#!/usr/bin/env bash
# macOS and Linux launcher. All the work is in the Node entry; see README.txt.
cd "$(dirname "$0")" || exit 2
command -v node >/dev/null 2>&1 || { echo "Node 22 is needed: https://nodejs.org" >&2; echo "RESULT status=error target=e2e-pack zip= reason=no-node"; exit 2; }
exec node scripts/validate/stream/pack-run.mjs "$@"
