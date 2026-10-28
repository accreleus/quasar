# Summary: autoresearch 0.5.1 overnight (#930)

All 20 approved experiments ran: 16 on Track P and 3 on Track U in round 1 and round 2, 11
kept and 9 discarded (exp 01–15 in round 1, exp 16–20 in round 2 on the lead's "try next"
list). Track U stopped at 3 in round 1: axe was at 0 in scope, and the only in-scope
detector hits left are the DESIGN.md-sanctioned `--t-xs` hint type.

## Cumulative effect (head after exp 20 vs baseline, interleaved in one window: `final20 --ref base1`, load 2.4)

| Metric | Baseline | Final | Δ |
|---|---|---|---|
| **bytes.app** (gzip JS+CSS before `/app` is usable) | 282,434 | 169,954 | **−39.8 %** |
| bytes.account | 282,434 | 244,415 | −13.5 % |
| bytes.settings | 299,977 | 198,835 | −33.7 % |
| ttu.app (ms, median of 7, CPU ×4, 20/5 Mbit, 40 ms) | 594 | 530 | −10.8 % |
| ttu.account | 774 | 718 | −7.2 % |
| ttu.settings | 781 | 670 | −14.2 % |
| axe nodes in the Region/Input scopes | 28 | 0 | −28 |
| impeccable detect findings | 175 | 175 | 0 |
| screenshot pixels changed | — | account shots only, 0.016 % / 0.027 % (Track U colours) | |
| **first open, game page** (probe, outside the harness) | 89 | 392 | **+303 ms** |
| **first open, session view** (probe) | 62 | 351 | **+289 ms** |

The last two rows are the cost of the exp 01/03 route splits: React's Suspense reveal
throttle, not the network (findings.md, round 2). exp 20 shows the cure for the account area.

## Safe to land on develop (non-visual)

1. exp 20 (account area chunk, console group, route preloads, synchronous lazy once loaded).
   It is the largest single win and is faster on all three pages. It carries a test
   obligation, recorded in findings.md.
2. exp 07, 08, 09, 11, 12: helper moves and the lazy palette and overlay preview. Small, with
   no first-open cost on a primary path.
3. exp 01 and 03 (session, game page) **only with** the exp 20 pattern applied to them
   (preload on hover, focus or idle, then a synchronous lazy). Otherwise they add about
   300 ms to the first session and game open.
4. exp 06 (lazy register and styleguide): rare routes, no concern.
5. The toast timer, reveal flake and Switch label fixes are being ported separately.

## Visual (stay on this branch until the operator approves)

- exp 14 and exp 15: two colour-role changes, each with a new DESIGN.md override row (#11 and
  #12) as DESIGN.md's own process requires.
