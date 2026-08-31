# Upstream candidates: the two input patches from the 2026-07-19 Steam-input investigation

Two small patches fixed the "keyboard/mouse dead until a game launches" class of bugs on
Quasar. Both address defects that exist upstream and affect anyone composing these projects
the same way (a smithay-based headless compositor hosting nested gamescope). This document
captures everything needed to file the issues/PRs later. Status: NOT yet filed — needs
operator sign-off before posting externally.

---

## 1. games-on-whales/gst-wayland-display — initial toplevel never receives wl_pointer focus

**Repo:** https://github.com/games-on-whales/gst-wayland-display
**Affects:** every consumer (Wolf included); reproduced at commit `43d4c25` (2026-06-30),
mechanism present on current master at time of writing.
**Our vendored copy:** `deploy/patches/vulkan/gst-wayland-display-initial-pointer-focus.patch`.

### Defect

Pointer focus is assigned in exactly one place: `pointer_motion()`
(`wayland-display-core/src/comp/input.rs`) via
`Space::element_under(pointer_location)`. That lookup filters on `Window::bbox()`
(smithay caches it; only `Window::on_commit()` recomputes it, initial value `(0,0)`).
The compositor's commit handler
(`wayland-display-core/src/wayland/handlers/compositor.rs`) calls `window.on_commit()`
only for windows **already mapped into the space** — the mapping commit itself never
refreshes the bbox. Keyboard focus, by contrast, is assigned *directly* at the map site.

Consequence: a client that never re-commits its **root** toplevel after the mapping
commit (an idle `wev`; any launcher-style client rendering via subsurfaces) keeps
`bbox == (0,0)` forever, `element_under()` returns `None` at every cursor position, and
the surface never receives `wl_pointer.enter` — mouse dead, keyboard fine. Clients that
re-commit their root per frame (games, gamescope) self-heal within one frame, which
masks the bug in the common case.

Upstream already knows the symptom without knowing it's a production bug: their own test
fixture works around it manually —
`wayland-display-core/src/tests/fixture.rs` (~line 169):

> "I don't know why this isn't triggered, but I've spent hours trying to debug why the
> window size was kept at (0,0). Without this `under` in input.rs would never work"
> — followed by a manual `window.on_commit()` loop. The `move_mouse` pointer test passes
> only because of that workaround.

### Evidence (live, Quasar Tower host, 2026-07-19)

- Single fullscreen `wev` client + direct evdev injection into the compositor's libinput
  devices: keyboard = keymap/enter/keys all delivered; pointer = **zero** events, even
  after a 200-step motion sweep across the whole output plus button press/release.
- Identical on two image builds three days apart → not a regression, latent.
- After the patch: `wl_pointer.enter` + motion + buttons delivered on the first injected
  event; keyboard behaviour unchanged. Also end-user validated (XFCE desktop session:
  full mouse + keyboard).

### Fix (what the PR should contain)

At the initial-map site in the commit handler (the `else` branch that does
`space.map_element(...)` + direct keyboard `set_focus`):

1. `window.on_commit()` immediately after `map_element` — refreshes the bbox from the
   buffer committed in that very commit (production form of the fixture workaround).
2. A synthetic zero-delta `pointer_motion(now, (0,0) delta)` after the keyboard focus
   assignment — delivers `enter` immediately (not waiting for the next physical motion),
   makes pointer focus follow the newest toplevel exactly like keyboard focus, and runs
   `maybe_activate_pointer_constraint()` so a client that requested a pointer
   lock/confine before mapping (nested gamescope with `--force-grab-cursor`) gets its
   constraint activated as soon as its surface is focusable.

Exact diff: see the vendored patch file (applies on `43d4c25` after our other patches;
regenerate context for upstream master before filing). Suggested PR framing: bug-fix +
delete the fixture workaround (or keep it and note it is now redundant), and the fixture
comment itself is the reproduction narrative.

---

## 2. ValveSoftware/gamescope — nested backend: parent pointer unusable under Steam BPM's Passthrough touch mode

**Repo:** https://github.com/ValveSoftware/gamescope
**Affects:** gamescope 3.16.23 (verified; path present on master) running **nested**
(Wayland backend) with Steam Big Picture, i.e. every "cloud gaming / Wolf-style" stack.
**Our vendored copy:** `quasar-images:images/quasar-steam/gamescope-nested-pointer-warp.patch`
(built via the `gamescope-builder` stage in `images/quasar-steam/Dockerfile`).

