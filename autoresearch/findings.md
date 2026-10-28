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
  `cancelReveal()`. It failed `make test-web` once in this run (exp 02).
