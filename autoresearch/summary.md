# Summary: autoresearch 0.5.1 overnight (#930)

## Round 3 (exp 21–30): measured against develop at the merge point

Round 3 starts by merging `origin/develop` (`bf609a71`: #941 toast timer, #945 reveal
`afterEach`) into the branch. Everything below is measured against that merge point,
and its eager baseline is plain `develop` (`dev`). The first-open probe was a guard
this round: neither the game page nor the session view may end more than 50 ms above
`dev`.

| Metric | develop (`dev`) | round-3 start (`m3`) | final head | Δ vs develop |
|---|---|---|---|---|
| **bytes.app** (gzip JS+CSS before `/app` is usable) | 282,440 | 169,952 | **150,364** | **−46.8 %** |
| bytes.account | 282,440 | 244,415 | 225,271 | −20.2 % |
| bytes.settings | 299,977 | 198,835 | 188,433 | −37.2 % |
| ttu.app (ms, median of 7, CPU ×4, 20/5 Mbit, 40 ms) | 585 | — | 513 | −12.3 % |
| ttu.account | 762 | — | 681 | −10.6 % |
| ttu.settings | 771 | — | 651 | −15.6 % |
| **first open, game page** (probe) | 91 | 392 | **91** | 0 |
| **first open, session view** (probe) | 59 | 353 | **60** | +1 ms |
| signed-out `/login` form (probe) | 445 | — | 370 | −16.9 % |
| axe nodes in the Region/Input scopes | 28 | 0 | 0 | −28 |
| impeccable detect findings | 175 | 175 | 175 | 0 |
| screenshot pixels changed | — | — | account shots only, 0.016 % / 0.027 % (Track U colours) | |

The final ttu and first-open rows are from one window (`final30 --ref dev`, load 1.6).
The `m3` first-open row is the round-3 starting point (`m3` guard run).

**Round 3 kept** five experiments (21 as a guard fix, then 22, 25 and 26) and two pieces of
zero-cost infrastructure (23, 24). It **discarded** four (27 and 28 inside the byte noise;
29 and 30 because the frozen rules cannot see their wins, patches kept).

### Safe to land on develop (non-visual), after round 3

1. **The split chain, as one unit or in this order:** exp 01, 03, 06, 07, 08, 09, 11, 12,
   20, **21**, **22**, **25**, **26**. exp 21 (`lib/preloadable.ts`) is what makes
   exp 01/03 safe: without it they add about 300 ms to the first game or session open.
2. **The guards that keep the wins:** exp 23 (the entry budget, which `make test-web`
   enforces) and exp 24 (the deep-link test), with `lib/preloadable.test.tsx`.
3. **Two patches the frozen rules discarded but the evidence supports**
   (`patches/`): exp 29, admin deep-link preloads (Settings time to usable
   646 → 569 ms), and exp 30, which makes a scrolling table keyboard-reachable
   (axe `scrollable-region-focusable`).
4. exp 13 (`Switch` label) is being ported separately (#943); the round-1 toast and
   reveal fixes are on develop.

**Visual (branch only, operator review):** exp 14 and 15 with DESIGN.md overrides #11 and #12.

---

## Rounds 1–2 (exp 01–20)

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
