# First-install recovery implementation — review candidate

This implements the deployment changes following the [investigation](2026-09-05-first-install-investigation.md)
and [Compose audit](2026-09-05-first-install-compose-audit.md), for #131, #129 and #130.
It is a candidate for operator testing, not evidence that Stuart's deployment or
stream now works. No live deployment, streaming test, release publication or merge
was performed for this work.

## Changes and the failures they address

Stop numbers below refer to #131's original six-stop sequence.

| Independently reviewable change | Result | Original stops addressed |
| --- | --- | --- |
| Derive the site template from repository Compose and its NVIDIA overlay; gate site test/build on snapshot freshness | One complete generated file includes GPU, driver-volume, updater and storage wiring. Source drift fails the build. | 4; prevents recurrence of configuration drift underlying 1–3 |
| Resolve a stable release manifest, pin control/agent digests, verify matching updater availability, pull before startup | A fresh installer no longer guesses `latest`. Missing or incompatible releases fail before writing credentials or starting services. Copied files require explicit image pins. | 1 |
| Default control-plane state to its canonical named volume; add an optional ownership-preparation entrypoint | Normal installs use image-compatible ownership. Bind installs may start the wrapper as root to adopt/override the host UID/GID; it prepares state and private runtime ownership, drops privileges and execs Go as PID 1. Default image user stays non-root. | 2, 3 |
| Bake agent identity/runtime defaults into the image; establish loader paths before exec | Basic Compose no longer needs to carry those internal constants. NVIDIA driver libraries are visible to the dynamic loader from process startup. | 4 |
| Use a persistent kernel lock with legacy-marker compatibility; retry failed provisioning | A killed modern writer cannot wedge the next writer on a fresh stale marker. Download backoff and digest trust remain authoritative. Existing provisioning states become visible without reconnecting. | 5 |
| Identify the container from Docker identity-file mounts, inspect structured mount data, prefer named volumes and reject unresolved required injection | Host networking no longer causes an overlay digest to be mistaken for the container ID. Apps cannot silently start without a required provisioned driver mount. Transient inspection failures are retried. | 6 |
| Refresh readiness in the background and validate sibling-container paths and EGL loading | Wrong-but-existing home/runtime/template sources and broken driver loading become actionable host findings. UI updates while provisioning progresses. | Detects 4–6 early; covers additional failure paths |
| Blank copied credentials with explicit generation instructions; encode database credentials inside the control plane | `.env` contains neither usable default secrets nor shell expressions as values. Installer-generated secrets stay on the host and survive reruns. Special characters no longer corrupt a generated database URL. | Additional database-userinfo failure |

These changes are separable by area, but deployment of the new generator depends
on the compatible images. The site snapshot still intentionally applies install
policy transformations (required image pins, compact environment, access mode,
optional diagnostics). These transformations are tested; the snapshot gate does
not prove every future Compose semantic change is correctly transformed.

## Recovery boundaries

The agent retries operations it can safely perform: mount inspection, readiness,
driver provisioning under a lock, and the disposable sibling EGL probe. It does
not change host drivers, reconfigure Docker, relax digest trust or invent a path
when inspection fails. Missing permissions or mounts name the operator action.
Installer preflight groups host prerequisites before any host tuning and enforces
Compose 2.30. Device creation through the host's existing uinput module setup is
verified before the stack starts.

Nonempty performance defaults remain in the generated service definitions,
including `QUASAR_APP_SHM_SIZE=1g` with its override. This remains an app-container
launch setting, not an image-level filesystem optimization. Only fixed process
values moved into the agent image/entrypoint; omitted advanced environment values
can be set in separate optional `agent.env` and `control.env` files.

Control-plane bind preparation is opt-in (`user: "0:0"`). It refuses symlink
roots, does not cross nested filesystems, and migrates files owned by the previous
image UID 1000 under the state and private runtime directories. It does not repair
arbitrary ownership throughout the host. An inaccessible default non-root start
reports the directory and identity before starting Go.

## Additional silent-failure protections

- Host lib32 discovery checks ELF class and the loaded kernel-driver version,
  rather than accepting an arbitrary filename or a 64-bit library in `/usr/lib`.
  It uses the running agent image when available, avoiding an incidental helper
  pull on normal Docker installs.
- Audio sidecars default to the running agent's actual image identity instead of
  an assumed development tag. An explicit sidecar image remains supported.
- Driver injection checks image inspection before replacing `LD_LIBRARY_PATH`;
  inability to read the image's environment is no longer treated as an empty one.
- An unreadable driver digest store is an error, not an empty trust record.
- Home cleanup aborts if Docker or process mount liveness cannot be established.
  It no longer treats a failed inspection as proof that aged homes are unused.
