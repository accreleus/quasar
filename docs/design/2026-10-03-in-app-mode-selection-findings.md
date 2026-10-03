# In-app resolution and refresh-rate selection: how the code works today (#445)

Findings only, 2026-10-03. Read before designing #445. Every claim was read in code at
develop `e6bb6787` and the compositor fork at `6638e07` (`docs/third-party-pins.md`).

## The ask

A player in a running desktop or game should be able to choose a resolution **and** a
refresh rate in the app's own display settings, from a list we define (for example the
approved streaming rungs, so the app can never exceed what we stream), and the session
and stream should follow that choice live, without restarting the session.

## Why nothing but one mode is listed today

A session has exactly one mode, and the app's display is a reflection of it.

1. The control plane picks one **rung** (W×H@fps) at launch, `planStream`
   (`control-plane/internal/session/stream_plan.go:168-216`), and sends it as `stream` in
   `session_assign` (`protocol/agent-api.md` §`session_assign`).
2. The agent pins that rung for the session's life: the compositor's output caps are fully
   fixed to W×H@fps (`node-agent/src/session/pipeline/caps.rs:95-129`), the interpipe runs
   `allow-renegotiation=false`, and `webrtcbin` never renegotiates
   (`pipeline/scale_stage.rs:1-15`; `protocol/signaling.md:98`).
3. The compositor derives its `wl_output` mode from those caps: `apply_video_info` takes the
   caps fps as the mode's refresh (fps × 1000 mHz, `wayland-display-core/src/comp/mod.rs:768-770`)
   and `apply_output_mode` makes it both the current and the preferred mode (`:951-971`).
   There is one virtual output, `HEADLESS-1`.
4. The app container is also told the same W×H@fps as env (`QUASAR_STREAM_*`, `GAMESCOPE_*`;
   `node-agent/src/session/container.rs:572-695`), never re-injected mid-session.
5. The `mode-ladder` the agent pushes (`runner.rs:1552-1559`) adds only the same-aspect rungs
   **at or below** the launch size, all stamped with the **same** refresh (`comp/mod.rs:1233-1238`).

What the app's display stack does with that single mode:

| How the app runs | What its display settings list |
|---|---|
| XFCE: rootful Xwayland `-geometry` | Xwayland's own built-in resolution table, every entry at a hard-coded 60 Hz (`hw/xwayland/xwayland-output.c`, `xwl_screen_init_randr_fixed`). |
| Steam: nested gamescope `-W/-H/-r` | gamescope's Xwayland: resolutions at or below `-W/-H`, one rate from `-r`. |
| KDE Plasma nested | `kwin_wayland` never binds `wl_output`; it shows one synthetic mode at its own size, refresh from `wp_presentation` feedback. |
| SDL3 native | reads only the mode flagged CURRENT; emulates smaller sizes at that one rate. |
| Wine `winewayland` | reads every advertised mode with its refresh (the one client that does). |

So even the ladder modes are invisible to everything but Wine, and no path shows a second
refresh rate.

## What can change mid-session today, and what cannot

The only live levers sit **downstream of the compositor** or inside it, and all of them go
**down from launch, never up**:

- **External (stream) size** and **fps**: the scale stage's tail capsfilter
  (`scale_stage.rs:99-169`), driven by the ABR ladder or `PATCH /v1/sessions/{id}/display`
  `stream_*` (`control-plane/internal/session/display.go:93-191`,
  `node-agent/src/session/runner.rs:976-1053`). On Vulkan a size step is an in-place
  encoder session restart (`new_sequence`), a few frames, not a pipeline rebuild. fps is
  capped at the launch rate (`scale_stage.rs:150-169`).
- **Render size** (the app-facing `wl_output` mode): `render-size` on the compositor, via the
  same PATCH's `render_*`; capped at the launch size (`runner.rs:1023-1071`,
  `comp/mod.rs:912-923`).
- **Nothing** changes the compositor's output caps, the app's env, or the refresh.

The compositor itself is not the blocker: `apply_video_info` re-runs on every caps change
while PLAYING (`comp/mod.rs:748-760`), it renders on the pipeline's pull throttled to the
caps fps rather than on a fixed timer (`:1623-1637`), and Smithay stores a refresh per mode
(`smithay src/output.rs:79-86`). The pinning is the agent's design: the one-offer
`webrtcbin` invariant and the swap machinery need the interpipe caps identical on both sides.

## What a client can ask for

Wayland has no "client sets a mode" request. A fullscreen client that commits a buffer of
another size is scaled to fit and letterboxed (`comp/mod.rs:1083-1104`). The protocols that
exist for this, `zwlr_output_manager_v1` and `kde_output_management_v2`, are implemented by
neither the fork nor the Smithay it builds on; `wp_fullscreen_shell_v1` (which has mode
feedback) is not implemented either. Xwayland's RandR requests are emulated inside Xwayland
with per-client viewports and never reach the compositor.

## What the ask needs, by component

1. **A session mode set, not a single rung.** The control plane defines the allowed modes
   (resolution × refresh) per session, for example the chain's approved rungs. Resources
   (encode slots, VRAM, cert cap) must be reserved for the largest, since the session may now
   move up as well as down.
2. **Live re-pin in the agent.** On a mode change: re-pin the compositor caps at the new
   W×H@fps and let the Vulkan encoder restart in place (the mechanism the resolution rung
   already uses), keeping the interpipe and `webrtcbin` up. The interpipe caps and the
   one-offer invariant are the constraints to design around; a refresh change that keeps the
   codec/profile can ride the existing IDR-at-next-caps path. Re-inject nothing into the app
   container: the app learns the mode from its display, as a real PC does.
3. **Advertise the mode set on the virtual output**, each mode with its own refresh
   (fork change: `advertise_mode_ladder` stamps one refresh today; the agent must pass
   W×H@Hz rungs instead of W×H).
4. **A mode request path from app to compositor.** Options, from least to most invasive:
   (a) treat a fullscreen client's buffer-size change to an advertised mode as a resolution
   request (no refresh); (b) implement `wlr-output-management` in the fork and let the desktop's
   own display settings and tools drive it (KDE and sway tooling speak it; XFCE does not);
   (c) a patched Xwayland (and gamescope's Xwayland) that lists the advertised modes with
   refresh and forwards a RandR mode-set to the compositor over a private protocol. Only (c)
   covers X11 games and XFCE, and only (b)/(c) carry a refresh rate.
5. **Report the running mode** to the control plane (an additive `agent-api.md` message,
   Opus plus sign-off) so the session row and admin UI tell the truth.
6. **Console mode**: weston follows the mode the same way the agent already restarts it
   (`console.rs`); a modeset under a running Vulkan source has faulted NVIDIA
   (`runner.rs:1483-1489`), so the order is source stop, weston re-mode, source start.

## VRR (the next level)

- The stream path is pull-based; a variable rate is a compositor change (render on client
  commit with a floor) plus encoder support. Our Vulkan encoders derive rate control from the
  caps fps and `framerate=0/1` violates the Vulkan spec (VUIDs 08350/08351); NVENC passes 0/1
  through with undocumented behaviour; x264 and openh264 adapt. A nominal rate with per-frame
  budget is the likely shape; needs a prototype on the NVIDIA test host.
- weston 14.0.2 (what the base image ships) forces `VRR_ENABLED=0` on every modeset; weston
  15.0 adds `[output] vrr-mode=game`. Console VRR is a base-image bump, not a fork change.
- A browser cannot change a monitor's refresh rate; GeForce NOW's Cloud G-SYNC needs its
  native app. The native client (photon) would present VRR; it is not checked out here.
