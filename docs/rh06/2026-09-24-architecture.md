# RH-06 architecture — the recovery actor and owned services

Drafted 2026-09-24/25. Implements the agreed product decisions in
[`2026-09-24-decisions.md`](2026-09-24-decisions.md) (D1–D17 as amended by R1), on the facts in
[`2026-09-24-research.md`](2026-09-24-research.md). Three independent designs were produced
first ("design it twice") and are kept in full under [`designs/`](designs/):

1. [`1-minimal-evolved-updater.md`](designs/1-minimal-evolved-updater.md) — evolve the Go updater;
   recipes compiled into the actor.
2. [`2-declarative-service-specs.md`](designs/2-declarative-service-specs.md) — fully rendered
   service specifications in Postgres; templates carried by images; the actor converges.
3. [`3-rust-runtime-reuse.md`](designs/3-rust-runtime-reuse.md) — a Rust actor reusing RH-01's
   engine facade; templates carried by images, interpreted by the actor.

This document compares them, recommends a hybrid, and records the owner's architecture decisions (A1–A3).
Vocabulary is `CONTEXT.md`'s (Platform service, Service owner, External manager, Seed,
Recovery actor, Replacement, Attempt, Fleet run, Preflight, Admission restriction, Host
identity, Enrollment) and the architecture terms module, interface, depth, seam, adapter,
leverage, locality. **[F]** marks a fact about current code; everything else is proposal.

---

## 1. The recommendation in one page

- **One deep module per machine: the recovery actor**, a small static **Rust** binary
  (`quasar-recovery`) built in the `node-agent` workspace on RH-01's engine facade, which is
  extracted into a GStreamer-free shared crate (design 3's premise; owner decision A3). Its
  interface is three entry points — `submit`, `status`, `resume` — and behind them sit install,
  update, agent revert, ADR 0004 restore, the pre-migration dump, `restore`, uninstall,
  GPU-host removal, its own hand-over, crash recovery and the race guard. (Design 1's
  interface.) The Go updater retires; its allowlist, request gates and ADR 0003 signature
  verifier are ported with shared golden vectors.
- **Container shape is compiled into the actor** as a recipe book keyed by
  `(role, recipe revision)`; each platform image names the revision it needs in one label.
  **The actor moves first** inside the same attempt, so the actor that renders release R's
  control plane or agent is always an R actor; an actor refuses an unsupported revision after
  the pull and before anything stops. No service-spec table, no template language. (Design 1's
  answer to skew, chosen over image-carried templates for YAGNI and for the trust boundary.)
- **Two scoped sockets instead of one shared socket**: the control socket is mounted only into
  the control plane; the agent socket only into the node agent and can name only the agent
  and the actor. Authority follows mounts the actor itself creates. (Designs 2 and 3.)
- **The seed is a mode of the same binary and image** (`quasar-recovery seed`), frozen by a
  one-file contract (`seed.json` format 1) and a contract test run against state written by
  every released actor. (Design 3.)
- **A durable, fsync'd journal in a machine-state volume** drives every Replacement, keeps the
  old container stopped and disabled until the new one is verified, and settles any interrupted
  attempt to a stated outcome on restart (D8). The hand-over uses design 3's two-process,
  one-lease protocol and crash table.
- **The control plane's apply machines stay as they are** (fleet run, per-host attempt, revert,
  `Adopt`, owned admission restrictions). The `UpdaterAPI` seam
  ([F] `control-plane/internal/platform/apply_self.go:48-61`) gets an actor adapter; the pure
  planner learns ordered component lists and a floor.
- **Contract surface is the smallest of the three**: manifest format 2 under a new asset name,
  `release_apply` accepting `recovery-actor`, a few optional `register` fields, appended
  reason/check identifiers, one apply-request boolean, a handful of nullable columns. No new
  wire protocol and no new tables.

---

## 2. What every design had to satisfy

