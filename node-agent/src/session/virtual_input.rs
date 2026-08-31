//! Virtual input devices (uinput) — the real evdev input path for a session.
//!
//! Creates three real Linux virtual input devices via `uinput` (keyboard, mouse,
//! gamepad) as real evdev nodes (`/dev/input/eventN`), so:
//!   - the per-session compositor opens mouse + keyboard via libinput (node paths
//!     handed to `waylanddisplaysrc`'s `mouse`/`keyboard` properties), and
//!   - the container gets all three nodes bind-mounted (`--device`), so an
//!     evdev-reading game — and the gamepad, which has no Wayland protocol — sees
//!     real devices.
//!
//! The DataChannel `handle()` path (see [`super::input`]) writes here instead of
//! emitting `CustomUpstream`, so one input pipeline feeds both Wayland-native and
//! evdev-native apps.
//!
//! ## "fake-udev"
//! `waylanddisplaysrc` opens input nodes via libinput's path backend (plain
//! `open()`, no udev/seat). A container's `/dev` is a fresh tmpfs, so a
//! freshly-created uinput device has no node there until something makes one.
//! [`VirtualDevices::create`] `mknod`s each device's `/dev/input/eventN` from its
//! sysfs `dev` (major:minor) if the kernel didn't; Docker `--device` then
//! materializes the same node inside the container.
//!
//! Requires `/dev/uinput` (root). Wire format: `protocol/input.md`.

use std::fs::{File, OpenOptions};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};

use anyhow::{anyhow, Context, Result};
use input_linux::sys as isys;
use input_linux::{
    AbsoluteAxis, AbsoluteInfo, AbsoluteInfoSetup, EventKind, InputId, Key, RelativeAxis,
    UInputHandle,
};

/// A zeroed event timestamp — uinput stamps events itself on `write`.
const ZERO_TIME: isys::timeval = isys::timeval {
    tv_sec: 0,
    tv_usec: 0,
};

/// Analog stick range (signed 16-bit, the XInput/`xpad` convention). W3C axes
/// (-1.0..=1.0) scale onto this.
const STICK_MIN: i32 = -32768;
const STICK_MAX: i32 = 32767;
/// Trigger range (unsigned 8-bit, the `xpad` convention). W3C trigger buttons
/// (0.0..=1.0) scale onto this.
const TRIGGER_MIN: i32 = 0;
const TRIGGER_MAX: i32 = 255;

/// Default relative-mouse batching window in ms (Moonlight-derived): accumulates
/// integer relative motion for up to this long, then writes one uinput event +
/// SYN_REPORT for the whole burst, decoupled from the browser render clock.
/// `QUASAR_INPUT_BATCH_MS=0` disables batching (per-arrival writes, fractional
/// accumulation still active). See `.claude/rules/gstreamer-gotchas.md`.
const DEFAULT_INPUT_BATCH_MS: u64 = 1;

/// Pending relative mouse motion awaiting a batched uinput flush, plus the
/// dedicated flush thread's coordination state.
struct RelFlushState {
    /// Accumulated integer motion waiting for the next flush.
    pending: Mutex<(i32, i32)>,
    /// Notifies the flush thread when new motion arrives or a drain is requested.
    cv: Condvar,
    /// Ends the flush thread (graceful shutdown on drop/release_all).
    stop: AtomicBool,
    /// Drains `pending` immediately and wakes the thread, used by non-mm events
    /// so motion always precedes them.
    flush_now: AtomicBool,
    /// Batching window in ms; 0 = disabled (per-arrival write, no thread).
    batch_ms: u64,
}

impl RelFlushState {
    fn new(batch_ms: u64) -> Self {
        Self {
            pending: Mutex::new((0, 0)),
            cv: Condvar::new(),
            stop: AtomicBool::new(false),
            flush_now: AtomicBool::new(false),
            batch_ms,
        }
    }
}

/// Read `QUASAR_INPUT_BATCH_MS` once at process start (default 1, 0 = disabled).
fn input_batch_ms() -> u64 {
    static ONCE: std::sync::OnceLock<u64> = std::sync::OnceLock::new();
    *ONCE.get_or_init(|| {
        std::env::var("QUASAR_INPUT_BATCH_MS")
            .ok()
            .and_then(|s| s.parse().ok())
            .unwrap_or(DEFAULT_INPUT_BATCH_MS)
    })
}

/// One synthetic input event.
fn ev(type_: u16, code: u16, value: i32) -> isys::input_event {
    isys::input_event {
        time: ZERO_TIME,
        type_,
        code,
        value,
    }
}

/// `EV_SYN`/`SYN_REPORT` — flush a batch of events as one atomic frame.
fn syn() -> isys::input_event {
    ev(isys::EV_SYN as u16, isys::SYN_REPORT as u16, 0)
}

/// Write one relative-motion frame (REL_X, REL_Y, SYN_REPORT) to the mouse
/// handle. A no-op when both deltas are zero (avoids an empty SYN that some
/// compositors treat as a frame boundary with no motion).
fn write_rel_frame(mouse: &UInputHandle<File>, dx: i32, dy: i32) -> Result<()> {
    if dx == 0 && dy == 0 {
        return Ok(());
    }
    let evs = [
        ev(isys::EV_REL as u16, isys::REL_X as u16, dx),
        ev(isys::EV_REL as u16, isys::REL_Y as u16, dy),
        syn(),
    ];
    mouse.write(&evs).context("write mouse motion")?;
    Ok(())
}

