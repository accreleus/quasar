//! Console mode's own virtual terminal (#407).
//!
//! The kernel's VT keyboard handler attaches to every keyboard evdev device, the session's
//! virtual keyboard included, and types into the foreground VT: on most hosts a login
//! prompt. So for the whole life of a local console session the agent makes
//! [`CONSOLE_VT`] the active VT with its keyboard off (`K_OFF`) and in `KD_GRAPHICS`, then
//! puts the previous VT back, as a desktop login does.
//!
//! Those ioctls need the VT to be the caller's controlling terminal (or
//! `CAP_SYS_TTY_CONFIG`, which the agent never has). A helper child
//! (`quasar-node-agent console-vt hold`) calls `setsid`, opens the VT so it becomes its
//! controlling terminal, and holds it until its stdin closes or it is signalled, restoring
//! on the way out. Fail-closed: a host with VTs never runs a console session without the
//! VT taken. `K_OFF` also disables the VT-switch keys, so a helper killed before it could
//! restore leaves the console on the dedicated VT: the next agent start
//! ([`reconcile_at_startup`], run by the console preflight) and the next take put it back.

use std::io::{self, BufRead, BufReader, Write};
use std::os::unix::fs::FileTypeExt;
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::{Duration, Instant};

/// Outside logind's default `NAutoVTs=6`, so no getty is started on it. The other halves:
/// `deploy/prepare-host.sh` (ACL, masked `getty@`/`autovt@tty8`) and the recovery recipe's
/// `CONSOLE_VT_NODE`.
pub const CONSOLE_VT: u32 = 8;

/// The hidden subcommand the helper runs as (`main.rs`).
pub const HELPER_ARG: &str = "console-vt";

/// What the helper recorded before switching, so a restart can undo a helper that died
/// holding the VT. Under the agent's runtime directory: VT state does not survive a reboot.
const STATE_FILE: &str = "/run/quasar-agent/console-vt.state";

const SWITCH_TIMEOUT: Duration = Duration::from_secs(5);
/// Covers the helper's own `SWITCH_TIMEOUT` and process start.
const ANSWER_TIMEOUT: Duration = Duration::from_secs(15);
const RELEASE_TIMEOUT: Duration = Duration::from_secs(10);
/// A previous console session's teardown still restoring the VT.
const LOCK_TIMEOUT: Duration = Duration::from_secs(30);

// linux/kd.h, linux/vt.h
const KDSETMODE: libc::Ioctl = 0x4B3A;
const KDGETMODE: libc::Ioctl = 0x4B3B;
const KDGKBMODE: libc::Ioctl = 0x4B44;
const KDSKBMODE: libc::Ioctl = 0x4B45;
const VT_GETSTATE: libc::Ioctl = 0x5603;
const VT_ACTIVATE: libc::Ioctl = 0x5606;
pub(crate) const KD_TEXT: i32 = 0;
pub(crate) const KD_GRAPHICS: i32 = 1;
pub(crate) const K_UNICODE: i32 = 3;
pub(crate) const K_OFF: i32 = 4;

const PREPARE_HINT: &str = "run host preparation (deploy/prepare-host.sh --console) as root, \
     then turn console mode off and on again";

/// The VT operations, behind a trait so the take/restore logic is tested without a VT.
pub(crate) trait Vt {
    /// This process's session has the VT as its controlling terminal.
    fn controlling(&self) -> io::Result<bool>;
    fn active(&self) -> io::Result<u32>;
    fn kb_mode(&self) -> io::Result<i32>;
    fn set_kb_mode(&self, mode: i32) -> io::Result<()>;
    fn kd_mode(&self) -> io::Result<i32>;
    fn set_kd_mode(&self, mode: i32) -> io::Result<()>;
    fn activate(&self, n: u32) -> io::Result<()>;
}

/// What a restore puts back.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct Saved {
    pub prev: u32,
    pub kb: i32,
    pub kd: i32,
}

impl Saved {
    const DEFAULT: Saved = Saved {
        prev: 1,
        kb: K_UNICODE,
        kd: KD_TEXT,
    };

    fn encode(&self) -> String {
        format!("prev={} kb={} kd={}\n", self.prev, self.kb, self.kd)
    }

    fn decode(body: &str) -> Option<Saved> {
        let mut saved = Saved::DEFAULT;
        let (mut prev, mut kb, mut kd) = (false, false, false);
        for word in body.split_whitespace() {
            let (key, value) = word.split_once('=')?;
            match key {
                "prev" => (saved.prev, prev) = (value.parse().ok().filter(|v| *v > 0)?, true),
                "kb" => (saved.kb, kb) = (value.parse().ok()?, true),
                "kd" => (saved.kd, kd) = (value.parse().ok()?, true),
                _ => {}
            }
        }
        (prev && kb && kd).then_some(saved)
    }

