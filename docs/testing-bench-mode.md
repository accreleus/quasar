# Bench mode — measuring a stream against the frames the app actually submitted

Bench mode turns the Quasar SPA into a measurement instrument. It is off for
every ordinary session and is enabled per page load.

```
https://<stack>/app/session/<id>?bench=1
```

`?bench=0` turns it off again, and overrides the sticky
`localStorage["quasar.bench"] = "1"` flag that exists for hand-driven debugging.

> **The reference bench app is not published.** `quasar-benchapp` is an internal
> tool and its image is not in the public catalog, so the walkthroughs below cannot
> be reproduced as written outside the Quasar fleet. Bench mode itself is not
> internal: the in-page decoder reads a documented marker, so any app that renders
> it to spec drives the same instrument.

## What it measures, and why it has to live in the page

The app under test is **quasar-benchapp**, which stamps a machine-readable
marker into the top-left of every frame it renders: the frame index, the app's
own `CLOCK_REALTIME` submit timestamp, the scene, the internal render size, and
a small set of event flags (input echo, scene change, resolution change, mark).
The marker survives the whole compositor → encoder → WebRTC → Chrome path, so a
frame arriving in the browser can be attributed back to the frame the app
submitted — which no opaque workload (Heaven, a game) can do.

The reference decoder could not simply be injected by a test harness: the SPA's
Content-Security-Policy blocks `page.addScriptTag` and in-page `eval`
(`docs/design/research/2026-08-18-benchapp-bringup.md` §6). Decoding in the
driver's Node process works, but only at ~1 Hz — and the two measurements that
matter most, drop/duplicate detection and the app's 3-frame (~50 ms) input echo,
need **every** displayed frame. So the decoder is vendored into
`web/src/bench/marker_decode.js` and runs in the page on
`requestVideoFrameCallback`.

Per displayed frame, bench mode derives:

| Quantity | How |
|---|---|
| **glass-to-glass** | browser present time (RVFC `expectedDisplayTime` against `performance.timeOrigin`) plus the ST-05 client↔host clock offset, minus the marker's own `host_time_ms`. Both terms end up on the host clock. |
| **missing / duplicated / reordered indices** | `frame_index` deltas between consecutive decoded frames: `+1` in order, `0` duplicated, `>1` means `delta − 1` indices never reached the display, `<0` reordered. |
| **input-to-photon** | from the moment a key is sent over the input DataChannel to the first frame whose marker carries the input-echo flag, matched on the flag's **rising edge**. Pure client clock on both ends, so it carries no clock-offset error. |

### `missing_indices` is not `dropped`

The count is deliberately **not** called "dropped", because a browser-side index
gap is not by itself evidence of pipeline loss. The app may never have
*presented* that index, the compositor may have resampled, the encoder may have
skipped, the network may have lost it, or the browser may have dropped it at
presentation. Attribution needs the app's own record for the same second, which
is why `bench_app_samples.py` carries `frame_index_min`, `frame_index_max` and
`app_index_span` on every `app` sample:

```
app_index_span − app frames      = indices the app itself never presented
missing_indices − (that)         = loss downstream of the app
```

Quote `missing_indices` on its own and someone will eventually book it as a
Quasar regression.

### The clock offset is allowed to be missing

The offset comes from the existing ST-05 ping/pong estimator and is `null` until
that estimate is stable. Bench mode never substitutes zero: it reports
`offset_unknown: true` and the `g2g_*` figures are then a **raw clock
difference between two hosts**, not a latency. A g2g that is negative, or far
larger than the round trip could explain, means the offset is missing or wrong —
read the variance, not the absolute value.

