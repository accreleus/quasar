---
name: quasar-diagnose
description: >
  Use when you need to diagnose a live Quasar streaming session's smoothness —
  "why is the stream hitching", "is it network or encoder or client", "diagnose
  the session", "not smooth", "what's the verdict", "pull the trace", "pull the
  diagnostic bundle", "compare runs", "soak/collect samples", "run the ABR
  experiment", "run the ABR matrix", "smoothness analysis", "stream perf tuning",
  "encoder saturation?", "network congestion?", "client presentation limit?".
  Pairs with quasar-host (bring the stack up), quasar-session (drive a live
  session), and quasar-netem (inject network impairment for experiments).
---

# quasar-diagnose — stream smoothness analyser

**The mechanics are `make` verbs. This skill is the wrapper.** Reading a live
session needs no script from this directory: `scripts/dx/session.sh` fetches, and
`scripts/dx/session_diagnose/` analyses. What stays here is the multi-run work
(soak, compare, the experiment matrices, the HTML report) and the analysis facts
in `config.json`.

Never ask which host/session/mode first. Run the verb; if `SID` is unknown use
`SID=latest`, and if that fails the error names the next command.

## Reading one live session

```
make session-verdict  SID=latest HOST=gpu-test            # the answer, in four lines
make session-diagnose SID=latest HOST=gpu-test            # the full analysis
make session-metrics  SID=latest HOST=gpu-test SINCE=5m   # the sample table
make session-trace    SID=latest HOST=gpu-test            # window + events
make session-bundle   SID=latest HOST=gpu-test            # raw JSON to a file
make session-logs     SID=<id>   HOST=gpu-test SINCE=10m  # the session's containers
make session-list     HOST=gpu-test                       # what is running
```

`SID=latest` resolves to the newest `state=running` session. `WINDOW=<from_ms>,<to_ms>`
scopes verdict/trace/bundle/diagnose. `JSON=1` makes any of them machine-readable.

Containers, for `session-logs` and for reading a host by hand:
`quasar-sess-<sid>` (the app), `quasar-pulse-<sid>` (its audio sidecar), plus the
node-agent container filtered to the sid. Where the target host has a Dozzle MCP
endpoint recorded in `_shared/hosts.json`, **prefer it** — same logs, no ssh hop.

## The contract every verb honours

Exactly one `RESULT` line on stdout, last:

```
RESULT status=<ok|degraded|failed|error> target=session-<verb> sid=<sid> host=<host> [verdict=…] [reason=…]
```

| exit | meaning |
|---|---|
| 0 | ok — including a verdict this tooling has never seen (that is DATA, printed verbatim) |
| 1 | degraded — the classifier returned a `likely_*` verdict |
| 2 | tool error (auth, unreachable, 404). The message always names the next command. |
| 3 | usage |

**The verdict vocabulary belongs to the control plane**
(`control-plane/internal/session/classifier.go`), not to this skill. Nothing here
validates a verdict against a list. A stale four-string copy is what made a
healthy `nominal` session exit 2 on 2026-08-22 and sent an agent to psql.

## When a verb fails

| symptom | next step |
|---|---|
| `reason=no-token` | `scripts/dx/admin_token.sh --host <h> --fresh` — it prints every tier it tried |
| `reason=unauthorized` (401/403) | same: the cached token outlived the stack. `--fresh` |
| `reason=not-found` (404) | wrong stack. A stopped session keeps its row. `make session-list HOST=<h>` |
| `reason=unreachable` | `make status HOST=<h>` |
| `reason=no-running-session` | `make session-list HOST=<h>`, or launch one (`quasar-session`) |
| `reason=unrecognised-verdict` | not a failure. The control plane grew a verdict; report it as-is |

Credentials come from ONE place: `scripts/dx/admin_token.sh`
(`$QUASAR_ADMIN_TOKEN` → cached token → mint on the host). Nothing in this skill
reads `deploy/.env`.

## Multi-run work (still scripts here)