/// Spawn the dedicated relative-mouse flush thread (Moonlight-style batching).
///
/// Loops: wait on the condvar (notified by `mouse_move_rel` on the first sample
/// of a burst), sleep up to `batch_ms` to coalesce further arrivals, then drain
/// `pending` under the lock and write one uinput frame. `flush_now` short-circuits
/// the sleep for non-`mm` events; `stop` drains and exits.
///
/// Must NOT snapshot-and-clear before sleeping — sleep first, then drain under
/// the lock right before writing, so motion arriving during the sleep is written
/// in order and `flush_pending_rel` always lands before a subsequent thread write.
fn spawn_rel_flush_thread(
    state: Arc<RelFlushState>,
    mouse: Arc<UInputHandle<File>>,
    batch_ms: u64,
) -> std::thread::JoinHandle<()> {
    let log_span = tracing::Span::current();
    std::thread::Builder::new()
        .name("quasar-rel-flush".into())
        .spawn(move || {
            // Re-enter the session span so this thread's lines carry session=<id>.
            let _log_span = log_span.enter();
            let window = std::time::Duration::from_millis(batch_ms);
            loop {
                // Wait for the first sample of a new burst, or flush_now/stop.
                let mut pending = state.pending.lock().unwrap();
                while (pending.0 == 0 && pending.1 == 0)
                    && !state.stop.load(Ordering::Acquire)
                    && !state.flush_now.load(Ordering::Acquire)
                {
                    pending = state.cv.wait(pending).unwrap();
                }
                if state.stop.load(Ordering::Acquire) {
                    // Final drain on shutdown so no motion is lost.
                    let (dx, dy) = *pending;
                    *pending = (0, 0);
                    drop(pending);
                    let _ = write_rel_frame(&mouse, dx, dy);
                    return;
                }
                // flush_now: a non-mm event already drained + wrote synchronously;
                // clear the flag and re-wait, no thread write needed.
                if state.flush_now.swap(false, Ordering::AcqRel) {
                    drop(pending);
                    continue;
                }
                drop(pending);
                std::thread::sleep(window);
                // flush_pending_rel may have drained concurrently, in which case
                // pending is 0 here and nothing is written.
                let (dx, dy) = {
                    let mut pending = state.pending.lock().unwrap();
                    let d = *pending;
                    *pending = (0, 0);
                    d
                };
                let _ = write_rel_frame(&mouse, dx, dy);
            }
        })
        .expect("spawn rel-flush thread")
}

/// Accumulate fractional relative motion and split off the integer part.
///
/// uinput `REL_X`/`REL_Y` are integers; rounding each `mm` message in isolation
/// quantizes sub-pixel motion (0.4+0.4+0.4 -> 0+0+0). `accum` carries the
/// fractional remainder across calls; emits the integer part via `trunc()`
/// (toward zero, so a negative remainder stays negative) and leaves the rest.
fn split_integer(accum: &mut (f64, f64), dx: f64, dy: f64) -> (i32, i32) {
    accum.0 += dx;
    accum.1 += dy;
    let ix = accum.0.trunc() as i32;
    let iy = accum.1.trunc() as i32;
    accum.0 -= ix as f64;
    accum.1 -= iy as f64;
    (ix, iy)
}

/// Open `/dev/uinput` for device creation.
fn open_uinput() -> Result<UInputHandle<File>> {
    let f = OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/uinput")
        .context(
            "open /dev/uinput — needs root and the uinput kernel module (and \
             the node mounted into the agent container: --device /dev/uinput)",
        )?;
    Ok(UInputHandle::new(f))
}

/// Per-session uinput device name: `tag` lets a host running several sessions
/// tell devices apart in logs and `/proc/bus/input/devices`. The evdev node
/// paths are kernel-allocated and always distinct; this is for observability.
fn device_name(kind: &str, tag: &str) -> String {
    format!("Quasar Virtual {kind} [{tag}]")
}

/// Stable-ish synthetic IDs so the devices are recognizable in logs / udev.
fn input_id(product: u16) -> InputId {
    InputId {
        bustype: isys::BUS_VIRTUAL,
        vendor: 0xab1e, // "able" — Quasar
        product,
        version: 1,
    }
}

/// Resolve the `/dev/input/eventN` path for a freshly-created uinput device,
/// ensure the node exists, and write a fake-udev record (`udev_props` are the
/// `ID_INPUT*` class) so libinput accepts it. Returns the path plus
/// `(major, minor)` for `export_udev_data` to republish for the app container.
fn resolve_and_ensure_node(
    handle: &UInputHandle<File>,
    udev_props: &[&str],
) -> Result<(PathBuf, (u32, u32))> {
    let path = handle
        .evdev_path()
        .context("uinput device created but its evdev node path could not be resolved")?;
    let (maj, min) = dev_major_minor(&path)?;
    ensure_dev_node(&path, maj, min)?;
    write_fake_udev_db(maj, min, udev_props);
    Ok((path, (maj, min)))
}

/// Host-shared directory holding this session's exported fake-udev records
/// (`{runtime_dir}/udev-{session_id}`), bind-mounted by the app container at
/// `/run/udev/data` so SDL/Steam (libudev discovery, not evdev scanning) see the
/// `--device`-passed gamepad node.
pub fn udev_export_dir(runtime_dir: &str, session_id: &str) -> PathBuf {
    PathBuf::from(runtime_dir).join(format!("udev-{session_id}"))
}

/// Read the `major:minor` of an input device's node from sysfs.
fn dev_major_minor(path: &Path) -> Result<(u32, u32)> {
    let name = path
        .file_name()
        .and_then(|n| n.to_str())
        .ok_or_else(|| anyhow!("evdev path {path:?} has no file name"))?;
    let dev = std::fs::read_to_string(format!("/sys/class/input/{name}/dev"))
        .with_context(|| format!("read /sys/class/input/{name}/dev for major:minor"))?;
    let dev = dev.trim();
    dev.split_once(':')
        .and_then(|(a, b)| Some((a.parse::<u32>().ok()?, b.parse::<u32>().ok()?)))
        .ok_or_else(|| anyhow!("unexpected sysfs dev format '{dev}' for {name}"))
}

/// `mknod` `path` (e.g. `/dev/input/event7`) if missing. No-op when the node
/// already exists AND is the expected char device (right major:minor) — this is
/// what lets Docker `--device` and libinput's path backend open it in a tmpfs
/// `/dev`.
///
/// #378: a prior session's `docker rm -f` can leave a bind-mount artifact (a
/// regular file/dir, or a char device with a stale major:minor) at this exact
/// path. A bare `path.exists()` treated any of those as "done" and every later
/// launch failed opening it. Validate type + rdev and self-heal a stale/wrong
/// artifact before falling through to `mknod`.
fn ensure_dev_node(path: &Path, maj: u32, min: u32) -> Result<()> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) => {
            let expected = libc::makedev(maj, min);
            let is_expected_char_device =
                meta.file_type().is_char_device() && meta.rdev() == expected;
            if is_expected_char_device {
                return Ok(());
            }
            std::fs::remove_file(path)
                .with_context(|| format!("remove stale artifact at {path:?}"))?;
            tracing::info!(
                token = "vinput-stale-artifact-removed",
                "removed stale non-device artifact at {path:?} (expected char device {maj}:{min})"
            );
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(e).with_context(|| format!("stat {path:?}")),
    }
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    let cpath = std::ffi::CString::new(path.as_os_str().as_bytes())?;
    // Character device, rw for owner+group (root in the dev image).
    let mode = libc::S_IFCHR | 0o660;
    let rc = unsafe { libc::mknod(cpath.as_ptr(), mode, libc::makedev(maj, min)) };
    if rc != 0 {
        return Err(std::io::Error::last_os_error())
            .with_context(|| format!("mknod {path:?} (c {maj}:{min})"));
    }
    tracing::debug!("mknod {path:?} (c {maj}:{min})");
    Ok(())
}

