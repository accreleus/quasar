# RH-07 research: where Quasar assumes Docker

Surveyed at develop `0be49b14`. Paths are relative to the repository root. Nothing was modified. Where this says how Podman behaves ("likely", "verify"), that comes from general knowledge of Podman, not from a test run, and must be checked against a real Podman before anything is designed around it.

## 1. How the agent and recovery actor talk to the engine

**Client library.** Both use bollard 0.21.1 with only the `pipe` feature, so the only transport is a Unix socket. No code shells out to `docker`/`podman`, and a test enforces that.
- `node-agent/Cargo.toml:22` and `node-agent/crates/quasar-runtime/Cargo.toml` (the `bollard = "=0.21.1"` line).
- `node-agent/tests/engine_subprocess_convention.rs:88-93` fails the build if the code runs a `docker` or `podman` child process.
- `deploy/image-contract.json:68-71,205` forbids `docker` and `podman` binaries in the images.
- The recovery crate's own hyper dependency is only for fetching release assets (`trust/https.rs`). Its engine access goes through `quasar-runtime` (`node-agent/crates/quasar-recovery/src/engine/docker.rs:20-116`).

**Socket discovery** (`node-agent/crates/quasar-runtime/src/config.rs`):
- `:45-57` refuses to start if `DOCKER_CONTEXT`, `DOCKER_TLS`, `DOCKER_TLS_VERIFY` or `DOCKER_API_VERSION` is set.
- `:62` reads `DOCKER_HOST`. `:101-109` accepts only an absolute `unix://` path.
- Otherwise `:69-95` reads `~/.docker/config.json`, refuses a non-default `currentContext`, and falls back to the hardcoded `/var/run/docker.sock` at `:95`.
- **Why it matters:** there is no Podman path (`$XDG_RUNTIME_DIR/podman/podman.sock`, `/run/podman/podman.sock`) and no `CONTAINER_HOST` support. The only way to point at Podman is `DOCKER_HOST`.

**Version negotiation** (`node-agent/crates/quasar-runtime/src/docker.rs:81-150`):
- Every operation calls `discover()`, which does `GET /version`.
- It takes `min(daemon ApiVersion, bollard ceiling 1.53)`, requires at least `API_FLOOR` = 1.40 (`node-agent/crates/quasar-runtime/src/engine.rs:18-23`), and requires major version 1.
- `:134-148` rewrites every request path to `/v{selected}`.
- **Why it matters:** Podman 5's compat API reports 1.41, which clears the floor, so negotiation should work. But `EngineInfo.name` comes from `platform.name` (`:109-114`), and a comment at `engine.rs:23` says discovery "is not a claim that GPU/rootless capabilities were tested".

**`/info` facts** (`docker.rs:154-192`, `node-agent/crates/quasar-runtime/src/docker/platform.rs:70-87`):
- They read `CDISpecDirs`, `DiscoveredDevices`, `Runtimes`, `DefaultRuntime`, `SecurityOptions`, `CgroupVersion`, `Name`.
- `DockerRootDir` is read in `node-agent/crates/quasar-runtime/src/docker/inspection.rs:215-229` and is required to be an absolute path.
- **Why it matters:** the CDI fields only exist on Docker 27+. Podman reports different runtime names (crun), and its `DockerRootDir` is the storage graphroot (under `~/.local/share/containers` when rootless).

**Endpoints in use:**
- **Containers:** `GET /version`, `/info`, `/containers/json?all`, `/containers/{id}/json`; `POST /containers/create`, `/start`, `/stop`, `/update` (restart policy, `platform.rs:371-394`), `/rename`, `/wait?condition=not-running` (`:432-446`); `DELETE /containers/{id}?force&v`; `GET /logs?tail`; `PUT /containers/{id}/archive` (`:475-493`).
- **Images:** `POST /images/create` with X-Registry-Auth (`:89-117`), `GET /images/{ref}/json`, `GET /images/json?all`, `DELETE /images`, `POST /build` and `/images/{}/tag` (`node-agent/src/runtime/docker/build.rs:70`).
- **Exec:** `POST /containers/{id}/exec`, `/exec/{id}/start`, `GET /exec/{id}/json` (`node-agent/src/runtime/docker/application.rs:113-250`).
- **Volumes and networks:** create, inspect and delete for both (`platform.rs:495-598`).
- **No `/events` streaming anywhere.** A grep found no callers.

