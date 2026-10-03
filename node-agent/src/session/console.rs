//! Headless `weston` process manager for the local-display path on nvidia-drm KMS.
//!
//! kmssink can't drive `nvidia-drm` (atomic-KMS only, kmssink is legacy modesetting).
//! weston's `drm-backend.so` speaks atomic KMS, so: spawn headless weston (takes DRM
//! master, enables the connected output), then `waylandsink` renders into its socket.
//!
//! On a rootful engine the compose/deploy layer grants `CAP_SYS_ADMIN`. On a rootless
//! engine (#407) none is granted, and none is needed for the common case: the kernel
//! makes the first opener of a free primary node master with no capability check
//! (`drm_auth.c`, proven live on nvidia-test 2026-09-29) — only re-asserting master over
//! an *already-held* display needs `CAP_SYS_ADMIN`, and a rootless container's capability
//! can never satisfy that check (it is checked against the host's initial user namespace).
//! So this module's own logic is unchanged either way; what differs is that a held
//! display now fails loud instead of being masked by the capability: seatd logs
//! `Could not make device fd drm master: Permission denied` and weston's atomic commits
//! fail silently from this process's point of view, which is exactly what
//! `session::console_preflight` checks for before this agent ever reports healthy.

use std::os::unix::process::CommandExt;
use std::path::Path;
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::{Duration, Instant};

use anyhow::{bail, Context, Result};

/// Fixed Wayland socket name for the console weston (never auto `wayland-N`): only
/// one console weston runs at a time, must not clash with the game compositor's
/// `wayland-N`, and avoids a stale-socket race against a before/after set-diff
/// detector.
const CONSOLE_SOCKET: &str = "wayland-console";

/// Process-wide mutex serializing console weston lifetimes: only one physical
/// console display exists, so a new launch must block until the previous
/// [`WestonConsole`] has fully torn down (group killed + drained) rather than
/// racing the shared DRM node for master. `OnceLock` gives the lock a `'static`
/// lifetime so the guard can live inside the returned `WestonConsole`.
static CONSOLE_WESTON_LOCK: OnceLock<Mutex<()>> = OnceLock::new();

/// Process-wide lock around the *moment* of opening a DRM primary node, shared with
/// `capacity::detect_drm_outputs_at` (#407). Opening a card node read-write can make the
/// opener DRM master automatically when the display is currently free (`drm_auth.c`), so
/// a capacity probe's brief open racing `spawn_weston_console`'s can make weston lose
/// master to the probe instead. Distinct from [`CONSOLE_WESTON_LOCK`], which serialises
/// whole weston launches against each other rather than guarding a single open.
static DRM_OPEN_LOCK: OnceLock<Mutex<()>> = OnceLock::new();

/// Take the DRM-open lock. Callers hold it only around the open (or, in
/// `spawn_weston_console`'s case, until the socket confirms weston/seatd already hold
/// master) — never across a whole session.
pub(crate) fn drm_open_lock() -> &'static Mutex<()> {
    DRM_OPEN_LOCK.get_or_init(|| Mutex::new(()))
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LocalBackend {
    Weston,
    DirectKms,
}

impl LocalBackend {
    pub fn name(self) -> &'static str {
        match self {
            Self::Weston => "weston",
            Self::DirectKms => "direct-kms",
        }
    }
}

/// NVIDIA's atomic-only KMS path requires Weston; amdgpu is handled directly by
/// kmssink. Unknown/missing driver identity fails safe to Weston.
pub fn local_backend(config: Option<&crate::messages::ConsoleConfig>) -> LocalBackend {
    local_backend_at(config, Path::new("/sys/class/drm"))
}

fn local_backend_at(
    config: Option<&crate::messages::ConsoleConfig>,
    drm_root: &Path,
) -> LocalBackend {
    let Some(card) = config
        .and_then(|c| c.output_id.as_deref())
        .and_then(|id| id.split_once(':').map(|(card, _)| card))
    else {
        return LocalBackend::Weston;
    };
    let driver = std::fs::read_link(drm_root.join(card).join("device/driver"))
        .ok()
        .and_then(|p| p.file_name().map(|n| n.to_string_lossy().into_owned()));
    if driver.as_deref() == Some("amdgpu") {
        LocalBackend::DirectKms
    } else {
        LocalBackend::Weston
    }
}

/// weston does not restrict scanout to a `[output] name=<connector>` stanza on its
/// own: naming a disconnected/absent connector is silently ignored and weston
/// auto-enables whatever other connector it finds, with no warning (verified
/// against weston 14.0.2). Pinning a connector on a multi-head host therefore
/// needs an explicit `[output] name=<other> mode=off` stanza per other connected
/// connector (`other_connected`, from `capacity::detect_drm_outputs`);
/// disconnected connectors need no stanza.
fn weston_output_config(
    output_id: &str,
    mode: &crate::messages::ConsoleModeSelection,
    other_connected: &[String],
) -> Result<String> {
    let connector = output_id
        .split_once(':')
        .map(|(_, connector)| connector)
        .context("console output_id must be card-scoped (cardN:CONNECTOR)")?;
    // weston.ini matches modes by rounded Hz (e.g. 119.997 mHz -> 120.0); Quasar
    // keeps the exact millihertz elsewhere and rounds only at this boundary.
    let refresh_hz = (mode.refresh_millihz + 500) / 1000;
    let mut cfg = format!(
        "[output]\nname={connector}\nmode={}x{}@{refresh_hz}\n",
        mode.width, mode.height
    );
    for other in other_connected {
        if other != connector {
            cfg.push_str(&format!("\n[output]\nname={other}\nmode=off\n"));
        }
    }
    Ok(cfg)
}

