//! Console-mode physical input — grab physical keyboard/mouse evdev nodes
//! exclusively and forward their events into the session's virtual uinput
//! devices ([`super::virtual_input::VirtualDevices`]).
//!
//! ## Why forwarding, not a second compositor input path
//! `waylanddisplaysrc` opens exactly one mouse and one keyboard path via
//! libinput's path backend, always the session's virtual uinput nodes (what
//! keeps the WebRTC DataChannel path working). It cannot also point at the
//! physical devices (a second libinput seat, one `mouse`/`keyboard` property
//! each), so instead: grab each physical device exclusively (`EVIOCGRAB`,
//! removing it from the host's seat) and forward its raw events verbatim into
//! the matching virtual uinput device. The compositor sees both WebRTC-origin
//! and physical-origin input on the single path it already opens.
//!
//! ## Safety: the grab must be released on every exit path
//! `EVIOCGRAB` removes the device from every other reader on the host until
//! ungrabbed; a leaked grab locks physical keyboard/mouse input to the host.
//! [`GrabbedDevice::drop`] always calls `grab(false)` before the fd closes, and
//! [`PhysicalInput`] holds the grabs for exactly the session's lifetime
//! (dropped alongside `_local_display`/`_local_audio` in `runner.rs`, on every
//! stop/error path). A bad grab (or a panic between `grab(true)` and
//! installing the `Drop`) is a host-input-locked incident; the only universal
//! recovery is the session ending or a host reboot.

use std::fs::{File, OpenOptions};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use input_linux::sys as isys;
use input_linux::{EvdevHandle, EventKind, Key};

use super::virtual_input::VirtualDevices;

/// How long a reader thread sleeps between non-blocking read attempts while a
/// grabbed device is idle. Bounds worst-case shutdown latency (join wait) —
/// small enough to be imperceptible for input, large enough to not spin.
const POLL_INTERVAL: Duration = Duration::from_millis(5);
const HOTPLUG_INTERVAL: Duration = Duration::from_millis(500);

/// A physical device's evdev capability class. `Other` devices are skipped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DeviceKind {
    Mouse,
    Keyboard,
    Gamepad,
    Other,
}

/// Classify a device by its evdev capability bits. Mice expose `EV_REL`
/// (checked first, since a mouse can also expose a few `EV_KEY` buttons);
/// keyboards expose `EV_KEY` with `KEY_A`, a marker no mouse/gamepad sets.
fn classify(handle: &EvdevHandle<File>) -> Result<DeviceKind> {
    let bits = handle.event_bits().context("EVIOCGBIT (event_bits)")?;
    if bits.get(EventKind::Relative) {
        return Ok(DeviceKind::Mouse);
    }
    if bits.get(EventKind::Key) {
        if let Ok(keys) = handle.key_bits() {
            if keys.get(Key::A) {
                return Ok(DeviceKind::Keyboard);
            }
            if keys.get(Key::ButtonSouth) || keys.get(Key::ButtonTrigger) {
                return Ok(DeviceKind::Gamepad);
            }
        }
    }
    Ok(DeviceKind::Other)
}

/// Open a physical `/dev/input/eventN` node non-blocking, so the reader thread
/// can poll `stop` instead of hanging with no bound on shutdown latency.
/// Opened read+write, not read-only: some kernels are stricter about grabbing
/// a read-only fd.
fn open_physical(path: &Path) -> Result<File> {
    OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
        .with_context(|| format!("open {path:?}"))
}

/// Resolve `console_config.input_devices` (`"auto"` or an array of path
/// strings) into concrete device paths, excluding the session's own virtual
/// devices. This exclusion must hold: grabbing+forwarding a virtual device
/// into itself would self-feed forever.
fn resolve_device_paths(
    input_devices: &serde_json::Value,
    virtual_paths: &[PathBuf],
) -> Vec<PathBuf> {
    let candidates: Vec<PathBuf> = match input_devices {
        serde_json::Value::String(s) if s == "auto" => crate::capacity::detect_input_devices()
            .into_iter()
            // Exclude by NAME, not path: /dev/input/eventN numbers shift as
            // sessions create/destroy uinput nodes, so a stale/renumbered
            // "Quasar Virtual …" node could otherwise be grabbed and EVIOCGRAB
            // would lock the compositor's own seat out entirely.
            .filter(|d| !d.label.contains("Quasar Virtual"))
            .map(|d| PathBuf::from(d.path))
            .collect(),
        serde_json::Value::Array(items) => items
            .iter()
            .filter_map(|v| v.as_str())
            .map(PathBuf::from)
            .collect(),
        _ => Vec::new(),
    };
    // Also drop the session's own virtual paths (covers an explicit-array
    // config naming one; a no-op after the name filter for "auto").
    candidates
        .into_iter()
        .filter(|p| !virtual_paths.contains(p))
        .collect()
}

