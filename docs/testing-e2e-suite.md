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

`HOST` resolves the stack the way the `session-*` verbs do (`local` when unset). The bearer
comes from `scripts/dx/admin_token.sh` (which may mint and cache one); it travels to the collector
in `QUASAR_ADMIN_TOKEN`, never on argv, and is **never written into the bundle**: it is replaced
wherever an answer echoes it. It can run days after the client half, on another machine. A
**directory** only: unzip a bundle first. The bundle is validated before the collector makes
any authenticated request, and a bundle that is not valid is not collected. `HOST`'s stack is
`http(s)://host[:port]` only: no credentials, path, query or fragment.

**A bundle that already has a host half is not collected.** Everything in a bundle is untrusted,
including its own `host-half.json`, so "already collected" is not believed from the bundle
alone. If any scenario holds a `host-half.json`, `diagnostic-bundle.json` or `conditions.json`
(at the top or under `sessions/`), or the manifest says `halves.host: true`, the run stops
before any request with exit 2 (`reason=host-half-present`). Collect a fresh copy of the client
bundle, or add `RESUME=1` to finish a collection **this machine started** (a scenario that failed,
or a run that was cut short).

The collector reads the control plane's own answers, in the control plane's own shapes: the
session row first, then the trace for the window the session ran in (`started_at - 5 s` to
`ended_at + 5 s`, or now), in reads the server allows (10 minutes at most; a read that comes
back full is split), then the raw samples, the host's settings and both builds. **The stack
deletes telemetry after its retention period** (a day by default; the row stays), so a session
collected later than that has no trace: it is not an error and not an empty half, it is
`collected: false` with the reason "the trace is no longer kept". Debug captures (`diag.*`
events) outlive the sweep and do not count as a trace. A live session over an hour has already
lost its oldest telemetry. A session that **never started** (a launch failure: no `started_at`)
is collected: its row (with `failure_code` and `error_message`), its conditions and the agent
identity, no trace; `host-half.json` says `"trace": "the session never started"`.

Exit codes, as they are:

| exit | meaning | last line |
|---|---|---|
| 0 | every session collected, the bundle valid | `RESULT status=ok target=e2e-collect scenarios=N sessions=N collected=N` |
| 1 | the bundle is not valid, before or after collecting | `RESULT status=failed target=e2e-collect errors=N` (plus ` reason=invalid-before-collect` when it was refused before any request) |
| 2 | usage, a bad `--base-url`, no bearer, a rejected bearer (names `scripts/dx/admin_token.sh`), a session id that is not a UUID, a bundle that already has a host half | `RESULT status=error target=e2e-collect reason=<usage\|no-token\|unauthorized\|host-half-present\|...>` |
| 3 | incomplete: some scenario could not be collected | `RESULT status=failed target=e2e-collect scenarios=N sessions=N collected=N` |

Usage is exit 2 here, as in `e2e-bundle`; the `session-*` verbs use 3.

**`RESUME=1` never fetches a collected scenario again.** `<scenario>/host-half.json` with
`collected: true` counts only if every file the collector writes for that scenario is present and
well shaped; otherwise the scenario is collected again. A counted scenario is skipped: no request,
not a byte of it changed, whatever the stack now answers. `manifest.json` and `builds.json` are
reconciled from the scenarios' states at the end of every run, so an interrupted run is repaired by
running again. A scenario's files are committed together: all are written to temporary names
first and renamed into place together, `host-half.json` last, so a failed write leaves the
scenario as it was and a retry merges with the client's rows only. The settings are **as read
at collection**, not as they were when the session ran (a host can be changed between):
`host-half.json` records `read_at_ms`. The builds are read when a scenario is collected; a failed
build read is not retried by `RESUME=1`, only by collecting a fresh copy.

### What the host half adds

| file | holds |
|---|---|
| `<scenario>/diagnostic-bundle.json` | the session's diagnostic bundle, as `GET /v1/admin/sessions/{id}/diagnostic-bundle` returned it for the session's window (addresses and credentials redacted). A session over one read (about 10 minutes) is folded: series and events together, `derived_windows` and `agent_adaptation` concatenated, `classifier` the first read's and `classifier_reads` every read's with its own window |
| `<scenario>/session.json` | the session row, in the run-directory shape `bench_submit.py` reads (`launch`, `fps`, `bitrate_kbps`, `codec`, `rungs`, `host_id`, timestamps), plus `transport`, `negotiated_codec` and `pyrowave` as the row names them |
| `<scenario>/conditions.json` | `effective`: the host's effective settings as read at collection, projected as `bench_run.sh`'s `flag()` does (a value that reads `true` or `false` in any case becomes `on`/`off`; the ladder aliases are added) |
| `<scenario>/metrics.jsonl` | the session's raw samples (every key, string-valued ones too, at their own timestamps) **merged** with the client half's own rows: same source and `ts_unix_ms` is the same row, joined key by key, the client half's value winning a key both have; sorted by ts then source. A key the metric manifest lacks, or a source the format lacks, is dropped and listed in `host-half.json` |
| `<scenario>/trace.json` | the session's stored trace events (as stored, not clock-aligned), merged with the client half's; a clock the client measured is kept when the server's is unmeasured. A second session's own events are in its `sessions/<id>/trace.json` |
| `<scenario>/host-half.json` | `{ "collected": true, "read_at_ms": ..., "node_agent": ... }` (plus `metric_keys_not_in_manifest` and `sources_not_in_bundle` when something was dropped: a name is listed only if it is a plain identifier, any other is counted in `other_dropped`; `"trace": "the session never started"` for a launch failure), or `collected: false` with the `reason` |
| `builds.json` | `components.control_plane` from `GET /v1/admin/platform/identity`, `components.node_agent` from the host row's `agent_version`/`source_commit`/`built_at` (`GET /v1/hosts/{id}`, which answers `{"host": ...}`); a build that cannot be read is `null` with its reason in `unknown`, replacing whatever the client half held; every other component is left as the client half wrote it |
| `manifest.json` | `halves.host: true`, only when every scenario is collected |

