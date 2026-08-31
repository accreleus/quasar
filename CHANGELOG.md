# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the version numbers are [semantic](https://semver.org/). `Unreleased` tracks
`develop`; each released version gets a dated section below it.

The published runtime images (`quasar-control-plane` and `quasar-node-agent` —
named `quasar-control` and `quasar-vulkan` through 0.1.0, alongside a third,
`quasar-nv`, retired after it) carry the release version as an immutable
`:X.Y.Z` tag. The shared
`quasar-images` base family is versioned separately, on a CalVer cadence of its
own; the two do not move together, and that is deliberate.

## Unreleased

### Security

- **A copy-pasted `make` line could run a second command (#550).** The Makefile
  interpolated caller-settable variables straight into recipe lines — `$(ARGS)` for
  about a dozen targets, plus `SID`, `DIR`, `RUN`, `NAME`, `URL`, `ROUTES`, `KEY`,
  `BEFORE`, `AFTER`, `OUT`, `HOST` and `MAKEFILE_LIST` (make-maintained, but a
  command-line assignment beats make's own). Make expands those into the recipe's command
  *text*, which `/bin/bash` then parses, so `make bench-run ARGS='--secs 5; whoami'` ran
  `whoami` at the make layer, before any script existed to validate it. The
  double-quoted ones were no safer: a `"` closes the quotes and a backtick is live
  inside them. It only escalates a shell the runner already has, but the realistic
  vector is a `make` line copied from a README, an issue comment or an agent transcript.

  No caller-settable variable reaches a recipe line any more. The knobs travel by
  environment — which no shell re-parses — and the receiving script turns them back
  into arguments with `dx_env_argv` (`scripts/dx/common.sh`), which splits on
  whitespace only (bash word splitting evaluates nothing) and then shape-checks each
  token, because several of these scripts forward a parsed value into a remote command
  where a metacharacter would be code again. `DX`, `CP` and `WEB` are `override`, so a
  command-line assignment cannot turn a repo constant into a command either. Every
  documented invocation still works unchanged.

- **A catalog image manifest could hand a tenant container host root.** `mounts` from an
  image manifest reached `docker run -v` verbatim: the control plane rejected only an
  exact `/var/run/docker.sock`, `/run/docker.sock` or `/`, and the node agent checked
  nothing at all, so `/var/run:/hostrun`, `/var/lib/docker:/hostdocker` or
  `/proc/1/root:/host` all installed cleanly and gave the app the host daemon. The image
  catalog is fetched unsigned from a mutable remote branch, so the manifest is not
  operator-authored input.

  The node agent now vets every wire-supplied mount before anything is spawned
  (`session/mount_policy.rs`), on the same "the wire is untrusted" footing as the
  container-network check: default-deny, with the managed-home root allowed read-write and
  everything else named by the operator in the new `QUASAR_APP_MOUNT_ALLOW` (read-only
  unless the entry ends `:rw`). A deny list beats the allowlist — `/`, `/proc`, `/sys`,
  `/dev`, `/etc`, `/root`, `/run`, `/var/run`, `/var/lib/docker` and the other runtime
  state dirs, any directory holding a container-runtime socket, and any `..` — so an
  operator typo cannot reopen the hole. The control plane applies the same deny list at
  image install **and** at admin preset writes, which previously had no mount check at all
  (`internal/mountpolicy`). Every shipped catalog image mounts nothing but its managed
  home, so the default library is unaffected; a deployment that relied on a manifest or
  preset binding another host path must now list it in `QUASAR_APP_MOUNT_ALLOW`.

- **`QUASAR_APP_PRIVILEGE_OPTOUT=deny`** lets a host ignore a manifest's
  `no_new_privileges: false` and `systempaths_unconfined: true`. It defaults to `allow`
  because the shipped Steam and KDE images need both; set it when running a catalog you do
  not author.

### Fixed

- **Installing a release no longer requires building it.** The documented quick start
  told self-hosters to run `deploy/redeploy.sh <profile> vX.Y.Z`, which compiles the web
  client and both runtime images from source — roughly 25 minutes and 25 GB of Docker
  disk to produce bytes that were already published to GHCR. `deploy/README.md` now
  leads with the pull-based install (fetch the tagged tree for its compose files, write
  `deploy/.env`, apply `docker-compose.release.yml` with digest-pinned
  `QUASAR_CONTROL_IMAGE` / `QUASAR_AGENT_IMAGE`), and the source build moves to a
  contributor section.

- **The release-artifact path could not bring a stack up at all.** Three defects, each
  independently fatal and all invisible on the build path, surfaced on the first virgin
  pull-only install (2026-08-27):
  - `docker-compose.release.yml` reset the control-plane's volume list to empty in order
    to drop a development bind mount, and took the persistent `quasar-control-tls` volume
    with it. The control plane exited at boot on `tls: create TLS dir ... permission
    denied`. The overlay now replaces the list rather than emptying it.
  - The base healthcheck shells out to `wget`, which the published control image (built
    from `Dockerfile.control.prod`) does not ship — it has `curl`. The container stayed
    `unhealthy` forever, so the node agent, which waits on `condition: service_healthy`,
    never started. The release overlay now carries the curl probe.
  - `Dockerfile.control.prod` runs as the non-root `quasar` user but never owned
    `/var/lib/quasar-control`, so a fresh Docker named volume mounted there was
    root-owned and unwritable. The image now creates and chowns the mount point.

  `v0.1.0` predates all three; `deploy/README.md` documents the two workarounds an
  install of that tag needs.

### Removed

- **There is one node-agent image now: `quasar-node-agent` runs on NVIDIA too.**
  The separate `quasar-nv` lineage is gone (#545) — no `nv` build target, no `nv`
  build role, no `quasar-nv` package published.

  It existed to carry one library. `libnvrtc` is CUDA *toolkit* userspace rather
  than driver userspace, so the driver-volume provisioner could not fetch it, and
  without it the four `cuda*` GStreamer elements never register and a session that
  falls back to NVENC cannot build a pipeline. Everything else NVIDIA needs — the
  whole NVENC encoder set included — already worked from the universal image.
  The agent now fetches that library from NVIDIA at launch, the same way it
  already fetches the graphics driver userspace, into the same volume.

  **Upgrading:** on an NVIDIA host, `deploy/docker-compose.nvidia.yml` selects
  `quasar-node-agent` and `deploy/redeploy.sh nvidia` builds it; if your
  `deploy/.env` pins `QUASAR_NODE_IMAGE=quasar-nv:latest` (or
  `QUASAR_PULSE_IMAGE`), change it. The first agent start after the upgrade
  downloads ~58 MB from `developer.download.nvidia.com` and may restart itself
  once to pick the libraries up. Set `QUASAR_CUDA_RUNTIME=0` to skip that
  entirely (the host then encodes on Vulkan only, which is the default anyway),
  or `QUASAR_CUDA_RUNTIME_DIR=<dir>` to stage the libraries from local disk on an
  air-gapped host. A host whose NVIDIA driver is older than r580 skips the fetch
  and says so. See [`docs/configuration.md`](docs/configuration.md).

### Changed

- **The platform container images are named for their role, not their
  implementation.** `quasar-control` → `quasar-control-plane`, `quasar-vulkan` →
  `quasar-node-agent`, and the build-time-only images `quasar-toolchain` →
  `quasar-gst-toolchain`, `quasar-dev` → `quasar-agent-dev`. `quasar-nv` was
  left out of the rename because that lineage was being retired rather than
  renamed — see Removed above. App and session images are unaffected.

  **Upgrading requires no action.** Both names are published for one transition
  window and resolve to the same digests, and a local `deploy/build-images.sh`
  writes the old name as an alias tag on the same image id
  (`--no-legacy-alias` opts out). Pin the new names when you next edit
  `deploy/.env`; the old names are dropped one release after the release that
  introduces the new ones. See [`docs/upgrading.md`](docs/upgrading.md).

## 0.1.0 — 2026-08-26

The first tagged release, and the first version a self-hoster can install by
name rather than by branch. Everything below already worked on `develop`; what
0.1.0 adds is a fixed tree, a version number to quote in a bug report, and
matching image tags on GHCR.

Install it with the quick start in [`README.md`](README.md):

```bash
bash deploy/redeploy.sh nvidia v0.1.0   # or: va v0.1.0
```

### Features

- **Browser cloud gaming over WebRTC.** A user registers, logs in, picks an app
  from the library, and plays it in the browser. Video, audio and input ride a
  direct WebRTC connection to the GPU host; only the API and signaling go
  through the control plane. There is no Moonlight/GameStream protocol.
- **Three video codecs — H.264, HEVC and AV1**, one per session, resolved
  server-side at launch from the profile's codec list, the host's encoder set,
  the client's decode probe, and that client's failure history, with H.264 as a
  guaranteed floor. Profiles ship H.264-only; an admin enables the others per
  profile.
- **Hardware encode on AMD, Intel and NVIDIA.** AMD/Intel encode through VA with
  DMABuf zero-copy. NVIDIA defaults to the Vulkan Video encoders for all three
  codecs, with the vendor NVENC elements as a per-session fallback and
  `QUASAR_ENCODER=nvenc` to restore the NVENC path wholesale.
- **Adaptive bitrate on by default** (`smooth` mode): encoder-aware and
  smoothness-biased, with a resolution/fps ladder on the Vulkan path. Client-side
  adaptive playout smooths presentation at the other end.
- **Multi-host scheduling.** Any number of GPU hosts register to one control
  plane; the scheduler places each session on a host with capacity, and supports
  per-GPU reservation, encode-slot and VRAM admission, host drain/cordon, and
  failover when a host goes offline.
- **App catalog with ready-made images** — Steam, a KDE desktop, XFCE, and a
  stream-diagnostics app — installed and configured from the admin console.
  Apps run in containers on `quasar-app`, with virtual keyboard, mouse and
  gamepad injected into a Wayland compositor.
- **Per-user persistent storage**: a managed home directory mounted into the game
  container, with single-writer guarantees and launch-time quotas.
- **Invite-gated registration and device binding.** Registration is closed by
  default and opens by invite; sessions are bound to a device token that can be
  revoked. Admin is a server-enforced role on the user, never a UI flag.
- **Two role-separated web areas in one SPA**: `/app` for players (library,
  search, account, live session) and `/admin` for operators (catalog, hosts and
  GPUs, sessions, users, invites).
- **Audio in both directions**: game audio to the browser, and optional
  microphone capture back to the session.

### Operations

- **One install command.** `deploy/redeploy.sh <va|nvidia> v0.1.0` syncs the ref,
  seeds `deploy/.env` with generated secrets and TLS cert hosts, builds the web
  SPA and the runtime images, brings the stack up, and verifies health. The
  virgin-deploy path is tested end to end on a clean host; roughly 25 minutes and
  25 GB of Docker disk for the first build.
- **Published images.** `ghcr.io/accreleus/quasar/{quasar-control,quasar-vulkan,quasar-nv}`
  are public and unauthenticated to pull, tagged `:0.1.0`, `:latest` and
  `:sha-<commit>`.
- **HTTPS out of the box.** The control plane serves a self-signed certificate by
  default with the host's LAN names baked in; an operator certificate or the
  Caddy-fronted hardened overlay are both supported.
- **Founding admin, two ways.** An interactive first-run wizard gated on a
  one-time setup token written to a file (never the log), or `BOOTSTRAP_ADMIN_*`
  environment variables for unattended installs.
- **Documented upgrade and rollback path** — [`docs/upgrading.md`](docs/upgrading.md)
  covers backing up Postgres, moving between versions, and the one-way migration
  rule (never run a control-plane binary older than its database).
- **Every knob documented.** [`docs/configuration.md`](docs/configuration.md)
  lists each environment variable with its default and accepted values.
- **Observability.** Per-session metrics, a session tracer, an always-on
  client-side glass-to-glass trace, admin host/GPU telemetry, and
  `make diagnose` for a one-page state dump or a sanitized shareable bundle.

### Known limitations

- **No relay ships with Quasar; a LAN or a VPN that acts like one is the
  supported shape.** Media is a direct browser-to-host connection, normally UDP,
  so the browser and the GPU host need a route to each other. Tailscale or an
  ordinary VPN both provide it. Once you run more than one GPU host a TURN relay
  starts to earn its place, because the scheduler may place a session on a host
  the client cannot reach — `QUASAR_ICE_SERVERS` accepts a STUN/TURN list you
  run yourself (coturn is the usual choice), and it is unset by default.
  Exposing the GPU host directly to the public internet is possible and is not
  recommended.
- **No per-session GPU routing.** A host's GPUs are reserved and scheduled
  against, but an operator cannot pin a session to a particular GPU or choose one
  from the admin console yet (tracked as #273).
- **HEVC decode is browser- and platform-dependent.** AV1 decodes everywhere
  Chrome does (dav1d) and H.264 decodes everywhere, but HEVC over WebRTC needs a
  browser and OS that support it — Chrome on Linux commonly does not, and will
  reject the video track outright. This is why profiles ship H.264-only and HEVC
  is an explicit per-profile opt-in. H.264 is negotiated down to
  constrained-baseline for browser receivers on every encoder vendor.
- **Docker Compose only.** The control-plane / node-agent split is
  Kubernetes-ready by design, but no manifests exist yet.
- **Linux GPU hosts only.** The node agent needs `network_mode: host`, which
  Docker Desktop on macOS and Windows cannot provide. This constrains the host,
  not the player: any modern browser on any OS can be the client.
- **Browser client only.** A native client (`photon`) is in design and spike; the
  browser is transport #1.
- Teardown-time `gst-wayland-display` EGL `0x3001` noise appears at ERROR level
  in the agent log and is harmless (#496 F6). It originates inside the vendored
  fork, which is not patched outside a deliberate, reviewed campaign.

### Late changes before the tag

The last batch merged to `develop` before this tag, for anyone who was already
tracking the branch.

#### Added
- Admin-wide persistent banner when the secret store's master key is unset,
  and a strongly-recommended `QUASAR_SECRET_KEY` block in `deploy/.env.example`
  (#522).
- Retryable `capacity_exhausted` responses with `Retry-After` and a client
  waiting state, instead of a dead-end error (#494).
- Access-log verbosity gets its own independent knob, defaulting to
  errors-only (#517).
- Boot now classifies a bad `DATABASE_URL` before blaming migrations (#518).

#### Changed
- Orphaned agent-plane session jobs are reclaimed on agent re-register and by
  a claim-timeout reaper, instead of leaking forever (#492).
- The launch screen distinguishes a real transport failure from ordinary
  scheduling delay, with stage-accurate copy (#482).
- Provider-image uninstall is refused while the provider is still enabled,
  closing a state where the catalog referenced a deleted image (#471).
- Catalog sync re-materializes managed presets when the runtime block changes
  at the same version, instead of silently drifting (#470).
- Admin Invites/Users pages render a real error state on load failure instead
  of an empty collection (#515).
- The provider stack gained an error boundary; admin surfaces show mapped
  error text instead of a raw error object (#521).
- A missing enrollment token now fails the node agent fast, and sustained
  registration failure turns host health unhealthy instead of looking idle
  (#519).

#### Fixed
- Virgin-deploy documentation polish: a DNS-name TLS hint, `--no-deps` on the
  cert re-issue recipe, and a nudge from image install to library discovery
  (#496 F2-F4).
- Two empty-string template env knobs (`QUASAR_TEMPLATE_SETTLE_SECS`,
  `QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS`) no longer warn "is not a number" when
  compose passes them through unset (#496 F5).
- The control plane's "agent connection closed" warning now notes that an
  abrupt (code 1006) close is expected right after an agent self-restart
  (driver-volume provisioning, GPU-fault recovery, or an admin restart
  command), rather than reading as an unexplained failure (#496 F8).
- `docs/configuration.md` drift: twelve undocumented env vars added, one dead
  knob removed (#516).

#### Documentation
- `SECURITY.md`, `CONTRIBUTING.md`, and `.github/ISSUE_TEMPLATE/` added for
  public-repo hygiene (#520).
- The network story is stated as LAN/VPN plus an operator-supplied relay for
  multi-host, on measured evidence (#509).
- An operator upgrade, backup and rollback guide, and a rollback error that
  explains its own cause (#514).
- The public quick start installs a release tag rather than `develop`, and the
  Images workflow mints matching `:X.Y.Z` image tags when dispatched on that
  tag (#510).
