# RH-06 design 1: minimal interface, evolve what exists

Date: 2026-09-24. Baseline `develop` @ `32aee45`. Binding inputs: `docs/rh06/2026-09-24-decisions.md`
(D1–D16 as amended by **R1**: D17 cut, Postgres created but never updated, Portainer dropped,
external database first-class, D6 = option (a), D12 kept) and `docs/rh06/2026-09-24-research.md`.

**Premise.** Evolve the Go `quasar-updater` into the **Recovery actor**. Keep the control
plane's platform apply machines (fleet run, per-host attempt, revert, adopt) and the agent relay
(`release_apply` / `release_state`) essentially as they are. Replace only the Compose executor:
an Engine API executor with a durable journal, rendering each Quasar platform service's
container from a small built-in **recipe** per role, the machine's inputs and a digest. The actor
has three entry points. Compose paths are retired. New frozen-contract surface is kept as small
as it can be.

Notation: **[F]** is a fact about today's code, with `path:line`. **[P]** is a design proposal.
Paths are repo-relative.

---

## 0. The design in one page

```
                 ┌──────── control plane (unchanged apply machines) ────────┐
                 │ fleet run ─► CP attempt ─► SelfApplier ── UpdaterAPI ─┐  │
                 │           └► host attempt ─► agent relay (WS) ──┐     │  │
                 └──────────────────────────────────────────────────┼─────┼──┘
                                                                    │     │ local socket
   GPU host                                          combined / control-only machine
 ┌──────────────────────────────┐                  ┌──────────────────────────────────┐
 │ node agent ──socket──┐       │                  │ control plane ──socket──┐         │
 │                      ▼       │                  │ node agent ────socket───┤         │
 │   RECOVERY ACTOR (Submit /   │                  │                         ▼         │
 │   Status / Resume)           │                  │   RECOVERY ACTOR ── Engine API ── │
 │     │ recipe  │ journal      │                  │     │ recipe │ journal │ pg_dump  │
 │     ▼ Engine API             │                  │     ▼                              │
 │ seed (manager-owned) ────────┘ ensures actor    │ Postgres (created, never updated)  │
 └──────────────────────────────┘                  │ seed (manager-owned)               │
                                                   └──────────────────────────────────┘
```

- **One deep module on each machine**: `actor.Actor` with `Submit`, `Status`, `Resume`. Install,
  update, revert, auto-restore, pre-migration dump, restore, self-replacement and uninstall are
  all one mechanism: a **Replacement** driven from a journal.
- **The knowledge of "how to build service X's container for version V" is compiled into the
  actor** as a recipe book keyed by `(role, recipe revision)`. Each platform image declares the
  revision it needs in an image label. Skew is handled by ordering (**the actor moves first,
  inside the same attempt**) plus a hard refusal before anything is stopped.
- **The seed** is a ~200-line frozen program whose whole contract with the actor is: a label, a
  name prefix, a local image tag used as a pointer, a tombstone volume and an input file.
- **Contract changes are small**: manifest `format_version: 2` (a third component, one floor
  integer), `release_apply` accepting `recovery-actor`, three optional `register` fields, a few
  appended reason/check identifiers, one apply-request boolean, four nullable columns. No new
  wire protocol, no new tables.

---

## 1. Module map

### 1.1 What exists today and what happens to it (deletion test applied)

| Module today | Fact | Fate | Deletion test |
|---|---|---|---|
| `control-plane/internal/updater/plan.go` `Plan()` | [F] pure: uuid, single-flight, closed component table, tag/digest rules, namespace allowlist, signature gate, then `.env` rewrite and two compose commands (`plan.go:204-256`, `:258-312`) | **Split.** The admission half (`:204-256` minus the compose check) moves unchanged into `actor/admit.go`. The `.env` rewrite and `ComposeArgs` are deleted. | Admission earns its keep (it is the trust boundary of ADR 0001). The env/compose half has no caller left. |
| `internal/updater/discover.go` | [F] reads its own `com.docker.compose.*` labels, fails closed (`discover.go:18-72`) | **Deleted.** | Nothing needs a compose project any more; complexity vanishes rather than moving. |
| `internal/updater/env.go` | [F] byte-preserving `.env` rewrite | **Deleted.** | #219: Quasar never reads or writes a manager's `.env`. |
| `internal/updater/exec.go` | [F] compose CLI child process, `.env.prev` non-atomic (`exec.go:86-95`), verdict from post-state, `restoreWorthy` (`:160`) | **Replaced** by `actor/replace.go` + `actor/engine`. The *verdict logic* (never-started vs started, log tail) and `restoreWorthy` survive as pure functions. | The compose executor has no caller; the verdict rules are reused. |
| `internal/updater/result.go` | [F] atomic result files (`:131-186`); single-flight latch and accepted cache **in memory only** (`:53-62`) | **Kept as a projection** of the journal (same file shape, so the agent relay keeps reading it). The in-memory latch is deleted: single-flight is "an open journal exists". | The projection earns its keep (agent relay reads it). The latch is the crash-safety defect. |
| `internal/updater/server.go` | [F] four routes over a 0666 unix socket, "authorisation is the request, never the caller" (`:23-26`) | **Kept as the socket adapter** over the three entry points, plus a per-caller token. | Adapter at a real seam (HTTP vs in-process tests). |
| `internal/updater/signature*.go` | [F] ADR 0003 gate | **Kept unchanged.** | Still the second trust gate. |
| `cmd/quasar-updater/main.go` | [F] discovery → config → listen (`main.go:47-151`), no recovery step | **Evolved** into `cmd/quasar-recovery-actor` (subcommands `run`, `restore`, `uninstall`, `probe`). | — |
| `deploy/Dockerfile.updater` | [F] `docker:29-cli` base, compose plugin, root (`Dockerfile.updater:6-17`) | **Replaced** by `deploy/Dockerfile.actor` (static Go binary on a minimal base: no docker CLI, no compose) and `deploy/Dockerfile.seed`. `image-contract.json` gets two new entries; the updater entry is removed. | The CLI and compose plugin exist only to be shelled out to. |
| `platform/apply_self.go` `UpdaterAPI` | [F] `Present`, `SocketState`, `Self`, `Apply`, `Result` (`apply_self.go:50-61`); install mode from `docker compose config` (`:305-315`) | **Kept** as the seam; its adapter becomes an `ActorClient` speaking the new socket. `Self` returns actor `Status`; install mode is read from the actor's inventory (always `registry` for an owned service). | Existing two-adapter seam (real socket, test fake). |
| `platform/apply_fleet.go`, `apply_runner.go`, `apply_revert.go`, `auto_apply.go` | [F] durable, re-adoptable run/attempt machines | **Kept.** Changes are limited to *which components* a target is sent (`apply.go:299-322`) and the migrating-step backup confirmation. | — |
| `platform/preflight.go` | [F] check ids `updater_socket`, `updater_stack_dir`, `updater_overlays`, `image_resolvable`, `agent_connected`, `health_addr_bindable` (`preflight.go:21-28`) | **Trimmed**: `updater_stack_dir`, `updater_overlays` retired; `owner_conflict` and `backup_space` appended. | The two Compose-drift checks describe a stack that no longer exists. |
| `node-agent/src/release/mod.rs` | [F] validates, acks, POSTs, relays the result file; `APPLIABLE_COMPONENTS = ["node-agent"]` (`mod.rs:46`) | **Kept**; allows `recovery-actor`; socket path and bearer token change. | — |
| `node-agent/src/buildinfo.rs` `discover_install` | [F] `updater_present` from compose project labels (`buildinfo.rs:103-106`, `:221-269`) | **Changed**: `updater_present` = the actor socket answers `Status`; actor and seed versions from the same answer. Compose label code deleted. | — |
| `node-agent/src/readiness/platform_update.rs` | [F] stack-dir and overlay readiness checks | **Trimmed**, and gains `owner_conflict` from actor `Status`. | — |
| `deploy/enroll-host.sh` | [F] 989 lines; writes a compose file and `.env` | **Rewritten small** (D13): host checks, then `docker run` the seed, then wait for the agent to register. | — |
| `deploy/docker-compose.yml` + overlays, `redeploy.sh` | [F] production file today | **Contributor lane only** (D15). `quasar-updater` service removed; such a stack reports `updater_absent` and gets the manual recipe. | — |
| Static `ENROLLMENT_TOKEN` | [F] `control-plane/internal/config/config.go:288` | **Retired** (D10), replaced by a single-use **bootstrap enrollment secret** the control plane imports into `host_enrollments` from a mounted file. Both lanes use it. | — |

### 1.2 New and evolved modules

