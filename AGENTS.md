# AGENTS.md — operating guide for coding agents (and humans)

The **canonical developer interface is the root `Makefile`**. Start every session with:

```
make help
```

Architecture, invariants, frozen contracts, and phase context live in `CLAUDE.md` — read it
first. This file covers *operations*: how to initialize, inspect, run, verify, and diagnose.
The full tooling inventory (every make target, canonical script, harness, and skill, with
when-to-use guidance) is [`docs/developer-tooling.md`](docs/developer-tooling.md).

## Initialize and inspect

```
make init          # idempotent first-time setup: protocol submodule, devtools image, doctor
make doctor        # environment check — docker, go, node, submodule, disk; remote is ADVISORY
make config-check  # every compose file set parses; .env keys diffed against docs (advisory)
make status        # stack state: healthy / degraded / stopped / failed — never guesses
make diagnose      # one page: git state, instance, stack, health, versions, recent errors
```

`make init` never overwrites an existing `.env`. `doctor` reporting `degraded` with an
unreachable remote is normal off-network; it is not a failure.

## Instance isolation (worktrees, agents, parallel sessions)

Every worktree gets its own `QUASAR_INSTANCE` (derived from the worktree path): its own
compose project name, its own 10-port block, its own ephemeral test database. Two agents in
two worktrees cannot collide on containers, volumes, ports, or Postgres. Don't hardcode
ports or container names in new tooling — go through `scripts/dx/common.sh`.

## Start, stop, rebuild

```
make up            # LOCAL agentless stack (postgres + control-plane + web) — UI/API loops
make down          # stop it (volumes preserved)
make rebuild       # local: compose build. Remote: see below.
make redeploy-cp   # ONLY the Go control-plane (~1 min) — see below
make dev-web       # vite dev loop        make dev-cp   # go run against ephemeral PG
```

The **local stack has no node-agent** (needs a Linux GPU host). Real streaming work happens
on the remote GPU host via the `quasar-host` skill (hosts are addressed by ROLE — `gpu-test`,
`aux-infra` — from `.claude/skills/_shared/hosts.json`; no hostnames in prose or scripts).

**Remote mutation is never a default.** `make up|down|restart|rebuild|redeploy-cp HOST=<role-or-host>`
(e.g. `HOST=gpu-test`, resolved from `.claude/skills/_shared/hosts.json`) requires typing
`HOST=` explicitly and delegates to the canonical scripts (`deploy/build-images.sh`,
`deploy/redeploy.sh`). Images are ONLY built via `deploy/build-images.sh` (contract-gated —
never a hand-typed `docker build`, never relax `deploy/image-contract.json`).

**Match the verb to what actually changed.**

| changed | verb | cost |
|---|---|---|
| Rust / GStreamer / the agent image | `make rebuild HOST=<h>` | 40+ min (build-images.sh + full redeploy) |
| Go control-plane only | `make redeploy-cp HOST=<h>` | ~1 min |

`redeploy-cp` runs `deploy/redeploy.sh <profile> <ref> control`: compose-builds the
`quasar-control-plane` service from `deploy/Dockerfile.control` (a Go image sharing nothing
with the `Dockerfile.vulkan` lineage), force-recreates that one service, waits for it to
report healthy — which is what proves an embedded migration finished — and leaves the
node-agent container alone, so running sessions survive. Reaching for `rebuild` on a
Go-only change spends 40 minutes rebuilding Rust and GStreamer that did not change.

Both verbs take `REF=<branch|sha>` and **default to the branch the host is already on**,
never `origin/main`. That default is load-bearing: `redeploy.sh` git-checkouts its `ref`
argument on the host, and its own default is `origin/main` — so a bare call silently reverts
the host off the branch under test, mid-run, while bash is still reading `redeploy.sh`
itself. The DX layer therefore always passes the ref explicitly (`remote_redeploy_args` in
`scripts/dx/stack.sh`); a new caller that hand-rolls redeploy.sh's arguments re-arms the trap.

## Logs and diagnostics — bounded by default

```
make logs S=<service> N=200   # finite, always
make logs-follow S=<service>  # the ONLY streaming command — use deliberately
make diagnose-bundle          # sanitized tar under .diagnostics/ (gitignored, 0700)
```