From the decisions: one recovery actor per machine, owning control plane, node agent and itself
(D1, D3); Postgres created, never updated, or external and never touched (R1); combined,
control-only and GPU hosts (D2); Engine API only, no Compose files, `.env` or Compose labels
for owned services (P1, #219); seed-only manager templates (D3); machine-state secrets as
mounted files (D5); instructions through the agent relay on GPU hosts and a local socket on
control-plane machines (D6(a)); pre-migration `pg_dump`, one `restore` command (D7, D4);
journalled, finish-don't-retry Replacements (D8); declared floors and per-service revert rules
(D9); one minted token per machine, no static token (D10); the one-line enrollment script
(D13); uninstall keeping data unless purged (D12); two lanes (D15). And ADRs 0001–0006 unchanged
in force.

---

## 3. The three designs

### 3.1 Design 1 — minimal, evolved updater (Go)

- **Premise.** Keep the control plane's apply machines and the agent relay; swap the Compose
  executor for an Engine API executor with a journal; teach the actor per-role recipes.
- **Interface.** `actor.Actor{Submit, Status, Resume}` — one request shape
  (`Kind: replace | restore | remove`, ordered `Components`), one status shape.
- **Container shape.** Compiled recipe book `(role, revision)`; images carry
  `org.quasar.recipe=<int>`; the actor moves first inside each attempt and supports a window
  of revisions back to the floor.
- **Engine.** A hand-rolled stdlib Docker client over the unix socket plus an in-memory fake
  with crash injection.
- **Contract.** Smallest: manifest format 2 (third component, floor), `release_apply` accepts
  `recovery-actor`, three optional `register` fields, appended reasons/checks, one apply
  boolean, four nullable columns.
- **Weak spots.** Every container-shape change is an actor change (but ships in the same commit
  and moves first); hand-over is delicate; floor expressed as a schema version is coarse;
  deviates from P1's "service specification held in Postgres".

### 3.2 Design 2 — declarative service specifications (Go)

- **Premise.** Every owned service is a fully rendered, secret-free specification stored per
  machine in Postgres with RH-05-style desired/applied revisions; the actor converges.
- **Interface.** A pure shared `servicespec.Render(template, inputs)`; a `converge` decision
  module; an `engine` port; the actor re-renders every spec from the image's own template and
  refuses a mismatch (authority bound).
- **Container shape.** A template baked into each image as an OCI label, read from the
  registry without a pull; format rules S1–S3 (frozen output per format, expand-then-contract,
  release-time check).
- **Contract.** Largest: replaces `release_apply`/`release_state` with `platform_converge` and
  friends, four new tables, new endpoints (bootstrap, developer apply, machine inventory, remove
  host), ADRs 0007 and 0008.
- **Strengths.** Highest extension leverage (a new agent env var is one file beside its Rust);
  one executor for every change; opaque agent relay.
- **Weak spots.** Two desired-state systems (RH-05 policy and RH-06 specs) needing a line drawn
  knob by knob; a rendered spec gives the control plane more authority than an image swap, closed
  only by load-bearing re-render verification; a permanent template-format discipline.

### 3.3 Design 3 — Rust, reuse the runtime, optimise for the common caller

- **Premise.** A small static Rust binary `quasar-recovery` (seed, actor, operator CLI) in a
  slim image, reusing RH-01's Bollard facade, ownership labels and RH-05's durable-file
  primitives, extracted into a GStreamer-free `quasar-runtime` crate.
- **Container shape.** Image-label template in a closed vocabulary (mount roles, device roles,
  settings, secret files, vendor variants) interpreted by a generic actor; skew reduces to one
  `template_format` integer.
- **Seed.** Same binary, `seed` mode; frozen `seed.json`; frozen compiled actor profile.
- **Contract.** Medium: manifest format 2 with compat/floor, `release_apply` carries
  `recovery-actor` and optional settings, a `host_uninstall` message, a settings table.
- **Strengths.** Reuses the one engine layer that is already tested; one engine vocabulary for
  RH-07; the most complete hand-over crash table.
- **Weak spots.** Ports ~700 lines of gates plus the ADR 0003 signature verifier (~560 lines,
  ~780 lines of tests) to Rust; a permanent Go↔Rust socket seam needing fixtures on both sides;
  a `node-agent` workspace split while RH-03/RH-04 touch the same files; vocabulary creep.

---

## 4. Comparison

| | Design 1 (minimal, Go) | Design 2 (declarative, Go) | Design 3 (Rust reuse) |
|---|---|---|---|
| **Depth at the actor's interface** | High: 3 entry points carry every lifecycle case | Medium: several modules each with their own interface (render, converge, store) | High for the actor; the CLI and socket are thin |
| **Locality of a container-shape change** | Actor recipe + image label, same commit | Template file beside the consuming code; no CP change | Template file beside the consuming code, unless it needs a new vocabulary word |
| **Seam placement** | At the existing `UpdaterAPI` and agent relay; engine port inside the actor | New seams everywhere (spec store, converge, relay format) | At `UpdaterAPI`, plus a new cross-language socket seam |
| **Who bounds host authority** | The actor's compiled recipes: an image picks among shapes the actor already has | Re-render verification in the actor (must never be bypassed) | The closed template vocabulary |
| **Frozen-contract surface** | Smallest | Largest (new wire protocol, 4 tables) | Medium |
| **New code the team must own** | Go engine client (~14 endpoints), journal, recipes, hand-over | Go engine client, renderer, converge, spec store, format discipline | Crate extraction, ported gates and signature verifier, templates, hand-over |
| **YAGNI fit (R1)** | Best | Worst: builds for TURN, Postgres updates, arbitrary env | Middle |
| **RH-07 (Podman) later** | Second engine client to certify | Second engine client to certify | One engine client for agent and actor |

**Where each is strongest.** Design 1's interface and contract economy; design 2's opaque relay
and its argument that container shape should travel with the version; design 3's hand-over
protocol, one-binary seed, and honest pricing of the language question.

**Why not design 2.** Its leverage is real but is leverage for changes RH-06 does not need
yet, bought with the largest contract and a second desired-state system beside RH-05. Its
key idea, "the image describes itself", is also design 3's, and is weighed below.

**Language — decided Rust (owner, A3).** The first draft recommended Go: it would keep the
signature verifier, allowlist, request gates and their tests unchanged and share request types
with the control plane. The owner chose Rust, preferring one systems language and naming a
later move of the control plane away from Go as the long-term direction. Rust reuses the one
Engine API layer that is already tested (RH-01), gives RH-07 one engine client to certify, and
yields a small static binary. Its costs are carried explicitly in the plan: a pure-refactor
crate extraction first (RH-03/RH-04 have not started), a security-reviewed port of the gates
and the ADR 0003 verifier with shared golden vectors, and a cross-language socket seam pinned
by JSON fixtures tested on both sides (removed if the control plane later moves to Rust).

**Image-carried templates versus compiled recipes.** Templates remove the "every shape change
is an actor change" coupling, but because the actor moves first inside every attempt and both
are built from one commit, that coupling costs one extra file touched per shape change, not
an extra release. Against that, templates add a vocabulary and format discipline that becomes a
long-lived contract, and they move the authority boundary into a document an image supplies.
Compiled recipes keep the boundary in reviewed actor code. Under R1's YAGNI stance, recipes
win for RH-06; templates are the recorded path if container shapes start changing faster
than releases.

---

## 5. The recommended architecture

### 5.1 Module map

**New (Rust, `node-agent` workspace)**

| Module | Interface (summary) | Replaces / notes |
|---|---|---|
| crate `quasar-runtime` | RH-01's engine facade (socket discovery and refusals, typed Docker adapter, credentials, errors), ownership labels, `DurableFile<T>` (tmp + fsync + rename), `StateLease` (flock), self-inspection | Extracted from `node-agent/src/runtime*`, `container_ownership.rs`, and RH-05's journal primitives in `policy.rs`; pure refactor, agent behaviour identical. Linked by the agent and the actor |
| crate `quasar-recovery` → `actor` | `Actor::submit(caller, Request) -> Result<Accepted, Rejection>`, `Actor::status(request_id) -> Status`, `Actor::resume() -> Result<()>` | New; the deep module. Journal, machine state, lease, hand-over and a pure `settle` are internal |
| `quasar-recovery` → `engine` port | `PlatformEngine` trait: pull, image inspect/tag, container create/start/stop/update-restart/rename/remove/inspect/list/logs-tail/wait, volume create/inspect/remove, info | Two adapters: the real one over `quasar-runtime`'s Docker facade (Engine API ≥ 1.40) and an in-memory fake with fault and crash injection |
| `quasar-recovery` → `recipe` | `render(role, rev, &Inputs, &ImageRef, &SecretMounts) -> Result<ContainerSpec, RenderError>`; `Book::supports(role, rev)`; `Book::window(role)` | Pure; golden-file tested; carries what `deploy/docker-compose.yml` + the NVIDIA overlay describe today |
| `quasar-recovery` → `trust` | `admit(Caller, &Request, &Config, Option<&SignatureEvidence>) -> Result<Admitted, Rejection>`; `HttpsFetcher::evidence(base, version, deadline) -> SignatureEvidence` | **Port** of `updater/plan.go:158-256` (allowlist, digest, component and request gates) and `signature.go`/`signature_source.go` (ADR 0003), with the Go test cases turned into shared golden vectors; security-reviewed slice |
| `quasar-recovery` binary | Subcommands `seed`, `actor`, `restore`, `uninstall`, `status`, `reconfigure` | Replaces `cmd/quasar-updater`; one static musl binary, one slim image (`deploy/Dockerfile.recovery`, not on the Vulkan lineage) |

**Changed (Go, `control-plane`)**

| Module | Change |
|---|---|
| `internal/platform` | Apply machines unchanged; the pure planner gains `TargetComponents` (ordered list, replacing `ControlPlaneComponents` / `NodeAgentComponents` at `apply.go:299-322`), floor evaluation (`below_floor`) and external-backup confirmation; an `ActorClient` adapter implements the existing `UpdaterAPI` seam over the control socket |
| socket fixtures | The request/status JSON shapes of the control socket live as JSON fixtures in one directory of this repo (outside the frozen `protocol/` submodule, because the socket is not frozen) and are decoded and encoded by tests in both Go and Rust |

**Changed (Rust, `node-agent`)**

- `release/mod.rs`: POST to the agent socket and poll `GET /v1/status?request_id=` instead of
  reading the shared results directory; accepts `recovery-actor` as a component it may relay.
- `buildinfo.rs`: identity comes from the actor's `/v1/status` (install mode `owned`, actor and
  seed versions), not from Compose labels.