```
control-plane/
  internal/actor/                 (git mv of internal/updater, then evolved)
    actor.go        Actor: Submit / Status / Resume          ← the module interface
    admit.go        pure admission (from updater/plan.go)
    request.go      Request, Target, Caller, Rejection, Accepted, Result, Status
    replace.go      the Replacement state machine: pure settle() + drive loop
    handover.go     self-replacement phases (inside replace.go's machine)
    journal.go      append-only fsync'd journal (machine-state volume)
    machine.go      machine.json, secrets, services/*.json, actor.json pointer
    lease.go        flock single-writer lease (same pattern as the agent's
                    container_ownership.rs)
    db.go           pg_dump / pg_restore via Engine exec (owned DB only)
    guard.go        race guard: classify containers as current/kept/next/foreign
    server.go       socket adapter (HTTP over unix socket, caller tokens)
    result.go       result-file projection (unchanged file shape)
    signature*.go   unchanged
    recipe/         pure: Book, Render(role, rev, inputs, image) → ContainerSpec
      control_plane.go node_agent.go postgres.go recovery_actor.go inputs.go
      testdata/     golden specs per role × revision × input set
    engine/         the Engine port
      engine.go     interface
      docker.go     stdlib-only Docker Engine API client over the unix socket
      fake.go       in-memory engine with fault and crash injection (exported for tests)
  internal/seed/                  pure Decide() + a 20-line loop
  cmd/quasar-recovery-actor/      subcommands: run (default) | restore | uninstall | probe
  cmd/quasar-seed/                subcommands: seed (default) | restore | uninstall
deploy/
  Dockerfile.actor  Dockerfile.seed   enroll-host.sh (rewritten)
```

[P] The `internal/updater` package is **renamed, not re-written**: `git mv` keeps the history of
the admission, signature and result code. The `internal/actor` package stays stdlib-only as
`internal/updater` is today ([F] `cmd/quasar-updater/main.go:17-19`): no Docker SDK dependency.
The Engine API subset needed is about fifteen endpoints (§1.4).

### 1.3 The Actor interface (the one that matters)

```go
package actor

// Actor is the recovery actor on one machine. Exactly one process on a
// machine holds the lease; only that process may call Submit or Resume.
type Actor struct {
	eng     engine.Engine   // port; real = Docker over unix socket
	machine *Machine        // machine-state volume
	journal *Journal        // machine-state volume, fsync'd
	book    recipe.Book     // compiled-in recipes
	lease   *Lease          // flock on <machine>/actor.lock
	self    Identity        // this binary's version/commit/built_at/gen
	clock   func() time.Time
}

// Submit admits one Request and, if admitted, journals it and starts driving
// it in the background. Idempotent on RequestID: re-posting an id that is
// open or terminal returns the same Accepted (never a second Replacement).
// Single-flight: another open RequestID ⇒ Rejection{busy}. Every Rejection
// happens BEFORE the first journal record, so a rejection changed nothing.
func (a *Actor) Submit(ctx context.Context, c Caller, r Request) (Accepted, *Rejection)

// Status is the machine inventory, and — when requestID != "" — that
// Replacement's projected Result. Read-only; bounded to 5 s of engine calls;
// answers from the last inventory if the engine is slow (stale=true).
func (a *Actor) Status(ctx context.Context, requestID string) (Status, error)

// Resume is the boot entry and runs on every start, before the socket opens:
//  1. take the lease (blocks while another actor holds it);
//  2. settle an open handover (commit if I am the named successor, abort if
//     I am the current actor and the successor never committed, retire if I
//     am neither);
//  3. drive an open Replacement to a terminal outcome (D8 rules, §4.4);
//  4. sweep owned leftovers the journal does not account for;
//  5. ensure every service this machine's role requires exists (install is
//     this step when services/ is empty).
// It never starts a new Replacement of its own (D8: no silent retry).
func (a *Actor) Resume(ctx context.Context) error
```

```go
type Caller int // established by the socket adapter from the bearer token file
const (
	CallerControlPlane Caller = iota // may target control-plane, node-agent, recovery-actor
	CallerAgent                      // may target node-agent, recovery-actor (confused-deputy guard)
	CallerOperator                   // CLI subcommands run from the actor image itself
)

type Kind string
const (
	KindReplace Kind = "replace" // move components to digests (update, revert, developer apply)
	KindRestore Kind = "restore" // load a pg_dump, then run the control plane that matches it
	KindRemove  Kind = "remove"  // uninstall a machine or remove a GPU host (D12)
)

type Request struct {
	RequestID    string   `json:"request_id"`   // uuid, minted and persisted by the caller first
	Kind         Kind     `json:"kind"`
	Components   []Target `json:"components"`   // applied IN ORDER; see §2.3
	Release      Release  `json:"release"`      // provenance only (ADR 0001), unchanged type
	Migrates     bool     `json:"migrates"`     // CP target: ReleaseRunsAMigration (plan.go:167)
	SchemaVersion int     `json:"schema_version"` // the release's; recorded beside a dump
	Dump         *DumpRef `json:"dump,omitempty"` // KindRestore: {id} in machine state, or {path}
	Purge        bool     `json:"purge,omitempty"` // KindRemove; operator caller only
	WaitTimeoutS int      `json:"wait_timeout_s,omitempty"`
}

type Target struct {
	Name   string `json:"name"`   // "control-plane" | "node-agent" | "recovery-actor"
	Image  string `json:"image"`  // bare repository, no tag, no digest
	Digest string `json:"digest"` // sha256:<64 hex>; "" only for KindRemove
}
```

`Status`, `Result`, `Accepted` and `Rejection` keep the field spellings of today's result file
and `release_state` ([F] `result.go:27-47`), so the agent relay is still a re-frame:

```go
type Status struct {
	Actor     Identity    `json:"actor"`      // version, source_commit, built_at, gen
	Seed      *Identity   `json:"seed"`       // nil when no seed container is found
	Role      string      `json:"role"`       // combined | control | gpu
	Database  string      `json:"database"`   // owned | external | none
	Services  []Service   `json:"services"`   // one per owned platform service
	Conflicts []Conflict  `json:"conflicts"`  // D3 race guard; never acted on
	InFlight  *string     `json:"in_flight"`
	Dumps     []Dump      `json:"dumps"`      // pre-update dumps kept (last 3)
	Result    *Result     `json:"result,omitempty"`
	Stale     bool        `json:"stale"`
}
type Service struct {
	Name, Image, Digest, ContainerID, State, Health string
	RecipeRevision int
	SpecHash       string // sha256 of the rendered ContainerSpec
}
```

**Invariants of the interface** (what a caller relies on, and what the tests assert):

1. At most one open Replacement per machine; the journal, not memory, says so.
2. A `Rejection` never follows a journal write. Anything journaled reaches a terminal outcome,
   across any number of crashes, without a new request.
3. Components are replaced in request order. A component that fails stops the sequence;
   components already verified stay on their new digest (each state is valid, §2.3).
4. For each role there is at most one container named `quasar-<role>`, and never two running
   containers of the same role.
5. The actor never stops, renames or removes a container it did not create (§4.6).
6. A control plane is never started against a database whose schema is newer than its binary.

**Error modes** (closed; all but the last are `Rejection` reasons that already exist today in
[F] `updater/plan.go:24-33` or are appended in §9): `invalid`, `busy`, `namespace_rejected`,
`digest_malformed`, `signature_missing`, `signature_invalid`, `recipe_unsupported`,
`owner_conflict`. Terminal failures of a Replacement: `pull_failed`, `backup_failed`,
`recreate_failed`, `never_started`, `unhealthy`, `interrupted`.

**Depth.** Three entry points carry: first install, every update, agent revert, ADR 0004
auto-restore, the pre-migration dump (D7), operator restore (D4, D7), self-replacement,
uninstall (D12), GPU-host removal, crash recovery (D8), the race guard (D3) and the service
inventory the console shows. The caller has to know one request shape and one status shape.

### 1.4 Internal seams

