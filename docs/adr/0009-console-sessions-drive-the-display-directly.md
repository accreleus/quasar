---
status: accepted
date: 2026-10-05
---
# Console sessions drive the display directly

A console session is a session whose desktop owns the host's own screen (`CONTEXT.md`: console
session, direct display). We decided that **the desktop drives the display itself**: the node
agent hands the session's container the GPU's card node, the input devices, the sound device and
the host's udev event stream, and the desktop inside opens the display the way it does on bare
metal — KWin on its DRM backend, gamescope on its DRM backend. Quasar puts **no compositor,
pipeline or display server of its own** between a console desktop and the monitor, and a console
session is **never streamed** (#453).

## Why not the streaming path

The first console mode rendered the desktop into the streaming compositor, carried frames through
a media pipeline, and had a second display server (weston) own the monitor and draw them. Three
hand-offs sat between the desktop and the screen, and each one cost us: every desktop needed a
patch to pass its mode changes through the nested client (#445, #447), the copy path pegged a core
at 4K120 (#450), the no-copy path corrupted the picture because the display never says when the
GPU finished reading a buffer, and audio needed its own routing (#452). Dual output — streaming the
console session at the same time — was the one thing that path could do and direct display cannot,
and the operator chose to give it up.

## What the decision commits us to

- **One display owner per card.** The console desktop is DRM master of the card node; streamed
  sessions on the same GPU receive only the render node, so they share the GPU without ever being
  able to take the display. Capacity accounting treats the console as an ordinary session.
- **The desktop owns its settings.** Resolution, refresh, VRR, HDR, idle blanking and the audio
  output are the desktop's, not the admin UI's. The admin picks the card and connector, the
  default app and user, and which input devices the desktop may have.
- **Device access is granted, not brokered.** No seat daemon crosses a container boundary; each
  desktop opens its devices and becomes master by being the first opener. This is what makes the
  rootless engine a peer of the rootful one: the same plan, with permissions granted on the host.
- **The agent only launches, grants and watches.** It holds the virtual terminal, reports
  "displaying" from the container's liveness and the DRM state, and stops the session. It carries
  no frame counters and no privileges the console container needs for itself.
- **Driver workarounds live in the images.** gamescope's scanout buffers are allocated through GBM
  because the NVIDIA 610 driver corrupts Vulkan-allocated 4K scanout (upstream gamescope issue
  2309); the patch is named for that issue and goes when the driver is fixed.

Proven on the operator's console host before this was written: KDE at 3840x2160@240 with hotplug,
audio and input; Steam at 3840x2160@240 with the GBM scanout patch; a Vulkan workload in a second
container sharing the GPU throughout.
