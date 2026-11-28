#!/usr/bin/env bash
# ESLint over web/ (web/eslint.config.js). eslint-suppressions.json counts the findings
# that predate the gate: a new one fails, and so does a count left higher than the code
# (after fixing one: `npm run lint -- --prune-suppressions`).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."/web || exit
[ -x node_modules/.bin/eslint ] || npm ci --no-audit --no-fund
node --test eslint.config.test.mjs
npm run lint
echo "web/ passes ESLint (no finding beyond web/eslint-suppressions.json)"