/// Write a minimal **fake-udev** database record at `/run/udev/data/c<maj>:<min>`.
///
/// libinput's path backend still asks udev whether a device is "initialized" and
/// how it's classified by reading this file; with no `udevd` in the container a
/// fresh uinput device has no record and libinput rejects it. The `I:` line marks
/// it initialized, the `E:` lines carry the `ID_INPUT*` class. Best-effort: only
/// compositor-seat injection depends on it, not direct evdev/gamepad access.
fn write_fake_udev_db(maj: u32, min: u32, props: &[&str]) {
    let dir = "/run/udev/data";
    if let Err(e) = std::fs::create_dir_all(dir) {
        tracing::warn!(
            token = "fake-udev-mkdir-failed",
            "fake-udev: cannot create {dir}: {e} — compositor input may not bind"
        );
        return;
    }
    let path = format!("{dir}/c{maj}:{min}");
    // If a record already exists, a real udevd processed the device — leave its
    // (richer) record alone. We only synthesize one when udev is absent.
    if Path::new(&path).exists() {
        tracing::debug!("fake-udev: {path} already present (udevd active) — leaving it");
        return;
    }
    match std::fs::write(&path, udev_record_body(props)) {
        Ok(()) => tracing::debug!("fake-udev: wrote {path} ({})", props.join(",")),
        Err(e) => tracing::warn!(
            token = "fake-udev-write-failed",
            "fake-udev: cannot write {path}: {e}"
        ),
    }
}

/// Serialize a minimal udev database record.
/// `I:<usec>` = initialization timestamp (presence ⇒ is_initialized==true).
fn udev_record_body(props: &[&str]) -> String {
    let mut body = String::from("I:1\n");
    for p in props {
        body.push_str("E:");
        body.push_str(p);
        body.push('\n');
    }
    body.push_str("G:seat\n");
    body
}

/// Tracks which keys, mouse buttons, and pad buttons are currently held, and
/// whether any pad analog has last been emitted nonzero. Pure (no I/O), so
/// unit-testable without uinput.
#[derive(Default)]
struct HeldInputs {
    keys: std::collections::BTreeSet<u16>,
    mouse_buttons: std::collections::BTreeSet<u16>,
    pad_buttons: std::collections::BTreeSet<u16>,
    /// True when any trigger or stick was last written nonzero, so
    /// `drain_releases` knows to zero them.
    pad_analog_active: bool,
}

/// Events that bring all held inputs to released (zero). Returned by
/// [`HeldInputs::drain_releases`].
pub struct ReleaseEvents {
    pub keyboard: Vec<isys::input_event>,
    pub mouse: Vec<isys::input_event>,
    pub gamepad: Vec<isys::input_event>,
}

impl HeldInputs {
    fn note_key(&mut self, code: u16, pressed: bool) {
        if pressed {
            self.keys.insert(code);
        } else {
            self.keys.remove(&code);
        }
    }

    fn note_mouse_button(&mut self, code: u16, pressed: bool) {
        if pressed {
            self.mouse_buttons.insert(code);
        } else {
            self.mouse_buttons.remove(&code);
        }
    }

    fn note_pad_button(&mut self, code: u16, pressed: bool) {
        if pressed {
            self.pad_buttons.insert(code);
        } else {
            self.pad_buttons.remove(&code);
        }
    }

    fn note_pad_analog(&mut self, active: bool) {
        self.pad_analog_active = active;
    }

    /// Events needed to release all currently-held inputs; clears state.
    fn drain_releases(&mut self) -> ReleaseEvents {
        let keyboard: Vec<_> = self
            .keys
            .iter()
            .map(|&code| ev(isys::EV_KEY as u16, code, 0))
            .collect();
        self.keys.clear();

        let mouse: Vec<_> = self
            .mouse_buttons
            .iter()
            .map(|&code| ev(isys::EV_KEY as u16, code, 0))
            .collect();
        self.mouse_buttons.clear();

        let mut gamepad: Vec<_> = self
            .pad_buttons
            .iter()
            .map(|&code| ev(isys::EV_KEY as u16, code, 0))
            .collect();
        self.pad_buttons.clear();

        if self.pad_analog_active {
            // Zero all sticks and triggers.
            for &(_, abs_code) in PAD_AXES {
                gamepad.push(ev(isys::EV_ABS as u16, abs_code, 0));
            }
            for &(_, abs_code, _) in PAD_TRIGGERS {
                gamepad.push(ev(isys::EV_ABS as u16, abs_code, 0));
            }
            self.pad_analog_active = false;
        }

        ReleaseEvents {
            keyboard,
            mouse,
            gamepad,
        }
    }
}

