# Autoresearch run 0.5.1 overnight (tracking issue #930)

Karpathy's method: a frozen evaluation harness, one change per experiment, a fixed
budget per experiment, one keep/discard rule, and every experiment logged. Nothing
from this branch merges as-is. Kept changes that are safe to land go to `develop`
later, each as its own PR.

## Budget

20 experiments at most: 12 on Track P, then 8 on Track U. About 20 minutes each.
P runs first, so every P comparison is made against a picture no U change has
moved. Stop early when no promising hypothesis is left, and say so in the log.

## Harness (frozen at the baseline commit)

`autoresearch/harness/` must not change after the baseline commit.

- `run.sh <label> [--guards]` builds `web/` in `node:22` (`npm ci`,
  `vite build --manifest`: the manifest flag is the harness-local override, and
  `web/vite.config.ts` is not touched). It then serves the build over
  `server.mjs`, a static mock of the control plane with fixed fixtures that
  gzips like the control plane does. It measures with `measure.mjs` in the
  Playwright 1.56 image. `--guards` also runs `make test-web`: schema drift,
  typecheck, vitest including the design lint, and the production build.
- `run.sh <label> --ref <kept>` interleaves timing. Each throttled candidate
  load is followed by one of the kept head's build (kept as `out/<kept>.dist`),
  so both medians see the same machine load. The machine is shared with other
  sessions, and in the first two unpaired baseline runs the absolute timings
  drifted ~40 %. Before measuring, the run waits (at most 10 minutes) for the
  1-minute load to fall below 4.
- `compare.mjs --track P|U --ref <kept> --cand <exp>` applies the keep rule.
- **Test flakes:** when `make test-web` fails only in vitest, the failing files
  are re-run once. If they pass, the guard reads `pass (flake re-run: …)`.
  Under load the Region tests time out at 5 s. Typecheck, build and schema
  drift failures are never re-run.

### Pages

| key | path | usable when |
|---|---|---|
| app | `/app` | the library grid ("Vector Rally") and the highlights rail are in the DOM |
| account | `/app/account/region` | Region's language select and timezone picker, and the Input section, are in the DOM |
| settings | `/admin/settings` | the Region defaults and Input defaults cards and a select are in the DOM |

## Track P: performance

- **Primary:** `bytes.app`. This is the gzip size, at the control plane's level
  6, of every `/assets` JS and CSS file a cold browser started fetching before
  `/app` was usable. It comes from Resource Timing, so lazy chunks count only
  when they are on the path to usable. The same measure is recorded for
  `account` and `settings`, along with `entry_static` (the manifest entry, its
  static imports and its CSS).
- **Time to usable:** `ttu.<page>`, the ms from navigation start to the usable
  marker. Each run is a cold context with CPU ×4 and a network of 20/5 Mbit/s
  at 40 ms RTT. The figure is the median of 7 runs.
- **Guards:**
  - every `ttu.<page>` is at most the interleaved kept head's value plus the
    noise band;
  - `make test-web` is green;
  - each of the six screenshots (three pages at 1280 and at 390 wide, dark
    theme, clock frozen) differs from the kept head's in at most 0.1 % of its
    pixels (pixelmatch threshold 0.1).
- **Keep** iff `bytes.app` falls by more than the bytes noise band and every
  guard holds.

## Track U: polish

Scope: Account ▸ Region, Account ▸ Input, and Admin ▸ Settings (the Region and
Input defaults cards).

- **Primary:** `polish_primary` = `axe_total` + `impeccable_total`, lower is
  better.
  - `axe_total` counts axe-core violation nodes inside the scoped sections, over
    {account, settings} × {dark, light} × {1280, 390}.
  - `impeccable_total` counts `impeccable detect` primary findings on the same
    URLs, themes and widths. It runs from the repo root, so DESIGN.md applies.
- **Guards:** `make test-web` is green (the design lint is part of vitest). The
  change must obey `DESIGN.md` and `design_handoff_v3/screens/region-input-v3.html`,
  and DESIGN.md wins where they disagree. Never restyle from taste.
- **Process for every U experiment (operator directive):**
  1. Run the `impeccable` skill's critique on the page before picking the
     hypothesis.
  2. Make the change with its polish pass, within DESIGN.md and the mock.
  3. Re-run detect through the harness for the keep rule.
  4. Record in `results.tsv` which impeccable finding (or axe rule) the
     experiment addresses.
- **Keep** iff `polish_primary` falls by more than its noise band and the guards
  hold. U changes are visual and stay on this branch.

## Noise

Two identical baseline runs of the same commit: `base1`, and `base2 --ref base1`
(an interleaved A/A test).

- the bytes band is max(|Δ|, 256 B);
- the ttu band per page is max(|base2 − its interleaved base1| of the medians,
  5 % of the mean);
- the polish band is |Δ|.

All three bands are recorded in `baseline.json`.

## Loop

1. Pick one hypothesis and make the change.
2. Run `bash autoresearch/harness/run.sh expNN --ref <kept> --guards`.
3. Run `node autoresearch/harness/compare.mjs --track P|U --ref <kept> --cand expNN`.
4. On keep, commit `exp NN: <hypothesis> (<before> → <after>)` and push.
5. On discard, `git reset --hard` to the last kept commit.
6. Either way, append the experiment to `results.tsv` and commit the log on the
   kept head.

`results.tsv` columns: `n, track, hypothesis, primary_before, primary_after,
guards, kept, impeccable_finding` (the last is U only; `-` on P).

Scope: `web/` and `autoresearch/` only, never `protocol/`, the control plane,
the agent or a contract. A tiny in-scope bug may be fixed. Anything else goes
in `findings.md`.

## Harness amendments (logged; no metric or keep rule changed)

1. **After exp 02.** The guard's flake re-run aborted (`set -e` on a `grep`
   with no match) whenever vitest failed with an unhandled error rather than a
   failing test, so no verdict was written. It now also re-runs the file an
   unhandled error "originated in". That class is a timer outliving its test:
   `settingsSearch/reveal.ts` ticks for up to 4 s after
   `CommandPalette.design.test.tsx` ends (see findings.md). exp 02 is recorded
   as a test failure. It was discarded on its time-to-usable guard either way.

## Stop note

Stopped after 15 experiments (12 P, 3 U). Track U had nothing left that its metric can see in
scope: axe was at 0, and the remaining detect hits are the sanctioned hint type role. A
change aimed at them would be restyling against DESIGN.md. See summary.md.

## Round 2 (lead, after the first report): exp 16–20

The same frozen harness and keep rule, on the same branch, spending the 5 remaining
experiments on the "try next" list. All 20 are now used. The kept head was re-measured as
`k15` first (its build had been cleaned up), and exp 16–20 are interleaved against it.
`probes/first-open.mjs` is a supplementary measurement outside the harness, never a keep
criterion.

## Round 3 (lead, after round 2): exp 21–30

- **Baseline.** Started by merging `origin/develop` (`bf609a71`). Round 3 is
  measured against that merge point (`m3`). The eager baseline is plain develop
  (`dev`), built in a throwaway worktree and measured with the frozen
  `measure.mjs`.
- **New guard:** `probes/guard.sh <label>` runs `probes/first-open.mjs` against
  `dev`, interleaved. The game page or session view more than 50 ms above `dev`
  means not kept.
- **Same harness, keep rule, load gate and noise bands.** `kept` values beyond
  yes/no:
  - `infra`: zero-cost infrastructure, no metric moved.
  - `guard-fix`: exp 21, kept under the lead's done-criterion for its item while
    the byte primary stayed inside the noise band.
