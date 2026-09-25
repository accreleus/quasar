# RH-06 product decisions

Date: 2026-09-24. Grilling record for RH-06 (#218, #219; initiative #207), grounded in
[`2026-09-24-research.md`](2026-09-24-research.md). Each entry records the question, the
options put to the owner, the recommendation, and the owner's answer. "Owner" is the
project owner answering in the shaping session; an entry is binding for the RH-06
specification only once it reads **Agreed**.

Frozen-contract consequences are flagged per entry; agreeing to a decision here is not
contract sign-off, which stays a separate gate (Opus review + explicit owner approval).

---

## D1. Which platform services Quasar owns

**Question.** Which platform services does RH-06 make Quasar the service owner of?

**Options.**
- (a) Control plane + node agent only (today's release scope, new executor).
- (b) Control plane, node agent, the recovery actor itself, and the Postgres container
  within one major version; an operator-supplied external database stays external; TURN
  gets a slot in the service model but no implementation.
- (c) (b) plus managed TURN now (pull RH-08 #223 forward).

**Recommendation.** (b). It is the smallest set that makes "one owner per machine" true:
without Postgres, the manager's `.env` stays the owner of the service whose loss costs
everything; without the recovery actor, the "updated by hand, tag-named, unversioned"
gap remains. Major Postgres upgrades need dump/restore and are a separate risk class.

**Agreed (owner, 2026-09-24): (b).**

- Quasar owns: control plane, node agent, the recovery actor, and the Postgres container
  it installed. Postgres moves only to digests a release names **within the same major
  version**; a major-version change is refused and out of scope.
- A Postgres reached through an operator-supplied `DATABASE_URL` / `QUASAR_DATABASE_*`
  is external and never touched.
- TURN: the service model has a place for it; no implementation in RH-06 (RH-08 consumes).
- Contract consequence: the release manifest grows beyond exactly two components
  (`format_version` bump), and `release_apply` / apply attempts gain components. Both
  are frozen surfaces → sign-off gate.
- Design constraint found while recording this: a control plane rejects any manifest
  whose `format_version` is not 1 (`control-plane/internal/platform/manifest.go:21-23`),
  so a release that *only* published a format-2 manifest would be invisible to every
  installed control plane, and the console could never offer the release that brings
  RH-06. The additional components must reach old control planes without breaking the
  format-1 asset (for example, a format-1 manifest kept alongside a second asset). The
  architecture document resolves how.

---

## D2. Supported host shapes

**Question.** Which host shapes does RH-06 support for fresh Quasar-owned installs?

**Options.** (a) combined, control-only and GPU hosts, one installation per container
engine, Quasar-owned Postgres always beside the control plane; (b) (a) plus a
Quasar-owned Postgres on its own machine; (c) combined and GPU hosts only.

**Recommendation.** (a): meets #218's control-only line and #219's "separate control/GPU
host" acceptance with the fewest new moving parts, and keeps the database beside the
actor that replaces the control plane (so it can take a pre-migration backup locally).

**Agreed (owner, 2026-09-24): (a).**

| Shape | Quasar-owned services on that machine |
|---|---|
| Combined host | control plane, Postgres, node agent, recovery actor |
| Control-only host | control plane, Postgres, recovery actor |
| GPU host | node agent, recovery actor |

- One Quasar installation per container engine; two on one engine stays unsupported.
- A database elsewhere uses D1's external-database mode.
- The control-only host is a new install target and needs its own fresh-install evidence
  and "no agent on this machine" coverage (readiness, preflight, release view).

## Premise P1 (clarified at the owner's request): how an owned service is changed

Not a new choice; it follows from #219's acceptance ("Managed updates no longer
read/write manager .env or depend on Compose labels/paths") and RH-01's Engine API
foundation. Recorded so later decisions can cite it.

- A Quasar-owned service is created and replaced **through the Docker Engine API**, from
  a versioned service specification (image digest, environment, mounts, devices, ports,
  restart behaviour) held in Postgres and cached by the recovery actor so it can act
  while the control plane is down. No compose file, no `.env`, no `com.docker.compose.*`
  label is read or written for an owned service.
- Bootstrap starts **only the recovery actor** (a `docker run` line or a manager template
  that declares that one container); the recovery actor creates everything else.
- Compose survives in two places only: the existing updater keeps serving **stacks that
  have not migrated** (legacy, maintained, no new features; not removed before every
  owned path is covered, per #218), and **external-manager mode**, where the manager's
  files are its own and Quasar never writes them.
- *Superseded in part:* D3 removed the write-free external-manager mode (a manager hosts
  only the seed), and D4 removed stack migration, so the Compose updater is retired in
  RH-06 rather than kept for unmigrated stacks. The first two bullets stand.

---

## D3. External managers: the seed, and no write-free mode

**Question (as first put).** What is external-manager mode, and how does Quasar avoid
racing a manager? First recommendation: a permanent, declared, per-machine write-free
mode (manager owns everything, Quasar only reports).

**Owner's challenge.** Under the new architecture the manager should deploy only a small
bootstrap container that stands up the node, and Quasar should fully update its nodes,
itself and potentially that bootstrap container, whether the manager is Dockge, Portainer
or anything else. The write-free mode was withdrawn as the primary design.

**Refinement put to the owner.** If the manager's template also declared the control plane
or Postgres, a manager redeploy (or GitOps auto-update) would recreate them from the
template's older image: a duplicate role, a port conflict, and potentially a control-plane
binary older than the database (the ADR 0002 crash loop). And if the manager-created
container were the recovery actor itself, Quasar replacing it would race the manager one
level down. So the manager-facing container is split in two roles:

- **Seed** — manager-owned, declared by the template, deliberately trivial: it ensures the
  recovery actor exists (creates it on first boot, or if it is deleted) and does nothing
  else. Designed never to need an update; if it ever does, the console says so and the
  operator updates it in the manager.
- **Recovery actor** — Quasar-owned, created by the seed; creates and replaces Postgres, the
  control plane and the node agent through the Engine API, and replaces itself by handing
  over to a successor.

**Options.** (a) template declares only the seed; Quasar owns everything else on every host
shape; no separate write-free mode. (b) (a) plus a declared, supported write-free mode.
(c) template declares control plane + Postgres + bootstrap container and Quasar updates them
anyway (rejected: the redeploy race).

**Recommendation.** (a).

**Agreed (owner, 2026-09-24): (a).** Confirmed on follow-up: Quasar updates the control
plane (by the recovery actor on its machine, a separate container), every node agent (by
the recovery actor on that host), Postgres within its major version, and the recovery actor
itself (handover). Initiated by a console fleet run, per-host apply/revert, or unattended
update; ADR 0002 ordering, migrating-release drain, unattended-never-migrates and "agent
update ends that host's sessions until RH-04" all still hold.

- A manager's template declares **only the seed**. The manager never holds a definition of
  any Quasar service it could redeploy, which makes the race structurally impossible
  rather than detected.
- **Race guard** anyway: a Quasar-service container on a machine that its recovery actor did
  not create (a leftover Compose stack, a mistaken manager definition) is never acted on;
  it raises a readiness fault naming both containers.
- **No write-free mode is built.** An operator who wants full manual control keeps an
  unmigrated stack; without an updater it already gets today's manual recipe (digests to
  apply by hand).
- The seed's version is reported in the console; its behaviour must stay tiny and frozen.
- Vocabulary consequence: "external manager" now means "hosts the seed", not "owns the
  services". `CONTEXT.md` is updated to match.

---

## D4. No in-place migration: a clean break, with data carried by restore

**Question (as first put).** Which existing deployments can migrate to Quasar ownership,
and what does migration read? Recommendation was: any registry-installed stack, reading
only the running containers through the Engine API; source-built stacks excluded.

**Owner's direction.** Quasar has about four confirmed users, all testing. Existing
deployments need not be migrated: the new way to deploy is a fresh install, and existing
stacks redeploy.

**Consequences agreed with it.**
- Removed from RH-06: the adoption engine, the exactly-once ownership-transfer protocol
  (disable old reconciler → verify → enable writes), failed-adoption rollback, resumable
  and reversible migration, and support for four install origins. The original Q5
  (opt-in / resumable / reversible migration) is answered by this: there is no migration.
- The Compose updater, the Compose preflight checks (`updater_stack_dir`,
  `updater_overlays`) and Compose-label discovery can be **retired in RH-06** rather than
  kept beside the new path, because no stack is carried forward on them.
- The release that ships RH-06 publishes only a format-2 manifest, so installed control
  planes (which accept only format 1) are never offered an in-place update onto the new
  architecture. Release notes say "reinstall".
  - *Corrected during design (2026-09-25):* an old control plane shows **nothing**, not a
    fault — `manifest_invalid` is reserved (`release.go:169`) but never emitted. And the
    **edge channel bypasses manifests** (`detect.go:150-200` resolves branch image tags
    directly), so old edge installs would be offered RH-06 builds unless RH-06-era edge
    builds publish under a tag family old control planes do not resolve. The architecture
    document fixes both.
- **This deliberately replaces tracker text.** #219's acceptance asks for "explicit
  migration from existing stacks", "failed adoption rollback" and preservation "during
  opt-in adoption"; #207 says Compose updating is removed "only after … installation
  migration exist". Per the owner, those lines are superseded by a clean break plus the
  data-carry path below. #207/#218/#219 are not rewritten; the RH-06 specification states
  the substitution. #219's "validate a third-party manager with stack files outside
  Quasar storage" survives as a fresh install whose seed runs under a stack manager.

**Follow-up question.** What, if anything, carries across the reinstall?
(a) clean break with a documented, tested data-carry path; (b) nothing carried.

**Recommendation.** (a): the restore path must exist and be tested for disaster recovery
anyway (see D7), so "migration" becomes "restore into a fresh install" with no special
code for old stacks.

**Agreed (owner, 2026-09-24): (a).**
- **Database:** back up the old stack with today's documented `pg_dump`; the new install
  restores it **before the control plane's first boot**, using the same restore capability
  the recovery actor provides for disaster recovery.
- **Homes:** the new install is pointed at the same host directory; nothing is copied.
- **GPU hosts:** re-enroll with a newly minted token under the **same node name**, which
  re-keys the existing host row and keeps its history.
- **Stored secrets:** re-entered (SteamGridDB key, release-webhook secret), unless the old
  `QUASAR_SECRET_KEY` is supplied at restore.
- Evidence required: one restore-into-a-fresh-install test and an operator doc page.

---

## D5. Where secrets and install-time settings live

**Question.** With no `.env`, where do secrets and install-time settings live on a
Quasar-owned machine?

**Options.** (a) the recovery actor holds machine-local state; Postgres holds desired
configuration; (b) the same, but the operator supplies every secret in the seed's
template; (c) an operator-owned env file Quasar reads but never writes.

**Recommendation.** (a): the only option where no secret sits in a manager's stored
configuration, and the one that lets the recovery actor act with the control plane down.

**Agreed (owner, 2026-09-24): (a).**
- A Quasar-owned **machine-state volume**, held by the recovery actor, stores that
  machine's generated secrets (database password, `QUASAR_SECRET_KEY`), its cached service
  specifications and its attempt journal. Secrets are 0600 files.
- Containers receive secrets as **mounted files, not environment variables** (Postgres's
  `POSTGRES_PASSWORD_FILE`; the control plane gains the equivalent `*_FILE` inputs).
- The seed's template takes only bootstrap inputs: role (combined / control-only / GPU),
  an enrollment string for a GPU host, the home root path, optionally a public host name.
  Everything else is defaulted at install and edited from the console, stored as desired
  specifications in Postgres beside RH-05's host policy.
- **Recovery bundle:** a backup is the database dump plus the machine's secrets; the
  operator is told to keep it. It is also D4's restore input.
- The machine-state volume is the one volume that must never be deleted. A readiness
  warning reports "no recovery bundle taken since install / since the key changed".
- No secret is written into a manager's configuration by any supported path.

---

## D6. How instructions reach each recovery actor

**Question.** How does the control plane instruct each machine's recovery actor?

**Options.**
- (a) Keep today's path, tightened: GPU hosts via the agent WebSocket relay to the local
  recovery actor; combined/control-only hosts via a local socket that the recovery actor
  mounts only into the containers it creates. One control-plane-facing process per host.
  Gap: an agent broken outside an apply cannot be reverted from the console.
- (b) Each recovery actor holds its own host-scoped identity and its own outbound
  connection to the control plane, so the control plane reaches it even when the agent is
  down (#201's "second relay path").
- (c) (a) now, (b) later if field failures demand it.

**Recommendation and the owner's challenge.** First recommendation was (a). Asked whether
that was because it was easier or technically better, the honest answer given was:
mostly easier. (a)'s one technical argument is a single control-plane-facing identity,
reconnect path and compatibility window per host (#207's "one host agent per machine");
(b) is technically better for recovery (independent path to a host whose agent is down,
the actor can report its own state, #201). Security is not a differentiator: the agent
already holds the Docker socket, and a compromised control plane can instruct the actor
either way. (a) mainly saves a new frozen wire protocol, a second enrollment/credential per
host, and a second reconnect/compatibility story.

**Agreed (owner, 2026-09-24): (b).** Goal: hands-off recovery of GPU hosts, no local
command needed for an agent that breaks outside an apply.
- Every recovery actor enrolls with its own host-scoped identity (alongside the agent,
  from the same enrollment) and keeps an outbound connection to the control plane.
- The control plane can instruct and read a host's recovery actor while that host's agent
  is down; the agent no longer relays platform replacement.
- On the control plane's own machine, the recovery actor keeps acting from its journal
  while the control plane it is replacing is down.
- **Contract approval.** The owner stated that choosing (b) is acceptance of the contract
  changes it requires. Recorded as owner approval of the **direction**. The concrete
  amendment text (`agent-api.md`, `control-api.md`, `schema.md`, the release manifest
  format) is still drafted as a proposal, reviewed by Opus, and signed off by the owner on
  that text before dependent code lands, per CLAUDE.md "Frozen interfaces". The RH-06
  breakdown opens with that contract ticket, as RH-05 did with #334.

---

## D7. Database safety for a migrating control-plane update

**Question.** What protects the database when a control-plane update carries a migration?

**Options.** (a) automatic pre-update backup, automatic restore only when the new control
plane never passed a health check, operator-chosen restore otherwise; (b) automatic
backup, never automatic restore; (c) no automatic backup (today's manual `pg_dump`).

**Recommendation.** (a): the case it restores automatically — a migration that fails
before the control plane ever serves — is the one that today leaves a household install
dead with no way back, and it risks no data because the fleet is drained and nothing was
served.

**Agreed (owner, 2026-09-24): (a).**
- Before replacing the control plane with a **migrating** release, the recovery actor takes
  a recovery bundle (D5) and **refuses to proceed** if it cannot (dump failure, not enough
  free space). The last few pre-update bundles are retained.
- **New control plane never passes a health check** → the recovery actor stops it, restores
  the pre-update bundle into the stopped database, starts the previous control plane, and
  records the attempt failed-and-restored.
- **New control plane passed a health check and failed later** → no automatic restore (real
  writes may exist). The console — or one recovery-actor action if the console is down —
  offers "restore the pre-update backup (taken at T) and return to version X".
- Invariant kept: Quasar **never runs an older control plane against a newer schema**. A
  return to an earlier version always goes through restoring a backup taken before the
  migration, into a stopped database (today's documented abandon-upgrade path; ADR 0006's
  new boot incarnation applies). No rollback below an applied schema is promised.
- Costs accepted: free space for a dump, a longer migrating update, and a precisely
  defined and fault-tested "passed a health check".

---

## D8. Interrupted replacement

**Question.** What happens when a replacement is interrupted part-way (recovery actor
crash, host reboot, Docker daemon restart, power loss during pull / create / replace /
health wait)?

**Options.** (a) finish the interrupted attempt to a stated outcome, never start a new
one; (b) always roll back after a restart; (c) stop and wait for an operator.

**Recommendation.** (a): delivers #218's "demonstrate interrupted pull/create/replace/
health-check recovery" with every attempt ending in a stated outcome and nothing
half-applied or silently retried.

**Agreed (owner, 2026-09-24): (a).**
- The recovery actor journals each phase durably (fsync) **before** acting on it.
- The old container is **stopped and kept** (disabled so a reboot cannot start it), not
  removed, until the new one is verified; restore is then immediate and needs no pull.
- After any restart the recovery actor reads its journal and drives the interrupted
  attempt to a terminal outcome: interrupted before the old container was stopped →
  *interrupted, nothing changed*; after that → continue to verification, and on failure
  restore under D7's rules.
- It never retries an attempt on its own; the operator or the unattended schedule starts a
  new one.
- Every step is idempotent (deterministic container names, journal ids), including the
  recovery actor's hand-over to its successor.
- The race guard (D3) recognises a kept old container as the recovery actor's own, not as a
  second owner.
- Costs accepted: the kept container's image stays on disk until verification; two
  same-role containers briefly coexist, one always stopped and disabled.

---

## D9. Compatibility floors and revert boundaries

**Question.** How far back may each service go, and which mixed versions are allowed?

**Options.** (a) a declared floor per release plus a stated revert rule per service;
(b) today's rules, no floor; (c) exact version match, no mixed fleet.

**Recommendation.** (a): meets #218's "compatible agent revert", gives the new recovery
actor protocol (D6) a boundary from day one, and leaves RH-04 (#215) room to extend the
window rather than redefine it.

**Agreed (owner, 2026-09-24): (a).**

| Service | Revert rule |
|---|---|
| Control plane | Never reverted; the only way back is D7's restore of a pre-migration backup |
| Postgres | Forward only, within one major version; never reverted except D8's restore of the kept container during a failed attempt |
| Node agent | Revert to any release ≤ the control plane's and ≥ that control plane's declared floor |
| Recovery actor | Same rule as the node agent, so a bad recovery actor can be backed out |

- Each control-plane release **declares the oldest agent and recovery-actor release it
  still speaks**. A host below the floor is not failed: it reads "must update before it can
  be managed" and is offered only an update.
- A release-time check refuses a release whose floor would strand a component with no
  path forward.
- ADR 0002 holds unchanged: control plane first, never offer below it, agents never ahead.
- RH-06 does **not** claim restart-safe agent updates: an agent update ends that host's
  sessions until RH-04 lands. RH-04 extends the floor into full compatibility windows
  including session workers.

---

## D10. Enrollment tokens, bootstrap idempotency and identity recovery

**Question.** How does a machine join, and how does it recover a lost identity?

**Options.** (a) one minted token enrolls one machine once; no static token; (b) (a) plus
the static token as break-glass; (c) long-lived multi-use tokens.

**Recommendation.** (a): removes the only credential that can enroll any node name, reuses
the existing `host_enrollments` model (#12/#96/#100) instead of a second one, and makes
re-running the bootstrap harmless (#219's "resumable/idempotent bootstrap").

**Agreed (owner, 2026-09-24): (a).**
- **Add a GPU host:** the admin chooses "add host" (optionally naming it, which binds the
  token to that node name); the console shows the seed's `docker run` line or manager
  template with the enrollment string filled in. The recovery actor redeems the token
  **once** and receives **both** host-scoped credentials (its own, D6, and the agent's),
  then creates the agent.
- **Re-running the seed is idempotent:** an identity already in the machine-state volume
  wins and the token is ignored.
- **Lost machine-state volume:** mint a new token; the same node name re-keys the host row
  and keeps its history; refused while the old identity is connected; a console action
  "forget this host's credentials" handles a dead machine.
- **Combined / control-only machines:** the recovery actor generates a one-time local
  enrollment secret for its own machine at install. The static deployment-wide
  `ENROLLMENT_TOKEN` is **retired**.
- **Console:** token list and revoke (the API exists; the UI is new); one-hour default
  expiry kept, admin choice up to 30 days, and a "regenerate" action.

---

## D11. Supported install front doors, managers and deferred modes

**Question.** Which install front doors and managers does RH-06 support and validate?

**First proposal.** Validated: plain Docker Engine on Linux (all three shapes), Dockge,
native Unraid with a seed template; documented only: Portainer; deferred: Podman/rootless
(RH-07), Kubernetes, control-plane HA, private registries (#9), two installs per engine,
major Postgres upgrades.

**Owner's changes.** Portainer and Arcane must work, being common deployment paths. The
Unraid Community Applications template is a design target, not supported today, because
no template exists.

**Agreed (owner, 2026-09-24), as confirmed back:**

| | RH-06 status |
|---|---|
| Supported, with fresh-install evidence | Plain Docker Engine on Linux (combined, control-only, GPU hosts); **Dockge, Portainer and Arcane**, each running the seed-only snippet with stack files in the manager's own storage. The Portainer run includes a GitOps auto-update or "redeploy stack" step as the live test of D3's race guard. A `docker run` seed on Unraid counts as plain Docker (appdata path documented). |
| Design target, not supported | Unraid Community Applications template: the seed must be declarable by one (one container, plain-field inputs, configurable persistent path, no Compose-label dependence); no template ships and nothing is claimed for it. |
| Retired (D4) | the Compose updater and the release-install compose path. (`enroll-host.sh` is rewritten, not retired: see D13.) |
| Deferred | Podman and rootless (RH-07), Kubernetes, control-plane high availability, private/credentialed registries (#9), two installations on one engine (D2), major Postgres upgrades (D1). |

**Clarified for the owner: the seed-only snippet.** One small container
(`quasar-seed`) whose few inputs (role, home root, optional public host name, and for a
GPU host a single-use enrollment string) are written in the `docker run` line or the
manager's stack definition — **no env file**, and no long-lived secret in it. Deploying it
creates the recovery actor, which generates the machine's secrets, detects the GPU
(no vendor overlay), and creates Postgres, the control plane and the agent as the role
requires. The manager shows one container in its stack; Quasar's services appear as
separate Quasar-labelled containers outside it, and a stack redeploy only touches the
seed. Variable names are illustrative until the specification fixes them.

---

## D12. Uninstall, and what removing the seed means

**Question.** Removing the stack in a manager removes only the seed. What does uninstall
mean?

**Options.** (a) removing the seed never removes Quasar; uninstall is an explicit Quasar
action; (b) removing the seed cascades to a teardown; (c) no uninstall feature, docs only.

**Recommendation.** (a): removing data is never a side effect of managing the stack; the
one irreversible step sits behind a confirmation and a backup offer. (b) is rejected
because a manager "redeploy" stops and recreates the seed and would look like an
uninstall.

**Agreed (owner, 2026-09-24): (a).**
- **Seed missing:** the machine keeps running; the console warns that Quasar cannot
  re-create its recovery actor if that is deleted.
- **Uninstall a GPU host:** console "remove host" drains it, stops and removes its agent and
  recovery actor, and forgets its credentials.
- **Uninstall a whole machine** (combined / control-only): one command (the seed image run
  with `uninstall`) that works with the console down; removes Quasar-owned containers in
  reverse order and **keeps data by default** (database volume, machine-state volume,
  homes). `--purge` needs a typed confirmation and first offers a recovery bundle.

---

## D13. The one-line enrollment URL stays, as the default way to add a host

**Owner's question.** Would today's one-time enrollment one-liner (run on the GPU host to
set everything up) still work? It is simpler in many cases.

**Answer given; confirmed by the owner (2026-09-24): keep the one-line enrollment script.**
Yes, and it is the better default,
because today's `enroll-host.sh` also performs host preparation a container cannot: render
node and `/dev/uinput` checks, persistent `uinput` / user-namespace sysctl guidance, and
installing and loading the app-container AppArmor profile — all host-root work the seed
does not have.

- `enroll-host.sh` is **rewritten much smaller, not retired** (corrects D11). The console
  one-liner keeps its form, including the `--pinnedpubkey` handling for a self-signed
  control plane: `curl -fsSL [-k --pinnedpubkey 'sha256//…'] https://<control-plane>/enroll-host.sh | QUASAR_ENROLLMENT='qenr1.…' sudo bash`.
- The script: (1) checks the host as today and prints (or, if asked, applies) fixes;
  (2) runs the **same seed container** with the token and home root; (3) waits until the
  machine is enrolled and reports. It writes **no compose file, no `.env`, no install
  directory**. A one-liner host is indistinguishable from a snippet host afterwards.
- Console "Add host": the one-liner by default; a second tab "Using Dockge / Portainer /
  Arcane? Paste this stack instead", noting that host preparation is then the operator's
  job and the readiness card lists anything missing (RH-02 checks).
- The site installer's combined-host command becomes the same script with a
  `combined` role: one install script for every shape.

---

## D14. Postgres update disruption, and unattended scope

**Fact checked for this question.** The control plane's `/health` pings the database and
answers 503 while it is unreachable (`control-plane/internal/health/health.go:19-39`); the
pgx pool reconnects and the process keeps running. [I] Whether live sessions and agent
connections survive a database outage has never been tested.

**Options.** (a) treat a Postgres replacement like a migrating control-plane step (drain,
stop control plane, recovery bundle, replace and verify Postgres, start control plane;
never unattended); (b) ride through with the control plane running; (c) (a) now, move to
(b) once fault evidence proves the ride-through.

**Recommendation.** (c).

**Agreed (owner, 2026-09-24): (c).**
- RH-06 ships the conservative Postgres replacement: drain the fleet, stop the control
  plane, take a recovery bundle, replace and verify Postgres, start the control plane.
  It briefly takes the instance offline, ends sessions, and waits for an operator.
- Ride-through is **not claimed**; proving that the control plane survives a Postgres
  restart without dropping agents or sessions is recorded as follow-up work.

| Update | Unattended allowed? |
|---|---|
| Recovery actor (hand-over; ends no sessions) | Yes, under the agent's rules |
| Node agent; non-migrating control plane | Yes, as today |
| Migrating control plane; Postgres | Never |

---

## D15. Contributor lane and product lane

**Question.** How do contributors and the shared test hosts run their own builds?

**Options.** (a) two lanes kept apart: the contributor lane (`redeploy.sh` + Compose from
source, a source install, never touched by the recovery actor) and the product lane
(images from `build-images.sh` pushed to a contributor-controlled registry namespace,
installed with the seed, moved by a developer apply); (b) owned path for everything,
retire `redeploy.sh`; (c) (a) plus an owned install accepting local image ids with no
registry.

**Recommendation.** (a).

**Agreed (owner, 2026-09-24): (a).**
- Contributor lane unchanged: fast source loops (`make redeploy-cp`, `make rebuild`, the
  AGENTS.md host skills) keep working; such a stack is a source install, gets the manual
  recipe, and is never acted on by a recovery actor.
- Product lane: RH-06's own evidence runs on exactly the path users run. A **developer
  apply** lets an admin apply an arbitrary digest set from an allowlisted namespace, so a
  branch build reaches an owned install without being published as a release; it reuses
  ADR 0001's digest and allowlist trust model rather than weakening it.
- A machine is in one lane or the other, never both (D2). Shared test hosts move between
  lanes deliberately, one host at a time, under the shared-host preflight in AGENTS.md.
- Docs must state that the Compose path is contributor tooling, not a supported install.

---

## D16. Console surfaces without mockups

**Fact.** `design_handoff_v3` covers Fleet ▸ Releases (`releases-v3.html`) and an "Enroll
host" button (`assets/pages-fleet.js`). It does not cover: the per-machine service
inventory (seed, recovery actor, Postgres, control plane, agent — version and owner),
recovery bundles and "restore the pre-update backup", the Add-host modal with one-liner and
manager-snippet tabs, the enrollment-token list and revoke, "forget this host's
credentials" / "remove host", the "must update before it can be managed" state, and
developer apply.

**Options.** (a) an early design ticket extends `design_handoff_v3` with mockups for these
surfaces (in its own HTML and tokens), owner-approved before any UI slice depends on them;
(b) compose from existing components, owner reviews screenshots per slice; (c) text-only UI.

**Recommendation.** (a): the rule CLAUDE.md already sets ("If no mockup covers the surface
being changed, say so explicitly and ask before styling").

**Agreed (owner, 2026-09-24): (a).** The design ticket depends only on the specification;
every UI slice builds and visually verifies against its approved mockups.

---

## D17. Recovery bundle storage and off-machine copies

**Question.** Where do recovery bundles live, and how does an operator get one off the
machine?

**Options.** (a) kept on the machine, optional backup directory copy, passphrase-encrypted
console download, no schedules; (b) local volume only; (c) (a) plus scheduled backups.

**Recommendation.** (a): makes "keep it safe" something a household operator will actually
do, without adding scheduling scope.

**Agreed (owner, 2026-09-24): (a).**
- Bundles are written into the machine-state volume: pre-update bundles retained up to the
  last three; on-demand bundles kept until deleted.
- Optional operator-set **backup directory** (a host path, e.g. a NAS share); every bundle
  is also copied there.
- Console **"Download recovery bundle"** (admin): asks for a passphrase and produces a
  passphrase-encrypted file; key and password never leave the machine in the clear.
- Restore (D4, D7) accepts either form. A lost passphrase makes that download useless; the
  on-machine copy is unaffected, and the console says so.
- Scheduled backups are **out of scope** (follow-up): bundles are taken on demand and before
  an update only.

---

## R1. YAGNI review of D1–D17 (owner-requested, 2026-09-24)

**Owner's prompt.** Review whether RH-06 is over-engineered for a self-hoster; D17 looks
like overkill: data persists outside the containers, and nothing stored is critical.

**Fact check behind the review.** The database lives in a volume and homes on host paths.
`QUASAR_SECRET_KEY` protects only re-enterable values (SteamGridDB key, release-webhook
secret). Because Quasar owns its Postgres container, a lost database password can be
reset by the recovery actor. The one thing that genuinely needs protection is the
database before a migration runs.

**Agreed changes (owner, 2026-09-24).** These amend the entries above; where they conflict,
this section wins.

| Entry | Outcome |
|---|---|
| **D17** | **Cut.** Replaced by: before a migrating control-plane update the recovery actor takes a `pg_dump` into a volume and keeps the last three. No secrets in it, no download, no passphrase, no backup directory. The "Recovery bundle" term is withdrawn from `CONTEXT.md`. |
| **D7** | **Simplified.** Automatic pre-migration dump kept. Restore is **one operator command** (the seed image run with `restore`) that the failure message prints; no automatic restore path. Invariant unchanged: an older control plane never runs against a newer schema. |
| **D4** | **Kept, simplified.** The same `restore` command, pointed at a `pg_dump` file, before the fresh install's first boot. |
| **D1 / D14** | **Postgres replacement cut from RH-06.** Quasar creates its Postgres at install, pinned, and does not update it in RH-06: no Postgres component in the release manifest, no drain procedure, no ride-through follow-up. A later ticket adds updates if ever needed. D14's unattended table loses its Postgres row. |
| **D1 (addition)** | **Owner: people may run their own Postgres; do not assume the database is Quasar's to manage.** Bringing one's own database is a first-class install option, not an edge case. Its consequences are the open question R1-Q1 below. |
| **D10** | **Cut:** token list/revoke UI and "forget this host's credentials" (tokens expire in an hour; a dead host is not live, so the same node name already re-enrolls). |
| **D11** | **Changed by owner:** supported managers are **Dockge and Arcane**; **Portainer is dropped** entirely (no support claim, no evidence). Later work may integrate with Arcane directly through its API. The D3 race-guard live test moves to a Dockge (or Arcane) "redeploy / update stack" step. |
| **D16** | **Kept, smaller** (fewer surfaces after the cuts). |
| **D2, D3, D8, D9, D13, D15** | **Kept.** |
| **D5, D12** | Proposed simplifications not yet answered (see below). |
| **D6** | Flagged as the largest remaining complexity; owner's answer pending. |

**Owner's answers on the pending items (2026-09-24).**
- **D5: kept as agreed** — secrets as mounted files in the machine-state volume, never in
  environment variables or manager configuration. The owner treats this as a security
  baseline not to be compromised. (The "no recovery bundle" warning disappears with D17.)
- **D6: switched to (a)** — the owner accepted the YAGNI recommendation. GPU hosts are
  instructed through the agent WebSocket relay to their local recovery actor; combined and
  control-only hosts through a local socket. The recovery actor mounts its socket **only**
  into the containers it creates (no shared world-writable volume). One control-plane-facing
  process per host. Accepted gap: an agent broken *outside* an update is recovered with one
  local command; a failed update is restored automatically (ADR 0004). D6 (b) — an own
  identity and connection per recovery actor — is recorded as the upgrade path if field
  failures show the gap matters.
  - Knock-on to D10: the recovery actor holds no control-plane identity; it passes the
    enrollment string to the agent it creates, and the agent enrolls exactly as today.
  - Knock-on to contracts: the new recovery-actor wire protocol disappears. Remaining frozen
    changes (release manifest format and components, `release_apply` components, install
    mode on `register`/host body, preflight check ids, retirement of Compose-specific
    fields) still go through a concrete amendment, Opus review and owner sign-off.
- **D12:** owner leans to keeping the uninstall command for user experience; confirmation
  requested (R1-Q2).

**R1-Q1 — an operator's own Postgres (agreed, owner, 2026-09-24).**
- Proposed first: (a) Quasar only *uses* an external database — no dump, restore or
  password reset; a migrating update requires the operator to confirm a current backup
  of their own database (console checkbox; unattended updates never migrate anyway).
- Proposed next, for D5 consistency: pass the password as a mounted file. **Rejected by
  the owner**: it creates friction for the Dockge and Arcane paths and is not common
  practice. The common practice is a stack folder holding the compose file and `.env`,
  values interpolated into the container environment, and only a data subfolder mounted —
  the `.env` itself is never mounted into a container.
- **Outcome:** (a) as first proposed. The external database's connection settings,
  password included, are seed inputs supplied the manager's usual way (interpolated from
  the stack's `.env`). Quasar never reads or writes that file (#219 holds); the recovery
  actor copies the values into machine state at first boot and passes them to the control
  plane as mounted files (D5 holds for everything Quasar generates or holds). This is the
  single, documented case where a secret also lives in the operator's manager
  configuration, by the operator's choice of database.

**R1-Q2 — uninstall (agreed, owner, 2026-09-24): keep D12**, revised for D17's cut: the seed
image run with `uninstall` removes Quasar-owned containers in reverse order and keeps the
database volume, machine-state volume and homes; `--purge` needs a typed confirmation and,
for a Quasar-owned database, first takes one final `pg_dump` (D7's dump, not a bundle).
Console "remove host" stays for GPU hosts.

---

## Architecture-stage decisions (2026-09-25)

Raised by the design step ([`2026-09-24-architecture.md`](2026-09-24-architecture.md) §9).

### A1. The recovery actor may lead the control plane on its own machine

**Question.** The actor replaces itself first inside each attempt (so the actor that renders
release R's control plane is an R actor). If the control-plane step then fails, the actor
stays one release ahead of the restored control plane. D9 said the actor follows the agent
rule (never ahead).

**Options.** (a) accept this one exception on the control plane's own machine; (b) keep D9
literally (every container-shape change then takes two releases, or needs an image-carried
template language).

**Recommendation.** (a): the actor carries no schema, so ADR 0002's reason for "control plane
first" does not apply to it; designs 1 and 3 each needed this independently.

**Agreed (owner, 2026-09-25): (a).** D9's actor rule is amended: on the control plane's own
machine the recovery actor may be one release ahead of the control plane while a
control-plane replacement is in flight or after one failed and was restored. Everywhere else
D9 stands; revert targets and host eligibility still never put an actor above the control
plane.

### A2. Machine inputs live in machine state; no service-spec table

**Question.** Where do a machine's install-time settings (home root, public host, ports,
database mode) live, and how are they changed in RH-06? P1 and D5 had said "desired
specifications held in Postgres, edited from the console".

**Options.** (a) set at install, changed with `quasar-recovery reconfigure` (a Replacement
with the same digest and new inputs), no console editor; (b) a console editor feeding the same
Replacement path now; (c) design 2's full service-spec table (rejected in the comparison).

**Recommendation.** (a): rarely changed after install, and the actor must own them to act
while the control plane is down; an editor can be added later on the same path.

**Agreed (owner, 2026-09-25): (a).** Amends P1 and D5: a service's versioned specification is
(recipe revision, machine inputs, image digest). Machine inputs and secrets live in the
recovery actor's machine state; Postgres keeps what it holds today (digests per attempt,
instance settings, RH-05 host policy). No service-spec table and no console editor for
machine inputs in RH-06. Agent-applied settings remain RH-05 host policy.

### A3. The recovery actor is written in Rust

**Question.** Go (grow today's updater; keep the signature verifier, allowlist and request
gates unchanged; hand-roll a small Go Engine client) or Rust (reuse RH-01's engine facade via a
shared crate; port the gates and the ADR 0003 verifier; a permanent Go↔Rust socket seam)?

**Recommendation given.** Go, to keep the security-sensitive gates exactly as reviewed and to
avoid a `node-agent` workspace split before RH-03/RH-04.

**Agreed (owner, 2026-09-25): Rust.** The owner prefers Rust and questions why Quasar has Go
at all; moving away from Go later is the owner's stated long-term direction.
- The recovery actor (and its seed mode and operator commands) is a Rust binary reusing
  RH-01's engine facade, ownership labels and RH-05's durable-file primitives, extracted into
  a GStreamer-free shared crate (design 3's premise). RH-03/RH-04 have not started, so the
  extraction lands first, as a pure refactor.
- The namespace allowlist, request gates and the ADR 0003 signature verifier are **ported with
  shared golden vectors**, and the Go originals retire with the updater. The port is a
  security-sensitive slice and is reviewed as one.
- The control plane ↔ actor socket is a cross-language seam: its request/status shapes are
  pinned by JSON fixtures checked by tests on both sides.
- **Out of RH-06 scope:** migrating the control plane from Go to Rust. Recorded as direction
  only; no RH-06 ticket starts it. Noted benefit: that migration would later remove the
  cross-language seam this decision introduces.
- Unchanged by this: compiled per-role recipes and "actor moves first" (A1), no service-spec
  table (A2), scoped sockets, the one-binary seed, the hand-over protocol.
