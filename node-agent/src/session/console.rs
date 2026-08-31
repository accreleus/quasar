//! Headless `weston` process manager for the local-display path on nvidia-drm KMS.
//!
//! kmssink can't drive `nvidia-drm` (atomic-KMS only, kmssink is legacy modesetting).
//! weston's `drm-backend.so` speaks atomic KMS, so: spawn headless weston (takes DRM
//! master, enables the connected output), then `waylandsink` renders into its socket.
//!
//! weston needs container `CAP_SYS_ADMIN` at runtime for `drmSetMaster` — granted by
//! the compose/deploy layer, not this module.

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
/// the device without full `privileged` — only `CAP_SYS_ADMIN`.
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

    // seatd instead of builtin libseat: the agent needs only CAP_SYS_ADMIN.
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
        .and_then(|c| c.output_id.as_deref().zip(c.mode.as_ref()))
        .map(|(output_id, mode)| -> Result<std::path::PathBuf> {
            // Gather every other connected connector so weston_output_config can
            // emit `mode=off` stanzas for them (see its doc). Uses the narrow
            // `detect_drm_outputs`, not `detect_console_capabilities` — the latter's
            // DDC/CI + audio + input enumeration would eat this fn's 15s budget.
            let pinned_connector = output_id.split_once(':').map(|(_, c)| c);
            let other_connected: Vec<String> = crate::capacity::detect_drm_outputs()
                .into_iter()
                .filter(|o| o.connected && Some(o.connector.as_str()) != pinned_connector)
                .map(|o| o.connector)
                .collect();
            let path = Path::new(&xdg).join(format!("weston-console-{session_id}.ini"));
            let body = weston_output_config(output_id, mode, &other_connected)?;
            std::fs::write(&path, body).context("write session-owned Weston config")?;
            Ok(path)
        })
        .transpose()?;

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
}
