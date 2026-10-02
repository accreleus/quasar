# RH-06 design 3 — "Optimise for the most common caller; reuse the Rust runtime"

Date: 2026-09-24/25. Baseline `develop` @ `32aee45`, `protocol/` @ `3bfce4a`.
Binding inputs: `docs/rh06/2026-09-24-decisions.md` D1–D16 **as amended by R1** (R1 wins:
D17 cut, D6 = option (a), Postgres created but never updated, Portainer dropped, external
database first-class, D7 restore is one operator command, D12 kept).

Conventions. **[F]** = a fact about today's code, with `path:line`. **[P]** = a proposal in
this design. Vocabulary is `CONTEXT.md`'s (Platform service, Service owner, External manager,
Seed, Recovery actor, Replacement, Attempt, Fleet run, Preflight, Install mode, Host identity,
Enrollment, Legacy container) plus the architecture vocabulary (Module, Interface,
Implementation, Depth, Seam, Adapter, Leverage, Locality). Hosts are named by role only.

---

## 0. The design in one page

The two callers that matter most are:

1. **A household operator** who pastes one line (or one seed-only stack into Dockge/Arcane)
   and never thinks about Quasar's containers again.
2. **The control plane's existing fleet run** (`control-plane/internal/platform/apply_fleet.go`)
   moving the instance to a release.

Everything below is arranged so those two callers see the smallest possible interface:

- **One small Rust binary, `quasar-recovery`, in one slim image** (static, `FROM scratch`,
  ~10–15 MB), built from a new crate in the node-agent workspace. It has three roles selected
  by subcommand: `seed` (the frozen, tiny manager-facing container), `actor` (the Recovery
  actor), and operator commands (`restore`, `uninstall`, `status`). One binary, one image,
  two long-running modes.
- **It reuses RH-01's engine facade by extracting it into a GStreamer-free crate**,
  `quasar-runtime`, which both the agent and the actor link. The extraction is cheap because
  the facade is already almost self-contained (§1.2).
- **"How to build service X's container for version V" lives in V's own image**, as a
  declarative service template in an image label covered by the image digest. The actor is a
  generic, version-independent interpreter of a small closed vocabulary. This is the whole
  answer to version skew (§2).
- **The Go control plane keeps its planner and its fleet run unchanged in shape.** The seam
  it already has — `UpdaterAPI` (`apply_self.go:50-63`) — gets a new adapter pointed at the
  actor's socket. The Go Compose updater (`internal/updater`, `cmd/quasar-updater`,
  `Dockerfile.updater`) is deleted; its two host-side gates (namespace allowlist, ADR 0003
  signature) are ported to Rust (§11 prices this honestly).
- **Every Replacement is a durable, phase-journalled attempt** in the machine-state volume,
  using the fsync/rename/lease primitives RH-01 and RH-05 already wrote
  (`runtime/application.rs:222-326`, `policy.rs:1312-1327`), extracted into one reusable
  `DurableFile<T>` (§4).

---

## 1. Module map

### 1.1 Facts the map is built on

- [F] `node-agent` is a **single crate**, not a workspace (`node-agent/Cargo.toml`), and its
  library pulls the whole gstreamer-rs family, `drm`, `alsa`, `input-linux`. Any second binary
  that links `quasar_node_agent` links GStreamer.
- [F] The RH-01 facade (`node-agent/src/runtime.rs` + `src/runtime/**`) reaches outside itself
  in only a handful of places: `crate::container_ownership` (owner token),
  `crate::images::build::{sanitize_relative, MAX_ENTRIES, MAX_EXTRACTED_BYTES}`
  (`runtime/builds.rs:27,73,93,107`), `crate::session::audio::PULSE_NAME_PREFIX`
  (`runtime/docker/legacy.rs:147`), and a doc link to `crate::agent` (`runtime.rs:84`).
  Its dependencies are `bollard`, `tokio`, `serde(_json)`, `sha2`, `libc`, `futures-util`,
  `tracing`, `tar`/`flate2` — none GStreamer.
- [F] Two durable-file implementations already exist and agree on the discipline
  (tmp + `sync_all` + `rename` + parent-dir `sync_all`, `O_NOFOLLOW`, 0600, flock lease):
  `ApplicationJournal` (`runtime/application.rs:222-326`) and `persist_journal`
  (`policy.rs:1312-1327`). RH-05 also has crash-after-fsync injection for tests
  (`policy.rs:266-269`, `:858-871`).
- [F] Ownership: a 64-hex owner token in a flocked lease file, and `owned_id` requires full id
  + owned name prefix + exact owner label (`container_ownership.rs:24-30`, `:127-145`). It is a
  process-global `OnceLock` keyed off `NODE_SECRET_PATH` (`:17`, `:47-53`).
- [F] Self-identification: `self_container_id()` parses `/proc/self/mountinfo`
  (`nvidia_volume.rs:414-428`); inspection types expose daemon host paths of mounts
  (`runtime/inspection.rs`, `DaemonHostPath`).
- [F] On the Go side the control plane already talks to "whoever replaces the control plane"
  through an interface: `UpdaterAPI { Present, SocketState, Self, Apply, Result }`
  (`control-plane/internal/platform/apply_self.go:50-63`), with one production adapter
  (`UpdaterClient`, `:102-126`) and test fakes. Two adapters: a real seam.
- [F] Both platform images already carry image-level `HEALTHCHECK`s
  (`deploy/Dockerfile.control.prod:130`, `deploy/Dockerfile.vulkan:1147-1148`), so health is
  observable through the Engine API without Compose.

### 1.2 Rust modules [P]