Every file the collector writes goes through one function: credential-named fields
(`authorization`, `cookie`, any `*token`, `password`, `secret`, `api key`, `private key`, in any
case or spelling) are dropped, IPv4 addresses become `[address]`, and the exact bearer is
replaced in every value. The file is written beside its target and renamed over it, so a link
already there is replaced, not followed. A value from an answer is never echoed into a `reason` (a reason names the route path, never its query). Every field the collector uses is shape-checked before anything is written: a wrong type makes that scenario `collected: false`.
Limits: a bearer an answer encodes (base64, URL-encoded, split) is not recognised, and a local
user racing the filesystem during the run is out of scope.

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

- **Shape.** The four top-level files and every `scenario.json` are JSON objects. Required: in the manifest `format`, `suite_run_id`, `plan`, `started_at_ms`, `ended_at_ms`, `runner`, `halves`, `scenarios` (directory names, no path) and `not_recorded` (each entry a `fact` and a `why`); `client.kind`; `builds.components`; `network.shaping`; in a scenario `id`, `params`, `sessions` (a list of strings; empty means nothing to collect), `status`, `clock_offset_ms` and `criteria`. A scenario that was not skipped has a `timeline.jsonl`. Every line of `timeline.jsonl` and `metrics.jsonl` is an object; a sample has `ts_unix_ms` and `metrics`. A bundle holds regular files only, no links. The other fields in the table are not checked yet.
- **Status** is `pass`, `fail`, `error` or `skipped`. `skipped` and `error` carry a `reason`.
- **A criterion** has `id`, `pass` (`true`, `false`, or `null` for "not judged"), `observed` and `expected`.
- **Not measured is `null`, never `0`.** A criterion that was not judged has `observed: null`. A fact listed in `not_recorded` has no value as a criterion or as a sample.
- **Keys are the metric manifest's.** An `agent` or `browser` sample key must be in `docs/session-trace/metrics.json` (bench-mode keys ride in `browser` samples, with the six keys `scripts/dx/bench_app_samples.py` derives: `bench_n`, `bench_decoded`, `i2p_p50_ms`, `i2p_max_ms`, `i2p_n`, `ts_nudged`). Sample sources are the five quasar-bench accepts: `agent`, `browser`, `app`, `netem`, `harness`.
- **Every timestamp names its clock.** A timeline row says `client` or `host`; a sample's clock is its key's clock in the metric manifest. `clock_offset_ms` is the measured client-to-host offset, or `null`.
- **A missing build has a reason.**
- **No address, token or password** in any text file, whatever its name. A JSON field whose name says it holds a credential (`authorization`, `cookie`, any `*token`, `password`, `secret`, `api key`, `private key`; any case, hyphen or camelCase) and whose value is not a "there is none" word is a credential too. A bundle's text is UTF-8: a file whose first two or four bytes are a UTF-16 or UTF-32 byte-order mark is not valid, and any other file with a NUL byte in its first 4 KB is taken as binary (a picture, a video, an archive) and not read. Documentation addresses (RFC 5737) pass. Known limits: the address check knows IPv4 only, reads a four-part version such as `1.2.3.4` as an address, and lets `x.0.0.0` and `127.0.0.1` through (a browser's version looks like the first); a bearer token made of letters only is not seen, nor is UTF-16 text with no byte-order mark (it reads as binary); host names and home paths are not looked for (a session container's own home is a real path in app logs). `scripts/dev/leak-scan.sh` with the operator's patterns still runs before a bundle goes anywhere public.

The test (`node --test scripts/validate/stream/bundle.test.mjs`, part of `make verify`) breaks the
fixture once per rule above. The collector's test drives it against a fake API as an in-process
`fetch` — no sockets, no stack — and checks a client-only bundle comes out complete and valid,
that a second collection (`RESUME=1`) changes no byte and a default one is refused, that a failed
write leaves the scenario as it was, and that a 404, an address and the bearer are each handled as
described above.
