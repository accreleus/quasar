#!/usr/bin/env bash
# Every actions/checkout in .github/workflows sets `persist-credentials: false` (#877), so the
# job token is not left in .git/config for later steps (or a container that mounts the tree).
# A checkout whose job runs `git fetch` afterwards says `persist-credentials: true` explicitly.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
python3 - "$repo/.github/workflows" <<'PY'
import glob, sys
bad = []
for f in sorted(glob.glob(sys.argv[1] + "/*.yml")):
    L = open(f).read().split("\n")
    for k, l in enumerate(L):
        if "actions/checkout@" not in l:
            continue
        j = k
        while not L[j].lstrip().startswith("- "):
            j -= 1
        ind = len(L[j]) - len(L[j].lstrip())
        e = k + 1
        while e < len(L) and not (L[e].strip() and len(L[e]) - len(L[e].lstrip()) <= ind):
            e += 1
        blk = "\n".join(L[j:e])
        if "persist-credentials: false" not in blk and "persist-credentials: true" not in blk:
            bad.append(f"{f.rsplit('/', 1)[-1]}:{k + 1}")
if bad:
    sys.exit("checkout without an explicit persist-credentials: " + ", ".join(bad))
print("workflow checkout test: ok")
PY