/// One grabbed physical device: its forwarding thread, and the ungrab-on-drop
/// safety net.
struct GrabbedDevice {
    path: PathBuf,
    kind: DeviceKind,
    stop: Arc<AtomicBool>,
    handle: Arc<EvdevHandle<File>>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl GrabbedDevice {
    /// Open, classify, grab, and start forwarding one physical device.
    /// Returns an error (never panics) for any device this session should
    /// skip — the caller treats each device independently (best-effort).
    fn start(path: &Path, devices: Arc<VirtualDevices>) -> Result<Self> {
        let file = open_physical(path)?;
        let handle = EvdevHandle::new(file);
        let kind = classify(&handle)?;
        if kind == DeviceKind::Other {
            bail!("not a mouse, keyboard, or gamepad (evdev capability probe) — skipped");
        }
        handle
            .grab(true)
            .with_context(|| format!("EVIOCGRAB {path:?}"))?;
        let handle = Arc::new(handle);
        let stop = Arc::new(AtomicBool::new(false));
        let thread = spawn_reader(
            path.to_path_buf(),
            handle.clone(),
            kind,
            devices,
            stop.clone(),
        );
        Ok(GrabbedDevice {
            path: path.to_path_buf(),
            kind,
            stop,
            handle,
            thread: Some(thread),
        })
    }
}

impl Drop for GrabbedDevice {
    fn drop(&mut self) {
        // Stop and join the reader before ungrabbing, so it's guaranteed gone
        // before the fd is released (order doesn't affect grab safety itself).
        self.stop.store(true, Ordering::Release);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
        match self.handle.grab(false) {
            Ok(()) => tracing::info!("console-mode: ungrabbed physical device {:?}", self.path),
            Err(e) => tracing::warn!(
                token = "phys-input-ungrab-failed",
                "console-mode: EVIOCGRAB(false) failed for {:?}: {e:#} — HOST INPUT MAY STILL \
                 BE LOCKED for this device; verify at the box",
                self.path
            ),
        }
    }
}

/// Reader thread body: non-blocking poll loop that forwards every readable
/// frame verbatim into the matching virtual device. Exits when `stop` is set
/// or the device goes away, whichever comes first.
fn spawn_reader(
    path: PathBuf,
    handle: Arc<EvdevHandle<File>>,
    kind: DeviceKind,
    devices: Arc<VirtualDevices>,
    stop: Arc<AtomicBool>,
) -> std::thread::JoinHandle<()> {
    let thread_name = format!(
        "quasar-phys-{}",
        path.file_name().and_then(|n| n.to_str()).unwrap_or("dev")
    );
    let log_span = tracing::Span::current();
    std::thread::Builder::new()
        .name(thread_name)
        .spawn(move || {
            // Re-enter the session span so this thread's lines carry session=<id>.
            let _log_span = log_span.enter();
            // 16 events is generous for one kernel-batched frame; a larger
            // frame just splits harmlessly across two ordered forward writes.
            let zero = isys::input_event {
                time: isys::timeval {
                    tv_sec: 0,
                    tv_usec: 0,
                },
                type_: 0,
                code: 0,
                value: 0,
            };
            let mut buf = [zero; 16];
            loop {
                if stop.load(Ordering::Acquire) {
                    return;
                }
                match handle.read(&mut buf) {
                    Ok(0) => std::thread::sleep(POLL_INTERVAL),
                    Ok(n) => {
                        // Defensive: never index past the buffer.
                        let evs = &buf[..n.min(buf.len())];
                        let res = match kind {
                            DeviceKind::Mouse => devices.forward_mouse_frame(evs),
                            DeviceKind::Keyboard => devices.forward_keyboard_frame(evs),
                            DeviceKind::Gamepad => devices.forward_gamepad_frame(evs),
                            DeviceKind::Other => Ok(()),
                        };
                        if let Err(e) = res {
                            tracing::warn!(
                                token = "phys-input-forward-failed",
                                "console-mode: forward from {path:?} failed: {e:#}"
                            );
                        }
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        std::thread::sleep(POLL_INTERVAL);
                    }
                    Err(e) => {
                        if !stop.load(Ordering::Acquire) {
                            tracing::warn!(
                                token = "phys-input-read-failed",
                                "console-mode: read {path:?} failed: {e:#} — device likely \
                                 unplugged; stopping its reader"
                            );
                        }
                        return;
                    }
                }
            }
        })
        .expect("spawn physical-input reader thread")
}

/// Session-scoped set of grabbed physical devices, held for the session's
/// lifetime and dropped on every exit path (mirrors `_local_display`/
/// `_local_audio` in `runner.rs`); dropping ungrabs every device.
pub struct PhysicalInput {
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl PhysicalInput {
    /// Resolve `input_devices` and grab+forward every mouse/keyboard/gamepad.
    /// Best-effort per device: a device that fails to open, classify, or grab
    /// is skipped with a warning and never fails the session.
    pub fn start(
        input_devices: &serde_json::Value,
        auto_connect_controller: bool,
        virtual_devices: &Arc<VirtualDevices>,
    ) -> Self {
        let virtual_paths = vec![
            virtual_devices.keyboard_path.clone(),
            virtual_devices.mouse_path.clone(),
            virtual_devices.gamepad_path.clone(),
        ];
        let selector = input_devices.clone();
        let devices = virtual_devices.clone();
        let stop = Arc::new(AtomicBool::new(false));
        let stop2 = stop.clone();
        let log_span = tracing::Span::current();
        let thread = std::thread::Builder::new()
            .name("quasar-physical-input-manager".into())
            .spawn(move || {
                // Re-enter the session span so this thread's lines carry session=<id>.
                let _log_span = log_span.enter();
                let mut grabbed: Vec<GrabbedDevice> = Vec::new();
                loop {
                    let paths = resolve_device_paths(&selector, &virtual_paths);
                    grabbed.retain(|g| {
                        let alive = paths.contains(&g.path)
                            && g.thread.as_ref().is_some_and(|t| !t.is_finished());
                        if !alive {
                            tracing::info!("console-mode: physical input detached {:?}", g.path);
                        }
                        alive
                    });
                    for path in paths {
                        if grabbed.iter().any(|g| g.path == path) {
                            continue;
                        }
                        match GrabbedDevice::start(&path, devices.clone()) {
                            Ok(g) => {
                                tracing::info!(
                                    "console-mode: grabbed physical input device {:?} (kind={:?})",
                                    g.path,
                                    g.kind
                                );
                                grabbed.push(g);
                            }
                            Err(e) => tracing::warn!(
                                token = "phys-input-device-skipped",
                                "console-mode: skipping physical device {path:?}: {e:#}"
                            ),
                        }
                    }
                    if stop2.load(Ordering::Acquire) {
                        return;
                    }
                    if !auto_connect_controller {
                        while !stop2.load(Ordering::Acquire) {
                            std::thread::sleep(HOTPLUG_INTERVAL);
                        }
                        return;
                    }
                    std::thread::sleep(HOTPLUG_INTERVAL);
                }
            })
            .expect("spawn physical-input manager");
        PhysicalInput {
            stop,
            thread: Some(thread),
        }
    }
}

impl Drop for PhysicalInput {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn vp() -> Vec<PathBuf> {
        vec![
            PathBuf::from("/dev/input/event10"),
            PathBuf::from("/dev/input/event11"),
            PathBuf::from("/dev/input/event12"),
        ]
    }