    fn load(path: &Path) -> Option<Saved> {
        Saved::decode(&std::fs::read_to_string(path).ok()?)
    }

    fn store(&self, path: &Path) -> io::Result<()> {
        let tmp = path.with_extension("new");
        std::fs::write(&tmp, self.encode())?;
        std::fs::rename(&tmp, path)
    }

    /// Never restore into the state being undone.
    fn sanitized(self, n: u32) -> Saved {
        Saved {
            prev: if self.prev == n { 1 } else { self.prev },
            kb: if self.kb == K_OFF { K_UNICODE } else { self.kb },
            kd: if self.kd == KD_GRAPHICS {
                KD_TEXT
            } else {
                self.kd
            },
        }
    }
}

fn io_msg(what: &str, e: io::Error) -> String {
    format!("{what}: {e}")
}

fn wait_active(vt: &dyn Vt, want: u32, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    loop {
        if vt.active().is_ok_and(|a| a == want) {
            return true;
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// Make VT `n` active with its keyboard off. On any failure everything already changed is
/// put back before the error is returned.
pub(crate) fn take(vt: &dyn Vt, n: u32, state: &Path, timeout: Duration) -> Result<Saved, String> {
    if !vt
        .controlling()
        .map_err(|e| io_msg("read the terminal's session", e))?
    {
        return Err(format!(
            "tty{n} is another session's terminal (a login prompt on it?)"
        ));
    }
    let active = vt.active().map_err(|e| io_msg("read the active VT", e))?;
    let kb = vt
        .kb_mode()
        .map_err(|e| io_msg("read the keyboard mode", e))?;
    let kd = vt
        .kd_mode()
        .map_err(|e| io_msg("read the display mode", e))?;
    // A VT already switched to and turned off is what a killed holder left: undo to what
    // it recorded, not to that.
    let stale = Saved::load(state).unwrap_or(Saved::DEFAULT);
    let saved = Saved {
        prev: if active == n { stale.prev } else { active },
        kb: if kb == K_OFF { stale.kb } else { kb },
        kd,
    }
    .sanitized(n);
    if let Err(e) = saved.store(state) {
        tracing::warn!(
            token = "console-vt-record-failed",
            "could not record the console terminal's previous state at {} ({e}); a crash \
             before the session ends restores the defaults (tty1)",
            state.display()
        );
    }
    let switched = (|| -> Result<(), String> {
        vt.set_kb_mode(K_OFF)
            .map_err(|e| io_msg("turn the keyboard off", e))?;
        vt.set_kd_mode(KD_GRAPHICS)
            .map_err(|e| io_msg("set graphics mode", e))?;
        vt.activate(n)
            .map_err(|e| io_msg(&format!("switch to tty{n}"), e))?;
        if !wait_active(vt, n, timeout) {
            return Err(format!(
                "tty{n} did not become the active terminal within {}s",
                timeout.as_secs()
            ));
        }
        if vt.kb_mode().ok() != Some(K_OFF) {
            return Err(format!("tty{n}'s keyboard did not stay off"));
        }
        Ok(())
    })();
    match switched {
        Ok(()) => Ok(saved),
        Err(e) => {
            let _ = restore(vt, n, &saved, state, timeout);
            Err(e)
        }
    }
}

/// Switch back to `saved.prev` if VT `n` is still active, then give `n` its keyboard and
/// text mode back. Every step is tried; the record is removed only when all succeeded.
pub(crate) fn restore(
    vt: &dyn Vt,
    n: u32,
    saved: &Saved,
    state: &Path,
    timeout: Duration,
) -> Result<(), String> {
    let saved = saved.sanitized(n);
    let mut problems = Vec::new();
    match vt.active() {
        Ok(a) if a == n => {
            if let Err(e) = vt.activate(saved.prev) {
                problems.push(io_msg(&format!("switch back to tty{}", saved.prev), e));
            } else if !wait_active(vt, saved.prev, timeout) {
                problems.push(format!(
                    "tty{} did not become active again within {}s",
                    saved.prev,
                    timeout.as_secs()
                ));
            }
        }
        Ok(_) => {}
        Err(e) => problems.push(io_msg("read the active VT", e)),
    }
    if let Err(e) = vt.set_kb_mode(saved.kb) {
        problems.push(io_msg("turn the keyboard back on", e));
    }
    if let Err(e) = vt.set_kd_mode(saved.kd) {
        problems.push(io_msg("set text mode", e));
    }
    if problems.is_empty() {
        let _ = std::fs::remove_file(state);
        Ok(())
    } else {
        Err(problems.join("; "))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Reconciled {
    Clean,
    Restored,
}

/// Undo what a holder that died without restoring left behind, when nothing holds the VT.
pub(crate) fn reconcile(
    vt: &dyn Vt,
    n: u32,
    state: &Path,
    timeout: Duration,
) -> Result<Reconciled, String> {
    if !vt
        .controlling()
        .map_err(|e| io_msg("read the terminal's session", e))?
    {
        return Err(format!(
            "tty{n} is another session's terminal (a login prompt on it?)"
        ));
    }
    let active = vt.active().map_err(|e| io_msg("read the active VT", e))?;
    let kb = vt
        .kb_mode()
        .map_err(|e| io_msg("read the keyboard mode", e))?;
    let kd = vt
        .kd_mode()
        .map_err(|e| io_msg("read the display mode", e))?;
    let record = Saved::load(state);
    let stranded = kb == K_OFF || kd == KD_GRAPHICS || (active == n && record.is_some());
    if !stranded {
        let _ = std::fs::remove_file(state);
        return Ok(Reconciled::Clean);
    }
    restore(vt, n, &record.unwrap_or(Saved::DEFAULT), state, timeout).map(|()| Reconciled::Restored)
}

/// Whether the host has VTs, and whether this agent was given the console VT.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Presence {
    /// No VTs (`CONFIG_VT` off): no kernel keyboard handler, nothing typed reaches a VT.
    NoVts,
    Present,
    /// The host has VTs (or cannot be read) and the VT cannot be used: fail closed.
    Unusable(String),
}

pub(crate) fn presence(sys_class_tty: &Path, node: &Path) -> Presence {
    if !sys_class_tty.join("tty0").exists() {
        return if sys_class_tty.is_dir() {
            Presence::NoVts
        } else {
            Presence::Unusable(format!(
                "cannot tell whether this host has virtual terminals: {} is not visible to \
                 the agent",
                sys_class_tty.display()
            ))
        };
    }
    match std::fs::metadata(node) {
        Ok(m) if m.file_type().is_char_device() => Presence::Present,
        _ => Presence::Unusable(format!(
            "this host has virtual terminals but {} is not in the agent's container; turn \
             console mode off and on again so the agent is re-created with it (a Compose \
             install: use the current deploy/overlays/docker-compose.console.yml)",
            node.display()
        )),
    }
}

fn node_path(n: u32) -> PathBuf {
    PathBuf::from(format!("/dev/tty{n}"))
}

/// The VT's device, opened as this process's controlling terminal.
struct RealVt(std::fs::File);

impl RealVt {
    /// `setsid`, then open without `O_NOCTTY`: the kernel makes a free tty the controlling
    /// terminal of a session leader that has none.
    fn open_controlling(node: &Path) -> io::Result<RealVt> {
        // SAFETY: setsid/getsid/getpid take no pointers.
        unsafe {
            if libc::setsid() == -1 && libc::getsid(0) != libc::getpid() {
                return Err(io::Error::last_os_error());
            }
        }
        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(node)?;
        Ok(RealVt(file))
    }

    fn get_int(&self, request: libc::Ioctl) -> io::Result<i32> {
        let mut value: libc::c_int = 0;
        // SAFETY: the request writes one int through the pointer.
        let r = unsafe { libc::ioctl(self.0.as_raw_fd(), request, &mut value) };
        if r == -1 {
            return Err(io::Error::last_os_error());
        }
        Ok(value)
    }

    fn set(&self, request: libc::Ioctl, value: libc::c_ulong) -> io::Result<()> {
        // SAFETY: the request takes its argument by value.
        let r = unsafe { libc::ioctl(self.0.as_raw_fd(), request, value) };
        if r == -1 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
}

impl Vt for RealVt {
    fn controlling(&self) -> io::Result<bool> {
        let mut sid: libc::pid_t = 0;
        // SAFETY: TIOCGSID writes one pid_t; it fails ENOTTY when the tty is not ours.
        let r = unsafe { libc::ioctl(self.0.as_raw_fd(), libc::TIOCGSID as _, &mut sid) };
        if r == -1 {
            let e = io::Error::last_os_error();
            return if e.raw_os_error() == Some(libc::ENOTTY) {
                Ok(false)
            } else {
                Err(e)
            };
        }
        // SAFETY: no arguments.
        Ok(sid == unsafe { libc::getpid() })
    }

    fn active(&self) -> io::Result<u32> {
        #[repr(C)]
        struct VtStat {
            v_active: libc::c_ushort,
            v_signal: libc::c_ushort,
            v_state: libc::c_ushort,
        }
        let mut stat = VtStat {
            v_active: 0,
            v_signal: 0,
            v_state: 0,
        };
        // SAFETY: VT_GETSTATE writes one `struct vt_stat`.
        let r = unsafe { libc::ioctl(self.0.as_raw_fd(), VT_GETSTATE, &mut stat) };
        if r == -1 {
            return Err(io::Error::last_os_error());
        }
        Ok(u32::from(stat.v_active))
    }

    fn kb_mode(&self) -> io::Result<i32> {
        self.get_int(KDGKBMODE)
    }

    fn set_kb_mode(&self, mode: i32) -> io::Result<()> {
        self.set(KDSKBMODE, mode as libc::c_ulong)
    }

    fn kd_mode(&self) -> io::Result<i32> {
        self.get_int(KDGETMODE)
    }

    fn set_kd_mode(&self, mode: i32) -> io::Result<()> {
        self.set(KDSETMODE, mode as libc::c_ulong)
    }

    fn activate(&self, n: u32) -> io::Result<()> {
        self.set(VT_ACTIVATE, libc::c_ulong::from(n))
    }
}

static RELEASE: AtomicBool = AtomicBool::new(false);

extern "C" fn on_release_signal(_: libc::c_int) {
    RELEASE.store(true, Ordering::SeqCst);
}

fn install_release_signals() {
    for signal in [libc::SIGTERM, libc::SIGINT, libc::SIGHUP] {
        // SAFETY: a zeroed sigaction with an async-signal-safe handler (one atomic store).
        unsafe {
            let mut action: libc::sigaction = std::mem::zeroed();
            action.sa_sigaction = on_release_signal as extern "C" fn(libc::c_int) as usize;
            libc::sigemptyset(&mut action.sa_mask);
            libc::sigaction(signal, &action, std::ptr::null_mut());
        }
    }
}

/// Until stdin reaches EOF (the agent closed it, or died) or a release signal arrives.
fn wait_for_release() {
    let mut buf = [0u8; 64];
    while !RELEASE.load(Ordering::SeqCst) {
        let mut pfd = libc::pollfd {
            fd: 0,
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: one pollfd; the timeout rechecks the flag a signal sets on another thread.
        let r = unsafe { libc::poll(&mut pfd, 1, 500) };
        if r == -1 {
            if io::Error::last_os_error().kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return;
        }
        if r == 0 {
            continue;
        }
        // SAFETY: reads into a local buffer of the given length.
        let n = unsafe { libc::read(0, buf.as_mut_ptr().cast(), buf.len()) };
        if n == 0 {
            return;
        }
        if n < 0 && io::Error::last_os_error().kind() != io::ErrorKind::Interrupted {
            return;
        }
    }
}

fn answer(line: &str) {
    let line = line.replace(['\n', '\r'], " ");
    let mut out = io::stdout().lock();
    let _ = writeln!(out, "{line}");
    let _ = out.flush();
}

/// The helper: `console-vt hold|reconcile [--vt N] [--state PATH]`. Answers one line on
/// stdout (`ok …` or `err <reason>`); logs go to stderr.
pub fn helper_main(args: &[String]) -> i32 {
    let value = |flag: &str| {
        args.iter()
            .position(|a| a == flag)
            .and_then(|i| args.get(i + 1))
            .cloned()
    };
    let n = value("--vt")
        .and_then(|v| v.parse().ok())
        .unwrap_or(CONSOLE_VT);
    let state = PathBuf::from(value("--state").unwrap_or_else(|| STATE_FILE.to_string()));
    let node = node_path(n);
    let action = args.first().map(String::as_str);
    if !matches!(action, Some("hold" | "reconcile")) {
        answer("err usage: console-vt hold|reconcile [--vt N] [--state PATH]");
        return 2;
    }
    if action == Some("hold") {
        install_release_signals();
    }
    let vt = match RealVt::open_controlling(&node) {
        Ok(vt) => vt,
        Err(e) if e.kind() == io::ErrorKind::PermissionDenied => {
            answer(&format!(
                "err could not open {} ({e}); {PREPARE_HINT}",
                node.display()
            ));
            return 1;
        }
        Err(e) => {
            answer(&format!("err could not open {} ({e})", node.display()));
            return 1;
        }
    };
    if action == Some("reconcile") {
        return match reconcile(&vt, n, &state, SWITCH_TIMEOUT) {
            Ok(Reconciled::Clean) => {
                answer("ok clean");
                0
            }
            Ok(Reconciled::Restored) => {
                answer("ok restored");
                0
            }
            Err(e) => {
                answer(&format!("err {e}"));
                1
            }
        };
    }
    let saved = match take(&vt, n, &state, SWITCH_TIMEOUT) {
        Ok(saved) => saved,
        Err(e) => {
            answer(&format!("err {e}"));
            return 1;
        }
    };
    answer(&format!("ok {}", saved.prev));
    wait_for_release();
    match restore(&vt, n, &saved, &state, SWITCH_TIMEOUT) {
        Ok(()) => 0,
        Err(e) => {
            tracing::error!(
                token = "console-vt-restore-failed",
                "could not fully restore the console terminal: {e}; from a shell on this \
                 host, `chvt {}` switches back",
                saved.prev
            );
            1
        }
    }
}

static CONSOLE_VT_LOCK: OnceLock<Mutex<()>> = OnceLock::new();

fn lock_bounded() -> Result<MutexGuard<'static, ()>, String> {
    let lock = CONSOLE_VT_LOCK.get_or_init(|| Mutex::new(()));
    let deadline = Instant::now() + LOCK_TIMEOUT;
    loop {
        match lock.try_lock() {
            Ok(guard) => return Ok(guard),
            Err(std::sync::TryLockError::Poisoned(p)) => return Ok(p.into_inner()),
            Err(std::sync::TryLockError::WouldBlock) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(50));
            }
            Err(std::sync::TryLockError::WouldBlock) => {
                return Err(format!(
                    "a previous console session did not release the console terminal within {}s",
                    LOCK_TIMEOUT.as_secs()
                ))
            }
        }
    }
}

fn helper_command(action: &str) -> Result<Command, String> {
    let exe =
        std::env::current_exe().map_err(|e| format!("cannot find the agent's own binary: {e}"))?;
    let mut command = Command::new(exe);
    command.args([
        HELPER_ARG,
        action,
        "--vt",
        &CONSOLE_VT.to_string(),
        "--state",
        STATE_FILE,
    ]);
    Ok(command)
}

/// Wait for `child` to exit, polling, up to `timeout`.
fn wait_bounded(child: &mut Child, timeout: Duration) -> Option<std::process::ExitStatus> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Ok(Some(status)) = child.try_wait() {
            return Some(status);
        }
        if Instant::now() >= deadline {
            return None;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
}

fn stop(child: &mut Child) {
    // SAFETY: signals our own child by pid.
    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    if wait_bounded(child, Duration::from_secs(2)).is_none() {
        let _ = child.kill();
        let _ = child.wait();
    }
}

/// Start the helper and read its one-line answer. `Ok` carries the text after `ok `.
fn start(mut command: Command, timeout: Duration) -> Result<(Child, String), String> {
    let mut child = command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .map_err(|e| format!("could not start the console terminal helper: {e}"))?;
    let Some(stdout) = child.stdout.take() else {
        stop(&mut child);
        return Err("the console terminal helper has no stdout".into());
    };
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let mut line = String::new();
        let _ = BufReader::new(stdout).read_line(&mut line);
        let _ = tx.send(line);
    });
    let line = match rx.recv_timeout(timeout) {
        Ok(line) => line,
        Err(_) => {
            stop(&mut child);
            return Err(format!(
                "the console terminal helper did not answer within {}s",
                timeout.as_secs()
            ));
        }
    };
    let line = line.trim();
    if let Some(rest) = line.strip_prefix("ok") {
        return Ok((child, rest.trim().to_string()));
    }
    stop(&mut child);
    Err(match line.strip_prefix("err ") {
        Some(reason) => reason.to_string(),
        None if line.is_empty() => "the console terminal helper exited without answering".into(),
        None => format!("the console terminal helper answered {line:?}"),
    })
}

struct Holder {
    child: Child,
    stdin: Option<ChildStdin>,
}

impl Holder {
    fn from_started(mut child: Child) -> Holder {
        let stdin = child.stdin.take();
        Holder { child, stdin }
    }

    /// Close stdin (the helper restores and exits), then escalate. `Err` when the helper
    /// could not be shown to have restored.
    fn release(&mut self, timeout: Duration) -> Result<(), String> {
        drop(self.stdin.take());
        match wait_bounded(&mut self.child, timeout) {
            Some(status) if status.success() => Ok(()),
            Some(status) => Err(format!("the helper reported a failed restore ({status})")),
            None => {
                stop(&mut self.child);
                Err(format!(
                    "the helper did not restore within {}s and was stopped",
                    timeout.as_secs()
                ))
            }
        }
    }
}

/// The console VT, held for one local console session. Dropping it restores the previous
/// VT; declare it before anything that can type (the session's virtual devices, the
/// physical-input forwarder) so it drops after them.
pub struct ConsoleVt {
    holder: Option<Holder>,
    // Last: the next take must not start before this one has restored.
    _lock: MutexGuard<'static, ()>,
}

impl ConsoleVt {
    /// Blocking. `Ok` with nothing held on a host with no VTs.
    pub fn take() -> anyhow::Result<ConsoleVt> {
        let lock = lock_bounded().map_err(anyhow::Error::msg)?;
        match presence(Path::new("/sys/class/tty"), &node_path(CONSOLE_VT)) {
            Presence::NoVts => {
                tracing::info!(
                    token = "console-vt-none",
                    "console: this host has no virtual terminals, so none is taken"
                );
                return Ok(ConsoleVt {
                    holder: None,
                    _lock: lock,
                });
            }
            Presence::Unusable(why) => anyhow::bail!(why),
            Presence::Present => {}
        }
        let (child, prev) = start(
            helper_command("hold").map_err(anyhow::Error::msg)?,
            ANSWER_TIMEOUT,
        )
        .map_err(anyhow::Error::msg)?;
        tracing::info!(
            token = "console-vt-taken",
            "console: tty{CONSOLE_VT} is the active terminal with its keyboard off (was tty{prev})"
        );
        Ok(ConsoleVt {
            holder: Some(Holder::from_started(child)),
            _lock: lock,
        })
    }

    /// `Some(reason)` once the helper has exited mid-session: the VT may be back in the
    /// kernel's hands, so the session must end.
    pub fn lost(&mut self) -> Option<String> {
        let holder = self.holder.as_mut()?;
        match holder.child.try_wait() {
            Ok(None) => None,
            Ok(Some(status)) => Some(format!("the console terminal helper exited ({status})")),
            Err(e) => Some(format!("the console terminal helper cannot be polled: {e}")),
        }
    }
}

impl Drop for ConsoleVt {
    fn drop(&mut self) {
        let Some(holder) = self.holder.as_mut() else {
            return;
        };
        match holder.release(RELEASE_TIMEOUT) {
            Ok(()) => tracing::info!(
                token = "console-vt-released",
                "console: the previous terminal is active again"
            ),
            Err(e) => tracing::error!(
                token = "console-vt-release-failed",
                "console: tty{CONSOLE_VT} may still be active with its keyboard off: {e}; \
                 the next agent start or console session restores it"
            ),
        }
    }
}

/// Run by the console preflight at startup: undo what a killed holder left, and prove the
/// VT can be taken. `Err` fails the preflight.
pub(crate) fn reconcile_at_startup() -> Result<(), String> {
    let _lock = lock_bounded()?;
    match presence(Path::new("/sys/class/tty"), &node_path(CONSOLE_VT)) {
        Presence::NoVts => return Ok(()),
        Presence::Unusable(why) => return Err(why),
        Presence::Present => {}
    }
    let (mut child, outcome) = start(helper_command("reconcile")?, ANSWER_TIMEOUT)?;
    if wait_bounded(&mut child, SWITCH_TIMEOUT).is_none() {
        stop(&mut child);
    }
    if outcome == "restored" {
        tracing::warn!(
            token = "console-vt-restored-at-startup",
            "console: tty{CONSOLE_VT} was left active with its keyboard off by an earlier \
             agent; the previous terminal is active again"
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::{Cell, RefCell};

    /// A VT layer that records what was done to it.
    struct FakeVt {
        n: u32,
        controlling: bool,
        active: Cell<u32>,
        kb: Cell<i32>,
        kd: Cell<i32>,
        /// `activate` is accepted but the switch never happens (a VT_PROCESS owner refusing).
        switch_stalls: bool,
        refuse_kb: bool,
        log: RefCell<Vec<String>>,
    }

    impl FakeVt {
        fn new(n: u32, active: u32) -> FakeVt {
            FakeVt {
                n,
                controlling: true,
                active: Cell::new(active),
                kb: Cell::new(K_UNICODE),
                kd: Cell::new(KD_TEXT),
                switch_stalls: false,
                refuse_kb: false,
                log: RefCell::new(Vec::new()),
            }
        }
    }

    impl Vt for FakeVt {
        fn controlling(&self) -> io::Result<bool> {
            Ok(self.controlling)
        }
        fn active(&self) -> io::Result<u32> {
            Ok(self.active.get())
        }
        fn kb_mode(&self) -> io::Result<i32> {
            Ok(self.kb.get())
        }
        fn set_kb_mode(&self, mode: i32) -> io::Result<()> {
            self.log.borrow_mut().push(format!("kb={mode}"));
            if self.refuse_kb {
                return Err(io::Error::from_raw_os_error(libc::EPERM));
            }
            self.kb.set(mode);
            Ok(())
        }
        fn kd_mode(&self) -> io::Result<i32> {
            Ok(self.kd.get())
        }
        fn set_kd_mode(&self, mode: i32) -> io::Result<()> {
            self.log.borrow_mut().push(format!("kd={mode}"));
            self.kd.set(mode);
            Ok(())
        }
        fn activate(&self, n: u32) -> io::Result<()> {
            self.log.borrow_mut().push(format!("activate={n}"));
            if !self.switch_stalls {
                self.active.set(n);
            }
            Ok(())
        }
    }

    const SHORT: Duration = Duration::from_millis(60);

    fn state() -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("console-vt.state");
        (dir, path)
    }

    fn off(vt: &FakeVt) -> bool {
        vt.active.get() == vt.n && vt.kb.get() == K_OFF && vt.kd.get() == KD_GRAPHICS
    }

    fn back(vt: &FakeVt, prev: u32) -> bool {
        vt.active.get() == prev && vt.kb.get() == K_UNICODE && vt.kd.get() == KD_TEXT
    }

    #[test]
    fn take_switches_with_the_keyboard_off_and_restore_puts_everything_back() {
        let (_d, path) = state();
        let vt = FakeVt::new(8, 2);
        let saved = take(&vt, 8, &path, SHORT).unwrap();
        assert!(off(&vt), "{:?}", vt.log);
        // The keyboard is off before the switch, so nothing typed lands on the new VT.
        assert_eq!(vt.log.borrow()[..3], ["kb=4", "kd=1", "activate=8"]);
        assert_eq!(Saved::load(&path), Some(saved));

        restore(&vt, 8, &saved, &path, SHORT).unwrap();
        assert!(back(&vt, 2), "{:?}", vt.log);
        assert!(!path.exists(), "a clean restore removes the record");
    }

    /// A holder killed with the VT taken: the startup reconcile restores from its record.
    #[test]
    fn reconcile_restores_what_a_dead_holder_left() {
        let (_d, path) = state();
        let vt = FakeVt::new(8, 3);
        take(&vt, 8, &path, SHORT).unwrap();
        assert!(off(&vt));

        assert_eq!(reconcile(&vt, 8, &path, SHORT), Ok(Reconciled::Restored));
        assert!(back(&vt, 3), "{:?}", vt.log);
        assert!(!path.exists());
    }

    #[test]
    fn reconcile_without_a_record_falls_back_to_tty1() {
        let (_d, path) = state();
        let vt = FakeVt::new(8, 8);
        vt.kb.set(K_OFF);
        vt.kd.set(KD_GRAPHICS);
        assert_eq!(reconcile(&vt, 8, &path, SHORT), Ok(Reconciled::Restored));
        assert!(back(&vt, 1), "{:?}", vt.log);
    }

    #[test]
    fn reconcile_of_a_clean_vt_changes_nothing() {
        let (_d, path) = state();
        let vt = FakeVt::new(8, 1);
        assert_eq!(reconcile(&vt, 8, &path, SHORT), Ok(Reconciled::Clean));
        assert!(vt.log.borrow().is_empty(), "{:?}", vt.log);
    }

    /// A new take over a stranded VT must not record the stranded state as "previous".
    #[test]
    fn take_over_a_stranded_vt_keeps_the_original_previous_state() {
        let (_d, path) = state();
        let vt = FakeVt::new(8, 4);
        take(&vt, 8, &path, SHORT).unwrap();
        let again = take(&vt, 8, &path, SHORT).unwrap();
        assert_eq!(
            again,
            Saved {
                prev: 4,
                kb: K_UNICODE,
                kd: KD_TEXT
            }
        );
        restore(&vt, 8, &again, &path, SHORT).unwrap();
        assert!(back(&vt, 4));
    }

    #[test]
    fn a_vt_held_by_another_session_is_refused_untouched() {
        let (_d, path) = state();
        let mut vt = FakeVt::new(8, 1);
        vt.controlling = false;
        let e = take(&vt, 8, &path, SHORT).unwrap_err();
        assert!(e.contains("another session's terminal"), "{e}");
        assert!(reconcile(&vt, 8, &path, SHORT).is_err());
        assert!(vt.log.borrow().is_empty(), "{:?}", vt.log);
    }

    /// Fail closed, and leave the host as it was.
    #[test]
    fn a_refused_keyboard_mode_fails_the_take_and_changes_nothing_lasting() {
        let (_d, path) = state();
        let mut vt = FakeVt::new(8, 1);
        vt.refuse_kb = true;
        let e = take(&vt, 8, &path, SHORT).unwrap_err();
        assert!(e.contains("keyboard off"), "{e}");
        assert_eq!(vt.active.get(), 1);
        assert_eq!(vt.kd.get(), KD_TEXT);
    }

    #[test]
    fn a_switch_that_never_happens_fails_the_take_and_is_undone() {
        let (_d, path) = state();
        let mut vt = FakeVt::new(8, 1);
        vt.switch_stalls = true;
        let e = take(&vt, 8, &path, SHORT).unwrap_err();
        assert!(e.contains("did not become the active terminal"), "{e}");
        assert!(back(&vt, 1), "{:?}", vt.log);
    }

    #[test]
    fn a_host_without_vts_proceeds_and_an_ungiven_node_fails_closed() {
        let dir = tempfile::tempdir().unwrap();
        let class = dir.path().join("class/tty");
        std::fs::create_dir_all(&class).unwrap();
        let null = Path::new("/dev/null");
        assert_eq!(presence(&class, null), Presence::NoVts);

        std::fs::create_dir_all(class.join("tty0")).unwrap();
        assert_eq!(presence(&class, null), Presence::Present);
        let missing = dir.path().join("tty8");
        assert!(
            matches!(presence(&class, &missing), Presence::Unusable(why) if why.contains("not in the agent's container"))
        );
        let regular = dir.path().join("file");
        std::fs::write(&regular, b"").unwrap();
        assert!(matches!(presence(&class, &regular), Presence::Unusable(_)));

        let unreadable = dir.path().join("no-sysfs");
        assert!(
            matches!(presence(&unreadable, null), Presence::Unusable(why) if why.contains("cannot tell"))
        );
    }

    #[test]
    fn the_record_round_trips_and_rejects_garbage() {
        let s = Saved {
            prev: 2,
            kb: 1,
            kd: 0,
        };
        assert_eq!(Saved::decode(&s.encode()), Some(s));
        assert_eq!(Saved::decode("prev=0 kb=3 kd=0"), None);
        assert_eq!(Saved::decode("prev=2 kb=3"), None);
        assert_eq!(Saved::decode("junk"), None);
    }

    fn sh(script: &str) -> Command {
        let mut c = Command::new("sh");
        c.args(["-c", script]);
        c
    }

    #[test]
    fn the_holder_is_released_by_closing_its_stdin() {
        let (child, prev) = start(sh("echo ok 3; cat >/dev/null; exit 0"), SHORT * 50).unwrap();
        assert_eq!(prev, "3");
        let mut holder = Holder::from_started(child);
        assert!(holder.child.try_wait().unwrap().is_none());
        holder.release(Duration::from_secs(5)).unwrap();
    }

    #[test]
    fn a_failed_restore_is_reported() {
        let (child, _) = start(sh("echo ok 1; cat >/dev/null; exit 1"), SHORT * 50).unwrap();
        let e = Holder::from_started(child)
            .release(Duration::from_secs(5))
            .unwrap_err();
        assert!(e.contains("failed restore"), "{e}");
    }

    #[test]
    fn a_helper_refusal_or_silence_fails_the_take() {
        let e = start(sh("echo 'err tty8 is busy'; exit 1"), SHORT * 50).unwrap_err();
        assert_eq!(e, "tty8 is busy");
        let e = start(sh("exit 1"), SHORT * 50).unwrap_err();
        assert!(e.contains("without answering"), "{e}");
        let e = start(sh("sleep 5"), SHORT).unwrap_err();
        assert!(e.contains("did not answer"), "{e}");
    }

    #[test]
    fn a_helper_that_exits_mid_session_is_lost() {
        let (child, _) = start(sh("echo ok 1; exit 0"), SHORT * 50).unwrap();
        let mut vt = ConsoleVt {
            holder: Some(Holder::from_started(child)),
            _lock: CONSOLE_VT_LOCK
                .get_or_init(|| Mutex::new(()))
                .lock()
                .unwrap_or_else(|p| p.into_inner()),
        };
        let deadline = Instant::now() + Duration::from_secs(5);
        while vt.lost().is_none() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(vt.lost().unwrap().contains("exited"));
    }
}
