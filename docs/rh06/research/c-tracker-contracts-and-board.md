# RH-06 research — Scope C: tracker, board and frozen contracts

Research date 2026-09-24. Repo `accreleus/quasar`, `develop` at `32aee45`; `protocol/` submodule
pinned at `3bfce4a` ("RH05 #346: add optional saved flag to typed host policy groups").
Read-only: no issue, label, board or git state was changed.

Conventions: **[F]** = confirmed fact, with its source (issue + comment author/date, or
file + section). **[I]** = inference or recommendation.

---

## 1. RH-06 parents and the initiative

### #207 — Resilient self-hosted deployment (initiative, OPEN, no milestone, `enhancement`)

- [F] Agreed target, verbatim bullets that bind RH-06 (#207 body):
  - "Explicit single-writer ownership for CP, host agent, Postgres and TURN. **A restart policy is not
    an image updater. Remove Compose-dependent updating only after a recoverable replacement and
    installation migration exist.**"
  - "Postgres owns policy; durable host identity/recovery state still exists locally.
    Desired/applied generations and readiness are separate."
  - "Retain ADR 0002, review expand/contract migrations now, and define CP/host/worker
    compatibility windows. No universal zero-disruption release claim."
  - "Docker API first behind a typed Quasar runtime interface; Podman and rootless are
    capability-tested profiles".
  - Optional STUN/TURN: "direct access, external relay or managed coturn".
- [F] Integration and release contract (owner override for the initiative): branch each unit from
  `initiative/resilient-host-architecture`, merge back into it; no automatic merge to `develop`;
  promotion to `develop` only after explicit owner approval; `main`/tags/images/deploys are
  separate operator actions. "No release or remote mutation is authorized by this issue."
- [F] "Preserve and link existing outcomes #131, #104, #173 and #185 rather than reopen or
  duplicate them. Reconcile their shipped changes before implementation." Coordinate private
  registries with #9 and runtime-preset concerns with #178/#179. Outstanding evidence #158,
  desktop acceptance #174, Intel #126/#149, cordon issues #183/#206 "remain independently tracked".
- [F] Excluded: Kubernetes, CP high availability, universal rootless compatibility, portable
  homes / live GPU migration.
- [F] Ordering: "RH-01 through RH-06 are broadly ordered … RH-08 external TURN can proceed
  independently; managed TURN depends on service ownership."
- [F] Status: RH-01 promoted to develop at `b134085` (body, 2026-09-17). RH-02 landed on develop at
  `fcaaae9` (comment salty2011 2026-09-20T15:17). RH-05 complete, #333 and #334–#346 closed,
  promoted in `32aee45` (comment salty2011 2026-09-24T14:21). RH-03/RH-04 not started.
- [F] Increment checklist in the body lists RH-06 as `- [ ] #218` and `- [ ] #219`.
- [F] Native sub-issues of #207: #208–#224 (includes #218, #219). `#218` and `#219` report
  parent `#207` via the sub-issues API.

### #218 — Define service ownership and recoverable API-based update execution (OPEN, RH-06, `enhancement`)

- [F] Body (no comments). "Planned implementation; decompose further when the local design is
  settled. **Not marked ready-for-agent by this planning step.**"
- [F] Scope: "Specify one owner for CP, host agent, Postgres and optional TURN, including
  control-only hosts. **External manager mode must never race Quasar ownership.** A restart policy
  is not an image updater. Build **versioned service specs** and a **persisted phase journal** with
  **independent CP replacement/recovery authority**."
- [F] Acceptance: "Demonstrate interrupted pull/create/replace/health-check recovery and compatible
  agent revert. Preserve database migration restrictions/backups/restore procedure. **Define the
  host-agent self-update boundary explicitly.** Reconcile existing #104/#173/#185 work; **do not
  remove the old updater before all owned service paths are covered.**"
- [F] Dependencies: "Coordinate with RH-05"; standard initiative-branch delivery paragraph;
  "Frozen wire changes require the prescribed separate sign-off."

### #219 — Ship minimal enrollment and explicit migration from existing stacks (OPEN, RH-06, `enhancement`)

- [F] Body (no comments). Same "not ready-for-agent" note. "**Depends on #218.**"
- [F] Scope: "Provide resumable/idempotent **one-command or manager-template bootstrap using minted
  enrollment tokens**. Preserve identity, homes, volumes and secrets during **opt-in adoption**.
  **Transfer resource ownership once, disable old reconciliation and verify before enabling new
  writes.**"
- [F] Acceptance: "Fresh combined-host and separate-control/GPU-host installs pass. **Validate a
  third-party manager with stack files outside Quasar storage.** Managed updates **no longer
  read/write manager .env or depend on Compose labels/paths**. Test reboot durability and failed
  adoption rollback. Remove obsolete overlays/updater paths only for supported migrated modes;
  retain recovery instructions."

### Other explicit RH-06 statements in the tracker/docs

- [F] #222 / #223 (RH-08): "managed service lifecycle depends on RH-06"; #223: "External TURN can
  ship before RH-06; managed lifecycle depends on its ownership decision." "Reuse service ownership
  design for managed lifecycle."
- [F] #333 (RH-05 spec) Out of Scope: "**Initial enrollment, device/mount changes and replacement of
  Quasar's own containers belong to RH06.**" Implementation decision 2: "Bootstrap identity,
  credentials, endpoint, TLS trust, runtime ownership and mounts are not mutable RH05 policy."
  Decision 10: configuration recovery "does not replace platform images … Preserve ADR 0002
  release ordering and ADR 0004 updater authority." User story 41: "clear remaining enrollment and
  mount requirements, so that I know what RH05 automates and what RH06 still needs to deliver."
- [F] `docs/rh05/operator-handoff.md` §"RH06 and other work still outside RH05": "RH06 owns
  first-host and additional-host enrollment automation, deployment identity, credentials,
  mount/device provisioning and Quasar platform-image replacement."
- [F] #217 acceptance: "Keep bootstrap endpoint, credentials and recovery independent of mutable
  policy."
- [F] `protocol/agent-api.md` §"Durable start, restart recovery, and database restore" (RH05):
  "Recovery cannot replace a platform image or repair a binary, runtime or mount." and "A
  supported database restore stops the stack first and starts a new boot incarnation; a live
  rewind has no guarantee."
- [F] No occurrence of "RH-06"/"RH06" anywhere in `protocol/` (grep of all files).
- [F] The phrases "Portainer", "Dockge", "external manager" appear in no issue other than via
  #218's "External manager mode" and #219's "third-party manager" / "manager-template". No
  existing issue specifies a manager integration.

---

## 2. Existing update/deploy/recovery work to reconcile

### #104 — Self-update (CLOSED 2026-09-05, `enhancement,ready-for-agent`; board Shipped/Outcome/Released)

- [F] Shipped: component identity stamping; stable/edge detection job (`platform.release_detect`,
  weekly); release plan as pure function (ADR 0002 ordering by schema version); fleet apply state
  machine (control plane first, then hosts sequentially with cordon + drain); per-host apply;
  revert; **per-host updater sidecar** (`quasar-updater`, Go, docker CLI + compose plugin, unix
  socket in a shared named volume, namespace allowlist from the host `.env`, digest-only).
  Children #105–#119. Migrations 0074/0075. Protocol amendments 1 and 2.
- [F] The updater design is **Compose-coupled by construction** (#104 comment salty2011
  2026-09-04, #113 prototype findings): "The sidecar reads its own container labels
  (`com.docker.compose.project`, `.project.config_files`, `.project.working_dir`) … reconstructs
  `docker compose -p … --project-directory … -f …`; the env file is `<working_dir>/.env`. … requires
  the stack directory to be mounted into the sidecar at the same absolute path as on the host."
  Env rewrite touches only `QUASAR_CONTROL_IMAGE` / `QUASAR_AGENT_IMAGE`, keeps `.env.prev`.
  Commands: `pull` then `up -d --force-recreate --no-deps --wait`.
- [F] Prototype finding 5: compose removes the old container before starting the new one; a failed
  `up` leaves the service `Created`/stopped; control-plane self-restore allowed only when
  `StartedAt` is zero (no migration can have run).
- [F] Out of scope in #104: signatures, beta, unattended apply, notifications, app-image updates,
  applying to source-built hosts, automatic rollback. All of #120–#123 were later implemented
  (closed 2026-09-10); #155 and #156 remain open follow-ups.
- [F] Live evidence: `docs/reports/2026-09-05-self-update-live-gate/`; #119 closing comment
  (salty2011 2026-09-08) and board note: operator-confirmed stable 0.2.4 → 0.2.5 console update.
- [F] "Every existing install … lacks the updater until an operator adds it once" (#104 Further
  Notes) — precedent for a one-time manual migration step.

### #173 — Deployment and update reliability (CLOSED 2026-09-12, `bug,needs-triage`; board Shipped/Outcome/"Implementation + validation")

- [F] Children #169, #170, #175, #176, #177, #185 (native sub-issues). Closing comment (salty2011
  2026-09-12): all finish-line boxes checked; evidence `docs/superpowers/plans/2026-09-12-173-live-evidence.md`
  on develop, and PR #196. Live on a **registry install on the edge channel**, images published by
  the Images workflow, applied by digest: skip-then-continue (#169), failed run leaves offline host
  alone (#170), adoption across CP recreate, #153 session streaming through a non-migrating fleet
  apply at 60 fps, #122 unattended refusal of a migrating release.
- [F] "A host-agent update remains explicitly disruptive (that host's sessions end) while a
  compatible control-plane-only apply is ridden through."
- [F] Carried: #144 retirement half (on #158); filed #199, #200, #201 (all since closed); #183 open.

### #185 — Self-update hardening (CLOSED 2026-09-13 on release v0.3.0, `enhancement,ready-for-agent`)

- [F] Sub-issues #186 (amendment 9), #187 (preflight), #188 (automatic revert of a failed host
  step — ADR 0004), #189 (readiness stack-conformance: `updater_socket`, `updater_stack_dir`,
  `health_addr_bindable`), #190 (`succeeded_partial` + retry skipped). Also #184. Landed on develop
  `2196ab2`, amendment 9 on quasar-protocol `8a6aed2`, migration 0083 (comment salty2011
  2026-09-12T04:38). Released in v0.3.0 (comment 2026-09-13).
- [F] Motivating incidents table (body): #140, #153 review, #169/#170, #184, #152 field case — all
  "environmental shapes that a unit test cannot see". Two external installs "were on hand-repaired
  compose files" (#187 body).
- [I] Preflight checks `updater_stack_dir` and `updater_overlays` are Compose-specific facts. RH-06's
  "no Compose labels/paths" acceptance on #219 makes these checks obsolete for migrated modes; they
  must remain for legacy (unmigrated) stacks.

### #128 — Sessions survive a control-plane restart (CLOSED 2026-09-08)

- [F] Three actors killed sessions (agent on WS drop, CP on re-register, browser on signaling
  close) — comment salty2011 2026-09-07. Fixed in three increments; agent holds sessions for a 90 s
  grace (`sessions-held-for-grace` → `session-grace-cleared` → `sessions-survived-reconnect`).
  Live gate PASS (comment 2026-09-08T12:43), report `docs/reports/2026-09-08-128-session-survival-gate/`.
  Rendering caveat later resolved (comment 2026-09-09: luma probe blind to sparse "Ball" content).
- [F] Prose-only contract amendments to `signaling.md` and `agent-api.md` (no wire change).
- [I] Relevance: a CP replacement under RH-06 can keep sessions alive **only** within this grace
  window; a new CP-replacement mechanism must keep outage below the agent's grace (~90 s) or
  sessions will drop.

### #153 — Fleet update no longer drains the whole fleet before the CP step (CLOSED 2026-09-10)

- [F] Non-migrating CP step does not drain; a migrating one still drains, because "every migration
  in this repo was authored under 'no session is live while I run'" (comment salty2011 2026-09-08).
  `platform.ReleaseRunsAMigration`. Amendment 6 (quasar-protocol `05c2ace`, sign-off 2026-09-09)
  plus served `migrates` field. Force on migrating release now force-drains then waits for zero.
- [F] Live gate met later under #173/#158 (comment on #158 2026-09-12).

### #9 — Private/credentialed registries (OPEN, `enhancement,ready-for-human`, no milestone; board Later/Implementation/Planned)

- [F] Digest resolver fetches anonymous pull tokens only; plan: per-registry credentials in
  `instance_secrets`. Triaged 2026-09-04 as roadmap, needs operator prioritisation.
- [I] RH-06 touches it only if platform images or the new executor must pull from a private
  registry (fork namespaces are already covered by `QUASAR_UPDATER_ALLOWED_NAMESPACES`). Keep as a
  dependency reference, not scope.

### Enrollment lineage (all CLOSED)

- [F] #12 (multi-host registration UX + secure agent transport), #96 (shared token could take over
  an enrolled host), #100 (one-line `enroll-host.sh` installer + modal; the `qenr1.` enrollment
  string), #127 (installer compose lacked updater volume), #199 (re-enrol loops on stale node
  secret; fixed with `cp-register-stale-identity` fallback and pin rotation, live-verified on edge
  `eaf042c`, comment 2026-09-13).
