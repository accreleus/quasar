# Soak-and-diff leak detection (PROF-03)

## What this measures, and why cycling beats uptime

A single long-lived session can look perfectly flat in RSS while every NEW
session leaks a fixed amount: a per-session goroutine that never exits, a
GObject ref cycle that pins a GstContext, a per-launch heap allocation that
never gets freed. None of that shows up in an uptime chart of one session -
it only shows up as a staircase across many launch-stream-teardown cycles.

`scripts/harness/run-soak-profile.sh` drives exactly that: launch a session, hold the
stream for a while, tear it down, wait for things to settle, sample a fixed
metric set, repeat. Every cycle's sample is a data point; `scripts/harness/lib/soak_report.py`
fits a trend line to each metric across cycles and classifies it.

This is a leak-detection tool, not a load test. It runs one session at a
time, sequentially.

## Prerequisites

- A live Quasar stack (docker compose) on the host you run this on. The
  script executes `docker exec`/`docker inspect` against the local docker
  daemon - it does not ssh anywhere.
- The PROF-01 debug listener merged and deployed (loopback `:6060` inside the
  control-plane container, `/debug/quasar/runtime` and `/debug/quasar/pool`).
- `curl`, `docker`, `python3` on the host running the script. `timeout`
  (coreutils) is used to cap docker-exec sampling calls at 15s if present;
  the script checks once at startup and warns (not fails) if it's absent.
- **The harness user must already exist AND be entitled to the target app.**
  Registration is closed by default (`registration_mode` defaults closed),
  and `/v1/apps` is entitlement-filtered - the old "auto-registered on first
  run" claim here was false and has been removed. On the Tower dev stack the
  `qses` harness user already satisfies both. On a fresh stack, either have
  an admin create the user and grant entitlement first, or pass admin
  credentials via `--email`/`--pass` so the harness's own registration call
  (which will only succeed if registration happens to be open) has a chance
  of working. If login fails, the script's error message points back here.

## CLI reference

```
bash scripts/harness/run-soak-profile.sh --app 'Steam' [options...]
```