`node-agent/Cargo.toml` becomes a workspace root (the agent stays the root package and the
default member, so `deploy/Dockerfile.vulkan:741-745`'s `cargo build --release` is unchanged).

```
node-agent/
  Cargo.toml                    [workspace] members = [".", "crates/*"]; default-members = ["."]
  src/                          the agent (unchanged apart from imports + §6 relay)
  crates/quasar-runtime/        EXTRACTED from src/runtime{.rs,/**} + container_ownership.rs
                                + nvidia_volume::{self_container_id, parse_container_id_from_mountinfo}
                                + NEW durable.rs (DurableFile<T>, StateLease)
  crates/quasar-recovery/       NEW binary crate: seed | actor | restore | uninstall | status
    src/main.rs                 subcommand dispatch; `seed` branch first, minimal init
    src/seed.rs                 the Seed (§3)
    src/machine.rs              machine-state volume layout, secrets, inputs, owner lease
    src/template.rs             ServiceTemplate parse + render (pure) (§2)
    src/engine.rs               PlatformEngine trait (the seam) + DockerEngine adapter
    src/engine/fake.rs          FakeEngine adapter (tests; second adapter)
    src/replacement.rs          the Replacement state machine (§4)
    src/handover.rs             actor-to-successor Replacement variant (§3.3)
    src/gate.rs                 request admission: shape, authority, namespace, signature (pure)
    src/signature.rs            ADR 0003 port (ed25519 via `ring`, already in the lock via rustls)
    src/socket.rs               two unix listeners (control, agent), HTTP/1.1, JSON
    src/postgres.rs             owned-DB dump (pg_dump via exec) and restore (pg_restore via exec)
    src/race_guard.rs           foreign-service classification (D3)
    src/install.rs              first-boot creation per role (§7)
    services/postgres.json      the one built-in template (Postgres is never updated, R1)
    fixtures/socket/*.json      cross-language socket fixtures (§10)
```

The extraction moves the three stray constants into `quasar-runtime` (build limits and the
pulse prefix become parameters of the closed profiles that use them) and replaces the
process-global owner with an explicit `Owner` value passed into `RuntimeConfig`:

```rust
// crates/quasar-runtime/src/ownership.rs   (was src/container_ownership.rs)
pub struct Owner { token: String, _lease: std::fs::File }          // unchanged semantics
impl Owner {
    pub fn acquire(lease_path: &Path) -> Result<Owner, OwnershipError>; // was acquire(), :62-122
    pub fn token(&self) -> &str;
}
pub fn owned_id(v: &serde_json::Value, owner: &str, prefixes: &[&str]) -> Option<String>; // :127-145
pub struct OwnerLabel(&'static str);   // agent: "io.quasar.agent-owner"; actor: "io.quasar.platform-owner"

// crates/quasar-runtime/src/durable.rs  (NEW; the two existing copies collapse into it)
pub struct StateLease { _file: std::fs::File }                    // flock LOCK_EX|LOCK_NB, never unlinked
pub struct DurableFile<T> { path: PathBuf, _t: PhantomData<T> }
impl<T: Serialize + DeserializeOwned> DurableFile<T> {
    pub fn read(&self) -> Result<Option<T>, DurableError>;          // size-capped, O_NOFOLLOW
    pub fn write(&self, value: &T) -> Result<(), DurableError>;     // tmp, fsync, rename, fsync dir
}
#[cfg(any(test, feature = "test-util"))]
pub struct CrashAfter(AtomicUsize);                                  // RH-05's injection, generalised
```

The agent keeps its process-global `OnceLock` as a thin wrapper around `Owner`, so no agent
behaviour changes (deletion test: `container_ownership.rs` shrinks to ~20 lines, its tests move).

**Deletion test for the new crate.** Delete `quasar-runtime` and the agent re-inlines it
unchanged, while `quasar-recovery` would need its own Bollard client, error classifier,
socket discovery (`runtime.rs:136-194`), credential lookup, pull-by-digest, inspection,
ownership and durable files — roughly the whole RH-01 facade again. The crate earns its keep.

### 1.3 The actor's internal interfaces [P]

**`PlatformEngine` — the seam between decisions and Docker.** Two adapters (`DockerEngine`
over `quasar-runtime`, `FakeEngine` in memory) make it a real seam. Blocking, because the
actor is a single-purpose process; each call bridges `quasar_runtime::Operation::wait`
exactly as the agent does today (`runtime.rs:340-376`).

```rust
pub trait PlatformEngine {
    fn host(&self) -> Result<HostFacts, EngineError>;            // /info: Name (host's hostname), runtimes, CDI
    fn image(&self, img: &PinnedImage) -> Result<Option<ImageFacts>, EngineError>;   // id + labels
    fn pull(&self, img: &PinnedImage, deadline: Duration) -> Result<ImageFacts, EngineError>;
    fn find(&self, name: &str) -> Result<Option<ServiceContainer>, EngineError>;
    fn list_owned(&self) -> Result<Vec<ServiceContainer>, EngineError>;  // by platform-owner label
    fn list_all(&self) -> Result<Vec<ContainerSummary>, EngineError>;    // race guard input
    /// Idempotent: an existing container with this name AND this attempt label is returned,
    /// never duplicated; one with this name and another label is `NameTaken`.
    fn create(&self, spec: &ContainerSpec, attempt: AttemptId) -> Result<ContainerId, EngineError>;
    fn inject_files(&self, id: &ContainerId, files: &[InjectedFile]) -> Result<(), EngineError>; // PUT archive, before start
    fn start(&self, id: &ContainerId) -> Result<(), EngineError>;
    fn stop_and_disable(&self, id: &ContainerId, grace: Duration) -> Result<(), EngineError>; // stop + RestartPolicy=no
    fn enable(&self, id: &ContainerId) -> Result<(), EngineError>;   // RestartPolicy=unless-stopped
    fn rename(&self, id: &ContainerId, to: &str) -> Result<(), EngineError>;
    fn remove(&self, id: &ContainerId) -> Result<(), EngineError>;   // owner-gated like owned_id
    fn state(&self, id: &ContainerId) -> Result<ContainerState, EngineError>; // running, health, started_at, exit, restarts
    fn log_tail(&self, id: &ContainerId, lines: usize) -> Result<String, EngineError>;
    fn exec_out(&self, id: &ContainerId, argv: &[&str], sink: &mut dyn Write, deadline: Duration) -> Result<i64, EngineError>;
    fn exec_in(&self, id: &ContainerId, argv: &[&str], src: &mut dyn Read, deadline: Duration) -> Result<i64, EngineError>;
    fn ensure_volume(&self, name: &str, labels: &Labels) -> Result<(), EngineError>;
}
pub enum EngineError { Unavailable, NameTaken { holder: ContainerSummary }, NotOwned,
                       PullFailed(PullFailure), UnknownOutcome, Timeout, Refused(String) }
```

Invariants of the interface: every mutation is addressed by container **id**, never by a
mutable name (the same rule RH-01 applies to images, `runtime/docker.rs:53-80`); `remove`
refuses anything without this installation's owner label; `create` is idempotent under a
retry after `UnknownOutcome` because the attempt label is the identity.

**`render` — the deep module (§2).** One pure function hides every fact that is today spread
over `deploy/docker-compose.yml`, the NVIDIA overlay and `enroll-host.sh`.

**`Replacement` — the state machine (§4).** Interface: `drive(&mut self) -> Outcome`, plus
`recover(journal) -> Replacement` on boot. Everything else (phases, crash rules, restore
rules) is implementation.

**`Gate` — admission (pure).**

```rust
pub fn admit(req: &ApplyRequest, from: SocketRole, host: &GatePolicy,
             open: Option<AttemptId>, evidence: Option<&SignatureEvidence>)
    -> Result<AdmittedRequest, Rejection>;
pub enum SocketRole { Control, Agent }     // which listener the request arrived on
```

Authority is by socket, not by caller claims: the **control** socket (mounted only into the
control plane) may name `control-plane` and `recovery-actor`; the **agent** socket (mounted
only into the node agent) may name `node-agent` and `recovery-actor`. `control-plane` on the
agent socket is `invalid` — the confused-deputy rule the agent enforces today
(`node-agent/src/release/mod.rs:43-46`) is now also enforced by the thing that acts.

### 1.4 Go modules [P]

| Module | Change |
|---|---|
| `internal/platform/apply_self.go` | `UpdaterAPI` renamed `ActorAPI` (same five methods). `UpdaterClient` → `ActorClient` dialling `/run/quasar-actor/control/actor.sock`. `Adopt` (`:532-576`) unchanged. |
| `internal/platform/actorapi/` (new) | The socket's Go types (`ApplyRequest`, `Result`, `Self`), replacing the imports of `internal/updater` in `apply_self.go:18`. Decoded against the Rust fixtures (§10). |
| `internal/platform/manifest.go` | Accepts `format_version` 1 (display only, never offerable) and 2 (the three-component manifest, §8). |
| `internal/platform/plan.go` | Per-target component sets (adds `recovery-actor`), D9 floor (`below_floor` reason), template-format check folded into preflight. |
| `internal/platform/preflight.go` | Check vocabulary change (§9): retire `updater_stack_dir`, `updater_overlays`; `updater_socket` → `actor_socket`; add `template_supported`, `backup_space`, `no_foreign_service`. |
| `internal/platform/apply_fleet.go` | The CP step's request carries `migrating` and `backup` (`owned_dump` / `external_confirmed`); a migrating run on an external DB needs the admin's confirmation flag. |
| `internal/platform/services.go` (new) | Desired service settings store (D5), `platform_service_settings`. |
| `internal/config` | `QUASAR_DATABASE_PASSWORD_FILE`, `QUASAR_SECRET_KEY_FILE`, `QUASAR_SECRET_KEY_PREVIOUS_FILE`, `QUASAR_LOCAL_ENROLLMENT_TOKEN_FILE` (env forms stay for the contributor lane, D15). |
| `internal/hostenroll` | Boot-provisions the local enrollment token (hash insert, idempotent) on combined hosts (§7). |
| `internal/updater`, `cmd/quasar-updater` | **Deleted** (§1.5). |

### 1.5 What is deleted or retired (deletion test applied)

| Deleted | Why it passes the deletion test |
|---|---|
| `control-plane/internal/updater/{discover,env,exec,result,server}.go`, the Compose half of `plan.go`, `cmd/quasar-updater/` | Every line is Compose coupling (research A §3, rows 1–11, 15–16). Its complexity does not reappear elsewhere; it is replaced by `render` + `Replacement`, which exist for different reasons. |
| `internal/updater/{plan.go gates, signature.go, signature_source.go}` | Ported to `gate.rs`/`signature.rs`; the complexity **moves** (it must live on the host). Honest cost in §11. |
| `deploy/Dockerfile.updater`, image-contract role `updater`, `build-images.sh` role `updater` (`:235`, `:440-457`) | Replaced by role `recovery` (`deploy/Dockerfile.recovery`). |
| Compose service `quasar-updater` and volume `quasar-updater-run` in `deploy/docker-compose.yml` | The Compose file survives only as the contributor lane (D15), which is never acted on by an actor. |
| Agent: compose-label discovery in `buildinfo.rs:101-106`, `:162-195`, `:249-266`; compose checks in `readiness/platform_update.rs` | `updater_present` becomes "the actor's agent socket answers" (§6). |
| Preflight `updater_stack_dir`, `updater_overlays` (`preflight.go:23-24`, `:145-212`) | Compose drift detectors with nothing left to detect. |
| Static `ENROLLMENT_TOKEN` (compose-required at `docker-compose.yml:113`, `:326`) | Retired by D10; combined hosts use a boot-provisioned single-use token. |
| `enroll-host.sh`'s compose/.env writer (most of its 989 lines) | Rewritten small (D13): host checks + `docker run` seed + wait. |
| `site/src/data/stack-template.js` stack generator | Becomes a seed snippet generator. |

Retained on purpose: `deploy/docker-compose.yml` + `redeploy.sh` (contributor lane, D15).

---

## 2. Where "how to build service X's container for version V" lives

### 2.1 The problem, concretely

[F] Today that knowledge is ~60 environment entries, 10 mounts, 3 devices, a cgroup rule,
capabilities, `init`, host networking and an NVIDIA overlay for the agent
(`deploy/docker-compose.yml`, agent service; `deploy/docker-compose.nvidia.yml`), and ~50
environment entries, two ports, two volumes and a healthcheck for the control plane. It lives
in files the operator owns, and the updater re-reads them on every recreate, so a recreate
also applies any drift (research A §2 inference). The actor that renders a new release's
container may be older or newer than that release.

Three candidate homes:

| Home | Skew behaviour | Verdict |
|---|---|---|
| The actor's code (a renderer per service) | An **older** actor renders a **newer** CP with the old env/mounts. Every CP config change forces an actor release first. | Rejected: couples every service change to the actor, the one component we want frozen-ish. |
| The running control plane (Go), stored as a desired spec in Postgres | The **old** CP renders the **new** CP's container. Same problem one level up, and nothing renders while the CP is down (D8 recovery, D7 restore). | Rejected. |
| **The image of version V itself** | The renderer is generic; V says what V needs. Skew reduces to "does the actor understand V's template *format*". | **Chosen.** |

### 2.2 The service template [P]

Each platform image carries a label **`io.quasar.service-template`** whose value is a JSON
document. Labels live in the image config, whose digest is covered by the manifest digest, so
ADR 0001's pinned-digest trust (and ADR 0003's signature over the manifest) covers the
template with no new trust mechanism. The actor reads it with an image inspect after the
pull, before anything is stopped.

