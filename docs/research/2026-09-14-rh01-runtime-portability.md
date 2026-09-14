# Docker and Podman API compatibility constraints

Research date: 2026-09-14. Resolves [research #226](https://github.com/accreleus/quasar/issues/226), under [Wayfinder #224](https://github.com/accreleus/quasar/issues/224), informing #208 and #209.

**Evidence level:** primary documentation and a tagged upstream API specification only. No daemon was contacted, no container was launched, and no runtime behavior was tested. Conformance cases below are proposed acceptance evidence, not completed checks. This note does not select an interface, retry policy, minimum supported release, or Podman support commitment.

## API identity and negotiation

Docker documents negotiation of a mutually supported API version; downgrading removes newer features. An explicit `DOCKER_API_VERSION` disables automatic negotiation in the documented CLI/SDK paths. A Rust client must establish its own library's behavior rather than inherit the Go SDK's guarantee. Record engine identity, server minimum/maximum, client supported range, and the selected version. An empty intersection needs a diagnostic before mutation. [Docker API versioning](https://docs.docker.com/reference/api/engine/)

Podman's service documents two distinct APIs: a Docker v1.40 compatibility layer and native Libpod. It explicitly accepts requests bearing unsupported version numbers. Consequently, an accepted `/v1.xx/` prefix or successful ping cannot prove that every requested field has the Docker meaning. Its service can terminate after inactivity and be restarted by socket activation; endpoint availability and container lifetime are separate concerns. The API executes with the service user's authority. [Podman service](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html)

**Implication:** engine family, API version, rootless mode, and individual operation support are different evidence. Compatibility must be attached to the exact engine/client/configuration combination exercised. Do not derive GPU, networking, or UID mapping support from a version string alone. Podman CLI examples in this note establish runtime features, not their transport through the Docker compatibility API. Native Libpod is a separate surface whose schemas would require explicit translation and validation. [Podman API reference](https://docs.podman.io/en/latest/_static/api.html)

The current Docker documentation is moving: even its overview example and matrix can report different maximum API versions. Therefore this research deliberately does not infer the fleet version from “latest.” The tagged [Moby v28.3.0 API specification](https://github.com/moby/moby/blob/v28.3.0/api/swagger.yaml) provides a reproducible baseline for the lifecycle facts below; target-version documentation must be checked when an implementation baseline is chosen.

## Devices, identity, and mounts

Docker documents Linux CDI enabled by default from Engine 28.3.0. A CDI request refers to a fully qualified device name whose specification must exist on the daemon host. CDI can inject libraries, environment, mounts, and hooks as well as device nodes. Raw device mappings and vendor GPU requests should therefore not be assumed equivalent to CDI injection. [Docker device options](https://docs.docker.com/reference/cli/docker/container/run/#device)

NVIDIA documents specification generation since Toolkit 1.12 and automatic refresh since 1.18. Driver removal and MIG reconfiguration remain explicit refresh gaps. Podman supports CDI device syntax from 4.1. NVIDIA warns that CDI injection can conflict with the legacy NVIDIA OCI hook; its examples include SELinux configuration. These establish prerequisites, not proof that Quasar's Vulkan/encode/input workload works. A successful `nvidia-smi` only checks a narrower workload. [NVIDIA CDI support](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html)

Podman's documented rootless constraints include:

- Devices are bind mounted; retained SELinux labels can deny access.
- Access granted only by supplementary group membership may require `keep-groups`; it requires `crun` and is documented unavailable for remote commands. Compatibility-API applicability remains unproven.
- `keep-id` maps the caller's UID/GID into the container and can override the image user unless explicitly set.
- `:U` recursively changes source ownership; `:z`/`:Z` relabel source content for shared/private use.
- Rootless networking defaults to pasta; named bridge networking and explicit host networking are distinct modes.

These are CLI/runtime facts, not verified API mappings. [Podman run](https://docs.podman.io/en/latest/markdown/podman-run.1.html)

Docker rootless runs both daemon and containers in user namespaces, unlike rootful `userns-remap`. Subordinate IDs and mapping helpers are prerequisites. Container UID 0 therefore does not establish host-root authority, and a requested numeric app UID does not alone establish access to the managed home. [Docker rootless model](https://docs.docker.com/engine/security/rootless/)

Docker bind sources belong to the daemon host, not the API client's filesystem. Mounting hides existing container content; source existence and propagation settings matter. CLI `--volume` and `--mount` differ on absent sources, so translating CLI strings mechanically can change behavior. Read-only bind semantics also interact with recursive submounts and kernel support. [Docker bind mounts](https://docs.docker.com/engine/storage/bind-mounts/)

**Conformance obligation:** inspect and exercise the final device set, effective UID/GID and supplementary groups, home read/write ownership, socket access, mount permissions and visibility, and SELinux enforcement. Include unavailable device/CDI specification and missing source negatives. Keep relabel/chown actions visible as host mutations; an adapter must not silently introduce them to make a request succeed. How requirements are represented in Quasar's interface remains a decision for #208.

## Networking is version- and mode-dependent

Docker's current rootless troubleshooting documents a particularly important version boundary: before Engine 29.5, host networking remained inside RootlessKit's namespace; newer documented behavior can share the actual host namespace. Inspected IP addresses are not automatically reachable externally, and user-mode networking and source-IP preservation vary with drivers. “Rootless never supports real host networking” is now too broad a claim. [Docker rootless networking](https://docs.docker.com/engine/security/rootless/troubleshoot/)

**Required evidence:** for each supported configuration, exercise the exact TCP/UDP directions Quasar needs, published bind address, namespace sharing, DNS, source address behavior, loopback access, and cleanup of occupied ports. Report the realized topology and engine/network-helper versions. A generic HTTP smoke test cannot establish WebRTC UDP reachability or streaming performance. No performance ranking between engines is supported by this research.

## Lifecycle, logs, events, and cancellation

The tagged Docker specification separates create, start, stop, inspect, wait, and delete. Start has 204 success and 304 already-started responses; stop similarly distinguishes already stopped. Wait has `not-running` (default), `next-exit`, and `removed` conditions, with process status in its JSON response. Logs use the attach stream format: non-TTY streams are multiplexed, TTY streams are raw. Logs do not perform attach's connection upgrade. A client must preserve process exit status separately from transport errors and handle partial stream frames. [Moby v28.3.0 API specification](https://github.com/moby/moby/blob/v28.3.0/api/swagger.yaml)

Podman documents additional wait conditions such as configured, created, running, and healthy, and defaults to stopped. This CLI contract is not proof that Docker `next-exit` has the same race behavior through its compatibility endpoint. [Podman wait](https://docs.podman.io/en/latest/markdown/podman-wait.1.html)

Docker retains only the last 256 events for historical retrieval. Podman events depend on its configured logger, and `none` disables them. Neither fact supports using an event subscription as a complete durable lifecycle ledger. [Docker events](https://docs.docker.com/reference/cli/docker/system/events/), [Podman events](https://docs.podman.io/en/latest/markdown/podman-events.1.html)

Podman documents a race where removal can delete logs before a follower reads the final content. Log collection completion cannot be inferred from removal success. [Podman logs](https://docs.podman.io/en/latest/markdown/podman-logs.1.html)

**Inference requiring fault-injection evidence:** cancelling a client task means the caller stopped waiting; it does not prove rollback of an already submitted create/start request. None of the cited API contracts provides an exactly-once transaction spanning these operations. A disconnect after request submission must remain distinguishable from a request known not to have reached the engine. A read failure must not become “container absent.” Changing transport to CLI or another engine after such a failure can repeat a mutation; no automatic fallback is justified by these sources.

A candidate reconciliation design could use a stable operation identity and ownership metadata to locate the exact object, then inspect it. Name conflict alone does not establish ownership. If a process has already exited, inspection of present state cannot prove it never started. The required durable identity, ownership checks, grace period, and caller-visible uncertain result belong to human/interface decisions; this note does not settle them.

## Minimum conformance evidence before a support claim

| Area | Proposed evidence, still unperformed |
| --- | --- |
| Negotiation | Old/new supported versions, empty overlap, forced version, unsupported fields, wrong engine, socket permission failure, service reactivation |
| Launch | Effective image/user/env/mount/network/device config; missing image; duplicate name; foreign ownership; immediate exit; warning preservation |
| Uncertain mutation | Drop response before/after create or start commits; restart client; reconcile without duplicating app processes or deleting a foreign object |
| Lifecycle | Repeated start/stop/remove, nonzero exit, force/graceful stop, missing versus inaccessible object, restart-policy and auto-remove races |
| Wait/cancellation | Subscribe before/after exit, selected condition, cancellation during handshake/read, reconnect; bounded task/socket cleanup without implicitly killing workload |
| Observability | TTY/non-TTY, fragmented frames, stderr separation, binary output, backpressure, log-driver variation, final-log/remove race, event disconnect/replay gaps |
| Rootless/CDI | Exact UID/GID/groups and SELinux configuration; stale/missing CDI; requested GPU isolation; home persistence and sockets; real UDP behavior |

Record exact engine, API, client library, OCI runtime, kernel, rootless mode, logging/network drivers, and toolkit versions with each result. Docker rootful, Docker rootless, Podman rootful compatibility API, and Podman rootless compatibility API are separate cells. A cell may remain unsupported or unknown; passing one does not establish the others.

Open questions are the supported version floor, capability evidence model, whether any Libpod-only operation is needed, ownership/reconciliation semantics, wait/cancel contract, log retention expectations, and topology/device requirements. Research resolves what must be considered; it does not complete #208/#209 or authorize runtime tests and deployment.

## Artifact validation

Whitespace and staged privacy checks passed. `make verify` ran on the research worktree: 405 passes, 14 failures in benchmark checks with the required `qses` helper absent, and two warnings (ShellCheck unavailable; `qses-stop` skipped). This is not a green repository verification result. No code changed, and no live runtime conformance was attempted.
