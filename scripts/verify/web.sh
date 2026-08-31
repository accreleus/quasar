#!/usr/bin/env bash
source scripts/verify/common.sh
cd web
run "web dependency install" npm ci

# web/src/api/schema.d.ts is GENERATED from protocol/openapi.yaml (`npm run
# gen:api`, openapi-typescript) but also checked in, so nothing catches it
# going stale — this is the web-side counterpart to Go's TestOpenAPIDrift
# (control-plane/cmd/quasar-control/openapi_drift_test.go), which reads the
# same protocol/openapi.yaml and fails the same "un-init'd submodule" way.
# Runs right after `npm ci` (before typecheck/tests/build) so a stale commit
# fails as fast as possible.
#
# MUST STAY IN SYNC with the `gen:api` script in web/package.json (spec path,
# generator flags). Not driven through `npm run gen:api -- -o <tmp>` because
# openapi-typescript's CLI does not support two `-o` flags on one invocation —
# it crashes with ERR_INVALID_ARG_TYPE ("path argument must be a string,
# received Array") rather than letting the later one win.
#
# gen1/gen2 are script-scope (not `local`) so the EXIT trap below can still
# see them after schema_drift_check returns — a RETURN trap can't: bash pops a
# function's locals before running a RETURN trap body, so a trap referencing
# them there dies with "unbound variable" under set -u.
gen1=""
trap 'rm -f "$gen1"' EXIT
schema_drift_check() {
  local spec="../protocol/openapi.yaml"
  if [ ! -f "$spec" ]; then
    echo "read protocol/openapi.yaml: no such file or directory (git submodule update --init protocol)" >&2
    return 1
  fi

  # Call the installed binary directly, not bare `npx openapi-typescript`:
  # npx silently auto-installs the package from the registry (unpinned
  # version, network dependency) whenever node_modules is missing or
  # partial, which would make this gate report "stale" against generator
  # output that isn't even the pinned version.
  local bin="./node_modules/.bin/openapi-typescript"
  if [ ! -x "$bin" ]; then
    echo "$bin not found — run npm ci (openapi-typescript is a devDependency)" >&2
    return 1
  fi

  gen1="$(mktemp -t quasar-schema-check-XXXXXX.d.ts)"
  if ! "$bin" "$spec" -o "$gen1" >/dev/null; then
    return 1
  fi

  if ! diff -q "$gen1" src/api/schema.d.ts >/dev/null; then
    echo "web/src/api/schema.d.ts is stale vs protocol/openapi.yaml — run npm run gen:api and commit" >&2
    echo "--- diff (schema.d.ts vs freshly generated, truncated to 40 lines) ---" >&2
    diff -u src/api/schema.d.ts "$gen1" | head -40 >&2
    return 1
  fi
}
run "web schema.d.ts drift vs protocol/openapi.yaml" schema_drift_check

run "web typecheck" npm run typecheck
run "web unit tests" npm test -- run
run "web production build" npm run build