Source of truth: `deploy/services/control-plane.json`, `deploy/services/node-agent.json`,
`deploy/services/recovery-actor.json`, stamped by `deploy/build-images.sh` as
`--label io.quasar.service-template="$(cat …)"`; `deploy/image-contract.json` gains an
assertion that the label exists and parses (tightening, never relaxing).

The vocabulary is **closed**. A template can only *name* host resources; the actor maps
names to real paths. It cannot express an arbitrary host bind, privileged mode, or a device
outside the enumerated set.

```rust
// crates/quasar-recovery/src/template.rs
pub struct ServiceTemplate {
    pub format: u32,                         // 1 in RH-06; actor declares supported set
    pub service: ServiceKind,                // ControlPlane | NodeAgent | RecoveryActor | Postgres
    pub compat: u32,                         // wire-compat level of this image (D9 floor input)
    pub entrypoint: Option<Vec<String>>, pub command: Vec<String>,
    pub network: NetworkMode,                // Bridge { ports: Vec<PortDecl> } | Host
    pub env: Vec<(String, EnvValue)>,
    pub settings: Vec<SettingDecl>,          // operator-editable keys this version accepts
    pub secrets: Vec<SecretDecl>,            // files injected at create (§7.4)
    pub mounts: Vec<MountDecl>,              // { role: MountRole, target, read_only }
    pub devices: Vec<DeviceRole>,            // Dri | Uinput | KmsgRead | DevInput
    pub device_cgroup_rules: Vec<String>,    // validated against an allowlist ("c 13:* rmw")
    pub cap_add: Vec<Capability>,            // closed enum: NetAdmin | Syslog | SysAdmin(console)
    pub init: bool,
    pub stop_grace_s: u32,
    pub health: HealthDecl,                  // ImageHealthcheck | None; start budget per mode
    pub variants: BTreeMap<GpuVendor, VariantPatch>,  // e.g. nvidia: gpus=all, driver volume, env
}
pub enum EnvValue { Literal(String), Setting(String), SecretFile(SecretName), Machine(MachineFact) }
pub enum MountRole { ControlState, AgentState, NvidiaDriver, PostgresData, ActorControlSocket,
                     ActorAgentSocket, DockerSocket, AgentRuntimeDir, HomeRoot, TemplateRoot,
                     HostOsRelease, HostDev, HostSysKernel }
pub enum MachineFact { NodeName, PublicHost, HomeRoot, TemplateRoot, ControlUrlLocal, DatabaseHost }

pub struct MachineFacts { role: Role, vendors: BTreeSet<GpuVendor>, node_name: String,
                          home_root: HostPath, template_root: HostPath, public_host: Option<String>,
                          docker_socket: HostPath, database: DatabaseMode }
pub struct SettingsSnapshot { revision: u64, values: BTreeMap<String, String> }

pub fn render(t: &ServiceTemplate, img: &PinnedImage, m: &MachineFacts, s: &SettingsSnapshot)
    -> Result<ContainerSpec, RenderError>;
pub enum RenderError { UnsupportedFormat(u32), UnknownMountRole(String), UnknownCapability(String),
    CgroupRuleNotAllowed(String), MissingMachineFact(MachineFact), SettingInvalid { key: String, why: String } }
```

`render` is total and deterministic; its output carries `spec_sha256` (hash of the canonical
JSON), which the actor stamps as a label and journals. Same inputs → same container.

### 2.3 How skew resolves [P]

| Case | What happens |
|---|---|
| New CP image adds an env key or a mount **role the actor knows** | Nothing special. Template format unchanged; the old actor renders it correctly because V described itself. This is the common case and it needs no actor release. |
| New CP image drops a setting | The stored operator value for the dropped key is ignored and logged; the key disappears from the console because the console lists `settings` from the target image's template (the CP reads it off the actor's `/v1/self`). |
| New CP image adds a setting | Rendered with its declared default until an operator sets it. |
| New image needs a **new vocabulary word** (new mount role, new device) | Template `format` bumps. The manifest's `compat.template_format` says so; preflight `template_supported` fails on any machine whose actor lacks it; the fleet run moves that machine's actor first (§5.2 ordering). An old actor that is handed the image anyway refuses with `template_unsupported` **after the pull and before any stop** — nothing changes. |
| Actor is **newer** than the CP it serves (after a failed CP step) | Allowed; the actor renders any format ≤ its own and serves socket versions down to its declared floor (§4.9, §8). |
| Rendering while the CP is down (D8 recovery, restore, reboot) | The actor caches, per service, the last *verified* `{template, image, settings snapshot, spec_sha256}` in `specs/<service>.json`. It never needs the CP to recreate what was running. |

**Postgres is the one built-in template** (`services/postgres.json`, `include_str!`), pinned to
the digest in `deploy/pins.env`, used at install only — R1 cut Postgres replacement.

**Operator settings (D5)** live in Postgres (`platform_service_settings`, §9) and reach the
actor in the apply request as a `SettingsSnapshot` with a revision. On a GPU host they ride
`release_apply` (a new optional field) through the agent relay. The actor validates each value
against the target template's `SettingDecl` (kind, range) before stopping anything.

**Compose parity (transition guard).** A Rust test renders the node-agent template for each
vendor and compares env/mounts/devices with the agent service in
`deploy/docker-compose.yml` + the NVIDIA overlay (the same idea as
`TestEnrollHostComposeMatchesBase`, `enroll_host_compose_test.go:107`). It keeps the
contributor lane (D15) and the product lane from drifting silently.

---

## 3. The seed

### 3.1 What it does [P]

`quasar-recovery seed` is a reconcile loop of about 200 lines:

```
every 30 s (and at start):
  if machine-state volume `quasar-machine-state` exists and seed.json.state == "uninstalled":
      log "Quasar was uninstalled here; remove this container from your manager"; idle
  if any container carries label io.quasar.platform-service=recovery-actor
     AND io.quasar.installation=<id from seed.json>   (running OR stopped, any name suffix):
      do nothing                           # includes a handover in progress (old + successor)
  else:
      image = seed.json.actor_ref@digest  if seed.json exists     (pull if absent; pull by digest only)
            | this container's own image id                        (first install: seed.json absent)
      create `quasar-actor` from the frozen ACTOR PROFILE (below); start it
```

The seed's entire interface to the actor is:

1. **`seed.json`** in the machine-state volume — **frozen format 1**, written only by an actor,
   only at the end of a verified actor Replacement:
   ```json
   {"format":1,"installation_id":"<uuid>","actor_ref":"ghcr.io/.../quasar-recovery",
    "actor_digest":"sha256:…","state":"active"}     // or "uninstalled"
   ```
2. **The frozen actor profile** (compiled into the seed path, never read from a label):
   name `quasar-actor`; restart `unless-stopped`; mounts: the Docker socket at the host path the
   seed itself was given (learned by self-inspection, reusing `self_container_id` +
   `DaemonHostPath`), `quasar-machine-state:/var/lib/quasar-machine`,
   `quasar-actor-control:/run/quasar-actor/control`, `quasar-actor-agent:/run/quasar-actor/agent`,
   `/sys/class/drm:/host/sys/class/drm:ro`; command `actor`; env `QUASAR_SEED_CONTAINER=<own id>`.
3. **Labels** it reads: `io.quasar.platform-service`, `io.quasar.installation`.

The seed never reads a template, never talks to the control plane, never stops or replaces
anything. Its version is reported by the actor (it inspects the seed container named in
`QUASAR_SEED_CONTAINER`) and shown in the console (D3).

### 3.2 Why one binary, two modes — decided [P]