/// The three virtual input devices backing one session. `Send + Sync` (only
/// `File` handles + `Mutex` state) so it can live in an `Arc` captured by the
/// DataChannel signal closure.
pub struct VirtualDevices {
    keyboard: UInputHandle<File>,
    /// Shared with the relative-mouse flush thread (writes are `&self`).
    mouse: Arc<UInputHandle<File>>,
    gamepad: UInputHandle<File>,
    /// evdev node paths, for the compositor properties + container `--device`.
    pub keyboard_path: PathBuf,
    pub mouse_path: PathBuf,
    pub gamepad_path: PathBuf,
    /// The devices' fake-udev records (`(major, minor)` + serialized body),
    /// exported for the app container via `export_udev_data`.
    udev_records: Vec<((u32, u32), String)>,
    /// Where `export_udev_data` published the records (removed on Drop).
    udev_export_dir: Mutex<Option<PathBuf>>,
    /// Last gamepad snapshot, for state-on-change diffing (`gp` arrives at frame
    /// rate; only transitions are emitted).
    last_pad: Mutex<PadSnapshot>,
    /// Last absolute pointer position (output pixels) so `ma` can emit a delta on
    /// the relative virtual mouse. `None` until the first `ma` seeds it.
    last_abs: Mutex<Option<(f64, f64)>>,
    /// Fractional remainder of relative mouse motion (device pixels): uinput
    /// `REL_X`/`REL_Y` are integers, so per-message rounding quantizes sub-pixel
    /// motion inconsistently (0.4+0.4+0.4 -> 0+0+0). Accumulated across `mm`
    /// messages; only the integer part is emitted. Lock covers only the
    /// accumulate+split arithmetic, not the uinput write.
    rel_accum: Mutex<(f64, f64)>,
    /// Moonlight-style relative-mouse batching state. `None` when
    /// `QUASAR_INPUT_BATCH_MS=0` (per-arrival writes, no flush thread).
    rel_flush: Option<Arc<RelFlushState>>,
    /// The dedicated relative-mouse flush thread handle, joined on drop.
    flush_thread: Option<std::thread::JoinHandle<()>>,
    /// Held-key / held-button tracking for Wolf #302: release all on disconnect
    /// or launcher↔game swap so stuck keys don't bleed across sessions or apps.
    held: Mutex<HeldInputs>,
}

#[derive(Default)]
struct PadSnapshot {
    buttons: Vec<bool>,
}

impl VirtualDevices {
    /// Create the keyboard, mouse, and gamepad uinput devices and resolve their
    /// `/dev/input/eventN` nodes. `tag` disambiguates devices across sessions.
    pub fn create(tag: &str) -> Result<Self> {
        let keyboard = Self::create_keyboard(tag).context("create virtual keyboard")?;
        let mouse = Self::create_mouse(tag).context("create virtual mouse")?;
        let gamepad = Self::create_gamepad(tag).context("create virtual gamepad")?;

        // Per-class udev properties so libinput classifies + binds each device.
        const KB_PROPS: &[&str] = &["ID_INPUT=1", "ID_INPUT_KEYBOARD=1"];
        const MOUSE_PROPS: &[&str] = &["ID_INPUT=1", "ID_INPUT_MOUSE=1"];
        const PAD_PROPS: &[&str] = &["ID_INPUT=1", "ID_INPUT_JOYSTICK=1"];
        let (keyboard_path, kb_dev) = resolve_and_ensure_node(&keyboard, KB_PROPS)?;
        let (mouse_path, mouse_dev) = resolve_and_ensure_node(&mouse, MOUSE_PROPS)?;
        // libinput ignores joysticks (gamepad reaches the app via the container's
        // device node, not the compositor seat), but SDL/Steam discover devices
        // via libudev, so `export_udev_data` must still publish this record.
        let (gamepad_path, pad_dev) = resolve_and_ensure_node(&gamepad, PAD_PROPS)?;
        let udev_records: Vec<((u32, u32), String)> = vec![
            (kb_dev, udev_record_body(KB_PROPS)),
            (mouse_dev, udev_record_body(MOUSE_PROPS)),
            (pad_dev, udev_record_body(PAD_PROPS)),
        ];

        tracing::info!(
            "virtual input devices ready: keyboard={}, mouse={}, gamepad={}",
            keyboard_path.display(),
            mouse_path.display(),
            gamepad_path.display()
        );

        // Arc so the dedicated flush thread can share the mouse handle
        // (UInputHandle::write is &self) without racing other writers on the fd.
        let mouse = Arc::new(mouse);
        let batch_ms = input_batch_ms();
        let (rel_flush, flush_thread) = if batch_ms > 0 {
            let state = Arc::new(RelFlushState::new(batch_ms));
            let thread = spawn_rel_flush_thread(state.clone(), mouse.clone(), batch_ms);
            (Some(state), Some(thread))
        } else {
            (None, None)
        };
        if let Some(s) = rel_flush.as_ref() {
            tracing::info!(
                "relative-mouse batching enabled ({} ms window, dedicated flush thread)",
                s.batch_ms
            );
        } else {
            tracing::info!("relative-mouse batching disabled (QUASAR_INPUT_BATCH_MS=0)");
        }

        Ok(VirtualDevices {
            keyboard,
            mouse,
            gamepad,
            keyboard_path,
            mouse_path,
            gamepad_path,
            udev_records,
            udev_export_dir: Mutex::new(None),
            last_pad: Mutex::new(PadSnapshot::default()),
            last_abs: Mutex::new(None),
            rel_accum: Mutex::new((0.0, 0.0)),
            rel_flush,
            flush_thread,
            held: Mutex::new(HeldInputs::default()),
        })
    }

