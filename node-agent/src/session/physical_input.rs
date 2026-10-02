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
//! ## Devices come and go during a session (#421)
//! The manager thread rescans `/dev/input` every [`HOTPLUG_INTERVAL`] for the
//! session's whole life, whatever `auto_connect_controller` says: under `"auto"`
//! a device that arrives mid-session is grabbed, one that leaves is released.
//! What each rescan does is decided by the pure [`resolve_candidates`] and
//! [`Tracker`], which also keep a node that is not wanted, or keeps failing,
//! from being probed and logged every rescan. The agent sees new nodes because
//! it binds the host's `/dev/input` directory, not the nodes present at start.
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

use anyhow::{Context, Result};
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

/// One `/dev/input/eventN` node seen by a rescan. `identity` tells two
/// devices that held the same `eventN` apart: the node's sysfs link
/// (`…/input/inputM/eventN`), whose `inputM` the kernel never reuses. A
/// renumbered `eventN` therefore reads as a different device, never as the one
/// that used to sit there.
#[derive(Debug, Clone, PartialEq, Eq)]
struct InputNode {
    path: PathBuf,
    label: String,
    identity: String,
}

/// Which device classes a candidate may be grabbed as.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Accept {
    /// Any keyboard, mouse or gamepad.
    Any,
    /// Only a gamepad (`auto_connect_controller` with an explicit device list).
    GamepadOnly,
}

impl Accept {
    fn admits(self, kind: DeviceKind) -> bool {
        match (self, kind) {
            (_, DeviceKind::Other) => false,
            (Accept::Any, _) => true,
            (Accept::GamepadOnly, k) => k == DeviceKind::Gamepad,
        }
    }
}

/// A node the selector wants this session to try to grab.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Candidate {
    path: PathBuf,
    identity: String,
    accept: Accept,
}

/// Resolve `console_config.input_devices` (`"auto"` or an array of path
/// strings) against the nodes present right now. Re-run on every rescan, so a
/// device that arrives mid-session is a candidate as soon as its node exists.
///
/// - `"auto"`: every present node, any class.
/// - an array: each listed path once its node is present; with
///   `auto_connect_controller`, also any other present node, gamepads only.
/// - anything else: nothing (fail closed).
///
/// Quasar's own virtual devices are never candidates. Excluded by NAME as well
/// as by the session's own paths: `eventN` numbers shift as sessions create and
/// destroy uinput nodes, so a listed or remembered path can come to name a
/// "Quasar Virtual …" node, and grabbing one either self-feeds forever or locks
/// the compositor's own seat out.
fn resolve_candidates(
    input_devices: &serde_json::Value,
    auto_connect_controller: bool,
    nodes: &[InputNode],
    virtual_paths: &[PathBuf],
) -> Vec<Candidate> {
    let eligible =
        |n: &&InputNode| !n.label.contains("Quasar Virtual") && !virtual_paths.contains(&n.path);
    let candidate = |n: &InputNode, accept| Candidate {
        path: n.path.clone(),
        identity: n.identity.clone(),
        accept,
    };
    match input_devices {
        serde_json::Value::String(s) if s == "auto" => nodes
            .iter()
            .filter(eligible)
            .map(|n| candidate(n, Accept::Any))
            .collect(),
        serde_json::Value::Array(items) => {
            let listed: Vec<PathBuf> = items
                .iter()
                .filter_map(|v| v.as_str())
                .map(PathBuf::from)
                .collect();
            let mut out: Vec<Candidate> = Vec::new();
            for path in &listed {
                if out.iter().any(|c| &c.path == path) {
                    continue;
                }
                if let Some(n) = nodes.iter().filter(eligible).find(|n| &n.path == path) {
                    out.push(candidate(n, Accept::Any));
                }
            }
            if auto_connect_controller {
                for n in nodes.iter().filter(eligible) {
                    if !listed.contains(&n.path) {
                        out.push(candidate(n, Accept::GamepadOnly));
                    }
                }
            }
            out
        }
        _ => Vec::new(),
    }
}

