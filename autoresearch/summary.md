# Summary: autoresearch 0.5.1 overnight (#930)

15 of the 20 approved experiments ran: 12 on Track P and 3 on Track U. 10 were kept and 5
discarded. Track U stopped early because nothing the keep rule can measure is left in scope.
After exp 15 the scoped axe count is 0, and every in-scope `impeccable detect` hit is
`tiny-text` on the hint role, which is DESIGN.md's `--t-xs` type role and cannot change without
restyling against the spec.

## Cumulative effect (final head vs baseline, interleaved in one window: `final --ref base1`)

| Metric | Baseline | Final | Δ |
|---|---|---|---|
| **bytes.app** (gzip JS+CSS before `/app` is usable) | 282,434 | 201,667 | **−28.6 %** |
| bytes.account | 282,434 | 242,034 | −14.3 % |
| bytes.settings | 299,977 | 219,326 | −26.9 % |
| ttu.app (ms, median of 7, CPU ×4, 20/5 Mbit, 40 ms) | 593 | 542 | −8.6 % |
| ttu.account | 756 | 705 | −6.7 % |
| ttu.settings | 791 | 753 | −4.8 % |
| axe nodes in the Region/Input scopes | 28 | 0 | −28 |
| impeccable detect findings | 175 | 175 | 0 |
| screenshot pixels changed | — | account shots only, 0.016 % / 0.027 % (Track U colours) | |

## Safe to land on develop (non-visual, one PR each)

1. `fix(web): clear a toast's auto-dismiss timer when it unmounts` (532d7608). It makes
   `make test-web` reliable.
2. The Track P chain (exp 01, 03, 06, 07, 08, 09, 11, 12), one PR per experiment or one bundle
   PR. Each is code splitting or a helper move. Every guard held: pixel diff 0, tests green and
   time to usable not worse. Cost to note: a first visit to a session, a game page, the palette
   or the overlay preview fetches its chunk then (one round trip, skeleton or nothing shown).
3. exp 13: the `Switch` nested-label fix. It is an a11y bug in a shared primitive with no
   pixel change, so it can land as a plain fix.

## Visual (stay on this branch until the operator approves)

- exp 14 and exp 15: two colour-role changes, each with a new DESIGN.md override row (#11 and
  #12) as DESIGN.md's own process requires. They need the operator's eye before develop.
