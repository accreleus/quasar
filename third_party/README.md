# third_party

Vendored upstream forks — added **only when modification is needed** (do not fork prematurely; until then, depend on / build upstream):

- `gst-wayland-display` — fork of `games-on-whales/gst-wayland-display` (the Wayland compositor). Reused, not rebuilt. Phase 0 needs zero changes to it.
- `inputtino` — fork of `games-on-whales/inputtino` (virtual input). Needed Phase 1 for containerized games reading evdev.

When added, vendor as git submodules and track upstream so fixes can be pulled. Confirm MIT compatibility.