### Defect

In the nested Wayland backend, parent **absolute** pointer motion is forwarded as
*touch*: `CWaylandInputThread::Wayland_Pointer_Motion`
(`src/Backends/WaylandBackend.cpp`, ~3089 at 3.16.23) ends in
`wlserver_touchmotion( flX, flY, 0, ++m_uFakeTimestamp )` — the 5th parameter
`bAlwaysWarpCursor` defaults to `false`. There is no code path that turns an absolute
parent pointer into the mouse path; the only mouse path is the relative handler, gated
on an active pointer lock (and once a relative-pointer object exists, absolute motion is
dropped entirely).

Steam Big Picture sets the root atom `STEAM_TOUCH_CLICK_MODE=4` (Passthrough), which
steamcompmgr copies into `cv_touch_click_mode`. In `wlserver_touchmotion`
(`src/wlserver.cpp`), the Passthrough branch emits only `wlr_seat_touch_notify_motion`
and skips `wlserver_mousewarp` unless `bAlwaysWarpCursor` — and gamescope starts its
Xwayland with `-noTouchPointerEmulation`, so those `wl_touch` events never become X core
pointer events either. Net: in nested gamescope + BPM, mouse motion/clicks never reach
any X client, and Steam's input context stays unlatched (keyboard delivered at X level
but ignored by BPM) until a game launch performs a focus-cycle `wlserver_mousewarp`.
Buttons already ride `wlserver_mousebutton`, but without motion/warp the pointer never
has position/focus, so they are lost too.

### Evidence (live, 2026-07-19)

- `GAMESCOPE_INPUT_COUNTER` root atom increments per injected parent event (gamescope
  receives everything), while `XSelectInput` spying on the BPM window sees KeyPress but
  **zero** MotionNotify/ButtonPress; `STEAM_TOUCH_CLICK_MODE(CARDINAL) = 4` confirmed.
- Reproduced identically with the untouched upstream GOW steam image
  (`ghcr.io/games-on-whales/steam:edge`) on the same home directory — not
  image/launcher-specific.
- Refuted alternatives (all measured, all still dead): `--xwayland-count 2` (two
  Xwaylands appear, no change), `--force-grab-cursor` (lock never activates without
  parent-side focus; binding the relative pointer kills the absolute path = strictly
  worse), synthetic X windows with/without `STEAM_GAME` atoms.
- After the patch: `XQueryPointer` in gamescope's Xwayland tracks injected parent motion
  pre-game (0,0 → 32,0), and end-user validation: mouse movement + clicks, keyboard and
  controller all working in BPM **and** in-game, no game-launch heal needed.

### Fix (what the PR should contain)

One argument at the nested backend call site — pass `bAlwaysWarpCursor = true`:

```cpp
-        wlserver_touchmotion( flX, flY, 0, ++m_uFakeTimestamp );
+        wlserver_touchmotion( flX, flY, 0, ++m_uFakeTimestamp, true );
```

Rationale for upstream: the nested parent pointer *is* a real mouse; Passthrough exists
for actual touchscreens. `wlserver_touchmotion` already performs the full
output-scale/offset transform before the warp, so the change composes with all touch
modes (Passthrough gains the warp; Left/Right/Middle/Trackpad behaviour unchanged).
Alternative shapes if upstream objects: a dedicated cvar
(e.g. `wayland_nested_pointer_warp`, default on for the Wayland backend), or routing
nested absolute motion through `wlserver_mousewarp` directly with the same transform.

---

## Validation recipe (for either PR discussion)

Headless, no human: run a wayland client under the compositor (or gamescope+Steam),
inject evdev into the compositor's virtual input devices, then:
- compositor level: client log shows `wl_pointer enter/motion/button`;
- gamescope level: `xprop -root GAMESCOPE_INPUT_COUNTER` (receipt),
  `XQueryPointer` position delta (forwarding), `XSelectInput` KeyPress/MotionNotify
  spy on the focused window (delivery). Python-ctypes one-liners for all three live in
  the 2026-07-19 session notes (no extra packages needed in the container).
