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
- **A WebTransport session's audio survives a lost packet (#1346, amendment 67).** The host sends every audio packet a second time, on a channel of its own and one packet later, so a packet lost or late on the wire is already there with the next one; the browser plays it once, from whichever copy arrived, and stops concealing it. Audio data use roughly doubles (a few hundred kbit/s at most) and is not counted against the video bitrate. A browser that does not ask for the copies is never sent them, and audio is still never retransmitted on request.
- **A WebTransport session reconstructs a stream that is smaller than the picture (#1349).** When the decoded frame is smaller than the area it is drawn into, the WebGPU presenter takes an upscale + sharpen pass instead of a plain bilinear stretch: it rebuilds the picture with a bicubic filter, whose edges are tighter than a bilinear stretch's, and adds back the detail the stretch would have blurred, bounded to the four source pixels around each point so it cannot draw a halo. Checked on a synthetic 1080p picture drawn at 1440p in a software renderer; not yet on a real session. The session overlay's Input pane gains **Sharpen an upscaled picture** beside Scaling (on by default, saved per device, like the scaling mode). Fit, Fill and Stretch take the pass when upscaling; the pixel-exact 1:1 mode, a 1:1 draw and a downscale draw exactly as before. A browser whose shader compiler rejects the pass draws the old way and logs once; the session is unaffected. The WebRTC `<video>` path is unchanged.
- **Ultra-low latency (PyroWave) in the console and the Launch options (#1060, amendment 58).** A launch profile's Delivery group gains **Ultra-low latency (PyroWave)**: Off, Allow or Prefer when possible, and a **Maximum bitrate** in Mb/s (blank is automatic by resolution). The Launch options Codec list gains it as a fifth entry with a "Local network only" note; when the profile or the device rules it out it is shown disabled with the reason. On a Prefer profile the note under Codec says why Auto did or did not pick it. Picking it launches with `pyrowave: true` and no codec; there is no transport choice.
- **`make e2e-run` can run the transport-fallback and keyboard-and-mouse scenarios (#1277).** `PLAN=fallback` cuts the WebTransport path mid-stream and before the launch through the `fault:webtransport-block` hook and judges the fallback event, the frame after it and that the stream continues on WebRTC; `PLAN=input` sends a fixed sequence of keys, buttons, wheel and relative motion as trusted DevTools events and judges the pointer lock and that nothing is left held; the input-to-photon samples are measured and recorded, not judged. Criteria with no evidence source today are recorded `pass: null` with what is missing.
- **`make e2e-run HOOKS=<dir>` runs the lab's own fault and shaping scripts around a scenario (#1277).** A scenario that needs a step it cannot do itself (`fault:webtransport-block`, `shape:<level>`) asks for it by name; the runner runs `<dir>/<name>` with `apply` or `clear`, and clears every hook it applied on every exit path (a failed scenario, Ctrl-C, a clear that fails, which makes the scenario `error`). A runner killed between apply and clear leaves a ledger beside the bundle; the next run with the same `HOOKS=` waits for a hook that is still running, then clears it, or refuses to start. A scenario whose hook has no script is `skipped` and names it.
- **A WebTransport session on a lossy link repairs lost video without waiting for a resend (#1046).** When the browser reports packet loss of 1 % or more, the host adds forward error correction: repair datagrams from which the browser rebuilds a lost piece of a frame at once, where a resend costs a round trip. The amount follows the loss (5 % extra data at 1 % loss, up to 25 % at 5 % and above) and goes away again within about ten seconds of the loss ending. It is counted against the bitrate the link is judged to carry. Small amounts (up to 15 %) fit in the slack the bitrate control already allows before it moves, so the video bitrate stays where it is: at the top of its range the stream can then send up to about 15 % more than its configured maximum while the link is losing packets. Larger amounts lower the video bitrate. Video never goes under the session's lowest bitrate to make room: near it, repairs use only what the link has above it. A piece the repair cannot rebuild is still asked for again as before, at most 8 ms later. The packet loss such a session reports (`packets_lost`, the stats overlay) no longer counts a piece a repair rebuilds before its NACK (the request to resend it) goes out; one rebuilt after that is still counted. Sessions with ABR off, PyroWave frames and WebRTC sessions are unchanged.
- **A WebTransport session holds a short display cushion when the link is bumpy (#1317, amendment 66).** Video was shown the moment it decoded, so link jitter became stutter (about 4 long frames a second on a weak link, against 0 on WebRTC). The cushion follows the measured frame-arrival jitter, stays 0 on a clean link, never exceeds three frame intervals or 50 ms, and shrinks back as the jitter goes. The stats overlay's playout buffer and `playout_target_ms` show it, and `wt_arrival_jitter_ms` is the jitter behind it. PyroWave and WebRTC are unchanged.
- **Audio dropouts are counted on both transports (#1318, amendment 66).** `audio_concealed_samples` and `audio_concealment_events` are cumulative for the page view: the browser's own concealment counters on WebRTC, silence from an empty audio queue plus concealed lost packets on WebTransport. The stats overlay shows them as "audio dropouts".
- **A WebTransport session reports the same telemetry as a WebRTC one (#1035).** The stats overlay shows bitrate, decode time, present cadence and freezes, and capture-to-present is measured on the host's clock through the ping and pong offset (it starts at encoder output, as on WebRTC). Samples and the clock offset are posted to the control plane for session diagnostics and client health, with fragments found missing reported as packet loss, and `?bench=1` reads the bench marker off the decoded frame and posts its windows through the trace. Two new metric keys: `wt_capture_to_present_ms` and `wt_keyframe_requests`.
- **The Scaling setting works on a WebTransport session (#1034).** Fit, Fill, Stretch and Integer draw as they do over WebRTC, and Match cursor follows the mode and the size the picture is shown at. Before, a WebTransport session always drew Fit.
- **`GET /v1/me/profiles` returns the PyroWave block for each launch profile (#1340, amendment 58).** `pyrowave` carries `mode`, `available` and `reason`, and for a `prefer` profile `auto` and `auto_reason` (`no_measurement`, `rtt_too_high`, `link_too_slow`, from the device's probe figures taken in the last 10 minutes). The Launch options Codec entry reads it, so "Ultra-low latency (PyroWave)" is enabled when the profile, a candidate GPU and the device allow it. The block and the launch share one eligibility check and the Auto rule. The block is advisory: the launch decides on the placed GPU and the resolved rung, and the per-GPU ceiling is not reported.
- **One setting picks the transport for every launch profile that follows it (#1217, amendment 61).** Streaming ▸ Launch profiles has a Default transport beside Default profile (WebRTC or WebTransport, WebRTC out of the box), and a launch profile's Transport control gains "Use instance default", now what a new profile gets. A launch reads the setting once; the transport choice, the PyroWave checks and the `pyrowave_needs_webtransport` warning all use the effective transport, and a request for WebTransport on a profile that follows a WebRTC default is answered with `transport_reason: instance_default_disallows`. Migration 0116 moves every `webrtc` profile to "Use instance default", which is WebRTC until the setting changes, so no session changes on upgrade; the down migration writes the setting back into each such profile. **It changes the database, so there is no going back below 0116 without running its down.** Protocol pin bumped to amendment 61.
- **A forced PyroWave session streams end to end over WebTransport (#1057, the first working end-to-end slice of #1048; amendments 55 and 58).** Add `?codec=pyrowave` to the library URL and launch on a profile whose `pyrowave_mode` is `allow` or `prefer` (settable through the admin launch-profile API): the stream plan grants PyroWave when the placed GPU passed its PyroWave probe and the device reports WebGPU, takes one of the GPU's PyroWave slots (`pyrowave_max_per_gpu`, default 1), and says why it refused on `session.pyrowave`. The agent encodes with `pyrowaveenc` in a pipeline of its own beside the standard encoder, packs each frame into datagrams by amendment 55's block packing, and switches the client back to the standard codec on `codec_fallback` or an encoder failure. Chrome reassembles each frame, conceals lost blocks from the previous frame, decodes in WebGPU and draws through the shared presenter. Migration 0115 adds the PyroWave columns; every profile reads `off`. Rate control and pacing (#1058), and the Launch options entry and the admin switch (#1060), follow.
- **HEVC and AV1 stream over WebTransport (#1043).** The node agent sends H.265 access units (length-prefixed NAL units, `hvcC` in `hello_ack`) and AV1 temporal units (OBUs without the temporal delimiter, `av1C` when the parser has one) as video datagrams, flagging a keyframe only when the bitstream holds an IRAP slice (HEVC) or a sequence header (AV1); NACK repair and keyframe requests work as for H.264. The web client derives the WebCodecs codec string from that description, and the browser now reports HEVC and AV1 decode in its device record (proved with `isConfigSupported` at 1080p and 4K; a browser that posted before re-posts once). The stream plan picks WebTransport for these codecs when that record says the device decodes them; a device or host that cannot stays on WebRTC.
- **A WebTransport session that cannot connect, or is lost, carries on over WebRTC by itself (#1036).** The browser gives a silent WebTransport host 10 seconds (a handshake that never finishes, or no answer to `hello`) instead of a minute, then closes WebTransport, reports a `transport.fallback` reason (`connect_timeout`, `connect_failed`, `auth_failed`, `unsupported_version`, `takeover_rate_limited`, `decoder_unsupported`, `decode_error`, `stream_lost`), mints a fresh signaling token and starts WebRTC; a lost stream gets one reconnect on a fresh transport token first. The launch screen says "Switching to WebRTC…" while it switches and gives WebRTC its own wait before any stall message, and a reload of a session that fell back goes straight to WebRTC. The overlay's detailed stats show the transport and "fell back" with the reason, and a WebTransport launch that is still connecting no longer reads "The signaling connection could not open". The device record now reports HEVC, AV1 and Opus encode support and WebGPU (with `subgroups` and `shader-f16`), and a record posted before those keys existed is posted again at the next launch instead of making a capable browser fall back with `device_no_webcodecs`. Each step of the probe is time-bounded, so a stalled link or a background tab can no longer leave the record unposted, and a record whose WebCodecs or WebGPU step timed out is posted but asked for again within five minutes, not trusted for a day.
- **Video packets shrink mid-session when the client's path drops the full-size ones (#1134).** When the client NACKs packets near the size cap and not the smaller ones, as through a VPN or tunnel that comes up mid-stream, the agent takes 100 bytes off its RTP packets (1200 to 1100 to 1000, never below 900, never back up) and sends a keyframe, so video comes back within a few seconds. A path that loses packets of every size is left alone. Each step logs `token="rtp-mtu-shrunk"`; `QUASAR_RTP_MTU_SHRINK=0` turns it off. WebRTC only.
- **An admin can delete a GE-Proton row that was never enabled and force-remove a tool in an emergency (#1074, part of #1063).** `DELETE /v1/admin/compat-tools/{id}` drops a never-enabled tag row, or a stuck pin-conflict row nothing uses (and puts the shipped entry in its place). `POST /v1/admin/compat-tools/{id}/force-remove` clears the tool's mappings and the fleet default, disables it so every host removes it, replaces parked Steam sessions and stops the sessions that mount it; the stops are retried at startup and every minute until they land. A launch now chooses and records the tools it mounts under the catalogue's row locks, so a disable cannot slip a tool into a session it has already passed over; a launch whose game's mapped GE-Proton build stopped being usable by then fails before the agent is told, instead of running without it. Disabling a tool a game still maps to stays refused with `compat_tool_in_use`.
- **A Proton change reaches a Hot Loading user's next launch (#1070, part of #1063).** Setting or clearing a game's Proton mapping, changing its forced flag, or changing the fleet default stops every parked Steam in the same transaction as the write (`parked_superseded`), and the next heartbeat parks a fresh Steam that reads the new mapping. A repeated identical write, a refused one and a clear of nothing leave parked Steam alone; play sessions are never stopped, but one that was running at the change releases its Steam when its game exits instead of parking it. **Migration 0114 changes the database, so there is no going back below it: once applied, an older control plane will not start.**
- **Hot Loading no longer hands a game the wrong Proton (#1174, part of #1063).** A launch for a game mapped to a GE-Proton tool adopts the parked Steam only if that tool was mounted when the Steam started; otherwise the Steam is restarted (`compat_tool`), as is one whose mounted tools have since been disabled or marked.
- **Hot Loading now takes over the parked Steam when a launch asks for different stream settings (#1133, amendment 59).** A Steam launch on another profile than the parked session's (a 1080p launch over a 1440p park, a different bitrate or a lower frame rate) adopts the park and moves its stream to the new size, bitrate and frame rate, instead of stopping it behind a "Waiting for a slot…" toast and starting Steam cold. Larger sizes or frame rates, a different microphone setting, or a codec the GPU cannot encode still restart Steam, and that answer now carries `reason: steam_restarting`. A codec or H.264-profile change rebuilds the encoder on the warm session (Steam and the game keep running); a change to another encoder family, a rebuild that fails, or an NVENC encoder beside another live one starts the session fresh instead, still without the restart wait. Needs both the control plane and the node agent from this release (agent capability `adopt_restream_v1`); with an older agent, Hot Loading behaves as before.
- **WebTransport video recovers from packet loss with NACK and a keyframe fallback (#1039).** The node agent keeps every video datagram it sent for at least 250 ms (twice the round trip when that is longer, within 16 MiB) and re-sends what a `nack` asks for, each fragment at most twice and at most 500 `nack`s a second per client, answering `nack_miss` for what it no longer holds or has already re-sent twice. The web client NACKs a missing fragment as soon as a later one shows the gap, repeats once after a measured round trip, releases frames in order, and asks for a keyframe only when a frame cannot complete, once per wait.
- **Keyboard, mouse, gamepad and touch input work over WebTransport, and the overlay shows its round trip (#1032).** The web client sends the same input JSON as on WebRTC: presses and releases on the control stream, pointer motion, scroll and stick movement as datagrams (`protocol/transport-webtransport.md` §Input). It pings the host once a second and the overlay's latency figure is that round trip; a client silent on the control stream for 10 s is treated as gone, and the keys, buttons and sticks it held are released. Anything that changes held state, any gamepad button or trigger value included, travels on the control stream; a stick or the pointer last sent as a datagram is repeated there 100 ms later, and a click is preceded by the latest absolute position. Only the attached client's input reaches the host's devices, and the ping runs off a Worker so a hidden tab keeps its session. Pointer lock, raw mouse and Esc are the WebRTC capture code unchanged.
- **A Chrome user can stream a session over WebTransport, video only (#1031).** Add `?transport=webtransport` to the library URL and launch on a profile whose Transport allows it: the web client connects to the host's WebTransport endpoint, reassembles the video datagrams, decodes them with WebCodecs and draws each frame through a WebGPU presenter, contain-scaled. The overlay's detailed stats show the session's transport. The device record now reports WebTransport and WebCodecs support, so the stream plan can choose it. Experimental and off by default: input, audio, fallback to WebRTC and reconnect follow, and WebRTC is unchanged.
- **Game audio and the microphone work over WebTransport (#1033).** The host sends the session's own 10 ms low-delay Opus as datagrams, tapped after the same encoder WebRTC uses; Chrome decodes it with WebCodecs and plays it through an audio worklet, each packet at the time its capture timestamp gives, delayed by the video's own capture-to-present latency so sound and picture are heard together (the offset is on the canvas as `data-av-offset-ms`). The microphone button works as on WebRTC, with the same permission prompt and the same release of the device when switched off: the page encodes Opus with WebCodecs and sends it to the session's microphone sink. The playout delay widens with the jitter the link shows and the delay floor is measured again after a step in the path (a handover); a lost packet is concealed with a 10 ms fade, and duplicate or reordered packets are dropped. The host reorders and paces the microphone through a jitter buffer sized by `QUASAR_MIC_JITTER_MS` (1 to 500 ms), and a reloaded or taken-over client is heard from its first packet. Experimental and off by default; the hardware check follows.
- **Admins can switch a launch profile to WebTransport and set a host's public URL (#1038).** Streaming ▸ Launch profiles gains a Delivery group with the Transport setting (WebRTC, or WebTransport, experimental; it saves as soon as it changes), and Fleet ▸ Hosts ▸ a host gains a WebTransport card: mode, detected address, the read-only listener port, the certificate's expiry and fingerprint, and the public URL override for an edge proxy. The card warns only when certificate renewal has failed with under 24 hours left.
- **The node agent serves video over WebTransport (#1029).** One QUIC listener per agent (`QUASAR_WT_PORT`, default 4433) with a short-lived self-signed certificate whose hashes ride every capacity report; a session the control plane assigns WebTransport accepts a client holding its single-use transport token and streams its access units as contract datagrams with the capture time. WebRTC is unchanged and stays every session's fallback. Experimental: audio, input and congestion control follow.
- **The control plane can now plan a WebTransport session: nothing streams over it yet, and every profile stays on WebRTC until an admin changes it (#1030, the control-plane half of #1024).** A launch profile has a Transport setting (`webrtc`, `webtransport_opt_in` or `webtransport`) and a launch can ask for `transport`; the stream plan picks WebTransport only when the profile allows it, the host reports a usable endpoint and the device probe shows WebTransport and WebCodecs for the session's codec, and otherwise stays on WebRTC and records why. The launch response then carries `transport_options.webtransport` (URL, single-use token, expiry, certificate hashes) beside the unchanged `signaling` block, `POST /v1/sessions/{id}/transport-token` mints reconnect tokens after the agent acknowledges them, and Fleet admins get `GET`/`PATCH /v1/admin/hosts/{id}/webtransport` for a public-URL override behind a reverse proxy. Transport tokens and signaling tokens are separate credentials: neither is accepted where the other belongs. **Migration 0111 changes the database, so there is no going back below it: once applied, an older control plane will not start.**
- **A game mapped to a GE-Proton build that no host has installed yet waits instead of starting without it (#1073).** A launch of that game is placed only on a host that has the tool; when no online host does, the launch answers `503 no_host_available` with reason `compat_tool_missing` and a message that names the tool and says no host can run it yet (or that it is disabled). The web client does not retry it and shows that message. It clears by itself once a host finishes the install; a host that registers without compatibility-tool support forgets what it had installed. The fleet default and Valve builds never hold a launch.
- **An admin can add any GE-Proton release by its tag, verified against the checksum file published in that release (#1072).** `POST /v1/admin/compat-tools` with `{"tag": "GE-Proton11-13"}` reads the release from github.com only (the guarded client: https, no proxy, redirects only to GitHub's own asset hosts), requires its `.sha512sum` file and pins that SHA-512 and the asset's size; the tool is stored disabled with `source: "tag"`, and every host verifies the download against that pin. It is a weaker pin than the recommended list's: the checksum file sits in the same release as the archive, so it proves every host installs the same bytes and catches a later change, but not that the release was untampered when it was added. A missing or malformed checksum file, an unknown tag, a prerelease and a redirect off GitHub are refused (`422 compat_tool_unverifiable`), nothing is stored, and adds, installs and removals are audited. The tool's Steam name is learned from its first install and must be the release's own asset name. `GET /v1/admin/compat-tools/available` lists the recommended releases and the five newest on GitHub (fetched at most once an hour, five minutes after a failure) with each one's catalogue state, for the Library ▸ Compatibility picker (#1075). A tag row whose tag a later release ships with another SHA-512 is marked `pin_conflict` at boot and cannot be enabled.
- **Enabling a recommended GE-Proton release installs it on every host (#1071).** `PATCH /v1/admin/compat-tools/{id}` with `{"enabled": true}` makes the tool desired fleet-wide (at most 8 GE tools; disabling is refused while a mapping or the fleet default names it), and the new `compat_tools.converge` job (Fleet ▸ Jobs) gives each host one install or removal at a time. The agent downloads the release from github.com only, checks its pinned SHA-512 and free space (size + 4 GiB + a 10 GiB reserve), extracts it itself with every unsafe entry refused, and keeps it read-only under `QUASAR_COMPAT_TOOLS_ROOT` (default `/var/lib/quasar/compat-tools`); a Steam session gets each installed tool mounted read-only and Steam's extra-tools path, so a fleet default or mapping naming it applies on hosts that have it. `GET /v1/admin/hosts/{id}/compat-tools` shows each host's tools, state and free space, and the catalogue counts hosts per state. An agent advertises compatibility tools only when that directory is bind-mounted: owned installs get it with node-agent recipe revision 5, and Compose gets a new bind in `deploy/docker-compose.yml`. Placement waits for a host to install a mapped tool (#1073).
- **An admin can set the fleet default Proton for Windows Steam games through the API (#1068, the first slice of #1063): `PUT /v1/admin/compat-tools/default` picks one of Valve's Proton builds (`GET /v1/admin/compat-tools` lists them), and the next Steam launch starts on it.** A game whose user chose a Proton inside Steam keeps that choice; nothing changes until a default is set. Valve's builds need no install; the screen for it is Library ▸ Compatibility (#1075). **Migration 0113 changes the database, so there is no going back below it: once applied, an older control plane will not start.** Needs the Steam image that carries the matching launch wrapper (quasar-images).
- **An admin can map one Steam game to a Proton build through the API (#1069, part of #1063): `PUT /v1/admin/compat-tools/mappings/steam/{appid}` covers every user's tile of that game, `DELETE` returns it to the fleet default or Steam's own choice, and `GET /v1/admin/compat-tools/mappings` lists them.** A mapping fills in only where the user chose nothing in Steam; one marked `forced` replaces the user's choice. Every change is audited. The per-game screen is the Compatibility tool select on the tile's Runtime tab (#1076).
- **An admin can pick the Proton a Steam game runs on from its tile (#1076, part of #1063).** A discovered Steam game's Runtime tab gains a Compatibility tool select (Steam's default, the installed tools, then Valve's builds) and a Force checkbox, saved with the editor's Save through the per-game mapping routes (#1069). The rest of the tab stays read-only.
- **Admins get Library ▸ Compatibility (#1075, part of #1063): pick the default tool for Windows games, see the tools installed on every host, add GE-Proton from a list and remove one that no game uses.** The Default tool select offers Steam's default (Quasar sets nothing), the installed tools, then Valve's builds. Add tool lists the recommended releases and the latest from github.com; picking one installs it on every registered host, and the table shows the install as it goes. Remove is refused with the reason while games use the tool. The per-host card is on each host's detail page (#1077).
- **Each host's detail page shows its compatibility tools (#1077, part of #1063).** A Compatibility tools card after Services lists the tools installed, installing, removing or failed on that host, the disk each holds, and why a failed or waiting install has not run (for example not enough free disk); a failed install links to Fleet ▸ Jobs, and an agent too old for tools says so. Install and removal runs already appear in Fleet ▸ Jobs under the Compatibility tools job. The card shows no install percentage because the agent reports only when a run ends.
- **CI runs a real WebTransport session against the node agent on every change to it (#1044, part of #1024).** A new job in the Agent live tests workflow, `node-agent (live WebTransport session, toolchain image)`, connects a WebTransport client to the agent's listener in the toolchain image, completes `hello` and `hello_ack`, reassembles video datagrams into well-formed H.264 access units and closes cleanly. It runs beside the live WebRTC job and is not a required check.
- **Takeover works across WebTransport and WebRTC, and a reloaded WebTransport tab picks its session back up (#1041, #1193).** One client streams a session at a time, whichever transport it uses, and the last one to connect wins. A WebTransport client that takes over from a WebRTC browser stops that browser's media, and the browser is told "session taken over elsewhere". A WebRTC browser that takes over closes the WebTransport client with `bye taken_over` (close code 4410). The displaced client's held keys and buttons are released once, at the takeover, and nothing it sends after reaches the game. So a page that falls back to WebRTC without its WebTransport close reaching the host no longer has a key it holds lifted 10 s later. At most 6 takeovers of a session in any 60 s, counted across both transports: a 7th client is refused with `takeover_rate_limited` (a WebTransport `hello` keeps its token), and the connected client keeps streaming; a page falling back to WebRTC is never refused. A reload connects again with a fresh token and resumes the same session from a keyframe, with no restart.
- **A WebTransport stream now adapts to the link (#1040, part of #1024).** The browser reports which datagrams arrived, and when, every 50 ms (the contract's `feedback` message). The agent estimates the available rate from it the way WebRTC's congestion control (GCC) does: a delay-gradient trendline with an adaptive threshold, a loss controller, growth capped at 1.5 × the receive rate, and QUIC's own window as a bound. Feedback that stops while media flows lowers the estimate. The estimate feeds the same ABR governor and ladder WebRTC uses, `smooth` mode and its emergency drop included, and one transport at a time writes the encoder's bitrate. Before this a WebTransport session never changed its bitrate, so a capped link dropped frames instead.


### Changed
- **PyroWave encodes the compositor's frames on the GPU with no copy (#1104).** On the Vulkan encode path a PyroWave session asks the compositor for BT.709 NV12, and `pyrowaveenc` encodes those images on the compositor's own Vulkan device: no download, no CPU colour conversion, no Vulkan device of its own, and the image's Vulkan loader is now 1.4.345, built from source over Fedora's 1.4.341, which closes the #267 loader race for every caller (PyroWave's device tables crashed beside instance creation on 1.4.341). A session whose frames are not BT.709 Vulkan images (a compositor started before the grant, CUDA, system memory) keeps the CPU path. The compositor (gst-wayland-display `05bc3c99`) now tags its NV12 output with its colorimetry, BT.601 by default, so that CPU path converts with the right matrix. Sessions without a PyroWave grant are unchanged. In a PyroWave session the standard stream's frames go to the encoder only after PyroWave has finished reading them (about 0.5 ms on an RTX 5090, 2 ms on an AMD iGPU), and the Vulkan HEVC encoder's VUI states the BT.709 those frames carry. Proven for the Vulkan encoders only; the NVENC and VA paths do not take this compositor output.
### Fixed
- **A Steam launch that its shader download suspended is started again instead of leaving the user in Big Picture (#1314).** When another app's update took priority, Steam suspended the launched game's shader pre-cache download, then dropped the app from its update schedule without starting it; the launch request was lost and the reveal showed Big Picture after 90 s. The in-container launch helper now reads Steam's `content_log.txt` beside the console log and, only when the app's last launch task was the shader or depot update, that update was suspended and the app then left the schedule with no later launch line, no game process and no `App Running` state, sends the launch again — at most twice, never over a dialog, a running game or a quit, and with the reveal at the cap unchanged if it still does not start. Not yet proved on a real Steam run.
- **WebTransport audio stretches across a change in the playout delay instead of going silent or skipping (#1346).** The browser placed each 10 ms chunk at the frame its capture timestamp mapped to, so when the delay it plays at grew between one chunk and the next the listener heard that much silence (12 ms of it for a 12 ms rise, 40 ms for 40 ms) counted as a dropout, and when the delay fell, that much audio was cut off. A change of up to 60 ms is now closed by playing the queue at most 2 % slow or fast until its timeline has caught up — a pitch shift of a third of a semitone for the second or two it takes. A change over 60 ms, a queue that runs dry and chunks that never arrived are unchanged, and nothing that used to be counted as a dropout is now hidden: only a step in the stream's own start times is smoothed. A rise that is a whole number of chunks (10, 20, 30 ms to within half a millisecond) cannot be told from lost packets and still plays as silence. Chunks now keep the stream's order when the delay falls by a chunk or more. Steady state is played sample for sample as before. Measured in the unit tests against the real queue: 12 ms and 42 ms rises play with no silence and no dropout, and falls of 8, 12 and 40 ms cut nothing and play in order. Not yet measured on a real link. Part of #1346.
- **WebTransport audio waits for the latest packets on a jittery link (#1346).** The browser sized its audio buffer to cover 95 % of the last two seconds of audio packets, so the latest few of every second could arrive too late and be dropped (about one dropout a second on a phone hotspot). It now also covers the latest packet of the last ten seconds, up to 40 ms: on such a link audio plays later by as much as that packet was late. A lone packet later than 40 ms is not covered beyond that; lateness that keeps recurring, and a stall that delivers its packets in a bunch, are covered as before. A clean link is unchanged. Not yet measured on a real link.
- **A WebTransport session no longer drops its bitrate far below what a jittery link carries (#1316).** When the delay rose, the host cut its rate estimate to 85 % of the rate the browser had just received. After a quiet scene that rate measured how little the game sent, and during a delay spike it measured the stall. Cut after cut, the estimate fell to 2.8 Mbit/s on a link that carried 10 Mbit/s with almost no loss. The cut now starts from the most the link delivered in the last 20 feedback windows (about a second) in which the host sent at full rate, so a delay spike costs one cut of about 15 %, as on WebRTC. A link that really carries less still pulls the estimate down within about a second, also while the scene is quiet. Every cut of the estimate (delay, loss, feedback silence) is logged with its inputs, and a change of its deciding limit at most once a second (`token="webtransport-cc-bound"`, INFO).
- **WebTransport no longer hands its bitrate to QUIC's loss response while the browser's feedback drives the encoder (#1330).** QUIC's own congestion control read reordered or late packets, and 0.1 % random loss, as congestion. It shrank its window until the video no longer fit, then dropped or held back the datagrams the host had already counted as sent: on a reordering link the video stopped. While the host's rate estimate drives the encoder from live feedback, QUIC's window now follows that estimate and loss no longer shrinks it, as on WebRTC. Whenever it does not (a browser that sends no feedback, feedback that stops, `QUASAR_ABR=0`, PyroWave), the connection uses QUIC's standard congestion control, restarted at the path's delay-free bandwidth-delay product when the estimate lets go. When a path stops carrying anything, the host skips video and NACK resends after the NACK hold, asks for a keyframe every hold once the queue has drained, and resumes on one. If the queue stays backed up, or the asks go unanswered, for 24 holds in which video came (about 6 s), it ends the WebTransport media: the browser reconnects over WebTransport once, and only a second loss falls back to WebRTC (`stream_lost`). When a client leaves, the host logs what QUIC did with its datagrams (`token="webtransport-client-quic-stats"`, INFO). Lab rows on jittery and reordering links are still owed.
- **A WebTransport session's present numbers (`present_long_frames`, `present_fps_median`, the interval percentiles, `present_beat_fraction`) are now stamped at the display frame, as WebRTC's are (#1328).** They were stamped when the client submitted each frame, so a decoder burst read as intervals under one display frame, a median above the display rate and long-frame counts WebRTC's could not match. Frames submitted within one display frame are now one displayed frame, and a tab with no display frames (hidden) reports none. The picture itself is shown exactly when it was; `wt_capture_to_present_ms` still ends at submission and now says so in the manifest.
- **A PyroWave session that moves to WebRTC no longer reads as PyroWave (#1215).** After an automatic fallback from WebTransport, or a takeover by a WebRTC browser, `Session.pyrowave.active` stayed true. The agent now reports the switch off (`codec.switched`, reason `webrtc`); the session keeps wanting PyroWave, so a client that comes back on WebTransport gets it again.
- **A session streaming PyroWave now says so (#1299).** `Session.pyrowave.active` and `used` stayed false for the whole session when the client connected before the session reported `running`, which it usually does: the control plane drops a trace event for a session that is not yet running, and the agent sent its `codec.switched` at that moment, on the lane that may drop events. The agent now reports every codec switch on its ordered lifecycle lane, and one made before `running` right after it.
- **A verdict, diagnostic bundle or trace read of an ended session now covers that session's own last five minutes (#1319).** With no `?from`/`?to` given, the read window was measured back from now, so a session that ended more than five minutes ago answered `insufficient` with 0 host and 0 client samples — and a bundle with empty series and no events — over hundreds of rows the control plane still held. An ended session's default window now ends at its `ended_at`; a live session's still ends now, and an explicit window is honoured as before.
- **A WebRTC session now posts `packets_lost`, `frames_dropped` and `freeze_count` as running totals (#1270).** The browser posted each second's own count, while the API contract, the congestion verdict and the bench read these as totals, so on WebRTC the congestion rule compared the wrong quantity. The totals carry on across a reconnect or a fallback from WebTransport, and a WebTransport session's `freeze_count` is a total too. Samples stored before this change stay per-second; the in-session stats still show each second's count, and the admin session cards now show the session's total.
- **A stream that moves to WebRTC mid-session no longer stops under "Click to play" (#1300).** After a WebTransport reconnect or a fallback to WebRTC the picture and sound carry on, and the page takes input capture back when the browser allows it without a click; otherwise the next click on the stream captures as usual. A first load or a reload outside full screen still offers "Click to play".
- **A PyroWave session now reports the datagrams it lost (#1298).** `packets_lost` read 0 on PyroWave whatever the path dropped, because PyroWave loss is concealed and never asked for again. It now counts the datagrams missing from each frame shown or dropped incomplete, so the congestion verdict, client health and the bench see the loss.
- **A WebTransport stream that stops receiving video no longer sits frozen (#1297).** When the connection stays up but no video arrives for 5 s in a visible tab, the session view reconnects once and then carries on over WebRTC, as it does when the connection is lost.
- **Ending a session while a quick switch waits for the Steam game now ends it at once (#1249).** A stop sent while the switch was waiting for the game was not acted on until the wait ended, up to 45 s later, and the session then reported the switch as complete. The switch now drops the app it was starting and the session stops, and a stop that arrives earlier in the switch no longer starts the new app at all. The earlier waits of a switch (the new display, and the outgoing game's quit and cloud sync) still finish first.
- **Signing in no longer holds a browser on WebRTC for a day after a slow capability check (#1254).** A check that timed out at sign-in or at first setup is asked again within minutes, as it already was on a page load. On the admin side: a host whose only failed check is the advisory PyroWave probe no longer shows a red "Needs attention" chip, Remove on Library ▸ Compatibility asks before it removes a tool from every host, and the stream's stats pane names a fallback reason in words.
- **A display change sent with a differently-cased session id no longer loses the stream-size cache (#1100).** The cache is keyed on the session's canonical id.
- **A PyroWave session stop now stops the PyroWave encoder before the compositor is torn down (#1228).** The encoder shares the GPU queue and a compositor frame with the source, and used to outlive its teardown; it is now released with the rest of the client side first.
- **A quick switch from a desktop into a Steam game no longer shows Steam starting up (#1234).** A switch that starts a new container now keeps the switch screen up until the game's window is on screen, as a launch and a game-to-game switch already did, within the switch's 45 s app wait. Any other app is shown on its first frame as before.
- **The same holds when the session started on the desktop (#1241).** A session that starts on a non-Steam app had no launch helper, so a switch into a Steam game had nothing to wait on. Quitting that game also left the player in Steam's library instead of ending the session. It now gets a helper for the first Steam game it switches to, so the start-up is held, quitting the game returns to the Quasar library, and later game-to-game switches stay in the same container.
- **A quick switch into a Steam game starts Steam parked, so no Steam window appears during the switch (#1243).** A switch that starts a new container, or a rollback that restarts the previous app, now hands the container `-silent` and launches the game through the launch helper once Steam is up; the helper holds the launch until a Steam client process exists, so the entrypoint's own start and the game's never race. A session's own cold launch or a container without a helper keeps the tile's `-applaunch` flags.
- **An owned host that is gone can be removed (#1237).** Remove host on a host that is not connected now offers Forget host, which drops its record, its GPUs and its finished sessions and touches nothing on the machine. Before, the dialog only said to reconnect it.
- **Compatibility-tool audit rows name the tool they changed (#1251).** Setting the fleet default records the tag and checksum of the GE-Proton build chosen and of the one it replaced, and enabling or disabling a Valve build records its Steam name, so the audit log reads as more than an id.
- **A desktop that shares the Steam library can quick-switch back into a Steam game, or into another sharing app (#1232, amendment 62).** The swap was refused `409 home_in_use` naming the session itself; the swapping session no longer counts against its own library, and the commit keeps its Steam hold when the target mounts that library. Another live sharing session still refuses the swap.
- **After a quick switch the session page shows the app the server is running (#1230).** The page now takes the current app from the session's `app_id` (read by the 5 s health poll), so a swap whose poll timed out, or a reload that restored the launch's app from router state, no longer leaves the old game marked as playing in the Switch game rail and the bar.
- **The `?codec=pyrowave` and `?transport=` URL switches survive opening a game page before launching (#1216).** A switch seen in the URL is kept for the tab, so a launch from a game page carries `pyrowave: true` or the transport again; an explicit value in the game page's URL replaces it.
- **Switching a running Steam game to another Steam game now starts the new game.** The swap carried none of the session's compatibility tools (GE-Proton), so the new game ran without its Proton and the swap never counted as a switch inside the running Steam; the switch now keeps the session's tools, and a switch that does need a fresh Steam container arms the launch helper on the new game instead of leaving it unwatched.
- **A desktop that shares the user's Steam library now carries it when it is swapped into or auto-started by the console, not only when it is launched (#1006, amendment 60).** A swap into a sharing desktop and a console auto-start of one take the same single-Steam decision, Steam claim and pending-home hold as `POST /v1/sessions` — decided, checked and claimed in the one guard transaction, so a library on another host is left alone: the parked Steam there is not stopped and the swap is not refused for it. The dispatched spec carries the Steam data dir mount. The one-Steam-at-a-time guard applies as for a launch: a live Steam game refuses the swap with `steam_game_running`, a sharing session swapping into another sharing app is refused naming itself, and a parked Hot Loading Steam is superseded first, so the swap answers `409 home_in_use` for a moment instead of taking the library. Swapping away from a sharing desktop releases the Steam home's hold once the agent reports the swap complete — the outgoing container has unmounted it by then — instead of holding the library until the session ends. The Steam hold and Steam claim that a swap or console auto-start newly took are released whenever it fails before the agent (an agent rejection, an unbuildable target spec, a failed auto-start), so nothing stays pinned or held for a launch that never mounted the library.
- **A host with many network interfaces no longer floods each session with ICE candidates (#1199).** The agent gathered on every address, Docker bridges and virtual ethernet pairs included: about 880 candidates per session on a host with 150 addresses. It now skips loopback, link-local addresses and container or VM interfaces (`docker*`, `veth*`, `virbr*`, `vnet*`, Docker's generated `br-<id>` bridges and similar) unless they carry the default route; `QUASAR_ICE_INTERFACES` chooses the interfaces instead.
- **A Hot Loading launch onto a park whose Steam is still signing in now reaches the game (#1159).** Steam accepts `-applaunch` while it starts up and drops it, so the player saw Steam and no game. The launch helper now sends the launch again every 5 s (for at most 120 s) until Steam shows it took it: it logs progress on the launch (a launch it then logs as failed is sent again) or the game runs. It waits while Steam needs you to sign in, and never sends it after another game or launch shows up, a quit, a new target or the end of the session.
- **A runtime torn down mid-restream keeps its compatibility-tool hold until it is gone (#1160).** A control-plane reconnect (or a stop or a failed restream) declined the runtime after the restream's claim had moved its hold to the play session, so for the seconds it took to stop, an admin removing the tool could delete files its container still mounts; the removal now waits (`in_use`) until the runtime's teardown completes.
- **A reaped session's idle reason no longer names WebRTC for a WebTransport session (#1190).** `idle: WebRTC transport lost` and `idle: WebRTC transport never established` are now `idle: transport lost` and `idle: transport never established`.
- **The web client's device probe now reports WebGPU, so a browser can be granted PyroWave (#1189).** The probe never sent a `webgpu` key, so the control plane refused every browser with `device_no_webgpu`. It now asks `navigator.gpu.requestAdapter()` (1.5 s limit) and posts `webgpu: {available, features}` (`subgroups` and `shader-f16` when the adapter has them); a browser with no WebGPU, no adapter, a software (fallback) adapter or a failing request reports not available (quasar-protocol#29 item 26). The freshness stamp holds the verdict, so a timed-out probe re-posts on the next page load, and browsers re-post once so existing device records gain the key.
- **A WebTransport session no longer fails about a second after it starts on a host with many network addresses.** The agent trickles WebRTC fallback ICE candidates that a WebTransport browser never collects; past 256 buffered frames the control plane failed the session with `relay pending queue full`. A full buffer now drops `ice` frames instead (logged once per session as `relay-ice-dropped` with a count), and an `offer` or `bye` evicts the oldest buffered `ice` rather than failing.
- **An enable or disable of a compatibility tool on a large fleet is audited again (#1183).** Its audit row listed every host id, so past about 100 hosts the row exceeded the 4096-byte bound and was dropped. The row now lists at most 40 ids, with `host_ids_truncated` and the exact `host_count`.
- **No admin audit row is dropped for being too long any more (#1183, #1209).** `admin_activity.details` has a 4096-byte bound measured on the row Postgres encodes, and a row over it was refused and only logged: the operation happened with no record of who did it. A row that does not fit is now written cut down instead — its longest free-text and list fields give way until the encoded row fits, and it carries `details_truncated`. On top of that, a compatibility tool's agent-reported `name` and `reason` are capped at 512 bytes from the start (`name_truncated`, `reason_truncated`), a `reason` that arrives as anything but text is dropped and flagged `reason_invalid` rather than written as a structure, and a deleted launch profile's widened app list stops at 40 names, with the true `allow_list_widened_app_count`. A NUL byte anywhere in the details — which Postgres cannot store at all, so the insert failed whatever the row's size — is replaced with U+FFFD and flagged `details_sanitized`.
- **A WebTransport play no longer parks Steam on exit (#1163).** Hot Loading parks are still WebRTC-only and no restream moves a runtime between transports, so a WebTransport launch could never adopt a park a WebTransport play left behind; that park only cost the next launch a supersede, a `503 steam_restarting` and a cold Steam start. Such a play now releases its runtime and Steam stops with it. The trade-off: a WebRTC relaunch within the idle window (another device, or a profile that does not opt into WebTransport) now cold-starts too, since there is no park left to adopt. WebRTC plays still park. Making parks transport-neutral (so WebTransport sessions can adopt one) remains the follow-up.
- **The library no longer warms Steam for a user whose launch would be WebTransport, and a launch that waits on the restarting Steam shows no wait message (#1163, follow-up).** Parks are WebRTC-only, so for a WebTransport device the library-open heartbeat started a Steam the launch then stopped, and the launch cold-started behind several `503`s labelled "Waiting for a slot to free up…". The heartbeat now runs the launch's own transport decision and parks nothing when that launch would plan WebTransport (a `webtransport` profile on a capable device and host, or PyroWave chosen on `prefer`); a `steam_restarting` refusal retries quietly in the ordinary "Launching…" state. Making parks work for WebTransport is #1214.
- **A WebTransport launch no longer adopts a parked Steam that streams over WebRTC (#1161).** Hot Loading parks always stream over WebRTC, and adopting one cannot move it to WebTransport, so such a launch adopted the park and the browser failed with "The signaling connection could not open". The claim now declines it (logged as `transport_differs`): the parked Steam restarts and the launch starts fresh on WebTransport.
- **A WebTransport profile whose codec the transport cannot carry now streams over WebRTC instead of connecting with no video (#1043).** The stream plan chooses WebRTC for such a codec, with `transport_reason` `codec_unsupported_on_transport`; the codec itself is unchanged. HEVC and AV1 are carried as of #1043 (above), so today no codec takes that path.
- **A refused WebTransport `hello` no longer kills Chrome's connection before it reads the `error` (#1120).** The agent dropped its session handle right after closing, which stopped the browser's HTTP/3 control stream (STOP_SENDING), a fatal connection error in Chrome, so a page could see a dead connection instead of a retryable `session_not_ready`. The agent now holds the session until the close has run (bounded by 2 s) on every close: refusals, `bye`, takeover and session end.
- **Video now plays over VPN, mobile and IPv6 tunnels with a 1280-byte MTU (#1131).** The agent's RTP packets were up to about 1430 bytes before SRTP, UDP and IP, so such a path dropped every full-size video packet: black video with sound. RTP packets are now at most 1200 bytes, header extensions and FEC included; `QUASAR_RTP_MTU` changes the limit (`900`-`1436`).
- **A WebTransport session is no longer ended 120 s into play as `idle: WebRTC transport never established` (#1179).** The idle reap counted only a WebRTC peer as connected. A WebTransport client now counts while it is attached, and once it leaves the session ends when the idle window passes with no client, as for WebRTC.
- **`QUASAR_RTP_MTU` can now be set on an owned install (#1168).** The recovery recipe did not carry it, so `reconfigure` refused it; it is now an agent variable, so `reconfigure` accepts it. The recipe's rendered spec is unchanged, so no revision bumps.
- **A stream that falls back from WebTransport to WebRTC always closes its WebTransport connection first (#1250).** The page left the connection open when its control stream had been reset or was blocked, so the host kept a second client for the session. A WebTransport stream the browser cannot configure a decoder for, cannot keep reading, or that names an impossible PyroWave picture size now also falls back to WebRTC instead of leaving a black view.

### Security
- **A session forced onto WebTransport connects on a real install (#1147).** The control plane's Content-Security-Policy allowed the page to connect only to its own origin and to `ws:`/`wss:`, so Chrome refused the WebTransport session to the agent's `https:` origin. `connect-src` now also allows `https:`. An injected script gains no capability it lacked, because `ws:`/`wss:` already reached any host, and `script-src` is unchanged.
- **The control plane's outbound fetches now refuse every non-public address and ignore `HTTPS_PROXY` / `HTTP_PROXY` (#1107).** The address check also refuses carrier-grade NAT (`100.64.0.0/10`, as overlay VPNs use), NAT64 and DNS64 answers that map to a non-public IPv4 address, 6to4, Teredo, documentation, benchmarking and reserved ranges, and any IPv6 address outside `2000::/3`. Before, a name resolving to such an address was fetched, and with a proxy set the check saw only the proxy. Release detection and downloads, the edge registry reads, release-notification webhooks, the image registry and GitHub lookups, the image catalog, SteamGridDB, Steam store lookups and artwork downloads all share one rule and connect direct. **A release-notification webhook to a `100.x` or tailnet receiver is now refused, and a control plane that reaches the internet only through a proxy loses all of these features** (`docs/configuration.md`). Guarded outbound requests ignore proxy environment variables.
- **An agent can no longer put signaling into another host's session (#1116).** The control plane relays an agent's `signaling` frame to the browser only when the session id is placed on the sending host, and drops (and logs, rate-limited) any other, including a non-canonical spelling (upper case, no hyphens) of a session that is placed there. A placement lookup that fails (a database blip) drops that frame, never the session, and the agent's read loop waits at most 2 s per session id per 2 s.
- **A session's rate limits and in-memory counters no longer reset when its id is spelled in another case (#1100).** The stats, trace-event, trace-clock and verdict limiters, the telemetry ingest counters, and the stop, swap and capture state now key on the session row's canonical id, so an owner can't multiply a limit by changing the case of the id in the URL.

### Internal
- **CI no longer fails when the runner's package mirror stops answering.** The Rust jobs' package install gave up when `apt-get update` could not reach any Ubuntu mirror within its two-minute bound (every run failed that way on 2026-11-11). With the package set restored from the CI cache it now installs those files offline, and a script or list edit still restores that job's last set for the runner image.
- **The protocol pin moves to amendment 65 (#1046).** It lets a client hold a missing fragment's first NACK for up to 8 ms while repair datagrams are arriving, and says a frame's repair datagrams follow its video datagrams; the web client and the agent already do both. No `openapi.yaml` change.
- **The protocol pin moves to amendments 63 and 64 (#1035, #1252).** Amendment 63 declares the two WebTransport stats keys and says a video frame's `capture_us` is stamped as it leaves the encoder, which is what the agent does. Amendment 64 says a session that steps down from PyroWave gives its GPU's PyroWave slot back, which is what the control plane does. Both state existing behaviour; `schema.d.ts` is regenerated.
- **A gate run no longer leaves Docker networks behind (#1266).** `make test-go`, `test-rust` and `test-web` ran in a compose project per worktree, and each run left that project's two networks in place. About 15 worktrees filled Docker's address pools, and then `make test-db` could not create its network on that machine. The stages that need no Postgres now run on Docker's own bridge and create no network.
- **The end-to-end streaming test suite has its bundle format (#1272, part of #1271).** A suite run will leave a run bundle: what was run, on which builds, from which client, and everything recorded. `scripts/validate/stream/bundle.mjs` says whether a directory is a valid one (statuses and criteria, metric-manifest keys, a clock on every timestamp, `null` for what was not measured, no address or token), and `docs/testing-e2e-suite.md` describes the format.
- **`e2e-review` writes the deterministic summary an agent reads first (#1275, part of #1271).** `make e2e-review BUNDLE=<dir>` writes `review/summary.json` and `review/REVIEW.md` inside a run bundle and exits 0 only when at least one scenario passed and none failed or errored (nothing passed exits 1, `reason=nothing-passed`). The verdict is derived from the statuses and criteria, never copied, so the validator refuses a `pass` beside a failed criterion, a `pass` with no criterion that passed, a scenario listed twice, and a criterion id that is not a unique non-empty string. The page shows the counts, the builds, the `not_recorded` facts, the failed and not-judged criteria as rows (the passed ones on one line), the five worst one-second windows, and the anomalies each rule can honestly report from `docs/session-trace/thresholds.json` — a rule whose key the file does not state, or whose sample key no sample carries, is named as not checked rather than reported as clean. Bundle text only ever reaches it inside a one-line code span, so a bundle from another machine cannot add a heading, a row or a count; the same bundle and thresholds file give the same bytes out (member order included), the previous review is removed before a new one is rendered so a failed write leaves no page, and both files are renamed into place so it leaves no half a review.
- **`make e2e-run` runs a plan of the end-to-end streaming suite from a test client (#1273, part of #1271).** `TARGET=<stack url> PLAN=smoke [CDP=<DevTools endpoint>] [CLIENT=<label>] [INSECURE_TLS=1]` signs in as an ordinary user through the web client, launches the reference app from the game page in bench mode (codec, frame rate and resolution through the page's own Launch options), holds a stream on the asked transport and codec, and writes a run bundle that passes the validator. Attached to a browser that is already running (`CDP=`), it works in a browser context of its own, so the person's tabs, storage and sign-in are never touched. It stops exactly the sessions in the same-origin `201`s of its own page's launches (a page address never adds one), after closing the page and waiting for every answer still arriving, only once the API confirms the stop; any cleanup that fails, or any write the page sent that the runner cannot put back (every non-read request outside the launch and sign-in calls, bar the library keep-alive and the device record every sign-in posts), makes the scenario `error`; an id the API does not return to the test user is never stopped or called stopped. A plan is strict: a key nothing defines is an `error` before anything launches. `stream-hold` judges first frame, marker, transport (a client fallback fails it), codec and profile as asked, `frameless_seconds` and `present_cadence` (the control plane's hitch rule) over the stream after the product's warm-up, and records `missing_indices_rate` without judging it until a limit is calibrated; a scenario passes only when the first frame was seen (the bench ring's or the `<video>`'s; the loader's reveal is not a frame, so bench mode off on WebTransport is an `error`). Every secret the run holds, including both bearers, is removed as a value (as is, URL-encoded and JSON-escaped; not base64) before anything is serialised, and what the page says is held until its bearer is known; a password or e-mail local part under 8 characters is refused; the bundle is built beside `OUT`, checked, and only then renamed into place, so a bundle that fails the check is deleted and the run exits 2. Certificate checks are on unless `INSECURE_TLS=1`, whatever `NODE_TLS_REJECT_UNAUTHORIZED` says. A run where nothing passed ends `failed`, `reason=nothing-passed`.
- **A run bundle can now be completed with the host's half (#1274, part of #1271).** `make e2e-collect BUNDLE=<dir> HOST=<role>` reads, by session id through the admin API, each scenario's diagnostic bundle for the session's own window (a long session is read in several windows and folded), its raw samples, session row and the host's effective settings, folds them into `metrics.jsonl`/`trace.json` in the run-directory shape `bench_submit.py` reads, and fills `builds.json` with the control plane's and the agent's builds as read at collection. It can run days after the client half: a session whose telemetry the stack has deleted (after its retention period, a day by default) is recorded as not collected (`host-half.json`) and the run exits 3, never as empty; a session that never started is collected as its failed row. A bundle that already has a host half is refused (exit 2) unless `RESUME=1`, which finishes a collection this machine started and never fetches a collected scenario again; a scenario's files are committed together. Addresses and credentials are redacted in every file, and the bearer is never written.
- **`make e2e-post` puts a suite run on quasar-bench (#1276, part of #1271).** `make e2e-post BUNDLE=<dir> HOST=<role> [DRY_RUN=1]` posts one quasar-bench run per scenario whose status is not `skipped`, through `scripts/dx/bench_submit.py`. The commit under test is the node agent's commit from the bundle's `builds.json` — a bundle without one is refused, so a run is never filed under the posting checkout's HEAD — and the tags name the suite run, the client label, the transport, codec and profile, and `clock=raw` because the collected trace keeps client-clock browser events. The run's host is `<role>@<client label>`, so two clients on one host never compare; posting twice updates the same runs, on the script's external id. The criteria reach the run's notes as `<id> pass|fail|not-judged <observed>` and nothing of a criterion's free text is copied into the notes or tags; the review summary, the scenario's `scenario.json` and `timeline.jsonl`, its pictures and, on the first run only, a `tar.gz` of the bundle are artifacts. A failed scenario posts FAIL and is not marked invalid, an error also carries `status=error`, and a skipped scenario posts nothing. Every value from the bundle is validated against the shape of its field and passed as its own argv element, never through a shell; one that does not match is left out and counted. `DRY_RUN=1` prints each run's plan and external id without posting anything. The bench scenario is the scenario's name with its transport, codec and profile, so the same scenario on two transports is never compared with itself.
- **The live WebTransport CI job also checks input and the fallback (#1044).** It now insists by name that the session test, the input test (datagram and control stream reach the devices) and a new fallback test ran: a client that says `bye` while holding a key leaves no client attached, no key held, and a session a fresh token can still join. The browser's half of the fallback stays in the web tests.
- **Release detection's failed-run note no longer runs its two causes together, and says what a failed pass may still write (#1003).** When the stable listing and the branch pass both failed, `last_error` held both messages on two lines; the console draws it as one sentence, so they read as one run-on cause. It is now one line, the two joined by `; `. `detect.go`'s doc comment now says a failed pass can still record the edge or preview build it read, the never-run schedule's unchanged-period case has a test, and the 0.5.2 note above names the Steam scan interval. No product change beyond the error text.
- **The WebTransport takeover audio test no longer fails at random.** `a_client_that_takes_over_numbers_from_zero_and_is_heard_from_its_first_packet` read the delivered count while the second client's last packet could still be draining through the 40 ms jitter buffer; it now reads the count once it has stopped moving for 200 ms. Test only.
- **The docs site is on `js-yaml` 5 (#1009).** Its own use is the stack-template test's parse of a generated Compose stack, which was already a named `load` import and needed no change; the build broke because `@astrojs/starlight` 0.42.7 default-imported the hoisted root copy, and v5 has no default export. Starlight 0.42.10 dropped that dependency (it uses `yaml`), so the site declares `js-yaml` 5.5.0 beside starlight `^0.42.10`; only `@apidevtools/json-schema-ref-parser`, through starlight-openapi, still carries a nested 4.3.2. Supersedes #905.
- **The ten-rebuild Hot Loading test no longer fails on glibc's memory retention (#1184).** Each encoder rebuild's RSS growth was freed memory the allocator kept (live heap grew ~70 KiB per rebuild, and `MALLOC_ARENA_MAX=2` or `malloc_trim` flattened it), not a leak. The test now runs in its own process, trims before each sample and allows 24 MiB; a 9 MiB-per-rebuild leak still fails it.
- **Amendment 60 is pinned (#1006).** The `protocol` submodule moves to its merge: a swap into a sharing desktop and a console auto-start of one share the Steam library as a launch does, and a swap's commit records (or releases) the shared home. Wording only on the protocol side: no field, code or migration changes, so the generated web types are unchanged.
- **Amendment 59 is pinned (#1133).** The `protocol` submodule moves to its merge, and the generated web types pick up the new `steam_restarting` reason text.
- **The node agent has a `pyrowaveenc` GStreamer element and a PyroWave codec probe (#1056).** The element encodes 8-bit 4:2:0 BT.709 frames from system memory with PyroWave on the GPU, one keyframe per frame (the start-of-frame header, then the blocks), at a byte budget of `bitrate` over the frame rate, and writes the header's colour fields itself: the range from the input caps (the library alone always says full range) and centre chroma siting (the library copies its input's, and the browser decoder refuses left siting). The image installs it and the image contract asserts it. Each GPU now runs a PyroWave codec probe (encode a test picture, decode it with the reference decoder); a pass adds `pyrowave` to that GPU's codec set, and a failure shows in readiness as `media_probe_gpu<N>_pyrowave` with the reason. Nothing launches PyroWave yet: sessions stream exactly as before.
- **PyroWave is now chosen at launch: amendment 58 is pinned (#1059).** The `protocol` submodule moves to the merge of amendment 58, which revises amendment 55: a profile's `pyrowave_mode` (`off`, `allow`, `prefer`) replaces `pyrowave_allowed`, a launch-time Auto rule applies to `prefer`, `pyrowave_max_per_gpu` caps PyroWave sessions per GPU (`sessions.pyrowave_slot`), and `GET /v1/me/profiles` gains a `pyrowave` block. The mid-session `PATCH /v1/sessions/{id}/pyrowave` and `session_pyrowave` are withdrawn (moved to 0.6.1), so that route leaves the OpenAPI drift test's pending list and the generated web types are refreshed. ADR 0011 records the decision. Nothing is implemented; behaviour is unchanged.
- **The available-tools route (amendment 57) is pinned (#1067).** The `protocol` submodule moves to the merge of amendment 57, which adds `GET /v1/admin/compat-tools/available`: the recommended GE-Proton list plus the five newest releases, with each tool's live catalogue state. The route is listed as pending in the OpenAPI drift test (C08, #1072) and the generated web types are refreshed. Nothing is implemented; behaviour is unchanged.
- **The refused-peer WebTransport test no longer races a 100 ms timer (#1125).** Its probe ran while `close_session` waited out web-transport-quinn's force-close; a test-only hold now keeps the close pending until the probe has run, with no change to production.
- **WebTransport certificate rotation is proven end to end (#1042).** The agent already made the next certificate at 72 h, presented it at 24 h and dropped the expired one (#1029); a conformance test now walks a certificate's life on a fake clock: a client pinned to both hashes connects before and after the switch, a client pinned only to the old or an expired hash is refused, and a live session streams on across the swap. `Listener::rotate_at` takes the clock so the loop's body is testable. Two defects the test found are fixed: after a suspend the agent could serve and report an expired certificate for up to 10 minutes (a capacity report now ages the certificates first, and the check runs every 60 s), and every swap built a new QUIC server config, which changed the Retry token key and refused a client mid-handshake (the swap now changes only the certificate the one config serves); a key that does not match its certificate is refused, and TLS session resumption is off so no client can resume past a swap.
- **A `taken_over` error that arrives while the signaling socket is still flushing its buffered frames now closes it with 4410 (#1109).** The attach-time flush wrote the error without closing, so the socket stayed open; this was also the cause of `TestAgentTakenOverErrorClosesWith4410` failing about one CI run in three. A new test holds the flush open to prove the close.
- **The published control-plane image builds again on the 0.6 integration branch.** Its web build typechecks the tests, and the PyroWave decoder test (#1105) imports the fixture manifest from `testdata/`, which the image's web stage did not copy. The stage now copies that one file.
- **The PyroWave decoder runs without `subgroups` (#1055).** A device that lacks `subgroups` takes a fallback path: the sign-bit scan runs in workgroup memory, and storage is fp32 even where the device has `shader-f16`. Every golden fixture meets the decoder bar on it, and `make test-webgpu` runs it on every adapter, including a device created with no optional features.
- **The browser's PyroWave decoder, in WebGPU (#1054).** `web/src/pyrowave/` decodes a PyroWave frame (its blocks plus amendment 55's coverage map) into an RGBA texture: dequantise, inverse DWT and BT.709 YCbCr to RGB, ported to WGSL from the spike that measured the codec (#1049), on `subgroups` with fp16 storage where the device has `shader-f16` and fp32 otherwise. A lost block keeps the last frame's coefficients (block concealment); a block the encoder never sent is zero. Nothing loads it yet (the session wiring comes with #1057), so the app bundle is unchanged. `make test-webgpu`, also a CI job, decodes every golden fixture in headless Chromium against its reference decode.
- **The compatibility-tools contract (amendment 56) is pinned (#1066).** The `protocol` submodule moves to the merge of amendment 56: the admin catalogue, fleet-default, per-game mapping and host-card routes, the `compat_tools.converge` job, the trust rules, and the `register` / `capacity` / `session_assign` fields. The ten new routes are listed as pending in the OpenAPI drift test and the generated web types are refreshed. Nothing is implemented and there is no migration yet (0113 comes with the implementation); behaviour is unchanged.
- **Golden PyroWave fixtures with reference decodes (#1053).** `testdata/pyrowave/` holds encoded bitstreams of deterministic synthetic pictures (detail and flat; two sizes, one not a multiple of 32; three rates, one with blocks larger than a datagram's ~1111-byte PyroWave payload) beside the reference library's own decode of each and a manifest of hashes, PSNR and the pin. `scripts/dev/pyrowave-fixtures/gen.sh` regenerates them in the platform image (`--check` proves it byte for byte); a control-plane test pins their hashes, framing and the Dockerfile's PyroWave pin. For the WGSL decoder and `pyrowaveenc` tests.
- **The platform image now builds the PyroWave C API (#1052).** `libpyrowave-shared`, its header and pkg-config file are built from a pinned PyroWave and Granite commit and installed into `/opt/gst`, and the image contract asserts they are there. Nothing uses it yet and a session streams exactly as before; it is the base for the Ultra-low latency (PyroWave) codec (#1048). This changes the toolchain pins, so the toolchain image is rebuilt once.
- **ADRs 0012 and 0013: compatibility tools are a provider-neutral host resource, and the GE-Proton trust rules.** Records the decisions behind choosing and installing Proton for the library's Windows games (fleet-wide desired state, placement, mapping and fill-when-absent rules; github.com-only downloads verified by checksum, safe extraction, audit). Adds the glossary terms library provider, compatibility tool, tool mapping, forced mapping and recommended tool list. Docs only.
- **ADR 0011: PyroWave is a LAN-only, WebTransport-only codec decoded in WebGPU.** Records why the Ultra-low latency (PyroWave) codec never rides WebRTC or the internet and is never chosen automatically over it, and adds the glossary terms Ultra-low latency (PyroWave), PyroWave and block concealment. Docs only.
- **ADR 0010: WebTransport is transport #2, and media may connect to the agent.** Records the second transport and rewords `CLAUDE.md` invariant 6: the API and control never go to the node agent, but the media transport may, with a short-lived token the control plane mints. Adds the glossary terms transport, transport fallback, WebTransport endpoint, edge proxy and presenter. Docs only.
- **A nightly Toolchain refresh builds the GStreamer toolchain image when `quasar-base` has been rebuilt (#1092).** The toolchain tag hashes the base image digest and Images publishes it only on a push to `develop`, so a base rebuild left the nightly live WebRTC job without an image until the next landing (#1088). The new workflow runs ahead of that job on `develop`'s pins, builds only when the tag is missing, treats an unreadable registry as an error, and never touches the preview tag or any agent or app image. The Images gate now also builds everything on a push to `develop` when the base digest differs from the one the preview's agent image was built on, so a rebuilt base still reaches the preview.
- **CI also runs on release integration branches (`integration/<version>`).** Pull requests into one run the jobs their change touches, and a landing on one runs every job, exactly as for `develop`. `CLAUDE.md` now says specs and tickets live as GitHub issues rather than plan files. No product change.
- **The 0.6 contract amendments 54 (WebTransport) and 55 (PyroWave) are pinned; nothing is implemented yet (#1026, #1050).** The `protocol` submodule moves to the merged contracts, the four new routes are listed as pending in the OpenAPI drift test, `schema.d.ts` is regenerated, and the trace allow-lists learn `transport.fallback` (browser) and `codec.switched` (agent). The only behaviour change: the trace endpoint now stores a `transport.fallback` event, which the web client sends when it falls back to WebRTC (#1036). No migration (0111 comes with the implementation).
- **The domain glossary is now `GLOSSARY.md` (was `CONTEXT.md`), matching the updated domain-modeling and setup skills.** Renamed with `git mv`; every reference in the docs, `CLAUDE.md` and code comments follows (the generated API types follow with the next protocol pin). No product change.
- **Session view: a per-platform remap table for controllers the browser does not map (#831).** A pad found in the table for your OS and browser, with the same number of buttons and axes it was captured with, is translated to the standard layout before it is sent, and no longer shows the "controller not recognised" notice. The table starts empty and gains an entry only from a real capture, so nothing changes yet (an entry can write a D-pad as buttons, as one hat axis, or as a pair of -1/0/+1 axes, which is how Linux Chrome shows a hat): other pads keep the notice and its advice.
- **The node agent's encode chain now ends at the encoded-frame boundary (the parser's output); the RTP payloader, its header extensions and `webrtcbin` sit behind a transport trait with WebRTC as its only implementation (#1028).** This prepares the WebTransport path. No product change: sessions stream exactly as before.
- **CI builds the control image's `web-builder` stage on pull requests that touch `web/`, `testdata/` or `deploy/`, so a web test that imports a fixture the Dockerfile does not copy fails on the pull request (#1119).** The check is `docker build --target web-builder` with no tag and no push; it is part of the `web` gate.
- **The image-cleanup scan tests no longer fail intermittently (#1098).** `TestCleanupReadyHistoryWriterQueuesFollowupDuringScan` and `TestCleanupSecondAttemptRequiresItsOwnFreshInventoryBeforeRetiredJournal` assumed a fixed number of follow-up scans, but the managed-identity worker's retry and the test's own notifications race for the same scan and the loser asks for another. Both now answer follow-ups until the service is quiet; no production change.
- **Documentation corrections before the release candidate (#1255).** The trace format now describes `codec.switched` and `transport.fallback` as events that are emitted, the glossary defines Adoption and Quick switch and states that Ultra-low latency (PyroWave) is chosen with the `?codec=pyrowave` URL switch in this release, and the release manifest records the Vulkan loader pin. No product change.

## 0.5.2 — 2026-11-01

### Upgrading
- **Sign-in now needs HTTPS: if your reverse proxy talks to the control plane's HTTP port, name it in `QUASAR_TRUSTED_PROXIES` before you update, or nobody can sign in afterwards (#604).** The proxy must also send `X-Forwarded-Proto: https`: Caddy does by default, nginx needs `proxy_set_header X-Forwarded-Proto $scheme;`. On a Compose install set it in `deploy/.env`; on an owned install run `docker exec -it quasar-recovery quasar-recovery reconfigure --yes QUASAR_TRUSTED_PROXIES=<proxy address>`. A proxy pointed at the HTTPS port, or a browser that opens the HTTPS port directly, needs nothing. The bundled hardened Caddy overlay talks to the HTTP port, so it now requires the variable: `compose up` stops with an error naming it until it is set, and the stack you had keeps running (`docs/operations/hardened-deployment.md`). Your 0.5.1 control plane told you if this is you: it logged `credential-route-plaintext` or `forwarded-from-unlisted-peer`. Users already signed in keep working until their sign-in expires (24 h by default). `QUASAR_TLS=off` with no named proxy can no longer sign anyone in.
- **This release changes the database (migrations 0109 and 0110), so there is no going back below it.** Once applied, an older control plane will not start. For Quasar's own database the way back is a restore of the dump the update takes first (`docs/upgrading.md`, "The one-way migration rule"); on your own database, back up first. Fleet ▸ Releases ▸ Update Quasar waits for live sessions to end before it replaces the control plane (or tick "Update now" to end them), and automatic updates never install a release that changes the database.
- **Then update the desktop images.** In Library ▸ Images, Sync catalog, then update KDE desktop and XFCE desktop. Steam inside the new desktop images signs in without its 10 s network wait (#807); on the older images it still waits.
- **It worked when** you can sign in through your usual address, the control plane logs no `credential-route-plaintext` line, Fleet ▸ Releases shows this release everywhere and the run ends `succeeded`, and Library ▸ Images shows KDE desktop and XFCE desktop on 2026.10.31 with no update offered.

### Added
- **A KDE or XFCE desktop can open the user's own Steam library (#1001).** An admin turns on "Share the user's Steam library" for a desktop app (Runtime tab, Storage). Steam inside that desktop is then the user's real Steam: same account, games and saves, no second download. Turn it on only once every host runs this release. One Steam at a time: while the desktop is open the user's Steam games show as in use, a running Steam game must be closed before the desktop opens, and an idle Hot Loading Steam steps aside first. If the user's Steam library is on another host, the desktop opens without it and says so. Console auto-start and swaps never share. Migration 0110.
- **An owned install can turn HSTS on (#874).** Set `QUASAR_HSTS_MAX_AGE` (seconds) as a seed input, or on a running install with `docker exec -it quasar-recovery quasar-recovery reconfigure --yes QUASAR_HSTS_MAX_AGE=86400` on the first machine; it re-creates the control plane, and live sessions keep streaming. The control-plane image moves to recipe revision 4, which the recovery actor of this release renders. HSTS stays off unless you set it, and it pins the whole host name: read `docs/configuration.md` first. `restore` back to a release before this one starts its control plane without HSTS (the value stays recorded and applies again at the next update).

### Changed
- **Sign-in, registration and password change are refused over plain HTTP (#604).** They are served on the HTTPS listener, or through a proxy named in `QUASAR_TRUSTED_PROXIES` that sends `X-Forwarded-Proto: https`; anything else gets `403 forbidden` before the password is read, the form says why, and the control plane logs `credential-route-plaintext` (bounded per source). Other signed-in calls and the agent routes are unchanged. The local `make up` stack now serves HTTPS too: sign in on the `https://` address it prints.
- **The node agent image is now built with its CUDA feature (#927).** It was silently left out, so an NVIDIA host that falls back to NVENC (`QUASAR_ENCODER=nvenc`, or a codec with its Vulkan knob turned off) copied each frame through system memory. It now stays on the GPU. Vulkan is still the default and AMD and Intel hosts are unchanged. The image contract now fails a build that lacks the feature.

### Fixed
- **A desktop app linked to a runtime preset with a managed home now shows it on in the app editor (#1012).** The app's own flag stays off, but the launch always took the preset's managed home, so the desktop did run with one; the editor read the stored flag and hid "Share the user's Steam library" (#1001). The switch now shows on and fixed while the preset provides it. Nothing is stored or moved: existing apps keep their own setting.
- **Steam started inside the KDE or XFCE desktop no longer waits 10 s for network before it signs in (#807).** In a session container NetworkManager never marks the connection as the default route, and Steam waited out its fixed timeout on every start. The desktop images now start Steam without the system bus, so it skips the wait; the desktop itself keeps NetworkManager. Steam's own network indicator reads "no network", which is cosmetic. Needs the 2026.10.31 desktop images.
- **A rate-limited GitHub no longer stops the Preview or Edge channel from checking its own build (#892).** The release check lists the stable releases first; when that listing failed (a 403 once the unauthenticated quota ran out) the run ended there and the preview tag, which comes from the registry only, was never read. The failure is still reported as the check's last error, and the build is recorded anyway.
- **Switching the release check to daily now moves a check that has never run to the next daily window at once (#892).** A job with no finished run kept the date it was first given (the next Monday) until it ran once. The same re-dating covers the Steam scan interval when its setting changes, because the keeping is the jobs dispatcher's, not the release check's.
- **The launch detail note for a chain whose top codec your host or browser lacks now says it starts one step lower (#870).** It used to read "No host that can encode this quality is available" for a launch that steps down and works.
- **Console: Bluetooth LE keyboards, mice and pads now teach the monitor input (#971).** The monitor-input learning (#966) skipped them, because they appear under the same `virtual` branch of `/sys` as a streamed session's devices; only a streamed session's own devices are now ignored. Classic Bluetooth, USB and wired devices were never affected.
- **Console auto-start no longer launches when a shared monitor powers on to another machine's input (#966).** Auto-start used to wait for the monitor's power alone, so a monitor shared between this host and another machine started a console session nobody could see (it held the user's Steam home too). The agent now learns which monitor input is this host's: on the first input from a physical keyboard, mouse or controller during a console session (a streamed session's virtual devices do not count) it reads the monitor's power and input over DDC/CI, at most 3 attempts 8 s apart per connector, then stops until console mode is toggled or the agent restarts, and keeps the value per connector. From then on auto-start waits until the monitor shows that input, and a session left on another input with no local input ends after the same waits as one left on a switched-off monitor. `QUASAR_CONSOLE_INPUT_SOURCE` still wins when set. Until an input is learned, power alone decides as before. A shared keyboard or KVM that stays on this host while the monitor shows another machine can teach the wrong input; turn console mode off and on again, or set `QUASAR_CONSOLE_INPUT_SOURCE`. The host's `console_ddc` readiness check says when auto-start is waiting on the input; turn console mode off and on again to learn it afresh after re-cabling.
- **Console: a gamepad stick's noise on both sides of rest no longer keeps the host looking busy, and one key press while the monitor wakes slowly no longer ends the look for it (#811).** On a console desktop an abandoned session on a monitor left off is stopped once the host's input has been quiet; noise flipping across a stick's resting value counted as input every time, and a single key press with a panel that took over 10 s to wake stopped the checks until 10 minutes of quiet. The checks now go on every 5 minutes after such a press until the monitor reads on.
- **A Region and Input change saved after you press Play no longer reaches that launch (#891).** The values are read before the launch is accepted, so a save made while the app is still starting applies from your next launch, as the settings page says. A time zone of `Local`, `localtime`, `posixrules`, `posix/…` or `right/…` is now refused when you save it, as none is a time zone name; one already saved keeps working.
- **Fleet ▸ Hosts ▸ Settings: an administrator can release a recovery hold (#656).** A host whose configuration restart ended `uncertain` stayed draining for good, and its agent refused every session and every new restart until a database edit and a journal edit. The Idle apply panel now offers "Release recovery hold": the confirmation says the host's state is unverified, the request (`POST /v1/admin/hosts/{id}/idle-apply/{attempt_id}/release-recovery` with `acknowledge: "unverified"`) is audited and sent to the agent as `config_policy_release` once its journal is reconciled, and the agent writes the attempt as `released` before reporting it. Only that report lifts the hold, moves the group to `failed` so the configuration can be approved again, and returns the host to service. Until then the panel says what the release waits for (the agent's reconnect, an agent update, or its confirmation). Needs this release's node agent. Migration 0109.
- **Account ▸ Stream quality: the empty choice names what it launches when an admin default is set (#814).** It read "Recommended (…)" and saved as "Launches use the recommendation again" even though an admin's global default decides the launch; it now reads "Admin default (…)" and says so.
- **Match cursor keeps the cursor under the pointer when the stream size differs from the compositor size (#898).** The stream-size lever and an automatic resolution step shrink the stream below the compositor, and a lowered render resolution shrinks the compositor below the stream. The client now scales mouse movement by the compositor size, so the cursor no longer lags or runs ahead of the pointer. A stream at the compositor size changes nothing.
- **Match cursor keeps the cursor under the pointer after a page reload that follows a lowered render size (#972).** The page now remembers the render size the host last accepted for the session, for as long as the tab lives, instead of assuming the launch size until you change it again. It does not see a render size changed from another tab or device.
- **The session HUD keeps Interface size after a page reload (#995).** It read 1× after a reload even though the host still rendered at the scale you last set, and the next change started from the wrong base. The page now remembers the interface size the host last accepted for the session, for as long as the tab lives, the same way it does the render size (#972). It does not see a change made from another tab or device.
- **A failed connection with the input channel gone shows "Back to library" at once instead of half a second after Exit disappears (#812).** The session view held the recovery banner for 500 ms whatever its phase; a terminal failure now skips the wait.
- **Swapping to a Steam game when you have no Steam library yet no longer says the library lives on another host (#813).** The in-session swap now says the app's library isn't set up on this host, to exit and launch it from the library, and to start Steam once first if you haven't.
- **Clicking a section on the Account page no longer shifts the top bar and the section list up (#1013).** The page's hidden timezone status line sat outside the scrolling box, so the whole window was scrollable by a few dozen pixels and a section jump scrolled it too. The scrolling box now holds everything inside it, so only the page content moves.

### Security
- **A caller retrying agent registration back to back can no longer get a few attempts past the enrollment-failure limit (#984).** The control plane replied to a refused registration (a bad enrollment token, an unknown or wrong `node_secret`) before counting it, and reset the count after a successful registration only after replying, so a dial sent the moment a reply arrived could land before the count did. Both now happen before the reply: 10 refused attempts from one address (an IPv6 /64 counts as one address), then 429 for a minute. Agents that register normally see no change.

### Internal
- **Parked-session test: the release check waits for the stop command (#1010).** `TestAParkEndedBeforeAdoptionKeepsTheHoldOnTheHeldRuntime` read the stop sent for the held runtime as soon as the parked row turned `failed`, but the control plane sends it just after; the test now waits (up to 5 s) for it. It failed on CI runs under load. No product change.
- **CI jobs run on a pinned `ubuntu-26.04` runner instead of `ubuntu-latest` (#985).** The next hosted-image rollout is now a deliberate change; `ci.yml` says how to make it. The stale Ubuntu 24.04 and GStreamer 1.24 notes now say 26.04 and 1.28.2. No product change.
- CI follows the runner image's 2026-10-30 update (#982): the engine suite no longer records Podman's `restart` row as a known failure (the runner's Podman is now 5.7, which passes it, so the jobs failed on the pass), and the real-uinput test step hands cargo its homes explicitly, because the runner's sudo no longer honours `-E`.
- **Image cleanup test: the second attempt's journal checks no longer race the control plane (#960).** The test now answers every inventory scan as the agent does (a late inventory change that overlaps the attempt's own reconcile costs one more scan before the journal), and treats only a journal request naming the second attempt as stale (the first dispatch's own request for the first attempt can land late). It failed on most CI runs under load. No product change.
- **Region and Input: the agent's recorded values after an app swap are tested to be exactly the nine keys (#891).** No product change.
- **The image contract's `agent_binary.no_dt_needed` assertion now actually runs (#975).** It called `readelf`, which the agent image does not have, and a missing tool read as a pass; it lists the agent's libraries with `ldd` and fails when `ldd` cannot run. `libgstcuda` is no longer on the forbidden list, because a correct `--features cuda` build links it. No product change.
- **The compose overlay render test passes again and now runs in CI on changes under `deploy/` (#967).** Its console-overlay checks still asserted `SYS_ADMIN` and a DRM cgroup rule, which the console overlay stopped granting when the session moved into its own container (ADR 0009); they now assert the overlay grants neither, keeps the i2c rule and leaves the base evdev rule intact. The overlay was right and the test stale (not Compose v5). `deploy/overlays/README.md` described the old grant too. No product change.
- **`docs/configuration.md` no longer calls the image catalog ref admin-editable (#818).** No route sets `instance_settings.image_catalog_ref`; the default is `stable`. The row now says so and gives the one SQL statement that moves an install to another catalog ref. No product change.
- **Dependency batch for 0.5.2:** node-agent `tokio` 1.54.1, `rustls` 0.23.46, `webpki-roots` 1.0.11, `sha2` 0.11.1, `libc` 0.2.191; web `vite` 8.3.5, `vitest` and `@vitest/coverage-v8` 5.0.5, `jsdom` 30.1.3, `react` and `react-dom` 19.3.1; site `astro` 7.4.2 and `@astrojs/starlight` 0.42.7 (the site stays on `js-yaml` 4.3.2); control plane `golang.org/x/image` 0.47.0; CI `docker/login-action` 4.7.0 and `Swatinem/rust-cache` 2.9.3. `webpki-roots` 1.0.11 drops three eMudhra roots (emSign Root CA - C1, emSign ECC Root CA - C3 and emSign ECC Root CA - G3; emSign Root CA - G1 is still trusted), so a node agent download or release check whose certificate chain ends at one of them stops verifying; the hosts the agent fetches from (GitHub, NVIDIA) chain elsewhere, and a control-plane link with a pinned fingerprint uses no public roots. An agent enrolled without a pin verifies the control plane's certificate against these roots, so a control-plane certificate that chains to one of the three dropped roots stops that agent connecting. `tokio` 1.54.1 carries the multi-thread scheduler injection-queue, `Notify` wakeup-ordering and accepted-socket readiness changes; the agent uses none of the affected APIs.

## 0.5.1 — 2026-10-30

### Upgrading
- **This release changes the database (migrations 0107 and 0108), so there is no going back below it.** Once applied, an older control plane will not start. For Quasar's own database the way back is a restore of the dump the update takes first (`docs/upgrading.md`, "The one-way migration rule"); on your own database, back up first. Fleet ▸ Releases ▸ Update Quasar waits for live sessions to end before it replaces the control plane (or tick "Update now" to end them), and automatic updates never install a release that changes the database.
- **Then update the Steam and desktop images.** In Library ▸ Images, Sync catalog, then update Steam, KDE desktop and XFCE desktop. The new Steam image turns its idle screensaver off in a stream (#834), and the new desktop images start KDE and XFCE in the session's keyboard layout (#850); on the older images those two stay as they were.
- **It worked when Fleet ▸ Releases shows this release everywhere and the run ends `succeeded`,** and Library ▸ Images shows Steam, KDE desktop and XFCE desktop on their new versions.

### Added
- **Control plane: a Preview release channel, checked daily (#841).** `release_channel` accepts `preview`: every build of `develop`, read from the `quasar-release-manifest:preview` image in the platform registry (no GitHub call for it) and otherwise handled as an edge build. Release detection now takes its period from the channel: daily on Preview, weekly on every other channel, still inside the Jobs tab's window. An interval set by hand on the detection job is ignored from now on; `QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL` still wins when set. The picker option and the Daily/Weekly choice come with #842. Migration 0107.
- **Fleet ▸ Releases: Daily or Weekly check, and Preview in the channel picker (#842).** A Daily / Weekly choice sits beside the channel and shows the period detection runs on, or says it follows the channel until you pick; "Follow the channel" undoes a pick. `release_check` joins the instance settings (`daily`, `weekly` or `null`; a channel switch never changes it). The picker gains Preview, Edge reads "Edge (test builds)", and a Preview release card shows its commit like an Edge one.
- **Fleet ▸ Releases: a Preview offer lists the commits you do not have yet (#843).** Each Preview build now carries one line per landing on `develop` since the last stable release (`#<number> <title>`, newest first, at most 500), and the Releases tab shows the ones newer than the installed build as a plain-text list of short commit and subject. The release view gains `new_commit_count`. A subject with a control, line-separator or bidi character is now refused by the control plane (#886).
- **Region and Input settings, server side (#888).** Each user's timezone, language, region format, keyboard layout and mouse, scroll and gamepad choices are stored on their account (`GET`/`PATCH /v1/me/region-input`), with nine install defaults on `/v1/admin/settings`. Every app now starts with `TZ`, `LANG`, `LC_*` and `XKB_DEFAULT_LAYOUT`/`_VARIANT` from the user's value, then the browser's, then the install default. Saving a changed region or keyboard value ends an idle Hot Loading Steam, and a parked Steam started with other values is never reused. The Account and Admin pages for them are listed below; language and region format come with #848, and the keyboard layout's Account control with #918. Migration 0108. After the agent is updated, the first launch that would reuse an idle Hot Loading Steam starts it fresh once, because that Steam was started before the agent recorded these values.
- **Account ▸ Region: set your timezone (#846).** A new Region section after Stream quality lists places to search by city, country or zone, with "Follow my browser" as the default, and saves each pick at once; a launch starts in it from your next one, and a pick closes your idle Hot Loading Steam. Admin ▸ Settings gains a Region defaults card with the install's default timezone, a grouped dropdown. Language and region format come with #848.
- **Admin ▸ Settings: Region and Input defaults (#894).** Two cards set what a user starts with until they pick their own: language, region format ("Same as language" by default), keyboard layout and variant, match cursor, sensitivity, raw mouse input, flip scroll and the Nintendo layout, beside the default timezone. Each saves as you change it, and a value the server refuses shows under its row. Nothing is locked: a user's own choice wins.
- **A session types in the user's keyboard layout (#849).** The session's compositor now builds its keymap from the app's `XKB_DEFAULT_LAYOUT` and `XKB_DEFAULT_VARIANT` instead of always US, so a `de` user gets `z` on the Y key and `ö` on the `;` key. A layout or variant the compositor cannot build falls back to US with a warning in the node agent's log; the session still starts. A swap keeps the session's layout, and Ctrl+Shift+Q still closes the focused window on a non-Latin layout. The compositor pin moves to `e650cf7f`.
- **Account ▸ Input: match the cursor to your window, and mouse sensitivity (#851).** Pick Auto (on for desktop apps, off for games), Always on or Off, and a sensitivity from 0.25× to 4×. When matching is on, mouse movement is scaled by the stream size over the size it is shown at, so the cursor stays under your pointer when the stream is bigger than your window; the fractional remainder is carried, and a window equal to the stream changes nothing. Applies from your next launch.
- **Account ▸ Input: raw mouse input (#852).** A switch, off by default, "Raw mouse input (no acceleration)". On, the session asks the browser for pointer lock with `unadjustedMovement`, so the cursor moves exactly as the mouse does with the system's acceleration off. Chrome and Edge only: other browsers show the switch disabled with "Not available in this browser". If a browser that looked able to refuses it at capture time, the session locks the mouse as it always did and says so once. The input wire is unchanged, and with the switch off the lock request is the same as before.
- **Account ▸ Input: flip scroll direction (#853).** A switch, off by default. On, the mouse wheel's direction is reversed in the browser before it is sent, on both axes, so a wheel that scrolls the wrong way for you scrolls the right way in a game or desktop. Touchscreen scrolling (the touch gesture layer) is unchanged; trackpad scrolling is a wheel and flips. The input wire is unchanged, and with the switch off the wheel values sent are the same as before.
- **Account ▸ Input: Nintendo gamepad layout (#854).** A switch, off by default, under a new Gamepad heading. On, the browser swaps button A with B and X with Y before the pad's state is sent, so on a Switch-style controller the button printed A acts as A in a game. It applies to pads the browser reports in the standard layout; other pads are sent as they always were. It applies from your next session, as the section says. The input wire is unchanged, and with the switch off the pad state sent is the same as before.
- **The KDE and XFCE desktops start in the session's keyboard layout (#850).** The desktop images write the session's layout and variant into the desktop's own keyboard settings at every launch, so a layout changed inside the desktop comes back to yours at the next one; an existing config is left untouched when no layout is sent. Ships with the quasar-images `stable` update that comes with this release.
- **Account ▸ Region: pick your timezone from a world map (#847).** A dot map beside the timezone list lights the places on the clock under your pointer; a click picks the nearest place, saves like a list pick and scrolls the list to it. The list stays the keyboard and screen-reader way in. The map is hidden on phones. Its data (Natural Earth land, tz database places, both public domain) is generated by `scripts/dev/gen-timezone-map.mjs` and committed, so nothing is fetched at runtime.
- **Account ▸ Input: keyboard layout and variant (#918).** The Input section opens with Layout and Variant, from the same lists as the install default. "Same as the install (<layout>)" is the first option and sends `keyboard: null`; picking a layout starts it on its basic variant, and a stored layout or variant the list lacks shows as itself. A change closes your idle Hot Loading Steam, and applies from your next launch.
- **Account ▸ Region: pick your language and region format (#848).** Two selects under the timezone list: Language ("Follow my browser" by default) and Region format ("Same as language" by default, with a live example of dates, numbers and time). Each saves at once; a launch starts with them from your next one, and a change closes your idle Hot Loading Steam. The `quasar-base` image no longer hard-sets `LC_ALL`, which had overridden them in every app container (quasar-images).

### Changed
- **The player app loads about 45 % less code before the library appears (#948).** The session view, game page, account area, command palette, sign-in, register and setup pages now load when needed, and the library fetches the game page and session view as you hover or focus a tile or Play. Opening a game or a session is as fast as before. Nothing looks different.
- **Classic layout is deprecated (#855).** Account ▸ Appearance now says the switch back to the previous player layout will be removed in a later release. Nothing is removed yet.

### Fixed
- A session stopped before its launch reached the host now ends `stopped` on a node agent without the home-cleanup ledger (an older agent, or one that logs `home-cleanup-ledger-unverified`) (#940). That agent reports nothing for a session it never received, so the session stayed `stopping`, holding its encode slot, VRAM and home, until the agent disconnected. The control plane now ends it when it abandons the launch.
- **Account ▸ Region: the world-map caption and the selected time zone's details are easier to read (#950).** The map's instruction line was too faint (4.3:1 in dark, 2.9:1 in light) and the selected row's country, zone and offset were 3.95:1 in light; each steps up one text level. Nothing else on the page changes.
- A toast's auto-dismiss timer no longer outlives the toast (#941). It started during render and was never cleared, so a toast closed early left a timer behind (two per toast in React StrictMode). No visible change.
- **Steam's idle screensaver is off when Steam runs from the Steam image (#834).** The Steam image sets it to Disabled before Steam starts, unless you chose a timeout yourself, so a long idle stream no longer ends on Steam's slideshow. Ships with the quasar-images `stable` update that comes with this release.
- **A keyboard layout that is not a real one no longer crashes the app on its first key (#902).** A value like `zz` passes the control plane's format check; the app's own compositor failed to build it and Steam exited seven seconds after the first key. The node agent now checks the layout and variant against its keyboard data before the app starts, and a pair that is not there becomes US for the app and the session's compositor alike, with one warning (`keyboard-layout-unavailable`) in the node agent's log.
- Pressing Play on a game tile launches it when the recommended quality only needs to step down from a codec the host lacks (for example AV1 on an AMD GPU), instead of opening the detail panel with "No host that can encode this quality is available" (#817).
- **Quitting a Steam game from inside the session no longer cuts the game's cloud save (#810).** The container used to be stopped as soon as the game's process was gone, which is before Steam uploads the save, so the next launch of the title could stop on "Cloud Out of Date". The session now ends the stream as before, then waits for Steam's exit sync to finish (up to 30 s) before the container goes. A game whose exit starts no sync within 5 s is not waited for, and the agent logs `session-end-no-cloud-sync`.
- **A session end that lands in the same instant a Steam game exits now waits for its cloud save (#872).** The helper handled the end's `quit` before its next poll, so it answered "no cloud sync" at once and the container could stop under the exit upload. A quit after an exit the helper had already reported now follows the upload even when `/proc` cannot be read, instead of stalling to the 30 s bound and logging a false `session-end-cloud-sync-unfinished`.
- **Switching away from a running Steam game to an app in another container no longer cuts the game's cloud save (#809).** The old container was stopped under Steam's exit upload, so the next launch of the title could stop on "Cloud Out of Date". The switch screen now waits while the game quits and Steam's exit sync finishes (up to 30 s) before the old container goes, and the agent logs the same `session-end-*` lines as a session end.
- **A stop that races a dispatch no longer logs and audits the session as failed (#924).** When a stop (a user's, or a parked Steam session superseded by a launch) took the row before its dispatch bound the managed home, the dispatch still wrote a `session.failed` audit row, a Warn and an Error log, and stamped "managed home dispatch binding failed" on the stopped row. It now returns without touching the row; the stop finishes as it would for a session that never started.
- **A labelled switch is announced by its text instead of as a bare "checkbox" (#943).** The shared switch nested one label inside another, so a screen reader lost the row text; the Account ▸ Input switches and the other labelled ones now carry it. Nothing moves on screen.
- **A stop that lands just before a launch reaches the host now ends the session `stopped` for every kind of launch (#955).** The home bind's stopped-or-ended check ran only for apps with a managed home, so for any other app, including console and certification launches, the assign still went out; on a node agent without the home-cleanup ledger the session then ended `failed` with a false `session.failed` audit. The check now runs first for every launch.

### Security
- **The add-a-host page says to keep the agent address on `wss://` (#823).** Agent enrollment over `ws://` sends the enrollment token and receives the node secret in cleartext, and the page said nothing about it. No code or behaviour change.
- **The control plane can send HSTS on its HTTPS listener, off by default (#820).** Set `QUASAR_HSTS_MAX_AGE` (seconds) on a Compose or source install to send `Strict-Transport-Security: max-age=<n>` there; the plain HTTP port never sends it, and it names no subdomains. It is off because HSTS pins the whole host name on every port: once your browser trusts the certificate, any other `http://` service on that name becomes unreachable for the max-age. Owned installs cannot set it yet (#874).
- **The control plane warns when a reverse proxy it does not trust talks to its plain HTTP port (#821).** A request carrying `X-Forwarded-*` from a peer outside `QUASAR_TRUSTED_PROXIES` logs `forwarded-from-unlisted-peer` with that peer's address, once per peer every 10 minutes. Nothing is refused. Name that proxy in `QUASAR_TRUSTED_PROXIES` before the release that refuses sign-in over plain HTTP (#604).

### Internal
- **The web build fails when the `/app` entry grows past a budget, and tests keep account deep links preloaded and free of the loading skeleton (#949).** The `/app` entry (entry chunk, its imports and CSS, gzipped) is 150,388 B today; the build stops at 154,900 B and says how to raise it on purpose. New tests check that an account deep link is preloaded by both index.html and App, and that `/app/account` then renders without the loading skeleton. No product change.
- **Small web helpers moved out of big modules so the player entry stops pulling those modules in (#947).** The pointer-lock, raw-mouse and touch probes, the settings-search place and label helpers, the playout override, the palette trigger labels and the microphone support check each live in a small module of their own. Imports only; no behaviour change. With the on-demand pages (#948) the capture, playout and microphone code now leaves the `/app` entry, which shrinks by about 6.4 KB gzip (4%).
- Every web test now ends a settings-search reveal it started, so the reveal's 4 s poll no longer outlives the test and fails `make test-web` with an unhandled `document is not defined` (#942). No product change.
- **The control-plane `session` tests run on their own runner (#928).** They were the slowest package and ran about twice as slow sharing a runner and a database with three others; `control-plane (build + vet + test)` now gathers two jobs, and `scripts/verify/go-test-parallel.sh` takes `GTP_ONLY` / `GTP_SKIP` to choose packages.
- **Pull request CI runs only the jobs a change touches, and the Go tests run in parallel (#919).** CI now runs on every push to `develop` (every job; these runs carry the checks `main` requires and save the caches) and on pull requests into `develop`, where `scripts/verify/ci-changed-areas.sh` skips the Go, web, Rust or site jobs a change cannot affect. The control-plane tests run four packages at a time, each against its own database (`scripts/verify/go-test-parallel.sh`), on a Postgres with fsync off, uncached (#883); test reads of `pg_stat_activity` / `pg_locks` are now scoped to their own database, and the script refuses one that is not. Go is pinned to an exact patch, pull requests restore the apt and Rust caches without saving them, vitest runs in two halves behind the `web` check, the engine suite is built once for its four modes, and the uinput step reuses the built tests. The agent live tests also run on a push to `develop`, which saves a cargo cache their pull request runs restore.
- **Contract: amendment 49 pinned (#845).** Per-user Region and Input settings; behaviour lands with #846–#854.
- **Contract: amendment 48 pinned (#840).** Adds the preview release channel, its commit list and the daily/weekly release check; behaviour lands with #839 and #841–#843.
- **Region and Input settings mock (#844).** `design_handoff_v3/screens/region-input-v3.html` shows the account page's new Region section (timezone list and world map, language, region format) and Input section (keyboard layout, mouse speed and sensitivity, raw input, scroll direction, Nintendo gamepad layout), plus the admin install-wide defaults. `DESIGN.md` gains the rules for its new controls.
- **CI no longer stalls on the GStreamer apt install (#864).** The node-agent and engine-suite jobs download packages with a 20 s idle timeout, 3 retries and a restart of any download that runs past 3 minutes, restore the `.deb` set from an `actions/cache` keyed on the package list and runner image, and carry `timeout-minutes` (30 and 20) where there was none.
- **The bench peer no longer ends a long hold on Steam's screensaver (#835).** A held peer longer than about 5 minutes ended on the static slideshow Steam draws after ~310 s without input; `scripts/harness/peer-driver.mjs` now taps Left Shift at decode and then every 250 s (`IDLE_NUDGE_EVERY_S`, `0` = off), so a soak passes or fails on stream health alone (#819).
- **The Images workflow no longer spends time on a toolchain it reuses (#865).** The `inputs` job reads the gst toolchain's digest (or says it must be built), the toolchain job does nothing when it is reused so `build-node-agent` starts seconds after it instead of waiting out a disk cleanup, and the node-agent runner cleanup runs only under 50 GB free instead of on every run.
- **Rebuilding the agent image of an unchanged tree reuses the cached compile (#866).** The agent's build time no longer goes into `cargo build`: the image sets it as the `QUASAR_BUILT_AT` ENV beside the `org.quasar.built.at` label, and the agent reads it at run time. The `built_at` it reports on `register` is the same value as before.
- **An Images run builds nothing when no image input changed (#867).** The `inputs` job diffs the tree against the branch's last successful run and asks `scripts/release/images-changed.sh` whether anything the control plane, node agent or recovery actor is built from changed. If not (docs, CI, tests, `protocol/`) the run skips every build, validation and promotion and says so in its summary; if so, every image is built for that commit, so a build is always one commit. A `v*` tag, a dispatch with `force`, and a run whose gst toolchain is not published yet (a pin, a patch or the base image moved) always build everything. A table test runs in `make verify`.
- **Every landing on `develop` publishes a preview build (#839).** A push to `develop` now runs the Images workflow (a newer push cancels an older run in flight). When an image input changed it builds and promotes every image for that commit, then publishes the preview manifest, the three digests and the floor with `version` null, as a one-layer OCI artifact at `ghcr.io/accreleus/quasar/quasar-release-manifest:preview`, moving the tag only after every image it names is validated and promoted and carries that commit, and only forward: a re-run of an older landing is refused. A landing diffs against the commit the preview names, so a landing that changes no image input publishes no new preview. `generate-platform-release-manifest.sh` and `validate-platform-release-manifest.sh` gain `--preview`; without it a null version or `commits` is still refused. Because the same run promotes as an edge build does, `o2-develop` (the edge channel's default branch) now moves on every landing that changes an image, not only on demand; edge dispatches of other branches and the `v*` release are unchanged, and a dispatch with `preview` ticked publishes to a test tag, `preview-<branch>`. The new package must be public; an org owner checks that once after the first push.
- **A control-plane test no longer reads a state its own dispatch moves past (#916).** `TestAdmittedLaunchSupersedesAParkedSessionTheReadMissed` expected the superseded parked row to still be `stopping`; the heartbeat's dispatch goroutine could refuse the bind after the supersede and end it `stopped` first. It now asserts the real invariant: stopping or stopped, with the supersede's reason kept. No product change.
- **Two tests name the catalog's default ref `stable` (#931).** `TestCatalogRefDefaultsStableWhenUnseeded` and the Images tab fixture still said `main`. No behaviour change.
- **A test pins that Account ▸ Region follows the browser's language before an install default that differs (#933).** The status sentence after a language pick now names the language from the same list as the select. No visible change.
- **The `agentws` policy-offer tests read offers off the socket instead of counting database claims (#925).** `collectOffers` timed out waiting for an offer pass when the startup pass had already claimed the offer, which showed under parallel load.
- **`agentws` tests no longer run alone, and `TestAClosingConnectionDoesNotReopenAHostAReconnectClaimed` waits for the first connection's capacity write (#925).** The test closed its first connection while that connection's capacity handshake could still be waiting on the row lock the reconnect had just taken, so no close was read and the 10 s wait for `markOffline` missed (about 7% of runs, with or without load). `scripts/verify/go-test-parallel.sh` loses its run-alone list and CI's `session` runner stops carrying `agentws`. No product change.

## 0.5.0 — 2026-10-25

### Upgrading
- **This release changes the database (migrations 0104, 0105 and 0106), so there is no going back below it.** Once applied, an older control plane will not start. For Quasar's own database the way back is a restore of the dump the update takes first (`docs/upgrading.md`, "The one-way migration rule"); on your own database, back up first. Fleet ▸ Releases ▸ Update Quasar waits for live sessions to end before it replaces the control plane (or tick "Update now" to end them), and automatic updates never install a release that changes the database.
- **Then update the Steam image.** In Library ▸ Images, Sync catalog, then **Update to `2026.10.23`** on Steam. Hot Loading for Steam Apps, the loader that stays up until the game is on screen, and quitting a game back to the library need it, and Steam signs in 10–20 s sooner with it; on the older image Steam tiles launch as before. Leave Hot Loading off until the image is updated.
- **If your Steam app uses a runtime preset you made yourself,** add `STEAM_STARTUP_FLAGS=-bigpicture` to it to keep Big Picture (#724).
- **If a reverse proxy talks to the control plane's HTTP port, name it in `QUASAR_TRUSTED_PROXIES`.** First-run setup is refused over plain HTTP from now on, and sign-in over plain HTTP logs `credential-route-plaintext`; the next release refuses it (#593, #604).
- **A custom image catalog with template builds:** `COPY` paths are now relative to the repository root (#615).
- **It worked when Fleet ▸ Releases shows this release everywhere and the run ends `succeeded`,** and Library ▸ Images shows Steam on `2026.10.23`.

### Added
- **A device whose takeover the host refuses now says why, instead of waiting on a stalled stream (#566).** When this browser cannot take the session over (it refused the stream, often because it cannot decode the session's video format) or the session has moved between devices too often in the last minute, the launch screen says so at once and offers a way back to the library without stopping the session. The host sends the reason as an optional `code` on its signaling `error` (protocol amendment 42); a client that does not know it is unaffected.
- **A console monitor shared with other machines (#782).** Set `QUASAR_CONSOLE_INPUT_SOURCE` on the node agent to the monitor input the host is cabled to (its DDC/CI `0x60` value, like `0x10`). Console auto-start then treats the monitor as off while it shows another input. Unset, the monitor's power alone decides, as before.
- **Firefox can send Escape to the game (#647).** Firefox keeps Escape for itself, so in a captured session **Ctrl+Alt+Shift+E** now sends Escape to the game. The release hint and the Input pane say so, and the Browser support page lists Firefox's limits.
- **Play goes straight into the game (#647).** Clicking Play puts the browser in full screen (left again when the session ends), and when the game appears input is captured without a second click, with a line in the session strip for about 5 seconds saying how to release it on this device. Where the browser refuses (for example Firefox or Safari, Esc pressed during the loading screen, or a session opened from elsewhere) the stream shows one "Click to play" button instead. Turn it off per device in Account ▸ Stream quality ▸ Launch in full screen and capture input.
- **Hot Loading for Steam Apps: Steam game tiles start without Steam's cold start (#759).** With Settings ▸ Library sources ▸ Hot Loading for Steam Apps on (off by default), opening the library keeps a hidden Steam running for you (`-silent`, for `steam_warm_idle_minutes`, 10 by default). A Steam game launch takes it over and swaps the game in, and quitting a game keeps Steam hot-loaded for the next one. Your launch always wins: a real launch elsewhere, or a host that runs short of room, stops the hidden Steam first. The hidden Steam is never shown in your session list, history or quota. It updates your games in the background. Turning the switch off stops every hidden Steam within a minute, and launches behave as before. Migration 0105 (`sessions.kind`, `parked_until`, `launch_progress`, `adopt_session_id`).
- **Account ▸ Streaming ▸ Steam dialogs (#759).** `Dismiss safe` lets the launch helper answer Steam's controller notice for you with one Return, only when it is the one thing on screen and Steam's own window has focus. Sign-in, Steam Guard, cloud conflicts, the EULA and anything unrecognised are always shown. The default is `Show`.
- **Quitting a Steam game from inside it ends the session and takes the browser back to the library (#726).** When the launch helper reports the game exited after it ran, the agent says `game exited` on the session row as soon as it starts stopping, and the session page goes to the library instead of showing the recovery banner; a swap that closes the game itself is not read as a quit. Every other stop is unchanged.
- **Steam launch detection core (#730).** A new GStreamer-free `quasar-launch-watch` crate in the node-agent workspace turns the `reaper SteamLaunch AppId=<id>` process tree, Steam's `console_log.txt` and a mapped-window list into launch-state, game-window and Steam-needs-user reports for the in-container launch helper.
- **The library and game pages show genres, credits, logos and your play history (#720).** Each title carries when it was added, and, with Settings ▸ Libraries ▸ App details lookup on, its genres, developer, publisher, release date and controller support from the Steam store. With a SteamGridDB key, titles also get a transparent logo and a small icon, and existing titles pick theirs up on a later artwork sweep. Your own play time, session count and last played come from a new `GET /v1/me/play-history`. The redesigned library uses them for its Genre menu and its Recently played, Most played and Recently added sorts.
- **The library is easier to move through (#714).** A library of 60 or more titles is split into letter sections with an A–Z index down the right edge. Search ranks results and forgives typos: the top bar shows the best matches under the box (Enter opens the first one's page) and Ctrl+K lists the games first. Account ▸ Appearance ▸ Classic layout brings back the previous library and search.
- **Steam game tiles report their launch state (#731).** For a Steam game tile the node agent hands the Steam container a small report-only launch helper and a per-session socket; `session_metrics.app_launch_state` and the `app.launch_state` trace event then follow the game through `starting`, `client_only`, `game_running` and `game_exited`. The socket is treated as hostile input (bounded lines and rate, one connection, strict parse) and nothing on it can end or change a session. The agent image ships the helper as a static binary; the Steam image starts it when it is mounted.
- **Account ▸ Appearance has a Classic layout switch (#714).** Off by default, so you get the redesigned pages; turning it on keeps the previous layout. It covers the library, the game page and the account pages. Once Classic layout is on, the switch is on Account ▸ Preferences ▸ Appearance. The choice is saved with your account, so it follows you to every device.
- **The new design gives the account area one scrolling page and one search in the top bar (#714).** Account settings sit on a single page with a link per section, sections edit in place, and devices are grouped as in use or not used in 90 days with a Revoke button named after each device. The Classic layout switch has its own Appearance section there. The search in the top bar also finds account settings, devices and (for admins, in the console) admin pages and settings; choosing one takes you to it, focused and highlighted. Turn on Classic layout (Account ▸ Appearance) to keep the tabbed account pages and the previous search.

### Changed
- **The contract's image wording matches what install, update, uninstall and discovery do (protocol amendment 45, #690).** Wording only.
- **A quick switch between two games from one Steam library swaps the game in place, with no new Steam (#759).** The switch screen stays up until the new game is on screen, holds the stream's sound while it is up, and shows Steam's shader progress when Steam reports some. Otherwise it shows a rotating retro-game line (`web/src/pages/app/getting-ready-phrases.txt`, one per line). The launch loader shows the same line.
- **The contracts describe Hot Loading (protocol amendment 39, #759).** Additive: `kind` and `adopt_session_id` on `session_assign`, `runtime_held` on `stopped`, `launch_progress`, `PUT /v1/me/steam-warm`, `steam_dialogs` on profile preferences and three instance settings. A refused Hot Loading claim, or a home still held by your own hot-loaded or released Steam, answers `503 capacity_exhausted` with `Retry-After: 2`, and a managed-home hold moves to a session only once the agent has adopted its runtime.
- **The browser plays a session's sound from the reveal, not before (#727).** Sound that arrives while the loader is up, such as Steam's startup sound before the game appears, is muted and unmutes when the loader turns see-through, with the picture. The native client and console mode are unchanged.
- **A Steam game tile keeps the loader up until the game is on screen (#727).** The stream is shown when the launch helper reports the game on screen or Steam needing you (a sign-in, a cloud conflict), or at most 90 s after Steam's first frame (`QUASAR_STEAM_REVEAL_CAP_SECS`; `0` turns the wait off). A swap during the wait shows the stream. Other apps, the plain Steam app, warm-ups and a Steam image without the launch helper show their first frame as before. The helper now sees Steam's sign-in screen from Steam's login log (under Big Picture it has no window of its own), no longer reports Steam as needing you for a routine wait it leaves within 2 s, and follows Steam's log through a rotation.
- **Steam's Big Picture flag now comes from the Steam runtime preset, not the Steam image (#724).** If your Steam app uses a runtime preset you made yourself, add `STEAM_STARTUP_FLAGS=-bigpicture` to it to keep Big Picture on the plain Steam app.
- **Opening a game now shows its own page, and Home has Favourites and Jump back in shelves (#714).** A tile, a featured-rail card, or a game chosen in the top-bar search or the Ctrl+K palette opens `/app/game/<id>`: the art, Play, what Play will ask for, the favourite heart, and the game's description below. Play there starts what the server would start for a tile's Play (the app's own setting, your default or the recommendation); where that setting cannot stream to this device and you may choose, Play starts the recommendation, as the detail band does. Change opens on what the server would start, or on the recommendation where that cannot stream here, and reads your settings again as it opens (its Play waits for that read): a different quality chosen there is sent as the detail band sends it, and nothing changed starts what Play starts. Where an admin has disabled profile overrides, or fixed the game's settings to one this device cannot use, Change is not offered and the page says which; a fixed setting that streams here keeps Change for the codec. If your stream settings do not load, the page says so with Retry, and Play lets the server choose. A game chosen in search or the palette arrives with focus on Play, so `P` plays it. Every library grid tile shows a resting favourite heart and its title, and `F` on a focused grid tile toggles the favourite; the shelf tiles have neither. Turn on Classic layout (Account ▸ Appearance) to keep the detail band: a game-page link then opens that game's band on Home.
- **The media host check now fails on a picture whose pixels were moved, not only on one whose colours were damaged (#289).** `media_probe_gpu<N>` used to encode a black field, on which a displaced block looks the same as the block it replaced. The compositor now has a test picture, a fixed grid of four colours shown only while no app is on screen, and the check compares its own decoded output with it. Nothing to configure: the check id, what it blocks and its wording are unchanged, and a session never shows the picture.
- **The compositor pin carries follow-ups to the #267 and #570 fixes (#575).** The gst-wayland-display fork pin moves `69438183` → `5910a1ac`. A test now fails on any Vulkan device table built the way that caused the #267 crash, the loader race test says whether it ran, and the EGL display handle kept for #570 is checked to be `Send` at compile time. Keeping the EGL display for the life of the agent was tried and dropped: on the NVIDIA lab host the next session found it terminated and failed to start.
- **The contracts now say what happens when the host cannot apply a client's answer or the client fails the fingerprint check, and what `sessions.device_id` is read for (protocol amendments 30 and 31).** `protocol/signaling.md` has a new section after "The answer's `a=fingerprint`" (#577): on the connection a session launched with, an answer the host cannot apply ends the session; on one that replaced it, the session keeps running and that client gets no media on that connection. A client that fails the fingerprint check is told nothing; the check is made again only on a new connection, and an answer must name `sha-1`, `sha-256` or `sha-512`. `protocol/schema.md` now says the control plane reads `sessions.device_id` at launch, to use the launching device's capability probe and decode history (#203); it used to call the column a read-only link. Wording only: no message, field, column or migration changes.
- **The contract now lets a device refuse H.264 (protocol amendment 35, #605).** `protocol/control-api.md` said every browser decodes H.264. It now says an H.264 rung is refused only when the device's fresh probe says `codecs.h264: false` (absent, stale or not a boolean still accepts it), that the session then moves onto another codec the device decodes, and that the H.264 floor still launches when nothing else is left. `GET /v1/me/profiles` reports such a rung as `codec_not_supported`. `protocol/schema.md` says what `codecs.h264` is. No message, field, reason code or migration changes.
- **The contract now says what installing and uninstalling an app image does (protocol amendment 36, #658).** `protocol/control-api.md` and `openapi.yaml` said an install pulls the image on every connected host and an uninstall removes it from every host that has it. A non-lazy install prepares the image only on the hosts an enabled app using it is placed on, and an uninstall drops the adoption and removes nothing from a host: the cached image stays until the host image cleanup removes it. Wording only; no message, field or status changes.
- **The vendored bench client is re-vendored to quasar-bench 1.10.0 (`bench.py` and `qbench`, from the same commit).** It was 1.7.0, so `bench post` warned that the server had a newer client. Only the vendored copy changes; the harness code that calls it is untouched.
- **The golden-home warm-up sets Steam's Big Picture flag itself (#724).** Steam's UI mode is moving out of the Steam image, so the warm-up no longer relies on the image's default; it starts Big Picture the same way as before.
- **The in-stream game switcher, the search icon and the game page's related tiles ask for the artwork size they draw (#734).** The switcher's covers were full size on every screen; they now download a 320 to 560 px copy by screen density, and the search icon and related tiles pick a size for their box and pixel density. Layout is unchanged.
- **The Steam launch detection code is tidier and its tests no longer sleep (#645).** Internal refactor with no change to what is detected: the launch helper's core reads Steam's console and sign-in logs through one tail (the sign-in log now shares the console log's 64 KiB line cap), takes the clock as an argument so the wait-for-user tests step time instead of sleeping 2.1 s seven times, and the agent's launch helper module exposes less. `CONTEXT.md` gains the launch helper, launch state, reveal, reveal cap and hold.

### Fixed
- **The console note no longer says a node agent is being replaced while it has only not yet heard from its recovery actor (#797).** For up to five minutes after an agent starts it reports console access as `applying` with no target, because it has not read the actor's console status; the host page read that as "Turning off console mode" with a replacement under way. It now shows "Waiting to hear from the recovery actor." followed on the same line by the agent's own sentence.
- **Ending a Steam game session while the game runs no longer cuts the game's cloud save (#742).** Any such end (the player's Stop, a closed tab, a revoke or an idle reap) stopped the container under Steam's exit upload, and the next launch of the title anywhere stopped on Steam's "Cloud Out of Date" dialog. The node agent now ends the client's picture, sound and input first, then quits the game the way a player would and waits, up to 30 s, for Steam to finish the sync before the container goes; the session reads `stopping` meanwhile. A game whose exit starts no sync within 5 s is not waited for. Past 30 s the agent tears down as before and logs `session-end-cloud-sync-unfinished`. Quitting the game from inside the session (#726, #753) does not wait for the sync yet (#810).
- **A game exit no longer flashes a "Recovering connection" banner before the library (#748).** The stream stalling a beat before the page learned the session was ending painted the banner for a few frames. The page now holds the banner half a second and never shows it once it knows the game has exited or the player pressed Stop; a real stall still shows it.
- **Steam signs in 10–20 s sooner (#741).** Inside a session container NetworkManager never marks the container's network as the default route, so Steam waited out its fixed network timeout (10 s, or 20 s for a hot-loaded Steam) before signing in. The Steam image (`2026.10.23`) no longer starts the system bus and NetworkManager, so Steam no longer waits for the network: from launch to signed in now takes about 6–9 s on the lab GPU hosts, down from 16–26 s. Big Picture's network icon now shows no network; Steam is online and works as before.
- **A launching app is no longer revealed on a black stream (#749).** The node agent reported `running` and, about 20 ms later, `app booting`; a browser poll landing between the two took the session for one with nothing left to wait for and dropped the loader. A launch that boots an app now reports `running` as `app booting` in one update (protocol amendment 46).
- **A console session left running on a switched-off monitor now stops (#782).** While a console desktop drives the display the agent did not read the monitor's power, so an auto-started session on a monitor in standby ran on, holding the GPU and the user's Steam. Once the console has had no keyboard, mouse or controller input for 10 minutes, the agent now reads the monitor's power every 5 minutes; a monitor that reads off stops the session 10 minutes later (`console_display_disconnected`), even with `QUASAR_CONSOLE_DISCONNECT_GRACE` unset (protocol amendment 47). A game with recent input never sees the read, and a monitor switched back on and used before the stop keeps the session. A console session that outlived a control-plane restart is now tracked too, so it is stopped the same way and no second one is launched beside it.
- **Two overlapping saves of display preferences can no longer lose a setting (#716).** `PATCH /v1/me/ui-preferences` read the stored preferences, merged in memory and wrote them back, so when two requests overlapped (two devices, or two quick toggles) the later write erased the other's change. The merge now runs in the database in one statement; what is merged and what is replaced is unchanged.
- **Play on a Steam game before Steam has ever been started now says what to do (#750).** The toast reads "Start Steam once first", stays up longer and offers an Open Steam button; it is shown once and never retried as a wait.
- **A stale `seed.json` after a recovery-actor update now heals (#561).** A hand-over whose `seed.json` write failed left the file on the previous actor image, and removing every actor container then brought that older actor back. The verified actor now writes the file again at every start. An older actor continuing an update step it cannot render is not fixed here (#794); `docs/upgrading.md` says what to do.
- **A node agent restarted while its recovery actor is dark now registers about 14 s after it starts, down from 27-30 s (#687).** The agent waited for the actor twice before dialling, 8 s for its install facts and 10 s for the console state, 18 s in all after a startup wait of the same kind. Each connection now spends one 5 s budget on the two reads together; the single startup read before the first connection waits its own 5 s. The host registers as an owned install whose actor does not answer, and reports the actor once it answers; an actor that answers slower than the budget makes the agent reconnect once, not repeatedly. Until the actor answers (for about the first 5 minutes after the agent starts, at most 6) the host takes no new sessions and console mode cannot be changed, so a replacement the agent cannot see is never placed onto; after that it is placeable as before. The log names the actor (`register-actor-dark`) instead of blaming the container runtime.
- **A second client's malformed answer can no longer end a session the first client is streaming (#591, #566).** An answer with no ICE ufrag or no DTLS fingerprint, sent after `restart_ice`, was applied to the transport the first client streams on and failed the session. The host now drops it without telling the client (protocol amendment 43). The log lines around an abandoned takeover also describe the session: no ERROR before the "takeover abandoned" warning, and the idle-reap lines say whether the session had streamed.
- **No HTTPS advice on a page that is already secure (#631).** In a browser with no Keyboard Lock (Safari), the Input pane and the browser console now say the browser keeps Esc and that Chrome or Edge can pass it to the game, instead of telling you to use HTTPS. Plain HTTP keeps the HTTPS advice.
- **A container engine that hangs no longer locks launches and image operations out of the node agent (#691, #574).** A call that never gets an answer now frees its slot after 3 s and keeps running under its own deadline, instead of holding it for up to 30 minutes. An image pull, build or removal refused for lack of a free slot asks again rather than ending `failed` (which the control plane counted as one of its 3 attempts), a launch's container steps wait up to 5 s for a slot instead of failing the session, and a ready image the engine lost is reported `absent` again if the first message is dropped.
- **A user's default profile no longer applies while user overrides are off (#721).** Play, and a launch with no profile of a game with no pinned profile, now start the global default (then the recommendation), as the control API says; the stored default is kept and applies again if an admin turns overrides back on. The game page follows the same rule.
- **An app can no longer be stored with zero encode slots (#784).** Migration 0106 sets `default_encode_slots` to 1 on any app that held 0 or less (apps created before the create path stopped zeroing omitted fields) and adds a CHECK that it stays above 0. Without it, on a console host a streamed session of such an app was mistaken for a console session by console self-heal.
- **A Firefox session now earns a client health class (#686).** Firefox reports `mediaTime` 0 on every WebRTC frame, and the present-cadence window dropped each one as a repeat of the last, so `present_n` was 0 and no class was posted. The window now tells Firefox frames apart by their RTP timestamp; Chrome's numbers are unchanged.
- **A launch whose saves are on an offline host now says so instead of waiting (#602, protocol amendment 40).** The refusal carries `error.reason: home_host_unavailable`; the browser skips the 20 s quiet retry and shows "Your saves are on an offline host", since only an admin can bring the host back.
- **A Firefox user who installs the OpenH264 plugin gets H.264 rungs back after one reload (#688).** The browser posted its device record once a day, so the library kept showing H.264 rungs as unplayable until the next post. It now posts again on page load when its H.264 verdict differs from the last one posted.
- **Stopping an auto-started console session now sticks (#782).** Console self-heal relaunched a stopped session within a millisecond, using the display report made while that session was on screen, when the agent does not read the monitor's power until the console has had no input for 10 minutes. It now waits for the agent's next report, so a stop with the monitor off holds. A session its user stops is not auto-started again until the monitor goes off and on. Before, a console session left running on a switched-off monitor held the user's Steam, and every Steam game in their library read "In use".
- **A node agent restart no longer starts console mode on a switched-off monitor (#782).** Until the control plane's settings arrived, the agent reported every plugged-in monitor as on.
- **The browser and session docs no longer contradict themselves about Firefox (#647).** Browser support and Requirements say Firefox works with its listed limits, and the session page lists the cases that show Click to play.
- **Quitting a Steam game returns the browser to the library at once (#753).** The page used to stay on the stream's last picture, often Steam's own window, for 2–4 s: the agent had already stopped the stream, but the page only found out when its 5 s status poll came round or its stall detector gave up on the silent stream, and that detector flashed the "Reconnecting" banner on the way out (#748). The agent now sends the signaling `bye` as soon as it marks the session `stopping` / `game exited`, and the page reads the session then. Steam's shutdown follows behind it, and its exit cloud upload is not waited for on this path (#810). A session that is already stopping is no longer marked failed when its browser's socket is too slow to take a signaling message.
- **Launching right after a game quit now waits for the previous session to close instead of being refused (#754).** While your last session is still stopping (Steam finishing its cloud upload), the new launch shows "Closing <game>…" and starts the moment it has finished, for up to 45 s; a stop that never finishes ends in a message saying so, and a session that is really running is refused as before.
- **A Steam game tile shows the stream when Steam stops on its controller notice (#755).** The launch helper took Steam's `ShowInterstitials` wait for routine and never reported it, so the loader stayed up until the 90 s cap (84 s on a measured launch) while Steam waited for you to answer. A wait that stands for more than 2 s now shows the stream so you can answer it. Steam's shader step is unchanged: the loader stays up for it, up to the cap. Nothing clicks a dialog for you.
- **A library scan no longer fails forever over a game name with bad bytes in it (#738).** A Steam manifest name containing a NUL byte made the scan's database write fail, which rolled the whole scan back and failed every later scan the same way, so the library never updated. The reported text is cleaned at the scan report's edge — the same repair the app-details store texts get — and a name left empty still falls back to the app ID. A scan's own failure message is cleaned too, and cut to its length cap without splitting a character.
- **An app's first artwork resolve can no longer overwrite an admin's artwork choice made while it was running (#739).** The first resolve of an app used to save with an unguarded upsert, so a pick or upload that landed while it was fetching from the provider was silently replaced. The resolve now writes only when no artwork row exists, stands aside when someone else wrote one in the meantime, and no longer touches the app's image links in that case.
- **Sized artwork copies (`?w=`) now come out upright for photos stored sideways (EXIF orientation) and keep the original's colours (an RGB colour profile travels with the copy), so tiles match the detail band (#713).** A control-plane kill mid-write no longer leaves a `.tmp-*` file in the artwork cache forever: the art sweep removes ones older than ten minutes (#713). When cached images cannot be checked, the art sweep logs one warning per pass instead of one per app (#734).
- **Artwork thumbnails no longer download full-size covers (#707).** `GET /v1/artwork/{asset}` takes an optional `?w=` (96, 320, 480 or 560; protocol amendment 37) and serves a copy scaled to cover a 2:3 frame that wide, made on first request and cached beside the original. The original is served instead when no copy would be smaller in bytes, and, briefly cached, when a copy cannot be made. The admin Apps table and the search results ask for 96; library tiles, Home shelf tiles, rail cards and the game page's related games pick a width by their size and the device pixel ratio, in both layouts. A full scroll of 1,466 covers drops from about 925 MB to about 8 MB in the admin table and about 61 MB (1×) to 156 MB (3×) in the library. Hero and detail art is unchanged.
- **Ctrl/Cmd/Alt+P and a held P on a focused library tile or featured-rail card no longer launch the game, and a launch starts once however fast Play is pressed (#714).** Ctrl/Cmd+P opens the browser's print dialog again. This holds with Classic layout on too.
- **Opening a featured-rail card for an app the kind filter hides now shows its detail band (#708).** The card reset the filter to All, but a second effect that closes the band on any filter change ran in the same render and closed the band that had just opened. The band now closes only when the filter, search or view changes after it opened. The rail art also no longer logs React's `fetchpriority` warning in development.
- **The setup wizard no longer shows an image as ready when its build failed on some hosts (#705).** The Libraries step summarised a provider's image from the catalog flags alone, which never see a per-host failure, so a partly failed install read "Ready". It now uses the same per-host rollup as Library ▸ Images and shows "Failed on N hosts" beside the chip, even while another host is still building. The step keeps checking after a failure, so a build the control plane retries successfully turns Ready on its own.
- **Repeated controls in lists now say which item they act on (#706).** Each device's trust switch, each session's Resume and End on Account ▸ Sessions, and each grant's Revoke on an app's Access tab shared one accessible name per row, so a screen reader heard the same name several times in a row. The visible label is unchanged and each name now starts with it and adds the item's name ("Resume Portal 2", "Revoke alice"). `Switch` now honours an `aria-label` given beside a visible label.
- **The player library no longer mounts every tile (#700).** Home and Library rendered all of a large library's tiles at once: with 1,500 titles, 13,741 DOM nodes and 1,473 images after load. The grid now mounts only the rows near the viewport (about 860 nodes at load) and keeps spacers for the rest, so the page height is unchanged. Scrolling, arrow-key and Tab navigation, and the open detail band (including one opened from a featured-rail card) work as before. The browser's find-in-page and a screen reader's browse mode now see only the titles mounted at that moment.
- **An image whose build failed on some hosts no longer reads "up to date" (#693).** Library ▸ Images and the image page's Rollout card summarised the image from the catalog flags alone, so a build that failed on 3 of 4 hosts still said "up to date". Both now say "Failed on 3 of 4 hosts" in the danger tone, counting only hosts still in the fleet (and just "Failed on 3 hosts" until the fleet has loaded), and the image page shows no version on a host where the build failed.
- **The Home detail band is readable over light artwork, and on a phone its Play button is no longer hidden under the tab bar (#692).** The band mixed its backdrop toward white over light art (GRIS, Counter-Strike) while its text stays light; it now always darkens toward black, and the band's danger ink (the fatal-codec note, the pressed heart) no longer turns dark red in the light theme. Opening the band on a phone scrolled it flush with the screen bottom, behind the tab bar; it now stops clear of the tab bar (home-indicator gutter included) and the 64 px top bar.
- **The admin Apps table no longer downloads every app's artwork on load (#702).** Each row's 26 px cover loaded eagerly, so with 1,500 apps the page requested about 890 MB of full-size art before anything was scrolled. Covers now load as their rows come into view (about 38 MB at load).
- **Each device's Revoke button on Account ▸ Devices now says which device it revokes (#698).** Screen readers announced every card's button as just "Revoke"; it is now "Revoke <device name>". The visible label is unchanged.
- **A warning or info line under a table row now shows in its colour (#695).** A table's grey caption style out-ranked the tone class, so a library image that is behind read "running 1.20.0" in grey, and the Storage tab's "Host deleted" never showed amber. The tone now wins inside a table.
- **A Firefox session now sends its stats (#610).** Firefox reports no RTP header extensions for the receiver, and the page read that list without checking, so every stats poll failed and the session sent no samples. Its health stayed `healthy` and no decode history was recorded. The page now treats a missing list as "no extension", and Firefox sends samples and updates its stats overlay. A window Firefox cannot measure (no freeze count and too few presented frames) reports no health class, and the control plane ends the client-health run on it, so it cannot add up to a pass.
- **Seven more request bodies are now size-limited (#655, follow-up to #622).** The signed-in user's UI-preferences edit, the app placement update, and the fleet apply, host apply, revert, developer apply and remove-host requests read at most 1 MiB; a larger body is refused with a 400 instead of being read in full.
- **A running agent no longer reports an image ready after it was removed from the engine by hand (#675).** An ensure for an image the agent holds as ready now inspects the tag once; if the engine no longer has it, the agent pulls it again instead of waiting for a restart to notice.
- **A compositor that runs out of memory while taking the handle on its picture now fails the launch instead of streaming an unchecked handle (#680).** Since #617 the compositor reads each new handle back where the driver allows it, and takes one without read-back where the driver refuses that. Any failure used to count as a refusal; now running out of memory fails the launch, as it did before #617, and only a refusal falls back.
- **An NVIDIA session no longer streams every second frame black after a driver fault at its start (#617).** In about one NVIDIA session in 25 the GPU driver failed to map the compositor's second handle on its picture while reporting success; that handle read as black, so half the frames were black for the whole session and nothing reported it. The compositor now takes one handle wherever its frames are converted one at a time (every Vulkan-encode session, and every NVIDIA session), reads each new handle back where the driver allows it, and takes a new one if it reads empty (`vulkan-import-reads-nothing` in the node agent's log; `vulkan-import-checked` logs every check). If three in a row read empty the session fails at launch instead of streaming a broken picture.
- **Opening a different session in the session view starts it clean (#560).** The session page kept the previous session's state when only the id in the URL changed (a launch-refused toast's "Go to session", or browser back/forward), so the next session got no launch loader and could show the earlier session's takeover or access-removed banner. The page now remounts per session id.
- **Artwork fetching is bounded.** A search thumbnail that redirects off the provider's hosts is refused, at most four artwork fetches run at once, an image over 16 megapixels (or with an unreadable header) is refused before it is stored, and a failed fetch logs the host and cause instead of the full URL (#623).
- **An image record the agent can no longer use no longer blocks the image (#608, #609).** A build interrupted before an engine-socket change (Docker to Podman, rootful to rootless) left a record that failed every later build, launch preparation and removal of that tag with "outcome unknown", and an empty or truncated record file left by an agent killed while writing it failed every pull or build of its image. The next operation on the image, or the startup sweep for an empty or truncated file, now retires such a record (logged as `image-build-intent-retired` or `image-record-unreadable-retired`) and carries on. A removal recorded against another socket is still refused.
- **The developer-apply registry reader now caps what it reads from a registry (#628).** The reader for registries an operator names in `QUASAR_PLATFORM_INSECURE_REGISTRIES` used a plain HTTP client with no body limit, so a hostile or broken test registry could stream an unbounded token or error body into memory (30 s at most). It now stops at 1 MiB, as the production resolver does.
- **The audit feed records only real changes to a user, and a long agent-reported reason no longer drops the console-access-restored record.** A user PATCH carrying a role, quota or disabled value the account already had left a `user.role_changed`, `user.quota_changed` or `user.enabled`/`user.disabled` row; now it leaves none. The `reason` and `request_id` on `console.access.restored` are cut to 200 bytes, so an over-long one no longer fails the audit insert while the reset still happens (#624).
- **A launch no longer fails because the node agent's engine slots are all busy for an instant (#574).** The agent runs at most four engine operations at once and refused a launch's image preparation outright when all four were taken, failing the session with `image preparation failed`. The launch now asks again for up to 5 seconds, and ends at once if it is stopped; each refusal it retries is logged as `launch-image-busy`. What held the other slots is still unknown.
- **A catalog app built from a template now gets the repository root as its build context (#615).** The control plane sent the Dockerfile's own folder, while the catalog repository writes every Dockerfile for the root (`COPY images/quasar-benchapp/...`), so those builds failed with "file not found in build context". `quasar-probe` and `unigine-bench` build from a catalog template (the image catalog's `unigine-bench` now builds without `RUN --mount`, from a published, digest-pinned base). `benchapp` was removed from the catalog, because its payload image comes from a private repository that no host can pull. A custom catalog must know four things: `COPY` paths are now relative to the repository root; only the `.dockerignore` at the root applies; a symlink or hardlink anywhere in the catalog repository now fails its template builds; and template builds need the control plane and the node agent both on this build, so a host whose agent is still older fails them at extraction until its update step runs.
- **Safari is now recommended 120 fps like Chrome and Firefox (#632).** Safari holds `requestAnimationFrame` near 60 Hz even on a 120 Hz Mac, so the device record said the screen was 60 Hz and the launch picker warned "Your screen refreshes slower than this frame rate" on every 90 and 120 fps option. A rate near 60 measured in Safari (and the other iOS browsers, which are Safari underneath) is now left out, and the control plane treats a Safari screen with no rate as able to show high-refresh options (protocol amendment 34), so Safari gets the same 120 fps recommendation as Chrome and Firefox when the connection allows it. A Safari screen that really is 60 Hz is offered it too, and by default recommended it. Each browser sends its probe again on its first page load after the update, so a signed-in Safari picks this up without signing in again. Chrome and Firefox on a computer are unchanged.
- **A node agent replaced by an update now counts as a restart (#586).** After a fleet update the Hosts page kept the agent's old restart count and "connected since" date, because a recreate takes under 15 seconds and was read as a network blip. A reconnect that reports a different agent version or commit than the host last had is now a restart whatever the gap. A restart on the same build inside 15 seconds, such as a hand-restarted container, is still read as a blip. No wire or schema change.
- **An app image whose build was interrupted is built again (#505).** Replacing the node agent while it built a catalog image left that build's record behind, and every later build of the image failed at once with "image operation outcome unknown", which hid the build's real result. The next build now checks the engine: a build that finished is used, anything else is built again, and a build that then fails shows its own error. A build that ran out of time is settled the same way.
- **The host now says so when a client fails the DTLS fingerprint check (#577).** A client whose answer does not name the certificate it presents in DTLS is sent video and sound, and nothing it sends is accepted: no input, no microphone. The node agent's log used to look healthy. It now has one warning for each of the session's two connections (video and sound), `token="transport-peer-fingerprint-mismatch"`, with the session id. The session is not ended and nothing changes for browsers, which always pass; this is for clients that write their own answer (`protocol/signaling.md`, "The answer's `a=fingerprint`").
- **Restore and uninstall messages name commands that work (#589).** The restore's "already created" refusal prints an `uninstall --purge` that runs in its own container, spelled for the engine (its CLI, the socket the install recorded, `--security-opt label=disable`), instead of a `docker exec` the product refuses; `status` on an install held for a restore says it is waiting for one and shows the restore command, not a restart that does nothing; `restore` tells the operator to read `status` if the recovery actor goes away; the Podman copy-out hint and the other `docker` hints in `uninstall` use `podman`; the API's refusal message for a launch whose home is claimed by an offline or gone host says so. The moving guide says what each `status` outcome means.
- **Fleet ▸ Releases counts a host already on the newest release as done (#587).** The Targets card's Node agents read "0/2" with a warning bar when every host was up to date, because it counted only the hosts still eligible to update. It now counts the up-to-date hosts (2/2, success bar), and a host ineligible for another reason is not counted. Up-to-date hosts are no longer listed under the count.
- **A job whose interval is longer than run retention no longer reruns at once (#559).** Pruning kept no terminal run once it was older than the retention window, and a scheduled run is already that old when it finishes, so the schedule lost its anchor and the next run was dated now. The newest terminal run of each job and target is now kept whatever its age.
- **A Firefox without its OpenH264 plugin now reports that it cannot decode H.264, and a launch it refuses says so (#565).** Such a Firefox lists H.264 as supported and then refuses the video, so its device record was wrong and the launch failed with a generic message. On Firefox the device record now carries the browser's own answer to an H.264 offer (a local test, a few milliseconds, once per page), and a launch the browser refuses reads "This browser cannot play the stream", with the plugin to turn on in Firefox. The library does not block play on this, on purpose. Chrome and Safari are unchanged.
- **A browser that cannot decode H.264 can play a launch profile through another codec it decodes (#605).** The library disabled Play on every profile and said "Without H.264 this device cannot play any stream" when the browser's codec list had no H.264 (such as Firefox 142 without its OpenH264 plugin), even on a profile with an AV1 or HEVC rung it plays. Auto now plays a profile whose first available rung this browser decodes. A profile that lists H.264 first keeps that message on Auto, and its AV1 or HEVC option can be picked in Adjust.
- **A session that ends no longer logs an audio fault (#588).** Every streamed session logged `audio pipeline error from … pulsesrc …: Disconnected: Connection terminated` as it ended, because its sound server is stopped while the host is still capturing from it. A session that is ending no longer reports that. The same line on a session that is still running means what it says: that session lost its sound server.
- **A takeover no longer stalls the session when its sound server stops answering (#613).** When another browser took a session over, the node agent stopped the old audio capture and started the new one on the session's own thread, and each waits on the sound server: a frozen one held the session for 30 s or more, during which it could not be stopped, swapped or reaped. Both now run beside the session, which waits at most 2 s; past that the new browser gets video and no sound.
- **A launch from a browser that cannot decode H.264 now uses another codec it decodes (#605).** A Firefox without its OpenH264 plugin says it cannot decode H.264 (#565), but the control plane ignored that: Auto on a profile that lists H.264 first launched H.264, and the stream failed with "cannot play the stream". The launch now skips H.264 for that device and takes the profile's AV1 or HEVC option when the device decodes it and a host can encode it; with nothing else it still launches H.264, as before. The launch picker marks H.264 options as not supported on that device. A plugin installed later is seen when the browser next sends its device record (at sign-in, or after a day).

### Security
- **Sign-in, registration and password change now log a warning when they are served over plain HTTP (#604); the next release refuses them.** `POST /v1/auth/login`, `POST /v1/auth/register` and `POST /v1/me/password` still work exactly as before, but a request that is neither on the HTTPS listener nor forwarded with `X-Forwarded-Proto: https` by a proxy named in `QUASAR_TRUSTED_PROXIES` logs `credential-route-plaintext` with the route and source address (about 10 a minute per source; never the email or the body). With `QUASAR_TLS=off` and no `QUASAR_TRUSTED_PROXIES`, boot logs one warning that the next release will refuse sign-in. If a proxy reaches the control plane's HTTP port, name it in `QUASAR_TRUSTED_PROXIES` (or point it at the HTTPS port) before upgrading.
- **A host's own report of success no longer closes its update (#619).** It used to, so a faulty or compromised agent could show its own update as done in the console and in a fleet run. A host update now succeeds when the new agent registers on the requested commit, as the contract says. The host's report still decides the cases a register cannot prove, and only when it echoes the requested digests: a request that moves only the recovery actor, a developer apply of the commit the host already ran, and a revert to a build no known release names. After a control-plane restart the control plane no longer knows which commit a host ran before an open developer apply, so that apply waits for a register of its commit; if the host already ran that commit, its next register closes the apply as succeeded whatever the outcome. No wire or schema change.
- **A host whose agent has disconnected is no longer offered sessions (#626).** Cancelling a waiting idle apply set the host back online from a connection check made before the cancel took effect, so an agent that disconnected in between left its host online and launches could be placed on it. The cancel now reads the recorded disconnect, and placement skips any host with one whatever its status says. No wire or schema change.
- **A host whose agent has disconnected no longer comes back online when its last hold is released, and a restart-scope attempt keeps its hold on the live report path (#656).** An uncordon, a platform run's release or restore, an idle-apply expiry or completion, a settings edit, a hardware report and the reconnect journal each set a draining host online from a connection check made earlier, or with none, so the Hosts page could show a disconnected host as online; the app placement view's ready column and the launch codec menu counted it too. They now read the recorded disconnect and set such a host offline. An `uncertain` attempt reported live keeps the host restricted even when its hold was lost, an unfinished attempt rebuilt from the reconnect journal gets its hold back, and a live report can no longer restart an attempt the journal closed as never started. No wire or schema change.
- **The debug listener now refuses requests a browser could send (#625).** The loopback profiling listener (`QUASAR_PPROF_ADDR`) armed mutex and block profiling and served pprof to any request that reached it, so a control plane in the host network namespace could be driven by a web page open on that machine (a cross-site POST, an `<img>` or navigation GET that starts a profile, or DNS rebinding). It now answers `403` to any request with an `Origin` or `Sec-Fetch-Site` header, or a `Host` that is not `127.0.0.1`, `[::1]` or `localhost` (the IPv4-mapped `[::ffff:127.0.0.1]` is refused too). `wget`, `curl` and `go tool pprof` from a shell on the host send none of these, so the recipes in `docs/configuration.md` work unchanged. A browser too old to send `Sec-Fetch-Site` or `Origin` (Chrome before 76, Firefox before 90, Safari before 16.4) is not stopped. No wire, schema or setting change.
- **An enrollment token can no longer replace a connected host's secret after a control-plane restart (#596).** Enrolling onto a host whose agent is connected is refused. After a control-plane restart, until a host's agent had reconnected, a token made for that host's name was accepted and locked the running agent out. That was every host after a restart, not only drained ones. A host now counts as connected until its disconnect is recorded. The second of two simultaneous first enrollments of one name is refused too. A host that never comes back after a restart keeps its name: re-add it under another name, or forget it first with the admin API (`DELETE /v1/hosts/{id}`), which deletes its history and tombstones its homes. The console cannot forget such a host yet (#634). `restore --dump` now sets drained hosts offline as well (a drain in a dump older than schema 88 is not carried over). No wire or schema change; the contract's definition of a live host is protocol amendment 32.
- **The first-run setup claim and the dev-only agent-session route are no longer served over plain HTTP (#593, #594).** A request carrying `X-Forwarded-Proto: https` passed the HTTP-to-HTTPS redirect from any sender, and with `QUASAR_HTTP_REDIRECT=off` or `QUASAR_TLS=off` nothing stopped it at all, so the setup token, the new admin's password or the dev key could cross the network unencrypted. Both routes now answer only on the HTTPS port, or through a reverse proxy listed in `QUASAR_TRUSTED_PROXIES` that forwards `X-Forwarded-Proto: https`; anything else is refused before the secret is compared, with a message that says what to do; a secret sent that way crossed the network in clear, so restart the control plane to rotate it. **If your proxy talks to the control plane's HTTP port, list it in `QUASAR_TRUSTED_PROXIES` before first-run setup** (the hardened Caddy overlay included), or claim on the HTTPS port. With `QUASAR_TLS=off` and no listed proxy, create the admin with `BOOTSTRAP_ADMIN_*`; the boot log says so. An install that already has an admin is not affected. The dev route also answers a caller inside the container, and a dev stack whose HTTP port is published on `127.0.0.1` only can set `QUASAR_DEV_AGENT_INSECURE_HTTP=1` (the local `make up` stack does). The contract's rule is protocol amendment 33.
- **A socket that connects to the agent endpoint and sends nothing now gives up its slot after 11 s instead of 15 s, and the agent no longer waits on its recovery actor while it connects (#627).** The control plane admits at most 256 agent sockets that have not registered yet, so with enough addresses a client could keep that pool full and refuse real agents. On an owned install the agent used to wait up to 10 s for its recovery actor's status before it sent `register`; it now asks in the background. 11 s still covers an older agent that waits. The window drops to 5 s once every supported agent has this change.
- **Seven admin request bodies are now size-limited (#622).** The user update, the console configuration update, the idle-apply approval, the host policy edit and retry, and the host settings edit and restart read at most 1 MiB, and a console `input_devices` list is capped at 256 entries of 256 bytes each. A larger body is refused with a 400 instead of being read in full.
- **A node agent's old secret can no longer reconnect after the host is re-enrolled (#618).** A reconnect whose secret was checked just before a re-enrollment replaced it was still accepted, and its connection took over from the newly enrolled agent. The reconnect is now written only while the host still holds the secret it was checked against; otherwise it is refused as a wrong secret (`auth_failed`). And a connection that closes while its host reconnects no longer marks the host offline after the reconnect, so a re-enrollment cannot replace the secret under the reconnecting agent. No wire or schema change.
- **The DX soak and bench tools no longer put an admin bearer on an ssh command line (#603).** A remote soak, and the bench runner's and suite's remote API calls, sent the token inside the remote command, where `ps` shows it on both ends. The token now goes over stdin (the soak's first line; a `curl -K -` config for the calls), so it is in no argument.
- **Patched dependencies for the node agent (`zerovec`) and the docs site (`js-yaml`, `devalue`, `undici`, `fast-uri`, `source-map-js`, `zod`, and `form-data` pinned to 4.0.6 by an `overrides` entry) (#629).** Lockfile and pin changes only; no code or behaviour change.
- **A host's journal report after a reconnect can no longer reopen admission it should keep closed (#620).** An `applied` attempt now releases its idle-apply hold only when the host's active snapshot shows that same verified configuration, a finished attempt can no longer be moved back to an unfinished phase, a journal that reports one attempt twice with different phases is not applied (both quarantine the host instead), and an `uncertain` attempt gets its recovery hold even when the control plane had lost it. Only an inconsistent agent journal or a restored database reaches these paths.

### Internal
- **`REVIEW.md` says how changes are reviewed.** It covers when one or two reviewers are needed, a budget of at most 3 review rounds (then a design directive to the writer), and what every reviewer checks: root cause, reversion proof, invariant over call sites, the other side's real message order, contract sentences, size, bench evidence and public hygiene. `AGENTS.md` points to it.
- **The contributor build image is half the size: 4,487 MB, down from 8,978 MB (#198).** Its toolchain stage installs a named list of packages instead of everything Fedora's GStreamer needs to build (`dnf builddep` brought GIS data, soundfonts, Qt and static LLVM), and no longer keeps the unpacked crate sources and the GStreamer clone in its layers. What it builds is unchanged: the same 427 registered GStreamer features, the same files under `/opt/gst` and the same libraries linked by each of them. The dev image no longer happens to contain PipeWire; nothing in the repo used it there.
- **Patch-chain notes left after the GStreamer 1.28.8 bump (#578).** The AV1 encoder patch's header says where it sits in the apply order, the fork-bump policy names hosts by role, and the image contract now refuses an agent image built on a GStreamer older than 1.28.6, which the agent's WebRTC code needs.
- **The bench guide says why NVIDIA encode time jumps between runs.** On an RTX 5090, a light stream's `encode_ms` windows sit near 2.9 ms (P5) or 6.7 ms (P8), so two runs of the same build can differ by about 2×. That mix explains a gap inside the band when both plateaus are unchanged. A moved plateau, or an `encode_ms_max` above the slow band, still blocks a landing (`docs/testing-bench-mode.md`).
- **The RH-07 acceptance map records the last hardware runs and the accepted gaps (#502).** A dump restored into a fresh rootless install and a restore interrupted at three points pass on the AMD test host. A machine reboot on that host is recorded as an accepted gap: the test hosts are containers, so both AMD Docker reboot rows were container restarts, and the earlier rootful row is corrected to say so. The lab-VM rows, a live GPU fault and a Podman bench re-run are accepted gaps too. The run's one product finding is #589.
- **The agent socket's pre-auth limit now counts an IPv6 client by its /64 (#674).** The cap of 10 unauthenticated sockets per client and the failed-register limit were keyed on the full address, so one IPv6 subscriber, who holds a whole /64, had a fresh budget for every address in it. They are now keyed on the /64; IPv4 is still per address. Other limiters (sign-in, setup claim, signaling) are unchanged. No wire, schema or setting change.
- **The host-settings idle-control web test no longer fails at random in CI (#765).** It asserted the legacy idle row in the same tick as the remedy text, but the row renders one render later; it now waits for it.
- **Dependency bumps: `actions/setup-node` 7.1.0 (CI and Pages), `hyper` 1.12.0 and `hyper-util` 0.1.21, `serde_json` 1.0.154 and `rcgen` 0.14.10 (node agent test certificate), `golang.org/x/crypto` 0.58.0 with `x/sync` 0.24.0, `x/sys` 0.49 and `x/text` 0.43 (control plane), the `vite` group with `@vitejs/plugin-react` 6.1.2, and `jsdom` (web), `astro` 7.3.8 and `@astrojs/starlight` 0.42.6 (docs site).** Lockfile and manifest changes only, plus the one `rcgen` 0.14 rename in a test helper; no behaviour change.

## 0.4.2 — 2026-10-13

### Upgrading
- **This release changes the database (five migrations, 0099–0103), so there is no going back below it.** Once applied, an older control plane will not start, and a failed update is not put back automatically. For Quasar's own database the way back is a restore of the dump the update takes first (`docs/upgrading.md`, "The one-way migration rule"). Quasar never dumps your own database, and a source install has no dump: back up first. On your own database the update asks you to confirm your backup; a source install is not updated from the console and is asked nothing.
- **Update from the console, by hand.** Fleet ▸ Releases ▸ Update Quasar waits for live sessions to end on their own before it replaces the control plane. To end them instead, tick "Update now — ends every live session on N hosts"; if sessions are still up when the wait runs out, the update fails with `timeout`. Automatic updates never install a release that changes the database. On a source install, redeploy the new ref.
- **Nothing to migrate yourself.** The migrations add a session stop reason, a library "missing since" mark and a pending provider-access table, drop the retired console settings from stored console configs, and give existing apps the direct-display setting their installed image declares.
- **On every SELinux host, run `prepare-host.sh` again after the update** (with `--console` on a console host), or streamed Steam sessions stay silent. If you set `QUASAR_PULSE_IMAGE`, it must name an image from this release or later.
- **If console mode is on, check its default app.** A console session starts only a default app that declares `direct_display`, and it is no longer streamed; turning console mode off and on does not bring the old path back. The Steam app, and any app that runs a catalog image through that image's own preset, gains the setting by itself: at this update if the installed image is already the version that declares it (Steam and KDE desktop `2026.10.06`), otherwise when you update that image. An app on your own preset or its own image needs `"direct_display": true` in its `runtime_spec`, added through the API (`PATCH /v1/apps/{id}`, sending the whole `runtime_spec`); the console has no control for it. XFCE desktop declares none, so it cannot be the console's default app.
- **If the console does not launch after the update, sync the catalog and update the default app's image.** In Library ▸ Images, Sync catalog, then **Update to `<version>`** on the image (a pinned image offers none: unpin it first). This also covers a catalog that is ahead of the installed image.
- **On an owned install with console mode on, turn console mode off and on once after the update,** so the sound and udev checks stop reading `unknown`.
- **It worked when Fleet ▸ Releases shows this release everywhere and the run ends `succeeded`.** On a console host, the Local console page also shows no error under **Default app**.

### Added
- **A direct-display console session now passes hidraw nodes, not just evdev, so Steam Input identifies a controller correctly (#462).** The agent grants every `/dev/hidrawN` node present at launch (or, with an `input_devices` allowlist, just the allowlisted events' hidraw siblings, resolved through sysfs) plus a device-cgroup rule for hidraw's major, so a plugged controller shows up in Steam Big Picture as itself instead of falling back to an evdev GUID match. The hidraw devices are listed from sysfs and passed by their host paths, so an owned install, whose agent sees neither the host's `/dev/hidraw*` nor `/host/dev`, is covered too. A controller plugged in for the first time mid-session still isn't identified correctly until the next session start — the container gets no new `/dev/hidrawN` node without a udev client of its own — but keeps working over evdev in the meantime.
- **A console session's desktop now drives the monitor itself (#458).** For an app that declares `runtime_spec.direct_display`, the node agent launches the app container with the console GPU's card node, the sound device, the input devices (plugged in later too) and the host's udev, and the desktop takes the display on its own DRM backend: no compositor, pipeline or second display server in between. The session is running once it is displaying, and reports the monitor's mode.

### Changed
- **A node agent crash now leaves a report in its log, which is how the #267 launch crash was located (#267).** The report names the faulting thread and carries the address, stack and mapping data needed to find the code; a session's threads have names of their own, and each launch logs the stage it has reached. It placed the rare crash about a second into a launch in the Vulkan loader, on the compositor's thread. The agent also loads its encoder's GStreamer plugin when it starts, which removes one of the Vulkan instance creations that could trigger that crash at the first session. The preload does not prevent the crash by itself; the crash is fixed in this release (see Fixed).
- **The troubleshooting page now lists the X/Y swap on a gamepad page inside a session (#348).** A gamepad tester opened in Firefox inside the streamed desktop shows X and Y swapped; games and Steam are correct. It is an accepted limit, so the page says so and that there is nothing to change.
- **A streamed session's sound now runs on PipeWire instead of a PulseAudio daemon (#392).** Each session's audio sidecar is PipeWire with its PulseAudio protocol and WirePlumber, so games see the same socket, the same `quasar_output` and `quasar_mic_src` devices and the same 48 kHz stereo format as before; nothing to configure. `QUASAR_PULSE_IMAGE`, if you set it, must name an image from this release or later: an older image has no PipeWire and its sessions are silent.
- **Streamed session sound no longer clicks when the host is busy (#392).** Outside a VM, the shortest audio cycle a game or the agent can ask for is now 10.7 ms instead of 5.3 ms (a VM keeps its 21.3 ms floor), trading a few milliseconds of audio latency for no dropouts under load.
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
- **Dependencies updated** (eleven patch and minor bumps: the node agent's tokio, tokio-rustls, http, bytes and libc; the web client's vite, jsdom and Node types; the docs site's astro, sharp and starlight-openapi).
- **The node agent image builds GStreamer 1.28.8 (was 1.28.4), and with it libnice 0.1.24 (was 0.1.23).** This brings the upstream fixes in the parts Quasar streams through: RTP and RTCP handling survives malformed control packets and limits how many sources one session tracks; the DTLS handshake copes with a long certificate subject; the Vulkan encoders synchronise their reference pictures before each frame; the VA encoders size their output buffer for the bitrate and recover from a corrupt or overflowing one; and libnice fixes crashes in ICE. One vendored patch is gone: the part of it that worked is upstream now, and the rest never took effect.
- **The host now holds a client to the `a=fingerprint` in its answer.** With GStreamer 1.28.8 the node agent checks the certificate a client presents in the DTLS handshake against the fingerprint its answer stated. A client whose fingerprint does not match its certificate still gets video, and the host takes nothing from it: no input, no microphone, no feedback. Browsers are unaffected, since each states its own certificate. A native or custom client must state the fingerprint of the certificate it really presents, written as browsers write it (`sha-256`, then upper-case hex pairs joined by colons).
- **The signaling contract now states that rule (protocol amendment 29).** `protocol/signaling.md` has a new section, "The answer's `a=fingerprint`": which fingerprint line the host reads, the form it must take, and what a client that fails gets. Browsers already answer this way, so nothing changes on the wire. The host does not yet report a failed check (#577).

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
- **The contract no longer lists a `manifest_invalid` fault for the Releases view (#157).** It
  was specified and no control plane ever raised it. A release whose manifest is missing or
  invalid is still left out of the list, and the release detection job's run summary (Jobs
  tab) still counts it. Nothing the control plane or the console does changes (protocol
  amendment 26).

### Fixed
- **An app that existed before the update gains `direct_display` by itself, so console mode keeps launching after an update from 0.4.1 (#580).** A console session starts only a default app that declares `direct_display`, and nothing gave the key to an app made before its image's manifest declared it: the console did not start and the Local console page offered no app. The Steam app, and any app that runs a catalog image through that image's own preset, now takes the key from the manifest: at this update when the installed image is already the version that declares it, otherwise when that image is updated. An app pointed at another preset or at its own image is left alone, and so is one that sets `direct_display` itself, `false` included.
- **The node agent no longer crashes now and then while starting a session (#267).** It was rare, about two launches in a hundred in testing, and it ended every session on that host until the agent restarted itself. Present since at least 0.3.0. The cause was a Vulkan loader fault that the compositor's start-up could run into; the compositor now avoids the call that reached it.
- **An NVIDIA host no longer logs an EGL error each time a session ends (#570).** The agent log carried one `eglDestroyContext: EGL_NOT_INITIALIZED` error line per session, with no other effect; the compositor now releases its display after the session's GPU device.
- **A takeover or a revoked access that lands while the stream is being revealed is now readable for the whole handoff (#533).** The launch screen keeps its notice card up on its own ground instead of hiding it and flashing the reveal wipe, and the card fades out as the banner fades in.
- **The Libraries scan interval now sets how often discovery scans run (#536).** Lowering Settings ▸ Libraries ▸ Scan interval (or `QUASAR_LIBRARY_SCAN_INTERVAL`) used to change only how old a home's last scan had to be; the `library.discovery` job kept its own six-hour period, so a 15 minute interval still scanned every six hours. The job now runs on the interval in force: an edit takes effect within the dispatcher's 10 s tick, in both directions, with no restart and no burst when it is raised. The Jobs tab shows that interval for this job as locked (by the setting, or by the environment variable when that is set) and refuses a different one with `422` when it is below 60, otherwise `409`; the reported value sent back unchanged is accepted and not stored. A period previously saved on this job in the Jobs tab is ignored; the Libraries interval is the period. A scheduled pass also no longer skips a home just because the agent reported its last scan after the pass that queued it, which had made every other pass a no-op.
- **A takeover or a revoked access that lands while the stream is being revealed now shows its banner up to 220 ms sooner (#533).** The banner appears when the launch screen goes inert and starts to fade, not when it is removed. Leaving the screen's revoked-access card no longer asks the server to stop a session it has already stopped.
- **A launch that deadlocks with a library scan report no longer fails with a server error (#553).** A launch could deadlock with a scan report that removes a game, on the host and the game's entitlement, and a launch that lost returned a 500. It is now retried, and refused cleanly if its game is gone.
- **The session page's frame callbacks keep their steady cadence again (#544).** A session-page change (#529) removed an always-running animation from the idle page, and the bench read about 50% more jitter between frame callbacks and 13% fewer of them, with no frame lost. The animated mark is back on the idle page; the overlay's text stays out of it. Smoothness on a real display was not measured.
- **A console update on Podman no longer fails at the control-plane step with the published images (#543).** Podman keeps no health check from a published image, so the new control plane read `health=none` and the step failed `unhealthy` after 300 s although it was serving. The recovery actor now gives the control plane its health check itself (control-plane recipe revision 3), on every engine, and it probes every 10 s instead of 30 s. Takes effect from the first update to a release that carries it; the health gate is unchanged. A restore back to 0.4.1 or earlier on Podman still ends `unhealthy` although it worked: `docs/upgrading.md` says how to check it.
- **An engine operation that had finished could still make the very next one be refused as busy (#295).** The agent's engine client released an operation's admission slot just after handing back its result, so a caller that started its next operation straight away, on a client at its in-flight limit, could be told `Busy`. Teardown retried it, so the visible effect was a flaky node-agent test (`saturated_application_admission_leaves_no_intent_so_abandon_releases_the_retry`) in CI; the slot is now released first.
- **An NVIDIA host whose containers are not given `/dev/nvidia-modeset` and cannot start Vulkan now fails readiness (#449).** The application GPU probe also tries to bring up a Vulkan device in the same container shape a session uses; when none comes up and the container has no `/dev/nvidia-modeset`, the check fails with the `nvidia-modprobe -m` and CDI regeneration fix, so launches stop failing one by one. Other Vulkan faults are shown but do not block launches. The install guide covers it, including Unraid's boot-time spec.
- **A host with no NVIDIA GPU no longer blames the NVIDIA driver volume for a missing Vulkan codec (#308).** An AMD or Intel host whose GPU cannot encode a default-enabled codec (AV1 on VCN 3.x, say) used to log "expected on first boot: the NVIDIA driver volume is still provisioning" at every start. It now says the codec has no Vulkan encoder on that host and is not advertised (`vulkan-codec-plan-no-vulkan-encoder`), or, when a vendor encoder serves the codec instead because the Vulkan element is missing, warns of that (`vulkan-codec-plan-vendor-fallback`). NVIDIA hosts keep their messages.
- **A failed library scan now says why in the logs (#534).** The node agent logs one WARN per failed scan (`library-scan-failed`) and the control plane's "scan reported failure" line is now WARN, both with the failure class (`walk timed out`, `path unreadable (PermissionDenied)`, `manifest not valid UTF-8`, and so on). The logs carry no path, file name or appid, because those name a user's games; the file the scan could not read is named in the scan row and the admin library status, unchanged.
- **A host tool that prints more than a pipe buffer no longer times out with its output lost (#195).** The agent's `nvidia-smi`, `vulkaninfo --summary` and EGL self-test child processes are now read while they run, so a long listing finishes instead of blocking until it is killed; output past 1 MiB is dropped.
- **An app that exits before its first frame is no longer reported as a network problem (#449).** The page's own "Could not reach the stream" guess now always yields to what the control plane says about the session, in either order of arrival, so you see "The app exited before producing any video" and the app's log instead. A stream that is truly unreachable while the session is still running keeps the network message.
- **Opening a running session in a second browser now moves the stream there (#535).** The page that took the session over sat on its loader with no video, the first page kept playing behind its "moved to another tab" notice, and the session ended when that first page was closed. The host answered the second browser with the first one's connection, which no other browser can use: the WebRTC library ignores the request to restart it. The host now recognises an answer from a different browser, gives it a connection of its own and closes the first one's, keeping the encoder (its bitrate goes back to the launch value, and so do the frame rate and size if adaptation had lowered them; a size you pinned stays); the second browser is sent one more offer for that. A page that reconnects with its own connections keeps them. The web client now handles two offers that arrive together one at a time. A session changes hands at most six times a minute. Reloading the page that is streaming still ends the session, as it did before.
- **A game a library scan could not see for a moment is no longer taken away (#521).** A successful scan can still miss an installed game, typically because its library sat on a mount that was briefly away. One such scan removed the game from the user's library and ended a running session of it. A missed game is now only marked, and is removed on the second scheduled scan after the one that missed it, about two intervals later, if that scan also misses it; a scan that sees it again clears the mark. The cost: an uninstalled game leaves on the second scheduled scan after the first one that missed it, about two intervals later, instead of at the first, and "Scan now" pressed sooner than one interval does not speed that up. A library that stays out of the scan's reach across both scans is still read as uninstalled (schema amendment 25, migration 0102).
- **A session that is over for its page no longer draws the HUD, and an idle session no longer carries the quick-switch overlay's text (#529).** Under the "Your access to this app was removed" banner (and the other verdicts the page cannot recover from) the HUD pill, the touch "Session menu" button and the HUD shortcut are gone, so the banner's button is the only action and no app swap can start; a session that may still be streaming keeps all of them. Under a takeover or access-removed verdict nothing else is drawn either: the decoder and stream-health banners (whose Stop would end a session another tab owns) and swap toasts give way to the verdict. The swap overlay's "Starting…" now exists in the page only while a swap is in flight or fading out, instead of sitting hidden in every session's text.
- **A session that ends while you stay on its page no longer leaves the microphone on or keyboard capture engaged (#524).** When the page reaches a verdict it cannot recover from (host lost, access removed, the session moved to another tab, a failed launch, or recovery stopped with the input channel gone), it releases the microphone and input capture, so the browser's recording indicator goes off and Tab and Enter reach the banner's button. A microphone permission prompt, a microphone attach or a Pointer Lock grant still in flight at that moment cannot undo the release while the capture is alive. A session that may still be streaming (signalling gave up while the input channel is open) keeps both.
- **A session taken over by another tab now says so, and a takeover after a failed connection stops the reconnect (#527).** Before, a takeover while you were streaming left a black page with no notice, and one that arrived after the page had already given up on the media path still let it mint a replacement connection and displace the tab that had just taken the session. A takeover now always reaches the page, once, discards the reconnect, replaces the "could not reach the stream" verdict (so the microphone and input capture are released), and shows as the top banner with a "Back to library" button (on the launch screen the same button no longer stops the other tab's session). A Pointer Lock request granted after input capture was torn down is now handed straight back instead of leaving the cursor locked until Escape, tearing down an older capture no longer takes the lock a newer one holds, and ending the session releases a lock inherited from an older capture.
- **A session ended because its owner lost access now says so (#516).** When an app's access was removed mid-stream, the session view showed "Connection recovery stopped" over a black frame. The session read now carries a `stop_reason` (`entitlement_revoked`, control-api amendment 24, migration 0101), and the session view shows "Your access to this app was removed" with a way back to the library, during launch as well as while streaming. Opening that session again later (a bookmark, a reload) returns to the library with the same notice instead of "Launch it again from your library".
- **Hosts no longer stay draining after a second control-plane process touches the database (#515).** Running the control-plane binary with an argument inside its own container (`quasar-control --version`) ignored the argument and started a second control plane: it held every host for journal reconciliation, then exited, and the hosts stayed `draining` until their agents reconnected. The binary now answers `--version`, refuses any other argument, and takes its HTTP and HTTPS addresses before it writes anything, so a second process on either address stops first. A control plane that finds such a hold on a host whose agent is still connected now asks that agent for its journal again at the agent's next heartbeat, and releases the hold once that journal is confirmed.
- **A stop the node agent never received is now sent again (#503).** If the stop command was lost (a full send queue, a dropped frame), the control plane showed the session as stopping while the stream kept running. It now re-sends the stop when the agent's heartbeat still lists a session that went stopping more than 10 seconds ago, or at once after a control-plane restart. A session stopped before it started, which the agent never lists, gets the same re-send, so it no longer holds its encode slot and home. This covers every kind of stop, not only a revoke.
- **An unreadable or missing Steam library no longer empties the library (#512).** A permission or I/O error reading a Steam library the scan was asked to read (the library directory, a folder on the way to it, or a manifest) now fails the scan, and the control plane keeps a user's observations and provider entitlements when a scan reports no games at all while it already holds some, instead of revoking games that are still installed. Removing a game is still picked up; the last game of a library is only dropped once a scan sees another.
- **An unreadable folder met while searching the home for Steam libraries no longer hides one (#514).** The search skipped a folder it could not read, so with two libraries found that way a locked-down parent of one made the scan report only the other as a success, and the control plane dropped the hidden library's games and entitlements (ending a running session of one within the sweep interval). The same went for a search that ran out of its time budget on a slow disk. A permission or I/O error on such a folder, or the search timing out, now fails the scan and the control plane keeps what it has. A folder that vanishes mid-scan, the search depth limit and symlinks are still skipped quietly. A scan that stops at the per-scan manifest cap (512 games) is no longer taken as proof that the games it left out are gone: the control plane adds what it lists and prunes or revokes nothing for being absent from it, so a library larger than the cap keeps its entitlements.
- **A Steam manifest the scan cannot read no longer reads as an uninstalled game (#521).** An `appmanifest_*.acf` that exists but cannot become an entry (damaged, incomplete or empty, not UTF-8, no appid key, or over `max_manifest_bytes`) now fails the scan with an error naming the file and the reason, so the control plane keeps the user's observations and provider entitlements instead of revoking them (and, with a revoke sweep, stopping a running session of that game). A manifest whose appid is invalid (after trimming surrounding whitespace, as the control plane does) is still dropped, since it can never have been ingested; symlinks, files that are not manifests, duplicates and the `max_entries` cap are skipped quietly too. A library moved beyond the discovery depth or behind a symlink, or a mount briefly away while another library is still visible, can still shorten a report; that is tracked in #521.
- **The node agent no longer sends a non-semver recovery_actor_version on edge builds (#507).** The recovery actor on an edge (develop) build reports its version as `dev`. The agent now filters it: only semver versions reach the wire, matching the control plane's contract rule that non-matching values are treated as absent. The local readiness fact remains unchanged (an operator still sees the raw `dev` value), so the edge-build fact is visible when needed.
- **A hand-installed Fedora stack's Steam sessions were silent (#478).** `prepare-host.sh`
  loads the `quasar-nested-audio` SELinux module on every SELinux host (#476), but the
  quick start's manual host steps never emitted it (only the NVIDIA device rule), and a
  machine installed by hand never runs host preparation, so the nested-sandbox type could
  not reach its session's PulseAudio sidecar. The steps now emit the same one-line module
  on every SELinux path, NVIDIA or not; the rule's scope is documented in Device rules
  ("SELinux and session sound").
- **A streamed Steam session on an SELinux host now has sound (#476).** The app runs as the nested-sandbox type (`container_engine_t`), which the policy did not allow to connect to its session's PulseAudio sidecar (`container_t`): the policy's connect rule covers only two containers of the same type, so the stream was silent. `prepare-host.sh` now loads a `quasar-nested-audio` SELinux module granting one permission between the two types and nothing else: a nested-sandbox process may connect to a `container_t` unix socket it can reach by path, which is only its own session's pulse directory because that is all the agent mounts into the app (an app given another session's socket directory could reach that session's audio). **On an SELinux host, run `prepare-host.sh` again** (it is safe to repeat); a host that has not has no sound in those sessions until it does.
- **A console relaunch after an agent reconnect no longer stalls the agent's connection (#477).** A reverse proxy in front of the control plane can close the agent's WebSocket (Caddy sends close 1001 to both ends on every config reload; `stream_close_delay` delays that close rather than preventing it). The agent retries after a one-second backoff, and a console launch still starting fails as `host_lost`, as before. The relaunch then hit that launch's managed-home hold and waited 10 s for the agent's cleanup proof. That proof arrives on the same connection, and nothing on it was read during the wait, so every attempt ran out and logged "managed home location requires repair", and the next reconnect's registration queued behind it. Console auto-start no longer waits: it logs that the previous session still holds the home, and the proof itself triggers the relaunch. Turning console mode off or unplugging the display no longer waits on that connection for the stop's acknowledgement. A heartbeat from a replaced socket is now ignored instead of being logged as "idle approval no longer matches reviewed policy"; it never involved an approval.
- **Docs site: corrected stale install, telemetry, latency, ABR, scan-interval, port, enrollment and codec statements and documented the homes GC, key rotation, image catalog, console grace and proxy knobs.** The site now matches `docs/configuration.md` on every
  default it names, and the environment reference carries the agent-side homes sweep and
  the control-plane knobs it had omitted.
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
- **A host no longer loses memory and GPU memory with every session a browser streamed (#564).** Each one left a GPU device open in the node agent until the agent restarted: about 33 MB of memory and 19 MiB of GPU memory per session on an NVIDIA host, less on AMD. Present since 0.4.0.

### Security
- **Losing access to an app now ends the sessions already running it (#503).** Removing an entitlement, or setting a library provider to "Only me" or "Nobody", used to take effect at the next launch: a session that was already running kept running and its owner could reconnect to it. Now a session whose owner is no longer entitled to its app is stopped: before the request returns when an admin removes the access, and within about 30 seconds otherwise (a library sync that drops an uninstalled game, a swap into an app revoked mid-swap). That includes sessions on a provider's discovered games. A session whose swap has not finished after two minutes, or was in flight when the control plane restarted, is ended, because the app it runs can no longer be known. A stopped session cannot be reconnected to, even with a token minted earlier. A user still covered by another grant keeps their session. The activity log records how many sessions an admin's change stopped (contract amendment 23).
- **An agent can no longer grow the control plane's memory with made-up restart-scope attempts, an endless policy journal or oversized reports (#504, part of #484).** A restart-scope `config_policy_state` is now checked against the durable attempt it names before anything is cached, so a report for an unknown attempt, for a real one under the wrong grant, or for a real id in another spelling (upper case, braces, no hyphens) is ignored and logged instead of held for the life of the connection. A policy journal inventory is capped at 16,384 entries (64 full pages), and an entry whose attempt id is not spelled as it was issued is refused; either closes the connection and leaves admission blocked, as a malformed inventory does. For each attempt the control plane now keeps a digest and the grant identity rather than the report's error and evidence, so one connection can pin about 64 MiB at most (what its inventory can carry), however many reports it sends. No contract change.
- **Restricting a library provider now also hides the games discovery found for it (#497, part of #484).** Each discovered game kept its own entitlement, so after an admin set Steam to "Only me" or "Nobody", or revoked a user from it, users still saw and launched the games already in their library. A discovered game now needs access to its provider app as well: it disappears from the library and is refused at launch, and comes back when access is restored. Deleting a provider's only app while library discovery is on is now refused (`409 provider_enabled`, turn discovery off first), because the next sync would have recreated it open to everyone. Contract: protocol amendment 22.
- **The control plane now accepts next-session policy state only for an attempt it issued on that agent connection, and a console change that meets a full agent send queue now forces a reconnect instead of being dropped (#498, part of #484).** Before, an agent could report state for any made-up attempt id, and each one in an in-progress phase was held for the life of the connection; such a report is now ignored and logged with its id. A console revocation sent while the agent's 32-slot queue was full was lost until the agent next reconnected; now the control plane closes that connection, and the reconnect delivers the current console config. No contract change.
- **An admin can no longer take over another user's live session (#496, part of #484).** `POST /v1/sessions/{id}/signaling-token` let any admin mint a token for someone else's session and attach to its stream, input included. It now mints only for the caller's own session and answers `404` otherwise, as `control-api.md` already said. Admins can still read and stop any session.
- **"Only me" or "Nobody yet" for a library provider now holds even when the provider app is created after the setup wizard has moved on (#490, part of #484).** The wizard's choice used to be retried for about nine seconds and then lost, which left the provider app visible and launchable for every user. Now the control plane keeps the choice (`202`) and grants it when the app is created, including one an admin creates or marks as the provider app by hand, and the wizard says it will apply once installed. With no choice made, a provider app is still for everyone. A provider now has one provider app: creating or marking a second one is refused (`409`), and so is un-marking the only one while library discovery is on (`409 provider_enabled`, turn discovery off first). Contract: protocol amendment 21, migration 0100.
- **A policy-inventory entry whose `scope` disagrees with its group's now blocks reconciliation instead of reading as terminal history (#488).** A `failed` entry for a known group with the wrong scope was left out of the journal view, so a never-started cancelled attempt for that group could be marked unstarted and its idle-apply hold dropped. Only the authenticated agent can send it; the entry now marks the inventory unknown and blocked, as a mismatched restart-scope entry already did. So does an entry that reports an attempt id the database holds under a different group or scope. No contract change.
- **Revoking a device now reliably ends its live sessions (#489).** The stop no longer runs on the request's context, so a client that disconnects mid-`DELETE` cannot leave the device's sessions streaming. The revoke transaction also locks the device row, so a session launched from that device at the same moment fails instead of committing between the session lookup and the delete and escaping the stop.
- **Re-enrolling an existing host now needs an enrollment token bound to its node name (#487, #491).** A token minted without a name still adds new hosts, but it can no longer take over a host whose agent is offline: before, anyone holding such a token (up to 100 uses, 30 days) could register under an offline host's name, take its identity and lock the real agent out. Re-adding a known machine: fill in its name in Admin → Fleet → Add host, or forget the host first. Contract: protocol amendment 20.
- **The first-admin bootstrap no longer hands admin to whoever registered the configured email first (#485).** When no admin exists and an account with `BOOTSTRAP_ADMIN_EMAIL` is already registered, the promotion now sets that account's password to `BOOTSTRAP_ADMIN_PASSWORD` and revokes its tokens, so only the operator's credential reaches the new admin. Before, the account kept its own password and sessions.
- **The node agent refuses a session id that is not a single path component before it names the session's audio socket directory, and the recovery actor refuses a dump record whose `name` is not its own file's (#481).** Both values build file paths. The control plane only sends UUIDs, and the dumps directory is owner-only, so neither is known to be reachable today; this closes them anyway, like the udev export already did.
- **A streamed game can no longer take the screen from a console session on a rootful Docker or Podman host (#464).** A streamed app now gets each render node and each card node mknod-only: the GPU still enumerates (radv and gamescope need the card node to exist) but the card never opens, so a console session started later still gets DRM master (measured on Docker 29.8.1 and Podman 5.8.4). NVIDIA hosts (the container toolkit grants the card read-write whatever is asked) and rootless engines (no device cgroup) cannot enforce this and keep today's grant: there the console holds the display by starting first. `console_card` now says which case the host is in, and the runtime journals and logs `token=app-card-nodes-openable` wherever a streamed app's card opens. A host with no NVIDIA GPU whose agent lists no DRM node now refuses a streamed GPU launch, naming why, instead of granting the whole `/dev/dri`. The per-engine table is in `docs/configuration.md`, "Streamed sessions beside a console".

### Internal
- **CI now runs the node agent's live WebRTC tests (#571).** Pull requests that touch `node-agent/`, `deploy/Dockerfile.vulkan`, `deploy/patches/`, `deploy/pins.env`, `deploy/toolchain-hash.sh`, `.github/workflows/agent-live.yml` or `.github/workflows/images.yml`, and a nightly run on `develop`, run the Rust suite in the published toolchain image, where the takeover tests and the #564 leak test run instead of skipping; there, a test that cannot run fails the job. No behaviour change.
- **A test now guards the lock order #271 said was inverted (#271).** A launch's placement recheck and a host's capacity report both lock the host row before its GPU rows, so that pair cannot deadlock each other. It covers that pair only: a launch can still deadlock with a library scan report (#553). No behaviour change.
- **The RH-07 acceptance map records the AMD rootless runs of 2026-10-11 and their fixes (#502).** A console update passes on rootless Docker, and on rootless Podman with the branch build that carries the #543 fix. The bench check that found less even frame callbacks reads clean since #544. Stories 2 and 15 are met again.
- **The RH-07 acceptance map is closed out.** It now records the direct-display console proofs on rootful Docker and rootless Podman, the fixes for every defect it listed as open, and the accepted gaps that remain; those runs are tracked in #502. The compositor pin's live exercise is recorded as done.
- **The failed-scan log test from #534 no longer fails now and then in CI (#534).** It checked that the whole log line did not contain the appid `730`, but the line also carries random scan and host ids and a timestamp, which contain those digits about one run in 75. It now checks every attribute of the record for the appid except the timestamp and the two random ids, and the whole line for letters-only markers that cannot occur in an id or a timestamp. A test-only change; the logs are unchanged.
- **The node agent's runtime helper tests no longer run close to the file-descriptor limit (#295).** Each `engine.client()` in the suite built its own tokio runtime (a thread, an epoll descriptor and eventfds), 378 of them per run, so the suite failed at `ulimit -n 256`; the tests now run their clients on one shared runtime and peak open descriptors halve. The runtime crate gains a `RuntimeClient::with_handle` for this, built only for tests (the `test-support` feature); the agent behaves as before.
- **`make test-db` no longer leaks a Postgres volume per run, and a killed run can no longer leave its Postgres running; the first test package can no longer hit Postgres's one-time init server (#205, #270).** The ephemeral Postgres's anonymous data volume (the official `postgres` image declares a `VOLUME` for the data directory) now goes with the container on every removal, and the next run removes a container or network that carries this tool's owner label only when that label names a dead owner in this pid namespace — one sentence: no pid namespace apart from this one, and no pid that has a process, ever loses resources. So a run killed hard (SIGKILL, which bypasses the exit trap) is cleaned up with its volume by its successor, while a live run — concurrent in one worktree, in another, or inside another pid namespace's container, or imitating another's name shape — is never touched; the same `-v` removal of the data volume and the same TCP readiness gate (`pg_isready -h 127.0.0.1`, which only the final server answers; the image's temporary init server listens on the Unix socket only) now also cover `make validate`, the apitest harness, and the devtools verify stack's healthcheck. Leftovers from before the owner label existed, and an orphan whose pid number some other process now occupies, are not swept automatically (`make reset` remains the manual sweeper for those). The readiness wait stays bounded and still reports a clear failure if Postgres never comes up.
- **The recovery actor's start-check tests no longer fail now and then in CI (#523).** The tests read the start's log lines through a per-test `tracing` subscriber, which can lose a line when parallel tests register the same log callsite as it appears; one process-wide subscriber, installed before any test logs, keeps them. A test-only change: the actor logs the same lines as before.
- **The agent-socket tests pass on a fresh database (#510).** Three typed-agent tests in `internal/agentws` failed whenever the test database had never held a control-plane boot row; the test fixture now seeds the row that the control plane writes before it admits any agent. Test-only, no product change.
- **The pull-deadline test flaked under a full parallel `cargo test --workspace` on a loaded
  machine (#463).** It raced a fixed server-side sleep against the client's own deadline and
  asserted a fixed wall-clock bound; the fixture now blocks on an explicit release signal
  instead of a timed sleep, and the elapsed-time assertion is gone. A few sibling timing tests
  in `images::tests` shared the same too-tight bound on a non-blocking call and got a generous
  one.
- **The API contract test harness pins `golang.org/x/net` v0.55.0 (#480).** It was on v0.50.0, which carries six published vulnerabilities (one critical). The harness is contributor tooling, not part of any image, and its use of the module is indirect.

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