    /// Publish this session's fake-udev records into `dir` (see `udev_export_dir`)
    /// so the app container can bind-mount them at `/run/udev/data`: SDL/Steam
    /// enumerate devices via libudev, not by scanning `/dev/input`, so without
    /// this the `--device`-passed gamepad is silently absent in games.
    /// World-readable (0755/0644): app containers run as arbitrary non-root UIDs.
    pub fn export_udev_data(&self, dir: &Path) -> Result<()> {
        std::fs::create_dir_all(dir).with_context(|| format!("create {}", dir.display()))?;
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755))
            .with_context(|| format!("open perms on {}", dir.display()))?;
        for ((maj, min), body) in &self.udev_records {
            let path = dir.join(format!("c{maj}:{min}"));
            std::fs::write(&path, body).with_context(|| format!("write {}", path.display()))?;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644))
                .with_context(|| format!("open perms on {}", path.display()))?;
        }
        *self.udev_export_dir.lock().unwrap() = Some(dir.to_path_buf());
        Ok(())
    }

    fn create_keyboard(tag: &str) -> Result<UInputHandle<File>> {
        let h = open_uinput()?;
        h.set_evbit(EventKind::Key)?;
        h.set_evbit(EventKind::Synchronize)?;
        // Every key, but NOT buttons: exposing BTN_GAMEPAD/BTN_MOUSE would
        // misclassify this device in libinput.
        for key in <Key as input_linux::enum_iterator::IterableEnum>::iter() {
            if key.is_key() {
                let _ = h.set_keybit(key);
            }
        }
        let name = device_name("Keyboard", tag);
        h.create(&input_id(0x0001), name.as_bytes(), 0, &[])?;
        Ok(h)
    }

    fn create_mouse(tag: &str) -> Result<UInputHandle<File>> {
        let h = open_uinput()?;
        h.set_evbit(EventKind::Key)?;
        h.set_evbit(EventKind::Relative)?;
        h.set_evbit(EventKind::Synchronize)?;
        for b in [
            Key::ButtonLeft,
            Key::ButtonRight,
            Key::ButtonMiddle,
            Key::ButtonSide,
            Key::ButtonExtra,
        ] {
            h.set_keybit(b)?;
        }
        // REL-only (no ABS) so libinput reliably classifies this as a mouse;
        // absolute motion is tracked as deltas (see `mouse_move_abs`).
        for a in [
            RelativeAxis::X,
            RelativeAxis::Y,
            RelativeAxis::Wheel,
            RelativeAxis::HorizontalWheel,
            RelativeAxis::WheelHiRes,
            RelativeAxis::HorizontalWheelHiRes,
        ] {
            h.set_relbit(a)?;
        }
        let name = device_name("Mouse", tag);
        h.create(&input_id(0x0002), name.as_bytes(), 0, &[])?;
        Ok(h)
    }

    fn create_gamepad(tag: &str) -> Result<UInputHandle<File>> {
        let h = open_uinput()?;
        h.set_evbit(EventKind::Key)?;
        h.set_evbit(EventKind::Absolute)?;
        h.set_evbit(EventKind::Synchronize)?;
        for &(_, code) in PAD_BUTTONS.iter() {
            // set_keybit wants a typed Key; the codes are all valid BTN_* values.
            if let Some(key) = key_from_code(code) {
                h.set_keybit(key)?;
            }
        }
        // Trigger digital fallbacks (W3C exposes triggers as buttons 6/7).
        h.set_keybit(Key::ButtonTL2)?;
        h.set_keybit(Key::ButtonTR2)?;

        let stick = AbsoluteInfo {
            value: 0,
            minimum: STICK_MIN,
            maximum: STICK_MAX,
            fuzz: 16,
            flat: 128,
            resolution: 0,
        };
        let trigger = AbsoluteInfo {
            value: 0,
            minimum: TRIGGER_MIN,
            maximum: TRIGGER_MAX,
            fuzz: 0,
            flat: 0,
            resolution: 0,
        };
        let abs = [
            AbsoluteInfoSetup {
                axis: AbsoluteAxis::X,
                info: stick,
            },
            AbsoluteInfoSetup {
                axis: AbsoluteAxis::Y,
                info: stick,
            },
            AbsoluteInfoSetup {
                axis: AbsoluteAxis::RX,
                info: stick,
            },
            AbsoluteInfoSetup {
                axis: AbsoluteAxis::RY,
                info: stick,
            },
            AbsoluteInfoSetup {
                axis: AbsoluteAxis::Z,
                info: trigger,
            },
            AbsoluteInfoSetup {
                axis: AbsoluteAxis::RZ,
                info: trigger,
            },
        ];
        for s in &abs {
            h.set_absbit(s.axis)?;
        }
        let name = device_name("Gamepad", tag);
        h.create(&input_id(0x0003), name.as_bytes(), 0, &abs)?;
        Ok(h)
    }

    // ── injection ─────────────────────────────────────────────────────────────

    /// Keyboard key by raw evdev code (the browser already maps `KeyboardEvent`
    /// → evdev, see `protocol/input.md`).
    pub fn key(&self, code: u32, pressed: bool) -> Result<()> {
        let evs = [ev(isys::EV_KEY as u16, code as u16, pressed as i32), syn()];
        self.keyboard.write(&evs).context("write keyboard event")?;
        self.held.lock().unwrap().note_key(code as u16, pressed);
        Ok(())
    }

    /// Relative mouse motion (device pixels at the streamed resolution).
    /// Accumulates the fractional remainder under `rel_accum` and emits only the
    /// integer part (see `split_integer`), so long-run motion converges to the
    /// true sum instead of rounding to zero.
    pub fn mouse_move_rel(&self, dx: f64, dy: f64) -> Result<()> {
        let (ix, iy) = {
            let mut acc = self.rel_accum.lock().unwrap();
            split_integer(&mut acc, dx, dy)
        };
        if ix == 0 && iy == 0 {
            return Ok(());
        }
        match &self.rel_flush {
            // Notify the flush thread only on the first sample of a burst — a
            // non-zero pending means the thread is already sleeping and will pick
            // this up.
            Some(state) => {
                let mut pending = state.pending.lock().unwrap();
                let was_empty = pending.0 == 0 && pending.1 == 0;
                pending.0 += ix;
                pending.1 += iy;
                if was_empty {
                    state.cv.notify_one();
                }
                Ok(())
            }
            None => write_rel_frame(&self.mouse, ix, iy),
        }
    }

    /// Drain any pending relative-mouse motion immediately and write it. Called
    /// before button/key/scroll/abs events so a click lands at the cursor
    /// position the user saw. No-op with batching disabled.
    ///
    /// Writes synchronously from the caller's thread so the following non-`mm`
    /// event is guaranteed to land after the motion; the mutex ensures the delta
    /// is claimed by whichever path drains first (no double-write).
    pub fn flush_pending_rel(&self) {
        if let Some(state) = &self.rel_flush {
            let (dx, dy) = {
                let mut pending = state.pending.lock().unwrap();
                let d = *pending;
                *pending = (0, 0);
                d
            };
            if dx == 0 && dy == 0 {
                return;
            }
            // Wake the thread so it re-waits instead of staying in a stale sleep.
            state.flush_now.store(true, Ordering::Release);
            state.cv.notify_one();
            if let Err(e) = write_rel_frame(&self.mouse, dx, dy) {
                tracing::warn!(
                    token = "vinput-rel-flush-failed",
                    "flush_pending_rel write failed: {e:#}"
                );
            }
        }
    }

    /// One-shot net-zero pointer jiggle (`REL_X +1, SYN` then `REL_X -1, SYN`) as
    /// two SEPARATE uinput frames, to heal Steam Big Picture controller-focus on
    /// the first gamepad event of a controller-only session (see
    /// `docs/design/plans/2026-07-20-bpm-controller-focus-spec.md` and
    /// `super::input::InputState`). Two frames guarantee two real `REL_X` events
    /// reach the compositor, where a single coalesced `+1-1=0` frame would emit
    /// nothing; net displacement is zero and imperceptible.
    ///
    /// Bypasses `mouse_move_rel` (writes straight to the mouse handle) so it never
    /// touches `rel_accum` or the pending-batch buffer.
    pub fn pointer_nudge(&self) -> Result<()> {
        write_rel_frame(&self.mouse, 1, 0)?;
        write_rel_frame(&self.mouse, -1, 0)?;
        Ok(())
    }

    /// Absolute mouse motion in output pixels (already denormalized). The
    /// relative virtual mouse can't teleport, so this emits the delta from the
    /// last absolute position; the first sample only seeds the anchor.
    pub fn mouse_move_abs(&self, x_px: f64, y_px: f64) -> Result<()> {
        let (dx, dy) = {
            let mut last = self.last_abs.lock().unwrap();
            let delta = match *last {
                Some((lx, ly)) => (x_px - lx, y_px - ly),
                None => (0.0, 0.0),
            };
            *last = Some((x_px, y_px));
            delta
        };
        if dx != 0.0 || dy != 0.0 {
            self.mouse_move_rel(dx, dy)?;
        }
        Ok(())
    }

    /// Mouse button (Linux button code, e.g. 0x110 BTN_LEFT).
    pub fn mouse_button(&self, button: u32, pressed: bool) -> Result<()> {
        let evs = [
            ev(isys::EV_KEY as u16, button as u16, pressed as i32),
            syn(),
        ];
        self.mouse.write(&evs).context("write mouse button")?;
        self.held
            .lock()
            .unwrap()
            .note_mouse_button(button as u16, pressed);
        Ok(())
    }

    /// Scroll (hi-res wheel units). `dy` is vertical, `dx` horizontal.
    pub fn scroll(&self, dx: f64, dy: f64) -> Result<()> {
        let mut evs = Vec::with_capacity(5);
        if dy != 0.0 {
            // Hi-res wheel is in 1/120 of a detent; also emit a coarse notch so
            // clients that only read REL_WHEEL still scroll.
            evs.push(ev(
                isys::EV_REL as u16,
                isys::REL_WHEEL_HI_RES as u16,
                dy.round() as i32,
            ));
            evs.push(ev(
                isys::EV_REL as u16,
                isys::REL_WHEEL as u16,
                (dy / 120.0).round() as i32,
            ));
        }
        if dx != 0.0 {
            evs.push(ev(
                isys::EV_REL as u16,
                isys::REL_HWHEEL_HI_RES as u16,
                dx.round() as i32,
            ));
            evs.push(ev(
                isys::EV_REL as u16,
                isys::REL_HWHEEL as u16,
                (dx / 120.0).round() as i32,
            ));
        }
        if evs.is_empty() {
            return Ok(());
        }
        evs.push(syn());
        self.mouse.write(&evs).context("write scroll")?;
        Ok(())
    }

    /// Gamepad state-on-change. `buttons`/`axes` follow the W3C Standard Gamepad
    /// layout (browser Gamepad API, `protocol/input.md`); we diff against the
    /// last snapshot and emit only transitions, then one `SYN_REPORT`.
    pub fn gamepad(&self, buttons: &[f64], axes: &[f64]) -> Result<()> {
        let mut last = self.last_pad.lock().unwrap();
        let mut evs: Vec<isys::input_event> = Vec::new();

        for &(idx, code) in PAD_BUTTONS.iter() {
            let pressed = buttons.get(idx).map(|v| *v >= 0.5).unwrap_or(false);
            let was = last.buttons.get(idx).copied().unwrap_or(false);
            if pressed != was {
                evs.push(ev(isys::EV_KEY as u16, code, pressed as i32));
            }
        }
        // Triggers (W3C buttons 6/7) -> ABS_Z/ABS_RZ + digital fallback.
        for &(idx, abs_code, btn_code) in PAD_TRIGGERS.iter() {
            let v = buttons.get(idx).copied().unwrap_or(0.0).clamp(0.0, 1.0);
            let was = last.buttons.get(idx).copied().unwrap_or(false);
            let pressed = v >= 0.5;
            evs.push(ev(
                isys::EV_ABS as u16,
                abs_code,
                (v * TRIGGER_MAX as f64).round() as i32,
            ));
            if pressed != was {
                evs.push(ev(isys::EV_KEY as u16, btn_code, pressed as i32));
            }
        }
        // Sticks (W3C axes 0..=3) -> ABS_X/Y/RX/RY, scaled to signed 16-bit.
        for &(idx, abs_code) in PAD_AXES.iter() {
            let v = axes.get(idx).copied().unwrap_or(0.0).clamp(-1.0, 1.0);
            evs.push(ev(isys::EV_ABS as u16, abs_code, scale_stick(v)));
        }

        if evs.is_empty() {
            return Ok(());
        }
        evs.push(syn());
        self.gamepad.write(&evs).context("write gamepad state")?;

        {
            let mut held = self.held.lock().unwrap();
            for &(idx, code) in PAD_BUTTONS.iter() {
                let pressed = buttons.get(idx).map(|v| *v >= 0.5).unwrap_or(false);
                let was = last.buttons.get(idx).copied().unwrap_or(false);
                if pressed != was {
                    held.note_pad_button(code, pressed);
                }
            }
            for &(idx, _, btn_code) in PAD_TRIGGERS.iter() {
                let pressed = buttons.get(idx).copied().unwrap_or(0.0) >= 0.5;
                let was = last.buttons.get(idx).copied().unwrap_or(false);
                if pressed != was {
                    held.note_pad_button(btn_code, pressed);
                }
            }
            let any_trigger = PAD_TRIGGERS
                .iter()
                .any(|&(idx, _, _)| buttons.get(idx).copied().unwrap_or(0.0) != 0.0);
            let any_stick = PAD_AXES
                .iter()
                .any(|&(idx, _)| axes.get(idx).copied().unwrap_or(0.0) != 0.0);
            held.note_pad_analog(any_trigger || any_stick);
        }

        last.buttons = (0..buttons.len()).map(|i| buttons[i] >= 0.5).collect();
        Ok(())
    }

    /// Forward one physical-keyboard evdev frame (already kernel-framed with its
    /// own `SYN_REPORT`) verbatim into the virtual keyboard — no re-batching.
    /// Held-key tracking updates from `EV_KEY` events so `release_all` (Wolf
    /// #302) still zeroes a physically-forwarded key still down on grab release.
    pub fn forward_keyboard_frame(&self, events: &[isys::input_event]) -> Result<()> {
        self.keyboard
            .write(events)
            .context("forward physical keyboard frame")?;
        let mut held = self.held.lock().unwrap();
        for e in events {
            if e.type_ == isys::EV_KEY as u16 {
                held.note_key(e.code, e.value != 0);
            }
        }
        Ok(())
    }

    /// Forward one physical-mouse evdev frame verbatim into the virtual mouse.
    /// Bypasses `mouse_move_rel`'s batching — the device's own cadence is already
    /// well-formed. Held-button tracking updates as in `forward_keyboard_frame`.
    pub fn forward_mouse_frame(&self, events: &[isys::input_event]) -> Result<()> {
        self.mouse
            .write(events)
            .context("forward physical mouse frame")?;
        let mut held = self.held.lock().unwrap();
        for e in events {
            if e.type_ == isys::EV_KEY as u16 {
                held.note_mouse_button(e.code, e.value != 0);
            }
        }
        Ok(())
    }

    /// Forward a physical controller's already-framed evdev events into the
    /// session gamepad. Linux gamepads use the same BTN_*/ABS_* event ABI as the
    /// virtual Xbox-style device, so no Wayland/compositor path is involved.
    pub fn forward_gamepad_frame(&self, events: &[isys::input_event]) -> Result<()> {
        self.gamepad
            .write(events)
            .map(|_| ())
            .context("forward physical gamepad frame")
    }

    /// Release all currently-held keys, mouse buttons, and gamepad buttons/analogs.
    /// Called on client disconnect (Wolf #302) and before a launcher<->game swap
    /// so a key held in one app doesn't bleed into the next.
    pub fn release_all(&self) -> Result<()> {
        let releases = self.held.lock().unwrap().drain_releases();
        if !releases.keyboard.is_empty() {
            let mut evs = releases.keyboard;
            evs.push(syn());
            self.keyboard
                .write(&evs)
                .context("release_all: write keyboard releases")?;
        }
        if !releases.mouse.is_empty() {
            let mut evs = releases.mouse;
            evs.push(syn());
            self.mouse
                .write(&evs)
                .context("release_all: write mouse releases")?;
        }
        if !releases.gamepad.is_empty() {
            let mut evs = releases.gamepad;
            evs.push(syn());
            self.gamepad
                .write(&evs)
                .context("release_all: write gamepad releases")?;
        }
        tracing::debug!("release_all: all held inputs released");
        // Drop the fractional remainder so it doesn't bias motion in the next
        // app/session, then flush any pending batched motion so it isn't lost.
        *self.rel_accum.lock().unwrap() = (0.0, 0.0);
        self.flush_pending_rel();
        Ok(())
    }
}