    /// An explicit array selects exactly those paths, minus any virtual ones.
    #[test]
    fn explicit_array_excludes_virtual_devices() {
        let cfg = serde_json::json!(["/dev/input/event3", "/dev/input/event10"]);
        let out = resolve_device_paths(&cfg, &vp());
        assert_eq!(out, vec![PathBuf::from("/dev/input/event3")]);
    }

    /// A non-"auto" string or any other JSON shape resolves to no devices (fail closed).
    #[test]
    fn unrecognized_shape_resolves_empty() {
        assert!(resolve_device_paths(&serde_json::Value::Null, &vp()).is_empty());
        assert!(resolve_device_paths(&serde_json::json!(42), &vp()).is_empty());
        assert!(resolve_device_paths(&serde_json::json!("nope"), &vp()).is_empty());
    }

    /// Explicit array preserves caller order and drops unknown/non-string
    /// entries rather than erroring.
    #[test]
    fn explicit_array_filters_non_strings() {
        let cfg = serde_json::json!(["/dev/input/event3", 5, null, "/dev/input/event4"]);
        let out = resolve_device_paths(&cfg, &vp());
        assert_eq!(
            out,
            vec![
                PathBuf::from("/dev/input/event3"),
                PathBuf::from("/dev/input/event4"),
            ]
        );
    }
}