```
scripts/qdiag-sample        [--host ROLE|NAME] [--session latest|UUID] [--interval N] [--duration M] [--out dir]
scripts/qdiag-compare       <runA.json> <runB.json> [...] | --latest N [--allow-cross]
scripts/qdiag-experiment    [--host] [--shapes clean,mild,...] [--abr on,off]
scripts/qdiag-ir-experiment [--experiment ir|fec] [--rows ...] [--cols ...] [--dry-run|--resume|--fail-fast]
scripts/qdiag-report        <run1.json> [...] --out <report.html> [--summary <file.md>]
scripts/qdiag               a shim for `make session-diagnose`
```

### sample
Polls the bundle every `--interval` s for `--duration` s and writes one run file
to `qdiag-runs/` (validated: ≥ 10 samples spanning ≥ 80 % of the duration). The
poller runs on the host over one ssh hop; its bearer comes from `admin_token.sh`
and its base URL from `_shared/hosts.json` `api`.

### compare
Two or more run files, side by side; refuses a cross-app/profile/host comparison
without `--allow-cross`. Fills `assets/comparison.md.tmpl`. Key metrics: ABR
setpoint sawtooth amplitude, encode_ms p50/p95, fps p50, present_interval_sd p95,
verdict distribution.

### experiment
The ABR × netem matrix via `scripts/harness/run-st-trace.sh`, then compares cells. Fills
`assets/experiment-matrix.md.tmpl`. Prerequisites: stack up, the bench app seeded,
Chrome-for-Testing at `/tmp/cft/`, playwright at `/tmp/t8-driver/`.
`abr_mode=smooth` is the **shipped default** (SPT-10 #346) and a legitimate arm —
the old "NOT IMPLEMENTED" note in `config.json` was years stale and is gone.

### ir-experiment (and `--experiment fec`)
Rows are env settings, columns are netem shapes; every value lives in
`config.json`'s `ir_experiment` / `fec_experiment` block — never hardcode a cell.
Per cell: mutate the host's `deploy/.env` (idempotent, via
`scripts/dx/session_diagnose/ir_env.py`), recreate `quasar-node-agent`, **Gate A**
(container env + the agent-log line; a cell that fails Gate A aborts and is never
sampled), `qses run --keep`, netem mid-session via `qnetem sender`, `qdiag-sample`,
then `qnetem clear` + `qses stop`. `deploy/.env` is restored on every exit path.
FEC's Gate A is `per_row_expect`: each FEC-on row asserts its own log line
(`fec-percentage=20`, `auto FEC controller armed`); the `fec-off` control passes
on env evidence alone.

### report
Self-contained single HTML file (inline CSS/SVG, no CDN — renders from `file://`).
Sections: `#summary` (from `--summary <file.md>`, hand-written), `#verdict-table`
(colour-coded against `control_row`, noise band ±`noise_band_pct` or
±`noise_band_abs_ms`, whichever is larger), `#graphs`, `#configuration-appendix`,
`#limitations`. Failed cells are shown, never dropped.

## Where the code lives

| what | where |
|---|---|
| fetch + verbs + exit contract | `scripts/dx/session.sh` |
| admin bearer | `scripts/dx/admin_token.sh` |
| analysis, report, experiment runners | `scripts/dx/session_diagnose/` |
| analysis FACTS (thresholds, matrices, netem shapes) | `.claude/skills/quasar-diagnose/config.json` |
| host addressing | `.claude/skills/_shared/hosts.json` (the only registry) |

## Self-test
```
scripts/validate                           # offline: config + syntax + template coverage
scripts/validate --live                    # + a live read against the gpu-test host
scripts/validate --live --host aux-infra
```

## Thresholds (from `control-plane/internal/session/classifier.go`)

| Constant | Value | Meaning |
|----------|-------|---------|
| `encoderCeilingMs` | 16.0 ms | encode_ms p95 at/above this → encoder_saturation window |
| `hitchSdThresholdMs` | 18.0 ms | present_interval_sd ≥ this → hitch window |
| `congestionRttP95Ms` | 50.0 ms | rtt p95 above this → congestion signal |
| `congestionLossDelta` | 5.0 packets | cumulative loss rise above this → congestion signal |
| `classifierMinHostFps` | 50.0 fps | host fps p10 below this → encoder NOT steady |

Classifier priority: network_congestion → encoder_saturation → client_presentation_limit
→ nominal / indeterminate_client_hidden / unknown.