- [F] #146: startup sweep now only removes containers carrying this agent's persisted ownership
  identity (v0.2.4). #148: quick-start installing into ramdisk on Unraid lost `.env` secrets at
  reboot; installer now refuses a second stack deployed from elsewhere (v0.2.5).
- [F] #131: first install brittle because "the compose file is treated as the source of truth for
  facts that belong to the image" — delivered in v0.2.4; board Shipped/Outcome/Released.
- [F] #277, #279 (docs, closed 2026-09-20 on the initiative branch): quick start never set the
  updater image or stack dir; a second install on one engine needs `COMPOSE_PROJECT_NAME`,
  ports, `QUASAR_HEALTH_ADDR`, own home/template/stack roots; the agent runtime directory "cannot be
  separated".

---

## 3. Related issues (search results, filtered)

Searched `--state all` for: updater, update, upgrade, self-update, release, compose, Unraid,
Portainer, Dockge, manager, enrollment, enroll, bootstrap, install, recovery, rollback, restore,
backup, migration, adopt, redeploy, identity, token, TURN, coturn (210 unique hits). Portainer and
Dockge: **zero hits**. Noise hits (UI, codec, flakes) omitted. No milestone unless stated.

| # | Title (short) | State | Milestone | Labels | RH-06 relevance | Recommended disposition |
|---|---|---|---|---|---|---|
| 207 | Resilient self-hosted deployment (initiative) | OPEN | — | enhancement | Parent; binding target statements | Parent — update checklist only on RH-06 completion |
| 218 | Service ownership + recoverable API-based update execution | OPEN | RH-06 | enhancement | RH-06 parent A | Reuse as parent; decompose into child tickets |
| 219 | Minimal enrollment + explicit migration from existing stacks | OPEN | RH-06 | enhancement | RH-06 parent B (depends on #218) | Reuse as parent; decompose |
| 104 | Self-update: detect/surface/apply | CLOSED | — | enhancement, ready-for-agent | The Compose-sidecar updater RH-06 replaces | Reconcile (preserve; do not reopen) |
| 105–119 | #104 children (identity, publish, detect, updater, apply, revert, live gate) | CLOSED | — | enhancement, ready-for-agent | Code/contract being superseded | Reconcile |
| 113 | Prototype: updater sidecar | CLOSED | — | enhancement | Source of every Compose coupling decision | Reconcile (design input) |
| 120 | Release signature verification | CLOSED | — | enhancement, needs-triage | New executor must keep signature modes (ADR 0003) | Reconcile |
| 121 / 123 | Beta channel / release notifications | CLOSED | — | enhancement, needs-triage | Detection side unaffected | Out of scope (keep working) |
| 122 | Unattended automatic apply | CLOSED | — | enhancement, needs-triage | New executor must honour unattended + `carries_migration` refusal | Reconcile |
| 128 | Sessions survive CP restart | CLOSED | — | bug, ready-for-human | Grace window bounds CP replacement outage | Dependency (existing capability) |
| 131 | First-time deployment brittle | CLOSED | — | bug, needs-triage | Installer/image-owns-facts precedent | Reconcile (preserve outcome) |
| 140 | Fleet run adopted from v0.2.0 leaves hosts draining | CLOSED | — | bug | Legacy-adoption hazard pattern for any run-record migration | Reconcile (lesson) |
| 146 | Second agent deletes another agent's containers | CLOSED | — | bug | Ownership identity for containers — base for resource ownership transfer | Reconcile |
| 148 | Quick-start installs to ramdisk; secrets lost | CLOSED | — | bug | Reboot durability; secrets custody; refuse-second-stack rule | Reconcile |
| 152 | Two agents answer each other's health checks | CLOSED | — | bug | Multi-stack-on-one-engine hazard | Reconcile |
| 153 | Fleet no longer drains before CP step | CLOSED | — | enhancement | Migration drain rule to preserve | Reconcile |
| 155 | Release-signing review follow-ups | OPEN | — | enhancement | Touches updater's signature fetch; will move if updater is replaced | Reconcile (fold or keep, decide explicitly) |
| 156 | Validate webhook hosts at save time | OPEN | — | enhancement | Unrelated to ownership | Out of scope |
| 157 | `manifest_invalid` fault contract/code mismatch | OPEN | — | bug | Release-view contract drift; decision recorded (correct the contract) | Out of scope (independent contract fix) |
| 158 | Live gates outstanding (#144 driver identity) | OPEN | — | — | Only #144 retirement left | Out of scope (independently tracked per #207) |
| 169 / 170 / 175 / 176 / 177 | Fleet-apply reliability fixes | CLOSED | — | bug | Behaviour the new executor must not regress | Reconcile |
| 173 | Deployment and update reliability parent | CLOSED | — | bug, needs-triage | Named in #218 acceptance | Reconcile |
| 183 | A cordon carries no owner | OPEN | — | bug | Likely superseded by RH05 owner-scoped restrictions (#337, migration 0088) | Reconcile — verify against #337 and close or narrow (#207 says tracked independently) |
| 184, 186–190 | Self-update hardening children | CLOSED | — | bug / enhancement | Preflight vocabulary is Compose-specific (`updater_stack_dir`, `updater_overlays`) | Reconcile |
| 185 | Self-update hardening parent | CLOSED | — | enhancement, ready-for-agent | Named in #218 acceptance | Reconcile |
| 191 / 192 | Register probes vs handshake window / reuse register prep | CLOSED / OPEN | — | bug / enhancement, ready-for-agent | `discover_install` feeds `install_mode`/`updater_present`; #192 affects reconnect within #128 grace during CP replacement | #192: dependency/adjacent, keep separate |
| 193 | Agent replays historical updater results on reconnect | CLOSED | — | bug, ready-for-agent | Result-relay behaviour of the old path | Reconcile |
| 199 | Re-enrolling on a stale node secret | CLOSED | — | bug, needs-triage | Enrollment/identity edge cases for adoption | Reconcile |
| 200 / 201 / 202 | Host-only run cordons; double-failure timeout; follow-ups | CLOSED | — | bug / needs-triage | Relay path depends on agent; second relay path called "probably out of scope" in #201 | Reconcile (#201 "second relay path" is an RH-06 question when CP replacement authority moves) |
| 205 | `make test-db` leaks Postgres volumes | OPEN | — | bug, needs-triage | Tooling | Out of scope |
| 206 | cordonFleet proceeds with no cordon on read failure | OPEN | — | bug, needs-triage | Fleet-run cordon residual; may be moot after #337 owned restrictions | Reconcile — check against #337 |
| 9 | Private/credentialed registries | OPEN | — | enhancement, ready-for-human | Coordinate per #207; pulls of platform images | Dependency / out of scope |
| 12 / 96 / 100 / 127 | Enrollment tokens, installer | CLOSED | — | various | The existing enrollment path #219 extends | Reconcile (reuse minted tokens; do not duplicate) |
| 130 | Driver-volume host path resolution via self-inspection | CLOSED | — | bug | Mount/device provisioning (RH-06 owns per handoff) | Reconcile |
| 208 / 209 | RH-01 runtime interface + Docker API | CLOSED | RH-01 | enhancement | API-based execution substrate for #218 | Dependency (done) |
| 210 / 211 / 252–265 | RH-02 readiness + fresh-install evidence | CLOSED | RH-02 | enhancement | Fresh-install evidence precedent; readiness card for conformance | Dependency (done) |
| 277 / 278 / 279 | Quick-start docs defects | CLOSED | — | documentation | Compose-install friction RH-06 aims to remove | Reconcile (docs to rewrite) |
| 212 / 213 | RH-03 media workers | OPEN | RH-03 | enhancement | Workers change what a host-agent update disrupts | Out of scope (sequencing input) |
| 214 / 215 | RH-04 authenticated worker adoption; pinned session versions + compatibility windows | OPEN | RH-04 | enhancement | Defines when an agent update can be non-disruptive; compatibility windows #218 must respect | Dependency (not a prerequisite unless RH-06 claims restart-safe agent updates) |
| 216 / 217 / 333–346 | RH-05 desired host state | CLOSED | RH-05 | enhancement / ready-for-agent | Owner-scoped restrictions, execution journal, boot incarnation, ADR 0006 | Dependency (done); reuse patterns |
| 220 / 221 | RH-07 Podman/rootless | OPEN | RH-07 | enhancement | #113 found rootless Podman needs `label=disable` + socket path; ownership design must not assume Docker-only | Out of scope (design constraint) |
| 222 / 223 | RH-08 TURN media policy + managed coturn | OPEN | RH-08 | enhancement | Managed coturn lifecycle depends on RH-06 ownership decision | Downstream consumer — design TURN as one owned service |
| 331 / 332 | Perf tuning / portable homes | OPEN | — | enhancement, needs-triage | Excluded | Out of scope |

Overlaps/duplicates to reconcile:
- [I] #218 "compatible agent revert" vs shipped #118 revert + #188/ADR 0004 automatic restore:
  the new executor needs the same guarantees, not a second revert concept.
- [I] #218 "persisted phase journal" vs existing `platform_apply_runs` / `platform_apply_attempts`
  (migration 0075, amended by 0076/0083/0084) and RH05's agent-side execution journal: decide
  extend-or-replace explicitly; the tracker has no decision yet.
- [I] #219 "minted enrollment tokens" is already shipped (#12/#96/#100); #219 should reuse
  `host_enrollments` rather than add a second token model.
- [I] #183 and #206 predate RH05's owner-scoped restrictions (#337, migration 0088); both may be
  resolved or reshaped by it. #207 keeps them "independently tracked", so only recommend a check.
- [I] #201's "second relay path that does not depend on the host's agent" becomes relevant if
  #218 gives the control plane independent replacement/recovery authority.

---

## 4. Milestones

| # | Title | State | Open/closed | Open issues |
|---|---|---|---|---|
| 1 | RH-01 Runtime API foundation | closed | 0/19 | — |
| 2 | RH-02 Probe-first host readiness | closed | 0/26 | — |
| 3 | RH-03 Per-session media workers | open | 2/0 | #212, #213 |
| 4 | RH-04 Restart-safe adoption and mixed versions | open | 2/0 | #214, #215 |
| 5 | RH-05 Versioned desired host state | closed | 0/16 | — (#216, #217, #333–#346 closed) |
| **6** | **RH-06 Deployment and update ownership migration** | **open** | **2/0** | **#218, #219** |
| 7 | RH-07 Podman and rootless support expansion | open | 2/0 | #220, #221 |
| 8 | RH-08 Optional STUN/TURN connectivity | open | 2/0 | #222, #223 |

- [F] Milestone 6 description: "Remove manager-owned Compose file coupling only after recoverable
  replacement paths are proven. Integration: initiative/resilient-host-architecture. Owner-approved
  promotion to develop; no due date or release authorization."
- [F] RH-04 (#214): "Persist host identity and minimal recovery metadata. Authenticate local worker
  reattachment … **Labels alone cannot authorize adoption.**" SIGKILL and graceful host-agent
  restart must preserve active media. Depends on RH-03 coordination.
- [F] RH-04 (#215): "Declare CP/host/worker compatibility windows and classify updates requiring
  drain. Establish expand/contract migration review and delayed contraction while old consumers
  exist." "Preserve ADR 0002 and no unsafe CP downgrade."
- [I] RH-06 without RH-04 can still ship, but host-agent updates stay "explicitly disruptive" (drain
  first), which matches #173's stated asymmetry. RH-06 should not claim restart-safe agent updates.
  The #218 "host-agent self-update boundary" is where to say this explicitly.
- [I] #214's "labels alone cannot authorize adoption" is a useful principle for #219's adoption of
  existing Compose stacks too (Compose labels are the current ownership evidence).

---

## 5. Project board (org project #1, "Quasar roadmap", private)

### Fields (from `gh project field-list 1 --owner accreleus`)

| Field | Type | Options |
|---|---|---|
| **Status** | single select | `Now`, **`Next`**, `Later`, `Shipped` |
| **Level** | single select | `Outcome`, **`Implementation`** |
| **Evidence** | single select | **`Planned`**, `Needs live evidence`, `Needs reconciliation`, `Exploratory`, `Released`, `Implementation + validation` |
| Title, Assignees, Labels, Linked pull requests, Milestone, Repository, Reviewers, Parent issue, Sub-issues progress, Created, Updated, Closed | built-in | — |

- Project and field node ids are operator working data and are kept out of this repository.
- [F] So "Next / Implementation / Planned" means **Status=Next, Level=Implementation,
  Evidence=Planned**. There is no field called "Type", "Phase" or "State".
- [F] Project readme: "The field named Status carries the roadmap horizon (Now/Next/Later/Shipped),
  not a New/In progress task workflow. Horizon is priority, while Evidence distinguishes released
  work, merged work awaiting live checks, planned outcomes and exploration."

### Current RH items (GraphQL field-value timestamps)

- [F] #218, #219: Status=Next, Level=Implementation, Evidence=Planned, all set 2026-09-13T15:48
  (same as #208–#223 when the initiative was planned). #207: Now/Outcome/Planned.
- [F] RH-01 children #224–#240 were added 2026-09-14 as **Now**/Implementation/Planned; Evidence moved to
  "Implementation + validation" on some as they landed.
- [F] RH-05 precedent: #333–#346 (and #331/#332 as Later) were added 2026-09-23T07:48–07:49 with
  Level=Implementation and Evidence=Planned. The scratch manifest used to publish them
  (`.scratch/rh05/board.json`) records `"horizon": "Next"` for #333–#346 and `"Later"` for
  #331/#332. On closure (2026-09-24T14:21) Status became **Shipped**; Evidence was advanced
  per-ticket during delivery (e.g. #335/#336/#341/#342 → "Implementation + validation", #338 →
  "Needs live evidence"), others left "Planned".
- [F] #333's closing comment noted board placement had once been blocked by "the current
  credential's missing project access"; the items were nonetheless added the same morning.

---

## 6. RH-05 ticket-writing precedent

### Spec issue #333 (labels `ready-for-agent`, milestone RH-05, closed)

- [F] Structure: `# RH05 specification — …`, then a status line, then sections **Problem
  Statement**, **Solution**, **User Stories** (41 numbered "As a …, I want …, so that …"),
  **Implementation Decisions** (17 numbered, bold-titled), **Testing Decisions** (bullets),
  **Out of Scope**, **Further Notes** (research baseline commits for quasar and protocol; ADRs in
  force; publication rules: "publish one issue per slice in dependency order and add native
  blocking relationships where supported. Parent issue bodies and status remain untouched.").
- [F] Not a native sub-issue of anything (parent API 404); linked by text only.

### Child tickets #334–#346 (titles `RH05-NN: <imperative>`)

- [F] Header block (bold labels, one line each):
  ```
  **Tier:** Tier 2 — bounded implementation against approved contracts
  **Depends:** #334
  **Scope:** …
  **Acceptance:** all checkboxes below plus the required evidence.
  **Status:** ready-for-agent. Breakdown approved by the owner. Blockers and frozen-contract sign-off remain mandatory.
  ```
  Tier vocabulary used: "Tier 2 — bounded implementation against approved contracts",
  "Tier 3 — state, concurrency or cross-runtime safety", "Tier 3 — contract/architecture review"
  (#334). #334's Depends: "None (can start contract preparation immediately; sign-off gates
  completion)".
- [F] Body sections: **Parent** ("#216; initiative #207; RH-05 milestone. Specification: #333."),
  **What to build** (one short user-visible paragraph), **Acceptance criteria** (checkboxes),
  **Blocked by** (issue list), **Required checks and evidence** (e.g. "make verify; make test-go;
  make test-db; make test-rust; make test-web; visual verification; live session evidence …" plus
  a shared paragraph on real ephemeral Postgres, crash-injection journals, design handoff, no
  simulated hardware claims), **Delivery and coordination** (shared paragraph: integrate only into
  the initiative branch; parents untouched; changelog entry; promotion/release/deploy need separate
  authorization; frozen contract sign-off not granted by breakdown approval; "Stop at this slice's
  acceptance").
- [F] Labels: `ready-for-agent` on all except #335 (no labels). No tier labels.
- [F] **Native "blocked by" relationships were used** (REST `issues/{n}/dependencies/blocked_by`):
  #335←#334; #336←#335; #337←#334; #338←#335,#337; #339←#338; #340←#336,#339; #341←#334;
  #342←#341; #343←#342; #344←#343; #345←#344; #346←#340,#345. They match each ticket's
  **Depends** line.
- [F] **Native sub-issue links were NOT used** for RH-05 children (parent API 404 for #333–#346;
  #216/#217 have no sub-issues), though the scratch manifest records intended parents 216/217.
- [F] The contract ticket (#334) is first and blocks the others; its acceptance ends with
  "Required contract reviewer and owner sign-off are attached; dependent tickets remain blocked
  until approved contract and pin are available." Sign-off records were appended as comments on
  the implementing ticket (e.g. #337: "owner explicitly signed off on 2026-09-23 after Opus returned
  APPROVED … published in quasar-protocol `20b1ebf` (branch `rh05-contracts`)").
- [F] The last ticket (#346) is the operator-journey/acceptance-matrix ticket: "Milestone acceptance
  and operator handoff; no feature gaps deferred to this ticket, no parent closure or automatic
  promotion."

---

## 7. Labels (`gh label list`)

- [F] Planning labels present: `ready-for-agent` ("Fully specified, ready for an AFK agent"),
  `ready-for-human`, `needs-triage`, `needs-info`, `wontfix`, `duplicate`, `enhancement`, `bug`,
  `documentation`, `help wanted`, `wayfinder:map|task|grilling|research` (used by RH-01 #224–#229).
- [F] **No `needs:*` tier labels exist** in the repo. Tier is carried in the ticket's `**Tier:**`
  header line (CLAUDE.md's reference to `needs:*` labels is not reflected in the label set).
- [F] #218/#219 currently carry only `enhancement` and explicitly say "Not marked ready-for-agent by
  this planning step."

---

## 8. Frozen contracts touching RH-06

Source: `protocol/` submodule at `3bfce4a` (populated locally). ADRs in `docs/adr/`: 0001 pinned-digest
trust, 0002 release order and no downgrade, 0003 release signatures, 0004 automatic restore of a
failed agent apply, 0005 only evidence gates a launch, 0006 expire unstarted idle approvals on
control-plane boot.

### `agent-api.md`

| Section | What it fixes | RH-06 impact |
|---|---|---|
| §"Auth / enrollment" (line ~258) | enrollment_token (minted or static) + `node_name`; CP mints `node_secret`, agent persists it; enrollment onto a live `node_name` refused (#96); "End state … mTLS / SPIFFE … message shape does not change" | #219 enrollment/adoption must preserve this; any new credential form (e.g. bootstrap bundle) is a change here |
| §`register` + "Optional identity fields (platform-release amendment 1)" | `source_commit`, `built_at`, `install_mode` (`registry`\|`source`), `updater_present`; replaced wholesale; learned "from its own container and stack through the docker CLI" | `install_mode`/`updater_present` are Compose/updater-shaped; a new ownership mode (e.g. "managed", "external manager") needs a new value or field → **amendment** |
| Amendment 2 header, §`release_apply`, §`release_state` | agent relays updater result file; components always exactly `node-agent`; CP is **never** a target over this wire; closed reason vocabulary (`updater_absent`, `busy`, `invalid`, `namespace_rejected`, `digest_malformed`, `pull_failed`, `recreate_failed`, `never_started`, `unhealthy`, `updater_unreachable`, `timeout`, `unsupported`, `signature_missing`, `signature_invalid`); `restored` (amendment 9); `recreating` = "the compose `up -d --force-recreate --no-deps` is running" | Replacing the Compose executor changes the meaning of `recreating` and possibly the vocabulary; adding components (Postgres, TURN, updater itself) or phases → **amendment** |
| §"Reconnection & reconciliation"; §Transport ("disconnect = offline and its sessions are reaped") | reconciliation via heartbeat `running_sessions` (#128 noted the inconsistency) | Relevant to CP replacement outage; no change expected |
| §"RH05 host configuration execution journal (Quasar #334)" → §"Capability and connection identity", §"Durable start, restart recovery, and database restore" | capability negotiation via `config_policy_versions`; `boot_incarnation`; supported DB restore "stops the stack first"; "Recovery cannot replace a platform image or repair a binary, runtime or mount"; host-wide operation lock | Pattern to reuse for a service-ownership capability and phase journal; RH-06 platform-image replacement must coexist with the RH05 host-wide operation lock → likely **amendment** to state serialization |

### `control-api.md`

| Section | RH-06 impact |
|---|---|
| §"Host enrollment tokens (#12/#96)" (mint/list/revoke; `qenr1.<FINGERPRINT>.<b64url(wss-url)>.<token>` composed by UI; redemption rules) | #219 bootstrap reuses this; new manager-template/bootstrap endpoints would be additive → **amendment + sign-off** |
| §"Hosts (admin lifecycle, P3-01)" incl. drain/uncordon/delete; host-body amendment "platform-release identity" | Migration/adoption state on the host body → amendment |
| §"Platform releases — identity and the release read surface (amendment 1)" incl. `GET /v1/admin/platform/identity`, `GET /v1/admin/platform/releases`, "The release manifest asset"; `EligibilityReason` precedence (`install_mode_source`, `updater_absent`, …) | New ownership modes need eligibility reasons (e.g. "externally managed", "not migrated") → amendment |
| §"Platform-release apply (amendment 2)" — "The shape of an apply", fleet/per-host apply, runs, cancel, revert, attempts, `active_apply`; "The control-plane target does not use agent-api.md … applied by the updater sitting beside it"; one automatic CP restore only when never started | #218's "independent CP replacement/recovery authority" directly revises this section → **amendment + sign-off** |
| §"Platform-release beta channel (amendment 3)", §"Release notifications (amendment 4)" | Unchanged |
| §"Self-update hardening … (amendment 9)" → §Preflight (`PreflightCheckId`: `updater_socket`, `updater_stack_dir`, `updater_overlays`, `image_resolvable`, `agent_connected`, `health_addr_bindable`), §`succeeded_partial`, §`auto_revert` | Compose-specific checks; the vocabulary is "closed" → new checks for a non-Compose executor need **amendment** |
| §"Evidence-gated readiness … (amendment 11, #260)" | Readiness checks for ownership/migration state would follow this model |
| §"RH05 — desired host policy and selected apps (amendment 13)" → §"Typed host policy", §"Idle apply, cancellation and recovery", §"Active admission reasons on the existing Host response" | Owner-scoped admission restrictions — a platform apply is one owner; RH-06 must use them, not status-based cordons |
| Amendment #509 (ICE servers on `SignalingCoords`, ~line 2966) | RH-08 base; managed TURN lifecycle would reuse RH-06 ownership |

### `schema.md`

| Section | RH-06 impact |
|---|---|
| §`instance_settings` (`release_channel`, `release_edge_branch`, `platform_auto_apply`, webhook columns) | Instance-level ownership mode / migration state would be new columns → amendment |
| §`instance_secrets` | Home for any new credential (bootstrap, manager API) |
| §`host_enrollments (#12/#96)` | Reuse for #219 |
| §`hosts` + §"Host status state machine (P3-01)" | Identity columns `source_commit`, `built_at`, `install_mode`, `updater_present` |
| §`platform_releases`, §`platform_apply_runs`, §`platform_apply_attempts` (migration 0075, later amended) | #218's "persisted phase journal" either extends these or adds new tables → amendment + migration |
| §"Not frozen: the updater's local socket" | The updater's socket/result-file interface is **explicitly not frozen**; changing the local executor mechanism needs no contract change as long as `release_state` and the tables keep their meaning |
| §Migrations (golang-migrate, linear integer, embedded) | Migration restrictions #218 must preserve; ADR 0002 one-way rule |
| §"RH05-01 — host policy, idle apply and placement (#334)" → §"0088 — owner-scoped admission restrictions", §"0089 — boot fence, approvals, attempts and journal inventory", §"Transaction order and migration ownership" | Lock order and owner-scoped restrictions any RH-06 table must slot into |

### What would need contract sign-off (Opus + explicit owner)

- [I] **Needs amendment:** new `install_mode` value(s) or an ownership-mode field on `register` and the
  host body; new eligibility reasons and preflight check ids; revising the CP-target apply model
  (independent replacement authority, new phases, a phase journal visible on the admin surface);
  applying components other than `node-agent`/`control-plane` (Postgres, TURN, the updater);
  any bootstrap/manager-template/adoption endpoints; new tables/columns for service specs, journals,
  migration state; a changed meaning of `release_state.state` values (`recreating` is defined as a
  compose command today).
- [I] **Does not need amendment:** replacing how the updater executes (Compose CLI → Engine API)
  behind the unchanged `release_apply`/`release_state` wire and the unchanged tables — the local
  updater socket is explicitly not frozen (schema.md §"Not frozen"). Documentation-only changes to
  `docs/upgrading.md` / `deploy/README.md`.
- [I] Precedent: RH-05 put one contract ticket first (#334, Tier 3), blocking all implementation;
  sign-off was recorded per ticket as comments with the quasar-protocol commit on a
  `rh05-contracts` branch. RH-06 should follow the same shape.

---

## 9. Key inferences for planning

1. [I] The whole shipped self-update path (#104/#173/#185) depends on Compose: labels for discovery,
   `.env` rewrite for desired digests, `compose up --force-recreate` for replacement, and a stack
   directory mounted at its host path. #219's acceptance explicitly removes all three for migrated
   modes, so RH-06 is a replacement of the executor and of where desired service specs live (DB +
   local journal instead of `.env`), with the Compose path kept for unmigrated stacks.
2. [I] The two parents are not ready-for-agent and have no child tickets; RH-06 needs a spec issue
   (like #333) and a sliced breakdown, with a contract ticket first.
3. [I] Board entries for new RH-06 children should be Status=Next, Level=Implementation,
   Evidence=Planned (the RH-05 precedent), with native blocked-by links and no native sub-issue
   links unless the owner wants them.
4. [I] Hard constraints to carry into the spec: ADR 0001 (digest-only), ADR 0002 (CP first, never
   below DB migration), ADR 0004 (agent auto-restore), ADR 0006 / RH05 boot incarnation, #153
   migrating-release drain, #128 grace window, RH05 owner-scoped restrictions, "do not remove the
   old updater before all owned service paths are covered".
5. [I] Open questions the tracker does not answer: what "external manager mode" means concretely
   (the only phrase is #218's); which party owns Postgres upgrades (none of the shipped paths touch
   Postgres images); whether the updater becomes a Quasar-owned service updated by the new
   executor; how a control-only host (no agent) is enrolled and updated; what backup is taken before
   a migrating CP replacement ("Preserve database migration restrictions/backups/restore
   procedure" in #218). [F] What exists today is a manual, documented procedure, not an automated
   pre-apply backup: `docs/upgrading.md` (manual `pg_dump --format=custom` / `pg_restore` recipe),
   `docs/operations/database-backup-restore.md`, and the drill script
   `deploy/db-backup-restore-drill.sh`; RH05 adds "a supported database restore stops the stack
   first". [I] Whether RH-06 automates a backup before a migrating CP step is undecided on the
   tracker.
