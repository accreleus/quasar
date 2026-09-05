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

### Added

- **Quasar updates itself from the admin console (#104; #105–#119).** A *platform
  release* is a matched control-plane + node-agent image set from one commit. Every
  component now stamps its build identity (semver, source commit, build time; the control
  plane also its highest embedded migration) and the agent reports it on `register`
  together with its install mode (registry or source) and whether an **updater** sits on
  its stack (#107). The control plane detects releases on a weekly job (Monday 02:00 UTC,
  editable, run-now) — the **stable** channel reads GitHub Releases and their
  `platform-release-manifest.json`, the **edge** channel resolves the digest behind a
  branch tag (default `develop`) and reads the build identity off the image labels (#110,
  #111) — and the admin console gains a Fleet ▸ Releases tab plus a top banner: installed
  vs available, cumulative sanitised release notes, channel + edge-branch settings, and a
  per-host eligibility table whose ineligible rows carry the exact manual recipe (#112).
  Applying is a per-host **updater** sidecar (`quasar-updater`, in every compose stack and
  in `enroll-host.sh`'s generated stack) that only accepts digests under an allowlisted
  registry namespace (`QUASAR_UPDATER_ALLOWED_NAMESPACES`, default the org's), rewrites
  the two image lines in `.env` (keeping `.env.prev`), pulls, and recreates (#115). From
  the console an admin applies to one host — the host is cordoned, the apply waits for
  zero sessions (or `force` ends them), the new agent's `register` is the success
  evidence — or presses **Update Quasar** to move the control plane first (it recreates
  itself through its own updater and picks the run back up on boot) and then every
  eligible host in sequence, stopping at the first failure; an open admin tab gets a
  "Quasar was updated — reload" toast (#116, #117). A failed apply is left as it is with
  the previous digests recorded, and an agent can be **reverted** to them from the console;
  the control plane never is (ADR 0002: control plane first, never below the database's
  migration) (#118). `make release VERSION=x.y.z` cuts a release from the changelog and
  pushes the tag that publishes it (#109). Contract: quasar-protocol amendments 1 and 2
  (register identity fields, `/v1/admin/platform/*`, `release_apply`/`release_state`,
  migrations 0074 and 0075). **Existing installs must add the updater once** — see
  `docs/upgrading.md` "The updater". Glossary: `CONTEXT.md` "Platform releases";
  decisions: ADR 0001 (pinned-digest trust), ADR 0002.

- **Pushing a `vX.Y.Z` tag on `main` publishes a platform release (#104, #108).** The
  Images workflow gains a `v*` tag-push trigger beside manual dispatch. A `release-gate`
  job runs first and every build needs it: the tag must be strict semver, its commit must
  be reachable from `main`, and `CHANGELOG.md` must carry a non-empty section for that
  version — each refusal fails in seconds, before the ~85-minute node-agent build. After
  the existing build/validate/preflight/promote lane, a `release` job creates the GitHub
  Release with that section as its body (a prerelease tag makes a GitHub prerelease) and
  attaches `platform-release-manifest.json`: format version, release version, source
  commit, build time, the highest embedded control-plane migration, and the two component
  images by tag-free reference and sha256 digest. The workflow asserts the promoted
  `:X.Y.Z` tags resolve to exactly those digests before it publishes. Generator,
  validator, changelog extractor and their fixture tests live in `scripts/release/`
  (schema: `scripts/release/platform-release-manifest.md`). Branch pushes still build
  nothing and `develop` publishing stays manual. The first live tag-push run is still
  outstanding: it needs the workflow on `main`, then a prerelease tag.

- **Protocol amendment 1 for platform releases — identity and the release read surface
  (#104, #106).** `protocol/` is pinned to the `amend/platform-release-identity` branch of
  `quasar-protocol` (merge to its `main` is the operator's sign-off): `register` gains four
  optional identity fields (`source_commit`, `built_at`, `install_mode`, `updater_present`;
  replaced wholesale on every register, absent ⇒ unknown), the host body carries them, and
  two admin reads are specified — `GET /v1/admin/platform/identity` and
  `GET /v1/admin/platform/releases` (installed identities, available releases newest first
  by schema version, per-target eligibility with a closed reason vocabulary, faults). The
  channel and edge branch ride `/v1/admin/settings` as `release_channel` /
  `release_edge_branch`. `schema.md` documents the `hosts` identity columns, the
  `platform_releases` table and the two settings columns as provisional migration 0074.
  Both routes carry `x-unimplemented` in `openapi.yaml` and sit in the drift test's
  reviewed allowlist until #107/#110 register them.

### Changed

- **One hardened outbound HTTP client for the control plane (#105).** The SSRF
  containment that lived inside the registry digest resolver — HTTPS only, per-caller
  host allowlist, no redirects, DNS-rebind dial guard, bounded bodies, short timeouts —
  is now `internal/outbound`, constructed per caller with its own allowlist and timeout
  so the coming GitHub Releases client (#110) gets it by construction. The registry
  resolver and the template-context resolver use it; `QUASAR_IMAGE_REGISTRY_HOSTS`
  stays the registry's own knob. Two visible deltas. A registry token body over 1 MiB
  now fails with a named "body too large" error instead of being silently truncated
  into a JSON parse failure. And the allowlist is now enforced on every host actually
  contacted, so a Docker Hub ref needs `docker.io,registry-1.docker.io,auth.docker.io`
  in `QUASAR_IMAGE_REGISTRY_HOSTS` — an allowlist of `docker.io` alone used to let the
  manifest request through unchecked and now refuses it (`docs/configuration.md`).

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