Bundles pass through a redactor (secrets, URL credentials, PEM, JWTs, bearer tokens) and
carry a MANIFEST listing what was included and what was deliberately excluded (`.env`,
credentials, DB contents, user data). Review before sharing.

## Reading a live session

```
make session-list     HOST=gpu-test                       # what is running
make session-verdict  SID=latest HOST=gpu-test            # the classifier's answer, in four lines
make session-diagnose SID=latest HOST=gpu-test            # the full analysis
make session-metrics  SID=latest HOST=gpu-test SINCE=5m   # fps / encode_ms_p95 / kbps / setpoint / present σ
make session-trace    SID=latest HOST=gpu-test            # trace window + events
make session-bundle   SID=latest HOST=gpu-test            # raw bundle JSON to a file
make session-capture  SID=latest HOST=gpu-test KIND=pipeline_dot   # the live encode graph, to a .dot/.svg
make session-logs     SID=<id>   HOST=gpu-test SINCE=10m  # quasar-sess-<sid>-g<N>, quasar-pulse-<sid>, node-agent
make admin-token      HOST=gpu-test                       # a bearer, for a one-off curl
```

**Reach for these before psql.** Knobs: `SID=<uuid>|latest`,
`WINDOW=<from_ms>,<to_ms>`, `JSON=1`, `SINCE=`, `N=<rows>`, `GREP=`, `OUT=`, and
`KIND=` for `session-capture` (`pipeline_dot|encoder_props|burst_stats|all`).
`SID=latest` is the newest `state=running` session. A **capture** is the one verb
here that asks the host to do something: it arms a bounded observation, is
single-flight per session, and is refused rather than queued — a `501` means that
host's agent predates captures, so `make rebuild HOST=<h>`, not a retry.

Every verb ends with one line, and nothing prompts:

```
RESULT status=<ok|degraded|failed|error> target=session-<verb> sid=<sid> host=<host> [verdict=…] [reason=…]
```

Exit **0** ok · **1** degraded (a `likely_*` verdict) · **2** tool error · **3** usage.
A tool error always names the next command (401 → `admin_token.sh --fresh`;
404 → `make session-list`, because a stopped session keeps its row and a 404
usually means the wrong stack). **A verdict the tooling does not recognise is
DATA**: printed verbatim, exit 0. The control plane owns that vocabulary.

One admin-bearer ladder serves all of it, `scripts/dx/admin_token.sh`:
`$QUASAR_ADMIN_TOKEN` → a cached token in
`${XDG_CACHE_HOME:-~/.cache}/quasar/<host>.token` → one ssh hop that mints on the
host (per-boot dev key, else `BOOTSTRAP_ADMIN_*` from its own `deploy/.env`).
Full detail, including the TLS and cache knobs: `docs/configuration.md`
"Reading a live session". Where a host has a Dozzle MCP endpoint recorded in
`.claude/skills/_shared/hosts.json`, **prefer it over an ssh hop** for logs.

Multi-run work — soaks, run comparison, the ABR/IR/FEC experiment matrices, the
HTML report — stays in the `quasar-diagnose` skill.

## Live-session verbs (adaptive external resolution)

```
make session-display SID=<id> ARGS='--stream 1280x720' HOST=gpu-test
make session-soak    SID=latest ARGS='--duration 180' HOST=gpu-test
```

| Target | Script | What it does |
|---|---|---|
| `session-display` | `scripts/dx/session_display.sh` | one `PATCH /v1/sessions/{id}/display` (stream / render / ui-scale), then prints the resulting `stream.external_*` / `render_*` / `rungs` |
| `session-soak` | `scripts/dx/session_soak.sh` (+ `session_soak_driver.py`, `session_soak_report.py`) | on-demand **bad-connection soak**: walks the EXTERNAL size down the rung ladder and back up over `--duration` (default 180 s) against a session someone is already playing, samples agent + browser telemetry, and writes `REPORT.md` + `summary.json` under `.diagnostics/soak/`. Never sends `render_*`; restores the launch size on every exit path including Ctrl-C. `SID=latest` picks the newest `running` session. Nothing is wired into ABR — manual only. |

## Benchmarks (quasar-bench results service)