`host_time_ms` is also sampled by the app just *before* render rather than at
present-submit (it has to be, to end up in that frame's pixels), so glass-to-glass
here includes the app's own render time. It is an app-submit-to-photon number.

## The stage split — where the glass-to-glass number goes

`g2g_ms` on its own is one number, and every report that quoted it could
attribute about 40 % of it. `web/src/bench/stages.ts` splits it, using the
`VideoFrameCallbackMetadata` the RVFC callback already receives. RVFC hands us
three instants on the page's high-resolution clock for the *same* displayed
frame, so the split is an identity rather than a model:

```
g2g = (receiveTime         − app submit)   host_to_receive
    + (presentationTime    − receiveTime)  receive_to_present
    + (expectedDisplayTime − presentationTime) present_to_display
```

`present_ms` — the term `g2g_ms` is built from — *is* `timeOrigin +
expectedDisplayTime`, so the three stages sum back to the measured
glass-to-glass with no residual. The window carries that self-check on the wire
as `stage_reconcile_p95_ms` (p95 of `|g2g − Σ stages|`, expected ~0); a non-zero
value means the UA supplied no `expectedDisplayTime` and `present_ms` fell back
to `Date.now()`, so the stage table is describing a different quantity than the
g2g beside it.

| key | stage | kind |
|---|---|---|
| `stage_host_to_receive_p50_ms` / `_p95_ms` | app submit → the frame's last packet arrives at the browser: app render, the compositor repaint wait, the whole agent chain, and the wire | measured (the only stage needing the ST-05 offset) |
| `stage_receive_to_present_p50_ms` / `_p95_ms` | last packet → the UA submits the frame for composition | measured |
| `stage_decode_p50_ms` / `_p95_ms` | RVFC `processingDuration` — decoder submit → decoded frame ready, **per frame** | measured |
| `stage_wait_queue_p50_ms` / `_p95_ms` | `receive_to_present − decode`: assembly + jitter-buffer wait + render queue | measured |
| `stage_present_to_display_p50_ms` / `_p95_ms` | composition submit → expected display (vsync quantisation) | measured, **headless caveat below** |
| `stage_jb_mean_ms`, `stage_jb_target_mean_ms` | `jitterBufferDelay` / `jitterBufferTargetDelay` per emitted frame | getStats window mean |
| `stage_assembly_mean_ms`, `stage_processing_mean_ms`, `stage_decode_stats_mean_ms`, `stage_interframe_mean_ms` | the matching `inbound-rtp` totals over their own counters | getStats window mean |
| `stage_frames_decoded`, `stage_frames_dropped` | counter deltas for the window | getStats window delta |
| `stage_render_queue_derived_p50_ms` | `wait_queue_p50 − jb_mean` — Chrome's post-decode render queue | **derived**, not measured |
| `stage_n`, `stage_host_to_receive_n`, `stage_decode_n` | how many frames carried each stage | count |

Three rules the keys follow, all of them load-bearing:

- **A stage the browser could not measure is `null`, never `0`.** `receiveTime`
  and `processingDuration` are optional metadata; a UA that omits them leaves
  the receive-anchored stages null, and the `_n` counts are how you tell a
  genuine 0.0 ms from a gap. `bench_app_samples.py` drops null stage keys rather
  than folding them in as zeros.
- **`host_to_receive` requires a measured clock offset**, for the same reason
  `g2g_ms` does — it is the only stage spanning two machines, so without the
  ST-05 offset it would be a raw clock difference wearing a latency's name. The
  other stages are differences *within* the page's clock and carry no offset
  error at all.
- **The getStats keys are window means, not per-frame joins.** The controller
  reads `getStats` immediately before each window closes, so the counter delta
  describes the window that just ended — but it is a ~1 s difference, which is
  why those keys are named `_mean_` and the per-frame ones are not.

The keys reach quasar-bench as `browser.stage_*`. The fold carries them **by
prefix**, not by an allow-list, so adding a stage to the instrument needs no
harness change — the opposite of the rename that silently holed 10 of the 12
runs in suite `opt-t1`. The `bench.window` payload is `additionalProperties:
true` in `openapi.yaml`, so none of this touches a frozen contract.

### The app → compositor head, split by owner

`stage_host_to_receive` still contains the app's own head, which T1 measured as
one 13.80 ms lump. benchapp now stamps `commit_ms` (`CLOCK_REALTIME` immediately
after `wl_surface.commit`), and the app fold turns that into `app.render_*` and
`app.repaint_wait_*`:

```
render_ms       = commit_ms  − host_time_ms   the app's render + GPU submit
repaint_wait_ms = present_ms − commit_ms      waiting for the compositor's next repaint
```

Both p50 and p95 are emitted because the **shape** is the diagnosis: a repaint
wait is roughly uniform over one refresh interval (p50 near half a frame, p95
near a whole one), whereas a fixed feedback or driver cost has p50 ≈ p95.

## Driving it

`window.__qBench` exists only in bench mode:

| Call | Returns |
|---|---|
| `__qBench.pressKey('Space')` | sends one key down+up and arms an input-to-photon measurement. `false` if the key is unknown or an echo is already outstanding. |
| `__qBench.windows()` | every completed one-second window so far. |
| `__qBench.dump()` | `{frames, windows}` — the last 5000 per-frame records plus the windows. |
| `__qBench.stats()` | `{frames, decoded, located}`. |

Each window is also posted as a `bench.window` trace event
(`POST /v1/sessions/{id}/trace/events`), payload:

```json
{"t_start_ms":1800000000000,"t_end_ms":1800000001000,
 "t_end_host_ms":1800000001003,"last_host_time_ms":1800000000950,
 "n":60,"decoded":60,"undecoded":0,"no_image":0,
 "g2g_p50_ms":85,"g2g_p95_ms":120,"g2g_max_ms":140,
 "missing_indices":1,"duplicated":0,"reordered":0,
 "i2p_ms":[44,52],"i2p_missed":0,"render_w":1920,"render_h":1080,
 "offset_ms":3,"offset_unknown":false}
```

**Each window carries its own clock**, and the fold joins on it — preferring
`last_host_time_ms` (the last decoded frame's own marker stamp, exact host clock,
no offset needed), then `t_end_host_ms`, then `t_end_ms`. It used to be stamped
downstream at `launch_epoch + ordinal × 1 s`, which began at process spawn rather
than first decode, compressed the timeline every time a frameless second was
skipped, and used the workstation's clock. **Runs submitted before 2026-08-18
have untrustworthy window timestamps**; their per-frame `dump()` records and
their time-independent aggregates are fine.

A frameless second is emitted as a window with `n: 0`, so a three-second freeze
reads as three windows rather than a hole. `undecoded` counts frames that had an
image but no valid marker (including "no marker present at all", not only a CRC
failure); a callback whose readback yielded nothing at all is counted separately
as `no_image` and never inflates `n`.

This is a **trace event**, deliberately not a persisted stats key: the browser
stats series is allow-listed per `schema.md` and bench mode must not extend that
contract. Event *types* are allow-listed too, in four places that must agree:
`browserEventTypes` in `control-plane/internal/session/trace_handler.go`, and —
in the frozen `quasar-protocol` submodule — `openapi.yaml`'s
`TraceEventsRequest` enum, `schema.md`'s `session_trace_events` type list, and a
`control-api.md` note marking the type instrument-only. The protocol amendment
is additive (no existing event's shape changes) and was merged to
`quasar-protocol` `main` as `91493fc` on 2026-08-18. A control plane that predates it silently
drops the events, which is why the harness reads the windows out of the page as
well.

## From the harness

```
make bench-run HOST=devbox ARGS="--app 'Quasar Benchapp' --profile 1080p60 \
  --bench-mode --codec h264 --input-pulse-every 10 --secs 180 \
  --app-log-glob '*/quasar-benchapp-*/benchapp/run-*/*.jsonl'"
```

- `--bench-mode` opens the peer at `?bench=1` and lifts `__qBench.windows()` into
  `<out>/bench-windows.json` (falling back to the admin trace-event query).
- `--codec` pins the negotiated codec — see the AGENTS.md table for why a
  probeless harness user otherwise lands on whichever codec the profile lists
  first.
- `--playout MS` pins the receiver jitter buffer via the SPA's `?playout=` override
  (which also disables the AS-05 adaptive controller — the measurement instrument
  is absolute). The cell is tagged `playout=MS`, or `playout=auto` when unpinned;
  **join on that tag, never on the scenario name** — a driver that builds arm
  specs wrongly produces cells whose name claims a pin that never happened, and
  the tag is the only thing that tells the truth (this happened in suite
  `opt-t1`, 2026-08-18).
- `--codec` pins the negotiated codec, and the pin is **verified twice** — `qses`
  against the launch response, `bench_run.sh` against `session.json` — with a
  non-zero exit on mismatch rather than a mislabelled cell.
- `--peer local|aux` picks where the headless WebRTC peer runs. **`local` is the
  default** (changed 2026-08-19): `QSES_PEER_ROLE` is set to the same role/host
  `bench_run.sh` resolved for `HOST`, so `qses` puts the peer on the stack host
  itself. `--peer aux` restores the pre-2026-08-19 default,
  `QSES_PEER_ROLE=aux-infra` (hermes). The switch is a direct result of
  `docs/reports/2026-08-19-peer-path/REPORT.md`: an otherwise identical cell
  measured **0.000% missing indices with a local peer vs 2.7% through hermes**
  — hermes is a WiFi NIC doing software H.264 decode on a weaker CPU, and its
  RTT p95 (136-173 ms) alone is enough to blow the 50 ms jitter buffer, which
  looks exactly like a missing-index gap. **Every browser-side drop number in
  every bench/soak report dated before 2026-08-19 was measured through the
  hermes peer** (the harness default at the time) and therefore carries the
  peer's own network/CPU headroom as well as Quasar's — they are not directly
  comparable to a run made with `--peer local` (the default from here on).
  `--netem` cells still need the peer on the aux-infra side of the shaped link
  (netem shapes the sender's egress toward the aux-infra NIC, which a local
  peer never crosses): `bench_run.sh`/`bench_suite.sh` refuse `--netem` +
  `--peer local` outright rather than submit unshaped data under an
  `impaired` label. Every run is tagged `peer=<resolved host>` (and
  `conditions.peer_host`), so old (implicitly hermes) and new runs stay
  distinguishable in `/v1/stats` queries. To reproduce the old peer:
  `make bench-run ARGS='--profile ... --peer aux'`.
- `scripts/dx/bench_app_samples.py` then folds everything into the two files
  `bench_submit.py` reads: bench windows become `browser` samples, the app's
  `frames.jsonl` becomes one `app` sample per second (with its `frame_index`
  range), and its `events.jsonl` becomes `app.event` events. The per-frame ring
  is written to `bench-frames.json` and attached as a run **artifact**, not as
  samples — 5000 records at 60 Hz is a timeline, not a metric series.

App marks are **not** emitted as `harness.mark`: `bench_submit.py` mints that
type itself with a `{phase, edge}` payload to tell the service where the
baseline / impaired / settled / recovery windows are, and feeding an app's
labelled marks into it would corrupt phase derivation.

## Read the rate over the OBSERVE window, not the whole peer hold

The peer is deliberately held longer than the measurement (`settle + observe +
60 s`), and the missing-index rate is **not** steady across that hold. Measured
live on devbox, 2026-08-18, one 150 s benchapp cell:

| scope | missing-index rate |
|---|---|
| whole peer hold (275 s) | 2.38 % |
| **observe window only (150 s)** | **0.86 %** |
| settle period (first 60 s) | 6.95 % |
| tail (last 65 s) | 1.64 % |

Per-window the distribution is bursty, not Gaussian: median **0.00 %**, p90
9.7 %, max 84.6 %, and **194 of 276 windows have zero missing indices at all**.

`bench_run.sh` now passes `--warmup-secs $SETTLE` to the submitter, which splits
an unimpaired run's single whole-hold `run` phase into `warmup` + `observe`, so
the scoping is answerable from the service rather than only from the local
files:

```
/v1/stats?suite=...&metric=browser.missing_indices&window=observe
```

Live on the same cell: whole-hold mean 1.428 missing/window, `warmup` 4.404,
`observe` 0.653 — a 6.7× difference between the two halves of one run.
The whole-hold aggregate is therefore dominated by app start-up and stream
warm-up — it is close to three times the steady-state figure, and comparing one
run's whole-hold number against another's observe-window number will invent a
regression or hide one. Scope explicitly, and say which scope a quoted number
used.

## Cost, and the observer effect

Bench mode reads back pixels every displayed frame. Steady state is cheap: once
the marker is located the decoder takes its cached-location path (570 cell-centre
samples) and the readback shrinks to the marker's own bounding box (~500×320 px
at 1080p). While the marker is **not** located the full search allocates a luma
buffer plus a Float64 integral image (~3.4 MB at the default search crop), so it
is throttled to four attempts a second — a cold app start can take tens of
seconds to first paint, and 60 fps of full searches through that is hundreds of
MB/s of GC churn for no information.

**Bench numbers are not directly comparable to a non-bench cell.** The
`drawImage` + `getImageData` runs synchronously in the RVFC callback, on top of
`peer-driver`'s own luma sampler — two GPU→CPU readbacks per displayed frame.
That can itself cost presentations, i.e. inflate the very `missing_indices` and
`duplicated` counts being reported. Compare bench cells against bench cells.

**Headless `expectedDisplayTime` is a prediction, not a vsync.** Headless Chrome
has no real display, so presentation timing rides a virtual clock. The
glass-to-glass and pacing figures are self-consistent across headless runs and
are the right instrument for A/B work, but they are not what a user on a real
monitor would see — especially at 120 fps profiles.

The instrument is also lazily loaded: `SessionPage` dynamically imports the
controller behind the flag check, so the decoder is its own chunk that only a
bench run ever fetches.

## Recovery behaviours worth knowing

- A **resolution change in either direction** resets the cached marker geometry,
  and 15 consecutive hinted-decode failures force a full re-acquire. Without
  this an upward rung change (720p→1080p) was unrecoverable: the crop stayed too
  small for the now-larger marker and decode was lost for the rest of the run.
- **Input pulses are queued, not refused**, and a send whose echo never arrives
  is abandoned after 2 s and counted as `i2p_missed`. A single dropped echo
  frame used to wedge the pending slot and silently empty the i2p series for the
  remainder of the run, with the harness still reporting green.

## The glass-to-glass budget (standing instrument)

`docs/reports/2026-08-19-latency-budget/REPORT.md` measured every stage of a
66 ms 1080p60 h264 glass-to-glass and made the budget close (section 1: 66.0 ms
measured, 65.7 ms of stages, 0.3 ms residual). That was one report, run by
hand. This section is the standing form of it: the same stages, captured on
**every** bench-mode run and printed as a table you don't have to build
yourself.

### What is measured where

| Stage | Source | Where it comes from |
|---|---|---|
| `stage_host_to_receive_*`, `stage_receive_to_present_*`, `stage_decode_*`, `stage_wait_queue_*`, `stage_present_to_display_*`, `stage_render_queue_derived_*`, `stage_jb_*`, `stage_assembly_*`, `stage_reconcile_*` | `browser` | the RVFC-derived stage split, "The stage split" above — always emitted in bench mode |
| `g2g_p50_ms`, `g2g_p95_ms` | `browser` | the headline glass-to-glass figure the stages reconcile against |
| `render_p50_ms`, `repaint_wait_p50_ms`, `submit_to_present_p50_ms` (+ `_p95_ms`) | `app` | benchapp's `commit_ms` split, "The app -> compositor head, split by owner" above — needs a benchapp image built from a `commit_ms`-carrying `quasar-benchgame` (2026-08-19: `946da34`) |
| `probe_capture_to_enc_in_*`, `probe_pts_to_emit_*`, `probe_enc_out_to_send_*`, `probe_pay_to_send_*` | `agent` | the host-stage latency probe (`QUASAR_LATENCY_PROBE` / hostcfg `latency_probe`) — **`bench_run.sh --bench-mode` now arms this automatically** for the run (PATCHes `overrides.latency_probe=true` on the session's host, verifies the read-back, restores the prior value on every exit path including Ctrl-C — same snapshot/PATCH/verify/restore-in-trap shape as `bench_suite.sh`'s ABR settings). Perturbation from the probe itself was measured and found nil: `docs/reports/2026-08-19-fps120-probe/REPORT.md` part 2 (drops Δ +0.143pp, g2g Δ +2.10ms, both inside the randomised-rerun PASS thresholds). Pass `--no-probe` to opt a run out. |
| `present_fps_median`, `present_interval_sd_ms`, `present_interval_max_ms`, `present_beat_fraction`, `present_long_frames`, `present_n` | `browser` | the AS-04/#108 present-cadence keys `docs/session-trace/metrics.json` defines (`present_fps_median` is what replaced the deprecated MEAN `present_fps` — a healthy 1440p120 session once read it as 88-108 fps). Rolled up on **every** run, not only bench-mode ones — `bench_submit.py`'s `ROLLUP_KEYS`/`ROLLUP_COUNTERS`. |
| `encode_ms_max` | `agent` | the worst single per-frame encode in the window — the one agent key that can see a one-frame stall, which a mean or p95 both wash out over a 5 s window. Also rolled up on every run. |

Every key above is registered with quasar-bench (`scripts/dx/bench_register_budget_metrics.py`, `POST /v1/metrics`) with a direction and a regression threshold — `lower` for every latency/judder/stall key (`stage_*`, `g2g_*`, `probe_*`, `present_interval_*`, `present_long_frames`, `encode_ms_max`), `higher` for `present_fps_median`, `neutral` for pure sample-count/proportion keys (`stage_n`, `stage_frames_decoded`, `present_n`, `present_beat_fraction`, …) — declared so they show up in `/v1/metrics`, but never flagged. The threshold is 15% for the per-stage split and the present-cadence/`encode_ms_max` keys (a smaller, noisier slice than the headline number), 10% for the two `g2g_*` keys (matching their pre-existing registration). The `present_interval_*`/`encode_ms_max` directions are cross-checked against `docs/session-trace/thresholds.json`'s `classifier.hitch_sd_ms` / `classifier.encoder_ceiling_ms` at registration time (`scripts/dx/thresholds.py`) — same reasoning ("worse" means the same thing here as it does to the session verdict), a different unit (a percent band, not an absolute ms line), so the threshold VALUE itself is not substituted in directly.

A drift test (`scripts/dx/tests/check_bench_keys.py`, wired into `scripts/dx/tests/run.sh`) asserts every key `ROLLUP_KEYS`/`ROLLUP_COUNTERS` folds exists in the manifest under the same `source` and carries no `deprecated_for` — the check that would have caught `present_fps` still being folded after it was deprecated.

### Reading the table

`make bench-budget RUN=<id>|latest [ARGS='--suite ... --baseline ...']` (or `scripts/dx/bench_budget.py --run <id>` directly) prints the reconciled table for one run: every stage's p50/p95, its delta against the pinned baseline (from `GET /v1/regressions`, which already carries the metric's registered direction and threshold), and a flag — `ok`, `REGRESSED`, `neutral`, or `no data` (a stage the browser/app/agent could not measure this run — see "A stage the browser could not measure is `null`, never `0`" above). A closing line reconciles the three top-level components against the measured g2g, the same identity check as section 2 of the report:

