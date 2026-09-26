# RH-06 acceptance map (#368)

Date: 2026-09-26. The acceptance record for RH-06, "Quasar-owned installation, updates and
recovery" (specification #352, slices #353–#367). It ties every user story in #352 to the
ticket whose acceptance covers it and to the evidence, and it records the status of #368's
own acceptance rows.

**Status: submitted for the owner's acceptance. This document promotes nothing.** Promotion
to `develop` or `main`, a published release, and image publication beyond the test registry
all keep their separate owner approvals. By the owner's decision of 2026-09-26, every release
stays edge until RH-07.

**Baseline.** `initiative/resilient-host-architecture` at `fa78e667` (#367 integrated). The
protocol pin is `79a8db0` on `quasar-protocol` branch `rh06-367-retire`. #364's live evidence
was still running when this map was drawn; its rows are marked below.

**How to read the evidence.** Each ticket's closing evidence is an issue comment, linked
below. Machines are named by role only: the AMD test host, the NVIDIA test host (its GPU is
shared with another workload), and the aux-infra host (headless browser peer and network
shaping only). Bench runs are quasar-bench run ids under `--repo accreleus/quasar`. A bench
verdict is quoted from its ticket, not restated here.

**Scope decisions that change the matrix.**
- Owner, 2026-09-26 (#368, #364, #367): pre-RH-06 installs are not migrated. Operators
  redeploy from scratch with the seed. The "pre-RH-06 dump restored into a fresh install" row
  is dropped; that work is #380, wanted before the first release to `main` together with RH-07.
- Owner, 2026-09-26: the reboot-mid-attempt row is not required and is skipped. The
  Docker-daemon restart row, which #352's owner comment of 2026-09-25 made its stand-in,
  covers an engine restart mid-attempt.

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
| #364 RH06-12 | Pre-update dump, `restore`, external-backup confirmation | pending (live run in progress) | pending |
| #365 RH06-13 | Format-2 releases, floor, `below_floor`, new edge tags | [containerized][365] | none (no hardware) |
| #366 RH06-14 | Race guard, remove host, `uninstall`, `reconfigure`, Dockge race | [live][366] | `93c55aad-d13b-408f-b53e-f432b788a9e1` |
| #367 RH06-15 | Compose updater retired, docs and site rewritten | [containerized][367] | none (no hardware) |
| #368 RH06-16 | This acceptance | this document; the live rows below | see "Rows run for this ticket" |

## #368's own acceptance rows

Status values: **covered** (an earlier ticket's live evidence covers it, and where), **run
here** (no earlier evidence; run live for #368), **skipped** or **dropped** (by owner
decision, and where the work went).

| # | Row | Status | Evidence |
|---|---|---|---|
| 1a | Fresh combined install on the AMD test host | **run here** (L1). Partial earlier: #366 r5 brought up a throwaway combined install on the AMD host with all four services healthy, then purged it; no first admin, inventory or session | [#366][366] r5; L1 below |
| 1b | Fresh control-only install with a GPU host on the other test host | **run here** (L2). Earlier control-only installs (#361 external database, #364 owned and external) had no GPU host enrolled | L2 below |
| 1c | GPU hosts on AMD and NVIDIA | **covered** | [#357][357] (both hosts, a session each); [#359][359a] (NVIDIA one-liner, AMD Dockge stack, Steam on both); [#361][361] (AMD by Add host); [#360][360] (NVIDIA fresh seed install) |
| 2a | An Arcane install with its stack in Arcane's own storage | **run here** (L1). Every earlier manager run used Dockge | L1 below |
| 2b | The Dockge race test, with the stack in Dockge's own storage | **covered** | [#366][366] row 7: Update unchanged, Update after an edit, Restart; actor and agent ids and start times unchanged, no conflict fought; the negative case (a stack that also defines `quasar-node-agent`) raised `owner_conflict` and was never acted on. [#359][359a]: Dockge stack install and redeploy |
| 2c | An operator-supplied Postgres install | **covered** | [#361][361]: control-only with an external Postgres, password only as a file, console "Your own · reachable", external database stopped and explained by `status`. #364 r6 (pending its comment): control-only on an external database, `backup_unconfirmed`, confirmed backup, `restore --to`, purge leaves the operator's database untouched |
| 3a | Agent replacement with real sessions | **covered** | [#360][360] step 8: the session ended on the agent update (as story 39 states), a new session ran on the new agent at 1080p60 with 0 packets lost |
| 3b | Control-plane replacement with real sessions | **covered** | [#363][363] §4: Steam AV1 1440p60 on the combined host's GPU held through a control-plane update, 0 packets lost, 0 freezes; bench `ad4ba5a2-d923-4d7a-b40b-744dfe12831c`, soak PASS; `qbench check` exit 4 (new scenario, nothing comparable; not a pass) |
| 3c | Recovery-actor replacement with real sessions | **covered** | [#362][362]: Steam 1080p60 held through a hand-over, 601 frames every 10 s straight through it, 0 packets lost; bench `b35fbdec-3866-4322-b0a6-d7e3aeccd626`, PASS |
| 3d | Actor killed mid-pull | **covered** | [#360][360] row 5 (agent, twice); [#363][363] §3 `d84ce147` (control plane, real pull); #364 r5 (dump and replacement phases, pending its comment) |
| 3e | Actor killed mid-verification | **covered** | [#360][360] row 6 (agent); [#362][362] a1–a3 (successor killed at `handing_over`, `verifying`, after `done`); [#363][363] §3 `a7c36693` (after `old_kept`) |
| 3f | Docker daemon restart mid-attempt | **covered** | [#360][360] 7a (during pulling: interrupted, nothing changed) and 7b (during verifying: continued to succeeded); [#362][362] b (successor verifying); #364 r4 (during a restore, pending its comment) |
| 3g | Reboot mid-attempt | **skipped by owner decision (not required)**, 2026-09-26. Row 3f covers an engine restart mid-attempt | none |
| 4a | A migrating update with `restore` | **covered, pending #364's comment** | #364 r1 (migrating developer apply dumps first), r2 (injected migration failure, not restored automatically, the printed `restore` command), r3 (restore, re-run refused, forced re-run) |
| 4b | A pre-RH-06 dump restored into a fresh install | **dropped** (owner, 2026-09-26) | moved to #380 |
| 5 | The acceptance map covers every user story; the report requests the owner's acceptance and promotes nothing | **this document**, and the #368 closing report | below |

### Rows run for this ticket

The live runs for rows 1a, 1b and 2a are planned and approved by the coordinator. They wait
until #364's live run releases the test hosts. Images are built by `deploy/build-images.sh`
at this ticket's branch head into the test registry under a content-addressed tag, and each
mutation is preceded by the shared-host version preflight in `AGENTS.md`.

| Run | Rows | Plan | Result |
|---|---|---|---|
| L1 | 1a, 2a | On the AMD test host: install Arcane, create a combined-role seed-only stack in Arcane from the site's Quick Start snippet, record where the stack file lives (Arcane's own storage), that Arcane lists one entry while Quasar's services run beside it, and that the machine-state volume carries no project prefix. Claim the first admin with the setup token, check the combined services card against `inv-combined.png`, run a Steam session on the AMD GPU (H.264 1080p60, observe soak, posted to bench), then redeploy the stack from Arcane and confirm the actor, control plane, Postgres and agent keep their ids and start times | pending |
| L2 | 1b | Uninstall L1 with `--purge`, keeping homes. Install control-only on the AMD test host from its seed-only stack. On the NVIDIA test host, remove the previous lab install with `uninstall --purge` (homes kept), then enroll it as an owned GPU host with the control-only console's Add host one-liner. Check both machines' services cards and run a Steam session on the NVIDIA GPU (AV1 1440p60, observe soak, posted to bench). If the NVIDIA GPU is held by its other workload, row 1b is recorded as pending, not inferred from AMD | pending |

## User-story map

Every user story in #352, the ticket(s) whose acceptance covers it, and the evidence.
"Live" means real-hardware evidence on the test hosts; "containerized" means the sanctioned
containerized tests only (the control plane's admin API against real ephemeral Postgres, the
recovery actor's three calls with the in-memory engine and crash injection, the shared
Go↔Rust fixtures, the real Docker adapter in the dev container).

| # | Story (short form) | Ticket(s) | Evidence | Covered by |
|---|---|---|---|---|
| 1 | Install with one command or one small stack | #358, #359, #361, #367 | [#358][358] `docker run` seed; [#359][359a] one-liner and Dockge stack; [#361][361] the whole combined install is a one-service stack; [#367][367] site Quick Start writes it; L1 (Arcane) | live |
| 2 | A Dockge or Arcane stack holds only the seed | #358, #359, #366 | [#359][359a] Dockge stack shows one container; [#366][366] row 7 Dockge race; Arcane: L1 | live (Dockge); Arcane in L1 |
| 3 | The seed snippet holds no long-lived secret | #358, #359, #361 | [#358][358] and [#359][359a] secret hygiene: the enrollment string is single-use and appears only in the seed's input, the 0600 machine-state secret and the agent's 0400 file. The operator's own database password is the one documented exception (#352 decision 7), [#361][361] | live |
| 4 | Quasar generates its own database password and secret key as files | #361 | [#361][361] "Secrets": only `*_FILE` paths in env, Postgres password file `0400 root:root`, 0 of 10 secret values found outside Quasar's secrets | live |
| 5 | Choose combined, control-only or GPU host at install | #357, #361 | [#357][357] GPU; [#361][361] combined and control-only; L1, L2 | live |
| 6 | Quasar detects the GPU vendor and devices | #357, #360 | [#357][357] probe on both hosts (NVIDIA: `actor-gpus-served`, device request, driver volume; AMD: render node); [#357 owner decision][357d] on the probe; [#360][360] NVIDIA shape survives every replacement | live |
| 7 | Point Quasar at my own Postgres with ordinary stack inputs | #361, #364 | [#361][361] control-only with an external database; #364 r6 | live |
| 8 | Quasar never dumps, restores, resets or upgrades my database | #361, #364, #366 | #364 r6 (pending its comment): `backup_unconfirmed` instead of a dump, `restore --to` never loads a dump, purge reports "The database is your own: Quasar never dumps or deletes it" and the database is unchanged afterwards | live (pending #364) |
| 9 | "Add host" gives a one-line command by default | #359 | [#359][359a] one-liner on the clean NVIDIA host, enrolled in 2.7 s; dialog matches `add-ready.png` | live |
| 10 | The one-line command checks and prepares the host | #359 | [#359][359a] host checks and `--fix`; [#359 follow-up][359b] `--fix-only` (tests, not re-run live) | live, follow-up containerized |
| 11 | An alternative seed stack for a GPU host | #359 | [#359][359a] Dockge stack from the "Dockge or Arcane" tab on the AMD host | live |
| 12 | Each enrollment token enrolls one machine once and expires | #357, #359, #361 | [#357][357] a second start with a different string does not re-enroll; [#359][359a] spent token refused with `auth_failed`; [#361][361] the local token used 1/1 | live |
| 13 | The static enrollment token is gone | #361, #367 | [#361][361] no static token on owned installs; [#367][367] `ENROLLMENT_TOKEN` retired | live (#361), containerized (#367) |
| 14 | Re-running the command or the seed changes nothing | #357, #358, #359, #361 | [#357][357], [#358][358] redeploy and restart, [#359][359a] re-run, [#361][361] idempotency: same ids and start times | live |
| 15 | A machine that lost its state re-enrolls under the same node name | #359, #366 | [#359][359a] spent-token reset re-enrolled on the same host id; [#366][366] re-add after reset and after removal onto the same host row, history kept | live |
| 16 | The manager shows one Quasar entry; services run beside it | #359, #366 | [#359][359a] Dockge shows one container; [#366][366] row 7; Arcane: L1 | live (Dockge); Arcane in L1 |
| 17 | Removing or redeploying the seed stack never removes or restarts Quasar | #358, #359, #366 | [#358][358] seed removed, restarted, redeployed: actor and agent start times unchanged; [#359][359a] Dockge redeploy and edit; [#366][366] row 7 Update and Restart | live |
| 18 | A warning when a machine's seed is missing | #358, #366 | [#358][358] seed row `NOT FOUND`; [#366][366] row 2 "No seed found", the agent re-registers by itself | live |
| 19 | The console shows each machine's services with version and owner | #357, #358, #361, #362 | [#357][357] GPU card vs `inv-gpu.png`; [#358][358] seed row; [#361][361] combined and control-only vs `inv-combined.png` / `inv-control-only.png`; [#362][362] new actor version shown | live |
| 20 | Update control plane, agents and recovery actors from the console | #360, #362, #363, #365 | Live through developer apply: agent [#360][360], actor [#362][362], control plane [#363][363]. Release-channel apply of a format-2 release: [#365][365] containerized. No format-2 release has been published (publication needs the owner's approval), so the Releases-channel path has no live run | live (developer apply); release channel containerized |
| 21 | Control plane first, never offered a downgrade | #353, #362, #363, #365 | [#363][363] refusals: schema below the database `422 release_below_schema_version`; [#362][362] A1 refusals on the combined host; [#365][365] planner ordering; ADR 0002 unchanged | live (refusals), containerized (planner) |
| 22 | A migrating release drains the fleet first | #364 | pending #364's comment; the fleet-run drain rule is unchanged from before RH-06 (`apply_fleet.go` `prepareFleet`) | pending #364 |
| 23 | Dump the database before a migrating control-plane replacement | #364 | #364 r1 (pending its comment) | pending #364 |
| 24 | A migrating update is refused when the dump cannot be taken | #364 | #364 (pending its comment); mockup `update-space.png` / `update-refused.png` ([#354][354a]) | pending #364 |
| 25 | My own Postgres: confirm a backup before a migrating update | #364 | #364 r6 (pending its comment): refused `backup_unconfirmed`, then accepted with the confirmation | pending #364 |
| 26 | A failed migrating update prints one `restore` command | #364 | #364 r2, r3 (pending its comment); console restore card | pending #364 |
| 27 | Never an older control plane against a newer schema | #363, #364 | [#363][363] `422 release_below_schema_version`; #364 r6d: `restore --to` refused against a dirty, newer schema with nothing changed (pending its comment) | live (pending #364) |
| 28 | A failed agent update is restored automatically | #360 | [#360][360] forced-unhealthy image on both GPU hosts: `restored=true`, same container id, `auto_revert` row | live |
| 29 | A control-plane update that never started is restored automatically | #363 | [#363][363] §2: a control plane whose health always fails is restored after 71.9 s, reason `unhealthy` | live |
| 30 | Revert an agent from the console | #360 | [#360][360] row 3 | live |
| 31 | A host below the floor says "must update before it can be managed" | #365 | [#365][365]: planner, `409 host_not_eligible / below_floor`, console states and screenshots. One live observation, on a separate owned combined install (not a test host), 2026-09-26: its node agent stamped `0.3.1-dev.edge1` read "must update before it can be managed" under the `0.4.0-0` floor; a developer apply of the same commit stamped `0.4.0-dev.edge2` (control-plane step, then agent step, both succeeded) cleared it and `below_floor` went false. Lab hosts on `0.3.1-dev.*` stamps read the same after #365, as expected | containerized, one live observation |
| 32 | Every attempt ends in a stated outcome | #360, #362, #363 | [#360][360] all four outcomes seen live; [#362][362] fault matrix; [#363][363] §3 | live |
| 33 | An interrupted update is finished after the actor, the daemon or the machine restarts | #360, #362, #363, #364 | actor and daemon restarts: [#360][360] rows 5–7, [#362][362] a1–b, [#363][363] §3, #364 r4/r5. Machine restart: **skipped by owner decision** (row 3g) | live, except the reboot |
| 34 | Quasar never retries a failed update on its own | #360, #363 | [#360][360] settle table; [#363][363] §3 `d84ce147`: "It is not retried: apply again to try again" | live |
| 35 | The recovery actor replaces itself safely | #362 | [#362][362] actor-first order, the full fault matrix, `seed.json` names the new actor | live |
| 36 | A failed recovery-actor update leaves the previous actor running | #362 | [#362][362] d, and after the follow-up: no agent, `restored: true`, the previous actor back, one actor | live |
| 37 | Unattended updates: agent, actor and non-migrating control plane only | #363, #364, #365 | the auto-apply rules are containerized (DB tests); #364: unattended runs never migrate (pending its comment). No published release, so no live unattended run | containerized |
| 38 | A non-migrating control-plane update keeps sessions streaming | #363 | [#363][363] §4; bench `ad4ba5a2-d923-4d7a-b40b-744dfe12831c` | live |
| 39 | An agent update states that it ends that host's sessions | #360 | [#360][360]: the console says so; step 8 shows the session ending | live |
| 40 | Refuse to act on a Quasar-looking container Quasar did not create; raise a readiness fault | #366 | [#366][366] row 1 (a leftover Compose agent) and row 7 negative (a stack-defined agent): reported, `owner_conflict`, never touched; [#362][362] c, c3 (a stray actor exits) | live |
| 41 | The console says when another owner's container blocks a release | #366 | [#366][366] console row 1: warning, "In the way" row, preflight `blocked`, developer apply `409` | live |
| 42 | Remove a GPU host from the console | #366 | [#366][366] rows 3 and 4, forced failure then Retry | live |
| 43 | Uninstall keeps my data unless I purge | #366, #364 | [#366][366] row 5 (combined); #364 r6e (control-only, external) | live |
| 44 | Purge needs typed confirmation and takes a final dump of an owned database | #366, #364 | [#366][366] row 5: refused without, with a wrong, and with a seed present; final `pg_dump` taken; #364 r7 | live |
| 45 | A documented way to move to the new install with my database and homes | — | **dropped** (owner, 2026-09-26): moved to #380. [#367][367] documents replacing an existing install with a fresh seed install; homes are found again under the same home root (verified live on an owned install) | dropped → #380 |
| 46 | Restore my dump before the new control plane's first boot | — | **dropped**: #380 | dropped → #380 |
| 47 | My GPU hosts re-enroll under their existing names | #366 (owned hosts only) | For owned hosts, [#366][366] re-adds onto the same host row. For hosts of a pre-RH-06 install: **dropped**, #380 | live (owned); pre-RH-06 dropped → #380 |
| 48 | An older install is not offered RH-06 as an in-place update | #365, #367 | [#365][365] v0.3.0's release readers (`internal/platform/prerh06/`) find nothing in a v2-only release; [#367][367] format-1 publication stopped | containerized |
| 49 | Edge does not offer RH-06 builds to a pre-RH-06 install | #365 | [#365][365] `o2-<branch>` edge tags are not resolved by the pre-RH-06 readers | containerized |
| 50 | Install-time settings changed with one `reconfigure` command | #366 | [#366][366] row 6 on a GPU host: home root, trust. **Partial:** on a combined or control-only machine, `reconfigure` refuses any input the control plane renders (home root, public host, ports, database mode, trust), because it does not yet drive control-plane replacement (`docs/configuration.md`, "reconfigure"). Follow-up #386 | partial → #386 |
| 51 | Agent settings stay in the existing host policy | #361 | [#361][361] decisions: machine inputs are install-time only, console settings stay in the database; [#366][366] `reconfigure` refuses the node name and the agent image | containerized, plus live refusals |
| 52 | Release signature and allowlist rules behave exactly as today | #356, #365, #367 | [#356][356] 282 golden vectors from Go's own output, passed by both implementations, Opus security review APPROVED; [#365][365] four new vectors for the v2 pair; [#367][367] Go originals deleted only after that. Live `namespace_rejected`: [#363][363] refusals | containerized, plus live refusals |
| 53 | Only the control plane may ask for a control-plane replacement; the agent only its host's agent and actor | #356, #360, #363 | [#356][356] agent-socket component guard; [#360][360] "The agent can name only the agent and the recovery actor"; [#363][363] §1 status scoping by socket | live and containerized |
| 54 | The last three pre-update dumps are kept | #364 | #364 r1 (four migrating updates, pending its comment) | pending #364 |
| 55 | See per machine whether the database is Quasar's or mine | #361 | [#361][361] console: "Quasar's own" / "Your own · reachable" | live |
| 56 | The Compose-from-source workflow stays for fast development loops | #367 | [#367][367] documented as contributor tooling; the throwaway contributor-lane control planes used for [#357][357], [#358][358] and [#360][360] | live (as used), docs |
| 57 | Apply a branch build by digest from an allowlisted namespace | #360 | [#360][360], [#362][362], [#363][363], [#366][366]: every live replacement ran as a developer apply | live |
| 58 | The Compose updater, its image, volume, preflight checks and label discovery are removed | #367 | [#367][367] deletion list and the `git grep` audit | containerized |
| 59 | Every container shape is declared by a recipe revision the actor carries | #357, #361, #365 | [#357][357] recipe label and golden rendered specifications, matched live on both hosts; [#361][361] agent recipe revision 2; [#365][365] control-plane recipe revision 2; ADR 0008 | live and containerized |
| 60 | The release process refuses a release whose floor or recipes would strand a machine | #365 | [#365][365] `check-release-compatibility.sh`, four refusal cases | containerized |
| 61 | The seed's contract is frozen and tested against every released actor | #353, #358 | ADR 0007; [#358][358] `seed_contract.rs` over the first fixture set; `testdata/recovery/seed/actors/` (append-only). No actor has been released yet, so the set is `rh06-06` plus `unreleased` | containerized |
| 62 | My session is unaffected by actor and non-migrating control-plane updates | #362, #363 | bench `b35fbdec-3866-4322-b0a6-d7e3aeccd626` (actor hand-over), `ad4ba5a2-d923-4d7a-b40b-744dfe12831c` (control-plane update) | live |

## Gaps and items for the owner

Each item here is a gap against a user story, not a failed #368 row. Where a new defect is
needed, it is filed against its slice rather than fixed in #368.

1. **Story 50, `reconfigure` on a control-plane machine (partial, #366).** It refuses every
   input the control plane renders until `reconfigure` drives control-plane replacement
   (#363). A combined or control-only operator cannot yet change the home root, public host,
   ports or database mode in place. It is a limit #366 documented, not a defect in its
   acceptance, and is filed as #386.
2. **Story 20 and 37, the release channel on hardware.** Every live replacement ran as a
   developer apply. The Releases-channel apply of a format-2 release, and an unattended
   update, are containerized only, because nothing has been published; publication needs
   the owner's approval.
3. **Story 31, `below_floor`**, is containerized, plus the one live observation on a
   separate owned install recorded in the map.
4. **Story 33, a machine reboot mid-attempt**, is skipped by owner decision (row 3g).
5. **Stories 45–47 and row 4b** are dropped by the owner's decision of 2026-09-26 and are
   #380.
6. **#364's rows** (22–27, 54, and #368 rows 2c, 3d, 3f, 4a) wait for #364's closing
   evidence.

## Open follow-ups

| Issue | What |
|---|---|
| #378 | Steam warm-up stays deferred on a host that already has the Steam image |
| #379 | Forgetting a host leaves its users' homes stuck in `home_conflict` |
| #380 | Restore a pre-RH-06 Compose dump into a fresh owned install (the dropped row) |
| #381 | A recovery actor stopped by the operator mid-replacement leaves the machine with no control plane |
| #382 | Small follow-ups from the control-plane replacement live run (#363) |
| #384 | Library scan does not fetch artwork for the games it detects |
| #385 | Add host serves the install-time seed image after the actor is moved by developer apply |
| #386 | `reconfigure` cannot change control-plane inputs on a combined or control-only machine (story 50) |

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
[365]: https://github.com/accreleus/quasar/issues/365#issuecomment-5845199594
[366]: https://github.com/accreleus/quasar/issues/366#issuecomment-5845894834
[367]: https://github.com/accreleus/quasar/issues/367#issuecomment-5846075742