- A separate seed binary would duplicate the engine client (§1.2 deletion test).
- The seed's **behaviour** is what must be frozen, not its bytes. It is frozen by: the tiny
  `seed.json` format, the compiled actor profile, and a contract test
  (`seed_contract_test.rs`) that runs the *current* seed code against machine-state fixtures
  written by every released actor version (fixtures appended per release). A seed image from
  2026 keeps working because it only ever needs `seed.json` format 1.
- `main.rs` dispatches `seed` before any actor initialisation (no socket bind, no lease), so
  actor start-up bugs cannot reach the seed path.
- The manager pins the seed by digest: the console's snippet is generated with the digest of
  the actor image running on the control plane's machine (from its `/v1/self`), so a
  manager redeploy re-creates the *same* seed. A manager that auto-updates the seed image is
  harmless: the seed never replaces an existing actor.
- Image size: static musl binary on `scratch` + CA bundle. Bollard + tokio + hyper + rustls
  (ring) + serde puts it around 10–15 MB [estimate], versus the agent image's 2000 MB ceiling
  (`deploy/image-contract.json`, role `runtime`). The actor updates in seconds and
  independently of GStreamer/CUDA rebuilds.
- Build: `deploy/Dockerfile.recovery`, `FROM rust:1.94.0-alpine AS build` (musl native, same
  `RUST_VERSION` as `Dockerfile.vulkan:113`), then `FROM scratch`. It must **not** build from
  the Vulkan toolchain image: the actor's release cadence must not wait on that lineage.

### 3.3 Handing over to a successor, crash-safely [P]

The actor cannot replace its own container while it is the process doing it, so a
**handover** is a Replacement with two processes and one lease.

Mutual exclusion: `/var/lib/quasar-machine/actor.lease` is a `StateLease` (flock, never
unlinked — the same inode discipline as `container_ownership.rs:1-2`). Only the lease holder
writes journals or acts on the engine. Docker restarting both containers after a reboot is
therefore harmless: flock arbitrates.

```
Phase (journal attempts/<id>.json, kind=Handover)        Actor A (old, lease holder)       Successor B
Admitted                                                   fsync
Pulled   (B's image by digest; B's template read;          fsync
          template.format ≤ A's formats NOT required —
          B renders its own profile; A only needs the
          frozen actor profile + B's template for env)
Created  (quasar-actor.next, label attempt=<id>)            create (idempotent) + fsync
Started                                                    start B + fsync
                                                                                          B boots, sees it is
                                                                                          journal.successor_id,
                                                                                          cannot get lease →
                                                                                          self-checks (engine,
                                                                                          state volume readable,
                                                                                          formats ⊇ journal's),
                                                                                          writes handover/<id>.ready
HandingOver                                                fsync, stop listening, RELEASE lease
                                                                                          acquire lease
SuccessorActive                                                                           fsync (B now owns attempt)
OldKept   (A stop+disable, rename quasar-actor.kept)                                      act + fsync
Renamed   (B: quasar-actor.next → quasar-actor)                                          act + fsync
Verifying (bind both sockets, serve /v1/self,                                             act
           on a GPU host wait for agent `hello`; ≤ 60 s)
Discarding (remove A)  → seed.json updated → Succeeded                                    fsync ×3
```

Recovery rules (whoever holds the lease after a crash reads the journal and compares its own
container id with `old_id` / `successor_id`):

| Crash point | Lease holder on restart | Rule (D8) |
|---|---|---|
| before `Started` | A | stop+remove B if it exists (by id or attempt label) → **interrupted, nothing changed** |
| `Started`, B never writes `.ready` within 60 s | A | same → **failed, nothing changed**, output = B's log tail |
| `HandingOver` fsynced, B dies before acquiring lease | A re-acquires after its 60 s watchdog | A was never stopped: stop+disable+remove B, A resumes → **failed, restored** |
| `SuccessorActive` or later, B crash-loops | A (still running, not yet stopped) gets the lease | successor committed but A not stopped: A stops B, keeps its logs → **failed, restored** |
| `OldKept` or later, B crash-loops | nobody (A is stopped+disabled) | the **seed** sees two actor containers and does nothing; B's restart policy retries B; after B's Nth restart without lease-verified progress B itself restores: re-enable + start A, stop+disable itself → **failed, restored**. If B cannot even start, `quasar-recovery status`/docs give the one-line fix (`docker start quasar-actor.kept`), the only case needing a human. |
| `Discarding` | B | finish removing A; write `seed.json`; **succeeded** |

`seed.json` names a new actor only after verification, so the seed always recreates a
verified actor.

---

## 4. The Replacement state machine

### 4.1 Machine-state volume layout [P]

```
/var/lib/quasar-machine/                 (named volume quasar-machine-state; must never be deleted)
  machine.json          {format, installation_id, role, created_at, node_name, database: owned|external,
                         enrolled_at?}
  actor.lease           StateLease (the actor's single-writer lock)
  owner                 64-hex platform-owner token (Owner::acquire)
  seed.json             FROZEN format 1 (§3.1)
  inputs.json           bootstrap inputs copied from the seed at first boot (no secrets)
  secrets/              0700; files 0600: db_password, secret_key, secret_key_previous,
                        local_enrollment, external_db_password, enrollment (one-time, GPU host)
  specs/<service>.json  last VERIFIED {template, image, settings, spec_sha256}
  attempts/<uuid>.json  one DurableFile<AttemptRecord> per attempt (terminal ones kept 30 days / 100 files)
  attempts/open         DurableFile<Option<Uuid>>: single-flight pointer
  handover/<uuid>.ready successor readiness marker (§3.3)
/var/lib/quasar-backups/                 (named volume quasar-backups; pg_dump output, last 3 pre-update)
```

### 4.2 Record [P]

```rust
#[derive(Serialize, Deserialize)]
pub struct AttemptRecord {
    pub format: u32,                     // 1; an actor meeting a newer format refuses to act
    pub attempt_id: Uuid,                // = control-plane-minted request_id (idempotency key)
    pub kind: AttemptKind,               // Replace | Handover | Install | Restore | Uninstall
    pub service: ServiceKind,
    pub requested: PinnedImage,          // image@digest
    pub previous: Option<PinnedImage>,
    pub migrating: bool,
    pub backup: Option<BackupRef>,       // { file, bytes, sha256, taken_at, schema_version }
    pub settings_revision: Option<u64>,
    pub spec_sha256: Option<String>,
    pub new_id: Option<ContainerId>,
    pub old_id: Option<ContainerId>,
    pub passed_health_at: Option<String>,// RFC3339; the D7 line
    pub phase: Phase,
    pub sequence: u64,                   // monotonic, like RH-05's Record.sequence (policy.rs:79)
    pub outcome: Option<Outcome>,
    pub reason: Option<Reason>,          // the closed release_state vocabulary + §9 additions
    pub restored: bool,
    pub output: BoundedString<8192>,     // same bound as apply_store.go:46-73
    pub started_at: String, pub updated_at: String, pub finished_at: Option<String>,
}
pub enum Phase { Admitted, Pulled, BackedUp, Created, StoppingOld, OldKept, StartingNew,
                 Verifying, Restoring, Discarding, Terminal }
pub enum Outcome { Succeeded, FailedRestored, FailedNeedsRestore, FailedNothingChanged,
                   InterruptedNothingChanged }
```

Wire mapping to `release_state.state` (unchanged vocabulary, amended meaning of
`recreating`): `Admitted` → `pending`; `Pulled`/`BackedUp` → `pulling`;
`Created`…`StartingNew` → `recreating`; `Verifying`/`Restoring`/`Discarding` → `verifying`;
`Terminal` → `succeeded`/`failed`.

### 4.3 Phases, fsync points, acts [P]

**Rule: write the phase (fsync) before the act it names; every act is idempotent and verified
by observation, never by the act's return value.**

| # | Journal (fsync) | Act | Idempotency / observation |
|---|---|---|---|
| 1 | `Admitted` (request, previous, spec inputs) + `attempts/open` | — | re-POST of the same id returns the record; another id → `busy` |
| 2 | — | pull `image@digest` | pull by digest is idempotent; `image()` confirms id |
| 3 | `Pulled` + `spec_sha256` | read label, `render`, validate settings, race-guard check of the target name | pure; fails here ⇒ **FailedNothingChanged** (`template_unsupported`, `foreign_service`, `invalid`) |
| 4 | `BackedUp` + `BackupRef` (migrating CP on owned DB only) | `pg_dump -Fc` via `exec_out` into `quasar-backups/<attempt>.dump.tmp`, fsync, rename | free-space refusal before dumping ⇒ **FailedNothingChanged** (`backup_space`) |
| 5 | `Created` + `new_id` | `create` `<svc>.next` (labels: owner, service, attempt, spec) + `inject_files` (secrets) | lookup by name+attempt label |
| 6 | `StoppingOld` | `stop_and_disable(old)` | `state(old)` not running, policy `no` |
| 7 | `OldKept` + `old_id` | `rename(old → <svc>.kept)` | name lookup |
| 8 | `StartingNew` | `rename(new → <svc>)`, `start(new)` | `state(new).started_at` |
| 9 | `Verifying` (+ `passed_health_at` when first healthy) | health wait (§4.5) | engine health + service hello |
| 10a | `Discarding` | `remove(old)`; update `specs/<svc>.json` | 404 = done |
| 10b | `Restoring` | restore per §4.6 | observation |
| 11 | `Terminal` + outcome; clear `attempts/open` | — | — |