| Seam | Interface | Adapters | Real seam? |
|---|---|---|---|
| **Engine port** `actor/engine` | `ImagePull(ref)`, `ImageInspect(ref)`, `ImageTag(id, repo, tag)`, `ContainerCreate(name, spec)`, `ContainerStart`, `ContainerStop(id, timeout)`, `ContainerUpdateRestart(id, policy)`, `ContainerRename`, `ContainerRemove`, `ContainerInspect`, `ContainerList(labelFilter)`, `ContainerLogsTail`, `Exec(id, argv, stdin, stdout)`, `CopyTo(id, path, tar)`, `VolumeCreate/Inspect/Remove`, `NetworkEnsure`, `Info()` | `docker.go` (real, API ≥ 1.41) and `fake.go` (in-memory, fault + crash injection) | **Yes** — two adapters. |
| **Socket** `actor/server.go` | HTTP: `POST /v1/replace`, `GET /v1/status[?request_id=]`, `GET /v1/healthz` | HTTP adapter; tests call `Actor` directly | Yes (HTTP vs in-process). |
| **Recipe** `actor/recipe` | `Render(role, rev, Inputs, ImageRef) (engine.ContainerSpec, error)`; `Book.Supports(role, rev) bool` | one (pure function) | No seam needed: it is pure. Tested through its interface with golden files. |
| **Journal** `actor/journal.go` | `Open(dir)`, `Begin(req) (*Attempt, error)`, `(*Attempt).Append(rec)`, `Open() (*Attempt, bool)`, `Terminal(outcome)` | one, file-backed; tests use a temp dir | No seam: the temp directory is the test adapter. |
| **Machine state** `actor/machine.go` | `Load/Save machine.json`, `Secret(name, gen)`, `Service(role)`, `SetService`, `Pointer()`/`Commit(pointer)` | one, file-backed | No seam. |
| **CP ↔ actor** `platform.UpdaterAPI` | unchanged method set (`apply_self.go:50-61`) | `ActorClient` (socket) and the existing test fakes | Yes, existing. |
| **Agent ↔ actor** `release::ReleaseManager` | unchanged: POST + result-file relay | real socket; `unix_http` test server | Yes, existing. |

---

## 2. Who knows how to build service X's container for version V (Q2)

### 2.1 The answer

[P] **The recovery actor does, compiled in, as a recipe book.** A recipe is a pure Go function:

```go
package recipe

type Role string // "control-plane" | "node-agent" | "postgres" | "recovery-actor"

// Inputs is everything machine-specific. Versioned; a newer recipe may only
// ADD inputs, each with a default, so an old machine.json always renders.
type Inputs struct {
	Format       int
	InstallID    string // random; stamped on every owned container
	Role         string // combined | control | gpu
	NodeName     string
	HomeRoot     string // D5: the home root path (same-path bind)
	TemplateRoot string
	PublicHost   string // optional; feeds QUASAR_TLS_HOSTS
	ControlPort  int    // default 8080
	TLSPort      int    // default 8443
	ControlURL   string // GPU host: from the enrollment string
	GPU          GPUFacts       // detected by a probe container, not the seed (§7.4)
	Database     DatabaseInputs // Owned, or External{host, port, name, user} (password is a secret)
	DockerSocket string         // default /var/run/docker.sock (RH-07 changes this)
}

type ImageRef struct{ Repository, Digest string }

// Render is total over (role, rev) pairs the Book lists and nothing else.
func Render(role Role, rev int, in Inputs, img ImageRef, sec SecretPaths) (engine.ContainerSpec, error)

type Book interface {
	Supports(role Role, rev int) bool
	Window(role Role) (min, max int)
}
```

`engine.ContainerSpec` is the Engine API create body reduced to what Quasar uses: image
(`repo@digest`), cmd, env (non-secret only), labels, mounts (named volumes, host binds,
read-only flags, volume-subpath not used), devices, device requests (NVIDIA), device cgroup
rules, capabilities, network mode, port bindings, `init`, security options, restart policy,
healthcheck, stop timeout. It is exactly the information in today's compose service blocks
([F] `deploy/docker-compose.yml:53-65`, `:77-275`, `:288-658`; `docker-compose.nvidia.yml:65-138`),
moved into Go.

**Each platform image declares the recipe revision it needs** in an image label,
`org.quasar.recipe=<int>`, stamped by `deploy/build-images.sh` from a constant in the service's own
source tree (`control-plane/internal/buildinfo`, `node-agent/build.rs`). A revision is bumped
**only** when the service needs a different container (a new mount, env input, port, device,
capability). A recipe change and its label bump land in the same commit.

After pulling a target digest and **before touching any container**, the actor inspects the image
label and checks `book.Supports(role, rev)`. If not: terminal `failed` / `recipe_unsupported`,
nothing changed.

### 2.2 Why in the actor and not in the image, the manifest or Postgres

- **The trust boundary sits in the actor.** A recipe grants host power: the docker socket, `/dev`,
  cgroup device rules, `network_mode: host`, `NET_ADMIN`. If an image could declare its own
  mounts (an image-carried spec), any digest in an allowlisted namespace could ask for
  arbitrary host access. With built-in recipes, an image can only choose among container shapes
  the actor already contains. ADR 0001's digest pin covers *what runs*. The recipe book covers
  *what it is allowed to touch*.
- **It is the smallest contract surface.** A spec in the manifest or in Postgres would be a new
  frozen document with its own versioning. A compiled recipe plus one integer label adds no
  frozen document. The label is an image fact that the release-time check verifies.
- **The actor must act with the control plane down** (D5, D6, D7). A spec it renders itself
  needs nothing from the control plane.
- **Deviation from P1, stated for the owner.** P1 says a "versioned service specification (…) held
  in Postgres and cached by the recovery actor". This design keeps the *versioned specification*
  as the triple **(recipe revision, machine inputs, digest)**. The actor journals the rendered
  spec of every container it verifies (`services/<role>.json`, the cache), and Postgres keeps
  what it has today: digests per attempt (`platform_apply_attempts.requested_digests`), settings
  in `instance_settings`, and agent knobs in RH-05 host policy. **No service-spec table.**
  Arbitrary per-knob environment overrides for platform containers are not offered in RH-06.
  A knob that turns out to be needed becomes a machine input (with a default) or a database
  setting. This is the main thing an owner should confirm.

### 2.3 Version skew and release ordering

**Rule A. The actor moves first, inside the same attempt.** A target's component list is ordered
`[recovery-actor, <service>]` whenever the actor's digest differs from the release. The current
actor hands over to its successor (§3.3). The successor, which carries the release's recipe
book, then renders the new service. So the actor that renders the control plane or agent of
release R is always an actor of release R.

- CP target (fleet run control-plane step): `[recovery-actor?, control-plane]`.
- Host target: `[recovery-actor?, node-agent]`. On a combined host the actor already moved in
  the CP step, so the host step is `[node-agent]`.
- The CP computes the list in the pure planner (`apply.go` `TargetComponents`, replacing
  `NodeAgentComponents` / `ControlPlaneComponents` at `apply.go:299-322`). The actor does not
  reorder anything.

**Rule B. Every actor supports a recipe window**, `[rev at the release floor, rev of its own
release]`, for every role. So the newer actor can still render *older* services, which covers
revert (D9), restore to a pre-update control plane (D7) and the ADR 0004 auto-restore of a
kept container that needs re-creating.

**Rule C. Refuse before stopping.** An older actor asked to render a newer revision (possible
only with a hand-built developer apply that omits the actor) refuses with `recipe_unsupported`
after the pull and before any stop. The outcome is "nothing changed".

**Rule D. Recipes only add inputs with defaults**, and secrets are generated lazily on first
render (`machine.Secret(name, generator)`), so a new revision never needs an operator step.

**Revert order** (D9, agent and actor): the planner orders a revert `[node-agent, recovery-actor]`
(the service first, rendered by the newer actor whose window covers it, then the actor hands back).

**Where ADR 0002 sits.** The actor is not schema-bearing, so moving it first does not touch
"control plane first, never below the DB". During a CP-machine attempt the actor is briefly on
release R while the control plane is on R−1. D9's "never ahead of the control plane" rule is
kept as a **rule about what is offered** (revert targets, host eligibility), with this one stated
exception: the actor on the control plane's own machine may be one release ahead while its
control-plane replacement is in flight or has failed and been restored. Its socket accepts
requests from any control plane at or above the floor (the socket is non-frozen but
**additive-only within the floor window**).

**Release-time check** (`scripts/release/`): for the manifest being published, (1) the actor's
book supports the recipe label of the control-plane and node-agent digests; (2) its window reaches
down to the floor release's labels; (3) `floor_schema_version` ≤ the previous published release's
`schema_version`, so no one-step update can strand a host (D9).

---

## 3. The seed and self-replacement (Q3)

### 3.1 What the seed does: three rules, forever

The seed runs as a `docker run` container or as the only service in a Dockge or Arcane stack. Its
inputs are environment variables in that definition: `QUASAR_ROLE`, `QUASAR_HOME_ROOT`,
`QUASAR_PUBLIC_HOST` (optional), `QUASAR_ENROLLMENT` (GPU hosts, single use),
`QUASAR_ACTOR_IMAGE` (`repo@sha256:…`, used only when no pointer exists), and the external-database
variables when chosen (R1-Q1). Its one mount is the docker socket. Every 15 s it evaluates:

