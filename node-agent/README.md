# node-agent (Rust)

Per-host agent. Owns the host's GPUs, reports capacity to the control plane, and runs the sessions assigned to it: drives the GStreamer compositor (`gst-wayland-display`) + encoder and pushes the stream over the pluggable transport. Plugs virtual input via `inputtino` + fake-udev.

Built in Phase 1, growing out of `../spike/host`. Holds no account/scheduling logic — that is the control plane's job (see `../CLAUDE.md`).