The kept container pins its image against `docker image prune` (Docker never prunes an image a
container references, stopped or not), which fixes research defect #3 ("restore depends on the
previous image still being present") without a pull.

### 4.4 Restart recovery (D8) [P]

On boot the actor takes the lease, then for `attempts/open`:

- phase ≤ `Created` ⇒ remove `<svc>.next` if present (owner+attempt label check) ⇒
  **InterruptedNothingChanged**. (Old container was never stopped.)
- phase ≥ `StoppingOld` ⇒ re-establish every earlier act idempotently (old stopped, disabled,
  renamed; new renamed, started) and continue to `Verifying`; on failure apply §4.6.
- `Verifying` with `passed_health_at` set ⇒ never auto-restore (D7).
- The actor **never** starts a new attempt on its own (D8); the next one is the operator's or
  the unattended schedule's, with a fresh control-plane-minted id.
- A Docker daemon restart looks like a reboot: `unless-stopped` restarts the actor and the
  *active* service containers; kept containers stay down because their policy is `no`.

### 4.5 Verification [P]

| Service | Pass |
|---|---|
| node-agent | container running, image health `healthy` within 120 s, **and** the new agent's `hello` on the agent socket reporting the requested commit. (The control plane's own success evidence stays the new agent's `register`, `apply_runner.go:656-699`.) |
| control-plane, non-migrating | running, `healthy` within 120 s, `hello` on the control socket with the requested commit. |
| control-plane, migrating | first `healthy` within 30 min (a migration runs at boot, `cmd/quasar-control/main.go:60-86`), then healthy for a 30 s settle window. `passed_health_at` = the first healthy observation. |
| recovery-actor | §3.3. |

`hello` is a new socket call the control plane and agent make at boot (not frozen); it is
what makes "passed a health check" precise and fault-testable, as D7 asks.

### 4.6 Restore rules — extends ADR 0004 [P] (new ADR 0007)

| Service / case | Automatic action | Outcome |
|---|---|---|
| node-agent, recovery-actor: `never_started`/`recreate_failed`/`unhealthy` | stop+disable new (rename `<svc>.failed`, keep for logs until next attempt), rename kept back, `enable`, `start` | `failed`, `restored: true` (ADR 0004 unchanged in meaning) |
| control-plane, **non-migrating**, any verify failure | same as above — safe because `release.schema_version == CP.schema_version` (`plan.go:155-169`), so no migration ran | `failed`, `restored: true` **(new: ADR 0007)** |
| control-plane, migrating, `never_started` | same as above (no migration can have run; ADR 0002/0004 rule kept) | `failed`, `restored: true` |
| control-plane, migrating, started but never `passed_health_at` | stop+disable new; **no automatic DB restore** (R1 D7); old stays kept | `failed`, reason `unhealthy`, `FailedNeedsRestore`; output ends with the exact `restore` command (§5.4) |
| control-plane, migrating, passed health then failed | leave new running (restart policy) | `failed`; console/CLI offer the restore of the pre-update dump (D7 (a) bullet 3) |

### 4.7 Race guard (D3) and labels [P]

Labels on every actor-created container:

```
io.quasar.platform-owner=<64-hex owner token>        # the gate for remove/stop (owned_id rule)
io.quasar.installation=<installation uuid>
io.quasar.platform-service=control-plane|node-agent|postgres|recovery-actor
io.quasar.attempt=<attempt uuid that created it>
io.quasar.spec=<spec_sha256>
```

Deterministic names: `quasar-postgres`, `quasar-control-plane`, `quasar-node-agent`,
`quasar-actor`; suffixes `.next`, `.kept`, `.failed`. D2's "one installation per engine" makes
fixed names correct, and the fixed name is itself the collision detector: `create` returns
`NameTaken{holder}` if anything else holds it.

`race_guard::classify(list_all(), owner)`: a container is *foreign* if it looks like a Quasar
platform service (image under an allowed namespace with a Quasar role label
`org.quasar.image.role`, or a `com.docker.compose.service` of `quasar-*`, or a reserved name)
**and** lacks this installation's owner label. Kept/next/failed containers carry the owner
label and are therefore own (D8's last bullet). A foreign container is never touched; it
becomes readiness check `platform_foreign_service` naming both containers, and preflight
`no_foreign_service` blocks a Replacement of that service.

### 4.8 Single flight and the RH-05 host-wide lock

`attempts/open` makes the actor single-flight per machine (refuse, never queue — today's
`busy`). On a GPU host the agent's RH-05 host-wide operation lock is independent: an agent
Replacement ends the agent process anyway, and RH-05's journal already recovers an
interrupted restart attempt on the next boot (`policy.rs:297-371`). The control plane already
serialises platform attempts against idle-apply through owned admission restrictions (0088),
so no new cross-lock is needed. [I]

### 4.9 Socket versioning

Both sockets serve `/v1/…`. The actor declares `socket_versions: [1]` in `/v1/self`, and the
**`recovery-actor` apply verb is frozen forever in shape** — "move the actor to image@digest" —
so an actor below any floor can always be moved forward. That is the D9 stranding guard (§8).

---

## 5. Control-plane replacement on its own machine

### 5.1 Path [P]

The fleet run's control-plane step is unchanged in shape (`apply_fleet.go:338-434`,
`apply_self.go:382-495`): mint `request_id` and persist it **before** the call, POST
`/v1/apply` on the **control** socket, record `previous` from the 202, poll
`/v1/results/{id}` every 2 s, get killed by the replacement, and let the next boot's `Adopt`
(`apply_self.go:532-576`) decide: this binary on the release's commit ⇒ `succeeded`, else keep
polling the actor's journal (which survives the CP) to terminal.

What changes: the socket path (`/run/quasar-actor/control/actor.sock`, a volume only the actor
and the CP mount — D6a), and the request body:

```go
// internal/platform/actorapi/types.go
type ApplyRequest struct {
    RequestID    string       `json:"request_id"`
    Components   []Component  `json:"components"`           // control-plane, and recovery-actor when it moves
    Release      Release      `json:"release"`
    Migrating    bool         `json:"migrating"`            // ReleaseRunsAMigration, plan.go:155-169
    Backup       string       `json:"backup"`               // "owned_dump" | "external_confirmed" | "none"
    Settings     *Settings    `json:"settings,omitempty"`   // {revision, values}
    WaitTimeoutS int          `json:"wait_timeout_s,omitempty"`
}
```

The actor refuses `migrating: true` with `backup: "none"`, and refuses `owned_dump` when
`machine.json.database == external` (and vice versa) — the database mode is the actor's fact,
not the request's.

### 5.2 Ordering within the control-plane machine's attempt

If the release's `recovery-actor` digest differs from the running actor, the CP attempt carries
both components and the actor moves **first** (a handover ends no sessions and is allowed
unattended, D14), then the control plane. The same pair is always what the release tested.

This means the actor can briefly lead the control plane (and stays ahead if the CP step then
fails). **This deviates from D9's literal "recovery actor: same rule as the node agent (≤ the
control plane)"** for the actor on the control plane's own machine. The deviation is safe
because the actor serves socket versions down to its floor (§4.9), and it matters only in
the failure case. **Needs owner confirmation.** The alternative (never move the actor ahead
of the CP) forces every template-format bump into two releases.

### 5.3 Migrating update with `pg_dump` first (D7 as amended by R1)

Owned database:
1. Fleet run drains as today (`apply_fleet.go:487-561`); unattended refuses (`auto_apply.go:20-27`).
2. Actor `Admitted` → pull → render (nothing stopped yet, the CP still serves).
3. Free-space check: `exec_out(postgres, ["psql","-Atc","select pg_database_size(current_database())"])`
   compared with `statvfs` of `/var/lib/quasar-backups`; require 2× the reported size + 1 GiB.
   Fail ⇒ `FailedNothingChanged`, reason `backup_space`.
4. `pg_dump --format=custom` via `exec_out` into `<attempt>.dump.tmp`; fsync; rename; record
   `BackupRef{schema_version = CP's current}`; keep the last three pre-update dumps. A dump
   failure ⇒ `FailedNothingChanged`, reason `backup_failed`. Only now is anything stopped.
5. Replace as §4.3; restore rules §4.6.

External database (R1-Q1): the actor never dumps, restores or resets. The console's fleet
apply for a migrating release shows a required "I have a current backup of my database"
checkbox; `POST /v1/admin/platform/apply` gains `external_backup_confirmed: true`
(control-api amendment); the fleet run sends `backup: "external_confirmed"`. Without it:
409 `external_backup_unconfirmed`. Unattended apply never migrates, so it never asks.

### 5.4 The `restore` command [P]

One command serves D4 (restore into a fresh install) and D7 (abandon a migrating update):