/// How many times a node that failed to open or grab is tried before it is
/// left alone until it changes. At [`HOTPLUG_INTERVAL`] that is ~5 s, which
/// covers a fresh node whose access the host's udev rule has not applied yet.
const MAX_ATTEMPTS: u32 = 10;

/// What the session knows about one node it has tried.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Known {
    Grabbed {
        identity: String,
    },
    /// Not a class the candidate admits. Stable for the life of the device,
    /// so it is not probed again until the node changes.
    Rejected {
        identity: String,
    },
    /// Open or grab failed `attempts` times. Retried quietly up to
    /// [`MAX_ATTEMPTS`].
    Failed {
        identity: String,
        attempts: u32,
    },
}

impl Known {
    fn identity(&self) -> &str {
        match self {
            Known::Grabbed { identity }
            | Known::Rejected { identity }
            | Known::Failed { identity, .. } => identity,
        }
    }
}

/// The result of trying one candidate.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Outcome {
    Grabbed,
    Rejected,
    Failed,
}

/// What one rescan must do: release these grabs, then try these candidates.
#[derive(Debug, Default, PartialEq, Eq)]
struct RescanPlan {
    release: Vec<PathBuf>,
    attempt: Vec<Candidate>,
}

/// The session's memory of every node it has tried, and the pure decision of
/// what each rescan does. No I/O: the manager thread scans, asks for a plan,
/// carries it out, and reports each attempt back through [`Tracker::record`].
#[derive(Debug, Default)]
struct Tracker {
    known: std::collections::BTreeMap<PathBuf, Known>,
}

impl Tracker {
    /// `candidates` is this rescan's [`resolve_candidates`]; `dead` lists
    /// grabbed paths whose reader has stopped (the device went away, or a read
    /// failed).
    fn plan(&mut self, candidates: &[Candidate], dead: &[PathBuf]) -> RescanPlan {
        let mut plan = RescanPlan::default();
        // Forget, and release, whatever is gone, deselected, renumbered, or dead.
        self.known.retain(|path, known| {
            let current = candidates.iter().find(|c| &c.path == path);
            let same_device = current.is_some_and(|c| c.identity == known.identity());
            let grabbed = matches!(known, Known::Grabbed { .. });
            if grabbed && (!same_device || dead.contains(path)) {
                plan.release.push(path.clone());
                if same_device {
                    // The node is still here but its reader stopped: count it
                    // as a failure so a node that keeps failing is not grabbed
                    // and dropped every rescan forever.
                    *known = Known::Failed {
                        identity: known.identity().to_string(),
                        attempts: 1,
                    };
                    return true;
                }
                return false;
            }
            same_device
        });
        for c in candidates {
            let retry = match self.known.get(&c.path) {
                None => true,
                Some(Known::Failed { attempts, .. }) => *attempts < MAX_ATTEMPTS,
                Some(Known::Grabbed { .. } | Known::Rejected { .. }) => false,
            };
            if retry {
                plan.attempt.push(c.clone());
            }
        }
        plan
    }

    /// Record an attempt's outcome. Returns true when the outcome is news
    /// worth a log line: the first failure or rejection of this device, not
    /// every quiet retry after it.
    fn record(&mut self, candidate: &Candidate, outcome: Outcome) -> bool {
        let identity = candidate.identity.clone();
        let prior = self.known.get(&candidate.path);
        let (next, news) = match outcome {
            Outcome::Grabbed => (Known::Grabbed { identity }, true),
            Outcome::Rejected => (Known::Rejected { identity }, true),
            Outcome::Failed => match prior {
                Some(Known::Failed { attempts, .. }) => (
                    Known::Failed {
                        identity,
                        attempts: attempts + 1,
                    },
                    false,
                ),
                _ => (
                    Known::Failed {
                        identity,
                        attempts: 1,
                    },
                    true,
                ),
            },
        };
        self.known.insert(candidate.path.clone(), next);
        news
    }
}

