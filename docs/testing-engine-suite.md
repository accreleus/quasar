# The engine-mode suite

One behavioural suite for the container runtime, run unchanged against every engine mode
Quasar supports: Docker and Podman, each rootful and rootless (RH-07 decision D1, #408).
"Supported" means one thing for all four because the same assertions pass on each.

It drives the runtime's external interface (`quasar_node_agent::runtime::RuntimeClient`
and the recovery actor's platform calls) against a real engine and checks what can be
observed: the engine's own report, what a container sees from the inside, and files and
device nodes on the host. It never reads private helpers or checks call sequences, and no
simulated engine stands in for a real one.

Source: `node-agent/tests/engine_suite/` (its own test harness, so a skipped case is
reported as a skip with its reason, never counted as a pass). The mode table is
`testdata/engine-suite/targets.json`.

## The cases

| Case | What it proves | Needs |
|---|---|---|
| `identity` | The socket answers as the engine and mode the target names. | |
| `create-read-back` | A session-shaped container (all capabilities dropped but the session's eight, no-new-privileges, `seccomp=unconfined`, a 64 MiB `/dev/shm`, a named volume) passes the runtime's engine-aware read-back, sees exactly that capability bounding set, and its volume outlives it. | |
| `user-mapping` | Who owns, on the host, a file the app user writes into a bind-mounted home (decision D14). | |
| `device-dri` | The host's DRM nodes, passed in with the groups the launcher grants, open for the app user. | `dri` |
| `device-uinput` | `/dev/uinput`, given as the recovery actor's recipe gives it to the node agent (a device of an agent-shaped service with `label=disable`), opens for the container's root (D9). Sessions never get `/dev/uinput`, only the event nodes the agent creates. | `uinput` |
| `cdi` | The engine injects NVIDIA through CDI (D10), and the app user opens what encode and render use: `/dev/nvidiactl`, `/dev/nvidia-uvm` and each `/dev/nvidiaN`. Other nodes a CDI specification lists may be withheld by host policy. | `cdi` |
| `restart` | An `unless-stopped` service comes back after it exits, and a running one that is stopped stays down. | |
| `stop-crash-loop` | A crash-looping `unless-stopped` service that is stopped stays down. Podman does not record a stop that finds it between two runs, so the runtime reads every stop back and makes it hold (#425). | |
| `health` | The engine runs a service's own healthcheck, to `healthy` and to `unhealthy`. | `health` |
| `removal` | A running service is removed; a volume in use refuses removal (`Busy`), outlives its container and is then removed; removing what is gone is not an error. | |
| `errors` | A missing image is refused with a named error on both lifecycles and leaves nothing behind. | |
| `missing-bind-source` | A typed bind whose source is missing is refused, the source is not created on the host, and nothing is left. | |

The app user is uid/gid 4321, chosen to be nobody's account, so a file's owner says which
mapping applied. The fixture image is a digest-pinned busybox (`sh`, `su`, `df`).

## Targets and what differs by mode

A target is an engine mode plus the socket to reach it. `targets.json` has one row per
engine and mode in `testdata/engine-profiles/profiles.json`, and the suite refuses to start
when the two disagree. A row carries the only thing that legitimately differs by mode,
`homeOwner`:

| Mode | A home file belongs to |
|---|---|
| `docker-rootful`, `podman-rootful` | the app's own PUID |
| `podman-rootless` | the engine's user, the Quasar user (`keep-id`) |
| `docker-rootless` | a subordinate id of the engine's user: rootless Docker has no per-container mapping, the known D14 gap |

**Adding an engine mode** is adding its profile rows, one row here, and a CI matrix entry or
a lab procedure below. It is never a new assertion. **Adding a case** is one function in
`cases.rs`, which every target then runs; a capability some hosts cannot give becomes a
`Capability` the case needs.

## Running it

```bash
make test-engines
```

It builds the suite in the devtools container, then runs the binary on this machine as the
invoking user, so bind-mount sources, device nodes and file owners are the host's. The
binary links only libc.

| Variable | Meaning |
|---|---|
| `QUASAR_ENGINE_SUITE_TARGETS` | `<mode>=<socket path>`, space- or comma-separated. Unset, `make test-engines` targets every standard socket this user can reach: `/var/run/docker.sock` (docker-rootful), `$XDG_RUNTIME_DIR/docker.sock` (docker-rootless), `/run/podman/podman.sock` (podman-rootful), `$XDG_RUNTIME_DIR/podman/podman.sock` (podman-rootless). The `identity` case fails a socket that is not the mode it was named as. |
| `QUASAR_ENGINE_SUITE_LACKS` | What this host cannot give: `;`-separated `[<mode>:]<capability>=<reason>`, capabilities `cdi`, `dri`, `uinput`, `health`. A declared gap turns the cases that need it into `SKIP` with the reason. An undeclared one fails them. |
| `QUASAR_ENGINE_SUITE_KNOWN` | Findings a mode is recorded as failing: `;`-separated `<mode>:<case>=<finding>` (a finding cannot contain `;`). The case still runs; a failure reports `KNOWN` with the finding and does not fail the run, and a pass does, so the entry is dropped once the finding is fixed. |
| `QUASAR_ENGINE_SUITE_IMAGE` | The fixture image, when the engine cannot pull the default from Docker Hub. Any image with `sh`, `su`, `df` and `grep`, by digest. |
| `QUASAR_ENGINE_SUITE_STATE_DIR` | Where the run's fixtures and runtime journals live (default: the system temp directory). It must be a path on the engine's host. |
| `QUASAR_ENGINE_SUITE_LOG` | The runtime's log filter (default `warn`): its warnings, on stderr, name what a refused read-back differed in. |
| `ENGINE_SUITE_BIN` | Run this binary instead of building one. |

Everything the suite creates is named `quasar-sess-suite-<run>-…` (applications) or
`quasar-engine-suite-<run>-…` (services and volumes), carries the `io.quasar.engine-suite`
label with the run id, and belongs to a fresh runtime ownership lease. Only those are
removed. A case that leaves anything behind fails, and anything still labelled with the run
at the end is removed and reported. If the runtime cannot prove an application's cleanup,
the run's journals are kept and their path printed.

The output is one line per target and case, then a total:

```text
engine-suite docker-rootful   create-read-back  PASS  session-shaped container read back; ... (0.3s)
engine-suite docker-rootful   cdi               SKIP  target lacks cdi: a hosted runner has no NVIDIA GPU (0.0s)
RESULT engine-suite: 7 passed, 3 skipped, 0 known, 0 failed (targets: docker-rootful)
```

Without `QUASAR_ENGINE_SUITE_TARGETS` the binary checks only its own tables, so the plain
`cargo test --workspace` in `make test-rust` stays hermetic.

## Where each mode runs

CI (`.github/workflows/ci.yml`, job `engines`, one job per mode on an Ubuntu 24.04 hosted
runner; `scripts/verify/ci-engine-target.sh` brings each engine up):

| Mode | CI | Lab |
|---|---|---|
| `docker-rootful` | yes: the runner's Docker | every GPU case, on each GPU test host |
| `docker-rootless` | yes: Docker's rootless helpers for the runner's own release, under the runner user's systemd session | every GPU case; Fedora with SELinux enforcing |
| `podman-rootful` | yes: Ubuntu's Podman 4.9, suite run as root | every GPU case; Fedora's Podman 5 |
| `podman-rootless` | yes: Ubuntu's Podman 4.9 through the user's `podman.socket` | every GPU case; the must-pass Fedora Atomic VM (SELinux enforcing, read-only `/usr`) |

CI's Podman jobs record one failure in `QUASAR_ENGINE_SUITE_KNOWN` (the job's `known`
matrix value) instead of hiding it:

- `restart`: Podman 4.9 cannot change a restart policy (`Engine`): its compatible API has
  no container update, and its native update has no restart policy. Podman 5.1 is the
  first that can, so it is Quasar's minimum (`engines.podman.minimumVersion` in the
  engine-profile table; #424). The case stays KNOWN here because the runner's Podman is
  below that minimum.

What only the lab can run, in every mode, and why:

- **`cdi`**: a hosted runner has no NVIDIA GPU.
- **`dri`**: a hosted runner has no DRM node.
- **`uinput`**: a hosted runner loads no `uinput` and has no host-preparation group rule for
  it; granting it by hand would test a different setup from the one Quasar ships.
- **Fedora and SELinux behaviour**: the runner is Ubuntu with AppArmor and Podman 4.9. The
  required profiles are Fedora (D5), so a green CI row is evidence for the mode, not for
  the profile.

A container without `/dev/net/tun` (such as an LXC development container) cannot run
rootless Docker at all: RootlessKit needs a tap device for the engine's network.

## Lab procedure

Run once per engine mode on each lab host, and record the output in the RH-07 acceptance
map (#409). Hosts are named here by role only.

1. **Build** on the development host: `make test-engines-build`. The binary is
   `.diagnostics/engine-suite/engine-suite`. Copy it to the lab host.
2. **Prepare the host** with `deploy/prepare-host.sh` if it is not already: the udev group
   rules, the NVIDIA CDI specification, subordinate ids and lingering are what the device,
   CDI and rootless cases exercise. Do not grant anything by hand that host preparation
   does not.
3. **Pick the state directory.** On an SELinux-enforcing host, point
   `QUASAR_ENGINE_SUITE_STATE_DIR` at a directory inside the homes root host preparation
   labelled, so the `user-mapping` bind mount is allowed without relabelling anything.
   Elsewhere the default is fine.
4. **Declare what the host lacks.** On the AMD test host:
   `QUASAR_ENGINE_SUITE_LACKS='cdi=no NVIDIA GPU on the AMD host'`. On the NVIDIA test host,
   nothing. The NVIDIA host's GPU can be held by another workload: if it is, record the
   NVIDIA row as pending. Never declare a gap to get a green run, and never infer an NVIDIA
   result from AMD.
5. **Run it as the engine's owner**, one mode at a time:

   | Mode | As | Target |
   |---|---|---|
   | `docker-rootful` | a member of the `docker` group | `docker-rootful=/var/run/docker.sock` |
   | `podman-rootful` | root (`sudo --preserve-env=QUASAR_ENGINE_SUITE_TARGETS,QUASAR_ENGINE_SUITE_LACKS,QUASAR_ENGINE_SUITE_KNOWN,QUASAR_ENGINE_SUITE_STATE_DIR`) | `podman-rootful=/run/podman/podman.sock` |
   | `docker-rootless` | the Quasar user | `docker-rootless=$XDG_RUNTIME_DIR/docker.sock` |
   | `podman-rootless` | the Quasar user, with `systemctl --user enable --now podman.socket` | `podman-rootless=$XDG_RUNTIME_DIR/podman/podman.sock` |

   ```bash
   QUASAR_ENGINE_SUITE_TARGETS=podman-rootless=$XDG_RUNTIME_DIR/podman/podman.sock \
     ./engine-suite
   ```

6. **Image.** A host that cannot reach Docker Hub sets `QUASAR_ENGINE_SUITE_IMAGE` to the
   same busybox in the test registry, by digest, never `:latest`.
7. **SELinux.** A state directory inside the labelled homes root is enough. On the
   Fedora Atomic VM (Podman rootless, SELinux enforcing), `user-mapping` passed with
   `QUASAR_ENGINE_SUITE_STATE_DIR=<homes root>/.engine-suite`, with nothing relabelled,
   and the home file belonged to the Quasar user. `device-dri` passed through the
   `video` group. A CDI-listed node the app does not need (`/dev/nvidia-uvm-tools`) could
   not even be listed there. That is host policy, and the `cdi` case does not ask for it.
8. **Record** every case line and the `RESULT` line. A `FAIL` is a finding about that engine
   mode: file it rather than retrying until it passes. A lab run sets no
   `QUASAR_ENGINE_SUITE_KNOWN`: CI's entries describe Ubuntu's Podman, not the lab's.