```go
package seed

type Observed struct {
	Actors    []ActorContainer // label io.quasar.role=recovery-actor, any state
	Tombstone bool             // volume "quasar-uninstalled" exists
	Pointer   string           // image id tagged quasar-recovery-actor:current, or ""
	NoneRunningFor time.Duration
}
type Action struct {
	Kind  string // "none" | "create" | "start"
	Image string // create: Pointer, else the QUASAR_ACTOR_IMAGE input
	ID    string // start
}

func Decide(o Observed, fallbackImage string) Action {
	switch {
	case o.Tombstone:                     return Action{Kind: "none"} // uninstalled; log "remove this seed"
	case len(o.Actors) == 0:              return create(o.Pointer, fallbackImage)
	case anyRunning(o.Actors):            return Action{Kind: "none"}
	case o.NoneRunningFor >= 2*time.Minute: return Action{Kind: "start", ID: highestGen(o.Actors).ID}
	}
	return Action{Kind: "none"}
}
```

`create` makes `quasar-recovery-actor-seed` with **the seed's frozen actor spec**: the docker
socket, the named volume `quasar-machine` at `/var/lib/quasar-machine`, the named volume
`quasar-actor-run` at `/run/quasar-actor`, restart `unless-stopped`, and labels
`io.quasar.role=recovery-actor`, `io.quasar.gen=0`. Before starting the container, the seed
uploads the inputs as `/run/quasar-seed/inputs.json` with the Engine archive API
(`PUT /containers/{id}/archive`). **No input, and no secret, lands in the actor's environment.**
The engine creates both volumes, so they belong to no compose project, and a manager's
"remove stack (with volumes)" cannot delete them.

The seed has two more subcommands, `restore` and `uninstall`. They are **dispatchers** with no
logic of their own. Each creates a one-off `quasar-actor-cli-<rand>` from the image the pointer
tag names, with the same three mounts plus any operator file bind, runs
`quasar-recovery-actor <subcommand> …`, streams its logs and exits with its code. The logic
lives in the actor and is updated with it. The seed image never needs a new behaviour to gain
one.

### 3.2 The seed–actor interface (frozen, "seed interface 1", recorded as ADR 0007)

| Item | Value |
|---|---|
| Actor discovery | containers with label `io.quasar.role=recovery-actor`; `io.quasar.gen` integer |
| Pointer to the current actor | local image tag `quasar-recovery-actor:current` (the actor retags on every committed handover) |
| Seed-created name | `quasar-recovery-actor-seed` |
| Actor spec the seed creates | the three mounts above + restart `unless-stopped` |
| Input file | `/run/quasar-seed/inputs.json`, `{"format":1, role, home_root, public_host, enrollment, database:{…}}` |
| Tombstone | volume `quasar-uninstalled` |
| Actor CLI | `quasar-recovery-actor restore|uninstall …` in the pointed image |
| Seed identity | image label `org.quasar.seed.interface=1` + `org.opencontainers.image.version` (the actor finds the seed by that label and reports it, D3) |
| `actor.json` v1 | `{gen, image, digest}` in the machine-state volume. Every actor version must be able to read v1 and create the current actor from it (see §3.3 "retire") |

The pointer is a local tag rather than a file so that the seed never mounts the machine-state
volume: the one volume that must never be deleted (D5) is never declared in any manager's stack.
If the pointer is lost (an operator ran `docker image prune -a` after deleting the actor
container), the seed falls back to `QUASAR_ACTOR_IMAGE`. The older actor it creates reads
`actor.json` and recreates the current one (the retire rule), so the fallback is self-healing.

### 3.3 Handover (self-replacement), crash-safe

A handover is a phase of the Replacement that names `recovery-actor`. It is journaled in the
same attempt file.

```
A = current actor, gen n (holds lease).  S = successor, gen n+1.
A1  journal handover.intent{gen:n+1, digest}                          fsync
A2  pull S digest; check recipe label of S (its OWN role, same rule)
A3  create quasar-recovery-actor-<n+1> from recipe(recovery-actor), NOT started
A4  journal handover.started                                           fsync
A5  close socket listener, release lease, start S, wait ≤ 120 s for handover.committed
S1  Resume: acquire lease; read actor.json (current = n) and the open handover (successor = n+1 = me)
S2  self-check: machine.json format readable, recipe window covers every service's recorded rev,
    engine reachable
S3  write actor.json{gen:n+1,…} (tmp, fsync, rename, fsync dir)              ← commit point
S4  journal handover.committed                                         fsync
S5  retag quasar-recovery-actor:current → S image; set A restart=no; stop A (A is the kept old actor)
S6  continue the attempt's remaining components; at terminal remove A's container
A6  (if no commit within 120 s) re-acquire lease — stopping S first if S holds it and is not
    progressing — journal handover.aborted, remove S, reopen socket, finish the attempt as
    failed{component: recovery-actor, restored: true}
```

**Rule on acquiring the lease** (`Resume` step 2). This is the whole crash story:

| I am… | open handover? | action |
|---|---|---|
| `actor.json` current | none | act normally |
| `actor.json` current | yes, not committed | abort it: remove the successor container, journal `handover.aborted`, fail the component with `restored: true` |
| the open handover's successor | yes | S2–S6 (commit forward) |
| neither (a kept predecessor, an aborted successor, a seed-created fallback) | — | **retire**: ensure the `actor.json` current container exists and is running (create it from `actor.json` if missing), then, if my gen > current gen, remove myself; otherwise set my restart policy to `no` and exit 0 |

The lease is an exclusive `flock` on `quasar-machine/actor.lock`. flock works across containers
that share a local volume, and it is the same single-writer pattern as the agent's ownership
lease ([F] `node-agent/src/container_ownership.rs:58-128`). Two actor processes can therefore
never act at once, whatever restart policies do after a reboot. The seed's "start the
highest gen" rule cannot loop, because a highest-gen container that is not current removes itself.

---

## 4. One Replacement: journal and state machine (Q4)

### 4.1 Machine-state volume layout (`quasar-machine`, mounted only into actors)

```
machine.json            {format, install_id, role, inputs, inputs_rev, created_at}
actor.json              {gen, image, digest}                   (the pointer's source of truth)
actor.lock              flock lease
secrets/                0600, root: pg_password, secret_key, bootstrap_enrollment,
                        caller_token_cp, caller_token_agent, db_external_password
services/<role>.json    last VERIFIED: {recipe_rev, image, digest, container_id, spec_hash, spec}
journal/<request_id>.jsonl   one file per Replacement; append-only
dumps/<request_id>.dump + .json   pre-migration pg_dump (last 3) + {schema_version, cp_digest, taken_at, sha256}
```

Secrets reach containers as **files**, never environment variables (D5). The actor copies the
files a service needs into a **per-service secrets volume** (`quasar-cp-secrets`,
`quasar-pg-secrets`, `quasar-agent-secrets`) and mounts it read-only at `/run/secrets/quasar`.
The machine-state volume itself is never mounted into a service. New `*_FILE` inputs:
`QUASAR_DATABASE_PASSWORD_FILE`, `QUASAR_SECRET_KEY_FILE`, `QUASAR_BOOTSTRAP_ENROLLMENT_FILE`,
`QUASAR_ACTOR_TOKEN_FILE` (control plane); `QUASAR_ENROLLMENT_FILE`, `QUASAR_ACTOR_TOKEN_FILE`
(agent); `POSTGRES_PASSWORD_FILE` (upstream).

### 4.2 Journal format

```go
type Record struct {
	Seq       int             `json:"seq"`       // 1.., strictly increasing
	At        time.Time       `json:"at"`
	Phase     Phase           `json:"phase"`
	Component string          `json:"component,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"` // container ids, names, spec hash, dump id, reason
}
```

- `Begin` creates `journal/<request_id>.jsonl` with `O_EXCL`, writes the `accepted` record
  (containing the whole Request), `fsync(file)`, `fsync(dir)`. **Idempotency key:** the
  `request_id` the control plane minted and persisted before sending ([F]
  `apply_store.go:272-295`); a second `Begin` with the same id returns the existing attempt.
- Every `Append` is one JSON line + `fsync(file)`. **Intent records are written and fsynced
  before the action they announce**; completion records after it.
- A torn final line (power loss mid-write) is dropped on read. That is safe because the action it
  announced was never started: the actor acts only after fsync returns.
- Terminal record: `{phase:"terminal", data:{state, reason, restored, output}}`. An attempt file
  without one is **open**; at most one is open (invariant 1).
- The result file (`/run/quasar-actor/results/<id>.json`, [F] shape of `result.go:27-47`) is a
  projection rewritten atomically after every record. Retention: the last 50 terminal journals
  and results.

### 4.3 Phases for one component