```
reconcile: A+B+C = 71.60 ms   measured g2g = 71.90 ms   residual = 0.30 ms
```

`bench_run.sh` calls the same script and prints the same table at the end of **every** bench-mode run — informational only, it does not change `bench_run.sh`'s own exit code (a matrix's cell bookkeeping depends on that rc meaning "did the cell submit", not "did every stage stay inside its threshold"). `bench_budget.py` itself exits non-zero when any stage regressed, so it can gate a CI-shaped check on its own: `make bench-budget RUN=<id>` (or a `run_id=` scraped from `bench_run.sh`'s output) is the thing to wire into anything that should actually fail on a regression.

A run whose suite/scenario/baseline name has no pinned baseline prints every stage as `no data`/unscored, not a failure — a budget run against a suite nobody has baselined yet is informative, not broken.

### Baseline policy — when to re-baseline

The pinned baseline is `latency-budget/1080p60-h264-local`, set from the median of the three clean local cells in `docs/reports/2026-08-19-latency-budget/REPORT.md` (run `640d5b00`, g2g p50 66.0 ms — the report's own headline number). Re-baseline:

- **After an intentional default change** that is expected to move a stage — an ABR-mode flip, a playout-floor change, a new compositor/encoder pin. The whole point of a threshold is to catch an *unintended* regression; re-pinning after a deliberate one is how the budget stays useful instead of permanently red.
- **Never** to make a run that regressed for an unknown reason quietly green. Find out why first (`quasar-diagnose`, the stage table itself, `docs/reports/2026-08-19-latency-budget/REPORT.md` section 6 for the known-actionable levers).
- `make bench-baseline RUN=<id> NAME=<suite/scenario>` pins a run as a named baseline (thin wrapper over `Bench.set_baseline`, `scripts/dx/bench_baseline.py`) — suite and scenario are read from the run itself, so a typo can't pin the wrong one. Omit `NAME` for the `<suite>/<scenario>` default; a different name lets more than one baseline coexist for the same suite+scenario (e.g. a `-clean` and a `-netem` variant).

### A nightly budget run — scheduled (Michael sign-off 2026-08-19)

The instrument above is complete per-run; the standing cadence is a cron job on the devbox itself (closest to the hardware, no extra CI surface — a GitHub Actions job stays out of scope while CI here is manual-only, `workflow_dispatch` only). `scripts/dx/nightly_budget.sh` runs ONE 1080p60 h264 bench-mode cell (`--secs 150`, suite `nightly-budget`, scenario `1080p60-h264-local`, app `Quasar Benchapp`, `--peer local`, tags `nightly=1 git_quasar=<sha>`) and gates it with `bench_budget.py` against the pinned baseline `latency-budget/1080p60-h264-local` — the same instrument `make bench-budget` runs by hand, on a fixed nightly schedule instead of whenever someone thinks to look.

**It never mutates the deployed stack.** The cron does not `git pull`, redeploy, or rebuild anything — it runs from whatever sha the devbox happens to have checked out (recorded verbatim as the `git_quasar` tag), the same way `redeploy.sh` already keeps that checkout in sync through its own, separate process. A cron job that redeployed the stack on its own schedule would race an operator's own deploys; reading the sha and moving on avoids that entirely.

**HOST=devbox-self, over a loopback ssh hop.** `bench_run.sh` always launches its session through `qses`, and `qses` always shells out over ssh to whatever `--stack` names — even when the target IS the machine the caller is already on, there is no "skip the hop, we're already here" path. So running the cron directly on the stack host still needs a working ssh hop back to itself. That host's own (untracked, machine-local) `.claude/skills/_shared/hosts.json` carries a self-entry — `ssh_host 127.0.0.1`, keyed by a **dedicated** keypair added to that host's own `authorized_keys`. Never reuse an interactive or agent-backed key for this. It is deliberately **not** named `local`: `common.sh`'s `DX_HOST=local` sentinel means "skip remote resolution entirely, this is the ephemeral `docker-compose.local.yml` dev stack" — `bench_run.sh`'s own admin-API calls fall back to that stack's port whenever `DX_HOST=local`, which silently pointed every one of them at the wrong stack the first time this ran live (the session itself launched and ran fine over the real ssh/`qses` path; `bench_run.sh`'s own poll for it just kept asking the wrong port whether it existed, and timed out). A real, resolvable host name routes `bench_run.sh` through its normal remote-host code path instead, which resolves correctly.

**Skips cleanly, never crashes, never leaves anything behind.** Before launching anything, the script checks — in order — that it is not already mid-run (a portable mkdir-based lock, no `flock` dependency), that `BENCH_KEY` can actually be read (`$HOME/quasar-bench/deploy/.env`'s `BENCH_API_KEYS=harness:<secret>`, never copied into this repo), that the stack answers healthy (`GET /health` == 200), and that no session is already running (`qses ls --stack=local`, using the control plane's per-boot dev-agent key fetched fresh via `docker exec` every run, since it rotates on every restart). Any failure there is a clean `status=skipped reason=<...>`, not a partial run. When it does run, `bench_run.sh`'s own trap (session stop + host-setting restore on every exit path, `--keep` never passed) is what guarantees nothing is left running or overridden — the nightly wrapper adds no cleanup of its own beyond releasing its lock.

**The alert is a log line, for now.** Every run appends exactly one `NIGHTLY-BUDGET status=ok|regression|skipped|error run_id=<id> suite=... scenario=... git_quasar=<sha> [reason=...]` line to `/home/quasar/quasar-nightly/<YYYY-MM-DD>.log` (30 daily files kept, older ones rotated out). On a regression, `/home/quasar/quasar-nightly/LAST_REGRESSION` is overwritten with the reconciled stage table — read either file over ssh, or via the Dozzle MCP (`CLAUDE.md` "Container logs on quasar-devbox"). There is no email/Slack wiring yet; the log line is the whole alerting surface.

**Bootstrap note:** `bench_budget.py`'s baseline lookup is keyed by `(suite, scenario, name)` — the pinned `latency-budget/1080p60-h264-local` baseline lives under suite `latency-budget`, so a run tagged suite `nightly-budget` would not find it without a matching baseline row for that suite too. A second baseline was pinned once, by hand, pointing at the same reference run (`640d5b00…`, the report's own `stages-local` cell) under `(suite=nightly-budget, scenario=1080p60-h264-local, name=latency-budget/1080p60-h264-local)` — the nightly cron reads that one. Re-baselining the nightly suite after a deliberate change follows the same policy as above, just pin it twice (once per suite) if both `latency-budget` and `nightly-budget` need to move together.

