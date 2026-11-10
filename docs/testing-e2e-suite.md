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
Exit 0 when every scenario's status is `pass` (a skipped scenario is listed and does not fail
the run), 1 when one is `fail` or `error`, 2 when the bundle is not valid: then it prints the
validator's problems and writes nothing.

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
- **A criterion** has `id`, `pass` (`true`, `false`, or `null` for "not judged"), `observed` and `expected`.
- **Not measured is `null`, never `0`.** A criterion that was not judged has `observed: null`. A fact listed in `not_recorded` has no value as a criterion or as a sample.
- **Keys are the metric manifest's.** An `agent` or `browser` sample key must be in `docs/session-trace/metrics.json` (bench-mode keys ride in `browser` samples, with the six keys `scripts/dx/bench_app_samples.py` derives: `bench_n`, `bench_decoded`, `i2p_p50_ms`, `i2p_max_ms`, `i2p_n`, `ts_nudged`). Sample sources are the five quasar-bench accepts: `agent`, `browser`, `app`, `netem`, `harness`.
- **Every timestamp names its clock.** A timeline row says `client` or `host`; a sample's clock is its key's clock in the metric manifest. `clock_offset_ms` is the measured client-to-host offset, or `null`.
- **A missing build has a reason.**
- **No address, token or password** in any file that is not binary, whatever its name. Documentation addresses (RFC 5737) pass. Known limits: the address check knows IPv4 only, reads a four-part version such as `1.2.3.4` as an address, and lets `x.0.0.0` and `127.0.0.1` through (a browser's version looks like the first); host names and home paths are not looked for (a session container's own home is a real path in app logs). `scripts/dev/leak-scan.sh` with the operator's patterns still runs before a bundle goes anywhere public.

The test (`node --test scripts/validate/stream/bundle.test.mjs`, part of `make verify`) breaks the
fixture once per rule above.

## Reading REVIEW.md

`REVIEW.md` is the deterministic summary: the same bundle in gives the same bytes out, so two
reviews of one bundle can be diffed. It is not a verdict — each scenario's status is what the
run recorded.

- **The top** is the suite run id, the plan, the test client (label and kind), the builds line
  (`unknown (why)` for a part that could not say), the counts, the thresholds version, and
  every `not_recorded` fact with its reason.
- **Criteria** are listed failed first (`fail`, then `not judged`, then `pass`), each with its
  observed and expected value. `null` stays `null`: a fact that was not measured is never `0`.
- **The five worst one-second windows** are ranked from the `browser` samples by
  `present_interval_max_ms`, `g2g_p95_ms` and `missing_indices` (highest) and `fps` (lowest).
  Each row names the key that put it there, and the line above the table names the keys the
  bundle does not carry: an absent key is skipped, never scored as `0`.
- **Anomalies** are rule-based: one row per threshold in `docs/session-trace/thresholds.json`
  that is a limit on one sample key, with how many windows crossed it and the worst value. A
  threshold phrased as a p10 or a delta over a window — rather than a value a single sample
  carries — maps onto no one key, so it is not used.
- **Files worth opening** appears for a scenario with a failed criterion or a `fail`/`error`
  status: its directory, its `timeline.jsonl` and any pictures in it, by path relative to the
  bundle.

The test (`node --test scripts/validate/stream/review.test.mjs`, part of `make verify`) pins the
fixture's two files as golden, runs it twice for the same bytes, and checks the 150-line budget
of a four-scenario review.
