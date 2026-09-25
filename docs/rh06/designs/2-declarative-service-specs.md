# RH-06 design 2: declarative desired service state

Date: 2026-09-25. Baseline: `develop` at `32aee45`. Binding inputs: `docs/rh06/2026-09-24-decisions.md`
(D1–D16 as amended by R1; D17 is cut, D6 ended as (a), Postgres is created but never updated,
Portainer is dropped, and an external database is a first-class option). This is one of three
independent architectures. Its premise is **maximise flexibility through declarative desired
service state**. It records the places where that premise costs more than YAGNI allows, and does
not build past those points.

Conventions. **[F]** is a fact about today's code, with `path:line`. **[P]** is a proposal in this
design. Architecture words follow the codebase-design vocabulary: module, interface,
implementation, depth, seam, adapter, leverage, locality. Domain words follow `CONTEXT.md`.
Hosts are named by role only.

---

## 0. The design in one page

1. **[P] Every platform service a machine runs is described by a service specification.** A
   service specification is a fully rendered, secret-free JSON document: image digest, environment,
   secret references, mounts, devices, ports, network, restart behaviour, health and verification
   rules. Postgres stores it per (machine, service) as a numbered **spec revision**, and each
   (machine, service) pair carries `desired_revision/desired_digest` and
   `applied_revision/applied_digest`. This copies RH-05's `host_setting_groups`
   (`control-plane/migrations/0087_host_policy.up.sql:34-48`).
2. **[P] A release, a developer apply, an agent revert or an edit to an install-time setting all
   do the same thing: they produce a new desired revision.** Replacement is the one executor, and
   an Attempt records each one.
3. **[P] Each image says how it must be run.** A **service template** is baked into every
   Quasar-owned image as an OCI label and is covered by the digest the release manifest pins. A
   renderer shared by the control plane and the recovery actor combines the template with machine
   facts, operator inputs and secret names to produce the specification. This is the answer to
   version skew (§2): an old control plane renders a new image correctly because the new image
   carries the new instructions.
4. **[P] The recovery actor is a convergence engine that acts only when instructed.** Given a
   desired revision, it re-renders that revision to check it, caches it in the machine-state
   volume, and converges the machine through a journalled Replacement. It then reports the applied
   revision. It keeps working while the control plane is down. It never reconciles on its own
   initiative (D8: "never retries an attempt on its own").
5. **[P] The seed is frozen around one file.** It reads a format-1 **seed pointer**: an Engine API
   container-create body that the actor writes verbatim. It recreates the actor from that pointer
   only when no actor container of this installation exists. Nothing else.
6. **[P] Instructions reach the actor under D6(a).** On the control plane's own machine they
   travel over a local socket. On a GPU host they travel through the agent WebSocket, and the
   agent relays the specification as **opaque JSON**, so it never needs to understand a spec
   format.

---

## 1. Module map

### 1.1 New modules

| Module | Language / path | Interface (what callers must know) | Depth |
|---|---|---|---|
| **servicespec** | Go, `control-plane/internal/servicespec/` | `Render(t Template, in Inputs) (Spec, error)`; `Digest(Spec) string`; `ParseTemplate([]byte) (Template, error)`; `Classify(old, new Spec) Disruption`; `const MaxTemplateFormat, MaxSpecFormat`. Invariants: pure; deterministic; **a given format's output is frozen forever** (golden files); a Spec never contains a secret value. Error modes: `ErrTemplateFormat` (too new), `ErrInputMissing{name}`, `ErrInputInvalid{name}`. | Deep: 5 functions over the input vocabulary, vendor branches (NVIDIA device requests, DRI nodes), same-path bind rules, secret-to-file wiring and canonical JSON. |
| **converge** | Go, `control-plane/internal/converge/` | `Decide(j Journal, obs Observation) Step`, the pure Replacement state machine (§4); `RestoreDecision(svc, migrating, startedEver, passedHealth, dbOwned) Restore`. Invariants: journal-then-act; every `Step` is idempotent when re-observed. | Deep: two functions carry all of D7, D8 and ADR 0004. |
| **engine** | Go, `control-plane/internal/engine/` | the `Engine` interface (below) with two adapters, `engine/docker` (a hand-rolled Engine API v1.40 client over a unix socket; there is no SDK dependency, and `control-plane/go.mod` has none today) and `engine/fake` (in memory, scriptable health and crash points). A later `engine/podman` is the third adapter (RH-07). | Real seam: two adapters from day one. |
| **recovery** | Go, `control-plane/internal/recovery/` + `cmd/quasar-recovery` | Process-level: two local sockets (§1.3), a journal directory, a spec cache, bootstrap, handover, `restore`, `repair`, `uninstall`, the race-guard scan and the inventory. Consumes `servicespec`, `converge`, `engine` and `trust`. | Moderate. Its complexity is orchestration and belongs in one place (locality). |
| **trust** | Go, `control-plane/internal/recovery/trust/` | Moved from `internal/updater`: namespace allowlist and digest rules (`updater/plan.go:158-256`), the ed25519 manifest signature gate (`updater/signature.go`, `signature_source.go`). `Admit(spec Spec, sig SignatureEvidence) error`. | Kept whole. It passes the deletion test (see 1.4). |
| **seed** | Go, stdlib only, `seed/` (its own Go module) + image `quasar-seed` | Frozen contract (ADR 0007, §3): its inputs, the seed pointer v1, the labels `io.quasar.install` and `io.quasar.service`, the tombstone, and the verbs `run` (default), `restore`, `uninstall`, `repair` and `status`, which delegate to the actor image. | Deliberately shallow and small, so that it can stay frozen. |
| **platform/servicestate** | Go, `control-plane/internal/platform/` new files `specs.go`, `spec_store.go`, `actor_client.go`, `machines.go` | `PlanServiceSpecs(in SpecPlanInputs) ([]DesiredRevision, []Refusal)`, pure, and a sibling of `PlanRelease`. `SpecStore` (Postgres). `ActorClient` interface (below) with adapters `localSocket`, `agentRelay` and `fake`. | `PlanServiceSpecs` is deep. `ActorClient` is a real seam with two production adapters. |
| **platform_relay** | Rust, `node-agent/src/platform_relay.rs` (replaces `release/mod.rs`) | Forwards `platform_converge` / `platform_remove` bodies **verbatim** to the agent-scoped actor socket, relays actor state as `platform_converge_state`, and replays non-terminal or recent attempts on connect. Invariants: never parses `spec`; refuses a service outside {`node-agent`, `recovery-actor`} (the confused-deputy guard, today at `release/mod.rs:43-46`). | Shallow on purpose. Its value is that it does not change when the spec format evolves. |

**`Engine` interface** [P]. It is everything the converge implementation needs. Every call is
idempotent under re-observation.

```go
type Engine interface {
    Inspect(ctx context.Context, nameOrID string) (Container, error)      // ErrNotFound
    List(ctx context.Context, f LabelFilter) ([]Container, error)          // all states
    ImagePresent(ctx context.Context, ref Digested) (ImageInfo, bool, error) // Labels from image config
    Pull(ctx context.Context, ref Digested, deadline time.Time) error        // digest-only
    Create(ctx context.Context, name string, body CreateBody) (string, error) // ErrNameInUse
    Start(ctx context.Context, id string) error
    Stop(ctx context.Context, id string, grace time.Duration) error
    Rename(ctx context.Context, id, newName string) error
    SetRestart(ctx context.Context, id string, p RestartPolicy) error        // POST /containers/{id}/update
    Remove(ctx context.Context, id string, removeVolumes bool) error         // never true in RH-06
    Exec(ctx context.Context, id string, argv []string, stdin io.Reader, stdout io.Writer) (int, error)
    EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
    EnsureVolume(ctx context.Context, name string, labels map[string]string) error
    Info(ctx context.Context) (EngineInfo, error)                            // runtimes (nvidia), API version
}
```

