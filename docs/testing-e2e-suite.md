# The end-to-end streaming test suite

A fixed list of scenarios run against a Quasar stack from a test client, with no model in the
loop, leaving one **run bundle** that an agent or a person reviews afterwards. Spec: #1271.

> **Status.** The bundle format and its validator (#1272), the runner (#1273, `make e2e-run`), the
> host half of a bundle (#1274, `make e2e-collect`), the review that summarises a bundle (#1275,
> `make e2e-review`) posting a run to quasar-bench (#1276, `make e2e-post`), the client half as one archive for a
> remote tester (#1280, `make e2e-pack`) and two bundles compared (#1284, `make e2e-compare`) exist.
> Serving a web build to the browser on the stack's own origin and the UI journeys in a plan are the
> spec's later tickets (#1284); this page grows with them.

## What you need

- Node 22. The validator, the review and the collector have no dependencies.
- To run a plan (`e2e-run`): Chrome or Chromium, `npm ci` in `scripts/validate` (the verb does it on first use, without your sign-in in its environment), and the stack's URL.
- A test user: an ordinary user's sign-in in `QUASAR_E2E_EMAIL` and `QUASAR_E2E_PASSWORD`, or in a file of `KEY=value` lines named by `QUASAR_E2E_CRED_FILE`. No admin token is needed or used. The password and the e-mail's local part must each be at least 8 characters: a shorter secret cannot be removed from the bundle without damaging unrelated values, so the runner refuses to start with one (exit 2). The account must not have the Classic layout on (the runner needs the game page).
- The reference app in that user's library (`QUASAR_E2E_APP` names it, default `Quasar Benchapp`).
- `CLIENT=<label>`: a name for this test client, by role (`mac-test-client`), never a host name. Letters, digits, `_`, `.`, `-`.
- A stack with a self-signed certificate: `INSECURE_TLS=1`. Certificate checks are on otherwise, for the runner's own requests and for a browser it launches, whatever `NODE_TLS_REJECT_UNAUTHORIZED` says in your environment (the runner removes it, and the verb keeps it out of `npm ci`); `network.json` records what was in force.
- The host half (`e2e-collect`) also needs an admin bearer for the stack, and a certificate Node trusts (the fleet's certs are; set `NODE_EXTRA_CA_CERTS` otherwise).

## What to do

Run a plan from the machine you are testing from:

```
make e2e-run TARGET=<stack url> PLAN=smoke [CDP=<DevTools http endpoint>] [OUT=<dir>] [CLIENT=<label>] [INSECURE_TLS=1]
```

`PLAN` is a name under `scripts/validate/stream/plans/` or a path to a plan file. The runner works
in a browser context of its own: with `CDP=` it attaches to a browser that is already running
(the Mac test client) and opens a separate context in it, a separate storage partition in the same
real browser, so the person's tabs, storage and sign-in on the stack are never read or written; it
closes that context at the end and nothing else. Without `CDP=` it launches `CHROME=` (or
Playwright's Chromium). Each `stream-hold` signs in through the login form, opens the game page,
asks for the codec, frame rate and resolution through the page's own Launch options ("Change"), and
presses Play now, with the product's `?transport=` and `?codec=pyrowave` switches and bench mode
on. It holds, then stops the session.

The runner owns what it created and nothing else. The sessions it stops are exactly the ids in the
`201` answers to `POST /v1/sessions` that its own page received from the stack's own origin: not an id
in the page's address (the address is used to follow a session only when that id is already one of
those; any other is that scenario's `error`, and that session is left alone), not a list, not a
baseline. Cleanup runs on every exit path, `Ctrl-C` included, in this order: close the page, wait for
every answer it was still delivering, stop the owned set, and again for any that arrived meanwhile
until the set is stable, at most 10 rounds (then every id not stopped is named and the scenario is
`error`; the bundle is still written). Before it stops an id the runner reads that session from the
API as the test user; an id the API answers 404 for is not shown to be theirs, so it is not deleted,
the timeline says `stop-unconfirmed` with the id, and the scenario is `error`. A session is `stopped`
in the timeline only after the API says it is over, and every confirmed owned id is in
`scenario.sessions`. Anything that could not be put back (a stop not confirmed, a page that would not
close, or any write the page sent to the stack: every method except GET, HEAD and OPTIONS, outside
`/v1/sessions` and `/v1/auth/`, bar two exceptions) makes the scenario `error` with a reason that names
exactly what was left behind (method and path, never a body). The two exceptions are exact method and
path: `PUT /v1/me/steam-warm` and `POST /v1/me/devices`. Signing in as the test user therefore has the
product's own two side effects: the library's keep-alive (`PUT /v1/me/steam-warm`, once a minute) and the
device record every sign-in posts (`POST /v1/me/devices`). The keep-alive can leave a parked Steam
session when Hot Loading is on, which a later launch claims or the stack reaps. While the page's own
bearer is not yet known, nothing the page said is written (an error's words included; the reason names
the step that failed instead), and at most 2000 lines or 1 MB of it are held; the timeline says how many
lines were dropped.
**Known gap:** the game page's Play remembers a non-default choice as that game's own settings; today
that call is a stub (`web/src/pages/app/game/gameData.ts`, no control-api route), so nothing is
written. The day it is real the runner will see the write, report the scenario `error`, and cannot
put it back: it needs an API read for those settings first. `OUT` must not exist: the bundle is built
in a staging directory beside it, checked with the validator, and only then renamed to `OUT`; a bundle
that fails the check is deleted, its problems are printed, and the run exits 2.

A plan is strict. The plan has `name` and `scenarios`; an entry has `scenario` and `params`. A key the
runner or the scenario does not define is that entry's `error` (an unknown top-level key is a usage
error, exit 2), so a typo cannot silently select a default. `params` may be omitted. The defaults of
`stream-hold`: `transport: "webrtc"`, `codec: "h264"`, `profile: "1080p60"`, `hold_s: 90`, `app`: the
`QUASAR_E2E_APP` app (`Quasar Benchapp`), `bench: true`, `cover_qr`: true for an app named `Steam`.

The run ends with one line:

```
RESULT status=ok|failed|error target=e2e-run bundle=<dir> pass=N fail=N error=N skipped=N [reason=nothing-passed]
```

Exit 0 only when at least one scenario passed and none failed or errored (skipped ones are
counted, never hidden); 1 when one did, and also when nothing passed (`reason=nothing-passed`); 2 for
a usage error, a bundle that does not pass the check below, or a browser context that would not
close. Start reading at `<bundle>/<NN>-stream-hold/scenario.json`: its `criteria` each say `pass`,
`observed` and `expected`; `pass: null` means it was recorded or could not be judged here (and
`observed` is `null`). A plan entry that cannot be run (an unknown scenario, a `hold_s` that is not
a positive number, a codec the runner does not know) is an `error` with the reason before anything is
launched; the bundle is still complete and the run goes on.

`stream-hold` criteria, each meaning what its name says:

| criterion | judged as |
|---|---|
| `first_frame_s` | the first frame the client decoded came within 30 s of the Play click (the runner's budget, not a product limit). It can still be under the loader: the loader's own reveal comes later and is recorded as the timeline step `revealed`, not judged. A frame is the bench ring's first frame or the visible `<video>`'s first frame; the loader is never one. A scenario with `bench: false` on WebTransport (a canvas) cannot see a frame and is `error` before launch ("bench mode is needed to see a frame on this transport"). No frame by the deadline is `pass: false`, and a scenario is never `pass` without this one |
| `marker_decoded` | the bench instrument decoded at least one marker frame (`stats().decoded > 0`) |
| `transport_as_asked` | the session's `transport` is the asked one and `transport_fallback_reason` is empty; `observed` carries the transport and any `transport_reason` or fallback reason |
| `codec_as_asked` | `negotiated_codec` (or `stream.codec`, or `pyrowave.active/used`) is the asked codec; `observed` names the field read |
| `profile_as_asked` | the asked height and frame rate against `stream.height` and `stream.fps`, the launch size (not `external_*`). It is not a profile id: the Launch options choose a codec, frame rate and resolution. Only the resolutions the sheet lists can be asked |
| `frameless_seconds` | the number of one-second bench windows with no frame at all, after the warm-up, excluding any span the page was hidden. Not Chrome's `freeze_count`; a stall shorter than a window is invisible to it. It is the bench callback's view: a stall of the instrument itself also counts (a false fail, never a false pass). It needs no marker |
| `missing_indices_rate` | recorded, not judged (`pass: null`): `missing / (decoded + missing)` over the judged span is in `expected`. No limit has been calibrated on the marker app (the first 60 s of a healthy bench cell read 6.95 % in `docs/testing-bench-mode.md`) |
| `present_cadence` | the control plane's hitch rule on the bench ring: per one-second window with at least `web.present_cadence.min_samples` present intervals, sd against `classifier.hitch_sd_ms`, a hitch at `classifier.hitch_min_samples` such windows. It needs no marker. When the bench callback saw under 90 % of the frames the video decoded over the span it is not judged, because the instrument set the cadence (issue #1304); `expected` carries the share as "N of M frames" |

The warm-up is one value for every criterion that has one: `classifier.warmup_exclude_s` in
`docs/session-trace/thresholds.json` (20 s). The smoke plan holds 90 s so the judged span is at
least 60 s after it. A scenario is `pass` only when the first frame was seen and no criterion failed;
if the first frame could not be judged it is `error`; a session that is already `stopped` or `failed` when
the hold ends is `error` ("the session ended during the hold"), whatever its criteria say. A number
the run does not have (a negative first-frame time, a bench window whose `n` or `no_image` is missing,
negative or not finite) is `null`, never a clean zero. `params.bench: false` leaves bench mode off.
**Use a fresh test user for each run.** A client that cannot keep up at a profile is refused it
afterwards (`historical_client_performance_failed`; the scenario ends `error: the launch was
refused: 409 ...`), and on the Mac test client one bench-armed 60 s hold at 1080p60 was enough (a
hold with `bench: false` was not). A scenario the client cannot run (no WebTransport, a codec it
cannot decode, the app not in the catalog) is `skipped` with the reason.

### Hooks

A scenario that needs a step it cannot do itself, such as cutting the WebTransport path or shaping the
network, names it as a hook (`fault:webtransport-block`, `shape:<level>`: lower-case word, a colon, then
letters, digits and dashes). You supply the scripts; the repository ships none, because a real one knows a
lab address (`scripts/validate/stream/fixtures/hooks/` has two that only print what they were asked).

```
make e2e-run TARGET=... PLAN=... HOOKS=<dir>        # or QUASAR_E2E_HOOKS=<dir>
```

- **Script.** `<dir>/<name with ':' as '-'>` (`fault:webtransport-block` is `<dir>/fault-webtransport-block`),
  an executable file inside `<dir>`. It is run with one argument, `apply` or `clear`, no shell, 30 s to
  finish (then it and its children are killed). Exit 0 is success; anything else, or the timeout, is a
  failure. Its environment is yours, without the test user's sign-in (`QUASAR_E2E_EMAIL`,
  `QUASAR_E2E_PASSWORD`, `QUASAR_E2E_CRED_FILE`), plus `QUASAR_E2E_SCENARIO` (the scenario directory name),
  `QUASAR_E2E_SESSION` (only once a session was launched), `QUASAR_E2E_TRANSPORT` and `QUASAR_E2E_PHASE`.
  `clear` must be safe to run when `apply` did nothing or only half of it.
- **Timeline.** Each run is a `hook` row: hook, phase, exit code, `timed_out`, milliseconds, and the first
  2000 characters of its output, redacted like everything the bundle holds.
- **Cleared on every exit path.** The scenario asks for `apply` when it needs the fault, so it can be
  applied mid-stream. Whatever a scenario does not clear itself, the runner clears when the scenario ends,
  however it ended (a throw, Ctrl-C, SIGTERM). A clear that fails makes the scenario `error`, names the
  hook under "left behind", and the following scenarios are `skipped` until it clears.
- **A runner killed between apply and clear** (`kill -9`) cannot clear. Before each apply it writes
  `.e2e-hooks-applied.json` in the directory that holds `OUT`; the next run with the same `HOOKS=` and
  an `OUT` in the same directory runs `clear` for every entry first and says so on stderr. A run with
  no `HOOKS=` that finds the file only warns: it cannot clear anything. A hook that was still running
  when its runner died keeps running; the next run waits for it to end (up to the hook's 30 s) before
  it clears, and does not start if it does not end. If one will not clear, or the process that wrote it is
  still running, the run does not start. A damaged ledger also stops it: remove the file once the hooks are
  known to be clear.
- **No script.** A scenario whose hook has no executable script (or no `HOOKS=`) is `skipped`; the reason
  names the hook. Nothing is launched.

The hooks directory is yours and trusted. The plan is not: it can only choose a scenario and its params,
never a script.

`client.json` carries the label, the browser, OS and CPU (from client hints; `user_agent` is Chrome's
reduced string), the GPU (the unmasked WebGL renderer where the browser offers it), decode support per
codec (`supported`, `power_efficient`, and WebCodecs), `power` (`null` when the Battery API only reports
a full, charging battery, which is also what a machine with no battery says), and `clock`: the
browser's clock minus the runner's, measured around one round trip. Timeline stamps are put on the
browser's clock with it, and each row says `clock_source`.

The runner reads only what the product exposes (`window.__qBench`, the page, the API) and wraps no
browser API. What it cannot see is in `manifest.json` `not_recorded`, each with a reason. The
issue also asked for a decoded-frame grab every 10 s, the client's own telemetry and the bundle
attached to the issue; none is done (the screenshot every 10 s stands in for the first, the bench
windows for the second, and posting is `e2e-post`, #1276).

**Redaction.** Every secret the run holds is an exact literal removed from every string before
anything is serialised: the password, the e-mail and its local part, the stack's host name, the bearer
the API sign-in returned and the one the page stored after its own sign-in, each in three spellings: as
is, URL-encoded and JSON-escaped. The validator's patterns for addresses and tokens run after that as
defence in depth. **Limits:** other encodings of a secret (base64 and the like) are not looked for. A
key name is never rewritten (a member named like a credential, `password`, `token`, `authorization`,
`sig_token`, ..., or equal to a secret, is dropped), and two keys the patterns would make equal make
the bundle invalid (exit 2) rather than merge them. What the page says (console lines, errors, API
answers' bodies) is held in memory until the page's own bearer is known and written only then; if the
scenario ends before that, it is dropped and the timeline says how many lines
(`pre-auth-evidence-dropped`). The manifest's `redacted` names the kinds replaced. A picture is binary
and is not scrubbed: the runner only photographs the stream page.

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
is collected: its row (with `has_failure_code` and `has_error_message`; the code and the message
are text the agent chose and are not copied, read them on the stack), its conditions and the
agent identity, no trace; `host-half.json` says
`"trace": "the session never started"`.

**What is and is not stripped.** Credential-named fields are dropped, the admin bearer and
addresses are replaced wherever they appear, and free text an answer carries (an event's
message) is passed through the validator's secret patterns. That last step is best effort: no
pattern finds every credential in free text. So every string the collector projects itself into
`session.json`, `conditions.json`, `host-half.json` and `builds.json` passes a shape check for
its own field or is left out, and the count of what was left out is written in that scenario's
`host-half.json` `not_projected` (`{"session": 2, "conditions": 1, "identity": 0}`, absent when
nothing was); a failed session's code and message are never copied; a host path setting is written as
`[omitted]`. `scripts/dev/leak-scan.sh` with the operator's patterns still runs before a bundle
goes anywhere public.

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
| `<scenario>/host-half.json` | `{ "collected": true, "read_at_ms": ..., "node_agent": ... }` (plus `metric_keys_not_in_manifest` and `sources_not_in_bundle` when something was dropped: a name is listed only if it is a plain identifier, any other is counted in `other_dropped`; `"trace": "the session never started"` for a launch failure; `not_projected` when a projected value had the wrong shape and was left out), or `collected: false` with the `reason` |
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

Post it on quasar-bench:

```
make e2e-post BUNDLE=<bundle-dir> HOST=<role> [DRY_RUN=1]
```

**What you need.** A collected bundle — its `builds.json` must name the node agent's commit, so
run `e2e-collect` first if it does not — the role name of the host it ran on (`HOST`; the bundle
holds no host name, so the role is the caller's word), and the bench server and key `bench-submit`
uses (`BENCH_URL` / `BENCH_KEY`, else qbench's own config). `scripts/dx/bench_submit.py` reads
those; this verb never sees them.

**What it does.** One quasar-bench run per scenario whose status is not `skipped`, through
`scripts/dx/bench_submit.py`, so the server and the API are the ones that script already posts to.
The bench scenario is the scenario's name, then `transport`, `codec` and `profile`, then every
other param of the scenario in sorted key order, as `<k>=<v>` joined with `/`
(`transport-fallback/transport=webtransport/codec=h264/profile=1080p60/when=mid`): the server
compares the newest run per suite, scenario and host, so two runs that measured different things
must not share a name. `/` and `=` are the separators because an identifier and a UUID hold
neither, so a scenario's own name can never spell another scenario's params. `app`, `bench`,
`cover_qr` and `hold_s` are not in the name — they do not change what the bench compares, and
`hold_s` is a tag — and a param takes part only when its value is an identifier, a finite number
or a boolean; anything else is left out and counted in `left_out=N`. The whole name may not pass
200 characters: a longer one is refused (`failed=N`), never truncated.
Each run is created with `repo=accreleus/quasar` and `commit` the node agent's commit from
`builds.json` — never this checkout's HEAD, which is why a bundle without one is refused rather
than posted under the wrong build. The tags are `suite_run` (the suite run id), `client` (the label
from `client.json`), `transport`, `codec` and `profile` (the scenario's params), `hold_s` (how long
it ran) and `clock=raw`: the collected `trace.json` keeps client-clock browser events, so these runs
are not comparable with host-aligned ones event for event. `git_quasar` is the bundle's commit and
`git_protocol` is `unknown` (the bundle does not hold the build's protocol pin, and the posting
checkout's would be a false fact). **`bench_submit.py` adds its own**, and posts them too: from
`session.json` the tags `launch`, `height`, `fps`, `codec`, `profile`, `session_id`,
`quasar_host_id`, `launch_bitrate_kbps` and `soak_profile`, and `source_dir` (the two `git_` tags
above win over its own); the files it uploads from the scenario directory — `summary.json`,
`steps.jsonl`, `marks.jsonl`, `session.json`, `conditions.json`, `verdict.json`,
`caps-negotiated.json`, and any `REPORT.md`, `LADDER.md` or other `.md` there; and the run's own
samples and events, read from `metrics.jsonl`, `trace.json`, `marks.jsonl` and `steps.jsonl`. The
run's notes carry the criteria, one line each, `<id> pass|fail|not-judged <observed>`;
`expected`, a scenario's `reason` and any other free text are not copied into the notes or the
tags. They do reach the server inside the artifacts: this verb adds `review/summary.json`, the
scenario's `scenario.json` and `timeline.jsonl`, its `shots/*.png`, and on the first run only a
`tar.gz` of the whole bundle. The verdict is PASS for a `pass` and FAIL for a `fail` or an `error`;
an error also carries the tag `status=error`. A failed scenario is posted, never withheld, and is
not marked invalid. A skipped scenario posts nothing.

**The host pairs the runs.** A run's host is `<HOST>@<client label>`: the server takes the host
string as a whole, so two test clients on one host
never compare with each other. **Posting twice updates the same runs** — `bench_submit.py` upserts
on an external id built from the scenario's session id and the suite run id, so a re-post converges
instead of forking a second run. A scenario that never had a session (an error before the launch)
has no `session.json`; its id comes from the directory's path, so post it from the same place.

**A value taken from the bundle is used only in the shape its field has** (an identifier, a UUID, a
commit); one that does not match is left out and counted in `left_out=N`, so a tag is never free
text. The bundle is validated before anything is posted, and every value reaches
`bench_submit.py` as its own argument, never through a shell. A scenario is not posted
(`failed=N`) when its name is not a name, when its bench scenario name would pass 200 characters,
or when its `session.json` does not carry one of the scenario's own session ids in the lower-case
form the collector writes (an id in another case is another external id, so a re-post would fork a
second run), or holds a codec, profile, host id, `encoder` or `profile_id` that is not a plain
name: the script builds the run's identity, its tags and its `conditions.negotiated` from that
file. If the bundle cannot be archived, nothing is posted (`reason=no-archive`).

The run ends with:

```
RESULT status=ok|failed|error target=e2e-post posted=N skipped=N [failed=N] [left_out=N] [warn=N] [dry_run=1] [reason=...]
```

Exit 0 every scenario posted (or planned, with `DRY_RUN=1`), 1 the bundle is not valid
(`reason=invalid-bundle`), the build has no commit (`reason=no-commit`), the archive could not be
made (`reason=no-archive`), a scenario was refused (`failed=N`, nothing posted for it) or a post
failed, 2 usage. A run `bench_submit.py` posted but whose tags disagree with what the host actually
did (its exit 3) is counted as posted and as `warn=N`, and the verb still exits 0: the evidence is
there, and it is not re-posted.

`DRY_RUN=1` runs `bench_submit.py --dry-run` for each scenario: it posts nothing (with a bench
server configured the script still asks it for its version) and prints each run's plan, tags and
external id — the way to see what a bundle would post. With a bench address set but no key it
fails instead: the plan is refused before anything is posted, because the script needs the key even
to plan, so the verb exits 1 with `failed=N`.

### Comparing two run bundles

```
make e2e-compare A=<bundle-dir> B=<bundle-dir> [OUT=<dir>]
```

**What you need.** Two run bundles of the same plan — two web builds on one stack, two commits, two
test clients, or two runs of one build — and Node 22. No stack, no token, no bench server: this verb
reads two directories and nothing else. Both bundles are validated first, and an invalid one is
refused (its problems are printed and nothing is written).

**What it does.** It writes `COMPARE.md` and `compare.json` into `OUT`, a new directory beside `B` by
default (`<B>-compare`). Nothing is written into `A` or `B`, and an `OUT` that exists is refused, so a
comparison is never overwritten. Neither file holds a clock reading or an absolute path, so the same
two bundles give the same bytes out, in any directory.

- **First, what differed about the runs**: one table of the field, each side's value and whether they
  are the same — every build component's version, commit and whether it could say; the client's label,
  kind, browser, OS, GPU and decode support per codec; the plan, the runner's commit and dirty flag,
  the suite run and its times; the network path and shaping. When only the facts of one run differ
  (a second run of one build), it says so in one line; when nothing does (a bundle compared with
  itself), it says that.
- **Then, per scenario**: the criteria. Scenarios pair by the identity `e2e-post` would post them
  under — the scenario's name, then `transport`, `codec`, `profile` and every other param — so two
  scenarios pair exactly when quasar-bench would compare them, and a scenario in only one bundle is
  listed, not compared. Every criterion id in either side is one row: A's verdict and observed value,
  B's, and a class — `same`, `regressed` (true to false), `fixed` (false to true), `now_judged` or
  `no_longer_judged` (to or from not judged), `observed_changed` (same verdict, a different value), or
  `only_a` / `only_b`.
- **Then the metric deltas**: for every key `docs/session-trace/metrics.json` defines for a source
  both sides carry, the median on each side, B minus A and the percent change, sorted by the size of
  the change. The medians are read after the same warm-up the review judges a scenario's samples
  after (`classifier.warmup_exclude_s` in `docs/session-trace/thresholds.json`, after each side's own
  first browser sample), so the first seconds of a run do not decide the number. A key with fewer
  than five samples on a side is listed as too few, not compared.
- **Then the pictures**: `shots/<label>.png` paired by label, whether the bytes are identical, and
  the two sizes; a picture only one side took is listed.
- **Last**, the one line this unit does not do: serving a web build (`WEB_DIST=`), UI journeys in a
  plan, decoded-frame pairs at the same reference-app tick, and a pixel difference in pictures.

**How to read the result.** Read `COMPARE.md` first: what the runs differed in, then what moved. It
carries **no judgement and no threshold** — the numbers are the report — because one pair of runs is
not evidence of a regression: quasar-bench's rule is at least two runs a side. `compare.json` is the
same comparison as data, for a script: it holds only values in a known shape (an identifier, a UUID,
a commit, a version, a finite number, a boolean, null, and the class words), and a value that fits no
shape — free text, an object — is left out and counted in `left_out`. `COMPARE.md` still shows those
values, as data in one-line code spans, so bundle text can never become a heading, a table row or a
link. At most 100 scenarios, 200 criteria and 500 metric keys per scenario are compared; over that the
page says it was truncated.

```
RESULT status=ok target=e2e-compare changed=N scenarios=N only_a=N only_b=N [left_out=N] [truncated=1]
```

`changed` counts the criteria whose class is not `same`, `scenarios` the pairs compared. Exit 0 both
bundles are valid and the comparison was written (whether or not anything changed), 1 a bundle is not
valid (`reason=invalid-a` or `invalid-b`), 2 usage, or a comparison that cannot be written
(`reason=out-exists`, `out-inside-bundle`, `cannot-write`). A bundle zip is not read: unzip it first.

### The fallback and keyboard-and-mouse scenarios

```
make e2e-run TARGET=... PLAN=fallback HOOKS=<dir>     # needs the hook fault:webtransport-block
make e2e-run TARGET=... PLAN=input
```

Neither is in `smoke`. Both launch the way `stream-hold` does and take its `transport`, `codec`, `profile`, `app`,
`bench` and `cover_qr`; `first_frame_s` is judged first and a scenario is never `pass` without it. A criterion
with no evidence source today is `pass: null` and its `expected` says what is missing: nothing is invented and
no telemetry key, trace event or stats field is added for it.

`transport-fallback` takes `transport: webtransport` and `when`: `mid` (default; the pointer lock is taken and a
key held, then the hook is applied while streaming) or `connect` (applied before the launch). `hold_s` is how
long it waits for the fallback and a frame: counted from the fault in `mid`, from the first frame in `connect`
(the fault was already on). A `mid` run whose session had already fallen back before the fault (the session row
has a reason, the page already posted the event, or the `<video>` is already decoding) is an `error` that says so
and the hook is not applied; only evidence from after the fault counts. That check runs before the pointer lock is taken and the key held, so such a run touches nothing, and a session row it cannot read just before the fault is an `error` that says so. The key is held only when the lock is held. With no first frame the fault is never
applied (`mid`) or nothing is watched after it (`connect`), so the criteria after the fault are `pass: null`.

| criterion | judged as |
|---|---|
| `webtransport_closed_first` | `pass: null`. The page does not expose when its WebTransport session closed, the trace has no event for it, and the agent logs nothing of its own for a lost WebTransport client. Needs a client trace event or bench field for the close time, or an agent log line, in the host half |
| `continues_on_webrtc` | after the fallback the session is not `stopped`/`failed`, and the `<video>` (the WebRTC path; WebTransport draws on a canvas) decodes frames across a 5 s tail |
| `fallback_event` | the session row's `transport_fallback_reason` (the control plane writes it from the `transport.fallback` event), or that event as the page itself posts it to the stack, has a reason matching `^[a-z0-9_]{1,64}$`; `observed` says which source |
| `frame_after_fallback` | the `<video>` decoded a frame after the fault went on (it had none before it); `observed` is the seconds from the fault and, in `mid`, how long no frame advanced (see below) |
| `held_key_released` | `pass: null`. A key held across the fallback (`key-held`, then `key-released` by the runner at the end) is the test; the client cannot see the host's key state. Needs the host half (an agent input log) or the reference app logging individual keys. In `connect` nothing is held: no input is captured before the first picture |

`fallback.json` also records why the wait ended (`wait_ended_by`: `fallback_and_frame_seen`, `hold_elapsed` or `session_ended`; the same in the timeline step `wait-ended`) with the seconds waited (`waited_s`), and, in `mid`, `no_frame_advance_before_video_s`: the longest run of one-second samples between the fault and the first `<video>` frame in which neither the bench frames nor the `<video>` advanced (about the seconds the picture froze; one-second resolution; when no `<video>` frame came it runs to the end of the wait). It is `null` in `connect`, when no sample was taken (the session ended at once), when a count could not be read, and once the bench's frame ring is full (5000 frames), because its count then stops growing. It is a fact, not a criterion, and has no threshold. `fallback_event` and `frame_after_fallback` say where their wait starts: the fault in `mid`, the first frame in `connect`.

`input-kbm` sends one fixed sequence (`SEQUENCE` in the scenario file: six keys, the three mouse buttons, wheel up
and down, relative motion) as trusted events through the DevTools protocol (`Input.dispatchKeyEvent`,
`Input.dispatchMouseEvent`). The pointer lock first: the product takes it from its "Click to play" layer (a click on
the stage only opens the HUD shelf), so the scenario waits for that layer and clicks it once with a trusted click (the
product removes the layer on the first click), then waits for the lock. With no layer within the wait the launch was not
immersive, and it tries the HUD's own route: a trusted click on the stream, which opens the shelf, then a click on the
shelf's "Capture input" button (the product's `aria-label`); `pointer_lock_taken`'s `observed` says which route held the lock
or why none did. A key or button held when the run is cut is released on the way out; a release that fails is named
in "left behind". `hold_s` is the pause after the sequence. `input-sent.json` holds what was sent (in order, with `sent_motion_sum`) and what the page
received (each `mousemove` as `[movementX, movementY, isTrusted]`, at most 200, with the total count and `motion_sum`; the motion
before the sequence starts is not counted), for the host half to compare. The moves sum to (50, 25), not zero, so no motion and
motion that cancels are different bundles.

| criterion | judged as |
|---|---|
| `pointer_lock_taken` | `document.pointerLockElement` is the stream surface after the click on the "Click to play" layer, or on the HUD's "Capture input" button when the launch was not immersive; `pass: null` when no first frame was seen (the sequence was not sent) |
| `keys_match`, `buttons_match`, `motion_sum_matches` | `pass: null`. The reference app does not log individual keys, buttons or motion (its echo is one bit) and the runner cannot see the page's input channel. Needs the app's own input log on the host. `motion_sum_matches`'s `expected` also says how many `mousemove` events the page received, their sum, and the sum sent |
| `nothing_held_at_end` | every key and button the page was sent down was also sent up, as the page received them. The product's own held-key set is not exposed, so a key the page failed to release on the host is not seen here |
| `input_to_photon_ms` | `pass: null` (no limit is calibrated); `expected` carries the count, median and max of the samples from eight `__qBench.pressKey` rounds. A sample that is not a finite number is not counted; none at all says the marker echoed no key |

### The pack for a remote tester

`make e2e-pack [OUT=<path without extension>]` writes the client half as one archive, `OUT.tar.gz`
and `OUT.zip` (the same files; Windows opens the zip, macOS and Linux either), 70 to 85 KB. It holds
the runner and every file it imports (worked out from the code, not listed by hand), the plans,
`thresholds.json` and `metrics.json`, `package.json` and a lockfile for `playwright-core` alone,
`run.sh` and `run.cmd`, a plain `README.txt` and `VERSION` (the commit it was made from, `dirty`
after it if tracked files differed; a scenario or plan git does not track stops the pack). No
tests, no hook scripts, no host or `.env` file, nothing from `.claude/`. The tester needs Node 22 and Chrome, nothing from this repository.

What the tester does (they are also told in `README.txt`): unpack, make `signin.txt` in the folder
with `QUASAR_E2E_EMAIL=` and `QUASAR_E2E_PASSWORD=` lines (the sign-in is never a command-line
argument and never goes into what comes back), then one command with the stack's address:

```
bash run.sh https://<stack> [<plan>] [--insecure]     # macOS, Linux
.\run.cmd https://<stack> [<plan>] [--insecure]       # Windows (cmd or PowerShell)
```

`--insecure` is `INSECURE_TLS=1`. The first run fetches `playwright-core` with `npm ci` (online,
without the sign-in in that process' environment). Then `doctor` checks, in order, Node, Chrome, the
plan, that the stack answers, that the sign-in is accepted, that the reference app is in the user's
library, that Chrome can open the stack's page (on a Mac a failure here is most often the Local
Network permission, and the message says so) and that this Chrome can decode what the plan asks
(a scenario it cannot run is a note; none that it can is a failure). Each line is `ok` or
`FAIL` with a sentence in plain words. `run.sh doctor https://<stack>` stops there. A failed check
ends the command (exit 1, `zip=` empty); otherwise it runs the plan (default `smoke`, in a headed
Chrome on Mac and Windows, since a desktop there has no `DISPLAY`), validates the bundle with
`validateBundle`, and writes `results/quasar-e2e-<time>.zip`: the bundle and nothing else. Hooks
are not part of the pack, so a scenario that needs one is `skipped`.

```
RESULT status=ok|failed|error target=e2e-pack zip=<file> run=ok|failed pass=N fail=N error=N skipped=N
```

A full run exits 0 only when that zip was written (`reason=no-zip` otherwise), whether or not the
streams passed; `doctor` alone exits 0 when every check passed, with `zip=` empty: the result is in the
bundle, and the tester sends the file back either way. `make e2e-review BUNDLE=<unzipped dir>`
reads it.

## The run bundle, format 1

A directory, zipped when it is handed between machines.

| file | holds |
|---|---|
| `manifest.json` | `format: 1`, `suite_run_id`, the `plan` as run, `started_at_ms` and `ended_at_ms`, the runner's commit (`runner.dirty: true` when tracked files differ from it), which `halves` are present (`client`, `host`), the scenario directories in order, `not_recorded` (each fact with why), what was `redacted` |
| `client.json` | the test client: `kind` (`web` or `native`), browser, operating system, CPU, GPU adapter, displays, decode support per codec, power source |
| `builds.json` | `components`: the exact build of each part (control plane, node agent, web client, app image, reference app, GPU driver, ...). A part that cannot say is `null` and `unknown` gives the reason |
| `network.json` | `path` facts, and `shaping` (`null` when none was applied) |
| `<scenario>/scenario.json` | the scenario as run: `id`, `params`, `status`, `reason` when skipped or in error, `sessions`, `clock_offset_ms`, `criteria` |
| `<scenario>/timeline.jsonl` | one step or event per line: `ts_unix_ms`, `clock`, `ev` |
| `<scenario>/metrics.jsonl`, `trace.json`, `session.json`, `conditions.json`, `summary.json` | the series and events, in the run-directory shape `scripts/dx/bench_submit.py` reads. Posting (`e2e-post`, #1276) adds what that script does not take from the directory: the verdict from `status`, and `scenario.json` and `timeline.jsonl` as artifacts |
| anything else in a scenario directory | pictures, per-frame records, input records, logs, the session's `diagnostic-bundle.json` and `host-half.json` |
| `review/summary.json`, `review/REVIEW.md` | written by `make e2e-review` (below), not by the runner: the summary an agent reads first. A bundle without them is still a bundle |

### Rules the validator enforces

- **Shape.** The four top-level files and every `scenario.json` are JSON objects. Required: in the manifest `format`, `suite_run_id`, `plan`, `started_at_ms`, `ended_at_ms`, `runner`, `halves`, `scenarios` (directory names, no path) and `not_recorded` (each entry a `fact` and a `why`); `client.kind`; `builds.components`; `network.shaping`; in a scenario `id`, `params`, `sessions` (a list of strings; empty means nothing to collect), `status`, `clock_offset_ms` and `criteria`. A scenario that was not skipped has a `timeline.jsonl`. Every line of `timeline.jsonl` and `metrics.jsonl` is an object; a sample has `ts_unix_ms` and `metrics`. A bundle holds regular files only, no links. The other fields in the table are not checked yet.
- **Status** is `pass`, `fail`, `error` or `skipped`. `skipped` and `error` carry a `reason`.
- **A criterion** has `id`, `pass` (`true`, `false`, or `null` for "not judged"), `observed` and `expected`. An `id` is an identifier, not text: ASCII letters, digits, `_`, `.` and `-`, at most 64, unique in its scenario. It is the page's row and the run's count, so nothing that prints like another id (a number beside its string, an accented letter in two encodings, a character that does not print) is accepted.
- **The status and the criteria agree.** A scenario whose status is `pass` has no criterion with `pass: false`: the review derives the run's verdict from both, so the two may not disagree.
- **A pass rests on something.** A scenario whose status is `pass` has at least one criterion with `pass: true`. A pass with zero criteria, or with every criterion `null`, is refused: a short bundle is not a success (spec #1271, story 3).
- **One name, one run.** `manifest.scenarios` holds each directory name once.
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