Operating it:

```
make nightly-budget-install HOST=devbox   # idempotent: writes the 03:30 UTC crontab line
make nightly-budget-run     HOST=devbox   # trigger one run now, foreground (minutes, not seconds)
make nightly-budget-status  HOST=devbox   # tail today's log + LAST_REGRESSION if any
```

`HOST=local` on the same commands runs them directly with no ssh at all (useful when already shelled into devbox). Re-running `nightly-budget-install` is safe any time — it replaces its own crontab line rather than duplicating it. Tests for the guard, the skip paths, the budget gate, and the crontab idempotency live in `scripts/dx/tests/run.sh` ("nightly budget"), run by `make verify`.

## The encoder-probe rule: `profile=main`, and why a bare `gst-launch` lies

**Never probe the encoder with a hand-typed `gst-launch` pipeline.** Use

```
docker exec <agent-container> quasar-node-agent probe-encoder \
  --codec h264|h265|av1 [--size 1920x1080@60] [--seconds 2] [--json]
```

On 2026-08-22 the h265 encode path was believed to be broken on every NVIDIA driver. It
was not. A bare `gst-launch-1.0 videotestsrc ! … ! vulkanh265enc ! fakesink` probe left
the output `profile` unpinned, the encoder negotiated `main-444`, and the failure that
followed read as a driver regression. **The production path has always pinned
`profile=main`** — `caps::caps_profile` resolves it and the bitstream chain's capsfilter
puts it on the encoder's src pad — so the two graphs were never the same graph, and the
harness had no code in common with the product to make that visible.

