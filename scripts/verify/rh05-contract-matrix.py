#!/usr/bin/env python3
"""Check the proposed RH05 matrix against the current hostcfg catalog.

This validates the review packet's completeness and effect classification. It
does not claim that an unimplemented RH05 feature passes its behavior examples.
"""

from pathlib import Path
import re
import sys


root = Path(__file__).resolve().parents[2]
catalog = (root / "control-plane/internal/hostcfg/catalog.go").read_text()
proposal = (root / "docs/design/rh05-contract-proposal.md").read_text()

catalog_rows = dict(
    re.findall(r'\{Key: "([^"]+)".*?Class: Class(Live|Restart),', catalog)
)
matrix_rows = {}
for line in proposal.splitlines():
    match = re.match(
        r"^\| `([a-z][a-z0-9_]*)` \| (A/D/E|D/E) \| .*? \| (N|R)/[^|]+ \| ([^|]+) \|$",
        line,
    )
    if match:
        key, modes, effect, evidence = match.groups()
        if key in matrix_rows:
            sys.exit(f"duplicate matrix key: {key}")
        matrix_rows[key] = (modes, effect, evidence.strip())

missing = sorted(catalog_rows.keys() - matrix_rows.keys())
extra = sorted(matrix_rows.keys() - catalog_rows.keys())
wrong_effect = sorted(
    key
    for key, (_, effect, _) in matrix_rows.items()
    if key in catalog_rows
    and effect != {"Live": "N", "Restart": "R"}[catalog_rows[key]]
)
if missing or extra or wrong_effect:
    sys.exit(
        f"RH05 matrix drift: missing={missing}, extra={extra}, "
        f"wrong_effect={wrong_effect}"
    )
if {key for key, (modes, _, _) in matrix_rows.items() if modes == "A/D/E"} != {
    "encoder",
    "render_node",
}:
    sys.exit("automatic source set changed without contract review")
if any(not evidence for _, _, evidence in matrix_rows.values()):
    sys.exit("matrix row missing evidence")
print(f"RH05 proposal matrix covers all {len(catalog_rows)} current catalog keys")