- Readiness `platform_update.rs`: Compose checks retired; `owner_conflict` added.

**Retired (deletion test: the complexity disappears, it does not move to callers)**

The whole Go updater: `control-plane/internal/updater` (Compose discovery, the `.env` rewrite,
the Compose executor, the in-memory latch; its gates and signature verifier after the port has
passed its golden vectors) and `control-plane/cmd/quasar-updater`, `deploy/Dockerfile.updater`,
the `quasar-updater` Compose service and its shared volume, preflight checks `updater_stack_dir` and `updater_overlays`, Compose-label discovery in
`buildinfo.rs`, the static `ENROLLMENT_TOKEN`, and `enroll-host.sh`'s compose/`.env` writing.

### 5.2 The actor interface

```rust
impl Actor {
    /// Admits one Request and, if admitted, journals it and drives it in the
    /// background. Idempotent on request_id (a re-post returns the same Accepted).
    /// Single flight: another open request => Rejection::Busy. Every rejection
    /// happens BEFORE the first journal record, so a rejection changed nothing.
    pub fn submit(&self, caller: Caller, req: Request) -> Result<Accepted, Rejection>;

    /// The machine inventory, plus one Replacement's projected result when
    /// request_id is given. Read-only, bounded; answers stale=true from the last
    /// inventory if the engine is slow.
    pub fn status(&self, request_id: Option<&RequestId>) -> Status;

    /// Runs on every start before any socket opens: take the lease; settle an open
    /// hand-over; drive an open Replacement to a terminal outcome (D8); sweep owned
    /// leftovers the journal does not account for; ensure every service this
    /// machine's role requires exists (first install is this step). Never starts a
    /// new Replacement of its own.
    pub fn resume(&self) -> Result<(), ResumeError>;
}
```

