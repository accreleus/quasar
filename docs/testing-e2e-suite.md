# The end-to-end streaming test suite

A fixed list of scenarios run against a Quasar stack from a test client, with no model in the
loop, leaving one **run bundle** that an agent or a person reviews afterwards. Spec: #1271.

> **Status.** The bundle format and its validator exist (#1272), and so does the review
> that summarises a bundle (#1275). The runner and the posting verb are the spec's later
> tickets; this page grows with them.

## What you need

- Node 22. The validator and the review have no dependencies.

## What to do

Check a bundle:

```
node scripts/validate/stream/bundle.mjs <bundle-dir>
```

Exit 0 valid, 1 not valid (each problem on its own line, as `<file>: <what>`), 2 usage. It never
stops on a stack trace: a bundle it could not read to the end is not valid, and says so.
The last line is `RESULT status=ok|failed target=e2e-bundle errors=N`.

Summarise it:

```
make e2e-review BUNDLE=<bundle-dir>
```

It writes `review/summary.json` and `review/REVIEW.md` inside the bundle — which leaves the
bundle valid — and ends with
`RESULT status=ok|failed target=e2e-review scenarios=N pass=N fail=N error=N skipped=N`.
Exit 0 only when at least one scenario passed and none failed or errored; 1 when one did, and
also when nothing passed (no scenario, or every one skipped — the line then ends
`reason=nothing-passed`); 2 when the bundle is not valid (it prints the validator's problems
and writes nothing) or the review cannot be written (`reason=cannot-write`).

A scenario passed when its status is `pass` and none of its criteria has `pass: false`: the
page's verdict is derived from the two, never copied from the run's own word, and nothing
short of a pass is a success. Both files are rendered in memory and renamed into place, so a
failed write leaves no half a review.

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
| `review/summary.json`, `review/REVIEW.md` | written by `make e2e-review` (below), not by the runner: the summary an agent reads first. A bundle without them is still a bundle |

### Rules the validator enforces

- **Shape.** The four top-level files and every `scenario.json` are JSON objects. Required: in the manifest `format`, `suite_run_id`, `plan`, `started_at_ms`, `ended_at_ms`, `runner`, `halves`, `scenarios` (directory names, no path) and `not_recorded` (each entry a `fact` and a `why`); `client.kind`; `builds.components`; `network.shaping`; in a scenario `id`, `params`, `sessions`, `status`, `clock_offset_ms` and `criteria`. A scenario that was not skipped has a `timeline.jsonl`. Every line of `timeline.jsonl` and `metrics.jsonl` is an object; a sample has `ts_unix_ms` and `metrics`. A bundle holds regular files only, no links. The other fields in the table are not checked yet.
- **Status** is `pass`, `fail`, `error` or `skipped`. `skipped` and `error` carry a `reason`.
- **A criterion** has `id`, `pass` (`true`, `false`, or `null` for "not judged"), `observed` and `expected`. An `id` is an identifier, not text: ASCII letters, digits, `_`, `.` and `-`, at most 64, unique in its scenario. It is the page's row and the run's count, so nothing that prints like another id (a number beside its string, an accented letter in two encodings, a character that does not print) is accepted.
- **The status and the criteria agree.** A scenario whose status is `pass` has no criterion with `pass: false`: the review derives the run's verdict from both, so the two may not disagree.
- **A pass rests on something.** A scenario whose status is `pass` has at least one criterion with `pass: true`. A pass with zero criteria, or with every criterion `null`, is refused: a short bundle is not a success (spec #1271, story 3).
- **One name, one run.** `manifest.scenarios` holds each directory name once.
- **Not measured is `null`, never `0`.** A criterion that was not judged has `observed: null`. A fact listed in `not_recorded` has no value as a criterion or as a sample.
- **Keys are the metric manifest's.** An `agent` or `browser` sample key must be in `docs/session-trace/metrics.json` (bench-mode keys ride in `browser` samples, with the six keys `scripts/dx/bench_app_samples.py` derives: `bench_n`, `bench_decoded`, `i2p_p50_ms`, `i2p_max_ms`, `i2p_n`, `ts_nudged`). Sample sources are the five quasar-bench accepts: `agent`, `browser`, `app`, `netem`, `harness`.
- **Every timestamp names its clock.** A timeline row says `client` or `host`; a sample's clock is its key's clock in the metric manifest. `clock_offset_ms` is the measured client-to-host offset, or `null`.
- **A missing build has a reason.**
- **No address, token or password** in any text file, whatever its name. A bundle's text is UTF-8: a file with a UTF-16 or UTF-32 byte-order mark is not valid, and any other file with a NUL byte in its first 4 KB is taken as binary (a picture, a video, an archive) and not read. Documentation addresses (RFC 5737) pass. Known limits: the address check knows IPv4 only, reads a four-part version such as `1.2.3.4` as an address, and lets `x.0.0.0` and `127.0.0.1` through (a browser's version looks like the first); a bearer token made of letters only is not seen, nor is UTF-16 text with no byte-order mark (it reads as binary); host names and home paths are not looked for (a session container's own home is a real path in app logs). `scripts/dev/leak-scan.sh` with the operator's patterns still runs before a bundle goes anywhere public.

The test (`node --test scripts/validate/stream/bundle.test.mjs`, part of `make verify`) breaks the
fixture once per rule above.

## Reading REVIEW.md

`REVIEW.md` is the deterministic summary: the same bundle and the same
`docs/session-trace/thresholds.json` give the same bytes out, so two reviews with the same
threshold file can be diffed. It is not a verdict — each scenario's status is what the run
recorded — but the run's result is derived from it, not copied.

- **Bundle text is data, never page structure.** Every id, reason, observed or expected value,
  param key and value, client label, suite run id, `not_recorded` fact, build version and file
  name is rendered as an inline code span holding one line: JSON-escaped (a newline becomes the
  two characters `\n`), backticks turned into quotes, `|` escaped and the text cut at 200
  characters. Headings are built from the scenario's ordinal (`## Scenario 1`, then the name on
  the next line), so no bundle value can start a line, forge a heading, a row or a count. The
  first lines are literals and derived numbers, and they come before any bundle text.
- **The top** is the derived result, the counts, the suite run id, the plan, the test client
  (label and kind), the builds line (`unknown (why)` for a part that could not say), the
  thresholds version, and every `not_recorded` fact with its reason.
- **Criteria.** Failed and not-judged criteria are table rows — fail first, then not judged,
  then by id — each with its observed and expected value. `null` stays `null`: a fact that was
  not measured is never `0`. The passed ones are a single `Passed: ` line.
- **The worst one-second windows.** One browser sample row is one window: nothing is folded,
  because bench-mode rows are stamped on the host clock and telemetry rows on the client's, and
  two rows that share a timestamp are two windows that collide. A row is a candidate for
  `present_interval_max_ms`, `g2g_p95_ms` and `missing_indices` (highest) or `fps` (lowest) only
  when its finite value is strictly worse than that key's median, so a clean window is never
  called worst. Candidates are ranked densely per key (equal values share a rank, 0 is the
  worst value) and the five with the lowest rank are shown, ties going to the earlier key and
  then the earlier timestamp; when a tie is cut, the last row shown at that value says
  `(+N more at this value)`. The line above the table names the keys the bundle does not carry
  (an absent key is skipped, never scored `0`). When — and only when — a ranked key comes from a
  bench-mode row, a line below names the ranked keys that are on the host clock (`g2g_p95_ms`,
  `missing_indices`), says the other keys are on the client's clock, and gives the scenario's
  `clock_offset_ms`.
- **Anomalies** use a rule only where the comparison is the one the product makes.
  `classifier.hitch_sd_ms` skips the first `classifier.warmup_exclude_s` seconds after the
  scenario's first browser sample (the control plane anchors on the session's running time, so
  the review can miss a hitch the control plane counts; it never adds one) and needs at least
  `classifier.hitch_min_samples` samples over it; the two `web.client_health.present_*` limits
  are per sample, as the client applies them, and skip a hidden tab (`is_hidden` is `1` or
  `true`, as the contract posts it). One row per rule that fired, with the seconds over, the
  worst value and the first one.
  A rule is **checked** only when the thresholds file states every key it needs — its limit and
  its helpers (`classifier.warmup_exclude_s`, `classifier.hitch_min_samples`), each a finite
  number — and at least one sample carries its sample key. Nothing is defaulted: no value is
  invented for a key the file does not state. The section prints a table row for a rule that
  crossed, nothing for a rule that was checked and did not, and one folded line,
  `Not checked: <rule> (<why>), ...`, for the rules that could not be checked.
  `No threshold crossed.` appears only when at least one rule was really checked and none
  crossed; with no browser samples or no threshold file the section says
  `Not checked: <why>` for the whole section. A bundle whose samples are bench-mode rows only
  therefore never implies that the stream was clean.
  A threshold phrased as a p10 or a delta over a window maps onto no one sample, and a rule on a
  cumulative counter is not a per-window comparison: `packets_lost` and `freeze_count` are
  cumulative in the contract (issue #1270) and are not used. The families the page does not use
  at all (`health.*`, `client_health.*`, `web.present_cadence.*`, `web.playout.*`,
  `web.signal.*`, `web.clock.*` and the classifier values that are a p10 or a p95) are listed
  with a reason in `scripts/validate/stream/review.mjs`.
- **Files worth opening** appears for a scenario that did not pass, in reading order: the
  `timeline.jsonl` first, then the other text evidence sorted by relative path (`*.log`,
  `diagnostic-bundle.json`, `host-half.json`, `failed-requests.json`, `session.json`,
  `bench-windows.json`), then every picture at any depth (the runner writes `shots/*.png`)
  sorted by path — at most 12 with `+N more`, so a long hold's shots never push the files that
  explain the failure out of the list.

The test (`node --test scripts/validate/stream/review.test.mjs`, part of `make verify`) pins the
fixture's two files as golden, runs it twice for the same bytes, and checks the properties the
page promises: a bundle value cannot add a line, member order changes no byte (including a
build component that says neither version nor commit, and a param named `__proto__`), a window
is one sample row (identical rows included), the anomaly rules fire at their limits and are
named as not checked when the thresholds file cannot state them, a hidden tab is not judged, a
write that cannot happen is one line and leaves no earlier review behind, the evidence list
reads timeline then text then pictures, and the worst realistic four-scenario bundle fits in
150 lines.
