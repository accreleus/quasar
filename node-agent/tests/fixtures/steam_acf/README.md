# Steam ACF manifest fixtures

These 9 files are verbatim captures of the real `appmanifest_*.acf` files from
the live Tower box (2026-07-29), used as the parser test corpus for the
library-discovery scanner (`node-agent/src/session/library_scan.rs`).

**One deliberate modification:** every `LastOwner` value has been replaced
with the synthetic `76561190000000000`. The real value is a SteamID64
belonging to an actual person, and committing it into this repo would be
exactly the disclosure the PII rule in
`docs/design/plans/2026-07-29-steam-library-discovery-spec.md` §9 exists to
prevent. A fixture still needs *a* `LastOwner` line present for the
allow-list test to mean anything, hence the synthetic replacement rather than
removal.

Do not replace this scrubbed value with a real SteamID64, and do not add new
fixtures captured from a live box without scrubbing `LastOwner` the same way
first.

The 5 Valve-tool appids in this corpus: 1493710, 2180100, 1628350, 4183110,
228980. The 4 games: 2183900, 2536520, 3179810, 517710 (the denylist decision
itself belongs to the control plane, not the agent parser — this split is
recorded here only so the corpus is recognisable).