/// The mode to write for a pinned `output_id` whose config carries no `mode` (#422):
/// the output's active mode (what its CRTC runs now), else its DRM-preferred mode,
/// else its first mode. `None` when the output is absent, disconnected or reports no
/// modes; the caller then writes no config and weston behaves as before. The control
/// plane sizes the session with the same rule (`console.ResolveSessionMode`).
fn pinned_output_mode(
    output_id: &str,
    outputs: &[crate::messages::DrmOutputCapability],
) -> Option<crate::messages::ConsoleModeSelection> {
    let output = outputs.iter().find(|o| o.id == output_id && o.connected)?;
    let mode = output
        .active_mode
        .as_ref()
        .or_else(|| output.modes.iter().find(|m| m.preferred))
        .or_else(|| output.modes.first())?;
    Some(crate::messages::ConsoleModeSelection {
        width: mode.width,
        height: mode.height,
        refresh_millihz: mode.refresh_millihz,
    })
}

/// The connector a console session's local display runs on: the pinned `output_id`'s, else
/// the first connected output's (the same rule `console.ResolveSessionMode` uses for an
/// Automatic console). `None` when nothing is connected.
pub fn console_output<'a>(
    config: Option<&crate::messages::ConsoleConfig>,
    outputs: &'a [crate::messages::DrmOutputCapability],
) -> Option<&'a crate::messages::DrmOutputCapability> {
    match config.and_then(|c| c.output_id.as_deref()) {
        Some(id) => outputs.iter().find(|o| o.id == id && o.connected),
        None => outputs.iter().find(|o| o.connected),
    }
}

/// The modes a console session's display genuinely supports, for the compositor to
/// advertise (#445): every non-interlaced DRM mode of [`console_output`], deduplicated on
/// `WxH@mHz`, in the order the kernel lists them (preferred first). Empty when no output is
/// connected, in which case the compositor advertises its one mode as before.
pub fn console_output_modes(
    config: Option<&crate::messages::ConsoleConfig>,
    outputs: &[crate::messages::DrmOutputCapability],
) -> Vec<crate::messages::ConsoleModeSelection> {
    let Some(output) = console_output(config, outputs) else {
        return Vec::new();
    };
    let mut modes: Vec<crate::messages::ConsoleModeSelection> = Vec::new();
    for mode in output.modes.iter().filter(|m| !m.interlaced) {
        let sel = crate::messages::ConsoleModeSelection {
            width: mode.width,
            height: mode.height,
            refresh_millihz: mode.refresh_millihz,
        };
        if !modes.contains(&sel) {
            modes.push(sel);
        }
    }
    modes
}

/// The console config to start weston with when the session moves to `mode` (#445): the
/// same config, pinned to the connector [`console_output`] resolves (an Automatic console
/// becomes pinned to the connector it is actually on, so weston lights that one at the
/// chosen mode and no other) with `mode` as its static mode.
pub fn console_config_at_mode(
    config: &crate::messages::ConsoleConfig,
    mode: crate::messages::ConsoleModeSelection,
    outputs: &[crate::messages::DrmOutputCapability],
) -> crate::messages::ConsoleConfig {
    let mut at = config.clone();
    if at.output_id.is_none() {
        at.output_id = console_output(Some(config), outputs).map(|o| o.id.clone());
    }
    at.mode = Some(mode);
    at
}

/// The refresh rate as the whole hertz the pipeline runs at (`143981` mHz -> `144`).
pub fn mode_fps(mode: &crate::messages::ConsoleModeSelection) -> i32 {
    ((mode.refresh_millihz + 500) / 1000) as i32
}

/// Why a client's mode request was not acted on. Logged, never fatal: the session keeps
/// running at its current mode.
#[derive(Debug, PartialEq, Eq)]
pub enum ModeSwitchRefusal {
    /// The requested mode is not one of the display's modes.
    NotAvailable,
    /// The session is already at that mode.
    AlreadyCurrent,
    /// A streamed console: a mode change is an encoder restart on the latency path, which
    /// the stream rule does not allow yet (#445 item 4).
    Streaming,
    /// The request is not a mode (non-positive dimension or refresh).
    Malformed,
}