Deterministic names: current `quasar-<role>` (e.g. `quasar-control-plane`), new-before-promotion
`quasar-<role>-next`, kept old `quasar-<role>-kept-<id8>`, failed new `quasar-<role>-failed-<id8>`
(`<id8>` = the first 8 hex digits of the request id).

```
accepted
pull.done                      image present locally (pull is idempotent by digest)
check.done                     recipe label supported; no owner_conflict for this role
[backup.intent → backup.done]  CP + Migrates + owned DB only (§5.2)
create_next.intent → .done     create quasar-<role>-next from Render(...) (not started)
stop_old.intent                ── the point of no return ──
  ContainerUpdateRestart(old, "no"); ContainerStop(old); rename old → -kept-<id8>
stop_old.done
promote.intent → .done         rename -next → quasar-<role>; start it
verify.done{verdict}           running + healthy within wait timeout (§4.5)
  success → services/<role>.json := new; remove -kept-<id8>; next component or terminal succeeded
  failure → restore per rule (§4.5)
restore.intent → restore.done | restore.failed
terminal
```

Postgres is never a target in RH-06 (R1). It only goes through `create_next` → `promote` at
install, or when missing.

### 4.4 Restart recovery (D8): `settle()` is pure

`settle(records []Record, obs engine.Snapshot) []Action` decides from the last durable record and
what the engine shows. The drive loop executes the actions, journaling each one:

| Last durable record (current component) | Engine shows | Actions | Outcome |
|---|---|---|---|
| `accepted` … `create_next.done` | old running | remove `-next` if present | `failed` / `interrupted`, **nothing changed** |
| `stop_old.intent` | old still running with its restart policy intact | remove `-next` | `interrupted`, nothing changed |
| `stop_old.intent` | old stopped, or restart policy already `no` | finish stop + rename (each idempotent: skip if already so), then promote | continue to verify |
| `stop_old.done`, `promote.*` | — | finish rename/start (idempotent) | continue to verify |
| `verify.done{ok}` | — | remove kept, write `services/…` | next component / `succeeded` |
| `verify.done{fail}`, `restore.intent` | — | (re)run restore; each step idempotent | `failed` + `restored` true/false |
| handover phases | — | lease rule table, §3.3 | — |

A host reboot mid-attempt is safe for the same reason. The old container's restart policy is set
to `no` **before** it is stopped, so the daemon never brings it back. The new container is
created `unless-stopped`, so if it was started it comes back. Resume then continues
verification. The actor never starts a fresh Replacement on its own (D8).

### 4.5 Verification and restore rules

- **Verdict** (reusing [F] `exec.go:287-371` semantics, now from `ContainerInspect`):
  `StartedAt` zero → `never_started`; running and (healthy, or no healthcheck) within
  `wait_timeout` → ok; else `unhealthy`; a create or start error → `recreate_failed`. Recipes
  carry the healthchecks (CP `/health`; agent health port #152; actor `/v1/healthz`). For a
  migrating CP, the wait timeout defaults to 30 min rather than 300 s, fixing [I] research §4.6.
- **Restore worthiness** (pure, evolving [F] `restoreWorthy`, `exec.go:160-170`):
  - `node-agent`, `recovery-actor`: always (ADR 0004).
  - `control-plane`: when `never_started`, **or** when the schema did not move. For an owned DB
    the actor reads `schema_migrations` through `Exec` in the Postgres container before and after;
    for an external DB it trusts `!Migrates`. A started control plane on a moved schema is
    **never** restored by the actor (invariant 6). The new container is stopped, not left
    crash-looping, and the failure output prints the restore command (§5.3).
- **Restore** = stop the new container, rename it `-failed-<id8>` (log tail captured into
  `output`), rename `-kept-<id8>` back to `quasar-<role>`, set restart `unless-stopped`, start,
  verify, remove `-failed-<id8>`. **No pull is needed**, because the kept container pins its
  image. This fixes research defect §8.3 (restore depended on the old image surviving a prune).

### 4.6 Labels and the race guard (D3)

Every container the actor creates carries:

| Label | Value |
|---|---|
| `io.quasar.install` | `install_id` from `machine.json` |
| `io.quasar.role` | `control-plane` / `node-agent` / `postgres` / `recovery-actor` |
| `io.quasar.request` | the request id that created it |
| `io.quasar.spec` | spec hash |
| `io.quasar.gen` | actors only |

`guard.Classify(containers, journal) → {current, next, kept, failed, leftover, foreign}`:

- **ours** = `io.quasar.install` equals our install id. Among ours, `-next`, `-kept-<id8>` and
  `-failed-<id8>` are **accounted for** only if `<id8>` belongs to the open attempt in the
  journal. So a kept old container is recognised as the actor's own by its **label plus a
  journal reference**, never by its name alone. Ours but unaccounted for → **leftover** →
  removed by `Resume` step 4.
- **foreign** = anything that looks like a Quasar platform service without our install label:
  the name `quasar-control-plane|quasar-node-agent|quasar-postgres|quasar-updater`, a
  `com.docker.compose.service` of those names, or an image repository ending in
  `/quasar-control-plane|/quasar-node-agent`. A foreign container is **never** stopped, renamed
  or removed. It appears in `Status.Conflicts` naming both containers, which surfaces as a
  readiness fault / preflight `owner_conflict`. A Replacement whose role has a foreign
  counterpart is refused with `owner_conflict` at `check.done`, before anything stops. Two agents
  sharing one identity, or two control planes on one database, are exactly what this prevents.
- The agent's own sibling sweep never matches these names ([F] owned prefixes are `quasar-sess-`,
  `quasar-pulse-`, `quasar-probe-`, `container_ownership.rs:10-16`), so the two owners cannot
  collide.

---

## 5. Control-plane replacement on its own machine (Q5)

### 5.1 Non-migrating update: the existing path, new executor

[F] The fleet run's control-plane step already does everything the control plane side needs:
it mints and persists the request id before the call, POSTs, records `previous`, polls, is killed
mid-poll, and treats **"this binary is serving the release's commit"** on the next boot as
success (`apply_self.go:385-495`, `Adopt` `:546-576`). [P] It is unchanged apart from:

- `send` builds `components = TargetComponents(manifest, TargetControlPlane, actorStatus)`,
  i.e. `[recovery-actor?, control-plane]`, and sets `Migrates` and `SchemaVersion`.
- `UpdaterPresent` = the actor socket exists (`/run/quasar-actor/actor.sock`, which only
  actor-created containers have). `InstallMode` = `registry` when `Status.Services` lists
  `control-plane` with a digest. A contributor-lane control plane has no socket, so it reads
  `updater_absent` and is shown the manual recipe (D15).