`CreateBody` is the Engine API `ContainerCreate` body. `servicespec.Spec.CreateBody(names)` builds
it. This matches the seed pointer format (§3): one engine vocabulary serves both.

**`ActorClient` interface** [P] (control-plane side). It replaces `UpdaterAPI`
(`control-plane/internal/platform/apply_self.go:49-61`), which has one socket adapter today.

```go
type ActorClient interface {
    Self(ctx context.Context) (ActorSelf, error)            // version, api level, formats, facts, faults, seed
    Services(ctx context.Context) ([]ServiceStatus, error)   // desired/applied per service, open attempt
    Converge(ctx context.Context, req ConvergeRequest) (Accepted, error) // 202; 409 busy; 422 reason
    Attempt(ctx context.Context, requestID string) (AttemptState, error) // ErrUnknownRequest => never received
    Remove(ctx context.Context, req RemoveRequest) (Accepted, error)     // GPU host "remove host" only
}
```

The adapters are `localSocket` (the control plane's own machine, over the control-scoped socket)
and `agentRelay`. `agentRelay` sends `platform_converge` over the registry
(`cmd/quasar-control/app.go:1006-1025` shows today's `SendReleaseApply`) and turns
`platform_converge_state` messages into `AttemptState`. `fake` covers tests. The fleet and apply
machines see one interface whatever the host shape. That is where the leverage is.

### 1.2 Modules changed

| Module | Change |
|---|---|
| `platform/plan.go` | `PlanRelease` gains the components `recovery-actor` and D9 floors. `updater_absent` becomes `actor_absent`. Adds `below_floor` ("must update before it can be managed": only an update is offered), `spec_format_unsupported`, `foreign_container` and `backup_unconfirmed` (external database, migrating release). ADR 0002 ordering is unchanged (`plan.go:171-264`). |
| `platform/apply_self.go` | Becomes the CP-target driver over `ActorClient`. It keeps the rule "persist request id before the call" (`apply_self.go:382-495`) and `Adopt` (success is this binary serving the release commit, `:532-576`). It adds a post-boot import of the actor's applied revision. |
| `platform/apply_runner.go`, `apply_fleet.go` | A host step becomes up to two Attempts, **recovery actor first, then node agent** (§6). Drain, owned admission restrictions (`app.go:993-1005`, `:1061-1077`), stop-at-first-failure and `succeeded_partial` are unchanged. |
| `platform/apply_revert.go` | Revert means "desire the prior agent *image*, re-rendered with current inputs" (§6.3). |
| `platform/preflight.go` | Retires `updater_stack_dir` and `updater_overlays` (`preflight.go:145-212`). Renames `updater_socket` to `actor_socket`. Adds `dump_space`, `foreign_container` and `seed_present` (warn only). |
| `platform/manifest.go`, `github.go` | Parse format 2 from a new asset name (§8). |
| `hostenroll` | Adds a token-scoped bootstrap render endpoint (§7.3). The combined-host bootstrap token is inserted from a mounted file. The static `ENROLLMENT_TOKEN` is retired (D10). |
| `config` | `*_FILE` inputs: `QUASAR_DATABASE_PASSWORD_FILE`, `QUASAR_SECRET_KEY_FILE`, `QUASAR_SECRET_KEY_PREVIOUS_FILE`, `QUASAR_BOOTSTRAP_ENROLLMENT_FILE` (D5). |
| `node-agent/src/buildinfo.rs` | `install_mode` becomes `owned` when the agent's own container carries `io.quasar.install`. Compose-label discovery (`buildinfo.rs:101-106`, `:221`) is removed. `updater_present` is no longer sent. |
| `node-agent/src/config.rs`, `enrollment.rs` | `QUASAR_ENROLLMENT_FILE` (the enrollment string is a secret file, D5). |
| `deploy/build-images.sh`, `image-contract.json` | New label `org.quasar.service.template` (JSON), required for roles `control`, `runtime` and `recovery`. New role `seed`. Validated by `quasar-recovery template-check`. |
| `deploy/enroll-host.sh` | Rewritten small (D13): host checks, then `docker run` of the seed, then wait for enrollment. |

### 1.3 Seams, stated once

| Seam | Adapters | Real? |
|---|---|---|
| `engine.Engine` | docker, fake (later podman) | Yes |
| `platform.ActorClient` | localSocket, agentRelay, fake | Yes |
| `images.ImageInspector` (reused, `control-plane/internal/images/config.go:46-47`) | registry resolver, test registry | Yes (exists) |
| Actor sockets: `control.sock`, `agent.sock` | one server, two **authority scopes** | Not a code seam. It is a trust boundary: the socket a caller reaches decides which services it may name. |
| `servicespec.Render` | none | No seam: a pure function. Tests use it directly. |
| Journal directory | concrete type over `os`; tests use `t.TempDir()`, and a `writeFile` hook injects fsync failure | No interface (one adapter would make the seam hypothetical). |

The two sockets [P] live in two volumes, `quasar-recovery-control` (mounted into the control-plane
container only) and `quasar-recovery-agent` (mounted into the node-agent container only). This is
what R1 means by "mounts its socket only into the containers it creates". It also replaces today's
0666 unauthenticated socket shared by three containers (`internal/updater/server.go:293-315`,
`deploy/docker-compose.yml:259`, `:625`, `:694`).

### 1.4 Deleted or retired (deletion test)

| Retired | Does the complexity reappear? | Verdict |
|---|---|---|
| `internal/updater/discover.go`, `env.go`, `exec.go`, compose parts of `plan.go`, `server.go`; `cmd/quasar-updater`; `deploy/Dockerfile.updater`; compose service `quasar-updater`; volume `quasar-updater-run` | No. Label discovery, `.env` rewriting and the compose CLI have no counterpart: the spec *is* the definition. | Delete (D4, D11). |
| `updater/signature*.go`, namespace/digest rules in `updater/plan.go` | Yes: the actor needs every one of these gates. | Keep, moved to `recovery/trust`. |
| Preflight `updater_stack_dir`, `updater_overlays` | No. Compose drift cannot exist. Its general form, "observed container's `io.quasar.spec.digest` ≠ applied digest", is a one-line inventory fault. | Delete. |
| `node-agent/src/release/mod.rs` poller and replay | Yes, the replay-on-connect is still needed (`release/mod.rs:142-203`). | Rewritten as `platform_relay.rs`. Replay logic kept. |
| `node-agent/src/readiness/platform_update.rs` compose checks | No. | Delete. `actor_socket` is kept. |
| `enroll_host_compose_test.go` (`TestEnrollHostComposeMatchesBase`, `:107`) | Yes. Parity between the contributor-lane compose file and the product-lane template is still needed. | Replaced by a template-to-compose parity golden test (§10). |
| `web/src/lib/platform/manualUpdate.ts` compose recipes | Partly. Owned installs need only `restore`/`repair`/`uninstall` commands. | Replaced. |
| Static `ENROLLMENT_TOKEN` | No. | Retired (D10). |

---

## 2. Where "how to run service X at version V" lives, and version skew

### 2.1 Facts behind the choice

- [F] Every published platform image already carries provenance labels enforced by the image
  contract (`deploy/Dockerfile.control.prod:91-95`; `deploy/Dockerfile.vulkan:1122-1129`;
  `deploy/image-contract.json:157-167`, `:255-263`).
- [F] The control plane already reads image-config labels **straight from the registry, without a
  pull** (`images.ImageInspector.InspectConfig`, `control-plane/internal/images/config.go:46-47`,
  `:76`). It uses this for edge schema ordering (`platform/edge.go:130`) and for edge apply commit
  verification (`platform/apply_edge.go:80-86`).
- [F] ADR 0001 pins every component by digest. The digest covers the image config, labels
  included, so anything in a label is as trusted as the image (and as the manifest signature, ADR
  0003).
- [F] Today the knowledge sits in `deploy/docker-compose.yml` (about 100 env keys on the agent,
  `:325-548`, plus mounts `:564-625`, devices `:631-654` and the NVIDIA overlay). It is versioned
  with the repository, not with the image, and the updater applies whatever the compose file on
  disk says (research A §2 inferences: "`up` re-reads the current compose files").

### 2.2 Options considered

| Option | CP self-replacement skew | Edge / developer apply | Verdict |
|---|---|---|---|
| (a) Rendering code compiled into the CP | **Broken**: the old CP renders the new CP from old knowledge, so a new env var or mount required by the new binary is missing. | Works | Rejected |
| (b) Templates carried in the release manifest | Works for stable | **Broken**: edge rows have no manifest (`detect.go:183-192`); developer apply has none. | Rejected |
| (c) **Templates as image labels** | Works: the new image states its own needs | Works: every image has labels | **Chosen** |
| (d) A file inside the image (`/usr/share/quasar/service.json`) | Works | Works | Rejected: it needs a pull and a create to read, where a label is one registry GET. |

### 2.3 The service template [P]

Source files live under `deploy/services/<service>.json`, reviewed with the code that consumes the
env and mounts. `build-images.sh` passes them as `--build-arg SERVICE_TEMPLATE="$(jq -c . …)"` into
a declared `ARG`, then `LABEL org.quasar.service.template="${SERVICE_TEMPLATE}"`. The image
contract requires the label, and `validate-image.sh` runs `quasar-recovery template-check`, which
parses the template and renders it against a fixture input set.

```jsonc
// deploy/services/node-agent.json (abridged)
{
  "template_format": 1,
  "service": "node-agent",
  "disruption": "ends_sessions",
  "network": { "mode": "host" },
  "init": true,
  "cap_add": ["NET_ADMIN", "SYSLOG"],
  "env": {
    "NODE_SECRET_PATH": "/var/lib/quasar-agent/node-secret",
    "XDG_RUNTIME_DIR": "/run/quasar-agent",
    "CONTROL_PLANE_URL": { "input": "control_plane.agent_url" },
    "NODE_NAME": { "input": "machine.node_name" },
    "QUASAR_HOME_ROOT": { "input": "machine.home_root" },
    "QUASAR_HEALTH_ADDR": "127.0.0.1:9091"
  },
  "secrets": [ { "name": "enrollment", "env_file": "QUASAR_ENROLLMENT_FILE", "optional": true } ],
  "mounts": [
    { "kind": "volume", "role": "agent-state", "target": "/var/lib/quasar-agent" },
    { "kind": "bind", "source": "/run/quasar-agent", "same_path": true },
    { "kind": "bind", "source": { "input": "machine.home_root" }, "same_path": true },
    { "kind": "bind", "source": { "input": "machine.template_root" }, "same_path": true },
    { "kind": "docker_socket", "target": "/var/run/docker.sock" },
    { "kind": "actor_socket", "scope": "agent" },
    { "kind": "bind", "source": "/dev", "target": "/host/dev", "read_only": true }
  ],
  "devices": [ { "input": "machine.gpu.dri_nodes" }, { "path": "/dev/uinput" }, { "path": "/dev/kmsg", "perms": "r" } ],
  "device_cgroup_rules": ["c 13:* rmw"],
  "vendor": {
    "nvidia": {
      "gpus": "all",
      "env": { "NVIDIA_DRIVER_CAPABILITIES": "all", "QUASAR_NVIDIA_DRIVER_VOLUME": "1",
               "LD_LIBRARY_PATH": "/opt/quasar/nvidia-driver/lib:/opt/quasar/nvidia-driver/cuda/lib64" },
      "mounts": [ { "kind": "volume", "role": "nvidia-driver", "target": "/opt/quasar/nvidia-driver" } ]
    }
  },
  "settable_env": ["RUST_LOG", "QUASAR_ENCODER", "QUASAR_FEC_PERCENTAGE", "…"],
  "health": { "kind": "image" },
  "verify": { "pass_window_s": 20, "deadline_s": 300 }
}
```

The **input vocabulary** is closed per `template_format`: `machine.*` (role, node name, home root,
template root, GPU vendor and nodes, public host), `control_plane.*` (agent URL, TLS pin, ports),
`install.*` (install id, machine id) and `operator.env` (values for `settable_env` only). Adding an
input kind is a format bump.

### 2.4 Who renders what, and the skew rules [P]

**Rule S1 (per-format frozen semantics).** For a given `template_format` and a given input set,
`Render` output is byte-identical in every build that supports that format. Golden files in
`servicespec/testdata/format<N>/` are never edited, only added to.

**Rule S2 (expand, then contract).** Release N may add renderer support for format k+1. An image
may *use* format k+1 only in a release whose D9 floor already guarantees a renderer ≥ N at every
point where that image will be rendered. The table shows why that point differs by service:

| Rendered spec | Rendered by | Converged by | Constraint |
|---|---|---|---|
| New CP (release R) | the **running, old CP** | the old actor on the CP's machine | R's CP template format ≤ the floor CP's renderer; R's CP spec format ≤ the floor actor's max. **This is the only hard skew case.** |
| New actor (R) | the **new CP** (after the CP step, ADR 0002) | the old actor (handover) | Spec format ≤ the old actor's max |
| New agent (R) | the new CP | the **new** actor (handover runs first, §6) | none beyond the same release |
| Bootstrap Postgres + CP | the actor (shared renderer) | the actor | same release by construction |

**Rule S3 (release-time check).** The manifest generator
(`scripts/release/generate-platform-release-manifest.sh`) reads the three new images' template
formats. It refuses a release whose control-plane template format exceeds the `MaxTemplateFormat`
of the release named as the floor. It also refuses one whose floor would strand a component: the
new floor must be ≤ the previous release's actor and agent api levels, so every host manageable
before the release can at least be updated after it. This is D9's "release-time check".

**Rule S4 (self-check, detector only).** After a CP boots on a new commit, it re-renders its own
spec with its own renderer. A digest different from the applied digest is logged as a
`render_skew` fault. It is never acted on: acting would be an automatic retry and would risk a
restart loop.

The result: adding an environment variable, mount or device to the agent is one PR that edits
`deploy/services/node-agent.json` beside the Rust that reads it, with **no control-plane change and
no release coupling**. That is the main leverage of this design.

### 2.5 Verification by re-render (the authority bound) [P]

A fully rendered spec gives the control plane a wider authority than today's updater, which could
only swap an image inside a fixed compose definition. A spec can name any mount, device or
privilege. The actor therefore **refuses any spec that is not exactly the render of its own image's
template**:

```
admit(spec, inputs):
  trust.Admit(spec.image, spec.digest, signature)   // allowlist + digest + optional signature (moved gates)
  tpl := template label of the image (Engine image inspect after pull, or registry config before)
  local := machine facts from machine.json               // CP cannot override these
  expect := servicespec.Render(tpl, inputs ⊕ local)
  require Digest(expect) == spec.digest_of_body
```

A compromised control plane can then only move within what an allowlisted, digest-pinned image's
own template declares (for example `settable_env`). The cost is a second render. The renderer is a
pure function, so that is cheap, but it makes the actor's renderer version part of the skew rules
above.

---

## 3. The seed

### 3.1 What it does (ADR 0007, "the seed contract") [P]

```
loop every 30 s (and once at start):
  if /state/seed/uninstalled exists: sleep; continue            # D12 tombstone
  p := read /state/seed/actor.json                              # pointer v1, may not exist
  if p == nil:                                                  # first boot
     create actor from built-in body + seed inputs (below); continue
  if no container with labels io.quasar.install=p.install_id AND io.quasar.service=recovery-actor
     exists in ANY state:                                       # a kept --prev counts
     pull p.image@p.digest; create p.name from p.create_body (verbatim); start
```

- **Inputs** (the manager snippet or the `docker run` line). `QUASAR_ROLE`
  (`combined|control-only|gpu`), `QUASAR_HOME_ROOT`, `QUASAR_PUBLIC_HOST` (optional),
  `QUASAR_ENROLLMENT` (GPU hosts, single use), `QUASAR_ACTOR_IMAGE` (optional; the console pins
  the CP's release actor by digest; the default is the channel tag `…/quasar-recovery:stable`,
  resolved once), `QUASAR_RELEASE_MANIFEST` (optional file path, for the product lane and
  air-gapped installs), and for R1-Q1 `QUASAR_DATABASE_*` (external database). The seed forwards
  these inputs to the first actor **once**. The actor copies them into machine state (secrets as
  0600 files) and never reads them again (D10: "an identity already in the machine-state volume
  wins").
- **Mounts.** The docker socket, plus the machine-state volume `quasar-machine-state` **read-only**
  (the seed never writes state).
- **Seed pointer v1** (`/state/seed/actor.json`, written only by a verified actor):

```json
{ "format": 1, "install_id": "<64 hex>", "name": "quasar-recovery",
  "image": "ghcr.io/accreleus/quasar/quasar-recovery", "digest": "sha256:…",
  "api_version": "1.40", "create_body": { "Image": "…@sha256:…", "Env": [], "Labels": {}, "HostConfig": {} } }
```

The seed does not interpret `create_body`. It POSTs it to `/v1.40/containers/create`. What is frozen
is the Engine API's own versioned request body plus five field names, which is why the seed can
stay unchanged for years. The body carries no secrets (§5.1).

- **Verbs.** `restore`, `uninstall`, `repair` and `status` are thin. They run the *actor image
  named in the pointer* as a one-shot container with the same mounts and forward the verb. If the
  live actor holds the machine lock, the one-shot forwards the command over the admin socket
  `/state/run/admin.sock` and streams the output. All logic lives in the actor, which is updatable.
  The seed only knows how to launch it.
- **Reported version.** The actor finds the seed by the label `io.quasar.seed=1` (the manager
  snippet sets it; the one-liner sets it) and reports the seed's image digest and version label. A
  missing seed gives the D12 warning.

### 3.2 Handover to a successor actor (crash-safe) [P]

Authority belongs to whichever container the journal names, not to whichever process holds a lock.
The lock `/state/actor.lock` (an `flock`, the same pattern as RH-01's owner lease,
`node-agent/src/container_ownership.rs:85-116`) only serialises journal writes.

| Phase (fsync'd before acting) | Actor doing it | Action |
|---|---|---|
| `accepted` | A (old) | journal intent (to revision R, spec digest) |
| `pulled` | A | pull the successor digest |
| `created` | A | create `quasar-recovery--next` with `RestartPolicy=no` and label `io.quasar.attempt` |
| `successor_started` | A | start B; A keeps serving read-only and **watches** |
| `successor_locked` | B | B takes the lock (A released it after `successor_started`); B is now the authority |
| `old_stopped` | B | set A's restart policy to `no`, stop A, rename A to `quasar-recovery--prev` (**point of no return**, D8) |
| `started` | B | rename B to `quasar-recovery`, set B's restart policy to `unless-stopped` |
| `verified` | B | self-check: both sockets answer, engine list works, journal round-trips; then write the **seed pointer** (tmp+fsync+rename) |
| `succeeded` | B | remove `--prev` (the image stays for GC) |

Recovery rules, all read from the journal plus observation:

- A restarts with phase < `successor_locked`: *interrupted, nothing changed*. Remove `--next`.
- Phase ≥ `successor_locked` and a process whose container id ≠ `to.container_id` starts: it exits
  quietly with its restart policy set to `no`. It never acts.
- B does not reach `verified` before `verify.deadline`: A (still running as watchdog before
  `old_stopped`) kills B, removes `--next`, retakes the lock, and records `failed` with `restored`.
  After `old_stopped`, if B crashes, the seed sees `--prev` (a container of this installation
  exists), so it does nothing. That is the gap, so the rule is: **`--prev` is restarted by `repair`**,
  and the seed pointer still names A until B writes `verified`. A seed-recreated actor therefore
  comes from the last verified body. [P] If both are gone, the seed recreates from the pointer,
  which is still A's body. That is safe by construction.
- The seed never races the rename window, because it keys on labels, not on the name
  `quasar-recovery`.

---

## 4. One Replacement: the crash-safe state machine

### 4.1 Machine-state volume layout [P]

```
/state (named volume quasar-machine-state; the one volume never to delete, D5)
  machine.json                 {format, machine_id, install_id, role, node_name?, facts, created_at}
  actor.lock
  secrets/        0700 root    database-password, secret-key, secret-key-previous?, local-enrollment?,
                               external-database/{host,port,user,password,name,sslmode}   (R1-Q1)
  specs/<service>/revisions/<n>.json   full spec + inputs (keep the last 5)
  specs/<service>/desired              {"revision": n, "digest": "…"}   (atomic pointer)
  specs/<service>/applied              {"revision": n, "digest": "…", "container_id": "…"}
  journal/<attempt_id>.json    one file per Replacement (below)
  dumps/<utc>-<attempt_id>.pgdump      pre-migration pg_dump (keep the last 3, R1)
  seed/actor.json              seed pointer v1
  seed/uninstalled             tombstone (D12)
  run/admin.sock               local admin socket for seed verbs
```

Consumer secret volumes [P]. Engine API 1.40 cannot bind-mount a sub-path of a named volume
(`VolumeOptions.Subpath` is API 1.45+). The actor therefore keeps **per-consumer secret volumes**:
`quasar-secrets-control` (files owned by the CP's uid, 0400) and `quasar-secrets-postgres`. Each is
written from the canonical copies in `/state/secrets`. A container mounts only its own, read-only,
at `/run/secrets`. When the engine floor reaches 1.45 this becomes a sub-path mount and the extra
volumes go away.

### 4.2 Journal record [P]

```json
{ "format": 1, "attempt_id": "uuid", "request_id": "uuid", "machine_id": "uuid",
  "service": "node-agent", "cause": "release|developer|revert|setting|bootstrap|repair",
  "migrating": false, "db_owned": true,
  "from": { "revision": 3, "spec_digest": "sha256:…", "container_id": "…", "image_digest": "sha256:…" },
  "to":   { "revision": 4, "spec_digest": "sha256:…", "container_id": null },
  "phase": "old_stopped", "sequence": 7, "deadline": "2026-…Z",
  "started_ever": false, "passed_health_at": null, "verified_at": null,
  "dump": null, "outcome": null, "reason": null, "restored": false,
  "history": [ { "phase": "accepted", "at": "…" } ], "output_tail": "…(≤ 8192)" }
```

Each write is tmp, `write`, `fsync`, `rename`, then `fsync(dir)`: exactly
`node-agent/src/policy.rs:1312-1327`, ported to Go. `sequence` increments on every write, as
`record_restart_phase` does (`policy.rs:1329-1342`).

### 4.3 Phases [P]

Every "doing" phase is written **before** the Engine call it names. On restart it is resolved by
observing the engine by deterministic name and labels, never by assuming the call happened.

| # | Phase | Engine action | Resolution after a crash |
|---|---|---|---|
| 1 | `accepted` | none. The 202 goes back only after this fsync. | continue |
| 2 | `pulling` → `pulled` | `Pull(digest)` | `ImagePresent`, then continue or re-pull |
| 3 | `dumping` → `dumped` | migrating CP with an owned DB: `Exec(pg_dump -Fc)` streamed to `dumps/*.tmp`; fsync; rename | a missing final file: re-dump; a `.tmp`: delete and re-dump |
| 4 | `creating` → `created` | `Create("quasar-<svc>--next", body)` with the attempt label | inspect `--next`: label = this attempt means done; another attempt's label means `foreign_container` |
| 5 | `stopping_old` → `old_stopped` | `SetRestart(old,no)`, `Stop(old)`, `Rename(old,"quasar-<svc>--prev")` | **point of no return**. Observe each of the three and finish the missing ones. |
| 6 | `starting_new` → `started` | `Rename(next,"quasar-<svc>")`, `Start`, `SetRestart(unless-stopped)` | observe name and state |
| 7 | `verifying` → `verified` \| `verify_failed` | poll health per `spec.health`; record `started_ever` and `passed_health_at` durably the first time each becomes true | re-poll until the deadline |
| 8a | `discarding_old` → `succeeded` | `Remove(--prev)`; write `applied` | idempotent |
| 8b | `restoring` → `restored` \| `restore_failed` \| `held_for_operator` | per `RestoreDecision` | idempotent |
| — | `interrupted` (terminal) | a crash anywhere before phase 5: remove `--next`, keep old untouched | D8: "interrupted, nothing changed" |

**`RestoreDecision`** [P] (pure; extends `updater/exec.go:158-170`):

| Service | Migrating | Outcome on `verify_failed` |
|---|---|---|
| node-agent, recovery-actor | n/a | restore the kept `--prev` (ADR 0004; no pull, because the image is kept, which fixes research inference #3) |
| control-plane | no | restore `--prev`. **Extension**: today only never-started is restored. A non-migrating release has `schema_version` ≤ the running one (`plan.go:155-169`), so the old binary is valid against the DB. |
| control-plane | yes, never started | restore `--prev` (no migration can have run) |
| control-plane | yes, started | `held_for_operator`: `--prev` and the dump are kept, the new container is stopped with restart `no` (no crash loop), and the printed message names `quasar-seed restore`. R1: no automatic DB restore. |
| postgres | n/a | not convergeable in RH-06: `not_updatable` is refused at admit |

**"Passed a health check"** (D7's precise definition) [P]. CP: the first HTTP 200 from `/health`
on the candidate. Boot runs migrations before any listener starts
(`control-plane/cmd/quasar-control/main.go:60-86`), so a 200 proves the migrations completed.
**Verified**: health keeps passing for `verify.pass_window_s`. Agent: the image `HEALTHCHECK` is
`healthy` for the pass window. Actor: the self-check. Success evidence reported to the CP stays as
today: the agent's `register` with the release commit (`apply_runner.go:656-699`) and the CP's own
commit at boot (`apply_self.go:546-557`). In addition, the actor's `applied` must equal the desired
digest.

### 4.4 Identity, labels, names and the race guard [P]

- Names: `quasar-recovery`, `quasar-postgres`, `quasar-control-plane`, `quasar-node-agent`, and the
  suffixes `--next` and `--prev`. One installation per engine (D2) makes fixed names safe. None of
  them collides with RH-01's owned prefixes `quasar-sess-`, `quasar-pulse-` and `quasar-probe-`
  (`container_ownership.rs:11-16`).
- Labels on every owned container: `io.quasar.install`, `io.quasar.machine`, `io.quasar.service`,
  `io.quasar.spec.revision`, `io.quasar.spec.digest`, `io.quasar.attempt`. Labels are immutable,
  so the role (live, next or prev) is carried by the **name**.
- Owned volumes and networks are named `quasar-<role>` with no project prefix, which removes the
  `<project>_` identity trap (research B §2 inference). The bridge network `quasar` carries the
  alias `quasar-postgres`.
- **Race guard (D3).** At boot, before each `creating`, and every 60 s in inventory: any container
  whose image-config `org.quasar.image.role` is in {control, runtime, updater, recovery}, or whose
  `com.docker.compose.service` is a Quasar service name, **and** whose `io.quasar.install` is not
  ours is foreign. Containers labelled `io.quasar.agent-owner` are excluded, because the Pulse
  sidecar and host probes run the agent image (`node-agent/src/session/audio.rs:91-100`,
  `host_probe/runner.rs:291`). A foreign container gives a `foreign_container` fault that names both
  containers, and any converge of that service is refused. A kept `--prev` carries our install id,
  so it is never foreign (D8).
- **Single flight per machine.** At most one non-terminal journal. A second converge gets `busy`
  (refused, never queued, as today, `updater/plan.go:203-256`).
- **Idempotency keys.** `request_id` (minted by the DB before the send, the existing rule at
  `apply_store.go:272-295`). A re-POST of the same id returns the same acceptance. `attempt_id` is
  the journal file name. Revisions are compare-and-swap: `to.revision` must be greater than the
  cached desired revision, with the exception in §5.4.

### 4.5 Edge-triggered, not level-triggered

The premise suggests a reconciliation loop. **D8 forbids it** ("never retries an attempt on its
own"). The actor converges when instructed. Divergence (desired ≠ applied after a failure, a
deleted container, a spec-digest label mismatch) is *reported* as `failed`, `missing` or `drifted`
in inventory. It is fixed by a new instruction from the console, the unattended schedule, or local
`quasar-seed repair` (the D6(a) accepted gap). The seed's "ensure the actor exists" is the only
self-healing rule, and D3 requires it.

---

## 5. Control-plane replacement on its own machine

### 5.1 Sequence (fleet run CP step) [P]

1. `apply_fleet.go` drives as today: cordon, and drain only if `ReleaseRunsAMigration`
   (`apply_fleet.go:460-561`).
2. `PlanServiceSpecs` fetches R's CP template (`InspectConfig` by digest), renders revision n+1, and
   **in one transaction** inserts the revision, sets `desired_*`, inserts the attempt, and mints
   `request_id`.
3. `localSocket.Converge({request_id, attempt_id, service: control-plane, revision, spec, inputs,
   migrating, db_owned, backup_confirmed, deadline})`. The actor admits (§2.5), journals `accepted`
   and returns 202. The CP polls `Attempt(request_id)` every 2 s, as `apply_self.go` does today.
4. The actor dumps if the release migrates and the DB is Quasar-owned, then creates, **stops the old
   CP (which kills this poller: expected)**, starts and verifies.
5. The new CP boots, and `Adopt` runs:
   (1) commit matches the release: `succeeded`, then import `Services()` into `platform_services.applied_*`;
   (2) otherwise `Attempt(request_id)`: `ErrUnknownRequest` means the PUT never landed, so re-drive
   with the same id (idempotent); a non-terminal state means keep polling; `restored` means record
   failed with `auto_revert` (as `apply_runner.go:588-654`); `held_for_operator` means record failed
   and surface the restore command.
6. **CP killed mid-poll.** Every durable fact precedes every side effect (request id, then PUT, then
   journal `accepted`, then act), so no interleaving loses the attempt.

The CP deadline becomes `journal.deadline + slack`. The actor sets its deadline from the spec's
`verify.deadline_s` plus the measured dump time, so a long migration is not reported as `unhealthy`
early (research A §6: the 300 s `--wait-timeout` trap).

### 5.2 Migrating update with a pre-update dump (D7 as simplified by R1) [P]

- **Quasar-owned DB.** Preflight `dump_space` estimates `pg_database_size` (via `Exec psql` in
  `quasar-postgres`) against free space on `/state` (statfs), and requires twice the size. At
  `dumping`, the actor runs `Exec(quasar-postgres, ["pg_dump","-Fc","-U","quasar","quasar"])` and
  streams stdout to `dumps/…tmp`. It fails closed on a non-zero exit, a missing `PGDMP` magic, or a
  short write. Result: `backup_failed` / `insufficient_space`, **refused before phase 5** (D7). It
  keeps the last three. No Postgres client ships in the actor image, because the dump runs inside
  the Postgres container.
- **External DB (R1-Q1).** No dump. The fleet-run request must carry
  `external_backup_confirmed: true`, otherwise the eligibility reason is `backup_unconfirmed`.
  Unattended runs never migrate, so they never need it (`auto_apply.go:20-27`).

### 5.3 `restore` (D7, D4) [P]

`docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v quasar-machine-state:/state
<seed-image> restore [--dump latest|<file>] [--to-previous]`

1. Take the machine lock (or forward to the live actor over `admin.sock`).
2. Refuse if the DB is external ("restore your own database, then run `repair`").
3. Stop the CP (restart `no`). Postgres stays up. Terminate connections, `DROP DATABASE`,
   `CREATE DATABASE`, then stream the dump into `Exec pg_restore --exit-on-error --no-owner -d
   quasar`. This is the documented stopped-stack restore (`docs/upgrading.md:125-132`); "stopped"
   means no clients.
4. Converge the CP to the **previous applied revision**: start the kept `--prev` if it exists,
   otherwise create from `specs/control-plane/revisions/<prev>.json`. Journal it as its own
   Replacement with `cause: restore`.
5. The CP boots on a restored DB. ADR 0006 expires unstarted approvals. The restored DB still holds
   the attempt row `running` (the dump was taken after the attempt was inserted), so `Adopt` asks
   the actor, which answers `failed/held_for_operator` then `restored_by_operator`, and the attempt
   closes truthfully.

**Fresh install with data (D4).** Run the seed with `-v /path/old.pgdump:/restore/db.pgdump:ro -e
QUASAR_RESTORE_FROM=/restore/db.pgdump` (and optionally a `QUASAR_SECRET_KEY` file). Actor
bootstrap is then: create Postgres, restore, create the CP. The restore runs **before the first
CP boot**, as D4 requires, and uses the same `RestoreDatabase` function as step 3.

### 5.4 Postgres restore versus the actor's cache

A DB restore makes Postgres hold *older* revision numbers than the actor's cache. [P] At CP boot,
revisions in `Services()` that are greater than the DB's are imported verbatim and the counter
advances past them: **machine truth wins for what is applied**. The DB wins for everything else.
This is the one exception to compare-and-swap.

### 5.5 Uninstall (D12, R1-Q2) [P]

- `quasar-seed uninstall`: write the `seed/uninstalled` tombstone **first** (so a running seed cannot
  recreate the actor), then remove the agent, the CP and Postgres, then the actor last, by
  `io.quasar.install`. Volumes and homes are kept.
- `--purge`: a typed confirmation, then one final `pg_dump` if the DB is Quasar-owned, then remove
  owned volumes except the directory holding that final dump (printed). Homes, which are host
  paths, are never touched.
- Console "remove host" (GPU hosts): drain, then `platform_remove` through the agent. The actor
  tombstones, removes the agent, and then removes itself. The CP forgets the host's credentials.

---

## 6. Agent replacement on GPU hosts (D6(a))

### 6.1 Relay [P]

The CP sends `platform_converge` over the agent WebSocket. The agent checks
`service ∈ {node-agent, recovery-actor}` and POSTs the body **unparsed** to `agent.sock`. It relays
the ack and then `platform_converge_state` on each change, with the same 1 s poll, 180 s
unreachable and 2 h bounds as `release/mod.rs:31-41`. On (re)connect, the new agent replays
non-terminal attempts and terminal attempts under two hours (`release/mod.rs:142-203`, `:483-496`).
The trust boundary is unchanged: a host speaks only about its own machine's attempts
(`apply_runner.go:539-608`).

### 6.2 Host step order [P]

A host step is **up to two Attempts: recovery actor first, then node agent**.

- The actor handover ends no sessions (D14 unattended table), and the new actor then converges an
  agent spec that may use a spec format the old actor did not know (rule S2).
- The agent Replacement ends that host's sessions (D9: no restart-safe claim until RH-04). It
  drains through the owned admission restriction as today.
- If the actor handover fails, it is restored, the agent attempt is skipped, and the run stops
  (stop at first failure).

### 6.3 ADR 0004, revert and floor

- **ADR 0004 [F→P]:** the automatic restore moves from `.env.prev` plus `up` (`updater/exec.go:181-206`)
  to "restart the kept `--prev`". No pull and no dependency on the image store. The CP records
  `auto_revert` exactly as today.
- **Revert [P]:** the CP desires the *prior node-agent image* (the `previous` digest of the last
  succeeded non-`auto_revert` attempt, `apply_revert.go:62-100`), **re-rendered from that image's
  own template with current inputs**. A setting changed since is not silently undone, and an old
  image is still run the way it expects to be run. This is where the declarative model beats
  "reinstate the old compose line".
- **D9 floor [P]:** "The manifest pins; the image describes." Each image carries
  `org.quasar.api.level` (an integer). The CP image also carries `org.quasar.floor.node-agent` and
  `org.quasar.floor.recovery-actor`. The CP reads a host's levels by inspecting the **applied image
  digests** reported in inventory (a registry lookup, cached), so `register` needs no new field.
  Hosts below the floor get `below_floor`: only an update is offered. Revert targets must satisfy
  floor ≤ level ≤ CP.

---

## 7. Enrollment and bootstrap

### 7.1 Shapes (D2)

| Shape | The actor creates | First desired specs come from |
|---|---|---|
| Combined | Postgres, CP, then the agent | the actor renders Postgres and CP (no CP exists yet); the **CP renders the agent** on its first boot |
| Control-only | Postgres, CP | the actor renders Postgres and CP |
| GPU host | the agent | the **CP renders** it through the token-scoped bootstrap endpoint (§7.3) |

### 7.2 Control-plane machine bootstrap (the chicken-and-egg) [P]

1. The seed creates the actor (from `QUASAR_ACTOR_IMAGE`, or `:stable` resolved once).
2. The actor creates `machine.json` (install id, machine id, role), generates secrets (DB password,
   `QUASAR_SECRET_KEY`, and on combined hosts a one-time local enrollment secret), and detects GPUs
   (DRI nodes present in `/dev/dri` of the host-`/dev` mount, `Info().Runtimes` for NVIDIA). It
   does not trust `/sys/class/drm`, which is not namespaced in some container hosts.
3. **Choose the release.** The actor image carries `org.quasar.release.version`. The actor fetches
   that release's format-2 manifest with the existing fetcher (`updater/signature_source.go:58-124`)
   and verifies the signature if keys are configured (ADR 0003). It **checks that its own digest is
   listed**, and refuses if it is not. `QUASAR_RELEASE_MANIFEST` overrides this for the product
   lane (D15) and for air-gapped installs.
4. Render Postgres from the template and digest baked into the actor image
   (`org.quasar.postgres.template`, digest from `deploy/pins.env`; R1: created, never updated).
   Render the CP from the CP image's template label after the pull. Both become revision 1 with
   `cause: bootstrap`. Converge Postgres, then the CP (the same Replacement machine, with no `from`).
5. CP first boot: it reads `QUASAR_BOOTSTRAP_ENROLLMENT_FILE` and inserts a single-use
   `host_enrollments` row by hash (idempotent). It imports `Services()` and `Self()` into
   `platform_machines` and `platform_service_*` as revision 1 **byte for byte**. On a combined
   machine it then renders the agent (revision 1) and converges it over the local socket. The agent
   enrolls with the local secret, and the actor deletes the secret file after the agent's
   `applied`. The CP links `platform_machines.host_id` when that node name registers.

### 7.3 GPU host bootstrap [P]

1. Console "Add host" mints the token (the existing `hostenroll` mint; D13 one-liner by default,
   and a Dockge/Arcane snippet tab). The seed input `QUASAR_ACTOR_IMAGE` is **pinned to the CP's
   applied release's actor digest**, so no host starts ahead of the CP (ADR 0002).
2. The actor parses the enrollment string (`qenr1.<FP>.<b64url(wss-url)>.<token>`) to get the URL
   and TLS pin, then calls **`POST /v1/enroll/bootstrap`** with `Authorization: Bearer <token>`
   (**checked, not redeemed**) and its machine facts.
3. The CP (the renderer of record) returns the agent's revision-1 spec and inputs, rendered from the
   agent image of **the CP's own release**. The actor admits it, converges it, and passes the
   enrollment string to the agent as a secret file. The agent enrolls exactly as today
   (`agentws/store.go:93-202`). This is R1's D10 knock-on: the actor holds no CP identity.
4. The agent's first connection relays `platform_inventory`. The CP creates the `platform_machines`
   row bound to that host and imports revision 1.

Idempotency: re-running the seed with an existing `machine.json` ignores every input (D10). A lost
machine-state volume goes through the existing re-key-by-node-name path (#96/#199), and the takeover
guard refuses while the old identity is connected (`agentws/store.go:140-152`).

### 7.4 Secrets as files (D5) [P]

A spec names `secrets: [{name, env_file|target}]`. The renderer emits `QUASAR_X_FILE=/run/secrets/x`
and a read-only mount of the consumer's secret volume. `POSTGRES_PASSWORD_FILE` already exists in
the official image. The CP gains `*_FILE` for the database password, the secret key, the previous
secret key and the bootstrap enrollment. The agent gains `QUASAR_ENROLLMENT_FILE`. The one R1-Q1
exception, an external database password supplied through the manager's `.env`, is copied into
`/state/secrets/external-database/` at first boot and from then on is also delivered as a file.

---

## 8. Release publication (D4, D1)

**[F]** Old CPs accept only `format_version: 1`, exactly two components in a fixed order
(`manifest.go:23`, `:33`, `:122-138`). The asset name is fixed (`github.go:171`). An invalid or
missing manifest is counted, logged and not stored (`detect.go:113-120`).

**[P] Stable.** Publish only `platform-release-manifest-v2.json`, and no format-1 asset. An old CP
logs "release carries no platform-release-manifest.json asset" and counts it as `manifest_invalid`,
so the release is invisible and the release notes say "reinstall". A test in the last format-1 line
should confirm the view shows the count (D4 [I]).

**[P] Edge: a trap D4 does not cover.** Old edge CPs detect branch builds from the
`org.quasar.schema.version` label on `quasar-control-plane:<branch>` (`edge.go:130`). They *would*
offer the RH-06 CP image to a compose stack. Fix: from the RH-06 merge onward, publish edge images
under a new tag family (`edge2-<branch>`, `sha2-<7>`) and **freeze the old `develop` tag** at its
last pre-RH-06 build. The new CP follows the new family. This is a small workflow change, but
without it the clean break is not clean.

**Format 2** [P]:

```json
{ "format_version": 2, "version": "0.6.0", "prerelease": false,
  "source_commit": "<40 hex>", "built_at": "RFC3339", "schema_version": 97,
  "components": [
    { "name": "control-plane",  "image": "ghcr.io/accreleus/quasar/quasar-control-plane", "digest": "sha256:…" },
    { "name": "recovery-actor", "image": "ghcr.io/accreleus/quasar/quasar-recovery",      "digest": "sha256:…" },
    { "name": "node-agent",     "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": "sha256:…" } ] }
```

The normative order is the apply order: control plane first, then per machine actor before agent.
Templates, api levels and floors are **not** in the manifest. They are image labels, covered by
the pinned digests and therefore by the signature. The same data then serves stable, edge and
developer apply. The seed is not a component: its manager owns it. Postgres is not a component
(R1). `turn` is reserved in the `service` enum and has no template (D1).

---

## 9. Frozen-contract changes (need Opus review and owner sign-off)

The owner approved the direction (D6/R1). The text below is a proposal for the RH-06 contract
ticket, which opens the breakdown as #334 did for RH-05.

**`protocol/agent-api.md`**
- §`register`, "Optional identity fields (platform-release amendment 1)": `install_mode` gains
  `owned` ("learned from the agent's own container labels"). `updater_present` is deprecated: an
  owned agent omits it, and the CP reads actor presence from `platform_inventory`.
- Amendment 2, §`release_apply` / §`release_state`: **retired**. Owned hosts never receive them. D4
  means no compose-updater host is managed by an RH-06 CP.
- New §"Platform service convergence (RH06)": `platform_converge{request_id, attempt_id, service,
  revision, spec (opaque object), inputs, deadline}`, `platform_converge_ack`,
  `platform_converge_state{request_id, attempt_id, service, state, reason, applied_revision,
  applied_digest, previous_revision, restored, output}`, `platform_inventory{machine_id, install_id,
  role, actor_version, spec_formats, seed{present, version, digest}, services[], faults[], dumps[]}`,
  `platform_remove`. The state vocabulary: `accepted|pulling|backing_up|replacing|verifying|succeeded|failed|interrupted`.
  Reasons: the existing set plus `actor_absent`, `spec_rejected`, `spec_format_unsupported`,
  `foreign_container`, `create_failed`, `backup_failed`, `insufficient_space`, `restore_failed`,
  `held_for_operator`, `not_updatable`. `replacing` replaces `recreating` (defined today as the
  compose command).
- §RH05 journal "Durable start…/host-wide operation lock": add that the CP never sends
  `platform_converge` to a host holding an open RH05 idle-apply attempt, and the reverse. The
  serialisation lives CP-side through admission-restriction owners, because the actor is a
  different process from the agent's lock.

**`protocol/control-api.md`**
- §"Host enrollment tokens (#12/#96)": add `POST /v1/enroll/bootstrap` (token-authenticated, not
  redeeming). Remove the static token.
- §"Hosts": the host body's `install_mode` gains `owned`, plus a `machine` object (service
  inventory). Add `POST /v1/admin/hosts/{id}/remove`.
- §"Platform releases… (amendment 1)", "The release manifest asset": format 2 and the new asset
  name. `EligibilityReason` gains `actor_absent`, `below_floor`, `spec_format_unsupported`,
  `foreign_container` and `backup_unconfirmed`, and loses `updater_absent`.
- §"Platform-release apply (amendment 2)": the CP target is applied by the recovery actor over its
  local socket. The automatic CP restore extends to non-migrating releases. A migrating release
  takes a pre-update dump, and its started-but-unhealthy outcome is `held_for_operator`.
  Components include `recovery-actor`, and a host step is actor then agent. The fleet request
  gains `external_backup_confirmed`. New `POST /v1/admin/platform/developer-apply` (D15; allowlist
  unchanged).
- §"Self-update hardening (amendment 9)" `PreflightCheckId`: remove `updater_stack_dir` and
  `updater_overlays`; rename `updater_socket` to `actor_socket`; add `dump_space`,
  `foreign_container` and `seed_present`.
- New §"Platform services": `GET /v1/admin/platform/machines` (per machine: seed, actor, Postgres,
  CP and agent, each with version, owner, desired and applied revision, status, and dumps);
  `GET …/machines/{id}/services/{service}/revisions`; `PUT …/machines/{id}/inputs` (home mount,
  public host, ports: the D5 install-time settings).

**`protocol/schema.md`**
- New `platform_machines(id, role CHECK combined|control_only|gpu, host_id UNIQUE NULL → hosts,
  install_id UNIQUE, actor_version, spec_formats, seed JSONB, facts JSONB, faults JSONB,
  reported_at)`.
- New `platform_service_revisions(machine_id, service, revision, spec JSONB, spec_digest, inputs
  JSONB, cause, release_id NULL, created_by NULL, created_at, PK(machine_id, service, revision))`.
- New `platform_services(machine_id, service, desired_revision, desired_digest, applied_revision,
  applied_digest, status CHECK pending|applied|failed|held|missing|drifted, evidence_at)`, with
  `host_setting_groups`' "applied iff both match" CHECK (`0087:47`).
- New `platform_machine_inputs(machine_id, inputs JSONB, revision)`.
- `platform_apply_attempts` gains `machine_id`, `service` and `spec_revision`. The open-attempt
  unique index becomes per machine. The `hosts.install_mode` CHECK gains `owned`, and
  `updater_present` is deprecated.
- §"Not frozen: the updater's local socket" becomes "Not frozen: the recovery actor's local
  sockets and journal". The seed pointer is governed by ADR 0007 (a compatibility policy for two
  Quasar images), not by `protocol/`.

**New ADRs** (not protocol, still reviewed): 0007 the seed contract; 0008 service templates live in
images (rules S1–S4); an amendment to 0004 (the kept-container restore and the non-migrating CP
restore).

---

## 10. Testing seams and adapters

| Level | What | How |
|---|---|---|
| Pure | `servicespec.Render` | table tests; **golden files per format, never edited**; a parity test that renders `node-agent.json` and `control-plane.json` against the fixture inputs and compares with `deploy/docker-compose.yml` service definitions (env keys, mounts, devices, caps), listing the allowed differences (secret files, actor socket). This replaces `TestEnrollHostComposeMatchesBase` and keeps the contributor lane honest. |
| Pure | `converge.Decide` + `RestoreDecision` | **a crash at every phase boundary**: for each phase *p*, run to *p* against `engine/fake`, drop all memory, reload the journal, re-run, and assert D8's terminal outcome. Include fsync failure (`writeFile` hook): refused before acting. |
| Pure | `PlanServiceSpecs`, `PlanRelease` (floors, S2/S3) | existing `plan_test.go` style |
| Actor | the handover state machine, the seed loop, the race guard, restore, uninstall | `engine/fake` + `t.TempDir()` journals. Two actor processes in one test, sharing a temp `/state`, for the lock and authority rules. |
| Engine adapter | `engine/docker` | against a real daemon: a docker-in-docker job (`make test-recovery-engine`): create, rename, update restart, exec streaming, pull by digest, and `Info().Runtimes`. |
| CP store | the spec tables and the import-on-restore rule | `make test-db` (a real ephemeral Postgres, `-p 1`) |
| CP↔actor | `localSocket` over a temp unix socket (the existing `UpdaterAPI` test pattern); `agentRelay` against the in-memory agent registry | Go |
| Agent | `platform_relay` opacity (bytes in equal bytes out), the scope refusal, replay | Rust, over a fake socket (the existing `release/tests.rs` pattern) |
| Seed compatibility | the seed from **release N-1** against the actor of release N (the pointer format) | a CI matrix job |

**Only live hosts can prove:** a real reboot or daemon restart mid-phase (especially `old_stopped`
and the handover window); NVIDIA device requests and the driver volume on the NVIDIA GPU host; DRI
node selection on the AMD GPU host; a migrating update with a realistic dump size and a disk-full
refusal; Dockge and Arcane "redeploy stack" against the race guard (R1: this replaces the Portainer
test); the self-signed one-liner with `--pinnedpubkey`; restore into a fresh install; and
control-only "no agent on this machine" coverage.

---

## 11. Trade-offs

**Where leverage is high**
1. **Templates in images.** Version skew for env, mounts and devices disappears for every service
   except the one hard case (the CP rendering its successor), and a written rule (S2/S3) bounds
   that case. A new agent knob is a one-file change beside its Rust.
2. **One executor for every change.** Release, developer apply, revert, an install-time setting
   edit, bootstrap, `repair` and `restore` are all "a new desired revision plus one Replacement".
   Revert re-renders the old image with current inputs. Drift detection is one generic comparison.
3. **Opaque relay.** The agent never changes when the spec format grows, which is the Rust side of
   the skew problem solved for free.
4. **Extension points cost nothing now.** Postgres updates later means lifting `not_updatable` and
   adding a drain choreography. TURN means a template and an enum value already reserved. Podman is
   a third `Engine` adapter, and the seam already exists for tests.

**Where it is thin or costly**
1. **Two renders and a new long-lived compatibility discipline** (per-format frozen semantics,
   S1–S3). This is a new kind of contract: a template-format bump is a two-release change. It pays
   only if templates actually change between releases, which is expected, because compose changed
   often (research B §1).
2. **Authority.** A rendered spec is more power than an image swap. Verification by re-render
   (§2.5) closes the gap, but it is load-bearing security logic that must never be "temporarily"
   bypassed.
3. **Two desired-state systems.** RH-05 host policy covers in-process and restart configuration,
   and RH-06 service specs cover container shape. The line [P] is *container shape (image, mounts,
   devices, ports, network, env the image reads only at start) belongs to the spec; everything the
   agent applies itself stays RH-05*. For example, the home **mount** is a spec input while the
   home root *path within it* stays RH-05 typed policy (`policy.rs:2761-2802` already enforces
   "within the deployment mount"). Ambiguous knobs will need adjudication one by one.
4. **The hand-rolled Engine client in Go** (about 14 endpoints, including exec stream framing) has
   to be written and tested. This was chosen over sharing the Rust Bollard facade because the
   renderer must be shared with the CP, which is Go.

**YAGNI: flexibility this design declines in RH-06**
- `settable_env` is in template format 1, but **no console editor for arbitrary env ships**. Only
  D5's install-time inputs (home mount, public host, ports) get UI. Other knobs keep their defaults
  until someone needs them.
- No reconciliation loop (D8). No automatic repair of a deleted service container (the D6(a)
  accepted gap).
- Postgres specs are stored but never converged. Nothing is built for major versions.
- No spec-diff UI, and revision history is capped at 5 per service on the machine. Postgres keeps
  all revisions: they are small.
- No direct Arcane or Portainer integration. No per-actor CP identity (D6(b) stays the recorded
  upgrade path). The actor-to-CP inventory path is the thing it would upgrade.
- Consumer secret volumes are a workaround for Engine API 1.40, to be removed when the floor rises.

---

## 12. Slice order (vertical tracer bullets)

0. **Contracts and design (blocking).** An amendment proposal for §9, ADR 0007 and ADR 0008, Opus
   review and owner sign-off. In parallel, a D16 mockup ticket (Add host with one-liner and snippet
   tabs, the machine service inventory, below-floor state, held-for-operator and restore guidance,
   developer apply).
1. **servicespec + agent/CP templates.** Templates, label plumbing in `build-images.sh`, the image
   contract, `template-check`, the compose-parity golden test. No behaviour change.
2. **engine + converge + journal.** An actor binary with a local `converge` verb converging a
   *dummy* service on a dev machine. The crash-at-every-phase suite.
3. **Tracer A: a GPU host end to end.** Seed, then actor, then `POST /v1/enroll/bootstrap`, then the
   agent enrolls, then `platform_inventory` becomes revision 1 in Postgres (a contributor-lane CP is
   fine here).
4. **Agent replacement.** Developer apply through the relay, ADR 0004 kept-container restore,
   revert, and the floor (`below_floor`).
5. **Tracer B: combined and control-only bootstrap.** Postgres and CP rendered by the actor,
   secrets as files, the local enrollment secret, CP import, and the CP-rendered agent.
6. **CP replacement.** Non-migrating over the local socket (`Adopt`, the import), then migrating
   with a dump, `dump_space`, `held_for_operator`, the `restore` verb, and the
   external-DB confirmation.
7. **Actor handover** (§3.2), including the seed pointer and the seed N-1 compatibility job.
8. **Release publication.** Format 2, the new asset name, the edge tag-family freeze, S3 in the
   manifest generator, unattended rules (D14). **Retire the updater.**
9. **Uninstall / remove host / repair**, the race-guard faults, the `enroll-host.sh` rewrite and the
   site installer.
10. **UI slices** against the approved mockups.
11. **Evidence.** Plain Docker for all three shapes, Dockge, Arcane (a redeploy test of the race
    guard), restore into a fresh install, and the operator documentation pages.

---

## Appendix: proposed `CONTEXT.md` additions

- **Service specification**: the fully rendered, secret-free description of how one platform
  service runs on one machine. It is stored as numbered spec revisions. *Avoid* "compose
  definition" and "config" (RH-05 owns configuration).
- **Service template**: an image's own statement of what it needs in order to run, carried as a
  label and rendered into a service specification. *Avoid* "manifest" (taken twice already).
- **Machine state**: the recovery actor's volume holding the identity, secrets, cached
  specifications, journal and dumps. It must never be deleted.
- **Seed pointer**: the frozen-format file from which the seed recreates the recovery actor.
- Retire **Updater** once slice 8 lands, and keep "Recovery bundle" withdrawn (R1).
