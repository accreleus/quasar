# CLAUDE.md — Agent operating context for Quasar

This file is the durable context for every AI-agent session in this repo. Read it before doing anything. It encodes the invariants that keep work — especially from cheaper models — on the rails.

## What Quasar is
A self-hostable, multi-host cloud-gaming platform (a self-hosted GeForce Now). Built on the strongest components of `games-on-whales/wolf` (the Wayland compositor, virtual input) while taking a different direction: no discontinued NVIDIA GameStream / Moonlight protocol, WebRTC-to-browser first. (Wording note: never describe Quasar as a "clean-room successor" — Wolf is MIT and held in high regard here.)

Full rationale: `docs/architecture-and-plan.md`. Phases 0–5 are complete (records below); their plans, execution records, and latency reports are archived under `docs/completed/`. Active work follows the roadmap-spec-v2 wave ladder — see "Current phase & standing defaults" below.

## The one rule that matters
**Design for the end state, not the current phase.** Wolf grew from a single-host, single-user core, so multi-host / K8s / multi-user were retrofitted onto it later. If a capability is on the roadmap, the architecture must anticipate it from the first commit. Do not introduce a workaround that a later phase will have to tear out.

## Architecture invariants (do not violate without escalation to Opus)
1. **Control plane / node agent split.** `control-plane/` (Go) owns accounts, API, signaling, scheduling, and holds no per-host GPU state. `node-agent/` (Rust) owns a host's GPUs and runs sessions. Multi-host and K8s fall out of this split — never collapse it back into a monolith.
2. **No Moonlight / GameStream.** No RTSP, no ENet control channel, no NVIDIA RTP payloaders, no `_nvstream` mDNS. Gone permanently.
3. **Transport is an interface.** Media path is `compositor -> encode -> [transport slot]`. `webrtcbin` is implementation #1 (browser). The compositor and encoder must not depend on the transport. A native-client UDP transport is expected later.
4. **GStreamer owns compositor + encode.** The compositor is the upstream `gst-wayland-display` element; the encode path stays GStreamer. The framework is not the latency bottleneck — pipeline construction is.
5. **State is external.** Control-plane state lives in Postgres (Phase 1+), never in-process-only. No TOML-as-database.
6. **Web client is one TypeScript + React + Vite SPA, not many.** `web/` is a single app that talks only to the control plane (P1-B `control-api.md`) — the node agent is never contacted directly by a client (follows invariant #1). It has two **role-separated areas in one codebase**, collapsing Wolf's three UIs (WolfUI launcher + WolfManager + WolfDen):
   - `/app/*` — the **user** UI (minimal GeForce-Now-client experience): app library, search, account, live session view.
   - `/admin/*` — the **admin/operator** UI: app-catalog configuration (what users can launch), host/GPU load + capacity, session oversight, user management.

   **Authorization is server-enforced, never UI-gated.** Admin is a `role` on the user (`control-api.md` / `schema.md`); admin API endpoints reject non-admin bearer tokens regardless of client. The `/admin` route gating in the UI is for UX only — hiding admin UI is *never* the access control. **Never** introduce a client-side-only admin flag as the enforcement mechanism. The shared API client, auth, and component library stay common to both areas.

   *Mechanism (implemented, reuse it — don't re-invent or add inline `role` checks):* the control-plane gate is the `auth.RequireAuth → RequireAdmin` middleware, wired per-endpoint at route registration; the admin surface and its server-enforcement rule are enumerated in `control-api.md §Authorization`. The first admin is **operator-provisioned** at startup via `BOOTSTRAP_ADMIN_{EMAIL,USERNAME,PASSWORD}` (idempotent, no-op once an admin exists) — *not* "first to register wins", since `/v1/auth/register` only ever mints `role=user`.

## Frozen interfaces — change only via Opus + explicit human sign-off
- `protocol/signaling.md` — WebRTC signaling messages
- `protocol/input.md` — input wire format (DataChannel)
- `protocol/agent-api.md`, `control-api.md`, `schema.md` (Phase 1–3 contracts) and all amendments
These are load-bearing: cheap-model tickets on the client and host sides depend on them being stable. Implement against them freely; changing them requires Opus + explicit human sign-off. Additive, admin-gated extensions that change no existing shape are the documented exception (see `control-api.md §Authorization`) and still want sign-off. If a ticket seems to need a contract change, stop and escalate.

## Repo map
- `protocol/`     shared wire definitions (frozen interfaces) — **a git submodule of `quasar-protocol`** (the canonical contracts repo, also submoduled by `photon`, the native client — renamed from `quasar-client` in the 2026-08-20 org move). Run `git submodule update --init` after cloning/pulling. **Contract changes now happen in `quasar-protocol`** (Opus + sign-off as before), then bump the submodule pin here and in `photon`. Builds don't read `protocol/` (it's docs), so a deploy box with an un-init'd submodule still builds/runs — but **`go test ./...` does**: `TestOpenAPIDrift` reads `protocol/openapi.yaml` and fails with "no such file or directory" in a fresh worktree until you `git submodule update --init protocol`.
- `node-agent/`   (Rust) real home; graduated out of the Phase-0 spike in Phase 1 (the `spike/` tree was retired 2026-07-17 — git history). **`session/pipeline.rs` is now a ~620-line facade over a `session/pipeline/` submodule tree** (`caps`/`encoders`/`source_branch`/`abr_glue`/`webrtc`/`rtp_ext`/`audio_branch`/`probes`.rs) — the gst-graph construction split (TD-01, review #5). Locate pipeline code by submodule, not one giant file.
- `control-plane/`(Go) real home; Phase 1 service exists (auth, CRUD, session lifecycle, signaling relay). **`internal/session/coordinator.go` is now a ~255-line facade** (implements `agentws.Events`) over per-concern files (`launcher`/`swapper`/`health_evaluator`/`host_lifecycle`/`profile_resolver`/`agent_state`.go); `swapper` + `healthEvaluator` own their state+mutex (TD-02, review #7). **The post-placement stream decision is `internal/session/stream_plan.go`** — `gatherStreamInputs` (launcher.go) does every read, `planStream` decides with no I/O, the caller logs + writes once + `applyTo`s the session. Rung/codec/cert-cap behaviour changes go in the pure function, not back into the launch path; its cert ranking (`pickCert`) is guarded against its SQL twin by `TestCertForRungMatchesPickCert`. **Self-update lives in `internal/platform`** (2026-09-05, #104): `buildinfo` (the control plane's own identity), `plan.go` (`PlanRelease` — the pure release decision: ordering by schema version then build time, per-target eligibility reasons, faults; every ADR 0002 rule lives here, not in handlers), the stable/edge release sources + the `platform.release_detect` job, and the apply machines (`apply_runner.go` per host, `apply_fleet.go` + `apply_self.go` for the fleet run and the control plane's own recreate, `apply_revert.go`). **The updater is `internal/updater` + `cmd/quasar-updater`** (image `deploy/Dockerfile.updater`, compose service `quasar-updater`): `Plan()` is the pure accept/reject + env-rewrite + command decision, the executor runs docker/compose; its unix-socket API is host-local and NOT a frozen contract.
- `web/`          unified TypeScript + React + Vite SPA; scaffolded in Phase 1 (ports the Phase-0 spike client's signaling/input logic).
  **The admin data cycle lives in `web/src/lib/resource/` (2026-08-20) — use it, don't hand-roll another loader.**
  `useResource({label, fetch, pollMs?, initialData?})` for reads, `useAdminAction` for writes,
  `<ResourceStates>` for the loading/error/empty lines. It owns cancellation, the visibility-aware
  poll (a chained `setTimeout`, never `setInterval` — stacking is unrepresentable), `ApiError` →
  message, and the mutation-beats-poll race. Load errors go to state, mutation errors reject to the
  caller; that asymmetry is deliberate. When a page genuinely doesn't fit (inline per-row errors,
  a multi-key pending set), call `resource.mutate` directly rather than growing the options bag.
  13 older admin files still hand-roll the cycle — migrating one is welcome; adding a new one is not.
  **Typecheck gate is `make test-web`.** The two hand-typed alternatives both lie:
  `tsc --noEmit -p tsconfig.json` is a silent no-op on this repo's solution-style tsconfig, and
  `npx tsc -b --noEmit` in a tree with no `node_modules` (every agent worktree) reports success for
  a tsc it never ran. `make test-web` runs it in the devtools container and has caught a real type
  error both of those passed.
- `deploy/`       compose now, k8s manifests later. **Build images with `deploy/build-images.sh`, never a hand-typed `docker build`** — it forces an explicit `--target` (a bare build takes the LAST stage regardless of `-t`), rejects a `--build-arg` for an undeclared ARG (Docker ignores those silently), and validates every artifact against `deploy/image-contract.json` before promoting `:latest`. The contract is the durable form of every image defect that reached production; **never relax an assertion to make a build green.** **`deploy/` is the OPERATOR front door: it holds only what someone installing Quasar needs.** Contributor tooling lives under `scripts/` — `dev/` (the dev container wrapper and dev seeders), `verify/` (verify stages + the devtools image), `harness/` (acceptance harnesses, `lib/`, `checks/`, the `apitest` module, `peer-driver.mjs`), `release/` (release-evidence gates), `dx/` (the Makefile's orchestration) — and non-operator compose overlays live in `deploy/overlays/`. Don't add a new development script to `deploy/`.
- `third_party/`  vendored forks — **currently only a README**; gst-wayland-display/inputtino are built from upstream pins in `deploy/Dockerfile.vulkan` (the single image lineage: dev/runtime/nv targets on the `quasar-base` family), vendored only when modification is needed. Don't go looking for source here. Pins + flip instructions live in **`docs/third-party-pins.md`** — current: gst-wayland-display fork `310c03ec` (upstream base `43d4c25`), gst-interpipe `0c454917` (gow fork) + two vendored caps-leak patches, GStreamer `1.28.4` + vendored patches. **A pin bump on either fork is gated on a live exercise, not a green build** — see "Fork-bump verification policy" in `docs/third-party-pins.md`.
- `CONTEXT.md`    the domain glossary (chain, rung, cert cap, stream plan, probe, envelope, entitlement, home, derived tile). Read it before naming things; add a term when work resolves one, rather than coining a synonym.
- `docs/`         design docs (config knobs: `docs/configuration.md` — every env var, default, accepted values). **`docs/tech-debt/REVIEW-REMAINING.md`** is the scoped backlog of still-open review findings (#6/#8/#9/#10/#13/#14/#15; all low/subjective or a future spike — none are bugs); the executed TD-01/TD-02 refactor plans are archived in `docs/completed/tech-debt/`.

## Conventions
- Rust: 2021 edition, `cargo fmt` + `cargo clippy -- -D warnings` clean before done.
- Go: `gofmt` + `go vet` clean; module path is `github.com/accreleus/quasar/control-plane`.
- Commits: conventional-commits style (`feat:`, `fix:`, `docs:`, `chore:`).
- No secrets in the repo. This is a private repo, but still: no keys, tokens, or `.env` committed.

## Git branching & environments (operator policy — 2026-07-07)
- **`main` = production.** Any merge INTO `main` requires **explicit human sign-off from the operator**. Never merge to `main` autonomously — not even a green feature branch.
- **`develop` = persistent, unstable integration branch.** All day-to-day work targets it.
- **Workflow:** branch off `develop` → do the work → merge **back into `develop`** with **no PR required**. Feature-branch → `develop` is self-serve; `develop` → `main` is the only sign-off gate. Default: `git checkout develop && git pull && git checkout -b <feature>`; land with `git checkout develop && git merge <feature>`.
- **Push branches to origin as you go — "no PR required" does not mean "no push".** Origin is
  how concurrent agents and other machines see in-flight work; a local-only branch is invisible
  to all of them. The 2026-08-31 develop merge is the cautionary tale: a multi-day local-only
  campaign produced a duplicate migration number (two 0070s, a boot crash-loop had it deployed
  unnoticed), a divergent contract amendment, and a pushed develop that briefly pinned an
  unpushed protocol commit (breaking fresh clones). Push the feature branch at least daily and
  before any long pause; a submodule commit must be pushed to its own remote BEFORE any
  superproject pin of it is pushed; and pull/rebase on develop before starting a migration so
  numbering races surface early.
- **Dev environments — addressed by ROLE, never by hostname.** The role→host map and every real
  address, ssh alias, and key path live in `.claude/skills/_shared/hosts.json`, which is
  operator-local and gitignored; `.claude/skills/_shared/hosts.example.json` documents the
  schema and the role vocabulary. The roles:
  - **`gpu-test`** — the primary validation host and the DEFAULT target of every host-taking
    verb. It is the box with the NVIDIA GPU, so it is the only place NVENC/Vulkan encode paths
    can be exercised. Runs `develop`. Judge stream quality here.
  - **`aux-infra`** — network-impairment (netem/IFB) shaping and the headless browser peer.
    **Infrastructure, not a testing host**: it runs `develop` too, but do not run validation
    on it.
  - **`deploy-only`** — a production/staging deployment target; read-only verbs only.

  Per-host facts (GPU passthrough topology, driver variants, disk layout, reboot-volatile
  paths) belong in that host's `notes[]` in `hosts.json`, not in this file — skills surface
  them from there. Exception as before: when the operator explicitly directs specific testing,
  hosts run whatever is specified.
- **Scope:** `quasar` and `photon` (the native client, formerly `quasar-client`) follow this. `quasar-protocol` (frozen contracts, low churn) stays `main`-only and always sign-off-gated. Frozen-interface sign-off rules stack on top of this everywhere.

## Build / test
**The canonical developer interface is the root `Makefile` — start with `make help`** (see
`AGENTS.md` for the full operating guide: init, verification levels, instance isolation,
logs/diagnostics, destructive-op rules). The Makefile is a thin façade; it delegates to
`scripts/dx/*.sh`, `scripts/verify.sh`, `scripts/dev/dev.sh`, `deploy/build-images.sh` and
`deploy/redeploy.sh` — those remain directly callable.

All Rust + GStreamer work runs **inside the `quasar-agent-dev` container**, not on the host
(the host has no GStreamer / Rust / Wayland). Build the image once (`scripts/dev/dev.sh image`,
which builds `deploy/Dockerfile.vulkan --target dev` on the `quasar-base` family), then
mount the repo and work in it:
```
bash scripts/dev/dev.sh image
docker run --rm -v "$PWD":/workspace -w /workspace/node-agent quasar-agent-dev:latest cargo build
```
- Rust:  `cargo build` / `cargo test` (in `node-agent`); `cargo fmt` + `cargo clippy -- -D warnings` clean before done. (`make test-rust`)
- Go:    `go build ./...` / `go test ./...` in `control-plane`. (`make test-go`; DB-backed: `make test-db`)
- **Control-plane DB tests need a real Postgres — and silently skip without one.** The
  `internal/{auth,crud,session,signal}` integration tests `t.Skip()` unless `TEST_DATABASE_URL`
  is set, so a green `dev.sh go-check` does **not** mean the DB tests ran — it means they were
  skipped (it builds + vets + tests in `golang:1.24` but wires no database). To actually exercise
  them, run **`make test-db`** — the sanctioned path: it provisions a FRESH ephemeral Postgres on
  a per-worktree port (no shared-DB contamination, no port collisions between concurrent
  sessions). `-p 1` is mandatory either way: the tests share one database and truncate in setup,
  so package binaries must not run concurrently. **A control-plane ticket touching the DB is not
  DONE until the DB tests ran green, not just `go-check`.** (`scripts/dev/dev.sh go-test-db` still
  exists and attaches the long-lived `quasar-pg3` Postgres on the `quasar-p3-test` docker
  network; that shared database is stateful across runs, so prefer `make test-db`.)
- Acceptance/regression harnesses live in `scripts/harness/run-*.sh` and run inside the container (one-off
  experiment harnesses from completed investigations were culled 2026-07-17 — recover any from git history);
  `scripts/dev/dev.sh` wraps the build/run/test invocations (`dev.sh run <name>` runs one).
- A ticket is DONE only when its build + tests pass.
- **The glass-to-glass budget runs nightly on the `gpu-test` host** (`make nightly-budget-install|run|status HOST=gpu-test`, cron 03:30 UTC) — `docs/testing-bench-mode.md` "A nightly budget run" for the recipe, alert shape, and re-baseline policy.
- **Migrations are one-way at deploy time: never roll a control-plane binary back BELOW the DB's
  applied migration version.** Boot runs `golang-migrate` `m.Up()`; if the DB is at version N but
  the binary embeds only ≤N-1, it **crash-loops** (`fatal: migrations: no migration found for
  version N: read down for version N`). So deploying a DB-touching branch to a live stack and then
  `git checkout main` + rebuild wedges the control-plane (hit live on **two** fleet stacks during
  AS10-03). Recovery: redeploy the branch/commit that embeds the migration. An unmerged
  DB-touching control-plane PR therefore keeps its stack on the branch until merge — it converges
  to main on merge (main then embeds the migration), it cannot be reverted to main beforehand
  without running the down-migration and resetting `schema_migrations` first.

## GStreamer / WebRTC / encoder gotchas — moved to path-scoped rules
Load-bearing gotchas now live in `.claude/rules/` and auto-load when working with matching files:
- `.claude/rules/gstreamer-gotchas.md` — GStreamer-rs + encoder-property gotchas (loads for `node-agent/**`, `deploy/**`)
- `.claude/rules/webrtc-testing.md` — WebRTC / browser testing gotchas (loads for `node-agent/**`, `web/**`, `deploy/**`)
If you are doing pipeline, encoder, WebRTC, or browser-testing work purely over ssh without touching those paths locally, read the relevant rule file explicitly first.

## UI work — the design handoff is the spec
Any change touching `web/` rendering or a user-visible surface MUST start by
reading **`design_handoff_v3/`** (README + `screens/assets/console-v3.css` — the
token contract — + the matching `screens/*.html` mock: `login-v3`, `home`,
`loading-v3`, `loading-to-stream-v3`, `session-overlay-v3`, `admin-console-v3`
with its `assets/pages-*.js` section renderers), and MUST be visually verified
against it (designer agent / `visual-verdict` skill) before being presented as
done. v3 supersedes the earlier `design_handoff_quasar` / `design_handoff_v2`
packages (removed 2026-08-28; git history has them) — where they differ, v3 wins.

The rule that outlives either reference: **do not invent a style guide or
restyle from taste.** One exists. If no mockup covers the surface being changed,
say so explicitly and ask before styling. If you cannot reach the handoff at
all, stop and ask rather than improvising. (History: a milestone run that
skipped the handoff produced a full UI that had to be redone.)

## Model tiering (per-ticket tiers ride on each issue's `needs:*` label / kickoff doc)
- Opus 4.8: architecture, interface/schema design, WebRTC negotiation, the latency path, security/concurrency, integration debugging, writing tickets, reviewing seams.
- Sonnet 4.6: mid-complexity features against a clear spec.
- Haiku 4.5: routine, well-scoped, machine-verifiable units.
Escalate to Opus when: a ticket is ambiguous, touches a frozen interface or the latency path, or a cheaper model has failed it twice.

## Current phase & standing defaults
**Roadmap of record: integrated roadmap spec v2**
(`docs/design/plans/2026-07-06-roadmap-spec-v2.html` — library-provider model + wave
ladder), which supersedes the numbered-phase framing. W0 (image consolidation, #367)
and W1 (security wave, PR #374: invite-gated registration + device binding, migration
0020) are merged; W2 is active (console-mode ∥ Phase 9 closure; executed plans are
archived under `docs/completed/plans/`, live ones stay in `docs/design/plans/`). **"What's active right now" lives in GitHub milestones + the
session memory `current-focus.md`, not this file.**

**Standing operational defaults (knobs, not history):**
- **Self-update (2026-09-05, #104):** admins see and apply *platform releases* from Fleet ▸
  Releases. Channel defaults to `stable` (GitHub Releases + `platform-release-manifest.json`);
  `edge` follows a branch tag (default `develop`). Detection is the `platform.release_detect`
  job, weekly, Monday 02:00 UTC (editable in the Jobs tab; run-now = "Check now"). Applying goes
  through the per-host **updater** (`quasar-updater` in every compose stack), which only accepts
  digests under `QUASAR_UPDATER_ALLOWED_NAMESPACES` (default the org's GHCR namespace). Control
  plane first, then hosts, never below the DB's migration (ADR 0002); the fleet run drains the
  whole fleet before the control-plane step because a control-plane restart ends every session
  today (#128). A **source-built host or control plane is never offered a release** — it shows
  the manual `redeploy.sh` recipe instead. Existing installs add the updater once
  (`docs/upgrading.md` "The updater"). Publishing is a `vX.Y.Z` tag push on `main`
  (`make release VERSION=`), still to be exercised live after the first main promotion.
- **ABR is ON by default, mode `smooth`** (SPT-10 #346, 2026-06-27). `smooth` is
  encoder-aware + smoothness-biased (under congestion: present σ p95 ~69→19 ms,
  freezes 14→2 vs `protective`; identical on a clean path; preserves the #68 emergency
  fast-drop). Revert per-host: `QUASAR_ABR_MODE=protective`; disable: `QUASAR_ABR=0`.
  Hardware encoders are the validated target; openh264 software ABR is best-effort
  (can't saturate 1080p@8Mbps → GCC sees no congestion). **The soak is done —
  treat `smooth` as permanent.** It was soaked 2026-08-17→24 over 147 bench runs,
  which also produced the #370 re-characterisation; no further operator soak is
  outstanding.
- **Encoders:** AMD/Intel = VA (ZC-03 DMABuf zero-copy). **NVIDIA = Vulkan by
  default** (2026-08-12): `docker-compose.nvidia.yml` sets
  `QUASAR_ENCODER=${QUASAR_ENCODER:-vulkan}`, so H.264 and HEVC encode with
  `vulkanh264enc`/`vulkanh265enc`. Rationale:
  #489 is an NVIDIA-driver NVENC teardown UAF spanning the 595 **and** 610
  branches — no driver pin escapes it — and Vulkan is immune, so the default path
  is off the affected library. **AV1 on Vulkan exists as of
  2026-08-22 (merge 345bad8c): our vendored `vulkanav1enc.patch` (gst-chain patch 8,
  with the `gstreamer-vulkan-rc-retarget-no-reset.patch` library fix as patch 7)
  adds a genuine Vulkan Video AV1 encoder (needs NO ring pin, unlike HEVC).
  **The three per-codec knobs `QUASAR_VULKAN_H264` / `QUASAR_VULKAN_HEVC` /
  `QUASAR_VULKAN_AV1` all DEFAULT ON (2026-08-22)** — they exist only to
  deliberately disable a codec (`0`/`false`/`off`; anything unrecognised warns and
  stays on). Disabling one, or an image whose Vulkan element is missing, makes
  `pipeline::resolve_effective_encoder` borrow the vendor HW encoder
  (`nvcuda<codec>enc` / `va<codec>enc`) per session and the codec stays advertised;
  with no vendor element the codec drops off the host — except h264, which is the
  floor and stays on `vulkanh264enc` with an error logged.
  `QUASAR_ENCODER=nvenc` in `deploy/.env` (or an admin host override) restores
  the whole NVENC path.
  **That NVENC fallback needs `libnvrtc`, which the agent now fetches at RUN TIME
  (#545, 2026-08-26) — there is no NVIDIA image any more.** `quasar-node-agent` is
  the universal agent image (CUDA-built like every lineage; `quasar-nv` is retired,
  and the belief that the universal image was `CUDA_ENABLE=0` with no `cudaconvert`
  was already false when it was written). The one library no driver `.run` can
  supply — NVRTC is CUDA toolkit userspace — is provisioned by
  `node-agent/src/cuda_runtime.rs` into `cuda/` inside the driver volume, pinned by
  version + published sha256, gated on an r580+ driver, **soft on every failure**:
  no NVRTC ⇒ the four `cuda*` elements are simply absent and Vulkan encode is
  unaffected. Knobs `QUASAR_CUDA_RUNTIME` / `QUASAR_CUDA_RUNTIME_DIR`
  (`docs/configuration.md`); the pin lives in that source file, `deploy/pins.env`
  discipline. The host `gop` setting is frames AT THE 60 FPS REFERENCE —
  the agent scales it to the session fps so keyframe cadence in time is constant.
  Full record: memory `vulkanav1enc-campaign`.
  `QUASAR_NVENC_DEFER_TEARDOWN` keys on the *effective* encoder, so fallback
  sessions stay #489-protected. There is **no separate Vulkan image** — since W0
  (#367) every lineage builds from `deploy/Dockerfile.vulkan` and shares the
  patched GStreamer (vendored RC + PTS patches — `docs/configuration.md`). The in-compositor NV12
  mode (GW-02, `QUASAR_COMPOSITOR_NV12`) was **retired** 2026-07-07 (#367/PR #372) —
  it was a ZC-03 alternative, not a win, and blocked the gst-wayland-display re-pin.
  Load-bearing VA lesson that survives it: a shared `GstVaDisplay` must be injected
  **at VA-element creation** (`node-agent/src/session/va_share.rs`), before a
  `device-path` read or NULL→READY makes the encoder bind its own display.
- **Multi-codec (2026-07-25): H.264 + HEVC + AV1, one codec per session, resolved
  server-side at launch** (profile codec list ∩ host encoder set ∩ client decode probe ∩
  failure history, guaranteed h264 floor; migrations 0031/0032; spec
  `docs/design/plans/2026-07-22-multi-codec-hevc-av1-spec.md`). SHIP-DARK: profiles
  default h264-only; admins enable per profile (list ORDER = preference; the admin API
  reorders, the UI only edits status). Wire vocab `h264|h265|av1` vs catalog vocab
  `hevc` — bridged ONLY in `control-plane/internal/session/codec.go`. Vulkan hosts:
  h265 and av1 encode on Vulkan by default (`QUASAR_VULKAN_HEVC=0` /
  `QUASAR_VULKAN_AV1=0` disable them per host, falling back per session to
  `nvcuda<codec>enc` — see the Encoders bullet). While h265 is enabled the agent
  auto-pins the compositor encode ring to 2 — a 5090 ring-slot tiling defect blacks
  1-in-4 frames at RING=4 otherwise. Browser reality:
  **the HEVC-headless rule and the `profile=main` encoder-probe rule live in
  `docs/testing-bench-mode.md`** — read it before diagnosing an HEVC run or
  hand-typing a `gst-launch` encoder probe. AV1 decodes everywhere in Chrome (dav1d). All three
  codecs live-validated 2026-07-25.
- **External (stream) resolution lever + ABR resolution/fps ladder work on the Vulkan
  encode path since 2026-08-22 (#501, develop 29882572)** for h264 + h265 + av1: the gwd
  fork's `vulkanscale` element (pin cdd0a764) gives the scale stage a `[videorate?]
  vulkanscale ! capsfilter` arm, gated at build time on the factory existing. h265 was
  briefly gated off on the belief that no driver could start `vulkanh265enc`; that was a
  probe caps-negotiation artifact (unpinned `profile` negotiating `main-444`), the
  production path always pinned `profile=main`, and the gate
  (`scale_stage::vulkan_resize_validated_for_codec`) now passes every codec.
  HARD DEPENDENCY: the image must also carry the 9th vendored patch
  `vulkan-enc-output-state-on-resize.patch` — without it a grow step in any second
  Vulkan session is an NVENC MMU fault (Xid 31), because the encoder kept the launch-size
  DPB. Each rung step on Vulkan is an encoder session restart (VRAM flat over 84 flips).
  `abr_ladder_resolution` is inert unless the master `abr_ladder` is also true.
  Evidence `docs/reports/2026-08-22-vulkanscale-validation/`; memory `vulkanscale-campaign`.
- **Deep glass-to-glass trace is an always-on client (Chrome) capability** — the
  host-side overlay/probe was removed (#270; it could crash the stream).
- Feature backlog: #39 (configurable swap disposition), #273 (per-session GPU routing +
  admin GPU selection), #227 (LTR / intra-refresh for unstable-network recovery —
  send-side ULPFEC shipped as `QUASAR_FEC_PERCENTAGE`, default off).

## Phase records — 0–5 + Optimization Spike/AS complete
Full plans, execution records, latency reports, and per-phase verdicts: `docs/completed/`
(compact verdicts also in the session memory `quasar-phase-records.md`).

## Code exploration
For structural code questions use `workspace_search` / `workspace_symbol` from the
workspace-intelligence MCP below, plus Grep/Glob. (codebase-memory-mcp and graphify
were both removed.)

## Container logs — access is configured per host, see hosts.json
Log access is a per-host deployment detail, not a repo-wide fact. Where a host runs a
Dozzle instance exposing an MCP endpoint, that endpoint is recorded in the host's entry
in `.claude/skills/_shared/hosts.json`. Its tools read container logs and container state
directly — **prefer them over an `ssh … docker logs …` hop** on any host that has one:
faster, no ssh round-trip, no ControlPath/agent setup, and log output comes back already
scoped instead of as a wall of terminal text.

- The MCP is configured **user-scoped** (`~/.claude.json`), deliberately NOT in the repo's
  MCP config — it is operator LAN infrastructure. If the `dozzle` tools aren't in your tool
  list, you're not on that machine: fall back to ssh through the DX layer and say so.
- Scope is one host's containers. Every other host is unaffected — keep using the DX layer /
  `quasar-host` skill (which speaks in roles) for those.
- Everything else about host work is unchanged: this replaces the log-reading step, not the
  `make` / `quasar-host` operating verbs.

## Workspace intelligence (trial — use it, and say when it misleads)
**Status 2026-08-20: the server is not available; the MCP is disabled in `.claude/settings.local.json` (`disabledMcpjsonServers`). Skip the steps below until it is re-enabled; use Explore agents, `git log`/`gh`, and the project memory instead.**

The `workspace-intelligence` MCP (operator-local MCP config, server on :8090) holds the
indexed Quasar workspace: this repo + photon (the native client) + reference repos
(Wolf, gst-wayland-display, inputtino, gst-interpipe), their GitHub
issues/PRs, and durable memory from prior sessions.
Workspace ID (all tools need it): `303cb3b6-ba5a-4d5d-a5f5-815a966abd99`

- **Task start:** call `workspace_context_pack` BEFORE manual exploration.
  Pass the task, and paste everything you've gathered (issue text, error
  output, logs) into `context` — raw, unsummarized. References like `#123`
  resolve automatically against ingested issues/PRs.
- **Read the pack:** `applicable_skills` lists repo skills to invoke first;
  `historical_notes` are prior sessions' learnings; `related_context_hints`
  tell you when reference repos (Wolf, gst-wayland-display) have relevant
  context — follow up with `workspace_expand_related_context` for debugging,
  protocol, or performance work.
- **Contract-affecting changes** (protocol/, signaling, encode pipeline):
  run `workspace_impact` with `cross_repo=true` — it reports photon (native-client)
  fallout with suggested follow-up tasks.
- **Task end:** call `workspace_learn` — outcome, files changed, and one
  durable note (gotcha/decision/fact, with `file_path` when it's about a
  specific file). This is what makes the next session smarter.
- If a pack is unhelpful or misleading, note it in your final summary — that
  feedback is part of an active evaluation of this tool.


## Agent skills

### Issue tracker

GitHub Issues on `accreleus/quasar` via `gh`; PRs are not a triage surface (Alice reviews PRs). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` at the root + `docs/adr/`, created lazily by `domain-modeling`; `docs/architecture-and-plan.md` and `protocol/` remain the architecture and contract sources. See `docs/agents/domain.md`.