`probe-encoder` closes that gap by construction: it resolves the encoder with
`pipeline::resolve_effective_encoder` (the vendor/Vulkan knob resolution, including the
per-session vendor-HW fallback), builds the element with the production builder, and pins
the output caps with the production bitstream chain. The one substitution is the source —
`videotestsrc` plus the same `caps::encoder_input_caps_for` the scale stage emits, because
a CLI probe has no compositor — and the report says so. A disagreement between
`probe-encoder` and a live session is therefore a real defect, not a difference in typing.

`scripts/harness/run-codec-validate.sh` runs it as a per-codec pre-flight (including for h265) and
records `<codec>_encoder_factory` and `<codec>_negotiated_profile` in its report.

Still `gst-launch`, deliberately: `scripts/release/probe-vulkan-encoder-runtime.sh`. That script
validates a candidate **image by digest** before it is promoted, in a disposable container,
and its h264 step is a registration/initialisation check rather than a negotiation
question. It also cannot assume the agent binary is on `PATH` in an arbitrary candidate.
If it grows a profile-sensitive assertion, it should move to `probe-encoder` then.

## The HEVC-headless rule

**Chrome's HEVC decode is hardware-gated and exists on macOS and Windows only — never on
Linux, and never in headless Chrome-for-Testing.** The harness therefore *cannot* validate
HEVC decode, on any host, ever. This is a browser limitation and says nothing about the
encode path.

What a headless HEVC run actually looks like, and how to read it:

- The session launches, reaches `running`, and the transport connects.
- The peer's answer **applies** — so there is no `webrtc.remote_description_failed`.
- Inside that answer the video m-line is rejected (port 0 or `inactive`). The agent emits
  `sdp.answer_applied` with `rejected_count: 1` and logs a WARN carrying the grep token
  `sdp-mline-rejected`, naming the codec.
- No media flows on that PeerConnection, so the encoder's output goes quiet and
  `encoder.stall` may follow with `reason: "input_starved"` or `"no_output"`.

**That signature is the expected outcome of an HEVC headless run, not a stall to
investigate.** Read `sdp.answer_applied` before reading anything about the encoder: a
rejected m-line explains the silence completely, and the 2026-08-22 investigation that
spent hours on an "encode-src ring stall" had exactly this shape. To exercise HEVC decode
you need a real macOS or Windows Chrome; to exercise HEVC *encode* without a decoder, use
`probe-encoder --codec h265` above.

AV1 has no such gate: Chrome decodes it everywhere via `dav1d`.