- `Caller` is established by **which socket** a request arrived on: `ControlPlane` (control
  socket), `Agent` (agent socket; may name only `node-agent` and `recovery-actor`, the existing
  confused-deputy guard), `Operator` (CLI subcommands inside the actor image).
- `Request{RequestID, Kind: replace|restore|remove, Components (ordered), Release (provenance),
  Migrates, SchemaVersion, ExternalBackupConfirmed, Dump, Purge, WaitTimeoutS}`.
- `Status{Actor, Seed, Role, Database: owned|external|none, Services[], Conflicts[], InFlight,
  Dumps[], Result, Stale}` — `Result` keeps today's result-file field spellings so the agent's
  `release_state` relay stays a re-frame.
- Invariants: at most one open Replacement per machine (the journal says so, not memory);
  anything journalled reaches a terminal outcome across any number of crashes without a new
  request; components are replaced in request order and a failure stops the sequence; at most
  one running container per role; the actor never stops, renames or removes a container it did
  not create; a control plane is never started against a newer schema.
- *Implementation note (#360):* the binary takes the lease, opens its socket, then runs
  `resume`; every submit is refused `busy` from the lease until `resume` returns, and while any
  journal is unreadable. `status` with no request id returns the latest attempt's result.
- Terminal outcomes: `succeeded`, `failed` (with `restored: true|false`), `interrupted`
  (nothing changed). Reasons: today's closed set plus `recipe_unsupported`, `owner_conflict`,
  `backup_failed`, `backup_unconfirmed`, `interrupted`. On the wire `interrupted` is `state: failed` + `reason: interrupted` (`release_state` has no such state).

### 5.3 Container shape and version skew

- **Recipes.** One per role (`control-plane`, `node-agent`, `postgres`, `recovery-actor`), keyed by
  revision; a revision bumps only when a container needs a new mount, env input, port, device or
  capability. `Inputs` are machine facts (role, node name, home and template roots, public host,
  ports, control URL, GPU facts detected by a disposable probe, database mode, Docker socket
  path) and only ever grow, each with a default.
- **Image label.** `org.quasar.recipe=<int>`, stamped by `deploy/build-images.sh` from a constant
  in the service's source tree; `deploy/image-contract.json` gains the assertion (tightening only).
- **Rule A — the actor moves first.** The planner orders each target's list
  `[recovery-actor?, <service>]` when the actor's digest differs from the release's. On a
  combined host the actor moves in the control-plane step, so the host step is `[node-agent]`.
- **Rule B — a window.** Every actor renders every revision from the release floor up to its
  own, which covers agent revert (D9), restore to a pre-update control plane (D7) and ADR 0004
  restore.
- **Rule C — refuse before stopping.** An unsupported revision (only reachable by a hand-built
  developer apply that omits the actor) fails `recipe_unsupported` after the pull, before any
  stop: nothing changed.
- **Release-time check** (`scripts/release/`): the release's actor supports the recipe labels
  of its control-plane and agent images, its window reaches the floor release, and the floor
  does not strand any component that the previous release could manage (D9).
- **Compose parity test** (transition guard for the contributor lane, D15): the rendered
  node-agent and control-plane specs are compared with the Compose service definitions, in the
  spirit of today's `TestEnrollHostComposeMatchesBase`.

### 5.4 Machine state, secrets and labels

- **`quasar-machine` volume** (mounted only into actors): `machine.json` (installation id, role,
  inputs, database mode), `seed.json` (frozen format 1), `actor.lease` (flock, never unlinked),
  `journal/<request-id>.json` (tmp + fsync + rename per phase), `services/<role>.json` (last
  verified rendered spec — the cache that lets the actor act with the control plane down),
  `dumps/` (last three pre-update dumps, each with its schema version), `secrets/`.
- **Secrets as files (D5).** Generated lazily at first render (database password, secret key,
  the local enrollment secret). Each consumer gets a small **per-service secrets volume** written
  by the actor and mounted read-only into that one container (works on Engine API 1.40; no
  volume sub-paths). The control plane gains `*_FILE` inputs for the database password and the
  secret key; Postgres uses `POSTGRES_PASSWORD_FILE`. External database settings arrive as seed
  inputs (R1-Q1) and are copied into `secrets/` at first boot.
- **Labels** on every owned container: `io.quasar.installation=<id>`,
  `io.quasar.platform-service=<role>`, `io.quasar.recipe=<rev>`, `io.quasar.spec=<sha256>`,
  `io.quasar.attempt=<request-id>` (on kept and successor containers). Names are deterministic:
  `quasar-<role>`, `quasar-<role>.kept`, `quasar-<role>.next`.
- **Race guard (D3).** A container that looks like a Quasar platform service (image repository
  in the allowlist, or a legacy Compose service name) but lacks this installation's labels is a
  `Conflict`: never acted on, reported by `Status`, raised as readiness check `owner_conflict`
  and as an eligibility reason. `.kept` and `.next` containers carrying this installation's
  attempt label are the actor's own.

### 5.5 One Replacement

Phases for one component, each journalled (fsync) **before** it is acted on:

`admitted → [dumped] → pulled → checked (recipe supported) → old_kept (stop, disable restart,
rename .kept) → created → started → verifying → verified → old_discarded → succeeded`

and on failure `restoring → restored` (old container renamed back, restart re-enabled, started).

**Settle on restart (D8)** — pure `settle(journal, observed engine state)`:

| Last journalled phase | Outcome |
|---|---|
| before `old_kept` | remove any partial `.next`; **interrupted, nothing changed** |
| `old_kept` … `verifying` | continue to verification; on failure restore under the rules below |
| `verified` or later | finish discarding; **succeeded** |

**Restore rules** (unchanged in spirit, ADR 0002/0004): node agent and recovery actor — always
restored on a failed verification. Control plane — restored automatically only if the new one
**never passed a health check**; on a **migrating** update that restore goes through the
pre-update dump, and per R1/D7 it is the operator's single `restore` command the failure
message prints, not an automatic path. Postgres is never replaced (R1).

**"Passed a health check"** is defined per recipe: the container's own healthcheck reported
`healthy` at least once (control plane: `/health` 200, which pings the database — [F]
`control-plane/internal/health/health.go:19-39`), and for the agent additionally the agent's
`register` carrying the expected commit, as today ([F] `apply_runner.go:656-699`).

### 5.6 Hand-over (the actor replacing itself)

Design 3's protocol, adopted as is: two processes, one flock lease in `quasar-machine`; the
successor is created as `quasar-recovery.next`, self-checks and writes a ready marker; the old
actor releases the lease and stays running until the successor commits; the successor stops,
disables and renames the old one `.kept`, renames itself, re-binds both sockets, verifies
(serves status; on a GPU host waits for the agent to reconnect), removes `.kept` and only then
rewrites `seed.json`. Its crash table (designs/3 §3.3) is the acceptance matrix. A successor
that never verifies is stopped and the old actor re-enabled — **failed, restored**. The one case
needing a human (successor cannot start at all after the old one was stopped) is covered by a
printed one-line fix and by the seed, which re-creates an actor from `seed.json`'s verified
digest if none exists.

*Implementation note (#362):* the hand-over is the recovery-actor component of an ordinary
attempt journal (`quasar-recovery` `handover.rs`); its settle table is per party (old actor,
successor, or an actor the seed re-created after every actor container was removed) in
`settle.rs`. A successor restarted three times without verifying hands the machine back. An
actor that cannot take the lease waits for it. A hand-over may replace an actor that was
started by hand without the installation's labels (it is the actor handing over), but an
actor carrying Compose labels is declared by an external manager (ADR 0007) and is refused
`owner_conflict`. The successor keeps the running actor's `QUASAR_UPDATER_*` only where machine
state records no release trust, since recorded trust wins. The machine states of each committed phase of a successful hand-over are
seed fixtures (`testdata/recovery/seed/actors/unreleased`).

### 5.7 The seed

- `quasar-recovery seed`: every 30 s, if `seed.json` says `uninstalled`, log and idle; if a
  container labelled `platform-service=recovery-actor` for this installation exists (running or
  stopped, any suffix), do nothing; otherwise create `quasar-recovery` from the **frozen actor
  profile** compiled into the seed path, using `seed.json`'s verified digest, or its own image
  on first install.
- Frozen interface (ADR 0007, "seed interface 1"): `seed.json` format 1, the two labels, the
  profile. A contract test runs the current seed code against machine-state fixtures written by
  every released actor.
- The console's snippet pins the seed by digest; a manager that updates the seed is harmless,
  because the seed never replaces an existing actor. The actor reports the seed's version (D3).

### 5.8 Flows

**Combined / control-only machine first install.** Seed → actor → `Resume` generates secrets,
creates `quasar-postgres` (or records the external database), creates `quasar-control-plane`
with the control socket and the local enrollment secret, then on a combined host creates
`quasar-node-agent` with the agent socket; the agent enrolls with the local secret (D10).
Restoring data first (D4) is `quasar-recovery restore --dump <file>` before the control plane's
first boot, via a one-shot Postgres helper container.

**GPU host.** The one-line script (D13) checks and prepares the host, then runs the seed with the
enrollment string and home root; the actor stores the string as a secret file and creates the
agent; the agent redeems it exactly as today (D6(a): the actor holds no control-plane
identity). A re-run finds the identity in the agent's state volume and does nothing new.

**Control-plane update (fleet run step).** The control plane mints and persists the request id
(as today), `Submit`s `[recovery-actor?, control-plane]` on the control socket, and is killed by
its own replacement mid-poll; success is its next boot reporting the release commit (`Adopt`,
unchanged). If the release migrates: owned database → the actor refuses unless free space
suffices, runs `pg_dump` in a one-shot Postgres helper container into `dumps/`, then proceeds;
external database → the request must carry `ExternalBackupConfirmed` (console checkbox; the
unattended path never migrates) or it is rejected `backup_unconfirmed`.

**Agent update, revert.** Unchanged control-plane side (`release_apply` over the agent
WebSocket); the agent relays to the agent socket; the actor replaces
`[recovery-actor?, node-agent]` and applies ADR 0004 restore; the agent relays `release_state`
from `Status`. Revert orders `[node-agent, recovery-actor?]`. Floors (D9) are evaluated by the
planner: a host whose actor or agent is below the control plane's declared floor reads
`below_floor` and is offered only an update.

**Remove host / uninstall.** Console "remove host" sends `Kind: remove` through the relay;
`quasar-recovery uninstall [--purge]` is the operator path (D12, R1-Q2).

### 5.9 Release publication

- **Manifest format 2 under a new asset name** (`platform-release-manifest.v2.json`) with three
  components (`control-plane`, `node-agent`, `recovery-actor`) and a floor. Format-1 control
  planes read only the old asset name, which RH-06-era releases stop publishing, so they are
  never offered an in-place update. They show nothing (not a fault): `manifest_invalid` is
  reserved but never emitted ([F] `release.go:169`). **No bridge release**: with a handful of
  known testers, release notes and a direct message suffice (YAGNI).
- **Edge.** Old control planes on `edge` resolve branch image tags directly ([F]
  `detect.go:150-200`) and would be offered RH-06 builds. RH-06-era edge builds publish under a
  new tag family (for example `o2-<branch>`, design 1), which old control planes never resolve.
- The recovery-actor image is part of every release (it is no longer tag-named and hand-updated).

### 5.10 Frozen-contract changes (Opus review + owner sign-off on the text)

- **Release manifest:** format 2, new asset name, third component, floor.
- **`agent-api.md`:** `release_apply` may carry `recovery-actor`; `release_state.state`
  `recreating` redefined without Compose; appended reasons (`recipe_unsupported`,
  `owner_conflict`, `backup_failed`, `backup_unconfirmed`, `interrupted`, and ADR 0003's
  `signature_missing`/`signature_invalid` formally added); `register` gains optional
  `install_mode: owned`, actor version/commit and seed version; a remove request for GPU-host
  removal.
- **`control-api.md`:** eligibility reason `below_floor`; preflight check ids `updater_stack_dir`
  and `updater_overlays` retired, `owner_conflict` and `backup_space` added; fleet-apply request
  gains `external_backup_confirmed`; the host body carries actor/seed identity and install mode
  `owned`; retirement of the static enrollment token from the enrollment section.
- **`schema.md`:** nullable columns for actor/seed identity on `hosts` and a backup reference on
  `platform_apply_attempts`; no new tables.
- **ADRs:** 0007 (the seed interface), 0008 (recipes compiled into the actor; actor moves first),
  and an amendment to ADR 0004 (the recovery actor's own restore; the agent restore rule
  unchanged).

The local sockets remain explicitly **not frozen** (`schema.md` §"Not frozen"), so the executor,
recipes, journal and seed-to-actor interface can land before the amendment is signed, behind the
unchanged `release_apply`/`release_state` wire.

---

## 6. Rejected alternatives (and what would revive them)

| Rejected | Why | Revive when |
|---|---|---|
| Fully rendered service specs in Postgres (design 2) | Largest contract; a second desired-state system beside RH-05; more control-plane authority than an image swap | Operators need per-service configuration beyond machine inputs, or TURN/Postgres lifecycle arrives |
| Image-carried templates (designs 2 and 3) | A long-lived vocabulary/format contract and an authority boundary in image-supplied data, for a coupling that costs one file per change | Container shapes change faster than releases, or third parties build Quasar-compatible images |
| Go actor grown from the updater (design 1's language; first recommendation) | Owner chose Rust (A3): one engine client for agent and actor, and the stated direction away from Go | — (the port's cost is accepted and carried as its own slice) |
| A separate seed binary/image (design 1) | A second engine client and image for no behavioural gain | — |
| One shared socket with per-caller tokens (design 1) | Token files to generate, rotate and mount; authority by mount is simpler | — |
| A bridge release that makes old control planes show a fault | Build and release work for four known testers | The user base grows before RH-06 ships |
| Each actor with its own control-plane identity (D6(b)) | Owner chose D6(a) under YAGNI | Field failures show agents breaking outside updates |

---

## 7. Testing seams

| Seam | Adapters | What is proven there |
|---|---|---|
| `Actor` (`submit`/`status`/`resume`) | in-memory `PlatformEngine` fake with crash injection at every phase; temp-dir machine state | Every Replacement path, the D8 settle table at every crash point, restore rules, hand-over crash table, race guard, idempotency, single flight |
| `recipe::render` | none (pure) | Golden specs per role/revision/vendor; Compose parity |
| `trust::admit` | none (pure) | The shared golden vectors ported from the Go updater's tests (allowlist, digests, components, ADR 0003 verify/require, key rotation) all pass before the Go originals are deleted |
| Real `PlatformEngine` adapter | real Docker Engine in the dev container or on a dev host (`make test-rust` and RH-01's real-engine tests) | Pull by digest, create/rename/restart-policy update, one-shot helper wait, volumes |
| Control socket (Go ↔ Rust) | shared JSON fixtures | Both sides encode and decode every request/status fixture identically |
| `platform` planner and apply machines | real Postgres (`make test-db`); `UpdaterAPI` fake | Ordered components, floor, `below_floor`, external-backup confirmation, attempts/`Adopt` unchanged |
| Agent relay | Rust `unix_http` test server | Relay against the new status endpoint, component guard |
| Seed contract | fixtures from every released actor | A frozen seed still works |
| Live hosts (only these prove it) | AMD and NVIDIA test hosts, a Dockge and an Arcane install | Fresh installs of all three shapes; agent/control-plane/actor Replacement with real sessions; kill the actor mid-pull and mid-verify; reboot mid-attempt; migrating update with dump and `restore`; restore into a fresh install; Dockge "update stack" race test |

Simulated tests never stand in for the live rows.

---

## 8. Slice order (for `to-tickets`; refined there)

0. **Contract proposal + ADRs 0007/0008 + 0004 amendment** (Tier 3, blocks dependent slices) and
   **design mockups** (D16).
1. **Extract `quasar-runtime`** from `node-agent` (pure refactor; `make test-rust` green; agent
   behaviour identical).
2. **Port the trust gates** (allowlist, request gates, ADR 0003 verifier) into `quasar-recovery`
   with shared golden vectors; security review.
3. **Actor core behind the unchanged wire:** engine port (real + fake), journal, `submit/status/
   resume`, agent Replacement with ADR 0004 restore. No contract change needed (local socket).
4. **Recipes + GPU-host install:** seed mode, frozen profile, `seed.json`, rewritten one-line
   script, agent enrolls via secret file; live on AMD and NVIDIA.
5. **Combined and control-only install:** Postgres creation, secrets volumes, control-plane
   `*_FILE`, local enrollment secret, control socket; console machine inventory.
6. **Control-plane Replacement (non-migrating)** through the fleet run; actor-first ordering.
7. **Migrating update:** dump, `restore`, external-database confirmation; restore into a fresh
   install.
8. **Hand-over** and release format 2 (new asset, floor, edge tag family, release-time check).
9. **Race guard, uninstall, remove host;** retire the Go updater, its image and its preflights.
10. **Evidence and docs:** Dockge and Arcane installs with the race test; `docs/upgrading.md` and
   install docs rewritten; Compose path documented as contributor-only.

---

## 9. Owner decisions taken at this stage

Recorded in [`2026-09-24-decisions.md`](2026-09-24-decisions.md) §"Architecture-stage decisions":

1. **A1 — the actor may lead the control plane on its own machine** by one release while a
   control-plane replacement is in flight or after one failed and was restored. Agreed.
2. **A2 — no service-spec table;** machine inputs live in machine state, changed with
   `quasar-recovery reconfigure`; no console editor in RH-06. Agreed.
3. **A3 — the recovery actor is Rust.** Agreed (owner's preference over the Go recommendation);
   moving the control plane off Go is recorded as long-term direction, outside RH-06.
