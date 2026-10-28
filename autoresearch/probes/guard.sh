#!/usr/bin/env bash
# Round-3 first-open guard: game page and session view, the eager baseline (dev) vs <label>,
# interleaved in one window. Fails (exit 1) when either is more than 50 ms above dev.
#   bash autoresearch/probes/guard.sh <label>    (needs autoresearch/harness/out/{dev,<label>}.dist)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
label=${1:?label}; O=autoresearch/harness/out
for _ in $(seq 60); do awk '{exit !($1 < 4)}' /proc/loadavg && break; sleep 10; done
docker run --rm --ipc=host --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD":/w -w /w/autoresearch/harness \
  mcr.microsoft.com/playwright:v1.56.0-noble sh -c "
  node server.mjs --dist /w/$O/dev.dist --port 4901 2>/dev/null &
  node server.mjs --dist /w/$O/$label.dist --port 4900 2>/dev/null &
  sleep 1; node /w/autoresearch/probes/first-open.mjs --a http://127.0.0.1:4901 --b http://127.0.0.1:4900 --runs 7" >"$O/$label.firstopen.json"
python3 - "$O/$label.firstopen.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
ok = all(d[s]["b"] - d[s]["a"] <= 50 for s in ("game", "session"))
print(f"first-open game {d['game']['a']}->{d['game']['b']} session {d['session']['a']}->{d['session']['b']} guard={'pass' if ok else 'FAIL'}")
sys.exit(0 if ok else 1)
PY