| Flag | Default | Meaning |
|---|---|---|
| `--duration SECS\|Nh\|Nm` | `2h` | Wall-clock budget for the whole run. |
| `--cycles N` | unlimited | Max cycles; whichever of duration/cycles hits first stops the run. |
| `--hold SECS` | `90` | How long to hold each session running before teardown. |
| `--settle SECS` | `10` | Pause after teardown, before sampling (lets teardown-triggered cleanup finish). |
| `--app NAME` | *(required)* | App to launch each cycle. No default - launching the wrong app silently produces misleading data. `Steam` is Tower's known-good app. |
| `--profile ID` | none | Launch profile id (e.g. `1080p60`) to pin the launch to. |
| `--api URL` | `https://localhost:18443` | Control-plane API base (curl always uses `-k`, self-signed cert). |
| `--cp CONTAINER` | `deploy-quasar-control-plane-1` | Control-plane container name. |
| `--agent CONTAINER` | `deploy-quasar-node-agent-1` | Node-agent container name. |
| `--pg CONTAINER` | `deploy-quasar-postgres-1` | Postgres container name. |
| `--out DIR` | `deploy/results/soak-<UTC ts>` | Output directory (CSV, params.json, report.html, verdicts.json). |
| `--gst-leaks` | off | Also sample the GStreamer live-object count (see below). |
| `--max-consecutive-failures N` | `5` | Abort the run if this many cycles in a row fail. |
| `--email` / `--pass` / `--user` | *(unset — dev-mint default)* | Explicit account override. When unset the harness mints a throwaway auto-reaped identity via the dev-gated `POST /v1/dev/agent-session` (#399), which requires `QUASAR_DEV_AGENT_AUTH=1` on the stack (key: CP log or `/run/quasar/dev-agent-key`, or export `QUASAR_DEV_AGENT_KEY`). No committed default credential exists anymore. |
| `--launch-timeout SECS` | `90` (or `$QSES_LAUNCH_TIMEOUT`) | Per-launch poll budget waiting for `running`. |
| `--teardown-timeout SECS` | `60` (or `$QSES_TEARDOWN_TIMEOUT`) | Per-teardown poll budget waiting for a terminal state. |
| `--report-only CSV` | - | Skip soaking; just run the report over an existing CSV. |

Every parameter is a flag - there are no hardcoded constants an operator has
to go find in the script to change behaviour. Every curl call carries
`--max-time 20 --connect-timeout 5`, and every `docker exec` sampling call is
wrapped in `timeout 15` (when available) - a wedged control plane or docker
daemon cannot hang an 8h run; a cycle that still exceeds
`hold + launch_timeout + teardown_timeout + 120s` is logged and treated as
failed, and the loop continues.

## CSV schema

`<out>/soak.csv`, header first, one row appended per cycle (plus a cycle-0
baseline row sampled BEFORE the first launch):

```
cycle, ts_iso, cycle_ok, session_id, launch_to_running_s,
cp_goroutines, cp_heap_inuse_bytes, cp_heap_objects, cp_heap_alloc_bytes, cp_sys_bytes, cp_num_gc, cp_rss_kb, cp_fds, cp_uptime_s,
pool_acquired, pool_idle, pool_total, pool_empty_acquire_count,
db_sessions, db_session_metrics, db_trace_events, db_auth_tokens, db_session_tokens, db_admin_activity,
agent_rss_kb, agent_threads, agent_fds, vram_used_mb, gst_alive,
agent_uptime_s
```

`cp_uptime_s` is seconds since the control-plane container's process 1
started (from `docker inspect .State.StartedAt`, converted to seconds-ago on
the harness host). `agent_uptime_s` (issue #420) is the same thing for
`AGENT_CONTAINER`, sampled at the same cadence and appended as the LAST
column so `--report-only` over a pre-#420 CSV (one written before this
column existed) still parses fine - the column is simply absent from that
CSV's header, same as any other missing-sample cell. Both are metadata, not
leak series - neither is ever itself classified. Their only job is restart
detection: a DECREASE between consecutive rows on EITHER column means that
container restarted mid-run (see "Restart detection" below). This closes a
real gap (#420): the D-5 chain hit a Tower 04:01 backup restart of the whole
stack, and because only `cp_uptime_s` was ever sampled, an agent-only
restart was invisible - agent fd/RSS baselines silently reset with no
segmentation banner, even though the control plane never restarted.

A failed cycle (launch timeout, `home_in_use`, session never reaches
`running`, or a cycle that overran its time budget) still writes a row:
`cycle_ok=0` plus whatever samples could still be taken. A sample that could
not be taken is an EMPTY cell, never `0` - a fabricated zero would look like
a real cliff to the classifier. Every assembled row is checked against the
header's field count before being written; a mismatch (the historical bug
class here - a sample fragment silently emitting the wrong number of fields
and shifting every later column) is logged as an error and replaced with an
all-empty-sample row of the correct width (`cycle_ok=0`) rather than ever
writing a malformed row. The CSV is appended incrementally, so a crash or
kill mid-run still leaves a valid, partially-complete artifact; the script's
EXIT trap always attempts a report from whatever CSV exists.

**Rows with `cycle_ok=0` are excluded from all series analysis** (only the
baseline row and `cycle_ok=1` rows feed the classifiers). A run with a lot of
failed cycles is itself worth investigating even before looking at any
series verdict - the report's summary block surfaces the failed-cycle count
for exactly this reason.

### Restart detection

If the control plane OR the node agent restarts mid-run (`cp_uptime_s` or
`agent_uptime_s` decreases between consecutive usable rows), or any counter
column (`pool_empty_acquire_count`, `cp_num_gc`, any `db_*` column) goes
backwards, `soak_report.py` flags the run RESTARTED: verdicts are computed on
the **longest contiguous segment only**, and the HTML report gets a
prominent red banner naming which container(s) restarted at which cycle(s)
(`control-plane restart`, `node-agent restart`, or `counter reset: <col>`) and
the analysed segment. A restart during a leak soak is itself a
leak-candidate signal (an OOM-restart, say) - it does not just get silently
averaged away; investigate the restart cause alongside whatever the
segment's verdicts say. `verdicts.json` carries this as a run-level `_run`
key (with `break_cycles` and `break_reasons`, not a series name, so it can
never collide with one).

Segmenting on `agent_uptime_s` is deliberately independent of `cp_uptime_s` -
an agent-only restart (the whole-stack-restart case from #420) must be
caught even when the control plane's own uptime never drops, and vice versa.

## Verdict vocabulary

### Gauges

Point-in-time measurements: goroutines, heap, RSS, fd/thread counts, VRAM,
pool `acquired`/`idle`/`total`, `gst_alive`.

- **LEAK** - significant upward slope (t >= 4), growth above the noise
  floor, not a plateau, and confirmed by a noise-robust quartile-median
  check (median of the last quarter of points vs the first quarter clears
  `max(floor/2, 1 residual std)`). This check REPLACED a raw
  monotonic-fraction gate: a Go heap sawtooth (GC alloc/collect cycles) can
  flip sign on most consecutive deltas while genuinely, steadily leaking -
  the old gate suppressed real leaks on exactly that kind of series. The
  monotonic fraction is still reported, but no longer gates the verdict.
- **STEP** - a one-time level shift, not a per-cycle leak: the best single
  changepoint (brute-forced over interior indices, comparing a two-segment
  flat-means fit against the overall linear fit) explains the data far
  better (>70% residual-variance reduction) than a steady trend. Typical
  cause: something that allocates once (a cache warming to steady state, a
  connection pool filling) rather than per cycle. Reported yellow/orange,
  not red - but still surfaced, not silently cleared, and investigate what
  allocated once.
- **PLATEAU** - rose then flattened. Usually warm-up (connection pools
  filling, caches populating) rather than an unbounded leak - but still
  reported, not silently cleared. Requires BOTH: the last-third slope
  (plus twice its standard error) is under 25% of the first-two-thirds
  slope, AND the last-third's own trend is not itself statistically
  significant (|t| < 2). Both sub-ranges need >= 5 points or no plateau
  verdict is possible (an unmeasurable sub-range never blocks LEAK).
- **FLAT_CLEAN** - no trend, low residual noise. Clear at this sensitivity.
- **FLAT_NOISY** / **SUSPECT** - inconclusive: either no significant trend
  but high variance, or a borderline slope that hasn't cleared significance
  yet. Carries the escalation note (below).
- **DOWNWARD** - significant fall. Unusual; worth a look (a metric
  trending down over a soak is not itself bad, but it's not supposed to
  happen either).
- **INSUFFICIENT** - fewer than 10 usable data points.

### Counters

`pool_empty_acquire_count`, `cp_num_gc`, and every `db_*` column are
monotonic COUNTERS by construction. `db_sessions` growing linearly is not a
leak - it is the schema doing exactly what it is supposed to do (every cycle
inserts a session row; `session_metrics`/`session_trace_events` accrue per
session; `admin_activity` accrues on admin actions). Reporting these as LEAK
would train an operator to ignore soak reports.

- **GROWING_BY_DESIGN** - reports the observed per-cycle growth rate. This
  is the expected verdict for every counter column in almost every run.
- **ACCELERATING** - the per-cycle growth RATE itself is increasing (not
  just the total). That would be unusual - e.g. an unbounded batch, a retry
  storm, a query touching more rows per cycle than it should - and is worth
  investigating.

### Escalation rule (verbatim, also in the HTML report)

> 2h flat and clean: clear at this sensitivity. 2h flat but noisy, or slope
> present but not significant: rerun at 8h / ~150 cycles. Before a release
> tag: one 8h run per host regardless.

**A flat 2h result does not clear a slow leak.** The noise floors and
significance thresholds are fixed at the 2h-tier sensitivity; an 8h run does
not get smaller floors, it gets more cycles, which is what turns a real but
small slope into a statistically significant one. A slow leak (a few KB per
cycle) can sit under both the floor and the significance threshold for 60
cycles and clear both by cycle 150. Treat a 2h FLAT_CLEAN as "nothing jumped
out," not "proven leak-free" - and always run the 8h/~150-cycle soak before
tagging a release.

## Self-test procedure

`scripts/harness/fixtures/soak-selftest-leak.patch` is a planted, deliberate
control-plane leak (1MiB retained + one goroutine leaked per session
launch, in `dispatchAssignStartWithTopology`, `control-plane/internal/session/launcher.go`
- the function every successful launch passes through). It exists to prove
the detector actually detects something, not just that it runs without
crashing.

**This patch must never land applied on `develop` or `main`.** Run it only
on a throwaway branch:

```bash
git checkout -b soak-selftest-throwaway
git apply --check scripts/harness/fixtures/soak-selftest-leak.patch   # sanity, BEFORE applying, should be clean
git apply scripts/harness/fixtures/soak-selftest-leak.patch

# Rebuild the control-plane image with the patch applied (never a hand-typed
# docker build - see deploy/build-images.sh / the quasar-image skill). The
# real invocation for the nv stack is a plain compose build against the
# control-plane service (context + Dockerfile.control), from the repo root:
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml build quasar-control-plane

# Redeploy that image to the test stack:
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml up -d quasar-control-plane

# Then run a short soak:
bash scripts/harness/run-soak-profile.sh --app 'Steam' --duration 20m --hold 15 --settle 5

# Expect the report to call cp_goroutines and cp_heap_inuse_bytes LEAK.

# Then restore and clean up:
git checkout -- control-plane/internal/session/launcher.go
git checkout develop   # or whatever branch you started from
git branch -D soak-selftest-throwaway
# ...and rebuild + redeploy the real (unpatched) image the same way.
```

A 20-minute/short-hold self-test run will not reach the 10-usable-point
minimum on every column at default `--hold 90`/`--settle 10` - shorten
`--hold`/`--settle` for the self-test so more cycles fit in the window, as
shown above.

## `--gst-leaks`

Optional: samples the GStreamer live-object count on the node-agent after
each cycle, via `docker kill -s USR1 <agent>` + parsing new `object-alive`
lines from `docker logs` since the cycle started. This is the same
mechanism as the leak-triage recipe in `.claude/rules/gstreamer-gotchas.md`.

Prerequisites (not satisfied by default - this is why the flag is off by
default):

- The node-agent must be started with `GST_TRACERS='leaks(filters=GstObject)'`
  and `GST_DEBUG=GST_TRACER:7` (and ideally `GST_LEAKS_TRACER_SIG=1`, so
  `USR1` triggers the dump instead of only exit).
- The vendored `/opt/gst` build has no coretracers by default - copy the
  Fedora one in first (`docker exec <agent> cp
  /usr/lib64/gstreamer-1.0/libgstcoretracers.so /opt/gst/lib64/gstreamer-1.0/`)
  and restart the container.

Without these, `--gst-leaks` samples will come back empty (not zero - the
script cannot distinguish "definitely 0 live objects" from "tracer not
armed," so it reports missing rather than guessing).

## AMD VRAM sampling: implemented, untested

`sample_vram_mb()` in `scripts/harness/run-soak-profile.sh` tries `nvidia-smi` first
(Tower, NVENC) and falls back to summing
`/sys/class/drm/card*/device/mem_info_vram_used` (AMD sysfs, bytes -> MiB)
when `nvidia-smi` is unavailable. The AMD fallback path is implemented but
**has not been exercised against a real AMD host** - hermes (the AMD/VA box)
is currently off limits for testing (see the "No testing on hermes"
standing note). Treat AMD VRAM numbers from this harness as unverified until
someone runs it there.
