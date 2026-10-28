# Findings for the lead (autoresearch 0.5.1 overnight, #930)

## Fixed on this branch (tiny, in scope)

- **Toast timers outlived their toast** (`web/src/components/Toast.tsx`). The
  auto-dismiss timer started inside a `useState` initializer (a side effect
  during render) and was never cleared on unmount. StrictMode therefore started
  two timers per toast, and in tests a timer fired after jsdom teardown. That
  made `make test-web` fail on an unchanged `develop` (a38fac5f) with
  "ReferenceError: window is not defined", reported from `GamePage.test.tsx`,
  even though all 4380 tests passed. It is now an effect with a cleanup. This
  was fixed before the baseline because the test guard has to be green there.
  It is safe to land on its own.

## For the lead

- **`settingsSearch/reveal.ts` outlives `CommandPalette.design.test.tsx`.**
  `revealWhenReady` polls for up to 4 s with a module-level timer. A test that
  triggers a reveal and ends sooner leaves the timer to fire after jsdom
  teardown ("document is not defined"), and vitest then fails the run as an
  unhandled error even though every test passed. The production behaviour is
  intended (navigation cancels the wait). The fix belongs in the test, which
  should cancel the pending reveal in `afterEach`, or in a small exported
  `cancelReveal()`. It failed `make test-web` in 2 of 13 guard runs here
  (exp 02, exp 11); vitest blames whichever file is running when the timer
  fires (`CommandPalette.design.test.tsx`, then `AccountAppearance.test.tsx`).

- **The setup wizard keeps the admin API client in every player's entry chunk.**
  `pages/setup/Step*.tsx` import `api/admin.ts` (46 KB of source),
  `components/SecretField` and the library API. The wizard is eager, so all of
  that ships to a player who will never see `/setup`. Lazy-loading it saves
  about 23 KB gzip on `/app` (exp 04: 254 → 231 KB). It was discarded because
  admin Settings then fetched the wizard ↔ admin shared modules as four extra
  chunks and got 73 ms slower. Inlining small common chunks (exp 10) fixed the
  request count but made settings slower again (+87 ms, more JS to parse).
  The structural fix: give the wizard its own thin API surface, or have the
  admin layout statically import what Settings and the wizard share.

- **Splitting the account area fragments admin chunks the same way** (exp 02:
  −25 KB on `/app`, but account +164 ms from a layout → section → page lazy
  waterfall, and settings +60 ms). Doing it well needs one account chunk (a
  barrel import) and the shared shell (`ConsoleShell`, zones data, `Chip`,
  `ResourceStates`) kept together.

- **Harness realism note.** The mock serves HTTP/1.1 (6 connections per host).
  Behind TLS with HTTP/2 the control plane would pay less per extra chunk, so
  exp 02/04 may be pessimistic. Their bytes savings stand either way.

- **A pattern worth a lint rule.** A shared module importing one small helper
  from a big module pins the big module into the entry chunk. That happened
  four times: `capability.ts` → `input/capture.ts`, `useLaunch` →
  `webrtc/playout.ts`, `ConsoleShell` → `settingsIndex.ts` and → `paletteSearch.ts`.
  The styleguide page did it too, rendering a live `CommandPalette`. A
  bundle-size budget for the entry chunk in `make test-web` would catch a
  regression.

## Round 2 (exp 16–20)

- **Lazy routes cost about 300 ms on first open, and most of it is React, not
  the network.** The probe (`probes/first-open.mjs`, outside the frozen
  harness) times a click from a usable library page to the page rendered,
  under the same CPU ×4 / 20 Mbit / 40 ms profile. Baseline (eager) against
  exp 20: game page **89 → 392 ms**, session view **62 → 351 ms**
  (`probes/first-open-base-vs-exp20.json`). The cost is flat at about 300 ms,
  which matches React's throttle on revealing content after a Suspense
  fallback has shown. This corrects round 1's "one round trip" caveat. The
  cure is the one exp 20 uses for the account area: start the chunk early
  (on hover or focus of a tile, or at idle), then hand `React.lazy` a
  thenable that resolves synchronously once the module is in, so no fallback
  is ever committed. **Do not land the exp 01/03 splits without it.**
- **The admin console is the binding constraint on splitting.** Every split
  that moved code shared with admin pages (exp 02, 04, 10, 16, 17, 18) failed
  the Settings or account time guard. Route preloads from `index.html` (exp 19)
  turned Settings from a loss into a win (−55 to −65 ms). The setup-wizard
  split (exp 17: −18.7 KB for players) is still blocked: Settings +69 ms. It
  would likely pass with the exp 19/20 preload map extended to
  `api/admin`'s chunk; that is untested, because the budget ran out.
- **exp 16's first run was contaminated** (load 43.8 at start; the harness
  gate gives up after 10 minutes and proceeds anyway). It was re-run at
  load 2.9. Both runs are logged, and the clean run decided it.
- **exp 20 relies on a React.lazy implementation detail** (a thenable that
  calls back synchronously is read without suspending). It held under React
  19.3 here. A React upgrade should keep a test that a deep link to
  `/app/account` renders without the route skeleton.
