# RH-06 acceptance map (#368)

Date: 2026-09-26. The acceptance record for RH-06, "Quasar-owned installation, updates and
recovery" (specification #352; slices #353–#367, plus #381, #385 and #386, which the owner
moved into RH-06 on 2026-09-26). It ties every user story in #352 to the ticket whose
acceptance covers it and to the evidence, and it records the status of #368's own acceptance
rows.

**Status: submitted for the owner's acceptance. This document promotes nothing.** Promotion
to `develop` or `main`, a published release, and image publication beyond the test registry
all keep their separate owner approvals. By the owner's decision of 2026-09-26, every release
stays edge until RH-07.

**Acceptance commit.** Branch `rh06/368-acceptance`, which is
`initiative/resilient-host-architecture` at `07a6d9cf` merged in as `70383a3f`, plus this
document and its CHANGELOG line. Every RH-06 slice is integrated at `07a6d9cf`. The protocol
pin is `79a8db0`, now on `quasar-protocol` `main` (the owner approved the contracts). The
images for #368's own live rows were built by `deploy/build-images.sh` from `70383a3f`,
version `0.4.0-dev.368`, and pushed to the test registry under a content-addressed tag:

| Image | Digest | Contract |
|---|---|---|
| recovery (seed and actor), recipe 1 | `sha256:c9da4e21a2464eaccaced6c5bde45da8e749f22461ab91ba35bd76b14eb33e97` | 26/0 |
| node agent | `sha256:d04e35250ca3fa62b343b7a68bf0a5a19e8d7271510a02f98f58ec30a8b187b2` | 149/0, 2 GPU-gated skips |
| control plane | `sha256:7d71023c48a60279cbaac3d23b71a0b1580df34e6be29a99480bdd3005f4907e` | 25/0 |

Schema 97, read from the control plane's identity on every install below.

**How to read the evidence.** Each ticket's closing evidence is an issue comment, linked
below. Machines are named by role only: the AMD test host, the NVIDIA test host (its GPU is
shared with another workload), and the aux-infra host (headless browser peer and network
shaping only). Bench runs are quasar-bench run ids under `--repo accreleus/quasar`. A bench
verdict is quoted verbatim, not restated.