impl Drop for VirtualDevices {
    fn drop(&mut self) {
        // Stop the flush thread so it doesn't outlive the devices; it drains any
        // remaining pending motion on shutdown (the `stop` branch).
        if let Some(state) = &self.rel_flush {
            state.stop.store(true, Ordering::Release);
            state.cv.notify_all();
        }
        if let Some(thread) = self.flush_thread.take() {
            let _ = thread.join();
        }
        if let Some(dir) = self.udev_export_dir.lock().unwrap().take() {
            let _ = std::fs::remove_dir_all(dir);
        }
    }
}

/// Scale a W3C axis value (-1.0..=1.0) onto signed 16-bit stick range.
fn scale_stick(v: f64) -> i32 {
    if v >= 0.0 {
        (v * STICK_MAX as f64).round() as i32
    } else {
        (-v * STICK_MIN as f64).round() as i32
    }
}

/// W3C Standard Gamepad button index → evdev BTN_* code (digital buttons).
const PAD_BUTTONS: &[(usize, u16)] = &[
    (0, isys::BTN_SOUTH as u16),       // A
    (1, isys::BTN_EAST as u16),        // B
    (2, isys::BTN_WEST as u16),        // X
    (3, isys::BTN_NORTH as u16),       // Y
    (4, isys::BTN_TL as u16),          // LB
    (5, isys::BTN_TR as u16),          // RB
    (8, isys::BTN_SELECT as u16),      // Back/View
    (9, isys::BTN_START as u16),       // Start/Menu
    (10, isys::BTN_THUMBL as u16),     // L3
    (11, isys::BTN_THUMBR as u16),     // R3
    (12, isys::BTN_DPAD_UP as u16),    // Dpad up
    (13, isys::BTN_DPAD_DOWN as u16),  // Dpad down
    (14, isys::BTN_DPAD_LEFT as u16),  // Dpad left
    (15, isys::BTN_DPAD_RIGHT as u16), // Dpad right
    (16, isys::BTN_MODE as u16),       // Guide
];

