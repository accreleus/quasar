# The end-to-end streaming test suite

A fixed list of scenarios run against a Quasar stack from a test client, with no model in the
loop, leaving one **run bundle** that an agent or a person reviews afterwards. Spec: #1271.

> **Status.** The bundle format and its validator (#1272) and the runner (#1273, `make e2e-run`)
> exist. `e2e-collect`, `e2e-review`, `e2e-post`, `e2e-compare` and `e2e-pack` are the spec's later
> tickets; this page grows with them.

## What you need

- Node 22. The validator has no dependencies.
- To run a plan (`e2e-run`): Chrome or Chromium, `npm ci` in `scripts/validate` (the verb does it on first use), and the stack's URL.
- A test user: an ordinary user's sign-in in `QUASAR_E2E_EMAIL` and `QUASAR_E2E_PASSWORD`, or in a file of `KEY=value` lines named by `QUASAR_E2E_CRED_FILE`. No admin token is needed or used.
- The reference app in that user's library (`QUASAR_E2E_APP` names it, default `Quasar Benchapp`).

## What to do

Run a plan from the machine you are testing from:

```
make e2e-run TARGET=<stack url> PLAN=smoke [CDP=<DevTools http endpoint>] [OUT=<dir>]
```

`PLAN` is a name under `scripts/validate/stream/plans/` or a path to a plan file. With `CDP=` the
runner attaches to a browser that is already running (the Mac test client), uses its default
context and a real window, and closes only the pages it opened. Without it, it launches `CHROME=`
(or Playwright's Chromium). Each `stream-hold` signs in through the login form, launches the
reference app through the page's own Launch options (codec, frame rate and resolution; that dialog
exists only in the classic library layout, so the runner turns the test user's `classic_layout`
setting on and puts it back) with the product's `?transport=` and `?codec=pyrowave` switches and bench
mode on, holds, and stops the session; the session is stopped on every exit path, `Ctrl-C`
included. The run ends with one line:

```
RESULT status=ok|failed|error target=e2e-run bundle=<dir> pass=N fail=N error=N skipped=N
```

Exit 0 when no scenario failed or errored (skipped ones are counted, never hidden), 1 when one did,
2 for a usage error or a bundle that does not pass the check below. Start reading at
`<bundle>/<NN>-stream-hold/scenario.json`: its `criteria` each say `pass`, `observed` and `expected`;
`pass: null` means it could not be judged here (and `observed` is `null`).

`stream-hold` criteria: `first_frame_s` within 30; `marker_decoded`; `transport_as_asked`,
`codec_as_asked` and `profile_as_asked` against the session row; `freeze_count` 0, `missing_indices_rate`
at most 1 % and `present_cadence` (fewer than `classifier.hitch_min_samples` seconds whose
present-interval sd reaches `classifier.hitch_sd_ms`) over the stream after its first 10 s. The
profile is not a control the user has (the Launch options pick a codec, frame rate and resolution),
so `profile_as_asked` compares the asked profile with the stream the session got. `freeze_count` is
judged from the bench windows with or without a marker; the other two bench criteria need one.
A scenario is `pass` only when at least one criterion held and none failed; if nothing could be
judged it is `error` ("nothing could be judged"). `params.bench: false` leaves bench mode off. In attach mode the runner
puts the stack origin's `localStorage` back as it found it after each scenario, on every exit path.
**Use a fresh test user for each run.** A client that cannot keep up at a profile is refused it
afterwards (`historical_client_performance_failed`; the scenario ends `error: the launch was
refused: 409 ...`), and on the Mac test client one bench-armed 60 s hold at 1080p60 was enough (a hold with
`bench: false` was not). A scenario the
client cannot run (no WebTransport, a codec it cannot decode, the app not in the catalog) is
`skipped` with the reason.

The runner reads only what the product exposes (`window.__qBench`, the page, the API) and wraps no
browser API. What it cannot see is in `manifest.json` `not_recorded`, each with a reason. Every file
is scrubbed when written: addresses, tokens and the test user's sign-in become markers, the
stack's host name becomes `<stack>`, and the manifest counts what was replaced.

Check a bundle:

```
node scripts/validate/stream/bundle.mjs <bundle-dir>
```

Exit 0 valid, 1 not valid (each problem on its own line, as `<file>: <what>`), 2 usage. It never
stops on a stack trace: a bundle it could not read to the end is not valid, and says so.
The last line is `RESULT status=ok|failed target=e2e-bundle errors=N`.

## The run bundle, format 1

A directory, zipped when it is handed between machines.

| file | holds |
|---|---|
| `manifest.json` | `format: 1`, `suite_run_id`, the `plan` as run, `started_at_ms` and `ended_at_ms`, the runner's commit, which `halves` are present (`client`, `host`), the scenario directories in order, `not_recorded` (each fact with why), what was `redacted` |
| `client.json` | the test client: `kind` (`web` or `native`), browser, operating system, CPU, GPU adapter, displays, decode support per codec, power source |
| `builds.json` | `components`: the exact build of each part (control plane, node agent, web client, app image, reference app, GPU driver, ...). A part that cannot say is `null` and `unknown` gives the reason |
| `network.json` | `path` facts, and `shaping` (`null` when none was applied) |
| `<scenario>/scenario.json` | the scenario as run: `id`, `params`, `status`, `reason` when skipped or in error, `sessions`, `clock_offset_ms`, `criteria` |
| `<scenario>/timeline.jsonl` | one step or event per line: `ts_unix_ms`, `clock`, `ev` |
| `<scenario>/metrics.jsonl`, `trace.json`, `session.json`, `conditions.json`, `summary.json` | the series and events, in the run-directory shape `scripts/dx/bench_submit.py` reads. Posting (`e2e-post`, #1276) adds what that script does not take from the directory: the verdict from `status`, and `scenario.json` and `timeline.jsonl` as artifacts |
| anything else in a scenario directory | pictures, per-frame records, input records, logs, the session's diagnostic bundle |

### Rules the validator enforces

- **Shape.** The four top-level files and every `scenario.json` are JSON objects. Required: in the manifest `format`, `suite_run_id`, `plan`, `started_at_ms`, `ended_at_ms`, `runner`, `halves`, `scenarios` (directory names, no path) and `not_recorded` (each entry a `fact` and a `why`); `client.kind`; `builds.components`; `network.shaping`; in a scenario `id`, `params`, `sessions`, `status`, `clock_offset_ms` and `criteria`. A scenario that was not skipped has a `timeline.jsonl`. Every line of `timeline.jsonl` and `metrics.jsonl` is an object; a sample has `ts_unix_ms` and `metrics`. A bundle holds regular files only, no links. The other fields in the table are not checked yet.
- **Status** is `pass`, `fail`, `error` or `skipped`. `skipped` and `error` carry a `reason`.
- **A criterion** has `id`, `pass` (`true`, `false`, or `null` for "not judged"), `observed` and `expected`.
- **Not measured is `null`, never `0`.** A criterion that was not judged has `observed: null`. A fact listed in `not_recorded` has no value as a criterion or as a sample.
- **Keys are the metric manifest's.** An `agent` or `browser` sample key must be in `docs/session-trace/metrics.json` (bench-mode keys ride in `browser` samples, with the six keys `scripts/dx/bench_app_samples.py` derives: `bench_n`, `bench_decoded`, `i2p_p50_ms`, `i2p_max_ms`, `i2p_n`, `ts_nudged`). Sample sources are the five quasar-bench accepts: `agent`, `browser`, `app`, `netem`, `harness`.
- **Every timestamp names its clock.** A timeline row says `client` or `host`; a sample's clock is its key's clock in the metric manifest. `clock_offset_ms` is the measured client-to-host offset, or `null`.
- **A missing build has a reason.**
- **No address, token or password** in any text file, whatever its name. A bundle's text is UTF-8: a file with a UTF-16 or UTF-32 byte-order mark is not valid, and any other file with a NUL byte in its first 4 KB is taken as binary (a picture, a video, an archive) and not read. Documentation addresses (RFC 5737) pass. Known limits: the address check knows IPv4 only, reads a four-part version such as `1.2.3.4` as an address, and lets `x.0.0.0` and `127.0.0.1` through (a browser's version looks like the first); a bearer token made of letters only is not seen, nor is UTF-16 text with no byte-order mark (it reads as binary); host names and home paths are not looked for (a session container's own home is a real path in app logs). `scripts/dev/leak-scan.sh` with the operator's patterns still runs before a bundle goes anywhere public.

The test (`node --test scripts/validate/stream/bundle.test.mjs`, part of `make verify`) breaks the
fixture once per rule above.