/// Decide what a client's `(width, height, refresh_mHz)` mode request means for a console
/// session (#445). Pure, so the rule is unit-tested: the request must name one of
/// `available` (same size, nearest refresh within half a hertz -- the compositor already
/// snaps, this re-checks against the DRM list the agent trusts), must differ from
/// `current`, and the session must not be streamed.
pub fn plan_mode_switch(
    request: (i32, i32, i32),
    current: &crate::messages::ConsoleModeSelection,
    available: &[crate::messages::ConsoleModeSelection],
    streaming: bool,
) -> Result<crate::messages::ConsoleModeSelection, ModeSwitchRefusal> {
    let (w, h, refresh) = request;
    if w <= 0 || h <= 0 || refresh <= 0 || w > u16::MAX as i32 || h > u16::MAX as i32 {
        return Err(ModeSwitchRefusal::Malformed);
    }
    let target = available
        .iter()
        .filter(|m| m.width as i32 == w && m.height as i32 == h)
        .min_by_key(|m| (m.refresh_millihz as i64 - refresh as i64).abs())
        .filter(|m| (m.refresh_millihz as i64 - refresh as i64).abs() <= 500)
        .cloned()
        .ok_or(ModeSwitchRefusal::NotAvailable)?;
    if streaming {
        return Err(ModeSwitchRefusal::Streaming);
    }
    if target == *current {
        return Err(ModeSwitchRefusal::AlreadyCurrent);
    }
    Ok(target)
}

/// A running headless weston process + the Wayland socket name it created. Killed
/// on Drop (every session exit path drops the owning [`super::pipeline::LocalDisplay`]
/// first — see the runner's reverse-declaration-order teardown).
pub struct WestonConsole {
    child: std::process::Child,
    pub socket: String,
    config_path: Option<std::path::PathBuf>,
    // Held for this instance's whole lifetime: `Drop` runs the SIGKILL + bounded
    // group-drain to completion before returning, so this releases strictly after
    // the process group is confirmed gone (or the 5s bound is hit) — must stay the
    // LAST field so drop order matches, since the next spawn's `.lock()` must not
    // succeed until the DRM master fd is guaranteed free.
    _console_lock: MutexGuard<'static, ()>,
}

impl WestonConsole {
    /// Non-blocking liveness probe used by the session runner. A physical
    /// compositor exit is terminal for a local-only session even when the
    /// headless capture pipeline remains PLAYING.
    pub fn try_exit(&mut self) -> Result<Option<std::process::ExitStatus>> {
        self.child.try_wait().context("poll weston process")
    }
}

impl Drop for WestonConsole {
    fn drop(&mut self) {
        // weston is a process-group leader (`process_group(0)` in spawn); SIGKILL
        // the whole group, not just weston, since a surviving helper holding the
        // DRM-master fd makes the NEXT weston's drmSetMaster fail "Device or
        // resource busy" under rapid launch/stop churn.
        let pgid = self.child.id() as i32;
        unsafe {
            libc::kill(-pgid, libc::SIGKILL);
        }

        // weston's descendants reparent to tini and reap asynchronously, so reaping
        // weston itself proves nothing about a helper still holding the DRM-master
        // fd. Bound-drain both on one ~5s deadline via `drain_weston_group` (never a
        // blocking wait — see its doc) before `drop` returns and `_console_lock`
        // releases, else the next launch's drmSetMaster can race a lingering holder.
        let deadline = Instant::now() + Duration::from_secs(5);
        if drain_weston_group(&mut self.child, pgid, deadline, Duration::from_millis(50)) {
            tracing::debug!("console: weston group {pgid} fully drained");
        } else {
            // Fail-loud backstop: the next spawn's 15s socket wait is the outer guard.
            tracing::error!(
                token = "console-weston-exit-timeout",
                "console: weston group {pgid} did not fully exit within 5s; next launch may hit a DRM-busy race"
            );
        }

        if let Some(path) = &self.config_path {
            let _ = std::fs::remove_file(path);
        }
    }
}

/// Does any process in `/proc` still report process-group `pgid`? Used to
/// bound-drain a killed weston group before releasing the console launch lock —
/// group members reparent to tini so cannot be `waitpid`'d, hence polling
/// `getpgid` over `/proc` instead. Best-effort: an unreadable `/proc` reports
/// "not alive" rather than looping forever.
///
/// Deliberately conservative: counts an unreaped zombie as alive (mirrors
/// waiting for tini's async reap), and a pid-reuse race can only make the drain
/// wait longer, never shorter — the caller's bounded deadline still applies.
fn process_group_alive(pgid: i32) -> bool {
    let Ok(entries) = std::fs::read_dir("/proc") else {
        return false;
    };
    for entry in entries.flatten() {
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|s| s.parse::<i32>().ok())
        else {
            continue;
        };
        // SAFETY: getpgid(2) is a pure query; ESRCH (pid gone mid-listing) returns
        // -1, which never equals a real pgid, so the race just reads "not this group".
        if unsafe { libc::getpgid(pid) } == pgid {
            return true;
        }
    }
    false
}