**Registry auth.** `node-agent/crates/quasar-runtime/src/docker/credentials.rs:1-29,56-63,142-145` reads the Docker `config.json` format (`auths`, `credHelpers`, `credsStore`) and defaults to `index.docker.io`. Podman's `auth.json` has the same shape but lives at a different path.

## 2. The container specs Quasar creates

**Recovery actor and seed profile** (`node-agent/crates/quasar-recovery/src/seed/profile.rs:21,51-66`; `node-agent/crates/quasar-recovery/src/recipe/mod.rs:963-990`):
- The engine socket is bound to a hardcoded in-container path, `/var/run/docker.sock` (also `recipe/mod.rs:88`).
- `security_opt: label=disable`, default bridge network, `unless-stopped`, named volumes `quasar-machine` and `quasar-recovery-agent`.

**Node agent** (`recipe/mod.rs:879-956`):
- `network_mode: host`, `cap_add` NET_ADMIN and SYSLOG, `init: true`, `unless-stopped`.
- Devices `/dev/dri` rwm, `/dev/uinput` rwm, `/dev/kmsg` r (`:895-916`), plus `device_cgroup_rules: ["c 13:* rmw"]` (`:948`).
- Binds: the socket, `/run/quasar-agent` from the host, `/dev/input`, `/dev → /host/dev`, `/sys/kernel → /host/sys/kernel`, the homes and templates roots at identical paths, `/etc/os-release`.
- **No `label=disable`** (`:951`), even though it mounts the engine socket.
- On NVIDIA it adds a DeviceRequest `{count:-1, caps:[[gpu]]}`, the `quasar-nvidia-driver` volume and an NVIDIA environment (`:918-935`, `:822-834`).

**Postgres and control plane** (`node-agent/crates/quasar-recovery/src/recipe/control.rs:211-235,392-415`):
- Both run on a custom bridge network, `quasar-platform` (`recipe/mod.rs:64`, created as a `bridge` driver at `platform.rs:525-544`).
- Postgres has a `pg_isready` healthcheck. The control plane publishes 8080 and 8443 on the configured host ports and relies on the image's own healthcheck.
- The control plane's uid is fixed at 1000 (`recipe/mod.rs:113`). The actor chowns the control socket and secret files to it (`node-agent/crates/quasar-recovery/src/server.rs:49-51`, `actor.rs:955`, `install_control.rs:26`).

**Helpers** (GPU probe, `--gpus` probe, secrets writer, final dump, DB helper):
- The probe runs with `network none`, binds `/dev → /host/dev` read-only, and runs a shell script (`node-agent/crates/quasar-recovery/src/probe.rs:33,165-189`).
- The `--gpus all` probe (`:191-219`) is how the actor decides whether the engine serves `--gpus`.
- The secrets writer is created and then filled with `upload_archive` (`actor.rs:1297-1321`).

**App (game) containers** (`node-agent/src/session/container.rs`; the header contract is at `:19-51`):
- `--cap-drop ALL`, then 8 capabilities added back (`:1045`), including KILL and SYS_NICE.
- `seccomp=unconfined` (`:1094-1108`); `apparmor=quasar-app` or `unconfined` (`:1120-1133`); optional `systempaths=unconfined` (`:1140`); `no-new-privileges`; `--pids-limit 8192`; `--shm-size 1g`.
- Network is `none` or `bridge` (`host` only by operator choice) (`:1404-1420`).
- Each virtual input node is passed with `--device` (`:1213-1216`), `/dev/fuse` when present (`:1224`), and udev data via a bind to `/run/udev/data` (`:1156-1167`).
- GPU: `--gpus all` plus the driver volume plus `--device /dev/dri`, with **one numeric `--group-add` per DRM node's host gid** (`:1637-1667`).
- PUID and PGID are passed as environment variables, never as `--user`, because the entrypoint starts as root and then drops privileges (`:1254-1265`).
- In the realized request, the NVIDIA DeviceRequest is `driver:"nvidia"` (`node-agent/src/runtime/docker/application.rs:369-374`).
- Typed binds use `create_mountpoint:false` (`:394`).
- Ownership labels: `io.quasar.agent-owner` (`node-agent/crates/quasar-runtime/src/ownership.rs:8`) and `io.quasar.application-operation` (`application.rs:31`).