```
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  <quasar-recovery image> restore --attempt <uuid>            # D7: the dump that attempt took
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v /path/dir:/restore:ro \
  <quasar-recovery image> restore --file /restore/quasar.dump --before-first-boot   # D4
```

- It is a thin Engine-API client. If `quasar-actor` is running, it `exec`s
  `quasar-recovery actor-cmd restore …` inside it, so the restore runs under the actor's lease
  and journal (kind `Restore`). If no actor exists, it acquires the lease itself by running as
  a short helper container that mounts `quasar-machine-state`.
- Sequence (owned DB only; refused for external): journal `Restore` → stop+disable the CP
  (keep) → ensure `quasar-postgres` running → stream the dump into
  `pg_restore --clean --if-exists --exit-on-error` via `exec_in` → start the CP version whose
  schema matches the dump (`BackupRef.schema_version`; for `--attempt`, the kept old CP of
  that attempt) → verify → terminal. The invariant "an older CP never runs against a newer
  schema" holds because the CP started is the one the dump was taken under.
- `--before-first-boot` (D4): writes `machine.json.restore_pending`, which makes first-boot
  install (§7) create Postgres, run the restore, and only then create the control plane.
  ADR 0006's boot incarnation then expires unstarted approvals on the CP's first boot.
- The failure message of §4.6 prints the exact `--attempt` form.

### 5.5 Uninstall (D12, R1-Q2) [P]

```
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock <image> uninstall [--purge --confirm <installation-id>]
```

- Removes owned containers in reverse order (agent, control plane, Postgres, actor), including
  `.kept/.next/.failed`; keeps `quasar-machine-state`, `quasar-postgres-data`,
  `quasar-control-state`, `quasar-agent-data`, `quasar-backups` and homes; writes
  `seed.json.state = "uninstalled"` so a surviving seed idles instead of re-creating the actor.
- `--purge` requires the typed installation id, takes one final `pg_dump` first (owned DB),
  **refuses while a seed container is running** ("remove the seed from your manager first"),
  then removes the volumes. Without that refusal the seed would see no machine state and
  reinstall.
- Console "remove host" (GPU hosts): drain → `host_uninstall` over the agent WebSocket (new
  message, §9) → agent relays to the actor's agent socket → actor removes the agent, writes
  the tombstone, removes itself last → control plane deletes the host row.

---

## 6. Agent replacement on GPU hosts through the agent relay (D6a)

### 6.1 Path [P]

Unchanged on the wire and in the control plane (`apply_runner.go`):
`release_apply` → agent validates (`release/mod.rs:541-560`) → POST to the **agent** socket
`/run/quasar-actor/agent/actor.sock` → `ack{ok:true}` → `release_state` relayed from
`GET /v1/results/{id}` instead of a result file in a shared volume → success is the new agent's
`register` with the release commit (unchanged).

Changes inside the agent (`node-agent/src/release/mod.rs`):
- `DEFAULT_SOCKET`/`DEFAULT_RESULTS_DIR` (`:26-27`) → the actor's agent socket; the
  result-file reader (`:373-443`) becomes an HTTP GET over the existing `unix_http` client.
  The adopt-on-connect behaviour ADR 0004 needs (`:142-203`) is kept: on connect, the agent asks
  the actor for non-terminal/recent attempts for its component and adopts them.
- `APPLIABLE_COMPONENTS` (`:46`) gains `recovery-actor`.
- On boot the agent calls `POST /v1/hello {service:"node-agent", commit, compat}` and, if
  `enrolled`, the actor deletes the one-time enrollment file (§7.2).
- `updater_present` becomes "the actor's agent socket answers `/v1/self`" (replacing
  compose-label discovery, `buildinfo.rs:101-106`). `install_mode` stays `registry` when the
  running image reference is digest-pinned (`buildinfo.rs:196-213`), which it always is on
  an owned install.

### 6.2 ADR 0004 automatic restore

Carried unchanged in meaning by §4.6 row 1, now with a durable journal: a crash in the middle
of a restore is itself resumed on the actor's next boot (the Go updater could not, research A §2
inference). The kept container means the restore needs no registry and no image-store luck.
The control plane's `auto_revert` row logic (`apply_runner.go:588-654`) is untouched.

### 6.3 D9 floor on hosts

The manifest declares `floor.node_agent` and `floor.recovery_actor` as integer compat levels
(§8). Each image carries `io.quasar.compat`; the agent reports both levels on `register`
(new optional fields). `PlanRelease` adds host reason **`below_floor`**: such a host is not
failed; it is offered only an update ("must update before it can be managed"), and a revert
never offers a digest set below the floor. A host whose actor is below floor can still be
moved: the `recovery-actor` apply verb never changes shape (§4.9), and the agent relays it.

### 6.4 An agent broken outside an update (accepted D6a gap)

One local command: `docker exec quasar-actor quasar-recovery actor-cmd recreate node-agent`
(recreates from `specs/node-agent.json`, the last verified spec), documented and printed by
`status`. D6(b) remains the upgrade path.

---

## 7. Enrollment and bootstrap (D2, D5, D10, D13)

### 7.1 First boot of an actor, per role [P]

