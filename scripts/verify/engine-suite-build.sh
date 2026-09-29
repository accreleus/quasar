#!/usr/bin/env bash
# Builds the engine-mode suite (#408) and leaves its binary where the host can run it:
# .diagnostics/engine-suite/engine-suite. It links only libc, so the same file runs on
# this machine and on a lab host (docs/testing-engine-suite.md).
source scripts/verify/common.sh
out=/workspace/.diagnostics/engine-suite
mkdir -p "$out"
cd node-agent
exe="$(cargo test -p quasar-node-agent --test engine_suite --no-run --message-format=json \
  | python3 -c 'import json, sys
for line in sys.stdin:
    m = json.loads(line)
    if m.get("reason") == "compiler-artifact" and m["target"]["name"] == "engine_suite" and m.get("executable"):
        print(m["executable"])')"
[ -n "$exe" ] || { echo "FAIL — no engine_suite executable was built" >&2; exit 1; }
install -m 0755 "$exe" "$out/engine-suite"
pass "engine suite built: .diagnostics/engine-suite/engine-suite"