**Audio and diagnostic helpers** (`node-agent/src/runtime/docker/helpers.rs:1149-1228`):
- `user 0:0`, `cap_drop ALL`, `security_opt ["no-new-privileges"]`, read-only root filesystem except for audio, `network none`, pids 512.
- NVIDIA probes use a DeviceRequest with `driver:"nvidia"`. The audio helper sets `HealthConfig NONE`.

**Legacy Compose** (still documented): `deploy/docker-compose.yml:307,326,330,579,588,630,638-665`. The NVIDIA overlay's `gpus: all` is at `deploy/docker-compose.nvidia.yml:60-65`.

## 3. Docker-only behaviour the code relies on

**Strict read-back after create** (`node-agent/src/runtime/docker/application.rs:839-921`). Every app launch re-inspects the container and rejects it unless all of these hold:
- `HostConfig.Runtime` is empty, `runc`, or `nvidia` (`:862-866`).
- `UsernsMode` is empty (`:853-856`) and `CgroupnsMode` is not `host`.
- `SecurityOpt` matches exactly.
- Every device reports `cgroup_permissions == "rwm"` (`:898-910`).
- The DeviceRequest is echoed back exactly (`exact_nvidia_all_request`, `:628-634`).

The helpers do the same at `helpers.rs:729-751,787`. **Why it matters:** Podman defaults to crun, may report `keep-id` or other userns modes, and turns `--gpus` into CDI. Under Podman these checks will probably fail every launch.

**GPU refusal detection by message text.** `node-agent/crates/quasar-recovery/src/engine/mod.rs:154-173` recognises "no GPU" only by Docker's exact error strings. Any Podman wording falls through to "transient", so the probe retries and then the start fails.

**Privileged exec.** `application.rs:113-128` runs a privileged exec as root to `umount /proc/driver/nvidia/params`.

**`unless-stopped` needs a long-lived daemon:**
- Seed interface 1 (ADR 0007) freezes the restart policy and relies on the engine re-applying it (`docs/adr/0007-the-seed-interface-is-frozen.md:36-40,76-99`).
- The seed restarts only an actor that is exited and has `unless-stopped` (`node-agent/crates/quasar-recovery/src/seed/mod.rs:150,180`).
- Hand-over and replace change restart policies with `/update` (`replace.rs:548,896`, `handover.rs:964,1027,1273`, `uninstall.rs:386`).
- **Why it matters:** Podman only restarts containers at boot through `podman-restart.service`, which covers the `always` policy, and a rootless user also needs lingering enabled.

**Healthchecks gate readiness.** Replace and install wait for `health == healthy` (`node-agent/crates/quasar-recovery/src/replace.rs:700-730`, `install_control.rs:318-325`, `explain.rs:14`). Podman runs healthchecks through systemd timers, so if a rootless host has no user systemd session, health may never leave `starting`.

**Finding its own container.** `node-agent/crates/quasar-runtime/src/self_inspection.rs:8-51` expects a Docker-style `/containers/<64hex>/hosts` path in mountinfo. Podman's layout is `overlay-containers/<id>/userdata`, so it falls back to a 12-character `$HOSTNAME`. The seed accepts that prefix (`seed/mod.rs:529`). The same self-inspection drives the NVIDIA driver-volume host path (`node-agent/src/nvidia_volume.rs:345-380`) and the storage-liveness host-path mapping (`node-agent/src/session/storage_liveness.rs:120-142`, which already checks `/run/.containerenv`).

**Named volumes and the socket volume's host path.** The socket volume's daemon-host mountpoint is bound in by subdirectory (`recipe/mod.rs:93-96`; `volume()` in `platform.rs:495-502`). Legacy `-v` binds rely on Docker creating a missing host source directory. Podman errors instead, which bites `/run/quasar-agent` on the host.

**Compose labels.** The race guard and submit logic read `com.docker.compose.*` (`node-agent/crates/quasar-recovery/src/race_guard.rs:21-23`, `node-agent/crates/quasar-recovery/src/submit.rs:59`).

**Other Docker-specific checks:**
- **Mount deny list:** the node agent's `node-agent/src/session/mount_policy.rs:24-54,261` and the control plane's `control-plane/internal/mountpolicy/mountpolicy.go:49-50`. Both already list `/run/podman/podman.sock`, but neither covers a rootless `/run/user/<uid>/podman/podman.sock` by name. The agent's socket-file check (`:261`) catches it.
- **`/run` tmpfs:** `deploy/Dockerfile.control.prod:110-113` notes that a Podman tmpfs over `/run` would hide `/run/quasar`.