The actor reads inputs from the seed container named in `QUASAR_SEED_CONTAINER` (its
`Config.Env` via inspect) **once**, copies them into `inputs.json`/`secrets/`, and never reads
manager configuration again (R1-Q1; D10 "an identity already in the machine-state volume wins
and the token is ignored").

Seed inputs (variable names illustrative until the spec fixes them):

| Input | Shapes |
|---|---|
| `QUASAR_ROLE` = `combined` \| `control` \| `gpu` | all |
| `QUASAR_HOME_ROOT` (host path) | combined, gpu |
| `QUASAR_PUBLIC_HOST` (optional) | combined, control |
| `QUASAR_ENROLLMENT` (`qenr1.…`, single-use, 1 h) | gpu |
| `QUASAR_NODE_NAME` (optional; default = engine `/info` `Name`, i.e. the host's hostname) | combined, gpu |
| `QUASAR_DATABASE_HOST`/`_PORT`/`_USER`/`_NAME`/`_SSLMODE`/`_PASSWORD` (optional ⇒ external DB) | combined, control |
| `QUASAR_ALLOWED_NAMESPACES` (optional; D15 developer lane) | all |
| `QUASAR_SIGNATURE_MODE`, `QUASAR_TRUSTED_KEYS` (optional; ADR 0003) | all |

The allowlist and signature trust are **machine-local seed inputs, deliberately not console
settings**: they are the host's defence against a compromised control plane, so the control
plane must not be able to widen them.

`install.rs` (journalled as `AttemptKind::Install`, every step idempotent, so re-running the
seed or crashing mid-install converges):

| Step | combined | control | gpu |
|---|---|---|---|
| owner token, installation id, `machine.json`, `seed.json` (pointing at its own image) | ✓ | ✓ | ✓ |
| generate `db_password`, `secret_key` (0600) unless external DB | ✓ | ✓ | – |
| generate `local_enrollment` token | ✓ | – | – |
| volumes (`quasar-postgres-data`, `quasar-control-state`, `quasar-agent-data`, `quasar-backups`, NVIDIA driver volume when vendor = nvidia), labelled | ✓ | ✓ | ✓ (agent ones) |
| verify `QUASAR_HOME_ROOT` exists and is writable with a short helper container (the pattern of `nvidia_volume/host_path.rs:9-50`) | ✓ | – | ✓ |
| create Postgres from the built-in template; wait healthy; `restore_pending` ⇒ restore | ✓ (owned) | ✓ (owned) | – |
| create CP from the digest in the actor image's `io.quasar.release` label | ✓ | ✓ | – |
| create agent (vendor variant from `/host/sys/class/drm` + engine runtimes/CDI facts, `runtime.rs` `EngineFacts`) | ✓ | – | ✓ |

**Which release gets installed.** The actor image is built last in the release workflow and
carries `io.quasar.release` = `{version, source_commit, components: [control-plane, node-agent]
digests}`. So "install release R" is exactly "run seed R", and the console's Add-host snippet
installs the release the control plane is on (it reads the digest from its own machine's
actor). Developer builds (D15) get the same label from `build-images.sh`.

GPU vendor detection is advisory: `/sys/class/drm` is not namespaced in every container
runtime, so the rendered agent may request access for a vendor whose device nodes are absent;
the agent's own readiness checks remain the authority (ADR 0005).

### 7.2 Enrollment [P]

- **GPU host (D10, R1 knock-on):** the actor holds no control-plane identity. It injects the
  enrollment string into the agent as a file (`QUASAR_ENROLLMENT_FILE`, new agent config, the
  `*_FILE` twin of `QUASAR_ENROLLMENT`, `config.rs:45-50`). The agent enrolls exactly as today
  (`agent.rs:5033-5055`) and persists its node secret in `quasar-agent-data`. After the
  agent's `hello{enrolled:true}` the actor deletes `secrets/enrollment` and records
  `enrolled_at`. Re-running the seed with another token changes nothing (inputs are read once).
  Lost `quasar-agent-data`: mint a new token; the same node name re-keys the host row
  (`agentws/store.go:162-182`).
- **Combined host (D10):** the actor generates `local_enrollment`, injects it into the CP as
  `QUASAR_LOCAL_ENROLLMENT_TOKEN_FILE` and into the agent as its enrollment file with
  `CONTROL_PLANE_URL=ws://localhost:8080`. On boot the CP inserts
  `sha256(token)` into `host_enrollments` (`max_uses 1`, bound to `machine.json.node_name`,
  no expiry, `note = "local"`) with `ON CONFLICT (token_hash) DO NOTHING`
  (`hostenroll/store.go:80-121` shape). The static `ENROLLMENT_TOKEN` is gone.
- **Control-only host:** no agent, no token.

### 7.3 One install script (D13) [P]

`enroll-host.sh` (≈150 lines instead of 989): (1) host checks as today — render node,
`/dev/uinput`, sysctls, AppArmor profile — printed or applied; (2)
`docker run -d --name quasar-seed --restart unless-stopped -v <socket>:<socket> -e … <image>@<digest> seed`;
(3) waits on `docker exec quasar-actor quasar-recovery status --wait enrolled|serving` and
reports. It writes no compose file, no `.env`, no install directory. `--role combined` is the
site installer's command. The Dockge/Arcane tab shows the equivalent single-service stack.

### 7.4 Secrets as files (D5) [P]

`inject_files` uploads a tar (`PUT /containers/{id}/archive`, Engine API ≥ 1.40, within the
floor `runtime.rs:251-254`) into the **created, not yet started** container at
`/run/quasar-secrets/<name>` with mode 0400 and the service's uid. The control plane reads
`*_FILE` (new in `internal/config`), Postgres reads `POSTGRES_PASSWORD_FILE`, the agent reads
`QUASAR_ENROLLMENT_FILE`. No secret is in any container's `Config.Env`, any label, or any
manager configuration — except the external DB password the operator chose to put in the
seed's environment (R1-Q1). If the owner reads D5's "mounted" literally, the alternative is a
per-service named secrets volume written by the actor; it costs one volume per service and
buys nothing observable. **Flag for the specification.**

---

## 8. Release publication (D4) and the new manifest

### 8.1 Format 2 [P]

Same asset name, `platform-release-manifest.json`:

```json
{
  "format_version": 2,
  "version": "0.3.0",
  "prerelease": false,
  "source_commit": "<40 hex>",
  "built_at": "2026-10-…Z",
  "schema_version": 95,
  "components": [
    { "name": "control-plane",  "image": "ghcr.io/accreleus/quasar/quasar-control-plane", "digest": "sha256:…" },
    { "name": "node-agent",     "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": "sha256:…" },
    { "name": "recovery-actor", "image": "ghcr.io/accreleus/quasar/quasar-recovery",      "digest": "sha256:…" }
  ],
  "compat": { "template_format": 1, "node_agent": 1, "recovery_actor": 1 },
  "floor":  { "node_agent": 1, "recovery_actor": 1 }
}
```

- Order is normative, as today. No Postgres entry (R1).
- `compat` = levels implemented by this release's images (duplicated from their
  `io.quasar.compat` labels so planning needs no pull). `floor` = the oldest levels this
  control plane still manages. Integers, not versions, because edge builds have no version.
- Release-time check (D9): `generate-platform-release-manifest.sh` refuses `floor` > the
  previous stable release's `compat`, and CI runs "previous release's actor hands over to this
  release's actor" against a real engine — the proof that nothing is stranded.

### 8.2 Invisibility to format-1 control planes [P]

- **Stable/beta:** [F] `ParseManifest` rejects any `format_version` other than 1
  (`manifest.go:21-23`, `:77-80`) and `Detect` counts it as `ManifestInvalid` and does not
  store the release (`detect.go:115-121`). So an installed control plane is never offered the
  release, and its detection error reads "manifest format_version 2 is not understood by this
  build (want 1)". Release notes say "reinstall" and link the restore page. [I] Whether that
  error reaches the console's release view as a visible fault must be confirmed by a test (D4).
- **Edge — a hole the decisions do not mention.** [F] Edge detection does not read the
  manifest; it reads the `org.quasar.schema.version` label off the branch-tagged CP image
  (`edge.go:23-28`) and skips an image without a readable label (`edge.go:68-71`,
  `detect.go` edge path). An installed Compose stack on `edge` would therefore be offered the
  first RH-06 develop build and its Go updater would recreate the new CP under Compose.
  **Mitigation [P]:** RH-06-era CP images carry the schema version under a new key
  (`org.quasar.schema-version.v2`) and drop the old one; old control planes skip them as
  "schema unknown", new control planes read the new key. The image contract is changed to
  assert the new key (a like-for-like replacement, not a relaxation).

### 8.3 Publishing order

Images workflow: build control plane and node agent, then the actor with
`io.quasar.release` naming their digests, then the manifest naming all three, then the
optional ADR 0003 signature over the manifest (unchanged mechanism; the actor verifies).

---

## 9. Frozen-contract changes (need Opus review + owner sign-off on the text)

| Document / section | Change |
|---|---|
| `control-api.md` §"The release manifest asset" (`:7200-7245`) | Format 2 as §8.1; format 1 remains readable for display and is never offerable. |
| `control-api.md` §"Platform-release apply (amendment 2)" (`:7247`ff), §"The shape of an apply" (`:7273`) | Components may include `recovery-actor`; the control-plane target is applied by the **recovery actor** beside it; ordering within the CP attempt (actor first, §5.2); restore rules of ADR 0007 (non-migrating CP auto-restore); a failed migrating attempt reads `failed` + "restore command" rather than auto-restore. |
| `control-api.md` `POST /v1/admin/platform/apply` (`:7435`) | Optional `external_backup_confirmed`; 409 `external_backup_unconfirmed`. |
| `control-api.md` `PlatformApplyAttempt` | Optional `backup {taken_at, bytes, schema_version}`. |
| `control-api.md` §Preflight (`:7940`), `PreflightCheckId` | Retire `updater_stack_dir`, `updater_overlays`; rename `updater_socket` → `actor_socket`; add `template_supported`, `backup_space`, `no_foreign_service`. |
| `control-api.md` `EligibilityReason` | Add `below_floor`; `updater_absent` keeps its id, reworded to "no recovery actor". |
| `control-api.md` §Hosts / host body; `GET /v1/admin/platform/identity` | Per-machine service inventory (service, image, digest, compat, state), seed version, database mode. New `POST /v1/admin/hosts/{id}/uninstall` (D12). |
| `control-api.md` §Host enrollment tokens (`:1861`) | Boot-provisioned local token (combined host); static `ENROLLMENT_TOKEN` retired. |
| `agent-api.md` §`register` optional identity fields (`:357-392`) | Add `agent_compat`, `recovery_actor_commit`, `recovery_actor_compat`, `seed_version`; reword `updater_present` ("an actor able to replace this host's containers" — the text at `:386-388` already says this) and the "learned through the docker CLI" sentence. |
| `agent-api.md` §`release_apply` (`:1901`) | `components` may carry `node-agent` and/or `recovery-actor`; optional `settings {revision, values}`; the actor, not "the updater", carries it out. |
| `agent-api.md` §`release_state` (`:1158`) | `recreating` = "the old container is stopped and kept and the new one is being created or started" (no longer a Compose command); reasons add `template_unsupported`, `foreign_service`; `signature_missing`/`signature_invalid` formally added (research defect #9). |
| `agent-api.md` new §`host_uninstall` | Downstream command + ack, relayed to the actor (D12 console "remove host"). |
| `schema.md` §`hosts` (`:937`) | Columns `agent_compat INT`, `recovery_actor_commit TEXT`, `recovery_actor_compat INT`, `seed_version TEXT` (NULL = unknown). |
| `schema.md` §`platform_apply_attempts` (`:2152`) | `backup JSONB NULL`; new reason values; `requested_digests`/`previous_digests` may name `recovery-actor`. |
| `schema.md` new §`platform_service_settings` | `(target_host_id UUID NULL /* NULL = control plane machine */, service TEXT, key TEXT, value TEXT, revision BIGINT, updated_by UUID, updated_at TIMESTAMPTZ, PK(target_host_id, service, key))` + a per-(target, service) revision row, mirroring RH-05's revision/CAS pattern (0087). |
| `schema.md` §"Not frozen: the updater's local socket" (`:2193`) | Becomes "the recovery actor's local sockets": still not frozen, **except** the `recovery-actor` apply verb's shape, which is frozen forever (§4.9). Adds the cross-language fixture rule (§10). |
| ADRs | ADR 0007 (restore rules by migration, §4.6); amend ADR 0003 "verified by the updater" → "by the recovery actor". |

Not frozen and unchanged in shape: the socket bodies, the journal format, the template format
(versioned by `template_format` in the manifest, which *is* frozen through the manifest).

---

## 10. Testing seams and adapters

| Seam | Adapters | What it proves |
|---|---|---|
| `render` (pure) | none needed | Golden `ContainerSpec` per template × role × vendor × DB mode; compose-parity test (§2.3); closed vocabulary refusals. |
| `PlatformEngine` | `FakeEngine` (in-memory: containers, names, labels, restart policies, health scripts, pull failures, `UnknownOutcome`, daemon restart), `DockerEngine` | The Replacement machine against every fault without Docker. |
| `DurableFile` + `CrashAfter` | temp dir | **Crash-injection matrix**: for each attempt kind and each k in 1..N, crash after the k-th fsync, restart (`recover`), drive to terminal, then assert: exactly one outcome; never two running same-role containers; the old container stopped only after `StoppingOld` and never removed before `Discarding`; no attempt restarted on its own; `seed.json` only names verified actors. The RH-05 pattern (`policy.rs:2936` "a crash after either fsync recovers the group exactly once") generalised. |
| Handover | two `Actor` instances on threads sharing a temp dir (real flock) + `FakeEngine` | Every row of §3.3's table. |
| `Gate` + signature | pure; golden vectors in `testdata/release-signature/` shared with the Go release-signing tests | Namespace boundary, digest shape, authority per socket, ADR 0003 bind rule (`signature.go:272-304`). |
| Socket contract (Rust actor ↔ Go CP, Rust actor ↔ Rust agent) | fixtures `crates/quasar-recovery/fixtures/socket/*.json`, decoded by a Go test in `internal/platform/actorapi` and a Rust test | Cross-language and cross-version drift (the one cost Go-to-Go did not have). |
| `ActorAPI` (Go) | existing fakes in `apply_self_test.go` style | Fleet run, Adopt, migrating/external confirmation, `below_floor`. |
| Postgres | `make test-db` for Go (new table, columns, planner reasons); a Rust `#[ignore]` real-engine test (pattern: `runtime/docker/real_tests.rs`) that dumps and restores a `postgres:16-alpine` container through `exec_out`/`exec_in` | Dump/restore bytes path, free-space refusal logic. |
| Agent relay | a unix-socket fake actor in a temp dir, like today's release tests | `release_apply` → socket → `release_state`, adopt-on-connect after restart. |

**Only live hosts can prove:** power loss and host reboot mid-attempt (fsync reaching the
disk, restart policies after boot); a real Docker daemon restart during pull/create; the
Dockge and Arcane "redeploy/update stack" step against the race guard (D11/R1); NVIDIA
device access, driver volume and CUDA runtime on an NVIDIA GPU host and render-node access
on an AMD GPU host through actor-created containers; a real migration failure followed by
the `restore` command on a real-sized database; the restore-into-fresh-install path (D4);
the control-only host's "no agent here" readiness/preflight/release view (D2); session
ride-through of a non-migrating CP Replacement (the 90 s hold, research A §7).

---

## 11. Trade-offs

### 11.1 Rust actor vs keeping update logic in Go — the honest price

| Item | Keep in Go | Move to Rust (this design) |
|---|---|---|
| Engine client | Needs the Moby Go SDK (large dependency tree) or a hand-rolled Engine API client, plus error classification, socket discovery refusals, credentials, pull-by-digest progress, exec streaming, archive upload — **all new**. | RH-01's facade already has all of these, tested (`runtime.rs:136-194`, `runtime/docker/*`, `credentials.rs`). Extraction ≈ mechanical (§1.1). **Largest single saving.** |
| Durable journal, lease, owner labels, self-inspection | New in Go. | Exist (§1.1); one generalisation. |
| Compose executor (`exec.go`, `discover.go`, `env.go`, `result.go`) | Deleted either way. | Deleted either way. |
| Request gates (`plan.go` validation, namespace allowlist, ~150 LOC) | Kept. | Ported; trivial; tests ported. |
| Signature gate (ADR 0003, `signature.go` + `signature_source.go`, ~560 LOC + ~780 LOC tests) | Kept. | Ported with `ring` Ed25519 (already in the lock via rustls) and `ureq` (already a dependency) for the fetch. **Real cost and real risk**; mitigated by shared golden vectors. |
| Socket types shared with the CP | Imported directly (`apply_self.go:18`). | Cross-language; needs fixtures on both sides (§10). **Permanent cost.** |
| Language of "all update logic" | One (Go planner + Go executor). | Split: planner in Go, executor in Rust. Contributors crossing the seam need both. |
| Future RH-07 (Podman/rootless) | Two engine clients to certify. | One (`quasar-runtime`) for agent and actor. |

Net: the Go option keeps ~700 LOC of gates for free but must build the whole engine layer that
Rust already has; the Rust option pays a port plus a permanent cross-language seam. Given the
executor is new code either way, Rust is cheaper overall and gives one engine vocabulary.

### 11.2 Where leverage is high

- **Template-in-image + generic `render`.** Small interface (one function, one label), large
  hidden behaviour (all of compose + overlays), and it makes skew a single integer. Most CP
  config changes need no actor release.
- **Unchanged Go call path.** The fleet run, `Adopt`, eligibility and preflight keep their
  structure; the `ActorAPI` seam swaps adapters.
- **One durable-file primitive** used by agent (applications, policy) and actor (attempts,
  handover, seed.json).
- **Seed as a mode of the actor binary**: no second engine client, no second image.

### 11.3 Where it is thin (shallow modules to watch)

- `socket.rs` is mostly pass-through; acceptable because it is also the authority boundary.
- The `restore`/`uninstall` CLI is a thin Engine-API client that delegates to the actor via
  `exec`; its value is operator ergonomics, not logic.

### 11.4 Risks

1. **Template vocabulary creep.** Every "just one more mount/device" is a format bump and an
   actor-first release. Mitigation: vocabulary reviewed like a contract; the compose-parity
   test catches missing words early.
2. **Handover is the most intricate protocol in the design** (two processes, one lease, a
   watchdog). Mitigation: it is the most heavily crash-tested path, and the `recovery-actor`
   verb is frozen so a bad actor can always be moved forward (or restored from `.kept`).
3. **Cross-language, cross-version socket drift** (Go CP N ↔ Rust actor N±1). Mitigation:
   fixtures per released version kept in the repo; the actor serves `socket_versions` down to
   its floor.
4. **Workspace split churn in `node-agent`** (imports, `pub(crate)` → `pub`, test moves)
   while RH-03/RH-04 touch the same files. Mitigation: slice 1 is a pure refactor landed
   first and fast.
5. **The edge-channel hole (§8.2)** if the label change is forgotten.
6. **Actor may lead the CP on its machine (§5.2)** — a deviation from D9's literal text that
   needs owner confirmation.

---

## 12. Slice order (vertical tracer bullets)

0. **Contract + ADR ticket** (Tier 3, blocks all implementation, like RH-05's #334): §9 text,
   ADR 0007, CONTEXT terms (Service template; Kept container; Handover). **Design ticket**
   (D16): mockups for service inventory, Add-host tabs, "must update before it can be
   managed", restore/uninstall prompts, developer apply.
1. **Extract `quasar-runtime`** (pure refactor; agent behaviour identical; `make test-rust`
   green). Add `DurableFile`, `StateLease`, `CrashAfter`, `Owner` value.
2. **Tracer A — GPU host install.** `quasar-recovery` with `seed` + `actor` install path,
   `Dockerfile.recovery`, image-contract role `recovery`; node-agent image gains
   `io.quasar.service-template`; enrollment via file; agent `hello`. Live on an AMD GPU host,
   then an NVIDIA GPU host. No updates yet.
3. **Tracer B — agent Replacement via relay.** `Replacement` + journal + crash matrix + ADR 0004
   restore; agent relay switched to the actor socket; `release_apply`/`release_state`
   unchanged on the CP. Live: apply, forced unhealthy, kill actor mid-pull/mid-verify.
4. **Tracer C — combined host install.** Built-in Postgres, secrets injection, CP `*_FILE`,
   boot-provisioned local token, control socket, `/v1/self` → CP identity/install mode.
5. **Tracer D — CP Replacement, non-migrating** through the fleet run; `Adopt`; ADR 0007
   restore; session ride-through evidence.
6. **Tracer E — migrating CP.** Space check, `pg_dump`, `restore --attempt`, external-DB
   confirmation (control-api field + console checkbox); control-only host shape.
7. **Tracer F — actor handover + release format 2.** Handover protocol and its crash table,
   `seed.json`, manifest format 2, compat/floor planning (`below_floor`), release workflow
   ordering, edge label mitigation.
8. **Retire and replace.** Race guard + preflight vocabulary; delete the Go updater, its image,
   compose service and preflights; `enroll-host.sh` rewrite; site installer seed snippet;
   `uninstall` CLI and console "remove host" (`host_uninstall`).
9. **Evidence.** Dockge and Arcane fresh installs with a redeploy step; restore into a fresh
   install (D4); docs (`docs/upgrading.md`, operator restore page; Compose path marked
   contributor-only, D15).
