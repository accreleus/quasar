---
name: quasar-session
description: Use when you need to launch/observe/tear down a live Quasar session from this workstation — "launch a session", "drive a headless browser at it", "did it decode", "pull the setpoint/fps/encode_ms series", "watch the agent logs for ABR/ICE", "Chrome-for-Testing is missing", or any ABR/latency experiment needing a real WebRTC peer. Works against any configured stack by role. Pairs with quasar-netem (impairment) and quasar-host (operating the deployment).
arguments:
  - name: verb
    description: "qses verb: run, series, logs, ls, stop, provision, display, matrix, soak"
    required: true
  - name: "--stack"
    description: "target stack, by role or host name (default: the gpu-test role)"
    required: false
  - name: "--app"
    description: "app name to launch (default: the stack host's bench_app from _shared/hosts.json)"
    required: false
  - name: "--profile"
    description: "profile ID to launch by (e.g. 1080p60)"
    required: false
  - name: "--secs"
    description: "session duration in seconds"
    required: false
  - name: "--warmup"
    description: "browser measurement warmup period in seconds"
    required: false
  - name: "--keep"
    description: "keep the session alive after run (prints SID for follow-up verbs)"
    required: false
  - name: "--no-browser"
    description: "skip the headless browser peer (launch only)"
    required: false
---

# Quasar live-session one-liners (qses)

Hand-rolling the session lifecycle (login → launch → SID/SIG extraction → drive
Chrome-for-Testing via `scripts/harness/peer-driver.mjs` → metrics → delete) costs ~30
lines of fragile inline bash per attempt. `qses` cans the whole loop.

```
.claude/skills/quasar-session/scripts/qses <verb> [--stack=ROLE|NAME] [args]
```

## Where things run
- **The headless browser peer runs on the `aux-infra` host** — this workstation has
  no toolchain, and running the peer on the aux host is **sanctioned
  infrastructure** use even where building or quality-judging Quasar there is not.
  The verdict still comes from the stack under test.
- **`--stack` defaults to the `gpu-test` role.** It accepts a role name or a
  literal host name from `_shared/hosts.json`. When the stack host *is* the peer
  host the runner uses that host's local API URL; otherwise its `api_external`.
- Admin-credentialed verbs (`series`, `stop`) and `logs` run **on the stack's own
  host** — each stack's admin credentials live in its own `deploy/.env`, which no
  other host can read.
- Role overrides for a session: `QSES_PEER_ROLE`, `QSES_STACK_ROLE`.
- **The browser driver (`scripts/harness/peer-driver.mjs`) is executed from the PEER
  HOST's repo checkout, not from this workstation.** Editing it locally changes
  nothing until the peer's checkout carries the change, so a harness fix living on
  an unmerged branch silently does not apply and the run is measured by whatever
  the peer has. Every `run` now prints a `HARNESS peer=… head=… driver=…` line —
  check it before trusting a surprising verdict, and check it on **both** peers
  when alternating, since two peers on different commits produce two different
  measurements of the same stack. (2026-08-11: peers on develop carried the
  pre-`10d17bb0` luma probe, which samples ~5 s from the instant decode is
  confirmed — during a cold Steam start, ~35-55 s before first paint — and
  reported `mean=0.0` on sessions that were streaming perfectly. Alternating
  peers made that read as a bistable "black stream" bug in the compositor.)