```
export BENCH_URL=https://<your-bench-host>:9400 BENCH_KEY=<a BENCH_API_KEYS secret>   # never commit the key
make bench-submit DIR=.diagnostics/soak/<run> ARGS='--suite abr-ladder --scenario 1080p120-h264-netem-moderate'
make bench-run   HOST=gpu-test ARGS="--app 'KDE Desktop' --profile 1080p60-h264 --secs 240"
make bench-suite HOST=gpu-test ARGS='--profiles 720p60-h264,1080p60-h264 --abr-modes off,smooth --dry-run'
make bench-retro                    # replay the archived pre-service runs (idempotent)

# read it back — window= is what makes an impairment comparison mean anything
python3 scripts/dx/bench_table.py --suite baseline --rows tag.profile --cols tag.abr \
    --filter matrix=baseline-v2 --metric browser.fps --agg p50        # --window auto
python3 scripts/dx/vendor/bench.py regressions --suite baseline \
    --scenario 1080p60-abr-smooth-ladder-off-netem-none --metric browser.fps --window baseline
```

**An unrecognised `/v1/stats` filter is IGNORED, not rejected — you get a
plausible unfiltered answer.** The tag-filter syntax is `&tag.<key>=<value>`;
the natural-looking `&tags=<key>%3D<value>` is accepted and silently does
nothing. Caught on 2026-08-18 only because a group that should have shrunk from
7 runs to 6 still said 7. **Sanity-check every filtered query against a known
run or sample count** — the `runs=` and `samples=` fields in the response are
there for exactly this, and a filter that changes neither did not apply.

| Target | Script | What it does |
|---|---|---|
| `bench-submit` | `scripts/dx/bench_submit.py` | posts ONE soak/observe run directory (the shape `session_soak.sh` writes) as a quasar-bench run: samples from `metrics.jsonl`, events from `trace.json` + `marks.jsonl` + `steps.jsonl` + `harness.mark` phase boundaries, artifacts, `summary.json` as the run summary, and `conditions` (`--conditions FILE`, default `<DIR>/conditions.json`). Tags are derived from `session.json` + the worktree's git shas and overridden by `--tag k=v`. **Idempotent** — a deterministic first-class `external_id` makes the service upsert onto the same run (200) instead of creating a second (201). **Exit 3** = posted but MISLABELLED (see mismatches below). |
| `bench-run` | `scripts/dx/bench_run.sh` | ONE live iteration: self-launch a session at a **pinned** `--profile` with the headless peer attached, observe for `--secs` (optionally under `--netem <level>`, which delegates to `abr_ladder_netem.sh`), optionally pull app-side files out of the managed home, then submit. Captures the host's `effective` settings **at launch** into `<out>/conditions.json`. Stops its own session on every exit path. |
| `bench-suite` | `scripts/dx/bench_suite.sh` | the matrix: profiles × abr_mode × ladder × netem × `--iterations`, one run per cell. PATCHes the host's ABR settings per cell and **restores the original overrides on every exit path** (snapshot kept at `<out>/host-settings-before.json`). Tags each cell's INTENT so the mismatch check is not vacuous. Resumable via a state file; `--only` filters cells; `--baseline` pins each ok cell as its scenario's baseline afterwards (default OFF); `--dry-run` prints the plan. |
| — | `scripts/dx/bench_app_samples.py` | folds a quasar-benchapp run's `frames.jsonl` (60 Hz) into one `app` sample per wall-clock second (carrying `frame_index_min/max` so browser-side `missing_indices` stays attributable), its `events.jsonl` into `app.event` events, and the bench-mode `bench-windows.json` into `browser` samples + `bench.window` events — all written back into the `metrics.jsonl` / `trace.json` that `bench_submit.py` already reads. Also writes the per-frame ring to `bench-frames.json` as an artifact. Windows are joined on **their own** timestamps (`last_host_time_ms` → `t_end_host_ms` → `t_end_ms`); a readout carrying none is ordinal-stamped and loudly warned about. Idempotent; MERGES into `trace.json` rather than replacing it. |
| `report-publish` / `report-attach` / `report-url` | `scripts/dx/report.sh` | completion reports + evidence on quasar-bench (C11, spec in `quasar-bench/docs/design/2026-08-23-c11-reports-evidence-spec.md`). A report is keyed by `REPO` + `COMMIT` (the merge SHA; `COMMIT=HEAD`/branch resolves locally). `report-publish REPORT=<md|html> TITLE=… [SUMMARY ISSUES PRS RUNS TAGS PIN=1]` creates or replaces the body; `report-attach COMMIT=… FILE=… [ROLE=screenshot\|video\|log\|bundle\|other CAPTION=…]` uploads evidence (role inferred from the extension; video and bundles are pruned after 90 d unless pinned, screenshots and the report never). The `RESULT` line carries the stable URL — paste it into the commit body, the issue, and memory. Credentials: `BENCH_URL`+`BENCH_KEY` if exported, else the service's own `deploy/.env` on `HOST` over ssh. The bench base URL comes from `BENCH_URL`, else `QUASAR_BENCH_URL` — **there is no built-in default address**; export `QUASAR_BENCH_URL` as the deployment's stable DNS name, because a report URL pasted into a commit body has to outlive an IP. Unset or unreachable, the URL is derived as `http://<HOST>:9400` with a WARN that published links will rot. The CLI underneath is `scripts/dx/vendor/qbench` (vendored from quasar-bench `client/`, same provenance rule as `bench.py`). |
| `bench-retro` | `scripts/dx/bench_retro.sh` | replays `docs/reports/2026-08-16-abr-ladder/bench-retro-manifest.json` — the archived runs that predate the service. |
| — | `scripts/dx/bench_table.py` | renders a `/v1/stats` cross-tab as markdown. `--window` defaults to `auto`: it probes `GET /v1/runs/{id}/phases` and scopes to `impaired` when the runs have one. `--window run` for the whole run. |