/// Every `/dev/input/eventN` node present now, with its name and identity.
fn scan_input_nodes() -> Vec<InputNode> {
    crate::capacity::detect_input_devices()
        .into_iter()
        .map(|d| {
            let event = Path::new(&d.path)
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            let identity = std::fs::read_link(format!("/sys/class/input/{event}"))
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_default();
            InputNode {
                path: PathBuf::from(d.path),
                label: d.label,
                identity,
            }
        })
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

/// Why one candidate was not grabbed.
enum StartError {
    /// Its class is not one the candidate admits (see [`Accept`]).
    Rejected(DeviceKind),
    /// Open, classify, or grab failed; may succeed on a later try.
    Failed(anyhow::Error),
}

impl GrabbedDevice {
    /// Open, classify, grab, and start forwarding one physical device.
    /// Returns an error (never panics) for any device this session should
    /// skip — the caller treats each device independently (best-effort).
    fn start(
        path: &Path,
        accept: Accept,
        devices: Arc<VirtualDevices>,
    ) -> std::result::Result<Self, StartError> {
        let file = open_physical(path).map_err(StartError::Failed)?;
        let handle = EvdevHandle::new(file);
        let kind = classify(&handle).map_err(StartError::Failed)?;
        if !accept.admits(kind) {
            return Err(StartError::Rejected(kind));
        }
        handle
            .grab(true)
            .with_context(|| format!("EVIOCGRAB {path:?}"))
            .map_err(StartError::Failed)?;
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
    /// Grab+forward every keyboard/mouse/gamepad `input_devices` selects, and
    /// keep doing so for the session's life: a rescan every
    /// [`HOTPLUG_INTERVAL`] grabs a device that arrives mid-session and
    /// releases one that leaves. Best-effort per device: a device that fails
    /// to open, classify, or grab is skipped with one warning and never fails
    /// the session.
    pub fn start(
        input_devices: &serde_json::Value,
        auto_connect_controller: bool,
        virtual_devices: &Arc<VirtualDevices>,
    ) -> Self {
        Self::start_with(
            input_devices,
            auto_connect_controller,
            virtual_devices,
            scan_input_nodes,
        )
    }

    /// [`Self::start`] with the node scan supplied: a real-kernel test scans
    /// only its own devices, so it never grabs the input of the host it runs on.
    fn start_with(
        input_devices: &serde_json::Value,
        auto_connect_controller: bool,
        virtual_devices: &Arc<VirtualDevices>,
        mut scan: impl FnMut() -> Vec<InputNode> + Send + 'static,
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
                let mut grabbed: std::collections::BTreeMap<PathBuf, GrabbedDevice> =
                    std::collections::BTreeMap::new();
                let mut tracker = Tracker::default();
                while !stop2.load(Ordering::Acquire) {
                    let nodes = scan();
                    let candidates = resolve_candidates(
                        &selector,
                        auto_connect_controller,
                        &nodes,
                        &virtual_paths,
                    );
                    let dead: Vec<PathBuf> = grabbed
                        .values()
                        .filter(|g| g.thread.as_ref().is_none_or(|t| t.is_finished()))
                        .map(|g| g.path.clone())
                        .collect();
                    let plan = tracker.plan(&candidates, &dead);
                    for path in &plan.release {
                        if grabbed.remove(path).is_some() {
                            tracing::info!("console-mode: physical input detached {path:?}");
                        }
                    }
                    for c in &plan.attempt {
                        if stop2.load(Ordering::Acquire) {
                            break;
                        }
                        match GrabbedDevice::start(&c.path, c.accept, devices.clone()) {
                            Ok(g) => {
                                tracker.record(c, Outcome::Grabbed);
                                tracing::info!(
                                    "console-mode: grabbed physical input device {:?} (kind={:?})",
                                    g.path,
                                    g.kind
                                );
                                grabbed.insert(c.path.clone(), g);
                            }
                            Err(StartError::Rejected(kind)) => {
                                if tracker.record(c, Outcome::Rejected) {
                                    tracing::info!(
                                        "console-mode: not grabbing physical device {:?} \
                                         (kind={kind:?}, accepts {:?})",
                                        c.path,
                                        c.accept
                                    );
                                }
                            }
                            Err(StartError::Failed(e)) => {
                                if tracker.record(c, Outcome::Failed) {
                                    tracing::warn!(
                                        token = "phys-input-device-skipped",
                                        "console-mode: skipping physical device {:?}: {e:#} \
                                         (retrying quietly for a few seconds)",
                                        c.path
                                    );
                                }
                            }
                        }
                    }
                    std::thread::sleep(HOTPLUG_INTERVAL);
                }
                // `grabbed` drops here: every device is ungrabbed before the
                // manager thread exits, so PhysicalInput::drop's join covers it.
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
    use serde_json::json;

    fn vp() -> Vec<PathBuf> {
        vec![
            PathBuf::from("/dev/input/event10"),
            PathBuf::from("/dev/input/event11"),
            PathBuf::from("/dev/input/event12"),
        ]
    }

    /// A node: `eventN`, its name, and the kernel's `inputM` registration.
    fn node(n: u32, label: &str, input: u32) -> InputNode {
        InputNode {
            path: PathBuf::from(format!("/dev/input/event{n}")),
            label: label.to_string(),
            identity: format!("../../devices/usb/input/input{input}/event{n}"),
        }
    }

    fn host() -> Vec<InputNode> {
        vec![
            node(3, "Logitech USB Receiver", 3),
            node(4, "Logitech USB Receiver Mouse", 4),
            node(10, "Quasar Virtual Keyboard", 40),
            node(11, "Quasar Virtual Mouse", 41),
            node(12, "Quasar Virtual Gamepad", 42),
        ]
    }

    fn paths(c: &[Candidate]) -> Vec<PathBuf> {
        c.iter().map(|c| c.path.clone()).collect()
    }

    fn p(n: u32) -> PathBuf {
        PathBuf::from(format!("/dev/input/event{n}"))
    }

    /// "auto" takes every present node except Quasar's own virtual devices.
    #[test]
    fn auto_selects_present_nodes_minus_virtual() {
        let out = resolve_candidates(&json!("auto"), false, &host(), &vp());
        assert_eq!(paths(&out), vec![p(3), p(4)]);
        assert!(out.iter().all(|c| c.accept == Accept::Any));
    }

    /// Another session's virtual device is excluded by name, even at a path
    /// that is not this session's.
    #[test]
    fn auto_excludes_other_sessions_virtual_devices_by_name() {
        let mut nodes = host();
        nodes.push(node(20, "Quasar Virtual Keyboard", 50));
        let out = resolve_candidates(&json!("auto"), false, &nodes, &vp());
        assert_eq!(paths(&out), vec![p(3), p(4)]);
    }

    /// An explicit array selects exactly those paths, minus any virtual ones.
    #[test]
    fn explicit_array_excludes_virtual_devices() {
        let cfg = json!(["/dev/input/event3", "/dev/input/event10"]);
        let out = resolve_candidates(&cfg, false, &host(), &vp());
        assert_eq!(paths(&out), vec![p(3)]);
    }

    /// A listed path renumbered onto a Quasar virtual device is never a candidate.
    #[test]
    fn explicit_array_never_selects_a_renumbered_virtual_node() {
        let cfg = json!(["/dev/input/event5"]);
        let nodes = vec![node(5, "Quasar Virtual Mouse", 60)];
        assert!(resolve_candidates(&cfg, false, &nodes, &vp()).is_empty());
    }

    /// A listed path that is not present yet is not a candidate until it appears.
    #[test]
    fn explicit_array_picks_up_a_listed_path_that_appears_later() {
        let cfg = json!(["/dev/input/event7"]);
        assert!(resolve_candidates(&cfg, false, &host(), &vp()).is_empty());
        let mut nodes = host();
        nodes.push(node(7, "8BitDo Pro 2", 7));
        assert_eq!(
            paths(&resolve_candidates(&cfg, false, &nodes, &vp())),
            vec![p(7)]
        );
    }

    /// With auto_connect_controller, an explicit list also takes any other
    /// present node, but only as a gamepad.
    #[test]
    fn explicit_array_with_auto_connect_controller_adds_gamepads_only() {
        let cfg = json!(["/dev/input/event3"]);
        let out = resolve_candidates(&cfg, true, &host(), &vp());
        assert_eq!(
            out,
            vec![
                Candidate {
                    path: p(3),
                    identity: host()[0].identity.clone(),
                    accept: Accept::Any
                },
                Candidate {
                    path: p(4),
                    identity: host()[1].identity.clone(),
                    accept: Accept::GamepadOnly
                },
            ]
        );
        assert!(Accept::GamepadOnly.admits(DeviceKind::Gamepad));
        assert!(!Accept::GamepadOnly.admits(DeviceKind::Keyboard));
        assert!(!Accept::Any.admits(DeviceKind::Other));
    }

    /// A non-"auto" string or any other JSON shape resolves to no devices (fail closed).
    #[test]
    fn unrecognized_shape_resolves_empty() {
        for cfg in [serde_json::Value::Null, json!(42), json!("nope")] {
            assert!(resolve_candidates(&cfg, true, &host(), &vp()).is_empty());
        }
    }

    /// Explicit array preserves caller order and drops unknown/non-string
    /// entries rather than erroring.
    #[test]
    fn explicit_array_filters_non_strings() {
        let cfg = json!(["/dev/input/event4", 5, null, "/dev/input/event3"]);
        let out = resolve_candidates(&cfg, false, &host(), &vp());
        assert_eq!(paths(&out), vec![p(4), p(3)]);
    }

    fn auto(nodes: &[InputNode]) -> Vec<Candidate> {
        resolve_candidates(&json!("auto"), false, nodes, &vp())
    }

    /// Carry out a plan with fixed outcomes per path, as the manager does.
    fn run(
        t: &mut Tracker,
        c: &[Candidate],
        dead: &[PathBuf],
        ok: &[(u32, Outcome)],
    ) -> RescanPlan {
        let plan = t.plan(c, dead);
        for a in &plan.attempt {
            let outcome = ok
                .iter()
                .find(|(n, _)| p(*n) == a.path)
                .map(|(_, o)| *o)
                .unwrap_or(Outcome::Grabbed);
            t.record(a, outcome);
        }
        plan
    }

    /// The issue (#421): a keyboard plugged in mid-session is attempted on the
    /// next rescan, and the already-grabbed ones are left alone.
    #[test]
    fn a_device_that_arrives_mid_session_is_attempted() {
        let mut t = Tracker::default();
        let first = run(&mut t, &auto(&host()), &[], &[]);
        assert_eq!(paths(&first.attempt), vec![p(3), p(4)]);

        let mut nodes = host();
        nodes.push(node(13, "USB Keyboard", 13));
        let next = run(&mut t, &auto(&nodes), &[], &[]);
        assert_eq!(next.release, Vec::<PathBuf>::new());
        assert_eq!(paths(&next.attempt), vec![p(13)]);

        let idle = t.plan(&auto(&nodes), &[]);
        assert_eq!(idle, RescanPlan::default());
    }

    /// A device that leaves is released, and forgotten.
    #[test]
    fn a_device_that_leaves_is_released() {
        let mut t = Tracker::default();
        run(&mut t, &auto(&host()), &[], &[]);
        let nodes: Vec<InputNode> = host().into_iter().filter(|n| n.path != p(4)).collect();
        let plan = run(&mut t, &auto(&nodes), &[], &[]);
        assert_eq!(plan.release, vec![p(4)]);
        assert!(plan.attempt.is_empty());
        assert!(!t.known.contains_key(&p(4)));
    }

    /// The same eventN now naming a different device (a new inputM) releases
    /// the old grab and attempts the new device.
    #[test]
    fn a_renumbered_node_is_released_and_reattempted() {
        let mut t = Tracker::default();
        run(&mut t, &auto(&host()), &[], &[]);
        let mut nodes = host();
        nodes[0] = node(3, "Xbox Wireless Controller", 77);
        let plan = run(&mut t, &auto(&nodes), &[], &[]);
        assert_eq!(plan.release, vec![p(3)]);
        assert_eq!(paths(&plan.attempt), vec![p(3)]);
    }

    /// A device of no wanted class is probed once and logged once, not every
    /// rescan; it is probed again only when its node changes.
    #[test]
    fn a_rejected_device_is_not_reprobed_until_it_changes() {
        let mut t = Tracker::default();
        let first = t.plan(&auto(&host()), &[]);
        assert!(t.record(&first.attempt[0], Outcome::Rejected));
        t.record(&first.attempt[1], Outcome::Grabbed);
        assert_eq!(t.plan(&auto(&host()), &[]), RescanPlan::default());

        let mut nodes = host();
        nodes[0] = node(3, "Power Button", 90);
        assert_eq!(paths(&t.plan(&auto(&nodes), &[]).attempt), vec![p(3)]);
    }

    /// A node that fails to open (the host has not applied its access yet) is
    /// retried quietly, warned about once, and left alone after MAX_ATTEMPTS.
    #[test]
    fn a_failing_device_is_retried_quietly_then_left_alone() {
        let mut t = Tracker::default();
        let c = auto(&host());
        let mut warnings = 0;
        let mut attempts = 0;
        for _ in 0..(MAX_ATTEMPTS + 5) {
            let plan = t.plan(&c, &[]);
            for a in &plan.attempt {
                if a.path == p(3) {
                    attempts += 1;
                    if t.record(a, Outcome::Failed) {
                        warnings += 1;
                    }
                } else {
                    t.record(a, Outcome::Grabbed);
                }
            }
        }
        assert_eq!(warnings, 1);
        assert_eq!(attempts, MAX_ATTEMPTS);
    }

    /// A failing node that later succeeds (the access arrived) is grabbed.
    #[test]
    fn a_failing_device_that_becomes_readable_is_grabbed() {
        let mut t = Tracker::default();
        run(&mut t, &auto(&host()), &[], &[(3, Outcome::Failed)]);
        let plan = run(&mut t, &auto(&host()), &[], &[]);
        assert_eq!(paths(&plan.attempt), vec![p(3)]);
        assert_eq!(
            t.known.get(&p(3)),
            Some(&Known::Grabbed {
                identity: host()[0].identity.clone()
            })
        );
    }

    /// A grabbed device whose reader stopped while its node remains is released
    /// and retried as a failure, so it cannot cycle grab/drop forever.
    #[test]
    fn a_dead_reader_on_a_present_node_is_released_and_bounded() {
        let mut t = Tracker::default();
        run(&mut t, &auto(&host()), &[], &[]);
        let plan = t.plan(&auto(&host()), &[p(3)]);
        assert_eq!(plan.release, vec![p(3)]);
        assert_eq!(paths(&plan.attempt), vec![p(3)]);
        assert!(matches!(
            t.known.get(&p(3)),
            Some(Known::Failed { attempts: 1, .. })
        ));
    }

    /// #421 against the real kernel: a keyboard created while the session's
    /// manager runs is grabbed and forwarded into the session's virtual
    /// keyboard, released when it leaves the scan, and grabbed again when it
    /// comes back. The scan sees only this test's devices (its tag), so a run
    /// never grabs the input of the host it runs on. Needs `/dev/uinput`, root,
    /// and the host's `/dev/input` (the agent's own bind).
    #[test]
    #[ignore = "needs /dev/uinput, the host's /dev/input and root: make test-uinput"]
    fn uinput_physical_input_grabs_a_device_plugged_in_mid_session() {
        use input_linux::{InputId, UInputHandle};
        use std::time::Instant;

        let test = "uinput_physical_input_grabs_a_device_plugged_in_mid_session";
        if let Err(e) = OpenOptions::new().write(true).open("/dev/uinput") {
            assert!(
                std::env::var_os("QUASAR_REQUIRE_UINPUT").is_none(),
                "{test}: /dev/uinput not usable ({e}), and QUASAR_REQUIRE_UINPUT is set"
            );
            eprintln!("SKIP {test}: /dev/uinput not usable ({e})");
            return;
        }
        let tag = format!("hotplug-{:08x}", std::process::id());
        let devs = Arc::new(VirtualDevices::create(&tag).expect("virtual devices"));

        // The scan the manager sees: this test's nodes, while `present`.
        let present = Arc::new(AtomicBool::new(true));
        let (scan_tag, scan_present) = (tag.clone(), present.clone());
        let scan = move || {
            scan_input_nodes()
                .into_iter()
                .filter(|n| n.label.contains(&scan_tag) && scan_present.load(Ordering::Acquire))
                .collect()
        };
        let physical = PhysicalInput::start_with(&json!("auto"), false, &devs, scan);
        std::thread::sleep(HOTPLUG_INTERVAL * 2);

        // Plug a keyboard in mid-session.
        let uinput = OpenOptions::new()
            .read(true)
            .write(true)
            .open("/dev/uinput")
            .unwrap();
        let plugged = UInputHandle::new(uinput);
        plugged.set_evbit(EventKind::Key).unwrap();
        plugged.set_evbit(EventKind::Synchronize).unwrap();
        for key in [Key::A, Key::B, Key::Enter] {
            plugged.set_keybit(key).unwrap();
        }
        let id = InputId {
            bustype: 0x03,
            vendor: 0x1234,
            product: 0x0421,
            version: 1,
        };
        let name = format!("Hotplug Test Keyboard [{tag}]");
        plugged.create(&id, name.as_bytes(), 0, &[]).unwrap();
        let node = plugged.evdev_path().unwrap();

        // Grabbed: another opener's EVIOCGRAB is refused while the session holds it.
        let held_elsewhere = |want: bool| {
            let deadline = Instant::now() + Duration::from_secs(3);
            loop {
                let probe = EvdevHandle::new(open_physical(&node).unwrap());
                let grabbed_by_us = probe.grab(true).is_ok();
                if grabbed_by_us {
                    probe.grab(false).unwrap();
                }
                if grabbed_by_us != want {
                    return;
                }
                assert!(
                    Instant::now() < deadline,
                    "{node:?}: session grab {} within 3 s",
                    if want { "not taken" } else { "not released" }
                );
                std::thread::sleep(Duration::from_millis(50));
            }
        };
        held_elsewhere(true);

        // Forwarded: a key on the plugged keyboard arrives on the virtual one.
        let virtual_kb = EvdevHandle::new(
            OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(&devs.keyboard_path)
                .unwrap(),
        );
        let frame = |code: Key, value: i32| {
            let ev = |type_: i32, code: u16, value: i32| isys::input_event {
                time: isys::timeval {
                    tv_sec: 0,
                    tv_usec: 0,
                },
                type_: type_ as u16,
                code,
                value,
            };
            [
                ev(isys::EV_KEY, code as u16, value),
                ev(isys::EV_SYN, isys::SYN_REPORT as u16, 0),
            ]
        };
        plugged.write(&frame(Key::A, 1)).unwrap();
        plugged.write(&frame(Key::A, 0)).unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        let zero = frame(Key::A, 0)[1];
        let mut seen = Vec::new();
        while !seen.contains(&(Key::A as u16, 1)) {
            assert!(
                Instant::now() < deadline,
                "KEY_A never reached the virtual keyboard: {seen:?}"
            );
            let mut buf = [zero; 16];
            match virtual_kb.read(&mut buf) {
                Ok(n) => seen.extend(
                    buf[..n]
                        .iter()
                        .filter(|e| e.type_ == isys::EV_KEY as u16)
                        .map(|e| (e.code, e.value)),
                ),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(10))
                }
                Err(e) => panic!("read virtual keyboard: {e}"),
            }
        }

        // Leaves the scan (unplugged, as the manager sees it): released.
        present.store(false, Ordering::Release);
        held_elsewhere(false);
        // Comes back: grabbed again.
        present.store(true, Ordering::Release);
        held_elsewhere(true);

        // Session end releases it too.
        drop(physical);
        held_elsewhere(false);
        drop(plugged);
    }
}