## 4. Seed, installer and docs: what an operator runs

**Quick-start generator** (`site/src/data/stack-template.js`):
- `:153-175` is the seed stack: `/var/run/docker.sock:/var/run/docker.sock`, `security_opt: [label=disable]`, `restart: unless-stopped`, volume `name: quasar-machine`.
- `:192` is `pinsCommand`: `docker pull` followed by `docker image inspect --format '{{range .RepoDigests}}…'`.
- The generated script runs:
  - `command -v docker` and `docker info` (`:226-235`);
  - `docker ps -aq --filter label=com.docker.compose.service=…` (`:254`);
  - `docker container inspect` (`:266`);
  - `docker pull -q` plus the RepoDigests inspect (`:276-282`);
  - `sudo install -d -o uid -g gid` for the homes and templates roots;
  - the host sysctl and `modprobe uinput` (from `site/src/data/platforms.js:19-44`);
  - `docker run -d --name quasar-seed --restart unless-stopped --security-opt label=disable -v /var/run/docker.sock:/var/run/docker.sock -v quasar-machine:…:ro` (`:301-306`);
  - a wait loop on `docker exec quasar-recovery quasar-recovery status` (`:311`);
  - `docker exec quasar-control-plane cat /run/quasar/setup-token` (`:327`).

**`deploy/enroll-host.sh`** (the control plane serves it through `control-plane/internal/enrollscript/enrollscript.go`):
- `:460` defines `dk() { $SUDO docker "$@"; }`, which **assumes root or sudo**.
- `:501-506`: `command -v docker`, `docker info`.
- `:639-664`: `ps -a --format`, `inspect -f`, `volume ls`.
- `:669-681`: `docker run --rm --security-opt label=disable -v /var/run/docker.sock…`.
- `:704-743`: inspect of labels, `rm -f`, `volume rm`.
- `:770`: `docker info --format '{{.Name}}'`.
- `:798-800`: `image inspect` and `pull`.
- `:826-827`: seed `docker run … --restart unless-stopped`.
- `:884-925`: `logs`, `exec`, `start`.
- `:590-627`: loads the AppArmor profile with `apparmor_parser` (the profile mentions crun at `:266-268`).

**Printed commands.** `node-agent/crates/quasar-recovery/src/uninstall.rs:722-728` prints `docker run --rm -it -v /var/run/docker.sock:/var/run/docker.sock …`. `seed/mod.rs:811` prints `add -v /var/run/docker.sock:…`.

**Site docs.** Count of `docker <verb>` occurrences per file under `site/src/content/docs/`:

| File | Count |
|---|---|
| `install/legacy-compose.mdx` | 28 |
| `install/install.mdx` | 22 (including `docker restart quasar-recovery` at `:343`) |
| `troubleshooting/install.mdx` | 16 |
| `operations/uninstall.mdx` | 15 (`docker volume rm` at `:95,118`) |
| `install/move-existing.mdx` | 12 |
| `operations/health.mdx` | 8 |
| `network/https.mdx` | 7 |
| `troubleshooting/connecting.mdx`, `operations/backup.mdx`, `install/verify.mdx` | 6 each |
| `install/second-host.mdx` | 5 |
| `operations/upgrading.mdx` | 4 |
| `install/first-run.mdx` | 3 |
| `start/requirements.mdx`, `reference/environment.mdx`, `network/reverse-proxy.mdx` | 2 each |
| `reference/ports.mdx`, `admin/images.mdx`, `admin/hosts.mdx` | 1 each |

The operator surface is mainly `docker exec quasar-recovery quasar-recovery status|reconfigure|restore`, `docker logs`, `docker exec quasar-postgres`, and `docker volume rm`.

## 5. Host prerequisites and readiness checks, and how rootless changes them

**Engine checks** (`node-agent/src/readiness/runtime_facts.rs`):
- `runtime_endpoint`'s remediation says "Check that Docker is running… /var/run/docker.sock" and "add its user to the group owning /var/run/docker.sock".
- `runtime_api_version`'s remediation says "Docker Engine 19.03 or later".
- `runtime_capabilities` and `runtime_cdi` only report, and say "GPU injection does not use CDI".

