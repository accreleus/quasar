# The end-to-end streaming test suite

A fixed list of scenarios run against a Quasar stack from a test client, with no model in the
loop, leaving one **run bundle** that an agent or a person reviews afterwards. Spec: #1271.

> **Status.** The bundle format and its validator exist (#1272), and so does the host
> half of a bundle (`make e2e-collect`, #1274). The runner and the other `make e2e-*`
> verbs are the spec's later tickets; this page grows with them.

## What you need

- Node 22. The validator and the collector have no dependencies.
- The host half also needs an admin bearer for the stack, and a certificate Node trusts
  (the fleet's certs are; set `NODE_EXTRA_CA_CERTS` otherwise).

## What to do

Check a bundle:

```
node scripts/validate/stream/bundle.mjs <bundle-dir>
```

Exit 0 valid, 1 not valid (each problem on its own line, as `<file>: <what>`), 2 usage. It never
stops on a stack trace: a bundle it could not read to the end is not valid, and says so.
The last line is `RESULT status=ok|failed target=e2e-bundle errors=N`.

Complete a bundle with the host's half, by session id:

```
make e2e-collect BUNDLE=<bundle-dir> HOST=<role>
```

`HOST` resolves the stack the way the `session-*` verbs do (`local` when unset), and the
bearer comes from `scripts/dx/admin_token.sh`; it travels to the collector in
`QUASAR_ADMIN_TOKEN` and is written nowhere. It can run days after the client half, on another
machine. A **directory** only: unzip a bundle first. The bundle is validated before the admin
bearer is used, and a bundle that is not valid is not collected.

Exit 0 every session collected, 1 the bundle is not valid after collection, 2 usage or the
bearer was rejected, 3 incomplete. The last line is
`RESULT status=ok|failed|error target=e2e-collect scenarios=N sessions=N collected=N`. A
session the stack no longer holds (404, or a trace that has been pruned) is not an error: it
gets `<scenario>/host-half.json` with `collected: false` and the reason, `halves.host` stays
`false`, and the run exits 3. Collecting the same bundle twice against the same answers leaves
every file byte-identical.

### What the host half adds

| file | holds |
|---|---|
| `<scenario>/diagnostic-bundle.json` | the session's diagnostic bundle, as `GET /v1/admin/sessions/{id}/diagnostic-bundle` returned it (addresses and credentials redacted) |
| `<scenario>/session.json` | the session row, in the run-directory shape `bench_submit.py` reads (`launch`, `fps`, `bitrate_kbps`, `codec`, `rungs`, `host_id`, timestamps) |
| `<scenario>/conditions.json` | `effective`: the host's effective settings, projected exactly as `bench_run.sh` writes them (booleans as `on`/`off`, ladder aliases) |
| `<scenario>/metrics.jsonl` | the trace series as samples, **merged** with the client half's own rows — same source and `ts_unix_ms` is the same row, and the client half's wins; sorted by ts then source |
| `<scenario>/trace.json` | the session's stored trace events, merged with the client half's |
| `<scenario>/host-half.json` | `{ "collected": true }`, or `false` with the `reason` for a session that could not be collected |
| `builds.json` | `components.control_plane` from `GET /v1/admin/platform/identity`, `components.node_agent` from the host's `agent_version`/`source_commit`/`built_at`; every other component is left as the client half wrote it, and one that cannot be read stays `null` with a reason in `unknown` |
| `manifest.json` | `halves.host: true`, only when every session was collected |

If a scenario names more than one session, the first one's files are the scenario's own and
the rest keep theirs under `<scenario>/sessions/<session-id>/`.

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
| anything else in a scenario directory | pictures, per-frame records, input records, logs, the session's `diagnostic-bundle.json` and `host-half.json` |

### Rules the validator enforces

- **Shape.** The four top-level files and every `scenario.json` are JSON objects. Required: in the manifest `format`, `suite_run_id`, `plan`, `started_at_ms`, `ended_at_ms`, `runner`, `halves`, `scenarios` (directory names, no path) and `not_recorded` (each entry a `fact` and a `why`); `client.kind`; `builds.components`; `network.shaping`; in a scenario `id`, `params`, `sessions`, `status`, `clock_offset_ms` and `criteria`. A scenario that was not skipped has a `timeline.jsonl`. Every line of `timeline.jsonl` and `metrics.jsonl` is an object; a sample has `ts_unix_ms` and `metrics`. A bundle holds regular files only, no links. The other fields in the table are not checked yet.
- **Status** is `pass`, `fail`, `error` or `skipped`. `skipped` and `error` carry a `reason`.
- **A criterion** has `id`, `pass` (`true`, `false`, or `null` for "not judged"), `observed` and `expected`.
- **Not measured is `null`, never `0`.** A criterion that was not judged has `observed: null`. A fact listed in `not_recorded` has no value as a criterion or as a sample.
- **Keys are the metric manifest's.** An `agent` or `browser` sample key must be in `docs/session-trace/metrics.json` (bench-mode keys ride in `browser` samples, with the six keys `scripts/dx/bench_app_samples.py` derives: `bench_n`, `bench_decoded`, `i2p_p50_ms`, `i2p_max_ms`, `i2p_n`, `ts_nudged`). Sample sources are the five quasar-bench accepts: `agent`, `browser`, `app`, `netem`, `harness`.
- **Every timestamp names its clock.** A timeline row says `client` or `host`; a sample's clock is its key's clock in the metric manifest. `clock_offset_ms` is the measured client-to-host offset, or `null`.
- **A missing build has a reason.**
- **No address, token or password** in any text file, whatever its name. A file with a NUL byte in its first 4 KB is taken as binary (a picture, a video, an archive) and not read; UTF-16 with a byte-order mark is text. Documentation addresses (RFC 5737) pass. Known limits: the address check knows IPv4 only, reads a four-part version such as `1.2.3.4` as an address, and lets `x.0.0.0` and `127.0.0.1` through (a browser's version looks like the first); a bearer token made of letters only, and UTF-16 with no byte-order mark, are not seen; host names and home paths are not looked for (a session container's own home is a real path in app logs). `scripts/dev/leak-scan.sh` with the operator's patterns still runs before a bundle goes anywhere public.

The test (`node --test scripts/validate/stream/bundle.test.mjs`, part of `make verify`) breaks the
fixture once per rule above. The collector's test drives it against a fake API as an in-process
`fetch` — no sockets, no stack — and checks a client-only bundle comes out complete and valid,
that a second collection changes no byte, and that a 404, an address and the bearer are each
handled as described above.
