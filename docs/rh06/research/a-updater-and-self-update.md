# RH-06 research A — the current platform self-update / updater mechanism

Scope: milestone RH-06 "Deployment and update ownership migration". Snapshot of `develop` at
`32aee45` (2026-09-24). Primary sources only: Go/Rust source, migrations, `protocol/` contracts,
ADRs, `docs/upgrading.md`, plans. Every claim under "Confirmed facts" carries a repo-relative
`path:line`. Anything not directly read in source is under "Inferences / open questions" and marked
**[INFERENCE]**. Hosts are named by role only.

Short vocabulary used below: **CP** = control plane, **agent** = node agent, **updater** =
`quasar-updater`, **attempt** = one `platform_apply_attempts` row, **run** = one
`platform_apply_runs` row (fleet apply).

---

## 0. One-paragraph picture

A platform release is exactly two images, `control-plane` and `node-agent`, pinned by digest in a
`platform-release-manifest.json` asset. The CP detects releases (weekly job), decides eligibility
in a pure `PlanRelease`, and drives either a per-host attempt or a fleet run (CP first, then hosts).
Nothing inside a container recreates itself: every image move is performed by a per-stack
**updater** container that holds the docker socket and the stack directory, rewrites two lines of
the stack's `.env`, and runs `docker compose pull` + `docker compose up -d --force-recreate
--no-deps --wait` using a compose invocation it reconstructs from **its own
`com.docker.compose.*` labels**. The CP talks to its own updater over a unix socket in a shared
named volume; hosts are reached through `release_apply` over the agent WebSocket, and the agent
relays the updater's result file back as `release_state`. All durable apply state lives in
Postgres; the updater's only state is `.env`, `.env.prev` and one JSON result file per request.
Compose (v2 CLI, labels, `.env`, `restart: unless-stopped`) is load-bearing throughout.

---

## 1. `control-plane/internal/platform/`

### 1.1 Build identity (`buildinfo`)

Confirmed facts
- `version`, `sourceCommit`, `builtAt` are linker-injected (`-ldflags -X`); unstamped reads as `"dev"`
  / null — `control-plane/internal/buildinfo/buildinfo.go:31-41`, `:43-46`.
- `schema_version` is NOT a build flag; it is the highest `NNNN_*.up.sql` in the embedded migration
  FS, computed at init, panics if none — `buildinfo.go:14-18`, `:57-61`, `:65-101`.
- `source_commit` is served only if it is a full 40-hex sha, else null — `buildinfo.go:134-146`.
- The manifest's `schema_version` is computed the same way from the tagged tree at release time —
  `scripts/release/generate-platform-release-manifest.sh:122-138`.

### 1.2 Release sources and the `platform.release_detect` job

Confirmed facts
- `ReleaseSource` interface: `List`, `FetchManifest` (egress-allowlisted), `CompareURL` —
  `control-plane/internal/platform/source.go:12-25`. GitHub Releases is the one implementation
  (`github.go`); edge uses a registry source (`cmd/quasar-control/app.go:977-982`).
- Job id `platform.release_detect` — `detect.go:22-24`. Registered in `app.go:1130-1168`:
  interval 7 days, window 02:00–03:00 UTC Mondays (`app.go:1137-1144`), env override
  `QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL` (`app.go:1145`). Skipped if no repo configured
  (`app.go:1147-1149`). Each pass: `refreshPreflight()` → `Detect` → `Notify` → `autoApplier.Consider`
  (`app.go:1150-1166`).
- Detection is idempotent on `(channel, source_commit)`; a listing failure fails the job and leaves
  rows untouched; an invalid manifest is counted and the release is NOT stored —
  `detect.go:85-145`, `store.go:53-79`.
- The manifest (not the tag) is identity; tag/manifest version and prerelease must agree —
  `detect.go:214-248`. Manifest validation is strict: `format_version` 1, unknown keys invalid,
  exactly two components in normative order `control-plane`, `node-agent`, bare repo refs, `sha256`
  digests, positive `schema_version` — `manifest.go:23`, `:33`, `:64-138`, `:142-158`.
- Edge: the row stores `manifest` NULL and `version` NULL (`detect.go:183-192`); an edge build whose
  schema cannot be read is skipped, not guessed (`detect.go:164-169`). At apply time edge digests are
  resolved from the `sha-<7>` tag and verified against the image's source-commit label —
  `apply_edge.go:11-16`, `:50-56`, `:72-93`.

### 1.3 `PlanRelease` (pure decision)

Confirmed facts
- Pure: all reads passed in via `PlanInputs` — `plan.go:13-71`, entry `plan.go:73-118`.
- Ordering (ADR 0002): `schema_version` DESC, then (beta only) semver precedence, then `built_at`
  DESC, then id — `plan.go:171-264`.
- Downgrade unrepresentable: rows with `schema_version` below the CP are filtered, as are
  versions below an installed prerelease, stable hides prereleases, non-edge rows without a manifest
  are dropped — `plan.go:181-211`, `:288-314`.
- **How a release knows it carries a migration:** `ReleaseRunsAMigration(r, schemaVersion) = r.SchemaVersion > schemaVersion` — `plan.go:155-169`; served as `Release.Migrates`, set only in
  `offerable` — `plan.go:205-207`, `release.go:55-64`.
- CP target eligibility precedence: `no_release` → `identity_unknown` (unstamped) → `up_to_date` →
  `updater_absent` → install mode unknown ⇒ `identity_unknown` / `source` ⇒ `install_mode_source` →
  `preflight_blocked` → `attempt_in_flight` → `run_active` — `plan.go:361-401`.
- Host target precedence: `no_release` → `identity_unknown` → `up_to_date` → `install_mode_source`
  → `updater_absent` → `host_offline` (live registry, not the status column, #169) →
  `release_above_control_plane` → `control_plane_not_first` → `preflight_blocked` →
  `attempt_in_flight` → `run_active` — `plan.go:418-468`.
- Faults (`agent_ahead_of_control_plane`, `identity_unknown`) gate nothing — `plan.go:470-524`,
  `release.go:132-151`.
- Host identity `Known()` requires all four of `source_commit`, `built_at`, `install_mode`,
  `updater_present` — `release.go:109-111`.

### 1.4 Persisted state (tables / migrations)

Confirmed facts
- **0074** — `hosts` gains `source_commit`, `built_at`, `install_mode` (`registry|source`),
  `updater_present` (NULL ≠ false); `platform_releases` table (cache of what exists, never of what
  was applied); `instance_settings.release_channel` / `release_edge_branch` —
  `control-plane/migrations/0074_platform_release_identity.up.sql:19-22`, `:42`, `:67`, `:86-87`.
- **0075** — `platform_apply_runs` (single active run via `platform_apply_runs_active_uk ON ((1))
  WHERE state IN ('pending','running')`) and `platform_apply_attempts` (`requested_digests`,
  `previous_digests`, `updater_request_id`, `state`, `reason`, `sessions_remaining`, `force`,
  `output` ≤ 8192) with `platform_apply_attempts_open_target_uk` (one open attempt per target; CP
  target collapses to the zero uuid) and `platform_apply_attempts_request_uk` —
  `0075_platform_apply.up.sql:13`, `:43`, `:53`, `:70`, `:113`, `:119-120`.
- **0076** — `platform_apply_runs.cordoned_hosts JSONB` (`[{host_id, was_cordoned}]`) —
  `0076_platform_apply_cordons.up.sql:13`.
- **0079 / 0080** — beta channel CHECK widening; release webhook (not update-path critical).
- **0081** — `instance_settings.platform_auto_apply`, `platform_apply_runs.unattended` —
  `0081_platform_auto_apply.up.sql:23`, `:39`.
- **0083** — run state `succeeded_partial`, `retry_of`, persisted `skipped`; attempt kind
  `auto_revert` — `0083_self_update_hardening.up.sql:24-39`.
- **0084** — `cordons_restored_at`, `cordon_restore_attempted_at` (durable "cleanup owed" marker) —
  `0084_platform_apply_cordons_restored.up.sql:18-19`.
- **0088** — `host_admission_restrictions` (owner_kind `platform` etc.), with a backfill from
  unrestored run cordons — `0088_host_admission_restrictions.up.sql:2-4`, `:18`. The production
  runner/fleet wiring now uses owned admission restrictions (`AcquireOwned`/`ReleaseOwned`) rather
  than raw cordon/uncordon — `app.go:993-1005`, `:1061-1077`.
- Single-flight is enforced by the DB indexes, and the store only translates the unique violation
  (never pre-checks) — `apply_store.go:14-33`, `:130-158`; `apply_fleet_store.go:61-76`.
- `updater_request_id` is minted by the DB (`gen_random_uuid()`) and persisted **before** the send
  — `apply_store.go:272-295`.
- Output bounded to 8192 bytes, NUL stripped, valid UTF-8 — `apply_store.go:46-73`.

### 1.5 Per-host apply (`apply_runner.go`)

Confirmed facts
- State machine: `queued` → cordon → `waiting_sessions` (poll live count) → mint request id →
  `release_apply` → relay `release_state` → terminal — `apply_runner.go:11-22`.
- "Success is the new agent's `register`, never a `release_state`" — `apply_runner.go:18-20`;
  implemented in `HandleRegister` (commit prefix match against the release's `source_commit`) —
  `apply_runner.go:656-699`. A late `release_state{succeeded}` is corroboration —
  `apply_runner.go:579-587`.
- "This package must never stop a session"; `force` only skips the wait, the recreate kills them —
  `apply_runner.go:21-22`, `:315-336`.
- Waits for the host's agent to be connected **before** minting (60 s connect wait) so a shutdown
  leaves the row re-drivable — `apply_runner.go:338-361`, `:433-453`.
- Ack timeout ⇒ `unsupported`, host marked unsupported until next `register` —
  `apply_runner.go:394-402`, `:221-234`, `:659-662`.
- Relay trust boundary: a host may only speak about its own attempt; unknown request ids dropped;
  `previous_digests` recorded from every message — `apply_runner.go:539-608`.
- Standalone attempts restore the cordon/restriction they took; fleet attempts leave that to the
  run — `apply_runner.go:278-289`, `:502-537`.
- Routes (admin-gated): `POST /v1/admin/platform/hosts/{id}/apply`, `.../revert`,
  `GET /v1/admin/platform/attempts`, `POST /v1/admin/platform/apply`, runs list/get/cancel —
  `apply_handler.go:92-100`. **There is no standalone CP apply endpoint**; the CP only moves inside a
  fleet run (`apply_fleet.go:338-434`).
- Host apply refusals order: 422 below schema → 409 not offered → edge resolve → eligibility
  (`attempt_in_flight`, `run_active`, `host_not_eligible`) → 501 unsupported → run active → insert —
  `apply_handler.go:129-281`.

### 1.6 Fleet run (`apply_fleet.go`), `prepareFleet`, cordon/drain

Confirmed facts
- Rules: stop at first failed target; skip (not fail) ineligible hosts; cancel read only between
  targets; fleet stays cordoned for the life of the run; every cordon recorded before it is taken —
  `apply_fleet.go:13-40`.
- `drive`: `controlPlanePhase` then `hostPhase`; outcome `succeeded` vs `succeeded_partial` from the
  **persisted** skips — `apply_fleet.go:295-333`, `apply.go:189-196`.
- CP phase: if CP `up_to_date`, go straight to hosts; any other ineligibility fails the run (ADR
  0002) — `apply_fleet.go:351-369`. Unattended + migrating ⇒ refused (re-checked here, unreadable
  release ⇒ treated as migrating) — `apply_fleet.go:370-385`, `:584-595`.
- `prepareFleet`: always cordons the fleet (`cordonFleet`); **non-migrating** ⇒ log held-session
  count and `settleInFlight` (≤ 45 s for non-`running` sessions; expiry proceeds) —
  `apply_fleet.go:460-485`, `:633-672`, `:99-105`. **Migrating** ⇒ count must read successfully and
  reach 0; `force` first stops sessions on every recorded host via `DrainOwned`/`Drain`; unknown
  count never counts as zero; deadline ⇒ `timeout` — `apply_fleet.go:487-561`, `:563-582`,
  `:597-631`.
- Host phase: re-reads the view per host; a host-only run cordons one host at a time
  (`cordonForHostStep`, #200); ErrAttemptInFlight ⇒ undo own cordon step + skip — `apply_fleet.go:
  1138-1235`, `:825-912`, `:914-966`. A re-adopted run waits `DefaultAdoptSettle` (10 s) before the
  first host — `apply_fleet.go:107-111`, `:1151-1167`.
- Every terminal transition goes through `finish`, which defers `settleCordons` —
  `apply_fleet.go:1342-1354`. A boot sweep (`ResumeCordonRestores`, ≤ 20 runs, 60 s budget) retries
  unfinished cleanup, and refuses while a run is active; it must run before `Adopt` —
  `apply_fleet.go:968-1027`, `app.go:1103-1114`.
- `previous` for a new attempt comes from the target's last succeeded attempt, else names with null
  digests ("nobody looked") — `apply_fleet.go:1404-1417`, `apply.go:324-334`.

### 1.7 Control-plane self-apply (`apply_self.go`)

Confirmed facts
- Applied only over the local updater socket, never via an agent — `apply_self.go:22-30`. Socket
  default `/run/quasar-updater/updater.sock`, override `QUASAR_UPDATER_SOCKET` —
  `apply_self.go:32-46`.
- `UpdaterPresent` = the socket file exists (plus a dir/socket three-way for preflight) —
  `apply_self.go:128-146`.
- **Install mode of the CP is learned from its updater's `/v1/self`**, i.e. from `docker compose
  config` output: digest pin or registry host ⇒ `registry`, bare local tag ⇒ `source`; unknown is
  never treated as registry; cached 30 s — `apply_self.go:79-100`, `:296-345`, `:36-38`;
  updater side `internal/updater/exec.go:208-245`.
- Apply: mint request id **before** the socket call; POST `/v1/apply`; record `previous` from the
  202; then poll `/v1/results/{id}` every 2 s until terminal or deadline (15 min from `started_at`) —
  `apply_self.go:382-495`. Normally the process is killed mid-poll by its own recreate.
- `Adopt` on boot: (1) if this binary's commit matches the release commit ⇒ `succeeded` regardless of
  any late result file; (2) else, if a request id exists, keep polling to terminal (this is where an
  auto-restored never-started CP lands as `failed`); (3) no request id ⇒ caller re-drives —
  `apply_self.go:532-576`.

### 1.8 Revert, auto-revert, "restore of a failed agent apply" (ADR 0004)

Confirmed facts
- Revert is per-host only; the CP is never revertible (ADR 0002) — `apply_revert.go:17-22`.
  `PlanRevert` restores the node-agent digest recorded as `previous` on the host's last succeeded
  non-`auto_revert` attempt; refuses if it would be above the CP — `apply_revert.go:62-100`,
  `:305-323`. Reverts honour only durable/transient-inflight reasons — `apply_revert.go:140-152`.
- Revert success when no release names the digest: updater `succeeded`, or a `register` on any
  commit other than the reverted-from one — `apply_revert.go:369-414`.
- **Updater-side automatic restore** (`restoreWorthy`): `never_started` for either component;
  `recreate_failed`/`unhealthy` only when the request does NOT target the CP — `internal/updater/
  exec.go:138-170`. Restore = copy `.env.prev` back + re-run the same `up` command (no pull) —
  `exec.go:181-206`. `pull_failed` restores `.env` only — `exec.go:99-109`.
- CP-side record: when a failed `release_state` carries `restored: true` and the attempt kind is
  `apply`, insert an already-`succeeded` `auto_revert` row with swapped digest sets —
  `apply_runner.go:588-654`, `apply_store.go:160-196`. The run still stops `failed` at that host
  (ADR 0004 consequences, `docs/adr/0004-...md:58-60`).
- If the restore itself fails, no `auto_revert` row; output says "apply the digests in `previous`
  by hand" — `exec.go:148-151`; plan decision 12 (`docs/superpowers/plans/2026-09-11-185-self-update-hardening.md:26`).
- Deadline with no agent back ⇒ `timeout` plus operator prose telling them to read the result on
  the host via `docker compose exec quasar-updater curl --unix-socket ...` —
  `apply_timeout.go:22-49`, `:54-57`, `:62-105`, `:110-138`.

### 1.9 Timeouts (all sources)

| Where | Value | Source |
|---|---|---|
| CP ack timeout | 10 s | `apply_runner.go:28` |
| CP apply deadline (host + CP self) | 15 min from `started_at` | `apply_runner.go:32`, `apply_self.go:285`, `:397` |
| CP poll | 2 s | `apply_runner.go:35` |
| CP connect wait | 60 s | `apply_runner.go:39` |
| In-flight settle (non-migrating CP step) | 45 s, proceeds on expiry | `apply_fleet.go:105` |
| Adopt settle | 10 s | `apply_fleet.go:111` |
| CP install-mode TTL | 30 s | `apply_self.go:38` |
| CP→updater HTTP | 30 s total, 5 s dial, 5 s for `/v1/self` | `apply_self.go:113-126`, `:333` |
| Image resolvable TTL | 10 min | `preflight_collect.go:19` |
| Cordon restore sweep | 20 runs, 60 s | `apply_fleet.go:968-977` |
| Updater discover | 30 s | `cmd/quasar-updater/main.go:60` |
| Updater pull | 3600 s (`QUASAR_UPDATER_PULL_TIMEOUT_S`) | `main.go:113` |
| Updater recreate | 900 s (`QUASAR_UPDATER_RECREATE_TIMEOUT_S`) | `main.go:114` |
| Compose `--wait-timeout` | 300 s default, request may override | `internal/updater/plan.go:275-301` |
| Manifest fetch | 15 s | `signature_source.go:44`, `main.go:117` |
| Agent socket POST / GET | 30 s / 10 s | `node-agent/src/release/mod.rs:273-279`, `:374-380` |
| Agent `updater_unreachable` | 180 s of no result | `release/mod.rs:33-37` |
| Agent poll deadline | 2 h | `release/mod.rs:39-41` |

### 1.10 Crash / restart mid-apply (CP side)

Confirmed facts
- All durable state is in Postgres; `Runner.Close`/`FleetRunner.Close` leave rows non-terminal on
  purpose; boot re-adopts: `applyRunner.Adopt` → `ResumeCordonRestores` → `fleetRunner.Adopt` —
  `apply_runner.go:148-219`, `apply_fleet.go:255-279`, `app.go:1098-1114`.
- A host attempt already sent skips to `watch`; one in `queued`/`waiting_sessions` is re-driven —
  `apply_runner.go:291-298`.
- A shutdown during connect-wait leaves the attempt for the next boot rather than failing it —
  `apply_runner.go:347-356`.
- Terminal standalone attempts whose admission hold was not released are released on boot —
  `apply_runner.go:151-167`, `apply_store.go:232-242`.

### 1.11 Preflight (health / conformance checks before apply)

Confirmed facts
- Pure `PlanPreflight`; `unknown` never blocks — `preflight.go:13-17`, `:102-140`.
- CP checks: `updater_socket` (dir vs socket vs answer), `updater_stack_dir`, `updater_overlays`
  (each service's `config_files` label must equal the updater's own `-f` set), `image_resolvable` —
  `preflight.go:106-113`, `:145-212`. Host checks come from the agent's readiness report
  (`updater_socket`, `updater_stack_dir`, `updater_overlays`, `health_addr_bindable`) plus
  `agent_connected` and `image_resolvable` — `preflight.go:114-123`, `:227-245`;
  agent side `node-agent/src/readiness/platform_update.rs:142-236`.
- `image_resolvable` = registry manifest GET by digest from the CP, no pull — `preflight_collect.go:
  1-19`, `:83-116`.
- The remediation strings name compose commands and `QUASAR_STACK_DIR` in `deploy/.env` —
  `preflight.go:150-157`, `:176-178`, `:205-208`.

### 1.12 Unattended auto-apply (#122)

Confirmed facts
- A trigger on the fleet runner, not a second sequencer; never migrating, never `force`, window =
  detection job schedule, failure suppresses that release only — `auto_apply.go:9-39`,
  `:100-161`; `apply_fleet_store.go:78-144`.

### Inferences / open questions (section 1)
- **[INFERENCE]** There is no standalone "update just the control plane" path; RH-06 must decide
  whether a new deployment owner keeps the fleet run as the only CP entry point.
- **[INFERENCE]** The CP's own install mode depends on the updater being able to run `docker compose
  config` successfully (any `:?` interpolation failure ⇒ `identity_unknown`, target ineligible).
  An install whose stack is not a compose project cannot be classified at all.
- **[OPEN]** Contract wording (`protocol/control-api.md:7367-7373`) says host-apply `force` "stops the
  sessions that are running"; code never stops them for a host attempt (the recreate does) —
  `apply_runner.go:21-22`; only the migrating CP step with force calls `StopHostSessions`
  (`app.go:1074-1076`). Behaviourally equivalent today; worth keeping straight if the recreate is no
  longer compose-driven.

---

## 2. The updater (`internal/updater`, `cmd/quasar-updater`, `deploy/Dockerfile.updater`, compose service)

### 2.1 Packaging and identity

Confirmed facts
- Own image, NOT part of a release, updated by hand (`docker compose pull quasar-updater && ... up -d
  quasar-updater`) — `control-plane/cmd/quasar-updater/main.go:1-19`, `docs/upgrading.md:390-403`.
- Base `docker:29-cli` pinned by tag (brings its own compose plugin, independent of host compose);
  runs as root because it talks to the mounted socket and writes the stack `.env`; adds curl +
  ca-certificates — `deploy/Dockerfile.updater:6-17`, `:36-39`, `:47-56`. Version stamped as the
  source commit — `Dockerfile.updater:30-34`. Image contract asserts docker, compose plugin, curl,
  CA bundle, root — `deploy/image-contract.json:196-237`.
- Published by its own workflow lane and tagged `:${VERSION}`/`latest`/`sha-*`; release notes
  advertise `QUASAR_UPDATER_IMAGE=...quasar-updater:${VERSION}` — `.github/workflows/images.yml:
  1071-1250`, `:1488`. The manifest has only the two components — `docs/upgrading.md:860-863`.

### 2.2 Compose service definition

Confirmed facts (`deploy/docker-compose.yml`)
- `image: ${QUASAR_UPDATER_IMAGE:-quasar-updater:latest}` — `:669-670`.
- Env passthrough: `QUASAR_UPDATER_ALLOWED_NAMESPACES`, `_WAIT_TIMEOUT_S`, `_SIGNATURE_MODE`,
  `_TRUSTED_KEYS`, `_MANIFEST_BASE_URL`, `_MANIFEST_TIMEOUT_S` — `:671-681`.
- Volumes: `${QUASAR_DOCKER_SOCKET:-/var/run/docker.sock}:/var/run/docker.sock` (Podman note);
  **`${QUASAR_STACK_DIR}` bind-mounted at the identical path** (sentinel default
  `/var/lib/quasar/stack-dir-unset`); `quasar-updater-run:/run/quasar-updater` — `:682-694`.
- `security_opt: ["label=disable"]` (SELinux/Podman), `restart: unless-stopped` — `:695-698`.
- The same `quasar-updater-run` named volume is mounted into the CP (`:256-259`) and the agent
  (`:622-625`); declared at `:714`.
- Enrolled hosts get a generated compose with only `quasar-node-agent` + `quasar-updater`, project
  `quasar-agent` by default — `deploy/enroll-host.sh:47-56`, `:88-95`, `:296-313`.

### 2.3 Self-discovery (how it finds the stack)

Confirmed facts
- Reads its **own** container's labels `com.docker.compose.project`, `.project.working_dir`,
  `.project.config_files` via `docker inspect` over the socket — `internal/updater/discover.go:18-22`,
  `:25-59`.
- Own container id from `/proc/self/mountinfo` (`/etc/hosts|hostname|resolv.conf` bind sources
  under `.../containers/<64hex>`), `$HOSTNAME` only if it looks like an id — `discover.go:76-116`.
- **Fail closed**: no labels ⇒ exit ("a bare `docker run` cannot be discovered"); working dir or any
  config file not visible at the same absolute path inside the container ⇒ exit naming
  `QUASAR_STACK_DIR` — `discover.go:53-72`; `main.go:55-65`. `restart: unless-stopped` then
  crash-loops it, which the CP reads as `updater_absent`/preflight fail (`main.go:67-69`).
- `.env` path is `WorkingDir/.env` — `internal/updater/exec.go:428-430`; `main.go:112`.

### 2.4 Unix-socket API (NOT frozen)

Confirmed facts
- Routes: `GET /v1/healthz`, `GET /v1/self`, `POST /v1/apply`, `GET /v1/results/{request_id}` —
  `internal/updater/server.go:64-75`.
- Socket mode 0666, stale socket removed on start — `server.go:293-315`. Authorization is "the
  request, never the caller" (namespace allowlist + digest rules) — `server.go:23-26`.
- Declared non-frozen: `protocol/schema.md:2193-2206`; `internal/updater/plan.go:6-9`.
- `/v1/self`: version, project, working_dir, config_files, env_path, allowed_namespaces,
  components, wait_timeout_s, in_flight, signature mode/key ids/manifest source, **effective images
  per component** (from `compose config`), **per-service config_files** (from container labels);
  cached 15 s — `server.go:77-147`, `exec.go:208-285`.
- `POST /v1/apply`: body ≤ 64 KiB, `DisallowUnknownFields`; idempotent re-post of an accepted id;
  reads `.env`; gathers signature evidence; `Plan`; `Store.Claim` (authoritative single-flight);
  **202 then executes detached with `context.Background()`** — `server.go:172-233`.
- Status codes: `busy` 409, namespace/digest/signature 422, else 400 — `server.go:157-170`.
- Request shape: `{request_id(uuid), components[{name,image,digest}], release{id,version,source_commit}, wait_timeout_s?}` — `plan.go:68-89`.

### 2.5 `Plan()` — accept/reject + env rewrite + commands (pure)

Confirmed facts
- Closed component table: `control-plane → service quasar-control-plane, env QUASAR_CONTROL_IMAGE`;
  `node-agent → service quasar-node-agent, env QUASAR_AGENT_IMAGE (alias QUASAR_NODE_IMAGE read,
  never written)`; the updater can never target itself — `plan.go:49-63`, `:224-229`.
- Rejections: non-uuid id; `busy` if another id in flight (refuse, never queue); empty components;
  undiscovered project; unknown/duplicate component; image with tag/digest; bad digest
  (`digest_malformed`); namespace outside allowlist on a path-segment boundary
  (`namespace_rejected`); signature gate last — `plan.go:203-256`, `:180-201`.
- Default allowlist `ghcr.io/accreleus/quasar`; blank ⇒ default, never empty — `plan.go:158-178`.
- Env rewrite: sets `VAR=image@digest`, replacing **every** definition or appending; byte-preserving
  (comments, order, CRLF) — `plan.go:258-273`; `internal/updater/env.go:5-9`, `:61-96`. `previous`
  digest parsed from the prior value; a local tag ⇒ null — `plan.go:314-326`.
- Commands (both via `docker`): `compose -p <project> --project-directory <working_dir> -f <each
  config file> pull <services>` and `... up -d --force-recreate --no-deps --wait --wait-timeout <N>
  <services>` — `plan.go:283-312`.

### 2.6 Executor — what it runs, reads, writes

Confirmed facts (`internal/updater/exec.go`)
- Writes `.env.prev` (verbatim prior `.env`, mode 0600) **first**, then the rewritten `.env` —
  `:67-68`, `:86-95`.
- `pulling` → run pull (bound `PullTimeout`); failure ⇒ restore `.env` only, `pull_failed` —
  `:97-109`.
- `recreating` → run `up` (bound `RecreateTimeout`); `verifying` → judge from post-state, not the
  exit code: `compose ps -a --format json <services>`; running + (healthy or no healthcheck) passes;
  `State.StartedAt` zero ⇒ `never_started`; else `recreate_failed`/`unhealthy`; a zero exit is not
  trusted over the stack and vice versa — `:111-124`, `:287-344`, `:360-371`.
- On failure, appends compose output, one detail line, last 40 log lines of the failed container —
  `:126-136`, `:346-358`; then `restoreWorthy` restore (see 1.8).
- Other docker calls: `inspect` (self labels, StartedAt), `logs --tail 40`, `compose config --format
  json`, `ps --filter label=com.docker.compose.project=<p>` with `com.docker.compose.service` and
  `config_files` labels — `discover.go:32-35`, `exec.go:223`, `:258-259`, `:353`, `:364`.
- `CLI.Run` executes `docker` as a **child process** of the updater (`exec.CommandContext`) —
  `exec.go:31-51`.

### 2.7 Result files, journal, recovery

Confirmed facts
- One file per request id under `/run/quasar-updater/results/<uuid>.json`, written tmp + fsync +
  rename, 0644, dir 0755 root-owned (CP uid 1000 reads only) — `internal/updater/result.go:14-25`,
  `:74-83`, `:131-186`; compose comment `deploy/docker-compose.yml:256-258`.
- Result carries `state, reason, components, previous, output (≤8192, tail), started/updated/finished,
  restored, release, commands` — `result.go:27-47`.
- The single-flight latch and the "accepted" cache are **in-memory only** (`Store.inflight`,
  `Store.accepted`) — `result.go:53-62`, `:92-127`. `NewStore` does not scan existing results —
  `result.go:74-83`; `main.go` has no recovery step — `main.go:103-151`.
- Nothing prunes the results directory — stated at `node-agent/src/release/mod.rs:154-158`.
- `save` failures are logged only — `exec.go:414-418`.

### 2.8 Signature gate (off by default)

Confirmed facts
- `QUASAR_UPDATER_SIGNATURE_MODE` = `off|verify|require`; typo ⇒ fatal — `signature.go:22-32`,
  `:322-336`. Updater fetches manifest + `.sig` itself over HTTPS from a host-local base URL
  (`{version}` substituted) — `signature.go:50-58`, `:385-410`; `signature_source.go:58-106`,
  `:111-124`. `verify` is bypassable by a request naming no version; `require` is the boundary —
  `signature_source.go:19-34`, `signature.go:172-186`. `bindManifest` ties the signed manifest to the
  request's digests — `signature.go:272-304`.

### Inferences / open questions (section 2)
- **[INFERENCE, high confidence] No crash recovery in the updater.** If the updater container is
  stopped/recreated/killed mid-apply, its `docker compose` child process dies with it (child of the
  updater process, `exec.go:36-51`, and container stop kills the PID namespace). On restart the latch
  is empty and no result is re-examined (`result.go:74-83`), so: the result file stays at
  `pulling`/`recreating`/`verifying` forever; `.env` may already name the new digest while the old
  container still runs (`exec.go:86-95` writes `.env` before pulling); a new apply would be accepted
  and would overwrite `.env.prev` with the already-rewritten `.env`, losing the original prior pin.
  This contradicts `docs/upgrading.md:401-403` ("an apply in flight is a detached `docker compose`
  invocation that finishes regardless"). The CP side times the attempt out at 15 min; the agent
  poller keeps re-reading the stale non-terminal file (so `last_seen` refreshes and
  `updater_unreachable` never fires) until its 2 h poll deadline (`release/mod.rs:329-363`).
  **Should be verified by test before RH-06 relies on either statement.**
- **[INFERENCE]** `.env` is rewritten with `os.WriteFile` (truncate+write, no fsync, no rename) —
  `exec.go:88-95` — so a power loss mid-write can leave a truncated operator-owned `.env` (which also
  holds DB password / enrollment token / secret key). Result files are atomic; `.env` is not.
- **[INFERENCE]** Restore re-runs `up` without a pull (`exec.go:196-206`); it relies on the previous
  image still being in the local image store. An image prune between apply and restore would make
  the restore fail.
- **[INFERENCE]** Because `up` re-reads the current compose files, a recreate applies ANY drift in
  that service's compose definition since it was created, not just the new image. Preflight
  `updater_overlays` only compares the `-f` list, not file contents (`preflight.go:186-212`).
- **[INFERENCE]** Compose `--env-file`, `env_file:` for interpolation, or exported shell variables
  are invisible to the updater: it assumes `WorkingDir/.env` (`exec.go:430`) and runs compose with
  its own container environment. No code reads the `com.docker.compose.project.environment_file`
  label (grep: no hits). An exported `QUASAR_CONTROL_IMAGE` in an operator's shell would silently
  override the updater's pin at the operator's next manual `up` (see `deploy/redeploy.sh:304-312` for
  the same precedence trap documented for secrets).
- **[OPEN]** No `/v1/*` endpoint cancels or aborts an in-flight apply; no retention or GC for result
  files; updater version skew across hosts is not tracked by the CP (only shown in CP preflight
  detail, `preflight.go:164-168`).

---

## 3. Where the update path couples to Compose / `.env` / labels / paths / socket / restart policy

Confirmed facts (grep of `com.docker.compose`, `.env`, `COMPOSE_PROJECT`, `working_dir`,
`redeploy`, manager names)
- `com.docker.compose.*` is read in exactly three production places: updater self-discovery
  (`internal/updater/discover.go:18-22`), updater per-service config-files probe
  (`exec.go:258-259`), agent install discovery (`node-agent/src/buildinfo.rs:101-106`, `:249-266`);
  plus `deploy/redeploy.sh:412-413`, `:507-508`, `:625-626` (find the postgres container) and a
  harness (`scripts/harness/run-readiness-faults.sh:797-801`).
- The agent decides `updater_present` by finding a RUNNING container with service label
  `quasar-updater` in its own compose project; no project label ⇒ unknown — `buildinfo.rs:101-106`,
  `:215-269`; re-discovered on every connect — `node-agent/src/agent.rs:1864-1872`. Its
  `install_mode` comes from its own container's configured image reference (not compose) —
  `buildinfo.rs:188-213`, `:235-247`.
- The CP's own install mode comes from `docker compose config` run by the updater —
  `exec.go:208-245`, `apply_self.go:296-315`.
- `.env` is the pin store: `QUASAR_CONTROL_IMAGE`, `QUASAR_AGENT_IMAGE` (`QUASAR_NODE_IMAGE` alias)
  interpolated into `image:` — `deploy/docker-compose.yml:13-18`, `:77`, `:288`; rewritten by
  `plan.go:258-273`. `enroll-host.sh` also owns `QUASAR_AGENT_IMAGE`, `QUASAR_UPDATER_IMAGE`,
  `QUASAR_STACK_DIR`, `COMPOSE_FILE`, `COMPOSE_PROJECT_NAME` as "managed" keys it replaces on re-run —
  `deploy/enroll-host.sh:736-763`.
- `QUASAR_STACK_DIR` must be the host path, seeded by `deploy/redeploy.sh:564-593` and
  `deploy/enroll-host.sh:755-758`; documented `docs/configuration.md:1236`.
- Restart policy `unless-stopped` is relied upon beyond the updater: the agent's `Restart` command
  acks then `exit(0)` "so the container restart policy restarts us" — `node-agent/src/messages.rs:
  1157-1158`, `node-agent/src/agent.rs:2381-2393`; the RH05 policy journal also exits for
  reconciliation/startup verification — `agent.rs:2409-2420`.
- The agent's own docker socket mount is hard-coded `/var/run/docker.sock` (not
  `QUASAR_DOCKER_SOCKET`) — `deploy/docker-compose.yml:568`.
- UI/doc recipes hard-code compose commands and `deploy/.env` paths —
  `web/src/lib/platform/manualUpdate.ts:44-70`, `:122-155`; `apply_timeout.go:54-57`;
  `preflight.go:150-157`; `docs/upgrading.md:195-203`, `:338-360`, `:396-399`, `:513-517`, `:540-544`.
- **No manager-specific code exists.** Searches for Portainer, Dockge, Unraid templates, TrueNAS,
  CasaOS, Synology, Watchtower find no update-path code; Unraid appears only as host-tuning /
  storage notes and in live-evidence reports (e.g. `deploy/host-tuning.md:28`,
  `deploy/redeploy.sh:604`, `:652`). No Unraid XML template is in the repo.

### Inferences / open questions (section 3)
- **[INFERENCE]** A stack managed by a tool that does NOT use the Compose v2 CLI (e.g. an Unraid
  Docker template / dockerman container) carries no compose labels ⇒ the updater exits at discovery
  and the agent reports `updater_present` unknown ⇒ every target is ineligible. A Compose-based
  manager (Portainer/Dockge "stacks") does set labels, but its working dir and config-file paths are
  the manager's internal paths; the updater would need those mounted at identical paths, and it
  would then edit the manager's `.env` behind the manager's back (the manager may later rewrite it
  from its own stored copy). Not tested anywhere in the repo.
- **[INFERENCE]** Two deploy scripts (`redeploy.sh` for source/first host, `enroll-host.sh` for
  enrolled hosts) plus the updater all write `.env`; there is no single owner. Re-running
  `enroll-host.sh` would replace an updater-applied `QUASAR_AGENT_IMAGE` pin with the script's ref.
- **[INFERENCE]** `deploy/redeploy.sh:884-887` says the agent discovers updater presence "once, at
  boot"; `agent.rs:1864-1866` re-discovers per connection. Comment drift.

---

## 4. ADRs relevant to updates

Confirmed facts
- **ADR 0001 — trust is a pinned digest.** Registry TLS + sha256 digest from the manifest, pinned
  end to end; a tag is never an identity; signatures deferred —
  `docs/adr/0001-platform-release-trust-is-a-pinned-digest.md:5-27`.
- **ADR 0002 — CP first, no downgrade.** CP migrates forward on boot and cannot start against a DB
  ahead of it; always update CP first, never offer older than the installed CP; agents may lag but
  never lead; "Revert" exists only for agents — `docs/adr/0002-release-order-and-no-downgrade.md:
  7-28`.
- **ADR 0003 — detached ed25519 signature over the manifest, verified by the updater, off by
  default.** Updater fetches assets itself; `verify` is a migration rung not a boundary; `require`
  refuses unsigned (including edge and unnamed reverts); key rotation via lists; new reasons
  `signature_missing`/`signature_invalid` not yet in `openapi.yaml` —
  `docs/adr/0003-release-signatures.md:5-118`.
- **ADR 0004 — automatic restore of a failed agent apply.** Node-agent `never_started`,
  `recreate_failed`, `unhealthy` ⇒ updater restores `.env.prev` and previous digest, captures log
  tail, reports `restored: true`; one restore, no retry; CP inserts `auto_revert` row; run still
  stops; CP rule unchanged (only never-started CP restored); restored agent adopts the in-flight
  result — `docs/adr/0004-automatic-restore-of-a-failed-agent-apply.md:21-69`.
- **ADR 0005 — only evidence gates a launch** (readiness; not update-path, but explains why
  readiness-derived preflight "unknown" never blocks) — `docs/adr/0005-only-evidence-gates-a-launch.md:7-38`.
- **ADR 0006 — unstarted idle-apply approvals expire on every CP boot**, which also protects a
  "supported stopped-stack database restore" from replaying an old approval; started attempts are
  recovered from the agent's durable journal —
  `docs/adr/0006-expire-unstarted-idle-approvals-on-control-plane-boot.md:7-32`.

---

## 5. `docs/upgrading.md` (current operator procedure)

Confirmed facts
- Headline rules: back up Postgres; never run an older CP than the DB — `docs/upgrading.md:8-10`.
- Image rename note, `docker-compose.release.yml` retired, pins now two `.env` vars —
  `:26-60`; source→registry switch needs TLS volume recreated or chowned — `:62-89`, `:293-322`.
- **Backup:** manual `pg_dump --format=custom` via `docker compose exec quasar-postgres`; restore
  with `pg_restore --clean` into a stopped stack; drill script `deploy/db-backup-restore-drill.sh` —
  `:93-142`.
- **Normal (source) upgrade:** `deploy/redeploy.sh <va|nvidia> <ref>` (or `... control`,
  `make redeploy-cp`); health wait = migration wait — `:146-179`.
- **Registry upgrade (manual):** set the two digests in `deploy/.env`, then `docker compose -f
  deploy/docker-compose.yml pull ...` and `up -d --force-recreate --no-deps ...`; "repeat every
  `-f`"; agent recreate kills sessions — `:183-221`.
- **One-way migration rule** and the fix (redeploy forward, don't hand-edit `schema_migrations`;
  restore backup only to abandon the upgrade) — `:225-289`.
- **The updater:** add `QUASAR_STACK_DIR` (+ image) to `.env`, `up -d quasar-updater
  quasar-control-plane quasar-node-agent`, verify with `curl --unix-socket .../v1/self` — `:326-373`;
  health port (#152) — `:375-388`; updating the updater by hand — `:390-403`; what an apply costs —
  `:405-418`.
- Channels stable/beta/edge; switching back never rolls back — `:420-484`.
- Console per-host apply, force, success = re-register, manual restore via result read, `timeout`
  diagnosis — `:486-552`.
- "Update Quasar" fleet run, migrating vs non-migrating drain, ~20 s console outage, stop at first
  failure, `succeeded_partial` + Retry, source-built CP not offered, preflight, cancel semantics —
  `:554-663`.
- Reverting an agent; CP never revertible — `:665-689`.
- Automatic updates — `:693-737`; release notifications — `:739-843`.
- **Cutting a release:** `make release VERSION=x.y.z` on clean `main`; tag push triggers images +
  GitHub Release + manifest; updater image published separately — `:847-886`.
- Signing: publishing and verifying halves, `verify` → `require`, rotation — `:890-1040`.

### Inferences / open questions (section 5)
- **[INFERENCE]** `:219-221` still says the updater does this "once the apply half ships" — stale; the
  apply half shipped (#116/#117).
- **[INFERENCE]** `:401-403` "Safe at any time ... finishes regardless" is at odds with the code (see
  §2 inferences).
- **[INFERENCE]** Every recipe assumes `docker compose -f deploy/docker-compose.yml` from a git
  checkout layout; enrolled hosts use `/opt/quasar-agent`-style dirs with `COMPOSE_FILE` in `.env`
  (`deploy/enroll-host.sh:43`, `:750-751`), so the doc's literal `-f deploy/...` paths do not apply
  there.

---

## 6. Database migration safety

Confirmed facts
- Boot order: DB preflight (parse/reach/auth) → `migrate.Run` (golang-migrate `m.Up()`, table
  `schema_migrations`) → pool → services — `control-plane/cmd/quasar-control/main.go:60-86`;
  `control-plane/internal/migrate/migrate.go:15-50`.
- "Binary older than DB" error is translated into cause + fix text; a dirty-DB error is passed
  through untranslated — `internal/migrate/rollback.go:18-52`; `rollback_test.go:66`.
- Schema version recorded by golang-migrate in `schema_migrations`; the binary's embedded max is
  `buildinfo.SchemaVersion()`; the release's is `manifest.schema_version` —
  `buildinfo.go:57-104`, `generate-platform-release-manifest.sh:122-138`.
- A release "carries a migration" iff `release.schema_version > CP schema_version` —
  `plan.go:155-169`. Unreadable release ⇒ treated as migrating — `apply_fleet.go:584-595`.
- Migrating CP step drains the fleet (and `force` stops sessions) before the recreate; the rationale
  is migrations authored assuming no live session (0027 example) — `apply_fleet.go:440-459`;
  `protocol/control-api.md:7308-7335`. Unattended never migrates — `auto_apply.go:20-27`,
  `:120-126`.
- The CP is auto-restored only if its new container never started (no migration can have run); a
  started CP is never restored and never revertible — `exec.go:138-170`; ADR 0002/0004.
- **No automatic backup anywhere in the update path.** `pg_dump` appears only in
  `deploy/db-backup-restore-drill.sh` and one DB test (grep). Neither the updater, the CP apply
  machines, nor `deploy/redeploy.sh` take a backup.
- Documented restore: manual `pg_restore --exit-on-error --clean --if-exists` into a stopped stack —
  `docs/upgrading.md:125-132`; production guidance: pause writes, dump, checksum, copy off-host,
  restore into an isolated instance and run the candidate image there; "Never restore over a live
  database" — `docs/operations/database-backup-restore.md:26-31`. The upgrade-from-last-supported
  rehearsal is SKIP/blocked for lack of a supported-pair manifest — `:33-41`.
- ADR 0006: a stopped-stack DB restore is a supported case for idle-apply approvals; restore under a
  running CP is not a supported guarantee — `docs/adr/0006-...md:10-13`, `:26-29`.

### Inferences / open questions (section 6)
- **[INFERENCE]** A migration that fails mid-way leaves `schema_migrations` dirty; the new CP exits,
  `restart: unless-stopped` loops it, the updater sees it started ⇒ `unhealthy`/`recreate_failed`, no
  restore, and the old binary cannot start either if any migration committed. The only recovery is
  manual (fix + `migrate force`, or restore the pre-upgrade dump) — and nothing took that dump.
- **[INFERENCE]** A long migration that exceeds `--wait-timeout` (300 s default) is reported
  `unhealthy` even if it would have succeeded; the CP container keeps running (restart policy) and
  may come healthy later, while the attempt is already `failed`. On the next boot, `Adopt` resolves
  by commit match (`apply_self.go:546-557`) — but only if the attempt is still non-terminal; the
  updater's `failed` result may have been recorded first. **[OPEN]** which wins in practice.
- **[OPEN]** RH-06 should decide whether backup-before-migrate becomes part of the apply path (the
  updater has the socket and could `exec` into `quasar-postgres`; the CP knows `Migrates`).

---

## 7. Node-agent self-update boundary

Confirmed facts
- The agent never recreates itself and runs no compose command; it validates, acks acceptance,
  POSTs to the local socket, and relays the result file — `node-agent/src/release/mod.rs:1-10`,
  `node-agent/src/agent.rs:4376-4387`; contract `protocol/agent-api.md:1267-1273`, `:1901-1990`.
- Only `node-agent` is appliable by an agent; `control-plane` in a request ⇒ `invalid` —
  `release/mod.rs:43-46`, `:541-560`.
- Ack: `updater_absent` if socket missing or POST fails; `busy` if another id in flight; updater's
  rejection reason relayed verbatim — `release/mod.rs:218-311`.
- The relay poller is a std thread that outlives the connection; emits on state change; 180 s of no
  result ⇒ `updater_unreachable`; 2 h hard stop — `release/mod.rs:320-368`.
- On every connect the agent re-emits (terminal) or **adopts** (non-terminal) result files for its
  own component; results older than 2 h or for other components are skipped —
  `release/mod.rs:142-203`, `:476-496`; wired at `agent.rs:1897-1902`.
- CP instructs a host only via `release_apply` over the agent WebSocket (`agentRegistry.SendReleaseApply`) —
  `cmd/quasar-control/app.go:1006-1025`; `release_state` adapted back — `app.go:273-290`, `:1028`.
- Success evidence is the NEW agent's `register` carrying the release's `source_commit` —
  `protocol/agent-api.md:1279-1288`; `apply_runner.go:656-699`.
- Recreating the agent kills every session on that host; siblings are removed by the new agent's
  startup sweep — `protocol/agent-api.md:1290-1296`; `docs/upgrading.md:405-411`.
- **Session survival across a CP restart (#128/#153):** CP reaps only non-`running` rows on
  disconnect/reconnect (`control-plane/internal/session/store.go:817-845`) and a stale-host sweep
  measured from `max(last_heartbeat, boot)` with `QUASAR_SESSION_GRACE_SECS` (120 s) is the
  backstop (`internal/session/stale_sweep.go:8-45`, `internal/config/config.go:66`, `:304`); the
  agent holds running sessions for 90 s and reconnects every ≤ 5 s while holding
  (`node-agent/src/agent.rs:2833-2867`, token `sessions-held-for-grace` at `:493`). Live evidence:
  1080p60 held through a 73 s CP outage (`docs/upgrading.md:579-584`) and through an unattended
  non-migrating apply (`docs/superpowers/plans/2026-09-12-173-live-evidence.md:32-44`).
- Drain/cordon is the CP's job only (owned admission restrictions) — `apply_runner.go:247-289`,
  `apply_fleet.go:674-809`; the agent "never checks, waits, or refuses" on sessions —
  `protocol/agent-api.md:1294-1296`.
- The agent also exits to be restarted by the container restart policy for `Restart` and for RH05
  restart-scope config application — `agent.rs:2381-2420`; `node-agent/src/policy.rs:274-296`,
  `:884-900`.

### Inferences / open questions (section 7)
- **[INFERENCE]** The agent's update role is purely relay + identity reporting; moving update
  ownership away from compose changes the agent only in `buildinfo.rs` discovery (compose labels)
  and the readiness checks that read the updater's `/v1/self` (`readiness/platform_update.rs`).
- **[INFERENCE]** Any new owner must preserve "restart on exit" semantics or the `Restart`
  command and RH05 restart-scope policy application break (process exits and nothing brings it back).

---

## 8. Plans — what shipped, what remains

Confirmed facts
- **#128 sessions survive CP restart** (`docs/superpowers/plans/2026-09-08-128-sessions-survive-control-plane-restart.md`):
  three phases (CP reconcile instead of reap, agent bounded grace, web mint retry) — `:5-9`. The
  checkboxes are unticked in the file, but the code shipped: `ReapHostExceptRunning`
  (`session/store.go:832`), `sweepStaleHosts` (`session/stale_sweep.go:30`), agent grace
  (`agent.rs:2841-2867`), config knob (`config.go:304`), `docs/configuration.md:69`, and the
  CHANGELOG entry (`CHANGELOG.md:873`); live gate recorded in `docs/upgrading.md:579-584` and the
  173 record.
- **#185 self-update hardening** (`docs/superpowers/plans/2026-09-11-185-self-update-hardening.md`):
  preflight (#187), updater automatic agent restore (#188, ADR 0004), readiness conformance (#189),
  partial outcome + retry (#190), socket three-way (#184); amendment 9; migration 0083 — `:5-45`.
  Phase H landed: develop `2196ab2` (2026-09-12), amendment 9 on protocol `main` `8a6aed2`; #182
  followed with 0084 — `:407-412`. Live gate on a registry appliance stack recorded, including the
  defect that a restored agent left the attempt `verifying` (fixed by adoption) — `:413-437`
  (file lines 413+). Phases A–G checkboxes are unticked, but their code is present (preflight.go,
  `restoreWorthy`, `auto_revert`, `succeeded_partial`, `retry_of`).
- **#173 live evidence** (`docs/superpowers/plans/2026-09-12-173-live-evidence.md`): PASS for
  unattended refusal of a migrating release, admin cordon preserved, session streams through an
  unattended non-migrating apply, skip-then-continue, failure with another host offline; deviations
  explained by #201 (timeout when no agent relays) and #200 (no fleet cordon when CP is current) —
  `:16-85`. Notes: updater stays on its pinned tag across applies; two-host gates needed a
  temporarily enrolled aux-infra host — `:50-56`, `:58-88`.

### Remaining / not done (from sources)
- Signature reasons not in `openapi.yaml` enum (sign-off needed) — ADR 0003 `:113-118`,
  `apply.go:77-84`.
- Version-upgrade rehearsal blocked for lack of a supported release manifest pair —
  `docs/operations/database-backup-restore.md:33-41`.
- #183: a cordon carries no owner, so a post-run admin cordon can be lifted by the restore sweep —
  `apply_fleet.go:994-996` (partly superseded by owned admission restrictions, 0088).
- **[INFERENCE]** Items not covered by any plan: updater crash recovery/journal, updater
  self-update/version skew, results GC, automatic pre-migration backup, non-compose owners.

---

## Coupling inventory

Every place the update path depends on Compose files, `.env`, compose labels, host paths, the
docker socket, restart policy, or manager-specific behaviour.

| # | file:line | Depends on | Why it matters for RH-06 |
|---|---|---|---|
| 1 | `control-plane/internal/updater/discover.go:18-22`, `:25-59` | `com.docker.compose.project`, `.project.working_dir`, `.project.config_files` labels on the updater's own container | Updater only works when started by the Compose v2 CLI as a service of the stack it manages; fails closed otherwise. Any other owner (template, orchestrator) must either set these labels or replace discovery. |
| 2 | `discover.go:61-72`; `deploy/docker-compose.yml:685-693`; `docs/configuration.md:1236` | Stack dir + every `-f` file visible at the **same absolute host path** inside the updater (`QUASAR_STACK_DIR` bind) | Host-path identity mount; breaks under managers that keep compose files in internal paths or remote contexts. |
| 3 | `discover.go:76-116`; `node-agent/src/buildinfo.rs:131-138` | `/proc/self/mountinfo` shape of Docker's `/etc/hosts` bind under `.../containers/<id>` | Self-identification is Docker-layout specific (Podman noted as supported via socket; layout not verified here). |
| 4 | `internal/updater/plan.go:303-312`, `:283-296` | `docker compose -p P --project-directory D -f ... pull` / `up -d --force-recreate --no-deps --wait --wait-timeout N` | The entire recreate semantic (health wait, no deps, force recreate) is Compose's. |
| 5 | `internal/updater/plan.go:49-63` | Fixed service names `quasar-control-plane`, `quasar-node-agent`; env vars `QUASAR_CONTROL_IMAGE`, `QUASAR_AGENT_IMAGE` (alias `QUASAR_NODE_IMAGE`) | Renaming services or moving pins out of `.env` breaks the updater. |
| 6 | `internal/updater/exec.go:428-430`; `server.go:196-202` | `.env` at `<working_dir>/.env` | Ignores `--env-file`, `env_file`, shell exports; assumes the Compose default lookup. |
| 7 | `internal/updater/env.go:5-9`, `:61-96`; `plan.go:258-273` | Byte-preserving rewrite of `NAME=value` lines (Compose dialect, no `export`) | The `.env` is the durable pin store and is co-owned by operator, `redeploy.sh`, `enroll-host.sh` and updater. |
| 8 | `exec.go:67-68`, `:86-95`, `:181-193` | `.env.prev` beside `.env`, non-atomic `os.WriteFile` | Only rollback record on the host; no journal; lost/overwritten on a crashed apply followed by another apply [INFERENCE]. |
| 9 | `exec.go:287-344`, `:360-371`, `:373-395` | `docker compose ps -a --format json` (array or NDJSON), `docker inspect .State.StartedAt`, `docker logs` | Health verdict and never-started detection are Docker/Compose-state derived. |
| 10 | `exec.go:208-245`; `apply_self.go:79-100`, `:296-315` | `docker compose config --format json` effective image | CP install-mode classification (registry vs source) depends on Compose rendering the whole project. |
| 11 | `exec.go:247-285`; `preflight.go:186-212`; `node-agent/src/readiness/platform_update.rs:193-217` | `docker ps --filter label=com.docker.compose.project=P` + `com.docker.compose.service` + `config_files` labels | `updater_overlays` preflight check is a compose-overlay drift detector. |
| 12 | `node-agent/src/buildinfo.rs:101-106`, `:249-266`; `agent.rs:1864-1872` | Agent's own `com.docker.compose.project` label and a running service labelled `quasar-updater` | `updater_present` (eligibility input) is only answerable inside a compose project. |
| 13 | `deploy/docker-compose.yml:684`; `enroll-host.sh:306`; `docs/configuration.md:1237` | Docker (or Podman) socket mounted into the updater (`QUASAR_DOCKER_SOCKET`) | Root-equivalent capability; the updater is the socket proxy. |
| 14 | `deploy/docker-compose.yml:568` | Agent's own hard-coded `/var/run/docker.sock` | Agent uses the socket for sessions/identity (not for self-update), but Podman/alt sockets need an overlay here. |
| 15 | `deploy/docker-compose.yml:256-259`, `:622-625`, `:694`, `:714`; `apply_self.go:32-46`; `release/mod.rs:26-27` | Shared named volume `quasar-updater-run` mounted at `/run/quasar-updater` in CP, agent and updater | The only CP↔updater and agent↔updater channel; a container created before the volume existed lacks it (preflight names the recreate). |
| 16 | `server.go:293-315`; `result.go:74-83`; compose `:256-258` | Socket 0666, results dir 0755 root-owned; CP uid 1000 reads only | Permission model assumes one host's containers share the volume. |
| 17 | `deploy/docker-compose.yml:65`, `:275`, `:658`, `:698` | `restart: unless-stopped` on every service | Updater fail-closed relies on it to retry; the CP's crash-loop behaviour after a bad migration; the agent's `Restart` and RH05 restart-scope exits (`agent.rs:2381-2420`, `messages.rs:1157-1158`) rely on it to come back. |
| 18 | `deploy/docker-compose.yml:260-275`, `:655-657` | CP compose healthcheck (curl `/health`), agent `depends_on: service_healthy`; agent image HEALTHCHECK (`docs/upgrading.md:386-387`) | `--wait` verdict and `verify` treat "no healthcheck" as healthy; health definitions live in compose/image. |
| 19 | `deploy/docker-compose.yml:13-18`, `:77`, `:288`, `:670` | `image:` interpolated from `.env` with local-tag defaults | Default `:latest` local tags are what makes a stack classify as `source`; the updater image is a tag, not a digest. |
| 20 | `deploy/Dockerfile.updater:6-10`, `:36-39` | Compose plugin version pinned by the `docker:29-cli` base | Behaviour (`--wait`, JSON shapes) tied to that CLI version. |
| 21 | `deploy/redeploy.sh:133-204`, `:352-354`, `:564-593`, `:878-895`, `:964-990` | Builds `-f` chain, derives `COMPOSE_PROJECT_NAME` (default `deploy`), seeds `QUASAR_STACK_DIR`, brings updater up before agent, probes `/v1/self` | Source/first-host deployment owner; separate from the updater path. |
| 22 | `deploy/enroll-host.sh:47-56`, `:88-95`, `:181-313`, `:736-763` | Generated compose (agent + updater), project `quasar-agent`, `.env` managed keys incl. `QUASAR_AGENT_IMAGE`, `COMPOSE_FILE`, `COMPOSE_PROJECT_NAME` | Enrolled-host deployment owner; volume names (identity) are scoped by compose project name; re-run overwrites image pins. |
| 23 | `deploy/redeploy.sh:412-413`, `:507-508`, `:625-626` | `docker ps --filter label=com.docker.compose.project=… service=quasar-postgres` | Finding the DB container for secret/volume checks is label-based. |
| 24 | `docs/upgrading.md:310-316`, `deploy/redeploy.sh:865` | Volume names `<compose project>_quasar-control-tls`, `_quasar-postgres-data` | Compose volume naming is part of the upgrade/TLS-ownership procedure. |
| 25 | `apply_timeout.go:54-57`; `preflight.go:150-157`, `:176-178`, `:205-208`; `platform_update.rs:147-148`, `:161`, `:179`, `:236` | Operator remediation text: `docker compose exec/up/logs quasar-updater`, `QUASAR_STACK_DIR in deploy/.env` | User-facing copy assumes Compose CLI and `deploy/.env`. |
| 26 | `web/src/lib/platform/manualUpdate.ts:44-70`, `:122-155`, `:185-199` | Manual recipes: `docker compose -f deploy/docker-compose.yml`, `.env` pins, `deploy/redeploy.sh` | Console-rendered manual path; doc-parity test greps `docs/upgrading.md`. |
| 27 | `web/src/pages/admin/fleet/HostsTab.tsx:363` | `docker compose --project-directory` in host UI copy | Same. |
| 28 | `docs/upgrading.md:195-221`, `:338-360`, `:396-399`, `:513-517`, `:540-544` | All manual update/updater procedures are Compose + `.env` | Operator docs to migrate. |
| 29 | `apply_edge.go:50-93`; `plan.go:158-160`; `docs/configuration.md:101`, `:1253` | Registry `ghcr.io/accreleus/quasar` namespace, `sha-<7>` tags, source-commit label | Not Compose, but a deployment-owner assumption (registry, allowlist) any new owner must honour. |
| 30 | (absence) grep for Portainer/Dockge/Unraid template/Watchtower in update code | No manager integration exists | Any external-manager support is net-new; today such stacks are ineligible (fail-closed discovery) [INFERENCE]. |