- Required home/template/runtime mount failures also refuse the app launch while
  leaving administration available.
- Firewall probes report unknown unless a containerized agent can verify host
  networking. Missing host OS metadata no longer falls back to the image distro.
- Basic installs do not require optional `/dev/kmsg` diagnostics to exist.
- Own-certificate mode mounts certificate and key independently, rejects missing
  host files, and supports files stored in different directories.
- Generated credential files are private, created once without clobbering a
  concurrent install, and contain each image pin exactly once for self-update.

## Validation

Local checks cover configuration and failure behavior. They do not replace a
real GPU deployment or successful browser stream.

- `make test-go` and `make test-db`: pass. The latter ran against this worktree's
  fresh isolated Postgres, including database integration tests.
- `make test-rust`: pass (1,217 library tests, 6 binary tests and 3 integration tests);
  media/hardware-dependent acceptance remains unrun.
- `make test-web`: pass (2,906 tests), including type checking, API drift and production build.
  One existing AppHomeNext test exceeded its 5-second timeout while image builds
  ran concurrently; the full rerun after those builds passed with unchanged tests.
  The new checklist was visually inspected using a local mock setup page and the
  existing design tokens/components.
- Site tests: 26 pass, including actual Docker Compose parsing for six GPU/access
  combinations, Bash syntax for 30 installer combinations, credentials created
  with mode 0600 and preserved on rerun, release filtering, and Compose version
  rejection. Site build: 215 pages pass.
- `make config-check`: 10 pass, 3 advisory warnings, no failures.
- `make verify`: 355 pass, 2 warnings, **14 failures also present at baseline**,
  caused by the absent `.claude/skills/quasar-session/scripts/qses` harness.
  This gate is not green and must be restored before merge acceptance.
- Canonical image builds validate the unchanged image contract. Control and agent
  contracts contain 23 and 139 assertions respectively in this checkout.
  Builds use unique local tags, `--no-latest --no-legacy-alias --no-prune`; no images
  are published or shared tags moved. The new `--no-prune` option preserves other
  work's images during this local validation; existing builder defaults remain.
- The entrypoint harness checks default non-root startup, 99:100 bind ownership,
  migration of old state and private token ownership, PID 1 behavior, and an
  inaccessible-state rejection using disposable local containers. Root aliases
  (`0`, `00`, `000`) and malformed UIDs are rejected before privilege drop.

Local image evidence: `quasar-control-plane:20260905-1645-first-install-131`
(source `49804c43d45a`) passed 23/23, and
`quasar-node-agent:20260905-1634-first-install-131` (source `2ced17e28d1f`)
passed 139/139. Later commits after the agent build change installation prose
and the control entrypoint only; the tested agent implementation is unchanged.
These are local validation tags, not published installation recommendations.

## Publication and operator validation still required

1. Review the branch and restore the missing verification harness. Publish the
   site only after a stable release containing the new image entrypoints and
   structured database configuration is available. The installer deliberately
   rejects older stable images; it does not choose the self-update agent's
   `0.2.0-rc.1` experiment or mutate that release.
2. Test a fresh Unraid NVIDIA install and a standard host-networked Docker NVIDIA
   install. Verify the generated file, named control state, driver readiness,
   killed-provisioner recovery, and a working Steam stream with 1 GiB shared memory.
   Also test AMD/Intel configuration and an explicitly chosen bind-storage UID.
3. Exercise temporary Docker-inspection failure and recovery without reinstalling
   a driver; inject wrong-but-existing mount sources and confirm the readiness
   message identifies the mismatch.
4. Verify video, sound, input and clean stop in a real browser session. The sibling
   probe only confirms driver loading using the agent image. It does not prove
   every custom app renderer, encoder, network route or client works.

The guided setup checklist is deliberately an operator confirmation, not an
automatic certification. A targeted “Test this host” session cannot be guaranteed
through the existing public launch request: it has no host selector
(`control-plane/internal/session/handler.go`, `createSessionRequest`). A dedicated
diagnostic-app workflow with host targeting and durable video/audio/input evidence
needs a separately approved API design. Frozen `protocol/` interfaces are unchanged.

Other recommendations are only partially covered here: the runtime now shares
vendor auto-detection, but a single durable GPU identity across mixed-vendor/CDI
paths still needs hardware-backed design and validation. Provisioning reuses the
existing progress/status fields and backoff; this change adds neither a new
“Retry now” API nor a new persisted progress contract. Per-app renderer/UID/socket
certification, backend-specific template persistence/reflink handling, and full
host-view validation for FUSE/AppArmor remain follow-up work. The supported
single-file Compose includes those host views; arbitrary remapped deployments
are not certified by this candidate.