/// Bounded reap-and-drain for a killed weston process group. Safe to release the
/// console launch lock only once both hold: our direct child (weston) is reaped,
/// AND no other group member remains ([`process_group_alive`]) — both polled
/// against the same `deadline`.
///
/// Must use `Child::try_wait` (non-blocking) for the reap, never a blocking
/// `wait()`: weston wedged in uninterruptible D-state on a DRM ioctl leaves the
/// pending SIGKILL undelivered, so a blocking wait here would hang forever,
/// holding `CONSOLE_WESTON_LOCK` and deadlocking every future console launch.
///
/// Returns `false` if `deadline` is hit first — the caller logs an ERROR and
/// releases the lock anyway (best-effort; the next spawn's 15s socket-wait is
/// the outer backstop).
fn drain_weston_group(
    child: &mut std::process::Child,
    pgid: i32,
    deadline: Instant,
    poll_interval: Duration,
) -> bool {
    loop {
        // Non-blocking: `Ok(None)` immediately if still running (or wedged).
        let child_reaped = matches!(child.try_wait(), Ok(Some(_)));
        if child_reaped && !process_group_alive(pgid) {
            return true;
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(poll_interval);
    }
}

/// Ensure a **VT-unbound** `seatd` is running so weston's DRM backend can acquire
/// the device without full `privileged` — only `CAP_SYS_ADMIN` on a rootful engine,
/// nothing extra on a rootless one (#407; see the module doc).
///
/// A container has no VT (and no logind), so seatd's default VT-bound seat never
/// goes "active" and every device open is refused (`seatd/seat.c: client is not
/// active` -> weston `fatal: failed to create compositor backend`).
/// `SEATD_VTBOUND=0` makes the seat always-active so seatd can do the privileged
/// `open()` + `drmSetMaster()` and hand weston the fd. Idempotent: no-op when a
/// live seatd is already listening.
///
/// Checks liveness by connecting to the socket, not just its existence: a
/// `docker restart` preserves the filesystem, so a stale `/run/seatd.sock` with no
/// seatd behind it would otherwise no-op and leave weston connecting to a dead
/// socket. A refused connect means stale; remove and respawn.
fn ensure_seatd() -> Result<()> {
    use std::os::unix::net::UnixStream;
    let sock = Path::new("/run/seatd.sock");
    if sock.exists() {
        if UnixStream::connect(sock).is_ok() {
            return Ok(()); // a live seatd is listening
        }
        tracing::info!(
            token = "console-stale-seatd-socket",
            "stale /run/seatd.sock (no live seatd behind it) — removing and respawning"
        );
        let _ = std::fs::remove_file(sock);
    }
    std::process::Command::new("seatd")
        .env("SEATD_VTBOUND", "0")
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .context("failed to spawn seatd — is it in the image?")?;
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if sock.exists() {
            tracing::info!("seatd up (VT-unbound): /run/seatd.sock");
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    anyhow::bail!("seatd did not create /run/seatd.sock within 5s")
}

/// Spawn a headless weston on the connected DRM connector with a FIXED Wayland
/// socket name ([`CONSOLE_SOCKET`]) and wait (≤15s) for that exact socket to
/// appear. Returns the [`WestonConsole`] holding the process alive; on timeout the
/// child is killed and an error is returned.
///
/// The socket lands in `XDG_RUNTIME_DIR` (defaulting to the node-agent's
/// `/run/quasar-agent`), so an in-process `waylandsink` reading the same
/// `XDG_RUNTIME_DIR` can reach it by socket name.
pub fn spawn_weston_console(
    session_id: &str,
    config: Option<&crate::messages::ConsoleConfig>,
) -> Result<WestonConsole> {
    // Acquire the process-wide console lock BEFORE ensure_seatd()/spawn: only one
    // physical console exists, so this blocks a new launch until the previous
    // WestonConsole's Drop has fully drained its process group (see Drop and
    // `_console_lock`), closing the stop->start race by construction. A poisoned
    // lock still recovers the guard — it serializes ordering only, protecting no
    // data a panic could leave inconsistent.
    let console_lock = CONSOLE_WESTON_LOCK
        .get_or_init(|| Mutex::new(()))
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());

    // #407: held until the socket wait below confirms weston/seatd already have the
    // display — see `drm_open_lock`. A capacity probe's own brief open cannot race in
    // between and steal master from underneath this launch.
    let _drm_open_guard = drm_open_lock()
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());

    // seatd instead of builtin libseat: on a rootful engine the agent needs only
    // CAP_SYS_ADMIN; on rootless (#407) it needs none — see the module doc.
    ensure_seatd()?;

    let xdg = std::env::var("XDG_RUNTIME_DIR").unwrap_or_else(|_| "/run/quasar-agent".to_string());
    std::fs::create_dir_all(&xdg).ok();
    let sock_path = Path::new(&xdg).join(CONSOLE_SOCKET);

    // Clear a stale socket + lock left by a killed prior run, else weston refuses
    // the name. Safe: this name is ours alone (game compositor uses wayland-N).
    let _ = std::fs::remove_file(&sock_path);
    let _ = std::fs::remove_file(format!("{}.lock", sock_path.display()));

    // `--shell=kiosk-shell.so` maps every toplevel straight to a fullscreen output,
    // fixing a `waylandsink` race: setting `fullscreen=true` there calls
    // `xdg_toplevel_set_fullscreen` before the wl surface exists
    // (`gst_wl_window_ensure_fullscreen: assertion 'self' failed`), since the
    // GstWlWindow is only created on first buffer. kiosk-shell fullscreens
    // compositor-side as soon as the surface maps, so waylandsink no longer sets
    // `fullscreen` (see `pipeline::build_local_display_pipeline`). Ships with stock
    // weston (9.0+).
    let config_path = config
        .and_then(|c| c.output_id.as_deref().map(|id| (id, c.mode.clone())))
        .map(|(output_id, mode)| -> Result<Option<std::path::PathBuf>> {
            // Gather every other connected connector so weston_output_config can
            // emit `mode=off` stanzas for them (see its doc). Uses the narrow
            // `detect_drm_outputs`, not `detect_console_capabilities` — the latter's
            // DDC/CI + audio + input enumeration would eat this fn's 15s budget.
            let outputs = crate::capacity::detect_drm_outputs();
            // #422: a pinned output with no configured mode runs at its physical
            // mode; without a config weston would light every connected output.
            let Some(mode) = mode.or_else(|| pinned_output_mode(output_id, &outputs)) else {
                tracing::warn!(
                    token = "console-pinned-output-mode-unknown",
                    "console: pinned output {output_id} has no configured mode and no \
                     detectable one; starting weston without an output config"
                );
                return Ok(None);
            };
            let pinned_connector = output_id.split_once(':').map(|(_, c)| c);
            let other_connected: Vec<String> = outputs
                .into_iter()
                .filter(|o| o.connected && Some(o.connector.as_str()) != pinned_connector)
                .map(|o| o.connector)
                .collect();
            let path = Path::new(&xdg).join(format!("weston-console-{session_id}.ini"));
            let body = weston_output_config(output_id, &mode, &other_connected)?;
            std::fs::write(&path, body).context("write session-owned Weston config")?;
            Ok(Some(path))
        })
        .transpose()?
        .flatten();

    let mut command = std::process::Command::new("weston");
    command.args([
        "--backend=drm-backend.so",
        "--shell=kiosk-shell.so",
        &format!("--socket={CONSOLE_SOCKET}"),
        "--continue-without-input",
        "--idle-time=0",
    ]);
    if let Some(path) = &config_path {
        command.arg(format!("--config={}", path.display()));
    }
    let mut child = command
        .env("XDG_RUNTIME_DIR", &xdg)
        // seatd (not builtin) holds the device + drmSetMaster, so weston needs
        // only CAP_SYS_ADMIN, not `privileged`.
        .env("LIBSEAT_BACKEND", "seatd")
        // Own process group so Drop can SIGKILL the whole group, not just weston.
        .process_group(0)
        .spawn()
        .context("failed to spawn weston — is it in the image?")?;

    let deadline = Instant::now() + Duration::from_secs(15);
    while Instant::now() < deadline {
        // The socket is created bound (weston is ready) — wait for the exact path.
        if sock_path.exists() {
            tracing::info!("weston console up: socket={CONSOLE_SOCKET}");
            return Ok(WestonConsole {
                child,
                socket: CONSOLE_SOCKET.to_string(),
                config_path,
                _console_lock: console_lock,
            });
        }
        std::thread::sleep(Duration::from_millis(200));
    }

    // This timeout path is a teardown path too: `console_lock` must release in the
    // same drained state as Drop, so mirror it exactly (SIGKILL the group, then
    // bounded-drain) rather than killing only the weston pid and bailing un-drained.
    let pgid = child.id() as i32;
    unsafe {
        libc::kill(-pgid, libc::SIGKILL);
    }
    let deadline = Instant::now() + Duration::from_secs(5);
    if drain_weston_group(&mut child, pgid, deadline, Duration::from_millis(50)) {
        tracing::debug!("console: weston group {pgid} fully drained (spawn-timeout path)");
    } else {
        tracing::error!(
            token = "console-weston-exit-timeout",
            "console: weston group {pgid} did not fully exit within 5s; next launch may hit a DRM-busy race"
        );
    }
    bail!("weston did not create the '{CONSOLE_SOCKET}' socket within 15s");
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    // DRM/weston itself isn't unit-testable (the real proof is the live churn
    // gate), but `drain_weston_group`'s polling primitive is, and exercises
    // exactly what `Drop` and the spawn-timeout path call.

    #[test]
    fn drain_weston_group_returns_true_once_the_child_exits() {
        let mut child = std::process::Command::new("sh")
            .args(["-c", "sleep 0.15"])
            .process_group(0)
            .spawn()
            .expect("spawn sh");
        let pgid = child.id() as i32;
        assert!(
            process_group_alive(pgid),
            "freshly spawned group must be visible in /proc"
        );

        let deadline = Instant::now() + Duration::from_secs(3);
        assert!(
            drain_weston_group(&mut child, pgid, deadline, Duration::from_millis(20)),
            "drain must return true once try_wait reaps the child and the group is empty"
        );
    }

    #[test]
    fn drain_weston_group_bounds_a_lingering_child() {
        let mut child = std::process::Command::new("sh")
            .args(["-c", "sleep 5"])
            .process_group(0)
            .spawn()
            .expect("spawn sh");
        let pgid = child.id() as i32;

        let deadline = Instant::now() + Duration::from_millis(80);
        assert!(
            !drain_weston_group(&mut child, pgid, deadline, Duration::from_millis(10)),
            "drain must return false (never block) when the child outlives the deadline"
        );

        unsafe {
            libc::kill(-pgid, libc::SIGKILL);
        }
        let _ = child.wait();
    }

    #[test]
    fn console_weston_lock_serializes_concurrent_holders() {
        // Proves the lock primitive spawn_weston_console leans on: a second
        // waiter is released only after the first guard drops.
        let lock: &'static Mutex<()> = CONSOLE_WESTON_LOCK.get_or_init(|| Mutex::new(()));
        let first = lock.lock().unwrap_or_else(|p| p.into_inner());

        let order = std::sync::Arc::new(std::sync::Mutex::new(Vec::<&'static str>::new()));
        let order_clone = order.clone();
        let waiter = std::thread::spawn(move || {
            let _second = CONSOLE_WESTON_LOCK
                .get()
                .unwrap()
                .lock()
                .unwrap_or_else(|p| p.into_inner());
            order_clone.lock().unwrap().push("second");
        });

        std::thread::sleep(Duration::from_millis(50));
        order.lock().unwrap().push("first-still-held");
        drop(first);
        waiter.join().unwrap();

        let seen = order.lock().unwrap().clone();
        assert_eq!(seen, vec!["first-still-held", "second"]);
    }

    #[test]
    fn drm_open_lock_serializes_concurrent_holders() {
        let first = drm_open_lock().lock().unwrap_or_else(|p| p.into_inner());

        let order = std::sync::Arc::new(std::sync::Mutex::new(Vec::<&'static str>::new()));
        let order_clone = order.clone();
        let waiter = std::thread::spawn(move || {
            let _second = drm_open_lock().lock().unwrap_or_else(|p| p.into_inner());
            order_clone.lock().unwrap().push("second");
        });

        std::thread::sleep(Duration::from_millis(50));
        order.lock().unwrap().push("first-still-held");
        drop(first);
        waiter.join().unwrap();

        let seen = order.lock().unwrap().clone();
        assert_eq!(seen, vec!["first-still-held", "second"]);
    }

    #[test]
    fn static_output_config_preserves_exact_refresh() {
        let mode = crate::messages::ConsoleModeSelection {
            width: 2560,
            height: 1440,
            refresh_millihz: 119_997,
        };
        assert_eq!(
            weston_output_config("card0:DP-4", &mode, &[]).unwrap(),
            "[output]\nname=DP-4\nmode=2560x1440@120\n"
        );
    }

    #[test]
    fn static_output_config_rejects_unscoped_connector() {
        let mode = crate::messages::ConsoleModeSelection {
            width: 1920,
            height: 1080,
            refresh_millihz: 60_000,
        };
        assert!(weston_output_config("DP-4", &mode, &[]).is_err());
    }

    // A sibling connected connector gets an explicit `mode=off` stanza; the
    // pinned connector itself must never get one, even if a caller bug echoes
    // it back in the "other" list.
    #[test]
    fn output_config_disables_other_connected_connectors() {
        let mode = crate::messages::ConsoleModeSelection {
            width: 1920,
            height: 1080,
            refresh_millihz: 60_000,
        };
        let others = vec!["DP-5".to_string(), "HDMI-A-1".to_string()];
        assert_eq!(
            weston_output_config("card0:DP-4", &mode, &others).unwrap(),
            "[output]\nname=DP-4\nmode=1920x1080@60\n\
             \n[output]\nname=DP-5\nmode=off\n\
             \n[output]\nname=HDMI-A-1\nmode=off\n"
        );
    }

    #[test]
    fn output_config_never_disables_the_pinned_connector() {
        let mode = crate::messages::ConsoleModeSelection {
            width: 1920,
            height: 1080,
            refresh_millihz: 60_000,
        };
        let others = vec!["DP-4".to_string(), "DP-5".to_string()];
        let cfg = weston_output_config("card0:DP-4", &mode, &others).unwrap();
        assert_eq!(cfg.matches("name=DP-4").count(), 1);
        assert!(!cfg.contains("name=DP-4\nmode=off"));
        assert!(cfg.contains("name=DP-5\nmode=off"));
    }

    fn drm_mode(
        width: u16,
        height: u16,
        refresh_millihz: u32,
        preferred: bool,
    ) -> crate::messages::DrmModeCapability {
        crate::messages::DrmModeCapability {
            name: format!("{width}x{height}"),
            width,
            height,
            refresh_millihz,
            preferred,
            interlaced: false,
            clock_khz: 0,
            htotal: 0,
            vtotal: 0,
        }
    }

    /// 42 modes as a 4K 240 Hz DisplayPort monitor reports them over DRM:
    /// native 3840x2160 first (preferred at 60 Hz), then scaled and legacy modes.
    fn four_k_240_modes() -> Vec<crate::messages::DrmModeCapability> {
        let table: [(u16, u16, u32); 42] = [
            (3840, 2160, 60_000),
            (3840, 2160, 239_990),
            (3840, 2160, 200_000),
            (3840, 2160, 165_000),
            (3840, 2160, 144_000),
            (3840, 2160, 120_000),
            (3840, 2160, 119_880),
            (3840, 2160, 100_000),
            (3840, 2160, 59_940),
            (3840, 2160, 50_000),
            (3840, 2160, 30_000),
            (3840, 2160, 29_970),
            (3840, 2160, 25_000),
            (3840, 2160, 24_000),
            (3840, 2160, 23_976),
            (2560, 1440, 239_970),
            (2560, 1440, 165_000),
            (2560, 1440, 144_000),
            (2560, 1440, 119_998),
            (2560, 1440, 59_951),
            (1920, 1080, 240_000),
            (1920, 1080, 144_001),
            (1920, 1080, 120_000),
            (1920, 1080, 119_880),
            (1920, 1080, 100_000),
            (1920, 1080, 60_000),
            (1920, 1080, 59_940),
            (1920, 1080, 50_000),
            (1920, 1080, 30_000),
            (1920, 1080, 24_000),
            (1680, 1050, 59_954),
            (1600, 900, 60_000),
            (1440, 900, 59_887),
            (1280, 1024, 75_025),
            (1280, 1024, 60_020),
            (1280, 800, 59_810),
            (1280, 720, 60_000),
            (1280, 720, 59_940),
            (1024, 768, 75_029),
            (1024, 768, 60_004),
            (800, 600, 60_317),
            (640, 480, 59_940),
        ];
        table
            .iter()
            .enumerate()
            .map(|(i, &(w, h, r))| drm_mode(w, h, r, i == 0))
            .collect()
    }

    fn drm_output(
        id: &str,
        connected: bool,
        active_mode: Option<crate::messages::DrmModeCapability>,
        modes: Vec<crate::messages::DrmModeCapability>,
    ) -> crate::messages::DrmOutputCapability {
        let (card, connector) = id.split_once(':').unwrap();
        crate::messages::DrmOutputCapability {
            id: id.to_string(),
            card: card.to_string(),
            render_node: None,
            connector: connector.to_string(),
            connected,
            active_mode,
            modes,
        }
    }

    // #422: a pinned output with no configured mode resolves to its active mode,
    // else its preferred one, else its first; absent/disconnected/mode-less -> None.
    #[test]
    fn pinned_output_mode_follows_the_physical_display() {
        assert_eq!(four_k_240_modes().len(), 42);
        let four_k_240 = drm_output(
            "card0:DP-4",
            true,
            Some(drm_mode(3840, 2160, 239_990, false)),
            four_k_240_modes(),
        );
        let four_k_idle = drm_output("card0:DP-4", true, None, four_k_240_modes());
        let qhd_119879 = drm_output(
            "card0:DP-5",
            true,
            Some(drm_mode(2560, 1440, 119_879, false)),
            vec![drm_mode(2560, 1440, 119_879, false)],
        );
        let no_flags = drm_output(
            "card0:DP-5",
            true,
            None,
            vec![
                drm_mode(2560, 1440, 119_879, false),
                drm_mode(1920, 1080, 60_000, false),
            ],
        );
        let unplugged = drm_output("card0:DP-4", false, None, four_k_240_modes());
        let empty = drm_output("card0:DP-4", true, None, vec![]);

        type Case<'a> = (
            &'a str,
            &'a str,
            Vec<crate::messages::DrmOutputCapability>,
            Option<(u16, u16, u32)>,
        );
        let cases: Vec<Case> = vec![
            (
                "active 4K@240 wins",
                "card0:DP-4",
                vec![qhd_119879.clone(), four_k_240.clone()],
                Some((3840, 2160, 239_990)),
            ),
            (
                "exact millihertz kept",
                "card0:DP-5",
                vec![four_k_240.clone(), qhd_119879.clone()],
                Some((2560, 1440, 119_879)),
            ),
            (
                "idle output -> preferred",
                "card0:DP-4",
                vec![four_k_idle],
                Some((3840, 2160, 60_000)),
            ),
            (
                "no active, no preferred -> first",
                "card0:DP-5",
                vec![no_flags],
                Some((2560, 1440, 119_879)),
            ),
            ("absent output", "card1:DP-4", vec![four_k_240], None),
            ("disconnected output", "card0:DP-4", vec![unplugged], None),
            ("no modes", "card0:DP-4", vec![empty], None),
        ];
        for (name, id, outputs, want) in cases {
            let got =
                pinned_output_mode(id, &outputs).map(|m| (m.width, m.height, m.refresh_millihz));
            assert_eq!(got, want, "{name}");
        }
    }

    // The resolved mode feeds the same weston ini as a configured one, rounded
    // only at that boundary (119879 mHz -> @120).
    #[test]
    fn pinned_output_mode_writes_a_weston_ini() {
        let outputs = vec![drm_output(
            "card0:DP-5",
            true,
            Some(drm_mode(2560, 1440, 119_879, false)),
            vec![drm_mode(2560, 1440, 119_879, false)],
        )];
        let mode = pinned_output_mode("card0:DP-5", &outputs).unwrap();
        assert_eq!(
            weston_output_config("card0:DP-5", &mode, &["DP-4".to_string()]).unwrap(),
            "[output]\nname=DP-5\nmode=2560x1440@120\n\n[output]\nname=DP-4\nmode=off\n"
        );
    }

    #[test]
    fn backend_selects_direct_kms_only_for_amdgpu() {
        let root =
            std::env::temp_dir().join(format!("quasar-console-backend-{}", std::process::id()));
        let driver_dir = root.join("drivers/amdgpu");
        std::fs::create_dir_all(root.join("card1/device")).unwrap();
        std::fs::create_dir_all(&driver_dir).unwrap();
        symlink(&driver_dir, root.join("card1/device/driver")).unwrap();
        let cfg: crate::messages::ConsoleConfig = serde_json::from_value(serde_json::json!({
            "enabled": true,
            "output_id": "card1:DP-1"
        }))
        .unwrap();
        assert_eq!(local_backend_at(Some(&cfg), &root), LocalBackend::DirectKms);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn backend_falls_back_to_weston_without_trusted_amd_driver() {
        let cfg: crate::messages::ConsoleConfig = serde_json::from_value(serde_json::json!({
            "enabled": true,
            "output_id": "card0:DP-4"
        }))
        .unwrap();
        assert_eq!(
            local_backend_at(Some(&cfg), Path::new("/definitely/missing")),
            LocalBackend::Weston
        );
    }

    fn console_config(output_id: Option<&str>) -> crate::messages::ConsoleConfig {
        serde_json::from_value(serde_json::json!({
            "enabled": true,
            "output_id": output_id,
        }))
        .unwrap()
    }

    fn mode(w: u16, h: u16, mhz: u32) -> crate::messages::ConsoleModeSelection {
        crate::messages::ConsoleModeSelection {
            width: w,
            height: h,
            refresh_millihz: mhz,
        }
    }

    /// #445: the compositor is told the connected display's modes, non-interlaced, each
    /// with its own refresh, deduplicated.
    #[test]
    fn console_output_modes_lists_the_pinned_displays_modes() {
        let mut modes = four_k_240_modes();
        // A duplicate timing (two DRM modes with the same WxH@mHz) collapses to one.
        modes.push(modes[0].clone());
        // An interlaced mode is left out: the compositor cannot render a field.
        let mut interlaced = drm_mode(1920, 1080, 60_000, false);
        interlaced.interlaced = true;
        modes.push(interlaced);
        let outputs = vec![
            drm_output("card1:DP-1", true, None, modes.clone()),
            drm_output(
                "card1:HDMI-A-1",
                true,
                None,
                vec![drm_mode(1280, 720, 60_000, true)],
            ),
        ];

        let got = console_output_modes(Some(&console_config(Some("card1:DP-1"))), &outputs);
        let expected: Vec<_> = four_k_240_modes()
            .iter()
            .map(|m| mode(m.width, m.height, m.refresh_millihz))
            .collect();
        assert_eq!(got, expected);

        // Automatic: the first connected output.
        let got = console_output_modes(Some(&console_config(None)), &outputs);
        assert_eq!(got, expected);

        // A pinned output that is not connected, or no output at all: nothing to advertise.
        assert!(
            console_output_modes(Some(&console_config(Some("card1:DP-9"))), &outputs).is_empty()
        );
        assert!(console_output_modes(None, &[]).is_empty());
    }

    /// #445: the config weston is restarted with pins the connector the session is on and
    /// carries the chosen mode, whether the admin pinned an output or left it Automatic.
    #[test]
    fn console_config_at_mode_pins_the_connector_and_the_mode() {
        let outputs = vec![
            drm_output("card1:DP-1", false, None, vec![]),
            drm_output("card1:HDMI-A-1", true, None, four_k_240_modes()),
        ];
        let chosen = mode(2560, 1440, 143_981);

        let at = console_config_at_mode(&console_config(None), chosen.clone(), &outputs);
        assert_eq!(at.output_id.as_deref(), Some("card1:HDMI-A-1"));
        assert_eq!(at.mode, Some(chosen.clone()));
        assert!(at.enabled, "everything else is carried over");

        let at = console_config_at_mode(
            &console_config(Some("card1:DP-1")),
            chosen.clone(),
            &outputs,
        );
        assert_eq!(
            at.output_id.as_deref(),
            Some("card1:DP-1"),
            "an admin pin is kept"
        );
        assert_eq!(at.mode, Some(chosen));
    }

    /// #445: the switch rule.
    #[test]
    fn plan_mode_switch_accepts_only_a_different_available_mode_on_a_local_console() {
        let available = vec![
            mode(3840, 2160, 239_990),
            mode(2560, 1440, 143_981),
            mode(1920, 1080, 119_880),
            mode(1920, 1080, 60_000),
        ];
        let current = mode(1920, 1080, 60_000);

        // The compositor rounds to whole hertz on the caps; the DRM entry is the truth.
        assert_eq!(
            plan_mode_switch((2560, 1440, 144_000), &current, &available, false),
            Ok(mode(2560, 1440, 143_981))
        );
        assert_eq!(
            plan_mode_switch((1920, 1080, 120_000), &current, &available, false),
            Ok(mode(1920, 1080, 119_880))
        );
        assert_eq!(
            plan_mode_switch((1920, 1080, 60_000), &current, &available, false),
            Err(ModeSwitchRefusal::AlreadyCurrent)
        );
        assert_eq!(
            plan_mode_switch((1920, 1080, 75_000), &current, &available, false),
            Err(ModeSwitchRefusal::NotAvailable),
            "a refresh more than half a hertz off every entry is not that entry"
        );
        assert_eq!(
            plan_mode_switch((800, 600, 60_000), &current, &available, false),
            Err(ModeSwitchRefusal::NotAvailable)
        );
        assert_eq!(
            plan_mode_switch((2560, 1440, 144_000), &current, &available, true),
            Err(ModeSwitchRefusal::Streaming)
        );
        assert_eq!(
            plan_mode_switch((0, 1440, 144_000), &current, &available, false),
            Err(ModeSwitchRefusal::Malformed)
        );
        assert_eq!(mode_fps(&mode(2560, 1440, 143_981)), 144);
        assert_eq!(mode_fps(&mode(1920, 1080, 59_940)), 60);
    }
}