/// W3C trigger button index → (ABS axis code, digital BTN_* fallback).
const PAD_TRIGGERS: &[(usize, u16, u16)] = &[
    (6, isys::ABS_Z as u16, isys::BTN_TL2 as u16),  // LT
    (7, isys::ABS_RZ as u16, isys::BTN_TR2 as u16), // RT
];

/// W3C axis index → evdev ABS axis code (sticks).
const PAD_AXES: &[(usize, u16)] = &[
    (0, isys::ABS_X as u16),  // left stick X
    (1, isys::ABS_Y as u16),  // left stick Y
    (2, isys::ABS_RX as u16), // right stick X
    (3, isys::ABS_RY as u16), // right stick Y
];

/// Map a raw BTN_* code to the typed `Key` needed for `set_keybit`.
fn key_from_code(code: u16) -> Option<Key> {
    <Key as input_linux::enum_iterator::IterableEnum>::iter().find(|k| *k as u16 == code)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn device_names_are_session_tagged_and_unique() {
        let a = "sess-a";
        let b = "sess-b";
        for kind in ["Keyboard", "Mouse", "Gamepad"] {
            assert!(device_name(kind, a).contains(a));
            assert!(device_name(kind, a).contains(kind));
            assert_ne!(device_name(kind, a), device_name(kind, b));
        }
    }

    /// Press-then-release leaves nothing held; a second drain is empty.
    #[test]
    fn press_then_release_leaves_nothing_held() {
        let mut h = HeldInputs::default();
        h.note_key(30, true);
        h.note_key(30, false);
        let r = h.drain_releases();
        assert!(
            r.keyboard.is_empty(),
            "key released before drain → nothing to emit"
        );
        assert!(r.mouse.is_empty());
        assert!(r.gamepad.is_empty());
    }

    /// Keys held at drain time appear in the keyboard release list; a second
    /// drain returns empty (state cleared).
    #[test]
    fn held_keys_appear_in_drain_and_state_is_cleared() {
        let mut h = HeldInputs::default();
        h.note_key(30, true);
        h.note_key(42, true);
        h.note_key(42, false);
        let r = h.drain_releases();
        assert_eq!(r.keyboard.len(), 1);
        assert_eq!(r.keyboard[0].code, 30);
        assert_eq!(r.keyboard[0].value, 0, "release event has value=0");

        let r2 = h.drain_releases();
        assert!(r2.keyboard.is_empty());
    }

    /// Mouse buttons follow the same rule as keyboard keys.
    #[test]
    fn held_mouse_buttons_appear_in_drain() {
        let mut h = HeldInputs::default();
        h.note_mouse_button(0x110, true);
        h.note_mouse_button(0x111, true);
        h.note_mouse_button(0x111, false);
        let r = h.drain_releases();
        assert_eq!(r.mouse.len(), 1);
        assert_eq!(r.mouse[0].code, 0x110);
        assert_eq!(r.mouse[0].value, 0);
        assert!(r.keyboard.is_empty());
    }

    /// Held pad buttons and active analog both appear; analog zeros come after
    /// the button releases in the gamepad list.
    #[test]
    fn pad_buttons_and_analog_appear_in_drain() {
        let mut h = HeldInputs::default();
        h.note_pad_button(isys::BTN_SOUTH as u16, true);
        h.note_pad_analog(true);
        let r = h.drain_releases();

        let btn_releases: Vec<_> = r
            .gamepad
            .iter()
            .filter(|e| e.type_ == isys::EV_KEY as u16)
            .collect();
        assert_eq!(btn_releases.len(), 1);
        assert_eq!(btn_releases[0].code, isys::BTN_SOUTH as u16);
        assert_eq!(btn_releases[0].value, 0);

        let abs_zeros: Vec<_> = r
            .gamepad
            .iter()
            .filter(|e| e.type_ == isys::EV_ABS as u16)
            .collect();
        assert_eq!(abs_zeros.len(), PAD_AXES.len() + PAD_TRIGGERS.len());
        assert!(abs_zeros.iter().all(|e| e.value == 0));

        let r2 = h.drain_releases();
        assert!(r2.gamepad.is_empty());
    }

    /// When pad_analog_active is false, no ABS zeroes are emitted.
    #[test]
    fn no_analog_events_when_analog_was_not_active() {
        let mut h = HeldInputs::default();
        h.note_pad_button(isys::BTN_EAST as u16, true);
        h.note_pad_analog(false);
        let r = h.drain_releases();
        let abs_events: Vec<_> = r
            .gamepad
            .iter()
            .filter(|e| e.type_ == isys::EV_ABS as u16)
            .collect();
        assert!(
            abs_events.is_empty(),
            "no ABS zeroes when analog was never active"
        );
    }

    /// Sub-pixel samples sum across messages instead of being rounded to zero.
    /// 0.4+0.4+0.4 must reach 1, not 0 (the per-message `round()` failure mode).
    #[test]
    fn split_integer_accumulates_subpixel_motion() {
        let mut acc = (0.0_f64, 0.0_f64);
        let mut total = (0, 0);
        for _ in 0..3 {
            let (ix, iy) = split_integer(&mut acc, 0.4, 0.4);
            total.0 += ix;
            total.1 += iy;
        }
        assert_eq!(total, (1, 1), "three 0.4 samples accumulate to 1, not 0");
        assert!(
            (acc.0 - 0.2).abs() < 1e-9,
            "remainder carried: ax={}",
            acc.0
        );
        assert!(
            (acc.1 - 0.2).abs() < 1e-9,
            "remainder carried: ay={}",
            acc.1
        );
    }

    /// Negative sub-pixel motion accumulates the same way; `trunc()` toward zero
    /// keeps the remainder negative so a follow-up same-direction sample reaches
    /// the next integer (not zero-biased).
    #[test]
    fn split_integer_handles_negative_motion() {
        let mut acc = (0.0_f64, 0.0_f64);
        let mut total = (0, 0);
        for _ in 0..3 {
            let (ix, iy) = split_integer(&mut acc, -0.4, -0.4);
            total.0 += ix;
            total.1 += iy;
        }
        assert_eq!(total, (-1, -1), "three -0.4 samples reach -1");
        assert!(
            (acc.0 + 0.2).abs() < 1e-9,
            "negative remainder carried: ax={}",
            acc.0
        );
    }

    /// Integer motion passes through unchanged and leaves no remainder.
    #[test]
    fn split_integer_passes_integer_motion_through() {
        let mut acc = (0.0_f64, 0.0_f64);
        let (ix, iy) = split_integer(&mut acc, 5.0, -3.0);
        assert_eq!((ix, iy), (5, -3));
        assert!(
            acc.0.abs() < 1e-9 && acc.1.abs() < 1e-9,
            "no remainder for integers"
        );
    }

    /// A long run of small same-direction samples converges to the true sum with
    /// at most ~1 unit of error (the carried remainder), never drifting.
    #[test]
    fn split_integer_converges_over_many_samples() {
        let mut acc = (0.0_f64, 0.0_f64);
        let mut total = (0_i32, 0_i32);
        for _ in 0..1000 {
            let (ix, iy) = split_integer(&mut acc, 0.1, 0.1);
            total.0 += ix;
            total.1 += iy;
        }
        assert!(
            (total.0 - 100).abs() <= 1 && (total.1 - 100).abs() <= 1,
            "converges to ~100, got {total:?}"
        );
    }
}