**Host checks** (`node-agent/src/readiness.rs`):
- `uinput` (`:1291-1321`) requires a write-open of `/dev/uinput`. Rootless: the host node is root-owned, so this will fail.
- `user_namespaces` (`:1332+`) reads host sysctls. Rootless needs userns anyway, and Steam's nested bwrap then runs inside the rootless userns.
- `app_apparmor_profile` (`:1440`) needs a root-loaded profile. Under rootless, a per-container `apparmor=` option may be refused.
- `render_node`, `host_render_node`, `dri_node_app_access` (`:1229,1545,1597`): the gid-based access model breaks under subuid/subgid mapping unless groups are kept (`--group-add keep-groups` with crun).
- `media_reachability` (`:1835-1990`) depends on NET_ADMIN to read nft/iptables in the host network namespace. In rootless, capabilities inside the user namespace do not grant that, so the check becomes skip. Its symptom text (`:1883`) and `site/src/content/docs/reference/ports.mdx:39-41` assume Docker's iptables DNAT for the control plane. Rootless port forwarding (rootlessport or pasta) passes through the host firewall and may hide the client source IP.
- `xid_visibility` (`:591`) needs SYSLOG plus `/dev/kmsg`, which rootless probably won't allow.
- `nvidia_*` and `driver_volume_version`: the agent installs the NVIDIA userspace into a volume.

**Rootless-specific breakage:**
- **Input nodes.** `mknod` of `/dev/input/eventN` (`node-agent/src/session/virtual_input.rs:15-23,346-378`) and of i2c nodes (`node-agent/src/ddc.rs:118-137`) is not allowed inside a user namespace. `device_cgroup_rules` has no effect or is refused under rootless.
- **Host `/run`.** The `/run/quasar-agent` bind needs root on the host.
- **Privileged ports.** Default ports 8080 and 8443 are fine. 443 (the hardened Caddy overlay, `ports.mdx:20`) needs `net.ipv4.ip_unprivileged_port_start`.
- **WebRTC UDP.** The agent uses host networking with the ephemeral range 32768-60999 plus mDNS 5353. `network=host` works rootless, but avahi and mDNS inside it need checking.
- **Host sysctls.** The UDP `wmem_default` sysctl and `modprobe uinput` remain root-only host preparation.
- **Ownership.** Homes owned by PUID/PGID end up as subuids on the host. The control-plane uid 1000 is remapped.

## 6. What the repo already says about Podman and rootless

**Status statements:**
- `site/src/content/docs/start/requirements.mdx:27`: "Podman, rootless Docker | Not supported".
- `site/src/content/docs/install/legacy-compose.mdx:70-74`: "Set `QUASAR_DOCKER_SOCKET=/run/user/<uid>/podman/podman.sock` … `label=disable`, without which an SELinux-enforcing host refuses every call to the mounted socket."
- `docs/configuration.md:822-834` says there is no docker or podman executable and no CLI fallback. `:958-961`: "Podman operators may point `DOCKER_HOST` at Podman's Docker-compatible Unix socket, but that configuration is not certified." Journals record the endpoint, and changing it while a record is non-terminal blocks boot.
- `docs/configuration.md:1000-1006`, the design constraint for a future rootless mode: "the API endpoint is the socket visible inside the agent, while bind-mount source paths belong to the daemon host. Socket access, subordinate UID/GID mappings, supplementary groups and persistent-home ownership must all agree; a successful connection proves none of the GPU/input/network requirements. Do not recursively change home ownership or relax device/security requirements merely to make a connection work."
- `docs/runtime-api-recovery.md:168-171`: "Docker is the validated engine… Podman/rootless certification and running-session adoption remain separate work." The RH-01 reports repeat this (`docs/reports/2026-09-15-rh01-235-gpu-runtime-acceptance.md:65`, `…rh01-239…:222`, `…rh01-240-integrated-acceptance.md:186`).

**Planning documents:**
- `docs/rh06/research/c-tracker-contracts-and-board.md:24-25`: "Docker API first behind a typed Quasar runtime interface; Podman and rootless are capability-tested profiles". `:35` excludes "universal rootless compatibility". `:251`: "#113 found rootless Podman needs `label=disable` + socket path; ownership design must not assume Docker-only".
- `docs/rh06/2026-09-24-decisions.md:411,426` defers Podman and rootless to RH-07. `docs/rh06/2026-09-24-architecture.md:144` and `docs/rh06/designs/3-rust-runtime-reuse.md:990` note that RH-07 then has one engine client, for both agent and actor.
- `docs/rh06/research/a-updater-and-self-update.md:713`: "Self-identification is Docker-layout specific (Podman … layout not verified)".
- `docs/reports/2026-09-05-first-install-compose-audit.md:163,210-213` covers socket endpoint drift, a fresh `/run` tmpfs, and "explicit rootless/non-root modes".
- The RH-02 plans put Podman out of scope (`docs/superpowers/plans/2026-09-17-rh02-probe-first-reconciliation.md:23`, `…2026-09-18-rh02-probe-first-spec.md:336`, which also says "Using CDI for injection" is out of scope).