- The actor's steps: handover if needed, pull, check, create `-next`, stop and keep the old
  control plane (the control plane dies here, mid-poll, as today), promote, verify, remove kept.
  The new control plane boots, `Adopt` sees its commit, and the attempt succeeds. Sessions ride
  through as today (#128: the agent holds them 90 s).

### 5.2 Migrating update: dump first (D7 as amended by R1)

[F] `prepareFleet` already drains the fleet and refuses unattended runs for a migrating release
(`apply_fleet.go:370-385`, `:487-561`). [P] Additions:

1. **Owned database.** Phase `backup` runs before `stop_old`, while the old control plane is still
   running (the fleet is already drained): `Exec(quasar-postgres, ["pg_dump","--format=custom",
   "-U",user,db])`, streamed to `dumps/<id>.dump.partial`, then fsync, `pg_restore --list`
   check, sha256, rename, and a `.json` recording `{schema_version (read from
   schema_migrations), cp_digest (the kept container's), taken_at}`. The version-matched
   `pg_dump` inside the Postgres container is used deliberately. **Free space** is checked
   first: 2× `pg_database_size` + 1 GiB must be free on the machine volume's filesystem.
   Otherwise terminal `backup_failed`, nothing changed. Retention: the last 3 dumps.
   Preflight gains `backup_space` for the CP target, so the console says so before the run.
2. **External database** (R1-Q1). The actor takes no dump. The control plane refuses a migrating
   fleet apply unless the request carries `database_backup_confirmed: true` (console checkbox),
   and records it on the run. Unattended runs never migrate ([F] `auto_apply.go:20-27`), so they
   never need it.
3. **The failure path.** If the new control plane fails after starting on a moved schema, the
   actor stops it, keeps both containers, writes the terminal `failed` result, and puts the exact
   command in `output`:
   `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock <seed image> restore --dump <id>`.
   No control plane is running, so nobody records the attempt until the restore brings the old
   one back. Its `Adopt` (commit ≠ release, request id present) then polls the terminal result
   and records `failed`, exactly as [F] `apply_self.go:560-576` handles an auto-restored control
   plane today.

### 5.3 The `restore` command (D4, D7)

`quasar-recovery-actor restore (--dump <id> | --file <path>) [--secret-key-file <path>]` is a
`KindRestore` Request with `CallerOperator`:

- If a live actor holds the lease, the CLI submits the Request over the socket and streams
  `Status`. Otherwise the CLI takes the lease itself for the duration of the command. Either way
  there is one code path: `Actor.Submit` + drive.
- Phases: `stop_old` on the control plane (kept); `restore.db`: drop and recreate the database
  through `Exec` (`dropdb`/`createdb`), then `pg_restore --exit-on-error` streamed from the file
  ([F] the documented procedure, `docs/upgrading.md:125-132`); choose the control-plane digest:
  the dump's `cp_digest` for `--dump`, or the current one for `--file`. Refuse if the dump's
  `schema_version` > that binary's embedded schema (ADR 0002). Then promote, verify, and
  **re-run local enrollment** (§7.2) so the combined host's own agent re-keys its row under the
  same node name. A restored database may not hold that agent's current secret.
  `--secret-key-file` replaces `secrets/secret_key` (D4 "unless the old key is supplied").
- ADR 0006 applies unchanged: the restored control plane boots a new incarnation.
- **D4 (reinstall with data)** = fresh install, then `restore --file old.dump`. The install script
  accepts `--restore <file>` and runs the command once the install is healthy. A restore before
  the first boot is not required, because the general path migrates the restored schema forward
  on the next boot.

### 5.4 Uninstall (D12, R1-Q2)

`quasar-recovery-actor uninstall [--purge --dump-to <host path>]` is a `KindRemove` Request.
It removes the agent, then the control plane, then Postgres. Then it creates the tombstone
volume `quasar-uninstalled`, so the seed stops re-creating the actor. The CLI container removes
the actor container last, after the actor has journaled `terminal`. Volumes are kept by default.
`--purge` requires the typed confirmation, writes one final `pg_dump` (owned DB) to the named
host path, and then removes `quasar-machine`, the secrets volumes, `quasar-postgres-data`,
`quasar-control-tls` and `quasar-agent-data`. Homes are never touched. The output tells the
operator to remove the seed from the manager. Console "remove host" for a GPU host is the same
`KindRemove` sent through the agent relay (components `[node-agent, recovery-actor]`). The
control plane then deletes the host row, which forgets its credentials.

---

## 6. Agent replacement on GPU hosts (Q6)

[F] The path today: `release_apply` → agent validates → POST to the local socket → relay the
result file as `release_state` → success is the new agent's `register` carrying the release
commit (`apply_runner.go:656-699`). [P] Only three things change:

1. **Components.** `APPLIABLE_COMPONENTS = ["node-agent", "recovery-actor"]`
   ([F] `node-agent/src/release/mod.rs:46`). If `recovery-actor` is present it comes first
   (validated). `control-plane` is still `invalid` at the agent, and now also at the actor
   (`CallerAgent` scope). The confused-deputy guard is enforced in two places.
2. **Socket.** `/run/quasar-actor/actor.sock` with `Authorization: Bearer <caller_token_agent>`
   read from `/run/secrets/quasar/actor_token`. The result files keep their shape.
3. **Success.** Unchanged: the new agent's `register` carrying the release's `source_commit`.
   For an actor-only step (agent already current), success is the agent's next
   `register` reporting the release's `actor_source_commit` (§9). The agent re-reads actor
   `Status` on every connect, as it re-discovers updater presence today ([F] `agent.rs:1864-1872`).

**ADR 0004 auto-restore** keeps its meaning with a better mechanism. A failed agent is restored
by restarting its kept container: no pull, no `.env`. The reply is `release_state{failed,
restored:true}`, the control plane inserts `auto_revert` ([F] `apply_runner.go:588-654`), and
the run still stops. The kept container means the restore cannot fail because of a pruned image.
A failed actor handover is restored the same way (abort, §3.3).

**Recovering an agent broken outside an update** (accepted gap, D6(a)): run
`docker run --rm -v /var/run/docker.sock:/var/run/docker.sock <seed image> restore-agent` on the
host. This is a `KindReplace` to the digest in `services/node-agent.json`, the last verified one.
It is cheap because it is the same machinery, but it stays optional in the slice order. If
omitted, the documented recovery is "re-run the one-liner", which makes `Resume` recreate a
missing agent.

**D9 floor** ([P]): the manifest carries `floor_schema_version`. The control plane compiles in
its own floor as well (`platform.AgentFloorSchema`). A host whose `source_commit` (agent) or
`actor_source_commit` matches a known release row with `schema_version < floor` gets:
- a new fault kind `below_floor` ("must update before it can be managed"), computed with the
  existing `matchRelease` lookup that `agent_ahead_of_control_plane` already uses
  ([F] `plan.go:481-524`);
- eligibility unchanged, so it is **offered the update** and nothing else;
- revert refused below the floor (`apply_revert.go` `PlanRevert` gains the bound).

A commit that matches no release row (a developer build) is not judged: unknown never blocks,
as with ADR 0005's posture.

**RH-05 coexistence.** The control plane already drains and takes a `platform`-owned admission
restriction before sending ([F] `apply_runner.go:247-289`). No RH-05 idle apply can start on a
restricted host, so the host-wide operation lock and a platform replacement never overlap. No
new mechanism is needed.

---

## 7. Enrollment and bootstrap (Q7)

### 7.1 GPU host (D10, D13)

1. The admin clicks **Add host** and the control plane mints a single-use token through the
   existing endpoint ([F] `control-api.md` §"Host enrollment tokens"). The console shows:
   - **One-liner (default):** `curl -fsSL [-k --pinnedpubkey 'sha256//…'] https://<control-plane>/enroll-host.sh | QUASAR_ENROLLMENT='qenr1.…' sudo bash`
   - **Stack tab (Dockge/Arcane):** a seed-only compose snippet with the same inputs.
   - Both fill in `QUASAR_ACTOR_IMAGE` = the actor digest **of the control plane's own release**,
     which satisfies "actor ≤ control plane".
2. `enroll-host.sh` (rewritten, ~200 lines) runs today's host checks: render node,
   `/dev/uinput`, sysctl guidance, AppArmor profile install. It then runs
   `docker run -d --name quasar-seed --restart unless-stopped -v /var/run/docker.sock:… -e … <seed image>`
   and polls the control plane's public health until the host appears. It writes no files.
3. The seed creates the actor. The actor's `Resume` step 5 (install) runs: it reads
   `inputs.json`, writes `machine.json` and secrets, and runs the GPU probe (§7.4). It then
   resolves the agent digest built from **its own source commit**: the release manifest for a
   stable actor, or the `sha-<7>` tag verified against the image's source-commit label for
   developer builds (the mechanism [F] `apply_edge.go:11-56` already uses). Then it runs
   `KindReplace` from absent for `node-agent`, with `quasar-agent-secrets/enrollment` holding the
   `qenr1` string. The agent enrolls **exactly as today** (D6(a) knock-on).
4. **Idempotency.** Re-running the one-liner re-runs the seed. If `machine.json` exists, the
   inputs are ignored, and the agent's node secret in `quasar-agent-data` wins over the token,
   as today.
5. **Lost machine state.** Mint a new token and re-run with the same node name. The existing
   re-key path keeps the host row ([F] #96 refuses while the old identity is connected).

### 7.2 Combined and control-only hosts

- **Front door:** the same script with `--role combined|control` (D13), served from the public
  site, or a seed snippet the site generates. Neither case has a control plane yet, so the
  script or site resolves `QUASAR_ACTOR_IMAGE` from the latest stable format-2 manifest. The site
  installer already resolves digests from the manifest ([F] `site/src/data/stack-template.js:219-251`).
- **Install order** in `Resume` step 5: network `quasar`; Postgres (owned DB only), with its
  password generated into `secrets/pg_password`; the control plane (secrets `pg_password` or
  `db_external_password`, `secret_key`, `bootstrap_enrollment`, `caller_token_cp`); then, for
  combined, the agent.
- **One-time local enrollment secret.** The actor generates `bootstrap_enrollment` (32 random
  bytes). At boot the control plane reads `QUASAR_BOOTSTRAP_ENROLLMENT_FILE` and, idempotently
  by hash, inserts one row into the **existing** `host_enrollments` table: single use, bound to
  this machine's node name, 24 h expiry. The combined agent gets the same value as
  `ENROLLMENT_TOKEN_FILE` with `CONTROL_PLANE_URL=ws://<loopback>:<port>`, which is today's
  combined-host link ([F] `deploy/docker-compose.yml:325`). The static `ENROLLMENT_TOKEN` is
  retired. The contributor lane's `redeploy.sh` switches to the same bootstrap variable, so both
  lanes use one mechanism.
- **Control-only**: no agent. The release view, readiness and preflight must handle "no host on
  this machine". The control-plane target's preflight comes from actor `Status`, so it needs no
  agent.

### 7.3 The seed snippet (illustrative; names are fixed by the specification)

```yaml
services:
  quasar-seed:
    image: ghcr.io/accreleus/quasar/quasar-seed:1
    restart: unless-stopped
    environment:
      QUASAR_ROLE: gpu
      QUASAR_HOME_ROOT: /srv/quasar/homes
      QUASAR_ENROLLMENT: ${QUASAR_ENROLLMENT}
      QUASAR_ACTOR_IMAGE: ghcr.io/accreleus/quasar/quasar-recovery-actor@sha256:<digest>
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
```

A Dockge/Arcane "redeploy" recreates only the seed. The seed finds a running actor and does
nothing. That is the live test of the race guard (D11 as amended).

### 7.4 GPU detection without widening the seed

The actor's seed-given spec has no `/dev` or `/sys` access. On install, the actor runs a
short-lived probe container from its own image (`quasar-recovery-actor probe gpu`) with
`/dev:/host/dev:ro`. It classifies by **device nodes** (`/dev/nvidia0`, `/dev/dri/renderD*`), not
by `/sys/class/drm`, which is not namespaced inside system containers and can show another
tenant's GPU. The recipe then emits NVIDIA device requests and the driver volume
(`quasar-nvidia-driver`, [F] `docker-compose.nvidia.yml:65-138`), or `/dev/dri` for VA/Vulkan.
Detected facts are stored in `machine.json` and re-probed on every `Resume`. A change produces a
readiness warning, never a silent re-render.

---

## 8. Release publication (Q8)

### 8.1 Invisible to format-1 control planes (D4)

[F] A control plane parses the asset named `platform-release-manifest.json`
(`github.go:171`, `:204-209`) and rejects any `format_version` ≠ 1 (`manifest.go:21-23`,
`:76-79`). A rejected manifest is counted as `manifest_invalid` in the detection job summary, and
**the release is not stored** (`detect.go:113-121`). [F] The `manifest_invalid` fault kind is
declared (`release.go:169`) but **never emitted** by `faults()` (`plan.go:481-524`). So D4's
[I] "old control planes should show an unreadable-manifest fault" is **false as the code
stands**: an old console shows nothing new, and only the job's summary carries
`manifest_errors`.

[P]:
- The RH-06 release publishes **only** `platform-release-manifest.json` with
  `format_version: 2`. Every installed control plane drops it and offers nothing.
- **Bridge release (optional, recommended).** The last format-1 release adds one change: the
  detector emits the already-frozen `manifest_invalid` fault when it meets `format_version > 1`,
  with detail "a newer release requires a reinstall; see the release notes". This uses no new
  vocabulary.
- **Edge channel.** [F] Edge resolves the image tag equal to the branch name
  (`edge.go:185-199`) for exactly two components (`edge.go:38-41`), so an old edge control plane
  would be offered an RH-06 build. [P] RH-06 builds publish their branch tag as
  `o2-<branch>` (new `BranchTag`), and the images workflow stops moving `:<branch>`. Old edge
  installs stay on the last pre-RH-06 build. New edge resolves three components.
- Release notes say "reinstall" and link the D4 restore page.

### 8.2 Format 2

```json
{
  "format_version": 2,
  "version": "0.6.0",
  "prerelease": false,
  "source_commit": "<40 hex>",
  "built_at": "2026-10-01T12:00:00Z",
  "schema_version": 96,
  "floor_schema_version": 96,
  "components": [
    { "name": "control-plane",  "image": "ghcr.io/accreleus/quasar/quasar-control-plane",  "digest": "sha256:…" },
    { "name": "node-agent",     "image": "ghcr.io/accreleus/quasar/quasar-node-agent",     "digest": "sha256:…" },
    { "name": "recovery-actor", "image": "ghcr.io/accreleus/quasar/quasar-recovery-actor", "digest": "sha256:…" }
  ]
}
```

- **Three components, normative order.** The seed is not a component: the manager owns it. Postgres
  is not a component either: its pin is a constant in the actor's Postgres recipe (R1), and a
  machine keeps whatever it installed.
- `floor_schema_version` ≤ `schema_version`, and ≤ the previous release's `schema_version`
  (release-time check).
- Recipe revisions are **not** in the manifest. They are image labels, checked at release time
  and at apply time.
- The actor image gains the signing path unchanged (ADR 0003 covers the manifest, which now
  names three digests).

---

## 9. Frozen-contract changes (Q9): the complete list

All of these go through one contract ticket first (the RH-05 #334 precedent), with Opus review
and owner sign-off on the text.

**`protocol/agent-api.md`**
1. §`register` optional identity fields (amendment 1): **add** three optional flat fields,
   `actor_version`, `actor_source_commit`, `seed_version`. **Reword** `updater_present` as "a
   recovery actor answers on this host's local socket", learned from the actor's status and not
   from compose labels. The field name and the `install_mode` values are unchanged.
2. §`release_apply`: `components` may name `recovery-actor` and/or `node-agent`, with
   `recovery-actor` first when present. `control-plane` is still `invalid`.
3. §`release_state`: **reword** `recreating` as "the recovery actor is replacing the container:
   the old one is stopped and kept, the new one created and started", and `recreate_failed`
   (the old container is kept, not gone). **Append** reasons `interrupted`,
   `recipe_unsupported`, `owner_conflict`, and `backup_failed` (control-plane target only, but it
   shares the attempts' reason column and client mapping, like `unsupported`).
4. §Auth / enrollment: remove the static-token alternative and the #199 "falls back once to the
   configured token" path. The mint-and-redeem rules are unchanged.

**`protocol/control-api.md`**
5. §"The release manifest asset": format 2 as in §8.2.
6. §"Platform-release apply (amendment 2)": the control-plane target is applied by the
   **recovery actor** beside it, with components `[recovery-actor?, control-plane]`. A host target
   is `[recovery-actor?, node-agent]`. `up_to_date` means "agent **and** actor on the release's
   commit". A migrating control-plane step takes a pre-update dump for an owned database and fails
   `backup_failed` if it cannot. The started-then-failed control plane on a moved schema is
   restored only by the operator command. The never-started, **or schema-unchanged**, control
   plane is restored automatically. **This widens ADR 0004's control-plane rule and needs an ADR
   0004 amendment.**
7. §`POST /v1/admin/platform/apply`: **add** `database_backup_confirmed` (bool, default false),
   required true when the release migrates and the database is external. Otherwise 409
   `database_backup_unconfirmed`.
8. §Preflight (amendment 9): `PreflightCheckId` **retires** `updater_stack_dir` and
   `updater_overlays` (reserved, never emitted), **appends** `owner_conflict` and `backup_space`,
   and **rewords** `updater_socket`.
9. §Identity read surface: `PlatformHostIdentity` gains `actor_version`, `actor_source_commit`,
   `seed_version`. The control-plane identity gains the same three for its own machine (read
   live from the actor) plus `database_mode` (`owned|external`). `PlatformReleaseFaultKind`
   **appends** `below_floor`. The revert endpoint gains the refusal `revert_below_floor`.

**`protocol/schema.md`**
10. `hosts`: three nullable columns `actor_version`, `actor_source_commit`, `seed_version`.
11. `platform_apply_runs`: `database_backup_confirmed boolean NOT NULL DEFAULT false`.
12. §"Not frozen: the updater's local socket" becomes "…the recovery actor's local socket, its
    journal and the machine-state volume". It stays **not frozen**, with one added promise:
    additive-only within the floor window.

**Outside `protocol/`**
- **ADR 0007 (new)**: the seed interface v1 (§3.2) is frozen, with the owner's sign-off.
- **ADR 0004 amendment**: restore = restart the kept container, and the control-plane rule is
  widened to "or schema unchanged".
- `CONTEXT.md`: **Updater** is retired in favour of **Recovery actor** (the existing
  "Deployment ownership" block). **Recipe** and **Machine inputs** are added as terms. **Preflight**
  loses "stack directory and overlays".

**Not changed:** no new tables; no new WebSocket message types; `release_state`'s shape; the
`host_enrollments` model; `install_mode` values; `EligibilityReason` vocabulary.

---

## 10. Testing seams (Q10)

| What | Where it runs | Adapter |
|---|---|---|
| `recipe.Render` for every role × window revision × input set (AMD, NVIDIA, none; owned and external DB; combined, control, gpu) | `make test-go` | golden files in `recipe/testdata`. Plus `TestRecipeMatchesContributorCompose`: the agent and control-plane recipes agree with the contributor compose services on every mount, device, capability and env name (the drift guard, like [F] `TestEnrollHostComposeMatchesBase`). |
| `admit.go` | `make test-go` | table tests moved from `updater/plan_test.go` |
| `settle()` decision table | `make test-go` | pure; one row per §4.4 row |
| **Exhaustive crash points**: for each script (CP update, CP migrating, agent update, agent + actor, restore, uninstall, handover), crash after each engine op *k* and each journal record *k*, then `Resume` on a fresh `Actor` over the same temp-dir journal and the same `engine.Fake` | `make test-go` | `engine.Fake` + `t.TempDir()`. Asserts invariants 1–6, "terminal reached without a new request", "never two running same-role", "foreign untouched", "kept removed at terminal". |
| Torn journal write | `make test-go` | truncate the last line at every byte offset |
| Handover lease rules, including two actors racing after a "reboot" | `make test-go` | real `flock` on a temp file, two `Actor`s |
| `seed.Decide` | `make test-go` | pure |
| `engine/docker.go` against a real daemon (create/stop/rename/update-restart/archive/exec/labels) | CI job with a Docker daemon, gated `QUASAR_TEST_DOCKER=1` | real adapter; mirrors [F] `node-agent/src/runtime/docker/real_tests.rs` |
| `db.go` dump → restore round trip against `postgres:16` created through the engine | same CI job | real engine + real Postgres |
| Control plane: `TargetComponents`, floor fault, `up_to_date` with actor, backup confirmation refusal | `make test-go` (pure) + `make test-db` (run column, host columns, `Adopt` after restore) | existing `UpdaterAPI` fakes in `platform` tests |
| Agent relay with `recovery-actor`, bearer token | `make test-rust` in `quasar-agent-dev` | existing `unix_http` test server |
| Manifest v2 parse, and "v1 control plane drops v2" (a v1 parser test pinned from the last format-1 tag) | `make test-go` | fixture manifests |

**Only live hosts can prove**: NVIDIA device requests and driver volume, and AMD `/dev/dri`
render and encode on the recipe-built agent; device-node GPU detection inside system containers;
a real reboot and a daemon restart mid-replacement on real GPU test hardware; the handover
under real restart policies; a Dockge and an Arcane stack "redeploy/update" doing nothing
harmful (race guard); the one-liner on a fresh GPU host; a migrating update with a real
database's dump size and time; the 90 s session grace across an actor-driven control-plane
replacement; a D4 reinstall + `restore --file` on the combined host (evidence required by D4).
Per D15 these runs use the product lane (images pushed to a contributor namespace, developer
apply).

---

## 11. Trade-offs

**Where leverage is high**
- **One request shape for every lifecycle verb.** Install, update, revert, restore, uninstall,
  remove-host and self-replace are all "a Replacement of components to digests or absent". One
  state machine, one journal format, one crash-test harness.
- **The control plane barely changes.** The re-adoptable run/attempt machines (#104/#173/#185),
  with their skip-then-continue, owned admission restrictions, `succeeded_partial` and `Adopt`,
  are kept verbatim. Success evidence is still "the new binary reports the commit".
- **The kept-container rule** gives immediate, pull-free restore and makes D8's "continue
  forward after the old stopped" safe.
- **Small frozen surface**: no new wire protocol, no new table, a third manifest component.

**Where it is thin**
- **Recipes in the actor mean every container-shape change is an actor change.** Developers must
  bump `org.quasar.recipe` and ship the actor in the same release. A developer apply that forgets
  the actor fails with `recipe_unsupported`, which is safe but a speed bump.
- **No per-knob env overrides** for owned services (the P1 deviation). Operators who tune
  `QUASAR_*` knobs in `.env` today lose that until a knob is promoted to a machine input or a
  database setting.
- **The floor in schema-version units is coarse.** It can only express "releases at or after
  migration N", and it cannot judge developer builds.
- **The seed pointer is a local image tag**: a deliberate trick to keep the precious volume out
  of managers' stacks. Recovery from a lost tag relies on every actor version reading
  `actor.json` v1, which is now frozen too.

**Risks**
1. **The handover protocol is the most delicate code in the design.** It is mitigated by the
   single flock lease (no two actors ever act), the one lease table (§3.3) and the exhaustive
   crash-point harness. It still needs live reboot evidence.
2. **A local volume is required.** flock and fsync semantics are assumed. A machine volume on NFS
   or another network filesystem is unsupported and should be refused by a readiness check.
3. **Engine API drift.** A hand-written stdlib client must track Docker API behaviour. Pin a
   minimum API version (1.41) and refuse with a readiness fault below it. Podman stays RH-07.
4. **Install-time digest resolution** for the first control plane uses the latest stable
   manifest fetched by the script or site. The actor's own "same commit" rule then pins
   everything else. If that fetch is compromised, the first install is compromised. ADR 0003
   signatures would cover it if the install script verifies them, which is a recommended
   follow-up.
5. **Widening the control-plane auto-restore to "schema unchanged"** relies on reading
   `schema_migrations` for an owned DB and on `Migrates` for an external DB. It is a small,
   evidence-based change, but it needs the ADR 0004 amendment.
6. **Old edge installs** stay frozen on the last pre-RH-06 tag. That is intended, but it must be
   said in the notes for anyone on edge.

---

## 12. Slice order (vertical tracer bullets)

0. **Contract ticket** (Tier 3, blocks slices 4–9 wherever they touch frozen text): the
   amendment text for §9, ADR 0007 (seed v1), the ADR 0004 amendment, and CONTEXT.md.
   **D16 design ticket** runs in parallel (mockups for the inventory, add-host tabs, "must
   update", backup confirmation, developer apply).
1. **Engine-API executor behind the unchanged wire (GPU host, agent only).** Rename
   `internal/updater` → `internal/actor`. Add `engine` (real + fake), `journal`, `lease`,
   `replace` with kept containers, `settle`, the result projection, the crash-point harness and
   `Resume`. Hand-install the actor with `docker run` on a test host and hand-create the agent
   from the first `node-agent` recipe. Proof: a console per-host apply and revert of an agent go
   through the **existing** `release_apply` relay with zero control-plane change, plus ADR 0004
   auto-restore via the kept container. This single slice retires the crash-safety defect.
2. **Recipes + install from inputs (GPU host).** Node-agent recipe (AMD, NVIDIA, none), GPU
   probe, `machine.json`/secrets, secrets volumes, `QUASAR_ENROLLMENT_FILE`, install via `Resume`
   step 5, the recipe/compose drift test.
3. **Seed v1 + rewritten `enroll-host.sh`.** Pointer tag, tombstone, `inputs.json` upload, the
   one-liner and the stack snippet. Proof: fresh GPU host from the console's Add host, both tabs,
   and a Dockge redeploy that does nothing.
4. **Handover.** `recovery-actor` in `release_apply`, actor/seed identity in `register` and the
   host identity, `TargetComponents`, `up_to_date` including the actor. Proof: a host attempt
   `[recovery-actor, node-agent]` plus crash tests, and one live reboot mid-handover.
5. **Control-plane machine, non-migrating.** Postgres/control-plane/actor recipes, `*_FILE`
   inputs in the control plane, bootstrap enrollment import, static token retired (redeploy.sh
   too), `ActorClient` behind `UpdaterAPI`, combined and control-only installs, CP fleet step
   `[recovery-actor, control-plane]`. Proof: fleet run on a combined host with a session riding
   through, and a fresh control-only install.
6. **Migrating update + restore.** `backup` phase, `backup_space` preflight, `backup_failed`,
   the widened restore rule, the `restore` CLI, external-DB confirmation. Proof: a forced
   failing migration restored by the printed command, and a D4 reinstall with `restore --file`.
7. **Publication.** Manifest v2 in the publish workflow, the release-time checks (recipe window,
   floor), `below_floor` fault and revert bound, the `o2-<branch>` edge tags, the bridge release
   fault, developer apply of a three-digest set.
8. **Race guard and removal.** `owner_conflict` readiness/preflight, uninstall/purge, console
   "remove host", Arcane evidence.
9. **Retirement and docs.** Delete the compose updater, `Dockerfile.updater`, the compose
   preflight checks and compose-label discovery in the agent. Rewrite `docs/upgrading.md`,
   `deploy/README.md` and the manual-recipe copy (source lane only). UI slices against the
   approved D16 mockups. CHANGELOG lines per landing.

Slices 1–3 need no contract sign-off: they sit behind the non-frozen socket and the unchanged
`release_apply`/`release_state` wire. That is the payoff of evolving what exists.