| verb | does |
|---|---|
| `run [--app 'Name'] [--profile ID] [--secs N] [--warmup N] [--keep] [--no-browser]` | full cycle: mint a throwaway per-run identity → launch → wait `running` → browser drive → decode verdict → delete. `--keep` prints the SID for follow-up verbs. Default app: the stack host's `bench_app`. `--profile <id>` launches by profile — the minted user is non-admin so the real eligibility gate applies; the resolved `profile_id`/`h264`/`WxH@fps` print, and an ineligible/unknown profile shows `launch failed: HTTP 409/400` not a traceback. **Browser launches resolve h264 to constrained-baseline — High does not decode in Chrome on any encoder.** |
| `series <SID>` | `abr_setpoint_kbps` / `fps` / `encode_ms` series, **chronological** (the API returns newest-first; this reverses). `setpoint: None` ⇒ ABR off — correct, not a bug. |
| `logs [abr\|ice\|all\|'<regex>']` | agent logs with the GL/EGL/smithay noise pre-filtered (unfiltered logs dump ~8k tokens of GL extension spam). `QSES_SINCE=5m QSES_LINES=25` env overrides. |
| `ls` / `stop <SID>` | list / delete sessions. **Always stop kept sessions — they hold encode slots.** |
| `provision` | (re)install Chrome-for-Testing + playwright-core on the peer host. `/tmp` wipes on reboot; the zipfile drops exec bits (the script re-chmods `chrome_crashpad_handler` — without it the browser dies "Permission denied (13)"). |
| `display <SID> [--stream WxH] [--render WxH] [--ui-scale S]` | adaptive external resolution: PATCH `/v1/sessions/{id}/display` with the stack's admin bearer. Thin wrapper over `scripts/dx/session_display.sh` (DX-layer-first — this verb has no logic of its own, it sets `HOST=<--stack>` for that script, which reads the same `_shared/hosts.json` roles). Prints `HTTP <code> ...` then the resulting `stream.external_*`/`render_*`/`rungs`/`ui_scale` (prints `absent` for any field the control-plane task hasn't shipped yet). |
| `soak [<SID>|--latest] [--duration 180] [--profile ladder|sawtooth|floor] [--dwell N] [--out DIR] [--dry-run]` | on-demand **bad-connection soak** for adaptive external resolution: walks the EXTERNAL (stream) size DOWN the rung ladder and back UP over `--duration`, sampling agent + browser telemetry throughout, and writes `REPORT.md` (step table with PATCH/echo latency and steady-state means, per-transition boundary analysis, ASCII timeline, internal-untouched verdict, auto-populated optimisation candidates) under `.diagnostics/soak/`. Thin wrapper over `scripts/dx/session_soak.sh` (DX-layer-first — it only sets `HOST=<--stack>`). Point it at a session **already being played in a real browser**: browser telemetry only arrives from a foregrounded tab, and the report says so when it is missing. It never launches or stops a session, it **never sends `render_*`** (proving the internal size stays put is the point), and it PATCHes the launch size back on every exit path including Ctrl-C. Exits 3 with a clear message when the host encoder cannot live-resize (`stream.external_resize_supported=false` — Vulkan; set `QUASAR_ENCODER=nvenc`/`va`). **Nothing here is wired into ABR — manual/scripted only.** |
| `matrix <SID> [--rungs WxH,WxH,...]` / `matrix --app 'Name' [--rungs ...] [--keep]` | steps external-resolution rungs (default `1280x720,1920x1080,1600x900`) against a running session and asserts the resize actually took effect: reconnects signaling **once** (the only browser attach for the whole run — reconnecting per rung would each mint a fresh offer), then for each rung PATCHes via `display` (reusing the reconnect's own admin bearer — see `QSES_ADMIN_TOKEN` below), waits 3s, and checks the held browser probe's `videoWidth` matches within 3s, `totalVideoFrames` advanced ≥30 across the step, and no `decodeFailed`. Also prints `qses series` fps/encode_ms before/after each step, and asserts exactly one **video-PC** `offer created` for the SID across the whole run (renegotiation-storm guard — there is also always one for the audio PC, a separate PeerConnection/offer, so this deliberately counts video only; see the offer-tagging gotcha below). Prints a PASS/FAIL table + overall verdict; exits nonzero on any FAIL. **`--app 'Name'`** self-launches instead of taking a SID: it mints a fresh long-TTL admin identity (`agentcreds.sh --role admin --ttl 2h`, unless `QSES_ADMIN_TOKEN` is already set) and launches with it, so the signaling-token reconnect's ownership requirement is trivially satisfied — and `matrix` then **stops that self-launched session itself on every exit path** (pass or fail, via an EXIT trap), printing `matrix: stopped session <sid>`; pass `--keep` to leave it running for follow-up verbs. An existing `<SID>` passed positionally is never touched — only a session `matrix` itself launched. |

`QSES_ADMIN_TOKEN` (env, exported on this workstation) is a pre-minted admin
bearer that overrides the `BOOTSTRAP_ADMIN` login in **both** `admin_exec()`
(`series`/`stop`/`matrix`'s reconnect) and `scripts/dx/session_display.sh`
(`display`/`matrix`'s per-rung PATCH). Two independent reasons to use it:
1. **No/stale `BOOTSTRAP_ADMIN` creds in `deploy/.env` on the stack.** Root-caused
   live on the devbox (2026-08-16): `qses matrix`'s per-rung PATCH used to call
   `session_display.sh` with no token, so it did its own *separate*
   `BOOTSTRAP_ADMIN` login on the target host — which silently failed there,
   so the PATCH never reached the control plane and the matrix table showed
   `HTTP ?` for every rung with no `display update accepted` in the agent log.
   Fixed: `matrix` now exports `QSES_ADMIN_TOKEN` right after its reconnect
   call succeeds, so every downstream call (including the per-rung PATCH)
   reuses that already-verified-working token instead of re-authenticating.
2. **Session ownership.** `POST /v1/sessions/{id}/signaling-token` (the
   reconnect `matrix` needs) is owner-gated — a SID from a throwaway per-run
   user (the `run` default, #399) 404s there. Mint one long-TTL
   (`make agent-creds ARGS='--role admin --ttl 2h'`), launch the session with
   it via `POST /v1/sessions`, `export QSES_ADMIN_TOKEN=<that token>`, then
   run `qses matrix <that SID>` — or just use `qses matrix --app 'Name'`,
   which does this automatically.

`run`/`ls` no longer register/log in as a shared, git-committed harness account
(#399) — each invocation mints a throwaway, auto-reaped identity via
`make agent-creds` (`scripts/dx/agentcreds.sh` → `POST /v1/dev/agent-session`),
run on the peer host against the target stack. That requires
`QUASAR_DEV_AGENT_AUTH=1` on the **stack** host and its per-boot dev key,
supplied via `QSES_DEV_KEY` or `QUASAR_DEV_AGENT_KEY` — see the `qses` script
header for the `ssh` + `docker exec` recipe to fetch a fleet host's key. The
per-host default bench app is that host's `bench_app` in `_shared/hosts.json`.

## Gotchas
- **Cross-host runs work** because the runner launches CFT with
  `--disable-features=WebRtcHideLocalIpsWithMdns` (real IPs — some hosts have no
  mDNS responder).
- **The `--stack` 404 trap:** `stop` is judged on the *selected* stack. A SID from
  another stack returns a genuine 404 that reads as "already gone" while the real
  session keeps running (this masqueraded as a control-plane DELETE race three
  times). Every outcome line names the stack — read it.
- ABR experiments: arm with `QUASAR_ABR=1` in that stack's `deploy/.env` + recreate
  the agent (see quasar-host); confirm with `qses logs abr` → "ABR armed". Apply
  impairment **mid-session** via quasar-netem, never before connect (a hard cap
  floods the cold-start keyframe → "decode failure"). For mid-session timing,
  background the run or sleep ~15 s after `qses run` starts before shaping —
  `--warmup` is the *browser measurement* warmup, not netem timing.
- If the peer host's LAN link is wireless, a cross-host "clean" baseline carries
  real jitter. Check `qhost notes --host aux-infra`.
- **`matrix` inherits the same peer-checkout provenance trap as `run`:**
  `scripts/harness/peer-driver.mjs`'s `--hold`/`--probe-every` HOLD mode runs from
  the **peer host's own repo checkout**, not this workstation's worktree — a
  fix to that file only takes effect once it's on the peer's checked-out
  branch. `matrix` doesn't print the `HARNESS peer=… head=… driver=…`
  provenance line `run` does (there's no single "the run" moment — the probe
  is held open across every rung); if a `matrix` verdict looks wrong, manually
  check the peer's `scripts/harness/peer-driver.mjs` sha256 (`ssh <peer> 'cd <dir>
  && sha256sum scripts/harness/peer-driver.mjs && git rev-parse --short HEAD'`)
  before trusting it.
- `matrix`'s held probe writes newline-delimited JSON to
  `/tmp/qses-matrix-<SID>.jsonl` (+ `.err`) on the **peer** host — left in
  place after the run for post-mortem; not auto-cleaned.
- **The hold probe's clock starts at process launch, not at decode-confirmed.**
  `--hold N` is consumed from the moment `node scripts/harness/peer-driver.mjs` is
  started — connecting and waiting for decode (up to `CONNECT_TIMEOUT_MS`,
  45s, plus page-nav) already eats into that budget before `matrix` issues its
  first PATCH. Root-caused live on the devbox (2026-08-16): a too-tight
  `HOLD_SECS` let the hold expire mid-loop and close the browser; every
  remaining rung then read back `decodeFailed:true videoWidth:0`, which looks
  exactly like a real resize/decode failure but is actually "the page is
  gone". `matrix` now budgets `HOLD_SECS = 60 (connect) + rungs×25 + 60`
  (generous on purpose) and stops the probe **explicitly** via a SID-scoped
  `pkill` right after the last rung's checks — the natural hold timeout
  should never be what ends it. If a `matrix` run still shows `decodeFailed`
  creeping in only near the end of a long `--rungs` list, that budget
  constant is the first thing to widen, not the resize logic.
- **The "offer created" log line is now tagged `session=<id>`** (node-agent/
  src/session/pipeline/webrtc.rs) — fixed alongside the `matrix` harness at
  the same time, because `matrix`'s one-offer assertion used to `grep -c
  "$SID"` a line that carried NO session id at all, so the count was always
  0 and the assertion could only ever FAIL, regardless of what actually
  happened. `matrix` now counts `offer created (video PC` lines tagged
  `session=$SID` specifically (there is also always one for the **audio**
  PC — a separate PeerConnection with its own offer — counted separately and
  ignored by this assertion on purpose). Until a stack is running an agent
  build with this tag, `matrix` sees 0 tagged + ≥1 untagged lines in the
  window and prints a **WARN**, not a FAIL — that reads as "can't judge this
  on an old agent", not "renegotiation storm". Re-run once the tagged agent
  build is actually deployed to get a real PASS/FAIL here.
