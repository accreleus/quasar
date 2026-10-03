#!/usr/bin/env bash
# Builds the engine-mode suite (#408) and leaves its binary where the host can run it:
# .diagnostics/engine-suite/engine-suite. It links only libc, so the same file runs on
# this machine and on a lab host (docs/testing-engine-suite.md).
source scripts/verify/common.sh
# This container runs as root over the bind-mounted tree. The output is the invoking
# user's (DEVTOOLS_UID/GID, set by scripts/verify.sh), so later targets that write under
# .diagnostics as that user still can (#442). An earlier run's root-owned directories on
# this path are handed back too: only these two, never recursively.
uid="${DEVTOOLS_UID:-0}"; gid="${DEVTOOLS_GID:-0}"
out=/workspace/.diagnostics/engine-suite
for d in /workspace/.diagnostics "$out"; do
  [ -d "$d" ] || install -d -m 0755 "$d"
  chown "$uid:$gid" "$d"
done
cd node-agent
exe="$(cargo test -p quasar-node-agent --test engine_suite --no-run --message-format=json \
  | python3 -c 'import json, sys
for line in sys.stdin:
    m = json.loads(line)
    if m.get("reason") == "compiler-artifact" and m["target"]["name"] == "engine_suite" and m.get("executable"):
        print(m["executable"])')"
[ -n "$exe" ] || { echo "FAIL — no engine_suite executable was built" >&2; exit 1; }
install -m 0755 -o "$uid" -g "$gid" "$exe" "$out/engine-suite"
pass "engine suite built: .diagnostics/engine-suite/engine-suite"