**Scope decisions that change the matrix.**
- Owner, 2026-09-26 (#368, #364, #367): pre-RH-06 installs are not migrated. Operators
  redeploy from scratch with the seed. The "pre-RH-06 dump restored into a fresh install" row
  is dropped; that work is #380, wanted before the first release to `main` together with RH-07.
- Owner, 2026-09-26: the reboot-mid-attempt row is not required and is skipped. The
  Docker-daemon restart row covers an engine restart mid-attempt.
- Owner, 2026-09-26: #381, #385 and #386 are fixed inside RH-06 before acceptance.

## Evidence index

| Ticket | What it delivered | Closing evidence | Bench runs |
|---|---|---|---|
| #353 RH06-01 | Amendment 14, ADRs 0007/0008, the ADR 0004 amendment | [Opus APPROVED][353a], [done][353b] | none (no hardware) |
| #354 RH06-02 | Console mockups | [mockups][354a], [owner approval][354b] | none |
| #355 RH06-03 | `quasar-runtime` crate (pure refactor) | [done][355] | none |
| #356 RH06-04 | Release trust in Rust, golden vectors, socket fixtures | [done + Opus security review][356] | none |
| #357 RH06-05 | Recovery actor, recipes, GPU probe, owned GPU host | [live, both GPU hosts][357] | none |
| #358 RH06-06 | Seed mode and first install | [live, AMD test host][358] | none |
| #359 RH06-07 | Add host: one-liner and Dockge/Arcane stack | [live][359a], [follow-up][359b] | `2479f913-be0b-4b32-8951-9e599d9f6d7d`, `9fb4c073-458e-4b15-82fd-c688c9c91fe1` |
| #360 RH06-08 | Agent replacement, revert, developer apply | [live, both GPU hosts][360] | `69dd785f-4473-469a-b0b9-821d8a574fd0` (the RH-06 Steam preflight at the #360 merge) |
| #361 RH06-09 | Combined and control-only installs, machine inputs | [live][361] | `9f54b075-d0ce-4ba0-be68-7042114603de`, `5eff50ef-6105-4d25-a499-758bceb49d66` |
| #362 RH06-10 | Recovery actor self-replacement | [live, fault matrix][362] | `b35fbdec-3866-4322-b0a6-d7e3aeccd626` (hand-over ride-through), `392d2919-b305-489a-b453-d1797643abbf`, `3d638335-21d0-4e1e-b7fc-cad8b0899156` |
| #363 RH06-11 | Control-plane replacement on an owned machine | [live][363] | `ad4ba5a2-d923-4d7a-b40b-744dfe12831c` (control-plane update ride-through) |
| #364 RH06-12 | Pre-update dump, `restore`, external-backup confirmation | [live, nine recipes][364] | one run, marked contaminated by bench (client-side judder); `qbench check` exit 4 |
| #365 RH06-13 | Format-2 releases, floor, `below_floor`, new edge tags | [containerized][365] | none (no hardware) |
| #366 RH06-14 | Race guard, remove host, `uninstall`, `reconfigure`, Dockge race | [live][366] | `93c55aad-d13b-408f-b53e-f432b788a9e1` |
| #367 RH06-15 | Compose updater retired, docs and site rewritten | [containerized][367] | none (no hardware) |
| #381 | The seed restarts a recovery actor stopped from outside | [live, on a separate owned install][381]; again in L1 below | none |
| #385 | Add host serves the images its machine runs | [live, on a separate owned install][385] | none |
| #386 | `reconfigure` of control-plane inputs on combined and control-only machines | [containerized][386]; live in L1 below | `3edd0589-7b28-426b-99c5-4e4d61e4597f` |
| #368 RH06-16 | This acceptance | this document; L1–L3 below | `7810b019-25ee-458e-87ab-5a5780139166`, `3edd0589-7b28-426b-99c5-4e4d61e4597f`, `2920ab7c-061d-4570-a39d-ed0277c21370` |

## #368's own acceptance rows

Status values: **covered** (an earlier ticket's live evidence covers it, and where), **run
here** (run live for #368, results below), **skipped** or **dropped** (by owner decision,
and where the work went).

| # | Row | Status | Evidence |
|---|---|---|---|
| 1a | Fresh combined install on the AMD test host | **run here: pass** (L1) | L1 below; bench `7810b019-25ee-458e-87ab-5a5780139166` |
| 1b | Fresh control-only install with a GPU host on the other test host | **run here: pass** (L2) | L2 below; bench `2920ab7c-061d-4570-a39d-ed0277c21370` |
| 1c | GPU hosts on AMD and NVIDIA | **covered**, and again in L2 | [#357][357] (both hosts, a session each); [#359][359a] (NVIDIA one-liner, AMD Dockge stack, Steam on both); [#361][361] (AMD by Add host); [#360][360] (NVIDIA fresh seed install); L2 (NVIDIA by Add host from a control-only console) |
| 2a | An Arcane install with its stack in Arcane's own storage | **run here: pass** (L1, and L2's control-only install) | L1 below |
| 2b | The Dockge race test, with the stack in Dockge's own storage | **covered** | [#366][366] row 7: Update unchanged, Update after an edit, Restart; actor and agent ids and start times unchanged, no conflict fought; the negative case (a stack that also defines `quasar-node-agent`) raised `owner_conflict` and was never acted on. [#359][359a]: Dockge stack install and redeploy |
| 2c | An operator-supplied Postgres install | **covered** | [#361][361]: control-only with an external Postgres, password only as a file, console "Your own · reachable", external database stopped and explained by `status`. [#364][364] recipe 6: `backup_unconfirmed`, a confirmed backup, `restore --to`, and a purge that leaves the operator's database untouched |
| 3a | Agent replacement with real sessions | **covered** | [#360][360] step 8: the session ended on the agent update (as story 39 states), and a new session ran on the new agent at 1080p60 with 0 packets lost |
| 3b | Control-plane replacement with real sessions | **covered** | [#363][363] §4: Steam AV1 1440p60 on the combined host's GPU held through a control-plane update, 0 packets lost, 0 freezes; bench `ad4ba5a2-d923-4d7a-b40b-744dfe12831c`, soak PASS; `qbench check` exit 4 (a new scenario, nothing comparable; not a pass) |
| 3c | Recovery-actor replacement with real sessions | **covered** | [#362][362]: Steam 1080p60 held through a hand-over, 601 frames every 10 s straight through it, 0 packets lost; bench `b35fbdec-3866-4322-b0a6-d7e3aeccd626`, PASS |
| 3d | Actor killed mid-pull | **covered** | [#360][360] row 5 (agent, twice); [#363][363] §3 `d84ce147` (control plane, a real pull); [#364][364] recipe 5 (during `dumping`: interrupted, nothing changed) |
| 3e | Actor killed mid-verification | **covered**, and again in L1 | [#360][360] row 6 (agent); [#362][362] a1–a3 (successor killed at `handing_over`, `verifying`, after `done`); [#363][363] §3 `a7c36693`; [#364][364] recipe 5; L1 #386 recipe 3 (`docker kill` while the control plane verified; the seed restarted the actor, #381) |
| 3f | Docker daemon restart mid-attempt | **covered** | [#360][360] 7a (during pulling: interrupted, nothing changed) and 7b (during verifying: continued to succeeded); [#362][362] b; [#364][364] recipe 4 (during a restore's load: continued, succeeded) |
| 3g | Reboot mid-attempt | **skipped by owner decision (not required)**, 2026-09-26. Row 3f covers an engine restart mid-attempt | none |
| 4a | A migrating update with `restore` | **covered** | [#364][364] recipes 1–3: four migrating developer applies each dumped first (three dumps kept); an injected migration failure was not restored automatically and printed one `restore` command; that command restored; a re-run was refused; `--force-again` worked |
| 4b | A pre-RH-06 dump restored into a fresh install | **dropped** (owner, 2026-09-26) | moved to #380 |
| 5 | The acceptance map covers every user story; the report requests the owner's acceptance and promotes nothing | **this document**, and the #368 closing report | below |

### Rows run for this ticket

Every mutation was preceded by the shared-host version preflight in `AGENTS.md`. Before L1,
the AMD test host ran an owned GPU host enrolled to the NVIDIA test host's combined lab
install; both were removed with `uninstall --purge`, keeping homes and templates.

**L1: a fresh combined install on the AMD test host, declared in Arcane (rows 1a, 2a).**

| Check | Result |
|---|---|
| Arcane installed (pre-approved), v2.14.0 | pass |
| The Quick Start's combined seed stack (`site/src/data/stack-template.js`, `seedStack`, with the lab's release-trust inputs for the test registry) created as an Arcane project and deployed with Arcane's Up | pass. The stack file is `projects/quasar/compose.yaml` inside Arcane's own data volume. Arcane lists one project with one service. Quasar's recovery actor, Postgres, control plane and node agent run beside it with this installation's labels and no Compose project label. The machine-state volume is `quasar-machine`, with no project prefix |
| Install to a healthy control plane | 15 s |
| First admin | claimed with the per-boot setup token (`POST /v1/setup/claim`, HTTP 201) |
| Identity | `install_mode: owned`, `machine_role: combined`, `database_mode: owned`, schema 97; the actor, seed and agent all at `0.4.0-dev.368` |
| Services card | matches `inv-combined.png`: COMBINED HOST, all five rows, the "not removed from here" note; the database reads "Quasar's own", as the handoff README allows |
| Steam from the user library on the AMD GPU | pass on the second session: H.264 1920×1080 at 60 fps, decoding 2.0 s after the click, 13 994 frames, 0 of 202 005 packets lost, 0 freezes, observe soak PASS; bench `7810b019-25ee-458e-87ab-5a5780139166`. The first session on the fresh agent failed: the agent segfaulted loading `libgstvulkan.so` and restarted. That is #388, found in #364's run and open |
| Arcane Redeploy (pull always, force recreate) and Arcane Restart | pass. Each re-created or restarted only the seed, which logged "a recovery actor exists; nothing to do". The actor, Postgres, control plane and agent kept their container ids, start times and restart counts, and `status` reported no conflicts |

**L1, continued: #386's live recipes on that combined install (story 50).**

| Recipe | Result |
|---|---|
| Public host, TLS names and both ports (`QUASAR_PUBLIC_HOST`, `QUASAR_TLS_HOSTS`, `QUASAR_HTTP_PORT` 8080→18080, `QUASAR_TLS_PORT` 8443→18443) | pass. `--dry-run` listed the changes, the re-creation order (control plane, then node agent) and their costs, and moved nothing; without `--yes`, exit 3 and nothing changed; with `--yes` the attempt succeeded in 14 s. The console answered on the new HTTPS port and no longer on the old one. The agent reconnected on the new loopback HTTP port as the same host. Both containers kept the same image digests, and `reconfigure.json` settled `applied` |
| A control plane that can never serve: the new HTTPS port held by a listener on the host | pass. The attempt failed `unhealthy` and was restored (`restored: true`); `reconfigure.json` settled `put_back`; `machine.json` kept the old ports; the same control-plane container was back, one control plane, no `.kept`; the console answered on the old port |
| `docker kill quasar-recovery` while the re-created control plane was verifying (a reconfigure back to the default ports) | pass. Docker counts a kill as a manual stop; the seed started the actor again 34 s later ("it was stopped from outside … and it finishes what it was doing", #381). The actor continued the attempt, which succeeded; `reconfigure.json` settled `applied`, one control plane, no `.kept` |
| Database mode, database settings, node name and role | refused, exit 1, nothing changed, each naming the reinstall path |
| A Steam session afterwards, on the re-created agent | pass: H.264 1080p60, 13 985 frames, 0 packets lost, 0 freezes; bench `3edd0589-7b28-426b-99c5-4e4d61e4597f` |

**L2: control-only on the AMD test host, the NVIDIA test host as a GPU host (rows 1b, 1c).**

| Check | Result |
|---|---|
| L1 removed | Arcane Down (the seed only), then `uninstall --purge` (homes kept) |
| Control-only install | the Quick Start's control-only seed stack as a new Arcane project: seed, recovery actor, Postgres and control plane healthy, no node agent; identity `machine_role: control_only`, `database_mode: owned`; Releases ▸ This machine reads "Control-only host … Node agent: none on this machine" |
| NVIDIA test host | its previous lab install removed with `uninstall --purge` (a final dump taken, homes kept); the RTX 5090 was free |
| Add host | the console's one-line command (from the real dialog) on the NVIDIA test host: host checks passed, the seed pulled and started, "enrolled: this host is now 'gpu-n368'" in 13 s. The actor logged `actor-gpus-served` and installed the NVIDIA shape (a GPU device request); the Add host command carried the control plane's release trust (#366) and the images its machine runs (#385) |
| GPU host card | the node agent and recovery actor at `0.4.0-dev.368`; "No database runs on a GPU host"; "Control plane · Runs on another machine"; Remove host offered |
| Steam from the user library on the RTX 5090 | pass: AV1 2560×1440 at 60 fps, decoding 3.0 s after the click, 13 814 frames, 0 of 152 841 packets lost, 0 freezes, jitter 3 ms, observe soak PASS; bench `2920ab7c-061d-4570-a39d-ed0277c21370` |

L2 is left running as the lab fleet.

**L3: the Releases channel on hardware (stories 20 and 37): not run, containerized only.** An
owned control plane cannot be pointed at the lab registry without a code change, so no
format-2 release could be offered to it:
- the stable and beta channels read GitHub Releases (`QUASAR_PLATFORM_RELEASE_REPO`), and
  nothing may be published there without the owner's approval;
- the edge channel resolves `<QUASAR_PLATFORM_REGISTRY>/<repo>/quasar-*:o2-<branch>` over
  HTTPS only (`control-plane/internal/images/digest.go`), while the test registry is plain
  HTTP;
- `QUASAR_PLATFORM_REGISTRY` and the release-source settings are not inputs of an owned
  install (`COMPOSE_ONLY_KNOBS` in `node-agent/crates/quasar-recovery/tests/recipe_compose_parity.rs`).

Every live replacement therefore ran as a developer apply, which goes through the same
recovery-actor path. The Releases-channel apply and the unattended run are covered by the
control plane's DB tests.

**Bench.** `qbench check --head 70383a3f` against the nearest benched ancestor, verbatim:

```
70383a3f vs c03faec1: no scenario has a valid run at both commits, so there is nothing to judge.
  only at head: baseline/steam-1080p60-h264-observe
  excluded (base): run f9efcec2 baseline/steam-1440p60-av1-observe is contaminated
result: no_comparable_runs
```

Exit 4, which is not a pass. Against earlier RH-06 commits (`--base`), verbatim:

```
70383a3f vs 3de104c8 over 1 scenario: 0 regressed, 0 improved, 1 unchanged.
result: clean
```

```
70383a3f vs 7f491e68 over 2 scenarios: 1 regressed, 1 improved, 0 unchanged. Worse on steam-1080p60-h264-observe.
  regressed  baseline/steam-1080p60-h264-observe: … worse on browser.jitter_buffer_ms (+19.3%).
  improved   baseline/steam-1440p60-av1-observe: … better on browser.frames_dropped (-100.0%); worse on nothing beyond threshold.
result: regressed
```

```
70383a3f vs 97e23520 over 1 scenario: 1 regressed, 0 improved, 0 unchanged. Worse on steam-1080p60-h264-observe.
  regressed  baseline/steam-1080p60-h264-observe: … worse on browser.present_interval_p95_ms (+85.0%) and browser.present_interval_sd_ms (+39.3%).
result: regressed
```

Every flagged metric is measured in the browser at the aux-infra peer, whose link is WiFi.
Each comparison sets one or two sessions against one. RH-06 changes nothing on the streaming
path. The same scenario is unchanged against the first owned-install baseline (`3de104c8`),
and the AV1 scenario improved. These are recorded as the check reported them. They are not
explained by a measured cause, only by the variance #362 and #366 already saw on this peer.

## User-story map

Every user story in #352, the ticket(s) whose acceptance covers it, and the evidence.
"Live" means real-hardware evidence on the test hosts, or, where noted, on a separate owned
install that is not a test host. "Containerized" means the sanctioned containerized tests
only: the control plane's admin API against real ephemeral Postgres, the recovery actor's
three calls with the in-memory engine and crash injection, the shared Go↔Rust fixtures, and
the real Docker adapter in the dev container.

| # | Story (short form) | Ticket(s) | Evidence | Covered by |
|---|---|---|---|---|
| 1 | Install with one command or one small stack | #358, #359, #361, #367 | [#358][358] `docker run` seed; [#359][359a] one-liner and Dockge stack; [#361][361] a one-service combined stack; [#367][367] the site Quick Start writes it; L1/L2 the Quick Start's stack in Arcane | live |
| 2 | A Dockge or Arcane stack holds only the seed | #358, #359, #366 | [#359][359a] Dockge stack shows one container; [#366][366] row 7 Dockge race; L1 Arcane project with one service | live |
| 3 | The seed snippet holds no long-lived secret | #358, #359, #361 | [#358][358] and [#359][359a] secret hygiene: the enrollment string is single-use and appears only in the seed's input, the 0600 machine-state secret and the agent's 0400 file. The operator's own database password is the one documented exception (#352 decision 7), [#361][361]. L1's Arcane stack file carries no secret | live |
| 4 | Quasar generates its own database password and secret key as files | #361 | [#361][361] "Secrets": only `*_FILE` paths in env, Postgres password file `0400 root:root`, 0 of 10 secret values found outside Quasar's secrets | live |
| 5 | Choose combined, control-only or GPU host at install | #357, #361 | [#357][357] GPU; [#361][361] combined and control-only; L1 combined, L2 control-only and GPU | live |
| 6 | Quasar detects the GPU vendor and devices | #357, #360 | [#357][357] the probe on both hosts; [#357 owner decision][357d] on the probe; [#360][360] the NVIDIA shape survives every replacement; L2 `actor-gpus-served` | live |
| 7 | Point Quasar at my own Postgres with ordinary stack inputs | #361, #364 | [#361][361] control-only with an external database; [#364][364] recipe 6 | live |
| 8 | Quasar never dumps, restores, resets or upgrades my database | #361, #364, #366 | [#364][364] recipe 6: `backup_unconfirmed` instead of a dump; `restore --to` never loads a dump and refuses a dirty, newer schema; a purge reports "The database is your own: Quasar never dumps or deletes it" and leaves it at its schema | live |
| 9 | "Add host" gives a one-line command by default | #359 | [#359][359a] one-liner on the clean NVIDIA host; L2 the same from a control-only console | live |
| 10 | The one-line command checks and prepares the host | #359 | [#359][359a] host checks and `--fix`; [#359 follow-up][359b] `--fix-only` (tests); L2 host checks | live, follow-up containerized |
| 11 | An alternative seed stack for a GPU host | #359 | [#359][359a] Dockge stack from the "Dockge or Arcane" tab on the AMD host | live |
| 12 | Each enrollment token enrolls one machine once and expires | #357, #359, #361 | [#357][357] a second start with a different string does not re-enroll; [#359][359a] a spent token refused with `auth_failed`; [#361][361] the local token used 1/1 | live |
| 13 | The static enrollment token is gone | #361, #367 | [#361][361] no static token on owned installs; [#367][367] `ENROLLMENT_TOKEN` retired | live (#361), containerized (#367) |
| 14 | Re-running the command or the seed changes nothing | #357, #358, #359, #361 | [#357][357], [#358][358] redeploy and restart, [#359][359a] re-run, [#361][361] idempotency: same ids and start times; L1 Arcane Redeploy and Restart | live |
| 15 | A machine that lost its state re-enrolls under the same node name | #359, #366 | [#359][359a] spent-token reset re-enrolled on the same host id; [#366][366] re-add after a reset and after a removal onto the same host row, history kept | live |
| 16 | The manager shows one Quasar entry; services run beside it | #359, #366 | [#359][359a] Dockge shows one container; [#366][366] row 7; L1 Arcane lists one project with one service | live |
| 17 | Removing or redeploying the seed stack never removes or restarts Quasar | #358, #359, #366 | [#358][358] seed removed, restarted, redeployed; [#359][359a] Dockge redeploy and edit; [#366][366] row 7; L1 Arcane Redeploy and Restart: the four services' ids, start times and restart counts unchanged | live |
| 18 | A warning when a machine's seed is missing | #358, #366 | [#358][358] seed row `NOT FOUND`; [#366][366] row 2 "No seed found", the agent re-registers by itself | live |
| 19 | The console shows each machine's services with version and owner | #357, #358, #361, #362 | [#357][357] GPU card vs `inv-gpu.png`; [#358][358] seed row; [#361][361] combined and control-only; [#362][362] new actor version; L1 `inv-combined.png`, L2 GPU host and control-only | live |
| 20 | Update control plane, agents and recovery actors from the console | #360, #362, #363, #365 | Live through developer apply: agent [#360][360], actor [#362][362], control plane [#363][363], migrating control plane [#364][364]. The Releases-channel apply of a format-2 release is containerized ([#365][365], the control plane's DB tests): no lab release source is possible without a code change (L3) | live (developer apply); release channel containerized |
| 21 | Control plane first, never offered a downgrade | #353, #362, #363, #365 | [#363][363] refusals: a schema below the database's is `422 release_below_schema_version`; [#362][362] A1 refusals on the combined host; [#365][365] planner ordering; ADR 0002 unchanged | live (refusals), containerized (planner) |
| 22 | A migrating release drains the fleet first | #363, #364 | the fleet-run drain before a migrating control-plane step is unchanged from before RH-06 (`apply_fleet.go` `prepareFleet`) and DB-tested (`apply_fleet_db_test.go`); the migrating decision a drain uses is tested in `migrating_step_test.go` (#364). No migrating fleet run was live: it needs a release (L3) | containerized |
| 23 | Dump the database before a migrating control-plane replacement | #364 | [#364][364] recipe 1: each dump taken before the old control plane stopped | live |
| 24 | A migrating update is refused when the dump cannot be taken | #364 | [#364][364]: refusal on dump failure and insufficient space (`actor_migrate.rs`, `migrating_step_test.go`); an interrupted dump ends `interrupted` with nothing changed (recipe 5, live); console states per the mockup | live and containerized |
| 25 | My own Postgres: confirm a backup before a migrating update | #364 | [#364][364] recipe 6: refused `backup_unconfirmed`, then accepted with the confirmation | live |
| 26 | A failed migrating update prints one `restore` command | #364 | [#364][364] recipes 2–3: the command in the actor's output, its log line and the console's restore card; running it restored | live |
| 27 | Never an older control plane against a newer schema | #363, #364 | [#363][363] `422 release_below_schema_version`; [#364][364] recipes 6 and 8: `restore --to` refused against a dirty, newer schema; a kept older control plane started by hand exits without touching the database | live |
| 28 | A failed agent update is restored automatically | #360 | [#360][360] forced-unhealthy image on both GPU hosts: `restored=true`, same container id, `auto_revert` row | live |
| 29 | A control-plane update that never started is restored automatically | #363, #386 | [#363][363] §2: a control plane whose health always fails is restored after 71.9 s, reason `unhealthy`; L1 #386 recipe 2 (a port already held) | live |
| 30 | Revert an agent from the console | #360 | [#360][360] row 3 | live |
| 31 | A host below the floor says "must update before it can be managed" | #365 | [#365][365]: planner, `409 host_not_eligible / below_floor`, console states and screenshots. One live observation, on a separate owned combined install (not a test host), 2026-09-26: its node agent stamped `0.3.1-dev.edge1` read "must update before it can be managed" under the `0.4.0-0` floor; a developer apply of the same commit stamped `0.4.0-dev.edge2` (control-plane step, then agent step, both succeeded) cleared it and `below_floor` went false. Lab hosts on `0.3.1-dev.*` stamps read the same after #365, as expected | containerized, one live observation |
| 32 | Every attempt ends in a stated outcome | #360, #362, #363, #386 | [#360][360] all four outcomes live; [#362][362] fault matrix; [#363][363] §3; L1 `reconfigure.json` settled `applied` and `put_back` | live |
| 33 | An interrupted update is finished after the actor, the daemon or the machine restarts | #360, #362, #363, #364, #381 | actor and daemon restarts: [#360][360] rows 5–7, [#362][362] a1–b, [#363][363] §3, [#364][364] recipes 4–5, [#381][381] (the seed restarts an actor stopped from outside), L1 #386 recipe 3. Machine restart: **skipped by owner decision** (row 3g) | live, except the reboot |
| 34 | Quasar never retries a failed update on its own | #360, #363 | [#360][360] settle table; [#363][363] §3 `d84ce147`: "It is not retried: apply again to try again" | live |
| 35 | The recovery actor replaces itself safely | #362 | [#362][362] actor-first order, the full fault matrix, `seed.json` names the new actor | live |
| 36 | A failed recovery-actor update leaves the previous actor running | #362 | [#362][362] d, and after the follow-up: no agent, `restored: true`, the previous actor back, one actor | live |
| 37 | Unattended updates: agent, actor and non-migrating control plane only | #363, #364, #365 | the auto-apply rules are DB-tested; [#364][364]: unattended runs never migrate. No live unattended run: it needs a release source the lab cannot provide without a code change (L3) | containerized |
| 38 | A non-migrating control-plane update keeps sessions streaming | #363 | [#363][363] §4; bench `ad4ba5a2-d923-4d7a-b40b-744dfe12831c` | live |
| 39 | An agent update states that it ends that host's sessions | #360 | [#360][360]: the console says so; step 8 shows the session ending | live |
| 40 | Refuse to act on a Quasar-looking container Quasar did not create; raise a readiness fault | #366 | [#366][366] row 1 (a leftover Compose agent) and the row 7 negative (a stack-defined agent): reported, `owner_conflict`, never touched; [#362][362] c, c3 (a stray actor exits) | live |
| 41 | The console says when another owner's container blocks a release | #366 | [#366][366] console row 1: warning, "In the way" row, preflight `blocked`, developer apply `409` | live |
| 42 | Remove a GPU host from the console | #366 | [#366][366] rows 3 and 4, forced failure then Retry | live |
| 43 | Uninstall keeps my data unless I purge | #366, #364 | [#366][366] row 5 (combined); [#364][364] recipe 6; L1/L2 teardowns kept homes | live |
| 44 | Purge needs typed confirmation and takes a final dump of an owned database | #366, #364 | [#366][366] row 5: refused without, with a wrong, and with a seed present; the final `pg_dump` taken; [#364][364] recipe 7; L2 the NVIDIA lab install's purge took its final dump | live |
| 45 | A documented way to move to the new install with my database and homes | — | **dropped** (owner, 2026-09-26): moved to #380. [#367][367] documents replacing an existing install with a fresh seed install; homes are found again under the same home root (L1's Steam session used the admin's existing home) | dropped → #380 |
| 46 | Restore my dump before the new control plane's first boot | — | **dropped**: #380 | dropped → #380 |
| 47 | My GPU hosts re-enroll under their existing names | #366 (owned hosts only) | for owned hosts, [#366][366] re-adds onto the same host row. For hosts of a pre-RH-06 install: **dropped**, #380 | live (owned); pre-RH-06 dropped → #380 |
| 48 | An older install is not offered RH-06 as an in-place update | #365, #367 | [#365][365] v0.3.0's release readers (`internal/platform/prerh06/`) find nothing in a v2-only release; [#367][367] format-1 publication stopped | containerized |
| 49 | Edge does not offer RH-06 builds to a pre-RH-06 install | #365 | [#365][365] `o2-<branch>` edge tags are not resolved by the pre-RH-06 readers | containerized |
| 50 | Install-time settings changed with one `reconfigure` command | #366, #386 | [#366][366] row 6 on a GPU host (home root, trust); [#386][386] and L1: the public host, TLS names and ports on a combined machine as a verified control-plane replacement, a never-healthy change put back, an actor kill mid-verify settled; the database mode and node name refused with the reinstall path (a decision #386 records) | live |
| 51 | Agent settings stay in the existing host policy | #361 | [#361][361] decisions: machine inputs are install-time only, console settings stay in the database; [#366][366] and L1: `reconfigure` refuses the node name | containerized, plus live refusals |
| 52 | Release signature and allowlist rules behave exactly as today | #356, #365, #367 | [#356][356] 282 golden vectors from Go's own output, passed by both implementations, Opus security review APPROVED; [#365][365] four new vectors for the v2 pair; [#367][367] Go originals deleted only after that. Live `namespace_rejected`: [#363][363] refusals | containerized, plus live refusals |
| 53 | Only the control plane may ask for a control-plane replacement; the agent only its host's agent and actor | #356, #360, #363 | [#356][356] agent-socket component guard; [#360][360] "The agent can name only the agent and the recovery actor"; [#363][363] §1 status scoping by socket | live and containerized |
| 54 | The last three pre-update dumps are kept | #364 | [#364][364] recipe 1: after four migrating updates, three dumps kept and the oldest pruned; `restore --list` shows them with their schemas | live |
| 55 | See per machine whether the database is Quasar's or mine | #361 | [#361][361] console: "Quasar's own" / "Your own · reachable"; L1, L2 | live |
| 56 | The Compose-from-source workflow stays for fast development loops | #367 | [#367][367] documented as contributor tooling; the throwaway contributor-lane control planes used for [#357][357], [#358][358] and [#360][360] | live (as used), docs |
| 57 | Apply a branch build by digest from an allowlisted namespace | #360 | [#360][360], [#362][362], [#363][363], [#364][364], [#366][366]: every live replacement ran as a developer apply | live |
| 58 | The Compose updater, its image, volume, preflight checks and label discovery are removed | #367 | [#367][367] deletion list and the `git grep` audit | containerized |
| 59 | Every container shape is declared by a recipe revision the actor carries | #357, #361, #365 | [#357][357] recipe label and golden rendered specifications, matched live on both hosts; [#361][361] agent recipe revision 2; [#365][365] control-plane recipe revision 2; ADR 0008 | live and containerized |
| 60 | The release process refuses a release whose floor or recipes would strand a machine | #365 | [#365][365] `check-release-compatibility.sh`, four refusal cases | containerized |
| 61 | The seed's contract is frozen and tested against every released actor | #353, #358, #381 | ADR 0007; [#358][358] `seed_contract.rs` over the first fixture set; [#381][381] adds `rh06-381`; `testdata/recovery/seed/actors/` is append-only. No actor has been released yet | containerized |
| 62 | My session is unaffected by actor and non-migrating control-plane updates | #362, #363 | bench `b35fbdec-3866-4322-b0a6-d7e3aeccd626` (actor hand-over), `ad4ba5a2-d923-4d7a-b40b-744dfe12831c` (control-plane update) | live |

## What is not fully live

- **Stories 20 and 37**, the Releases channel and unattended updates on hardware: the lab
  cannot offer an owned control plane a format-2 release without a code change or a
  publication (L3). Covered by DB tests.
- **Story 22**, the fleet run's drain before a migrating step: DB-tested; no migrating fleet
  run was possible without a release (L3). Every live migrating update was a developer apply.
- **Story 31**, `below_floor`: containerized, plus the one live observation above.
- **Story 33**, a machine reboot mid-attempt: skipped by owner decision (row 3g).
- **Stories 45–47** and row 4b: dropped by the owner's decision of 2026-09-26; #380.

## Open follow-ups

| Issue | What |
|---|---|
| #378 | Steam warm-up stays deferred on a host that already has the Steam image |
| #379 | Forgetting a host leaves its users' homes stuck in `home_conflict` |
| #380 | Restore a pre-RH-06 Compose dump into a fresh owned install (the dropped row) |
| #382 | Small follow-ups from the control-plane replacement live run (#363) |
| #384 | Library scan does not fetch artwork for the games it detects |
| #388 | The node agent segfaults loading `libgstvulkan.so` on its first session after being replaced (seen again in L1, on a fresh install) |

## Checks at the acceptance commit

Rerun at this ticket's branch head, one at a time, under the shared heavy-gate lock. The
results are recorded in #368's closing report.

[353a]: https://github.com/accreleus/quasar/issues/353#issuecomment-5828476092
[353b]: https://github.com/accreleus/quasar/issues/353#issuecomment-5828534676
[354a]: https://github.com/accreleus/quasar/issues/354#issuecomment-5826395465
[354b]: https://github.com/accreleus/quasar/issues/354#issuecomment-5827853143
[355]: https://github.com/accreleus/quasar/issues/355#issuecomment-5826554473
[356]: https://github.com/accreleus/quasar/issues/356#issuecomment-5827744803
[357]: https://github.com/accreleus/quasar/issues/357#issuecomment-5833073898
[357d]: https://github.com/accreleus/quasar/issues/357#issuecomment-5832756112
[358]: https://github.com/accreleus/quasar/issues/358#issuecomment-5835044359
[359a]: https://github.com/accreleus/quasar/issues/359#issuecomment-5841554216
[359b]: https://github.com/accreleus/quasar/issues/359#issuecomment-5841752511
[360]: https://github.com/accreleus/quasar/issues/360#issuecomment-5835797659
[361]: https://github.com/accreleus/quasar/issues/361#issuecomment-5841902646
[362]: https://github.com/accreleus/quasar/issues/362#issuecomment-5843459446
[363]: https://github.com/accreleus/quasar/issues/363#issuecomment-5843988843
[364]: https://github.com/accreleus/quasar/issues/364#issuecomment-5846201259
[365]: https://github.com/accreleus/quasar/issues/365#issuecomment-5845199594
[366]: https://github.com/accreleus/quasar/issues/366#issuecomment-5845894834
[367]: https://github.com/accreleus/quasar/issues/367#issuecomment-5846075742
[381]: https://github.com/accreleus/quasar/issues/381#issuecomment-5846986063
[385]: https://github.com/accreleus/quasar/issues/385#issuecomment-5846528223
[386]: https://github.com/accreleus/quasar/issues/386#issuecomment-5847138292
