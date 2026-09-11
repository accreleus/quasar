# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the version numbers are [semantic](https://semver.org/). `Unreleased` tracks
`develop`; each released version gets a dated section below it.

**Each released version needs a `## X.Y.Z — YYYY-MM-DD` section with a non-empty
body, and it must exist on `main` before the tag is pushed.** The Images workflow
publishes that section verbatim as the GitHub Release notes and **refuses a tag
whose section is missing or empty** — before any image is built, so the fix is to
add the section and re-tag, not to wait out an 85-minute build. A prerelease tag
needs its own section under its full version (`## 0.2.0-rc.1 — 2026-09-04`): a
`## 0.2.0` heading does not satisfy `v0.2.0-rc.1`. `scripts/release/changelog-section.sh <version>`
prints exactly what the release will carry.

The published runtime images (`quasar-control-plane` and `quasar-node-agent` —
named `quasar-control` and `quasar-vulkan` through 0.1.0, alongside a third,
`quasar-nv`, retired after it) carry the release version as an immutable
`:X.Y.Z` tag. The shared
`quasar-images` base family is versioned separately, on a CalVer cadence of its
own; the two do not move together, and that is deliberate.

## Unreleased

### Added
- **Quasar can install its own updates** (#122, migration 0081). Settings ▸ Platform updates
  ▸ "Install updates automatically", off by default. When it is on, a detected release is
  applied without a click — the control plane first, then every eligible host, through
  exactly the fleet run the Update button starts. **A release that changes the database is
  never installed this way**: that is the one case where the control-plane step still empties
  the instance first, so it waits for a person. Everything else rides through with sessions
  still streaming (#128/#153), which is what makes this safe to leave on. There is no second
  schedule to configure — an automatic update happens when release detection next runs, so
  that job's own schedule is the window. An automatic run is **never forced**: "Update now"
  is an operator agreeing to end live sessions, and there is no operator. A failed automatic
  run stops that **release**, not the feature — a newer one is still installed, and applying
  the failed one by hand clears the block, so one flaky host cannot end automatic updates for
  an instance. What a pass did, or why it did nothing, is in the detection job's run summary,
  and a run started this way is marked in Fleet ▸ Releases. Contract: `quasar-protocol`
  amendment 8.

- A **beta release channel** (#121). Admin ▸ Fleet ▸ Releases now offers a third
  channel between stable and edge: beta lists the same tagged releases stable
  does **and the prereleases among them**, so a release candidate can be applied
  from the console through exactly the path a stable release takes. Beta stores
  no releases of its own — a prerelease was already detected and cached, stable
  simply hides it — so switching to or from it re-detects nothing and writes
  nothing (migration 0079 widens one `CHECK`). Two rules come with it. Ordering
  on beta is **SemVer precedence**, not publication order, because an rc cut from
  `develop` and a patch cut from `main` arrive out of version order
  (`0.2.0-rc.2` < `0.2.0` < `0.2.1-rc.1`; `0.3.0-rc.9` < `0.3.0-rc.10`). And
  **leaving beta never rolls an instance back**: a release whose version orders
  below an installed prerelease at the same schema version is not offered on any
  channel, so an instance on `0.3.0-rc.1` that switches to stable waits, showing
  `no_release`, until `0.3.0` ships. Contract: `control-api.md` §Platform-release
  beta channel (amendment 3). Operator guide: `docs/upgrading.md` "Release
  channels".
- Quasar can now tell you a release is available without you looking at the
  console (#123, migration 0080). **Fleet ▸ Releases ▸ Notifications** takes a
  webhook URL and POSTs one message when the detector finds a release this
  instance could move to; the body carries `text` and `content` alongside the
  structured fields, so a Slack, Discord or ntfy incoming webhook renders it
  with no adapter in between. A **Send test** button exercises the URL before
  you switch it on, and records nothing, so it can never use up the one
  notification a real release gets. An optional signing secret
  (**Secrets → Release notification signing secret**, or
  `QUASAR_PLATFORM_RELEASE_WEBHOOK_SECRET`) adds an HMAC-SHA256 signature over
  a timestamped body for a receiver you wrote yourself; Slack, Discord and ntfy
  authenticate by URL and need none. The same release is never announced twice,
  a fresh install announces at most the one release it could take rather than
  its whole back catalogue, and a refused webhook is a line in the detection
  job's run summary — it never fails detection or holds back the banner.
  Delivery is `https` only, follows no redirect and refuses any host resolving
  to a loopback, private or link-local address, so it cannot become a probe of
  your own network; a LAN receiver therefore needs a public https endpoint in
  front of it. `docs/upgrading.md` "Release notifications".
- Platform releases can be signed, and the updater can verify the signature
  (#120). A release may now publish a second asset,
  `platform-release-manifest.json.sig`: a detached ed25519 signature over the
  release manifest's exact bytes, which covers every component image through the
  digests the manifest already names. Hosts check it as a second gate beside the
  registry-namespace allowlist, fetching the manifest and signature themselves so
  a signature can never be supplied by the same party as the digests, and
  refusing an apply whose signed manifest does not name the digests being asked
  for. **Both halves are off by default and nothing changes for an existing
  install**: publishing signs only once the `QUASAR_RELEASE_SIGNING_KEY` secret
  exists, and hosts verify only once `QUASAR_UPDATER_SIGNATURE_MODE` is set to
  `verify` or `require`. `verify` refuses a bad signature but accepts an unsigned
  release, so a fleet can be configured before the first signed release exists;
  `require` closes it. Several trusted keys at once make a key rotation a period
  rather than a flag day. Operator procedure, including the CI secret to create
  and how to rotate: `docs/upgrading.md` "Signing platform releases"; knobs in
  `docs/configuration.md`; decision record in `docs/adr/0003-release-signatures.md`.

### Changed
- A fleet update no longer empties the whole instance before it updates the control
  plane (#153). That drain existed because a control-plane restart used to end every
  session; #128 removed that, so a release carrying no database migration now takes the
  control-plane step with sessions still streaming through it, and only each host's own
  sessions end as that host is updated. A release that **does** carry a migration still
  drains the fleet first — the held session's row is read back by a binary that has just
  migrated the database under it, and no migration was ever written to survive that. The
  confirmation says which of the two you are about to do — and it reads a `migrates` flag
  the server now serves, rather than working it out itself — and the fleet is still cordoned
  for the whole run either way. "Update now" on a migrating release now **stops** the
  instance's sessions and waits for them to be gone, instead of skipping a wait that since
  #128 nothing else would have satisfied. Even a non-migrating step gives a launch already
  in flight a moment to land, because that is the one session a restart still loses. "Update now" on a migrating release now **stops** the
  instance's sessions and waits for them to be gone, instead of skipping a wait that since
  #128 nothing else would have satisfied. Even a non-migrating step gives a launch already
  in flight a moment to land, because that is the one session a restart still loses.
  Contract: `quasar-protocol` amendment 6.
- **The audit log names the things it is talking about.** Every row served by
  `GET /v1/admin/activity` now carries a `names` map — id to display name — covering both
  the row's target and the identifiers inside `details`, so a `session.launched` entry that
  used to read `session 85d0b6a9` over `{"app_id": "8b1116c8-…", "host_id": "4daeaa27-…"}`
  now says *Steam* and *gpu-test*. The ids are unchanged and still shown in full in the
  expanded readout and the CSV: an id is what you paste into a query, a name is what tells
  you what you are looking at, and the log now carries both. Resolved at read time, like
  `actor_username`, so a rename shows the current name; where the entity has been deleted
  the name the emitter stamped at write time is served instead, which is what lets a
  `user.deleted` or `app.delete` row still say *whose* account or *which* app. Deleting a
  user now records the username for exactly that reason. On the page: the Target column
  shows the name, the Detail column shows the mock's `key=value` summary
  (`app=Steam host=gpu-test`) instead of a repeat of the action, the expanded pane opens
  with a plain-English sentence, and the action labels behind it were rebuilt from the
  actions the server actually emits — nine of the old ones named actions that no longer
  exist. CSV export gains a `target_name` column beside `target_id`.
  Contract: `quasar-protocol` "Audit-log names" amendment (additive, no migration).

### Fixed
- **A busy Docker host no longer makes the agent report its own container runtime as
  unresponsive** (#194). Every container-runtime command the agent runs had its output
  read only after the child exited, so a command printing more than one pipe buffer's
  worth of output blocked in `write(2)`, never exited, and was killed at the 30 s deadline
  with "container runtime unresponsive" — blaming a daemon that was answering that same
  command in hundredths of a second. The buffer is 8 KiB rather than 64 KiB on a host whose
  root uid has exhausted its pipe-page quota, which dozens of running containers will do,
  and a bare `docker image inspect`'s JSON clears 8 KiB: an external reporter's agent burnt
  30 s on every reconnect failing to reconcile one catalog image, and before #191 that cost
  it its registration. Both pipes are now drained while the command runs — the capture cap
  discards the excess instead of stalling the writer — and the deadline stays hard even when
  a process that inherited the pipe outlives the command it came from.
- **A reconnecting agent no longer replays every updater result it has ever seen** (#193).
  On each reconnect the node agent re-emitted a `release_state` for every result file in
  the updater's results directory, including the control plane's own steps (written to
  the same directory, never applied by the agent), and the control plane answered each of
  those with a "names another host's attempt" warning — five per reconnect on a stack
  with a few fleet runs behind it, drowning the warning that check exists to give. The
  replay stays, narrowed to this agent's own results that are still live or finished
  within the last two hours; a non-terminal result is always replayed, and a result whose
  age cannot be read is kept rather than dropped. Nothing is deleted.
- **An agent on a host whose Docker daemon answers slowly can register again** (#191). The
  agent opened its WebSocket to the control plane first and only then ran the two
  container-runtime probes `register` needs (the image reconcile and the install-mode probe,
  each `docker inspect` bounded at 30 s). The control plane gives a fresh connection 15 s to
  send `register`, so on such a host it closed the socket before `register` was written, the
  agent logged "connection reset without closing handshake", reconnected, repeated the same
  probes, and never came back. Found live by an external reporter straight after a successful
  control-plane update; the host showed as down with the agent container running. The probes
  now run before the socket is opened, the agent warns (`register-prep-slow`) when they took
  more than 10 s, and the control plane's log names the handshake timeout in words instead of
  a bare `i/o timeout`. The cost: the probes now run on every dial attempt, including while
  the control plane is down, so a reconnect loop on a slow-runtime host is slower than
  before rather than impossible.

- `docs/upgrading.md` "Adding it to an existing install" no longer leaves the control plane
  without the updater's socket. Step 3 brought up only the updater; the compose file also
  mounts its socket volume into the control plane and the node agent, and a container
  created before the volume existed keeps running without the mount, so the console said
  the updater was not installed for the control plane while the agent reported it present.
  The step now recreates all three. The same page gains a "before applying" check for the
  agent's health port: since #152 an updated agent refuses to start when `127.0.0.1:9091`
  is already taken on the host, which an older agent tolerated, so a release apply is the
  first place that shows.
- **`redeploy.sh` no longer reports a deploy healthy on evidence it never saw (#177).**
  Host readiness and the codec plan were initialised to `ok` the moment the node-agent
  log came back non-empty, *before* anything looked for a verdict — so a log carrying no
  readiness verdict at all summarised as a confident `result=OK`, and so did a log with
  no agent logs to read. Worse, the verdict the agent emits mid-provision (`no failures;
  N check(s) are being remediated automatically and are not usable yet`) was missing from
  the classifier entirely, which is the commonest first-boot redeploy there is. Absence
  of evidence is now its own state: the summary reports `readiness=unverified` /
  `codecs=unverified` and downgrades the result to `WARN`, the mid-provision verdict is
  classified and reported as `PROVISIONING`, and only the agent's own all-clear earns an
  `ok`. The verdict is polled for on the same bounded 30s budget the registration check
  already uses, over a deeper log tail, so a verdict that simply had not landed yet is
  not mistaken for one that never will.
- **A migrating fleet update can no longer run its migration on a session count it
  never read (#175).** `FleetNonTerminalSessions` answers a failed read with
  `(0, error)`, and the wait before the control-plane step was written against the
  count, so a database error at the wrong moment read exactly like "the fleet has
  drained". Three places could take it: the first count returned `true` outright
  ("the count is advisory"), the recount after a forced drain assigned the zero
  before the error was looked at, and the drain poll did the same and then re-tested
  its own loop condition against it. Past that wait the database is migrated, and
  every migration in this repo was authored assuming no session was live. An
  unreadable count is now held distinct from zero, only a read that *succeeded* and
  said zero lets the step proceed, and a store that never answers ends the attempt
  as `timeout` within the existing deadline rather than as a migration over live
  sessions. A transient failure still costs a healthy run nothing.
- **The install page's compose template is no longer stale.** `deploy/docker-compose.yml`
  gained the release-webhook, agent health-address and updater signature knobs without
  `npm run compose:sync` being re-run, so the quick-start page handed operators a compose
  file missing knobs the running stack expects — and the `site` CI job failed on every
  pull request against `develop`, which is what surfaced it.
- **`make up` no longer crash-loops a fresh local stack.** The local dev overlay defaulted
  `BOOTSTRAP_ADMIN_PASSWORD` to `local-dev-admin` against username `admin`, and the
  password policy refuses a password containing its own username, so the control plane
  died at boot on every new worktree. Contributor-facing only; no released image was
  affected.
- **A desktop app created in the admin console now launches** (#171, migration 0082).
  Every app made in the console against a managed desktop preset (KDE, XFCE) failed in
  seconds with `app_exited_early`; the app container's own log said
  `software Vulkan renderer detected`. The image's launch requirements -- `gpu`,
  `no_new_privileges`, `systempaths_unconfined` -- live on the image catalog entry and
  reached an app row only when the library provider created the app; the managed preset
  deliberately carries none of them, and the console's editor wrote `gpu: false` for
  every new app while believing the flag inert. It is not: the agent passes the GPU into
  the container only when it is true. The control plane now stamps the managed image's
  declared values onto the app on every create or edit that touches its spec or preset,
  and migration 0082 applies the same rule to existing rows. Hand-made presets,
  preset-less apps and provider-created apps (whose values the provider copied at install)
  store exactly what they are sent, as before. The `deploy/README.md` paragraph telling
  operators to copy the three values by hand is gone.
- A `session.failed` audit entry now names its `app_id` (#171). The row carried the host,
  the failure code and the state detail but not the app, so a failed launch could not be
  matched to its app from the audit feed; `session.launched` already carried it. Additive.
  Contract: `quasar-protocol` "session.failed app_id" amendment (additive, no migration).
  The audit page's name resolution already covers `details.app_id`, so the row shows the
  app's name beside it.

- A fleet update no longer fails because a host was offline (#169, #170). Found by a
  real update on hardware: a live 0.2.3 -> 0.2.5 fleet apply failed at its first host
  and stopped, leaving the rest of the fleet unattempted -- against the sequencer's own
  rule that an ineligible host is skipped, not failed. The cause is that `hosts.status`
  is never corrected across a control-plane restart: the row is marked offline only from
  the agent connection's own goroutine, so a control plane that exits never marks
  anything, and the stale sweep only visits hosts with active sessions. Since every fleet
  run restarts the control plane, "the row says online but no agent is there" is the
  normal shape of a run rather than an edge case. Eligibility now reads whether the agent
  is actually connected, not just the stored status, so such a host is skipped with
  `host_offline` and the run continues. A separate defect fixed alongside it (#170): the
  cordon restore treated every not-online host as one this run had cordoned, so a host
  that was already offline before the run could be un-cordoned by it.

- The bench harness no longer reports a healthy stream as black. The peer's luma probe
  judges a 160x90 canvas with thresholds calibrated on full-frame content, and
  `Quasar Bench: Ball` is a 20px-radius ball — about 0.06% of a 1080p frame — so a
  perfectly good Ball stream read `mean=3.5 sd=0.00 "first content never"`, which is
  indistinguishable from a black picture. Because Ball is the default bench app on more
  than one host, the same false reading appeared on both the AMD/VA and the 5090/Vulkan
  paths and looked like a confirmed cross-platform rendering defect; it is what left
  #128's live gate recording rendering as unproven. Documented in
  `docs/testing-bench-mode.md`, with the probe's own calibration comment corrected: judge
  rendering from a full-frame app (Snow, Colour Ripple), and never from Ball.



- **Intel hosts can register a Vulkan encoder at all** (#126). Mesa's Intel Vulkan
  driver hides the whole Vulkan Video extension family behind an opt-in instance
  debug flag. Unset, the device does not advertise `VK_KHR_video_queue`, every other
  video extension depends on that one, and GStreamer therefore registered no vulkan
  video element while `vulkansink` still appeared — so the host looked like a working
  GPU whose encoder supported nothing, which is exactly what the `encoder_codecs`
  readiness check reported. The image now bakes in `ANV_DEBUG=video-encode` (inert on
  AMD and NVIDIA, since no other driver reads it), so a `docker exec … gst-inspect-1.0`
  agrees with the running agent instead of contradicting it, and the agent reconciles
  that variable against the new `QUASAR_INTEL_VULKAN_VIDEO` knob at startup.
  **This only changes anything on a host whose encoder is `vulkan`.** Intel's vendor
  default is still VA, so an Intel operator wanting the Vulkan path has to set
  `QUASAR_ENCODER=vulkan` as well, and will also want `QUASAR_VULKAN_AV1=0`: ANV has
  no AV1 encode, so leaving that knob on makes every boot log a
  `vulkan-codec-plan-degraded` warning pointing at the image contract, which is the
  wrong place to look on an Intel host. Which Intel parts actually expose a usable
  encode queue is not established: the pinned Mesa gates the encode extensions on the
  flag and on the driver's codec build, not on a generation, and nobody on the project
  has the hardware. Gen12 integrated graphics is the expected target; DG2/Arc is
  untested. Mesa ships this off by default and does not treat the path as validated,
  which is what this release is asking Intel users to try.

- **Intel hosts now ship a VA driver** (#126). `mesa-va-drivers` is gallium only
  (radeonsi/nouveau/virtio/d3d12), so libva had nothing to load on an Intel GPU:
  `vaInitialize` failed, `vah264lpenc` never registered, and the agent's startup
  codec probe reported an empty set — on the path that is the *documented default*
  for Intel. The images now carry `intel-media-driver` (iHD, Gen9+) from RPM Fusion
  nonfree, plus `libva-utils` so `vainfo` is available inside the agent container.
  Fedora's in-distro build was measured and rejected: both packages are MIT and BSD,
  Fedora's source RPM is named `intel-media-driver-free`, and its build is 11.6 MB
  with a quarter of the AVC/HEVC encode symbol references. That is the same patent
  split already accepted for AMD via `mesa-va-drivers-freeworld`, on identically
  licensed code.

- A node agent no longer reports another agent's health as its own (#152). The stack
  uses host networking, so two agents on one machine share `QUASAR_HEALTH_ADDR`; the
  loser of that bind kept running while its container `HEALTHCHECK` — and any operator
  probing by hand — was answered by the winner. In the field this reported a perfectly
  healthy agent as unhealthy for sixteen hours, with a different process's failure
  reason attached, and the log was the only thing that disagreed. The agent now refuses
  to start if it cannot bind the address, `/health` identifies the answering agent by
  `node` and `pid`, the image's `HEALTHCHECK` follows the configured address instead of
  hardcoding the default, and the multi-agent overlay gives each extra agent its own.
- An expired session now signs you out and returns you to the sign-in form, saying
  so, instead of leaving your library on screen behind a red banner whose "Try
  again" could not work (#154). The SPA handled a rejected token in exactly one
  place — the check it makes when a page first loads — so a token that expired
  while a tab sat open, or one an admin revoked, surfaced as an ordinary "could not
  load" error over data that was already on screen. A 401 on any authenticated
  request now ends the session everywhere: local credentials are cleared, so a
  reload cannot resurrect them, and the signed-in page is unmounted rather than
  left rendering what the dead token had fetched.

- Sessions now survive a control-plane restart (#128), confirmed on a live 73 s
  outage with a real browser peer: decode continued at 60 fps with no dropped samples
  and the session stayed `running` (`docs/reports/2026-09-08-128-session-survival-gate/`). The browser treated any
  signalling-socket close as a session failure and answered it by minting new
  coordinates and rebuilding its transport — which destroyed the peer connection
  that was still carrying the stream, so a session that had survived the outage
  was killed by its own recovery. Signalling health and media health are now
  tracked separately: while media is still flowing the client re-attaches
  signalling **in place**, keeping the peer connections, the input channel and
  telemetry untouched. Only a dead media path rebuilds the transport. A refused
  token (4401), an ended session (4404) and a takeover (4410) stay terminal.
- Encoder certification no longer caps a session with a measurement taken under a
  different GPU driver. A certification records `encode_ms` for one silicon + driver +
  encode-stack combination, but nothing on the wire carried that combination, so an old
  performance cap stayed applicable for its full week after a driver change. Agents now
  report a per-GPU driver identity (NVIDIA kernel-module version, else the Vulkan
  driver properties, which cover RADV/AMDVLK/ANV), the control plane stamps it onto each
  certification row, and the launch path skips rows carrying a different one. A host
  that reports no identity, and every certification measured before this shipped, stay
  applicable exactly as before — dropping those caps would start sessions at rungs the
  host may not sustain (#144, migration 0078).
- The agent-side and control-plane-side groundwork for the above (#128). The agent now
  holds its sessions for a bounded grace window instead of stopping them when its
  websocket drops, and the control plane reconciles against the agent's own
  `heartbeat.running_sessions` on reconnect rather than assuming none survived,
  with a stale-host sweep as the backstop. Both were confirmed on a live 72 s
  outage. **This is not yet end to end**: the browser still re-seats its
  signalling coordinates after the outage, which tears down the peer connection
  that was still carrying media, so the session ends anyway. The user-visible fix
  lands when that is resolved. Knobs `QUASAR_SESSION_GRACE_SECS` on the control
  plane (120 s) and the agent (90 s). Also closes a pre-existing hole where a host
  that never came back after a control-plane restart kept its sessions
  non-terminal and its status online forever, still attracting placements.
- Encoder certification no longer caps a session using a measurement taken under
  a different encoder. The certification table is keyed on the encoder, but the
  batch read the launch path uses did not filter on it and the ranking compared
  only rung, bitrate and age — so a row measured under NVENC could cap a Vulkan
  session on the same rung, in either direction. `vulkanh265enc` and
  `nvcudah265enc` are different silicon paths and their encode times do not
  transfer. A host that has not reported an encoder still uses every row, since
  dropping the cap outright would launch at a rung the host may not sustain.
  Driver identity is a separate follow-up (#144).

## 0.2.5 — 2026-09-07

### Fixed

- `deploy/redeploy.sh`'s header no longer claims that running sessions survive a
  control-plane-only deploy. They do not, and have not: recreating the control
  plane ends every session on the host (#128). Drain first if the sessions
  matter; the fleet self-update run already does.
- Pull-request CI now builds and tests `site/`, which generates the quick-start
  compose file, `.env` and install script. Those tests ran only in the manually
  dispatched docs workflow, so a regression in the operator-facing installer
  could reach `main` without anything failing.
- `make test-db` now works on a host with no Go toolchain outside a container,
  which is every fleet host. It selects a containerised runner when `go` is
  absent (or when `TESTDB_CONTAINERISED=1` forces it), reaching the ephemeral
  Postgres by container name on a private network instead of the published
  loopback port. The target previously refused to run at all with
  `FAIL go — not on PATH`, so the one gate that proves a DB-touching
  control-plane change was unavailable exactly where changes get validated (#125).
- Publishing a home template no longer deletes the version it supersedes while
  readers may still be inside it. The symlink swap was already atomic, but the
  previous versioned directory was reclaimed immediately after it, so a reader
  mid-path-resolution could see the template as absent and an in-flight clone
  could have its source removed underneath it. The superseded version is now
  spared for one publish generation and reclaimed by the next publish, which
  keeps `.versions/` bounded at two per image. Reclamation is decided by
  reachability rather than by name, so an image id that is a prefix of another
  cannot collect its neighbour's versions (#150).
- The quick-start installer writes the stack to an absolute path derived from the
  base path you give it, instead of a `deploy/` directory beside wherever the
  script was run. On Unraid the root shell starts on a ramdisk, so the previous
  behavior lost `docker-compose.yml` and the only copy of `POSTGRES_PASSWORD`,
  `QUASAR_SECRET_KEY` and the enrollment token at the next reboot, silently: the
  containers restart from Docker's own state and look healthy until the next
  compose command or upgrade. `QUASAR_STACK_DIR` records that absolute path, so
  the updater keeps resolving it, and the stack directory is created `0700`.

  The installer also now refuses to run on a host that already has a stack
  deployed from somewhere else. Compose takes its project name from the stack
  directory's name, which is `deploy` in both the old and new layouts, so
  starting a second stack would have recreated the existing one's containers
  with freshly generated credentials against its existing database volume --
  which keeps the original password. Installing gained a "Moving an existing
  stack" recipe for the case where the old directory still exists, and a by-hand
  recovery recipe for the case where a reboot already took it (#148).
- Intel GPUs are no longer dropped from a host's capacity inventory. Capacity
  detection required a dedicated-VRAM reading that only AMD and NVIDIA expose, so
  every Intel host reported zero GPUs, logged `gpu-capacity-unavailable`, and was
  unschedulable while reporting "no GPU detected". Intel now reads i915
  `lmem_total_bytes`, then per-tile `physical_vram_size_bytes`, and otherwise
  budgets an explicit share of host RAM for an iGPU whose memory is shared.
  Live free-VRAM stays unknown for Intel, which the admission veto already
  abstains on (#126, PR #133).

## 0.2.4 — 2026-09-06

### Added

- Steam preparation is enabled by default for supported Steam images, with a
  separate switch under Library → Sources → Steam. Image details distinguish
  preparation progress, host opt-outs and measured reflink/copy behavior. Turning
  preparation off preserves existing homes, templates and running sessions (#145).

### Fixed

- Release publication waits for the updater image to be validated and promoted,
  so its installation instructions cannot advertise a missing updater tag.
- Agent startup cleanup only removes its own session and audio containers;
  separate agents on the same Docker daemon preserve each other's sessions (#146).
- First-run setup saves the browser origin while respecting explicit deployment
  policy, and reports signaling failures separately from media connectivity (#131).
- NVIDIA provisioning retry messages preserve the original failure instead of
  recursively nesting backoff messages (#129).
- An advanced NVIDIA driver host-path override verifies the actual shared
  directory before app launch and reports wrong paths through readiness (#130).
- Tagged agent images report their release version consistently with the control
  plane; source builds report a development identity (#135).
- Older same-schema edge builds are no longer offered or accepted as updates,
  including forced apply requests (#136).

- RTX 5090 hosts running NVIDIA 595.99.02 exclude the known-corrupt Vulkan AV1
  path and its unsafe NVENC AV1 fallback. Host readiness and setup explain the
  restriction; the profile picker reflects reported host codecs and automatic
  negotiation can select eligible HEVC/H.264 instead.
- First-install Compose now includes the selected GPU wiring from repository
  definitions, preserves generated credentials on reruns, and uses verified
  image pins. Copied environment files carry blank credentials and instructions.
- Control-plane state ownership preparation, NVIDIA provisioning recovery,
  refreshed readiness and validated sibling mounts reduce delayed app-launch
  failures. App shared memory remains 1 GiB by default.
- Unraid deployments tolerate absent securityfs, including additional-host
  enrollment, and the updater no longer mistakes Btrfs subvolume IDs for Docker
  container IDs.

## 0.2.3 — 2026-09-06

### Fixed

- **A fleet update no longer re-cordons each host moments after it finishes (#140,
  second half).** The per-host apply inside a fleet run found the host already draining
  — the run's own cordon — took it for an admin's, and restored it a few milliseconds
  after the run had lifted it, so 0.2.2 still ended with the host `draining`. An apply
  that belongs to a fleet run now leaves the restore to the run. Found on the 0.2.2 gate.

## 0.2.2 — 2026-09-06

### Fixed

- **A fleet update from v0.2.0 no longer leaves every host `draining` when it
  finishes (#140).** The v0.2.0 control plane cordoned the fleet with nothing to record
  it in; when the new control plane picked the run up it found every host draining, took
  that for the operator's intent, and "restored" the cordons at the end — the run said
  `succeeded` with zero hosts in scheduling. A run adopted with no cordon record now
  treats every cordon as its own and lifts them all; a run adopted with a record
  re-cordons the hosts it owns (the old code only claimed to). An agent's re-register
  also no longer lifts a cordon: `draining` stays `draining` until an admin or the run
  uncordons it, so a cordon survives the control plane's own restart. Found on the
  first real `v0.2.0` → `v0.2.1` update.

## 0.2.1 — 2026-09-05

### Fixed

- **A fleet update no longer fails on the first host right after the control plane
  updates itself (#117).** When the new control plane came back and picked the run up,
  it moved to the first host before the agents had reconnected, recorded the miss as
  `updater_unreachable` and failed the run. An apply now waits for the host's agent to
  be connected (up to 60 s) before sending, a re-adopted run pauses briefly before its
  first host, and an unreachable agent is reported as `timeout`. Found on the first real
  fleet update, `v0.2.0-rc.2` → `v0.2.0`.
- **A failed fleet run no longer leaves hosts cordoned (#117).** The hosts' pre-run
  cordon state was held in memory and lost across the control plane's own restart. It is
  now recorded on the run (migration 0076, `platform_apply_runs.cordoned_hosts`) and
  restored on every terminal path; a host still draining afterwards is logged as an
  error.
- **The Releases tab's Targets rail says "Up to date" for a current control plane (#104)**
  instead of "Not ready".

## 0.2.0 — 2026-09-05

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

### Added

- **Quasar updates itself from the admin console (#104; #105–#119).** A *platform
  release* is a matched control-plane + node-agent image set from one commit. Every
  component now stamps its build identity (semver, source commit, build time; the control
  plane also its highest embedded migration) and the agent reports it on `register`
  together with its install mode (registry or source) and whether an **updater** sits on
  its stack (#107). The control plane detects releases on a weekly job (Monday 02:00 UTC,
  editable, run-now) — the **stable** channel reads GitHub Releases and their
  `platform-release-manifest.json`, the **edge** channel resolves the digest behind a
  branch tag (default `develop`) and reads the build identity off the image labels, each
  following exactly one validated redirect through the shared outbound client
  (`release-assets.githubusercontent.com` for a GitHub Release asset, GHCR's blob host
  for an edge image config) (#110, #111) — and the admin console gains a Fleet ▸ Releases
  tab plus a top banner: installed vs available, cumulative sanitised release notes,
  channel + edge-branch settings, and a per-host eligibility table whose ineligible rows
  carry the exact manual recipe (#112). The release-detection knobs
  (`QUASAR_PLATFORM_RELEASE_REPO/_API/_ASSET_HOSTS/_TOKEN/_DETECT_INTERVAL`,
  `QUASAR_PLATFORM_REGISTRY`, `QUASAR_IMAGE_REGISTRY_HOSTS`) travel from `deploy/.env`
  to the control plane on a registry install, and the public site's stack template
  matches (#110). Applying is a per-host **updater** sidecar (`quasar-updater`, in every
  compose stack and in `enroll-host.sh`'s generated stack) that only accepts digests
  under an allowlisted registry namespace (`QUASAR_UPDATER_ALLOWED_NAMESPACES`, default
  the org's), rewrites the two image lines in `.env` (keeping `.env.prev`), pulls, and
  recreates (#115). From the console an admin applies to one host — the host is
  cordoned, the apply waits for zero sessions (or `force` ends them), the new agent's
  `register` is the success evidence — or presses **Update Quasar** to move the control
  plane first (it recreates itself through its own updater and picks the run back up on
  boot) and then every eligible host in sequence, stopping at the first failure.
  **Update Quasar** is disabled, with the reason in its tooltip, when the control plane
  is not eligible, instead of answering a `409` after the click; a source-built control
  plane is never offered a registry image, and both control-plane images run as uid 1000
  so a volume created by either is usable by the other (`redeploy.sh` fixes the TLS
  volume's ownership once) (#117, #115). An open admin tab gets a dismissible "Quasar was
  updated — reload" toast that appears only when the served bundle actually differs from
  the loaded one (#116, #117). A failed apply is left as it is with the previous digests
  recorded, and an agent can be **reverted** to them from the console; the control plane
  never is (ADR 0002: control plane first, never below the database's migration) (#118).
  `make release VERSION=x.y.z` cuts a release from the changelog and pushes the tag that
  publishes it (#109). Contract: quasar-protocol amendments 1 and 2 (register identity
  fields, `/v1/admin/platform/*`, `release_apply`/`release_state`, migrations 0074 and
  0075). **Existing installs must add the updater once** — see `docs/upgrading.md` "The
  updater". Glossary: `CONTEXT.md` "Platform releases"; decisions: ADR 0001 (pinned-digest
  trust), ADR 0002.

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
  nothing and `develop` publishing stays manual.

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

### Changed

- **One hardened outbound HTTP client for the control plane (#105).** The SSRF
  containment that lived inside the registry digest resolver — HTTPS only, per-caller
  host allowlist, no redirects, DNS-rebind dial guard, bounded bodies, short timeouts —
  is now `internal/outbound`, constructed per caller with its own allowlist and timeout
  so the GitHub Releases client and the edge-channel image-config reader (#110) get it by
  construction, each following exactly one validated redirect (https only, redirect host
  allowlisted, no `Authorization` on the hop) to cover the redirects GitHub and GHCR
  actually answer with — `release-assets.githubusercontent.com` and GHCR's blob host join
  the default allowlist (#111). The registry resolver and the template-context resolver
  use it; `QUASAR_IMAGE_REGISTRY_HOSTS` stays the registry's own knob. Two visible
  deltas. A registry token body over 1 MiB now fails with a named "body too large" error
  instead of being silently truncated into a JSON parse failure. And the allowlist is now
  enforced on every host actually contacted, so a Docker Hub ref needs
  `docker.io,registry-1.docker.io,auth.docker.io` in `QUASAR_IMAGE_REGISTRY_HOSTS` — an
  allowlist of `docker.io` alone used to let the manifest request through unchecked and
  now refuses it (`docs/configuration.md`).

- **The platform container images are named for their role, not their
  implementation.** `quasar-control` → `quasar-control-plane`, `quasar-vulkan` →
  `quasar-node-agent`, and the build-time-only images `quasar-toolchain` →
  `quasar-gst-toolchain`, `quasar-dev` → `quasar-agent-dev`. `quasar-nv` was
  left out of the rename because that lineage was being retired rather than
  renamed — see Removed below. App and session images are unaffected.

  **Upgrading requires no action.** Both names are published for one transition
  window and resolve to the same digests, and a local `deploy/build-images.sh`
  writes the old name as an alias tag on the same image id
  (`--no-legacy-alias` opts out). Pin the new names when you next edit
  `deploy/.env`; the old names are dropped one release after the release that
  introduces the new ones. See [`docs/upgrading.md`](docs/upgrading.md).

- **The published GitHub Release body ends with an install/upgrade footer carrying the
  digests (#104).** Generated from the same validated manifest as the machine-readable
  asset, it gives an operator the per-component digests, the updater's image tag,
  `QUASAR_STACK_DIR`, the pull/recreate command, and links to the docs — the
  human-readable form of the facts the manifest already carries for machines.

- **The admin Fleet ▸ Releases tab was rebuilt to the v3 design (#104).** A
  changelog-aware notes renderer replaces the raw markdown dump: category tags,
  bold-title entries, issue-reference chips, and a "View on GitHub" link per release.
  The release view also gains an additive `source_repo` field.

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
