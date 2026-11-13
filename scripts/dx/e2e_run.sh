#!/usr/bin/env bash
# make e2e-run: runs a plan of the end-to-end streaming suite (docs/testing-e2e-suite.md) from
# this machine against a stack. Needs Node 22 and a Chrome; no admin token, no ssh.
# env: TARGET PLAN [CDP] [OUT] [CHROME] [CLIENT=<label>] [HOOKS=<dir>, or QUASAR_E2E_HOOKS] [WEB_DIST=<dir>, or QUASAR_E2E_WEB_DIST] [INSECURE_TLS=1], and the test user's
# QUASAR_E2E_EMAIL / QUASAR_E2E_PASSWORD (or QUASAR_E2E_CRED_FILE). Ends with one RESULT line;
# exit 0 ok, 1 a scenario failed or nothing passed, 2 usage or a bundle that failed its check.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VALIDATE="$ROOT/scripts/validate"

usage() {
  echo "e2e-run: $1" >&2
  echo "usage: make e2e-run TARGET=<stack url> PLAN=<smoke|file> [CDP=<DevTools http endpoint>] [OUT=<dir>] [CLIENT=<label>] [HOOKS=<dir>] [WEB_DIST=<dir>] [INSECURE_TLS=1]" >&2
  echo "RESULT status=error target=e2e-run bundle= pass=0 fail=0 error=0 skipped=0"
  exit 2
}

[ -n "${TARGET:-}" ] || usage "TARGET is required"
[ -n "${PLAN:-}" ] || usage "PLAN is required"
command -v node >/dev/null 2>&1 || usage "node is not installed"

# npm ci, as validate.sh does: never rewrite the lockfile. Not needed to attach (CDP=) to a browser
# that is already there, but playwright is what attaches.
if [ ! -d "$VALIDATE/node_modules" ]; then
  echo "installing scripts/validate node deps (npm ci, first run)" >&2
  # The sign-in is for the runner alone: npm and its install scripts do not get it.
  (cd "$VALIDATE" && env -u QUASAR_E2E_EMAIL -u QUASAR_E2E_PASSWORD -u QUASAR_E2E_CRED_FILE -u NODE_TLS_REJECT_UNAUTHORIZED npm ci >&2)
fi

args=(--target "$TARGET" --plan "$PLAN")
[ -z "${CDP:-}" ] || args+=(--cdp "$CDP")
[ -z "${OUT:-}" ] || args+=(--out "$OUT")
[ -z "${HOOKS:-}" ] || args+=(--hooks "$HOOKS")
[ -z "${WEB_DIST:-}" ] || args+=(--web-dist "$WEB_DIST")
# Certificate checks are on unless INSECURE_TLS=1, whatever NODE_TLS_REJECT_UNAUTHORIZED says here: the runner removes it itself.
exec node "$VALIDATE/stream/run.mjs" "${args[@]}"