**GitHub issues:**
- **#220:** "Detect capability differences in devices/CDI, networking, labels, mounts, user namespaces, events and lifecycle semantics… no silent privileged fallback."
- **#221:** test "UID/GID mapping, home ownership, supplementary groups, GPU/CDI, uinput, cgroups, Steam/Proton namespaces, audio and WebRTC"; "Do not equate GPU visibility with a functional rootless gaming session."

**Name collision.** In the control plane's library and storage code (`control-plane/internal/storage/storage.go:286`, `control-plane/internal/library/janitor.go:73`, `web/src/pages/setup/StepHosts.tsx:190,311`), "rootless" means **a host with no storage root**. It has nothing to do with rootless containers, and RH-07 terminology should avoid overloading it (for example in CONTEXT.md).

## Top 10 risks for rootless Podman GPU streaming (highest first)

1. **Input injection.** `/dev/uinput` write access, `mknod` of `/dev/input/eventN`, and `device_cgroup_rules c 13:*` (`virtual_input.rs:372`, `recipe/mod.rs:903-948`) are all root-only. Without them, sessions stream video but ignore input.
2. **NVIDIA injection.** DeviceRequests with `driver:"nvidia"` appear in the agent recipe, apps and helpers. Podman needs CDI (`nvidia.com/gpu=all`) and a rootless nvidia-container-toolkit setup. The "no GPU" answer is detected from Docker's error text (`engine/mod.rs:158`), and apps check the request is echoed back exactly (`application.rs:628`).
3. **Strict inspect read-back.** The checks on runtime `runc`, empty `UsernsMode`, exact `SecurityOpt` and device `rwm` (`application.rs:839-921`, `helpers.rs:729-751`) are likely to fail every app and helper launch on crun/Podman.
4. **Render-node access.** Numeric `--group-add` of host render/video gids (`container.rs:1637`) doesn't carry through a subgid mapping, so it needs `keep-groups`. Without it: no VA/Vulkan, a fall back to llvmpipe, and no hardware encode.
5. **The frozen seed interface hardcodes Docker paths.** `/var/run/docker.sock` is fixed inside the containers (`profile.rs:21`, ADR 0007), and every script, doc and printed command binds the host's `/var/run/docker.sock`. The node agent lacks `label=disable` while mounting the socket (`recipe/mod.rs:951`), so SELinux Podman hosts will refuse it.
6. **Daemon lifecycle semantics.** `unless-stopped` plus the seed's restart rules and `/update`-based policy flips assume dockerd. Podman needs `podman-restart.service` (which covers only `always`), lingering, and working compat `/update`.
7. **Healthcheck-gated install and replace.** If Podman's healthchecks never run (no systemd user session), `replace.rs:700-730` and `install_control.rs:318` wait until they time out.
8. **Host paths and ownership under subuid mapping.** This covers `/run/quasar-agent` on the host, Podman's refusal to auto-create bind sources, homes owned by PUID/PGID, the control plane's fixed uid 1000 chown, and the NVIDIA driver-volume host-path resolution through self-inspection.
9. **Network and kernel observability.** NET_ADMIN and SYSLOG in the rootless user namespace give no access to the host's nft rules or `/dev/kmsg` (`media_reachability` and `xid_visibility` degrade). The control plane's published ports go through rootlessport or pasta: source IP may be lost, the firewall now applies, and 443 needs `ip_unprivileged_port_start`.
10. **Steam/Proton sandboxing inside rootless.** `seccomp=unconfined`, the `apparmor=quasar-app` profile, `systempaths=unconfined` and the privileged exec (`application.rs:113-128`) must work inside a rootless user namespace, with bwrap/pressure-vessel then creating nested user namespaces. Container self-identification also falls back to the 12-character `$HOSTNAME` (`self_inspection.rs:28-49`).