### `bench-run` measurement flags

| Flag | What it does |
|---|---|
| `--codec h264\|h265\|av1\|auto` | **Pins the negotiated codec** for the cell. Codec is resolved server-side as *profile codec list ∩ host encoder set ∩ client decode probe ∩ failure history*, and the harness's minted per-run user is probeless — so a ladder profile like `1080p60` (av1 → hevc → h264) silently resolves to its FIRST rung and the cell is labelled with a codec nobody chose. `--codec` posts a device capability record (`POST /v1/me/devices`, exactly as the SPA does at login) declaring only the codecs wanted, which gates the rest out of the intersection. There is no codec field on `POST /v1/sessions` for a non-admin user. The run is tagged `codec_pin=<value>`, and the pin is **verified twice** — `qses` against the launch response, `bench_run.sh` against `session.json` — each failing the run on a mismatch rather than submitting a mislabelled cell. |
| `--bench-mode` | Opens the headless peer at `?bench=1`, arming the SPA's in-page marker decoder (`web/src/bench`). Every displayed frame is decoded, giving true glass-to-glass, frame-index drop/dup/reorder, and input-to-photon. Requires the app under test to render the quasar-benchapp marker. Tagged `bench_mode=1`. |
| `--input-pulse-every N` | In bench mode, presses Space over the input DataChannel every N seconds so input-to-photon samples exist at all (the app's echo is a 3-frame, ~50 ms pulse — nothing sees it unless something presses a key). Requires `--bench-mode`. |
| `--app-log-glob GLOB` | Pulls app-side files out of the managed home (scoped to this run by mtime). With a benchapp glob such as `'*/quasar-benchapp-*/benchapp/run-*/*.jsonl'` the fold above turns them into `app` samples and events. |

`bench-run` and `bench-suite` launch sessions and mutate host settings, so they
carry the same **typed** `HOST=<host>` guard as `up/down/restart/rebuild/abr-ladder`.
The vendored quasar-bench client is `scripts/dx/vendor/bench.py`; it records its
source commit in a header and is a verbatim copy — re-vendor, don't patch.
`bench_submit.py` warns when it is older than the version the service publishes.

**A whole-run aggregate cannot answer "was the stream better under impairment"** —
the clean baseline and recovery holds outvote the impaired window (over the ladder
runs C/D/E a whole-run `browser.fps` p50 reads 60/61/61, i.e. "no difference",
while the same query at `window=settled` reads 0/0/60). Since quasar-bench 1.1 the
SERVICE owns this: `bench_submit.py` posts `harness.mark {phase, edge}` events for
`baseline`/`impaired`/`settled`/`recovery` and every query takes `window=<phase>`.
Compare configurations with `GET /v1/stats?...&window=impaired`, **not** the old
`harness.<phase>_*` sample keys — those are no longer posted (they survive only as
an advisory block in the run summary).

**Tags are intent, `conditions` is reality, and the service compares them.** Every
`conditions.effective` key that disagrees with the same-named tag comes back as a
`mismatches` entry; `bench_submit.py` prints it, tags the run `mismatch=1` and exits
3, `bench_run.sh` fails, and the matrix stops. The run is deliberately NOT marked
invalid — the evidence may be good and only the label wrong, which is a human call
(`PATCH /v1/runs/{id} {"validity":"contaminated","validity_reason":"..."}`, or
`vendor/bench.py validity <id> contaminated --reason ...`). Non-valid runs drop out
of stats/trend/compare/regressions unless `include_invalid=1`.

Every script above — `session-display`, `session-soak` and the four `bench-*`
verbs alike — resolves the host the same way as the rest of the DX layer
(`HOST=<role-or-hostname>` via `.claude/skills/_shared/hosts.json`) and honours
`QSES_ADMIN_TOKEN` as the admin-credential override. `qses display` / `qses soak`
are thin wrappers over exactly these scripts.

`PATCH /v1/admin/hosts/{id}/settings` takes `{"overrides": {...}}` and MERGES it
(a `null` clears a key). **A bare map is a 200 OK that changes nothing** — it
silently cost a whole benchmark matrix its independent variable on 2026-08-17.
`bench_suite.sh` now reads the overrides back and refuses to run a cell the host
does not agree with; do the same in anything else that writes host settings.

## Verification levels — what "done" requires

| Change | Required |
|---|---|
| Any change | `make verify` (fmt/lint/build all components + shellcheck + DX self-tests) |
| Go code | + `make test-go`; **DB-touching: + `make test-db`** (green `test-go` alone means DB tests were SKIPPED) |
| Rust code | + `make test-rust` (runs in the `quasar-agent-dev` container) |
| Web code | + `make test-web` — includes a `web/src/api/schema.d.ts` drift check against `protocol/openapi.yaml` (the `npm run gen:api` output; Go's `TestOpenAPIDrift` counterpart); UI surfaces additionally need the design-handoff visual check (CLAUDE.md) |
| Pre-merge to develop | `make preflight` |
| Pipeline / encoder / streaming | remote validation on the gpu-test host (`quasar-host`, `quasar-session` skills) — a compiling pipeline is not a working pipeline |
| Images / deploy | `quasar-image` skill; contract must pass 150/150 |
| A candidate app image (steam/kde/xfce/gnome) | `make qa IMAGE=<tag> PROFILE=<name> HOST=<role>` — launches real sessions on a GPU stack and emits one self-contained `report.html` (launch/decode, oracle screenshot, per-device input, clean shutdown, teardown). Repoints the app at the candidate and restores it on exit. |

A ticket is DONE only when its build + tests pass at the level above. Test before commit;
commit per unit of work (see CLAUDE.md git contract; branches come off `develop` and merge
back to `develop` — no PR; only `develop → main` is sign-off-gated).

## Destructive operations — explicit authorization required

- `make reset CONFIRM=reset` — removes THIS instance's local containers only; volumes survive.
- `make reset CONFIRM=reset-data` — also removes this instance's volumes. Never touches
  other instances, never touches any remote host.
- Remote destructive actions (fleet stacks, databases, images) are **never** taken
  autonomously — they require the operator's explicit instruction in the session.
- `protocol/` is frozen: changes need Opus + explicit human sign-off (CLAUDE.md).

## Skills (judgment-heavy remote workflows — mechanics stay in `make`)

| Skill | Use for |
|---|---|
| `quasar-host` | anything that must run ON a fleet host: container build/test, stack ops, redeploy, shell |
| `quasar-session` | driving a real WebRTC session (headless peer on the aux-infra role host) |
| `quasar-netem` | network impairment experiments |
| `quasar-diagnose` | stream-smoothness verdicts, traces, A/B experiments |
| `quasar-image` | image audit/build/re-pin/deploy |
| `quasar-ticket` | per-ticket implementation discipline |
| `ship-milestone` | driving a whole GitHub milestone |
