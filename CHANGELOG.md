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
- **A direct-display console session now passes hidraw nodes, not just evdev, so Steam Input identifies a controller correctly (#462).** The agent grants every `/dev/hidrawN` node present at launch (or, with an `input_devices` allowlist, just the allowlisted events' hidraw siblings, resolved through sysfs) plus a device-cgroup rule for hidraw's major, so a plugged controller shows up in Steam Big Picture as itself instead of falling back to an evdev GUID match. The hidraw devices are listed from sysfs and passed by their host paths, so an owned install, whose agent sees neither the host's `/dev/hidraw*` nor `/host/dev`, is covered too. A controller plugged in for the first time mid-session still isn't identified correctly until the next session start — the container gets no new `/dev/hidrawN` node without a udev client of its own — but keeps working over evdev in the meantime.
- **A console session's desktop now drives the monitor itself (#458).** For an app that declares `runtime_spec.direct_display`, the node agent launches the app container with the console GPU's card node, the sound device, the input devices (plugged in later too) and the host's udev, and the desktop takes the display on its own DRM backend: no compositor, pipeline or second display server in between. The session is running once it is displaying, and reports the monitor's mode.
- **Console mode is being rebuilt as direct display (#453).** Decision and spec recorded: a console session's desktop drives the monitor itself (KWin or gamescope on their DRM backends), nothing of Quasar's sits between it and the screen, and it is never streamed; ADR 0009 and the glossary carry the terms (console session, direct display, displaying). No behaviour changes yet.

### Changed
- **A streamed session's sound now runs on PipeWire instead of a PulseAudio daemon (#392).** Each session's audio sidecar is PipeWire with its PulseAudio protocol and WirePlumber, so games see the same socket, the same `quasar_output` and `quasar_mic_src` devices and the same 48 kHz stereo format as before; nothing to configure. `QUASAR_PULSE_IMAGE`, if you set it, must name an image from this release or later: an older image has no PipeWire and its sessions are silent.
- **The compositor pin drops the retired console additions (#461).** The gst-wayland-display fork pin moves `3dce9d16` → `abb58285`: the `output-modes`, `follow-client-size` and `display-dmabuf` properties, the `wlr-output-management` global and the `quasar-mode-request` bus message are removed from the compositor. The agent stopped using them in #461, so streamed sessions behave the same; the RGB dmabuf ring that feeds the VA zero-copy path stays.
- **Console mode works the same on a rootless engine (#460).** A rootless engine is never asked for a device-cgroup rule it cannot apply: input plugged in later opens through the host's ACL, and `prepare-host.sh --console` now grants every input device, not only keyboards, mice and controllers. The agent opens display cards read-only. The console readiness checks are now `console_card`, `console_input`, `console_sound`, `console_terminal`, `console_udev` and `console_ddc`, each naming the missing grant and the fix (replacing `console_display` and `console_audio`); `console_ddc` skips on a host not prepared for monitor control. `console_udev` reads `unknown` on a console host until its console mode is turned off and on once after this upgrade, which is when the recovery actor tells the agent whether the host has udev data.
- **The node agent reaches every console concern through one place (#454).** The terminal hold, the local-only session and a streamed session's console fan-out moved out of the session runner's streaming path into one module, so the direct-display engine can replace it and the retirement can delete it in one place. No behaviour change.
- **The console page keeps only the settings direct display still uses (#455).** Console
  config is now `enabled`, the output pick, input devices, auto-start on display, default app
  and default user. Physical mode, the streaming switches (dual output), local audio output,
  grab local input, auto-connect controller, compositor and fullscreen are gone: the desktop
  owns its mode, sound and input, and a console session is never streamed. An upgrade drops
  the retired keys from stored configs (migration 0099) and never blocks on them. The
  default-app list offers only apps whose `runtime_spec` declares `direct_display: true`; a
  default app without it shows a readiness failure on the console page and is not launched.
  Contract: protocol amendment 19.

### Removed
- **The old console path is gone: a console session runs only direct (#461).** Console mode
  needs a default app whose `runtime_spec` declares `direct_display`. Gone with the path that
  drew the console through a display server of the agent's: `QUASAR_LOCAL_DISPLAY`, the
  `QUASAR_EXPERIMENTAL_LOCAL_DMABUF` display ring (still accepted as an agent variable, and
  ignored), dual output (a `dual_output` assignment is refused), the agent's console audio
  routing (the desktop plays its own audio; `prepare-host.sh --console-audio-user` is accepted
  and does nothing), and in-app monitor-mode picking (#445, shipped in 0.4.1: the agent no
  longer forwards a mode the app picks; a direct desktop sets its own modes). At node-agent
  recipe revision 4 the agent's own console grants are exactly the console terminal, read
  access to the display cards, the i2c nodes (DDC), logind's seat and session state files, and
  two host facts (udev data and sound): no `SYS_ADMIN` and no sound device, and the Compose
  console overlay likewise. The console desktop's container holds the screen, the input and
  the sound device. After upgrading, turn a console host's console mode off and on once so the
  agent learns whether the host has a sound device (`console_sound` reads `unknown` until
  then). The agent image drops weston, seatd and the `kmssink`/`alsasink` elements.

### Fixed
- **A hand-installed Fedora stack's Steam sessions were silent (#478).** `prepare-host.sh`
  loads the `quasar-nested-audio` SELinux module on every SELinux host (#476), but the
  quick start's manual host steps never emitted it (only the NVIDIA device rule), and a
  machine installed by hand never runs host preparation, so the nested-sandbox type could
  not reach its session's PulseAudio sidecar. The steps now emit the same one-line module
  on every SELinux path, NVIDIA or not; the rule's scope is documented in Device rules
  ("SELinux and session sound").
- **A streamed Steam session on an SELinux host now has sound (#476).** The app runs as the nested-sandbox type (`container_engine_t`), which the policy did not allow to connect to its session's PulseAudio sidecar (`container_t`): the policy's connect rule covers only two containers of the same type, so the stream was silent. `prepare-host.sh` now loads a `quasar-nested-audio` SELinux module granting one permission between the two types and nothing else: a nested-sandbox process may connect to a `container_t` unix socket it can reach by path, which is only its own session's pulse directory because that is all the agent mounts into the app (an app given another session's socket directory could reach that session's audio). **On an SELinux host, run `prepare-host.sh` again** (it is safe to repeat); a host that has not has no sound in those sessions until it does.
- **A console relaunch after an agent reconnect no longer stalls the agent's connection (#477).** A reverse proxy in front of the control plane can close the agent's WebSocket (Caddy sends close 1001 to both ends on every config reload; `stream_close_delay` delays that close rather than preventing it). The agent retries after a one-second backoff, and a console launch still starting fails as `host_lost`, as before. The relaunch then hit that launch's managed-home hold and waited 10 s for the agent's cleanup proof. That proof arrives on the same connection, and nothing on it was read during the wait, so every attempt ran out and logged "managed home location requires repair", and the next reconnect's registration queued behind it. Console auto-start no longer waits: it logs that the previous session still holds the home, and the proof itself triggers the relaunch. Turning console mode off or unplugging the display no longer waits on that connection for the stop's acknowledgement. A heartbeat from a replaced socket is now ignored instead of being logged as "idle approval no longer matches reviewed policy"; it never involved an approval.
- **A rootless console on an SELinux host had no keyboard, mouse or sound (#460).** The
  console session runs as `container_engine_t`, and the policy denied it the input nodes,
  the sound nodes and the udev database, so the desktop started with no input at all.
  `prepare-host.sh --console` now loads a small `quasar-console-devices` SELinux module
  that allows exactly those (and the `hidraw` nodes Steam Input reads); SELinux stays
  enforcing (including the inotify watch PipeWire puts on `/dev/snd`, without which the desktop
  found no sound card). Run host preparation again with `--console` on an SELinux console host.
- **A rootless console desktop had no sound (#460).** The console plan passed `--device /dev/snd`, which rootless Podman realizes as a bind over an empty regular file per node, so `readdir` listed every sound node as a regular file and PipeWire's ALSA monitor counted "0 PCM device(s)" and ignored the card, leaving only a Dummy Output. The console container now gets `/dev/snd` as a read-only directory bind, like `/dev/input`, plus the device-cgroup rule `c 116:* rwm` (a USB sound card plugged in later opens on rootful engines; a rootless engine drops the rule as it does the input one).
- **A Steam library tile can no longer be set as the console's default app (#453).** The
  console page's default-app list and the `console_default_app` readiness check treated a
  game tile as direct-capable through its parent launcher, so it was offered alongside real
  desktops and launchers. Both now also require kind `desktop` or `launcher` with no parent
  app; a saved game or tile fails readiness naming the app's kind instead of launching it.
- **A streamed session on a host with console mode on is reaped for a lost transport again
  (#461).** Every session on such a host was exempt from the idle reaper, an exemption meant
  for the retired dual output; a streamed session whose browser is gone now ends like any
  other.
- **A game on the console no longer hitches every 10 seconds (#458).** The agent read the monitor's power state over DDC every 10 s while a console session ran, and each read is over a second of i2c traffic on the display link. While a desktop owns the display the read is skipped; it still gates auto-start when nothing is on the screen.
- **A console session survives the monitor being switched off (#458).** A monitor that drops its DisplayPort link on power-off looked like an unplug and the desktop was restarted, closing its apps. A disconnected connector no longer stops the session; the desktop handles the display's return. A host that wants an unplug to end the session sets `QUASAR_CONSOLE_DISCONNECT_GRACE` on the control plane. The admin session list and the session page show what a running session is doing: "displaying 3840×2160 @ 240 Hz" (or "not displaying: …") for a console session, "app presented" for a stream.
- **The pull-deadline test flaked under a full parallel `cargo test --workspace` on a loaded
  machine (#463).** It raced a fixed server-side sleep against the client's own deadline and
  asserted a fixed wall-clock bound; the fixture now blocks on an explicit release signal
  instead of a timed sleep, and the elapsed-time assertion is gone. A few sibling timing tests
  in `images::tests` shared the same too-tight bound on a non-blocking call and got a generous
  one.

### Security
- **"Only me" or "Nobody yet" for a library provider now holds even when the provider app is created after the setup wizard has moved on (#490, part of #484).** The wizard's choice used to be retried for about nine seconds and then lost, which left the provider app visible and launchable for every user. Now the control plane keeps the choice (`202`) and grants it when the app is created, including one an admin creates or marks as the provider app by hand, and the wizard says it will apply once installed. With no choice made, a provider app is still for everyone. A provider now has one provider app: creating or marking a second one is refused (`409`), and so is un-marking the only one while library discovery is on (`409 provider_enabled`, turn discovery off first). Contract: protocol amendment 21, migration 0100.
- **The node agent refuses a session id that is not a single path component before it names the session's audio socket directory, and the recovery actor refuses a dump record whose `name` is not its own file's (#481).** Both values build file paths. The control plane only sends UUIDs, and the dumps directory is owner-only, so neither is known to be reachable today; this closes them anyway, like the udev export already did.
- **The API contract test harness pins `golang.org/x/net` v0.55.0 (#480).** It was on v0.50.0, which carries six published vulnerabilities (one critical). The harness is contributor tooling, not part of any image, and its use of the module is indirect.
- **A streamed game can no longer take the screen from a console session on a rootful Docker or Podman host (#464).** A streamed app now gets each render node and each card node mknod-only: the GPU still enumerates (radv and gamescope need the card node to exist) but the card never opens, so a console session started later still gets DRM master (measured on Docker 29.8.1 and Podman 5.8.4). NVIDIA hosts (the container toolkit grants the card read-write whatever is asked) and rootless engines (no device cgroup) cannot enforce this and keep today's grant: there the console holds the display by starting first. `console_card` now says which case the host is in, and the runtime journals and logs `token=app-card-nodes-openable` wherever a streamed app's card opens. A host with no NVIDIA GPU whose agent lists no DRM node now refuses a streamed GPU launch, naming why, instead of granting the whole `/dev/dri`. The per-engine table is in `docs/configuration.md`, "Streamed sessions beside a console".

## 0.4.1 — 2026-10-04

### Added
- **Seed-managed installs can set the node agent's own settings (#448).** Set them on the seed,
  such as `QUASAR_APP_MOUNT_ALLOW` for host folders apps may bind, or change them later with
  `quasar-recovery reconfigure`, which re-creates only the node agent. Settings the install
  manages are refused. See "Agent variables" in `docs/configuration.md`.
- **The console mode page says more about a black picture in a virtual machine (#414).**
  With an NVIDIA GPU passed through, the virtual machine's own boot screen is black too, so
  the cause is the card's hand-off rather than Quasar; the page says so and what to use instead.
- **The Podman page explains a console unreachable from the network (#416).** When Docker
  runs on the same host, it can drop rootful Podman's forwarded traffic, so Quasar answers
  only on the host itself. The page and the troubleshooting guide give Docker's one-line
  fix and how to check it.
- **A readiness check says whether Quasar came back after a reboot (#412).** On Podman and
  rootless Docker, `engine_restart_on_boot` passes once a reboot shows the engine started
  Quasar again, fails if the node agent only came back much later, and names the host
  preparation that fixes it. Before the first reboot it reads `unknown`.

### Fixed
- **A KDE console session's resolution pick no longer snaps back (#447).** The compositor's
  follow-the-window fallback stays quiet while the desktop speaks wlr-output-management, so a
  stale frame at the old size during the switch is not read as a request to go back.
- **Docker install commands separate the recovery image from `seed`.** Both the
  first-machine and GPU-host examples now include the missing space so Docker runs
  the seed command instead of trying to pull a `quasar-recoveryseed` image.
- **In console mode, the desktop's display settings list the monitor's real modes, and
  picking one changes the monitor (#445).** The compositor now advertises every resolution
  and refresh rate the connected display supports and takes a choice through the standard
  output-management protocol; the node agent moves the monitor and the picture to that mode
  with the app still running, and the session reports the mode it runs at. Works with the
  XFCE desktop image; KDE and Steam follow in a later release.
- **`make test-engines-build` no longer leaves `.diagnostics` owned by root (#442).** The
  suite binary and its directories now belong to whoever ran it, so a later `make verify`
  passes. `bench_run.sh` now says when it cannot write its output directory, and how to fix it.
- **A host's Settings page fits a phone screen (#444).** Its two columns now stack instead of
  squeezing the settings to a sliver beside the host card.
- **A stale file at `/dev/i2c-N` no longer stops console mode turning on (#443).** On a
  rootless engine the recovery actor now skips, and logs, an i2c entry that is not a device,
  so console mode turns on and only monitor control goes without that bus. The `console_ddc`
  check says how to clear it.
- **Console audio survives a reboot, and host preparation can restart PipeWire on Fedora
  CoreOS (#433).** The desktop user's PipeWire now starts with their session, so the
  console-audio socket is back after a reboot. Host preparation restarts it through
  `runuser`, which also works on uCore.
- **The host readiness card files the engine and console mode checks in their own groups (#437).**
  The engine checks now sit under Container runtime, and console mode's display, audio and
  monitor control under a new Console mode group, instead of under Other.
- **The Console page's input-device table stays inside its card (#436).** A long device path
  is shortened with an ellipsis, and hovering it shows the whole path. On a phone the page's
  two columns now stack instead of squeezing the settings to a sliver.
- **A web test of the trace viewer no longer fails at random (#427).** It hovered the chart
  before the chart had loaded; it now waits for it.
- **The image-reference test runs again, in `make verify` (#415).** It checked the whole image
  contract against a stand-in Docker that knew only the older checks, so it had been failing
  unnoticed; it now checks only which image reference is pulled and inspected.
- **A local console starts at the monitor's own resolution and refresh rate (#422).** With no
  mode configured and streaming off, the console app now opens at the mode the display runs (for
  example 3840×2160 at 60 Hz) instead of 1920×1080 at 60. A streamed console keeps the app's
  defaults unless you pick a mode, and "Preferred" can be chosen again after picking another mode.
- **Console mode grabs a keyboard, mouse or controller plugged in during a session (#421).**
  With **Input devices** on auto, a device that arrives mid-session is now taken within
  about a second, and one that is unplugged is released. Before, only the devices present
  when the session started were grabbed. A device listed by path is taken when it appears.
- **Rootful Podman brings the node agent back after a reboot with no extra host step (#439).**
  A reboot empties `/run`, and Podman would not start the agent without its runtime
  directory. The recovery actor now has the engine make it at every start, then starts the
  agent. The quick start and **Add host** no longer ask for a `tmpfiles.d` line.
- **Read-only mounts stay read-only on Podman, all the way down (#410).** Podman made only
  the top of a read-only mount read-only, so a disk mounted inside it stayed writable. Quasar
  now asks Podman for a mount without the submounts and checks it. On Podman, a disk mounted
  inside a read-only catalog mount is therefore not shown in the app.
- **Podman older than 5.1 is refused up front (#424).** Podman before 5.1 can't change a
  container's restart policy, so updates failed on it. The quick start and **Add host** now
  stop before pulling anything and name the version, and the host's `runtime_engine` check
  reads it as unsupported. Ubuntu 24.04 ships Podman 4.9.
- **Podman no longer starts a session with a missing bind source (#426).** Podman makes a
  missing bind source on the host instead of refusing the container. Quasar now checks the
  source first and refuses the launch, as Docker does, and nothing is created.
- **Relaunching an app straight after stopping it no longer asks for an operator (#434).**
  While the previous session is still shutting down, the launch now says so and names that
  session, so trying again in a moment works. If it has only just stopped, the launch waits
  briefly for its cleanup instead of reporting that the home needs operator review.
- **Steam starts on an NVIDIA host without CDI (#413).** On a rootful Docker that passes the
  GPU with `--gpus`, Steam no longer exits at its GPU check with "Vulkan loader failed".
- **Console audio through Host PipeWire works straight after host preparation (#433).**
  `prepare-host.sh --console-audio-user USER` now restarts USER's pipewire-pulse when it
  writes the drop-in and USER is logged in, and otherwise prints the restart command. The
  `console_audio` check now says when the console-audio socket is missing, and why.
- **Missing cover art is fetched again (#441).** If the artwork cache is lost, for example
  when a database is restored into a fresh install, the artwork job now fetches the missing
  images again from the reference it already has. An admin's chosen art stays locked. An
  uploaded image can't be fetched again, so the log asks for it to be uploaded once more.
- **A changed homes root takes effect (#418).** After `quasar-recovery reconfigure
  QUASAR_HOME_ROOT=...`, the host switches to the new root and sessions start again. Existing
  homes follow to the same path under the new root, so move their files there first or players
  start with fresh homes.
- **A stopped Quasar service stays stopped on Podman (#425).** Podman restarted a
  crash-looping service, such as a failed control plane, after Quasar had stopped it. Quasar
  now disables a service's restart before stopping it and checks that every stop holds.

## 0.4.0 — 2026-10-02

Quasar 0.4.0 makes a first deployment much simpler. You paste the quick start's stack
into Docker, Dockge, Arcane or Unraid, or run its command, and one seed container
installs Quasar and keeps it running. From then on, updates are applied from the
console, and one that fails its checks is rolled back automatically.

Underneath, this is a major rearchitecture of how Quasar runs. Quasar no longer drives
the Docker CLI or Compose: the control plane and node agent talk to the container engine
through its API, and each machine's recovery actor owns its services. That is what lets
one release run on Docker or Podman, rootful or rootless, with a least-privilege agent.

### Upgrading
- **v0.3.0 and earlier are not updated in place.** Install fresh with the quick start, then restore your old database dump into it (see Upgrading in the docs). From 0.4.0 on, updates are applied from the console.

### Added
- **Owned installs.** One seed container installs Quasar and keeps it running. Updates are applied from Fleet ▸ Releases, and an update that fails its checks is rolled back automatically (#358–#366).
- **Docker and Podman, rootful and rootless (RH-07).** Rootful Docker and Unraid are supported. Rootless Docker and rootful or rootless Podman are experimental on any Linux (#390–#408).
- **Least-privilege node agent.** NVIDIA GPUs are passed by CDI, input uses the host's own device nodes (no `mknod`), and the device rules ship as plain files you can read (#399, #401, #402).
- **Console mode on owned installs.** Play on a monitor attached to the host, including on rootless engines with PipeWire audio (#395, #407).
- **Add host in one line,** or as a seed stack for Dockge or Arcane (#359).
- **Host readiness.** Probe-based checks say what is missing and how to fix it, and a failing check blocks only the launches it affects (#253–#264).
- **Per-GPU codecs.** Each GPU advertises the H.264, HEVC and AV1 it has proven, and Auto picks the GPU with the best codec (#300–#306).
- **App placement and managed images.** You choose where apps may run, Auto picks the hardware, Steam templates are prepared ahead of time, and image cleanup is explicit (#334–#346).
- **Stable and edge documentation,** with a version switcher.

### Changed
- **New owned installs follow the stable release channel** (#409).
- **The quick start names `:latest` image tags,** and the seed pins them to exact digests on the first install (#440).
- **Installing never asks you to download and run a script as root.** The docs are organised by platform and are about 40% shorter.
- **The node agent talks to the container engine through its API** and no longer needs a `docker` or `podman` executable (#239).
- **Dependencies updated;** Go 1.26.

### Fixed
- **Stream audio no longer turns robotic after about 40 minutes,** and game audio is no longer resampled twice (#351).
- **Steam no longer swaps the X and Y buttons,** an unrecognised controller is no longer scrambled silently, and mouse-wheel scrolling is no longer inverted (#348, #350).
- **On rootless Docker, the gamepad reaches the game** (#428).
- **An update interrupted by a reboot no longer leaves a host down** (#438), and an interrupted image pull can be retried (#429).
- **The first session on a new NVIDIA host no longer crashes the node agent** (#388).
- **AMD hosts get a clean picture on the default Vulkan encoder** (#272, #281).
- **Many smaller fixes** to readiness, reconnects, removing and re-adding hosts, Steam preparation and artwork (#255–#292, #378–#389).

### Security
- **Session input stays inside the session.** The compositor takes the session's virtual keyboard and mouse exclusively, so the host no longer also receives a player's keys, and the virtual keyboard no longer offers keys the host acts on (power, sleep, SysRq).
- **A console session owns its own virtual terminal,** so keys typed in it never reach the host's login prompt (#407).

### Known limitations
- **Podman (experimental):**
  - Podman 4.9 (Ubuntu 24.04) can't change a restart policy in place (#424).
  - A crash-looping service can restart after an explicit stop (#425).
  - A missing bind source is created rather than refused (#426).
  - Rootful Podman needs one `tmpfiles.d` line so `/run/quasar-agent` exists at boot (#439).
- **NVIDIA without CDI:** the rootful `--gpus` fallback can't run Steam's Vulkan on a CUDA-only host. Use CDI (#413).
- **Rootless Docker:** home files are owned by a subordinate UID on the host. This is harmless, and readiness notes it.

## 0.3.0 — 2026-09-13

### Added
- **Updating is checked before it starts, put back when it fails, and honest about what it
  skipped** (#185: #186–#190, closes #184; migration 0083; protocol amendment 9). Every target on
  Fleet ▸ Releases now carries a **preflight**: is the updater reachable (with the three-way
  "socket volume not mounted / updater not running / updater not answering" diagnosis), does it
  see the stack directory, was the container started with the same compose files the updater will
  recreate it with, do the release's images resolve at the registry, and — for a host — is the
  agent's health port answered by that agent. A failing check makes the target `preflight_blocked`
  with the fix named on the card; the fleet Update is refused while the control plane is blocked,
  and a blocked host is skipped and named. A host's checks are its own readiness checks (Hosts
  tab ▸ Updates). When a host's new agent container fails its health wait, the **updater restores
  the previous digest itself**, the result carries the failed container's last log lines, and the
  history shows an automatic revert beside the failed apply (ADR 0004); the restored agent adopts
  the apply that replaced it and relays its final state, found on the live gate. A run that skipped a host
  that was behind ends **`succeeded_partial`**, the banner says which host and why, and **Retry
  skipped hosts** starts a plain fleet apply linked to the first run.
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
- **The audit log names the things it is talking about** (#172). Every row served by
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
- **A `429` from the control plane no longer reads as a second, unrelated fault** (#199). When an
  agent's saved node secret belongs to a control plane that has never seen it, a run of refused
  registers trips the enrollment-failure limiter and the WebSocket upgrade is refused — and the
  agent logged that as a bare `agent connection failed: ... HTTP error: 429 Too Many Requests`,
  with nothing tying it to the refusals above it. It now explains the 429 as the consequence it
  is, under its own `cp-connect-rate-limited` token: ten refused registers with no minute's gap
  between them trip it, it lifts a minute after the last refusal, the agent is admitted again on
  its own backoff, and the fault to act on is whatever the refusals reported. The line also names
  the other thing that answers 429 — more than ten handshakes in flight from one address, which a
  fleet behind one NAT can do on a simultaneous reconnect, and where there will be no refusals
  above it at all. A rate-limited upgrade is also no longer counted as a
  registration failure once the agent is already reporting unhealthy, so the 429 cannot overwrite
  the real reason in `/health`. The limiter itself is unchanged: an unknown `node_name` answers
  `host_not_found` where a known one answers `auth_failed`, so exempting it would make `/agent/ws`
  a free node-name enumeration oracle.
- **Restarting the control plane mid-update no longer fails the update, and a long verdict no
  longer strands one** (#202). An apply waiting for its host's agent to come back read a
  shutdown as the host never coming back: the attempt was written `timeout`, which is terminal,
  so the next boot could not resume it — and in the unattended lane a failed release is
  suppressed, so two restarts in a row quietly blocked automatic updates until an admin applied
  by hand. The wait now happens before the request id is minted, so a shutdown leaves the
  attempt where the next boot's adoption picks it up — provided the control plane is back
  inside the apply's own 15-minute deadline; a longer outage still expires it, as it does any
  waiting attempt. Separately, an attempt's `output` column refuses an oversized value, a
  half-a-character one, or one carrying a NUL, rather than truncating it — so a verdict whose
  8 KiB tail began mid-character, or whose last log lines held binary, could not be written at
  all and the attempt hung to its 15-minute deadline; every writer now bounds the output, and
  the four places that spell the 8192 out are pinned to the migration by a test. A failed **revert** also stops borrowing an
  apply's wording: the updater does restore, but the build it puts back is the one the revert
  was leaving, and no automatic revert is recorded in the history.
- **A fleet update that only moves hosts no longer risks leaving one out of scheduling**
  (#200). The instance-wide cordon a fleet run takes belongs to its control-plane step, so a
  run whose control plane was already on the release took none and recorded nothing — while
  the host step it drove still cordoned the host it was about to recreate, leaving the
  restore to the run that held no record of it. The returning agent's registration masked
  it: a host whose update was refused **before** any recreate (`updater_absent`, a busy
  updater, a rejected image namespace) has no agent going away and no registration coming
  back, and stayed `draining` with nothing that knew to lift it. Such a run now records and
  takes the cordon for each host as it reaches it — one host, not the fleet — so the run's
  own finish lifts it, and a control plane that dies mid-run leaves a requirement the next
  start finds and settles.
- **Re-enrolling a machine that was enrolled to another control plane now works on the
  first try** (#199). The node secret minted by the earlier control plane lives in the
  agent's `quasar-agent-data` volume, which survives a re-run of the installer, and the
  agent presented that secret in preference to the enrollment token the operator had just
  pasted. The new control plane had never seen the node, so it answered `host_not_found`
  forever — with a message ("use enrollment_token to enroll first") naming the very thing
  the operator had already done — and ten rejects inside a minute then added a `429` that
  read like a second, unrelated fault. Two changes: the agent, refused with
  `host_not_found` while holding a saved secret, registers **again with the configured
  enrollment token** (once per reject, so a control plane that is merely mid-restore still
  gets the saved secret offered on the attempt after); and the refusal now names the
  credential it refused rather than the remedy the operator had already applied — with no
  token configured the agent names the stale secret's path and how to reset it instead of
  looping silently. The `429` needs no separate fix: a working re-enrollment now costs one
  reject rather than ten. The enrollment-failure budget deliberately still counts this
  refusal, because an unknown `node_name` answers `host_not_found` where a known one
  answers `auth_failed`, and an uncounted miss would make `/agent/ws` a free node-name
  enumeration oracle. `deploy/enroll-host.sh` gains `--reset-identity` /
  `QUASAR_RESET_IDENTITY=1` (clear the saved identity and enroll from scratch) and
  `QUASAR_PROJECT` (the compose project name, which namespaces the identity volume and so
  selects which saved identity an install uses), says when an identity volume is already
  present rather than silently reusing it, and its `--help` now states that
  `--pinnedpubkey` needs `-k` on a self-signed control plane — alone it fails with
  `self-signed certificate (18)` before the pin is ever checked.
- **A re-enrollment now saves the certificate pin of the control plane it actually joined**
  (#199). The pin file beside the node secret was only ever overwritten for an operator-driven
  `CONTROL_PLANE_FINGERPRINT` rotation, so a host that re-enrolled onto a *second* control
  plane kept the *first* one's fingerprint: it connected only while the enrollment string was
  still in its environment, and was stranded the moment that was removed — which is what the
  docs tell operators to do once enrolled. A register that mints a node secret now refreshes
  the pin, because it replaces the identity the old pin belonged to and the new pin has just
  verified a real handshake. A reconnect still never re-learns a pin, and neither path follows
  a symlink at the pin path.
- **A failed update no longer promises a rollback nobody tried — or denies one that ran**
  (#201). The failed-attempt panel on Fleet ▸ Releases carried one fixed line, *"the host is
  still running whatever it had; nothing was rolled back for it automatically"*, written before
  the updater restored anything itself. Since ADR 0004 that is false for every failure past the
  health wait, and a timed-out host is running neither build reliably. The line is now derived
  from the failure: a rejected or un-pulled release says the host is untouched, a container that
  did not come up says the updater puts the previous build back itself, a timeout or an updater
  that stopped answering says what is running there can only be read on the host — and a reason
  this build does not recognise says nothing at all.
- **An update whose host never came back now says where the verdict is** (#201). When a host's
  new agent fails its health wait *and* the updater's automatic restore fails too — one squatted
  health port does both — no agent is left to relay the updater's result, so the attempt could
  only expire on its deadline and the console reported `timeout` with an empty output for a
  double failure the updater had already diagnosed on the host. A timed-out host attempt whose
  agent is not connected now records what that shape means and the exact command to read the
  updater's own verdict there, request id included. It distinguishes three cases, because the
  attempt row does: one that expired before the release was ever sent says so rather than pointing
  at a result that cannot exist; one that was acked and then went silent says the control plane
  cannot tell whether the updater received it, gives the read anyway, and explains that a 404
  there means it never did; and one that got further names the double failure as the likeliest —
  not the only — reading. `docs/upgrading.md` carries the same recipe.
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
- **A fleet run that cannot put the fleet back into scheduling now leaves a recovery
  requirement the next start acts on (#176, migration 0084).** The terminal state is
  written before the cordons are lifted, and `ActiveRun` selects only non-terminal runs —
  so an uncordon that failed, or a process that died in that window, left hosts
  `draining` with a single ERROR line as the entire record and nothing that would ever
  look again. Whether a run's scheduling changes were proven undone is now recorded
  (`platform_apply_runs.cordons_restored_at`, not served), and the control plane sweeps
  the unfinished ones once at start: bounded, idempotent, and still putting an admin's
  own cordon back rather than lifting it. A failure that persists stays outstanding for
  the next start instead of being swallowed.
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
