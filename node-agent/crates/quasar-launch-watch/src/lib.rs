//! Steam launch detection core for the in-container launch helper (#730).
//!
//! GStreamer-free and CUDA-free by construction, like `quasar-runtime`: the
//! helper that links this crate runs inside an app container next to untrusted
//! game code, so the surface here stays small and every read is fallible.
//! Nothing is unwrapped, expected or indexed on data read from `/proc` or the
//! log: a pid can vanish between two reads, and an unreadable source simply
//! means "skip this pass".
//!
//! Three sources, in precedence order:
//! - a `reaper SteamLaunch AppId=<id> -- …` process under a `/proc` root —
//!   the primary game signal. The reaper's own existence is the rule: it is a
//!   subreaper that outlives everything Steam launched through it (also
//!   through Proton and launcher shims), so no ancestry walk is needed;
//! - `console_log.txt` in Steam's log directory, read incrementally from the
//!   end of whatever a previous session left behind — the second source,
//!   which may move the state earlier than the reaper and may end it when
//!   the reaper scan is unreadable; `steamui_login.txt` beside it says when
//!   Steam waits for a sign-in;
//! - a caller-supplied window list and gamescope's published focus,
//!   classified here; the core makes no X11 call. "Game window on screen" is
//!   that focus (`GAMESCOPE_FOCUSED_APP` == the appid, amendment 1 from the
//!   #723 spike), not a mapped `STEAM_GAME` window, which under `-silent` is
//!   black until the baselayer is pinned. `registry.vdf` `RunningAppID` is
//!   never read (it stays 0 on Linux).

use std::io::{Read, Seek, SeekFrom};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

// Bounds on input the game (same user, same container) controls.
const MAX_CMDLINE_BYTES: u64 = 4 * 1024;
const MAX_LOG_BYTES_PER_PASS: u64 = 1024 * 1024;
const MAX_LOG_LINE_BYTES: usize = 64 * 1024;

/// A `waiting for user response` means Steam needs the user only when no next task follows
/// within this. Measured by the #723 spike: routine waits continued within 0.11 s (70/70).
const USER_WAIT: Duration = Duration::from_secs(2);
/// Waits that never need the user. `CreatingProcess` always continued at once (#723); the shader
/// cache clears itself, 0.16 to 17.4 s measured (#723), so Steam's shader dialog is not shown for
/// it (the reveal cap still ends a long run). `ShowInterstitials`, Steam's controller notice,
/// stands until answered (#755), so it is not listed: USER_WAIT tells it from a quick one.
const ROUTINE_WAITS: [&str; 2] = ["CreatingProcess", "ProcessingShaderCache"];

/// Mirrors agent-api `session_metrics.app_launch_state`; serialises to the same strings.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum LaunchState {
    Starting,
    ClientOnly,
    GameRunning,
    GameExited,
}

/// Why Steam is shown to the user on purpose.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum NeedsUser {
    SignIn,
    SteamGuard,
    CloudConflict,
    Eula,
    Install,
    Other,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Report {
    LaunchState(LaunchState),
    GameWindow {
        on_screen: bool,
    },
    SteamNeedsUser(NeedsUser),
    /// The target's shader progress from `Still replaying <appid> (NN%, a/b).` (#759).
    ShaderProgress(u8),
}

/// One mapped top-level window on gamescope's X display. The helper fills it from X11;
/// tests fill it by hand. The core does the classifying.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WindowInfo {
    pub steam_game: Option<u32>,
    pub wm_class: String,
    pub title: String,
}

pub trait Windows {
    fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>>;
    /// gamescope's root `GAMESCOPE_FOCUSED_APP` (CARDINAL); Ok(None) when unset.
    fn focused_app(&mut self) -> std::io::Result<Option<u32>>;
}

pub struct WatchConfig {
    pub proc_root: PathBuf,     // "/proc" in production, a fixture tree in tests
    pub steam_log_dir: PathBuf, // contains console_log.txt
    pub appid: u32,
}

/// `-applaunch <id>` out of `STEAM_STARTUP_FLAGS`; None for anything else (plain Steam app).
pub fn appid_from_flags(flags: &str) -> Option<u32> {
    // The agent's rule (`node-agent/src/session/launch_helper.rs` `appid_from_env`): one
    // `-applaunch`, then a bare positive integer with no sign or leading zero. Anything
    // else is not a launch: refuse rather than guess.
    let mut tokens = flags.split_ascii_whitespace();
    let mut appid = None;
    while let Some(token) = tokens.next() {
        if token == "-applaunch" {
            let value = tokens.next()?;
            if appid.is_some()
                || value.starts_with('0')
                || !value.bytes().all(|b| b.is_ascii_digit())
            {
                return None;
            }
            appid = Some(value.parse().ok()?);
        }
    }
    appid
}

pub struct Watcher {
    cfg: WatchConfig,
    windows: Box<dyn Windows + Send>,
    first_poll: bool,
    state: LaunchState,
    console: LogTail,
    log_game: Option<bool>,
    /// A wait for the user read from the log, and when it was read.
    log_waiting: Option<(NeedsUser, Instant)>,
    login: LoginLog,
    running_via_reaper: bool,
    last_game_window: bool,
    active_needs: Option<NeedsUser>,
    shader: LogTail,
    last_shader: Option<u8>,
    /// When Steam started waiting on its controller notice (`ShowInterstitials`).
    interstitial_since: Option<Instant>,
    /// `QUASAR_STEAM_DIALOGS=dismiss_safe`: the caller answers the notice, so it is reported
    /// only if it still stands [`USER_WAIT`] after the caller's key press.
    answers_notice: bool,
    notice_pressed: Option<Instant>,
    /// The last window list read, for the helper's pin.
    last_windows: Vec<WindowInfo>,
}

impl Watcher {
    pub fn new(cfg: WatchConfig, windows: Box<dyn Windows + Send>) -> Self {
        let console = LogTail::new(
            cfg.steam_log_dir.join("console_log.txt"),
            Some(cfg.steam_log_dir.join("console_log.previous.txt")),
        );
        let shader = LogTail::new(cfg_shader_log(&cfg.steam_log_dir), None);
        let login = LoginLog {
            tail: LogTail::new(cfg.steam_log_dir.join("steamui_login.txt"), None),
            needs: None,
        };
        Self {
            cfg,
            windows,
            first_poll: true,
            state: LaunchState::Starting,
            console,
            log_game: None,
            log_waiting: None,
            login,
            running_via_reaper: false,
            last_game_window: false,
            active_needs: None,
            shader,
            last_shader: None,
            interstitial_since: None,
            answers_notice: false,
            notice_pressed: None,
            last_windows: Vec::new(),
        }
    }

    /// The appid this watcher follows.
    pub fn appid(&self) -> u32 {
        self.cfg.appid
    }

    /// A viewable window with `STEAM_GAME` = `appid` was in the last pass's list.
    pub fn has_window(&self, appid: u32) -> bool {
        self.last_windows
            .iter()
            .any(|window| window.steam_game == Some(appid))
    }

    /// Steam has waited on its controller notice for the target since at least [`USER_WAIT`]
    /// before `now`: the one wait `QUASAR_STEAM_DIALOGS=dismiss_safe` answers (#759).
    pub fn interstitial_standing(&self, now: Instant) -> bool {
        self.interstitial_since
            .is_some_and(|since| now.saturating_duration_since(since) >= USER_WAIT)
    }

    /// One observation pass: /proc, the new tail of console_log.txt, the window list.
    /// Returns only CHANGES since the last call, in order. Never panics on odd input;
    /// an unreadable source is skipped for that pass.
    pub fn poll(&mut self) -> Vec<Report> {
        self.poll_at(Instant::now())
    }

    /// `poll` with the clock passed in, so a test can step past [`USER_WAIT`] without sleeping.
    pub fn poll_at(&mut self, now: Instant) -> Vec<Report> {
        let mut reports = Vec::new();

        if self.first_poll {
            self.first_poll = false;
            reports.push(Report::LaunchState(LaunchState::Starting));
        }

        let scan = scan_proc(&self.cfg.proc_root, self.cfg.appid);
        if scan.reaper_alive {
            self.running_via_reaper = true;
        }
        self.read_console_log(now);
        self.read_login_state();

        let next = self.next_launch_state(&scan);
        if next != self.state {
            self.state = next;
            reports.push(Report::LaunchState(next));
        }

        // On screen is gamescope's published focus alone; an error skips
        // the check for this pass, leaving the last reported value standing.
        if let Ok(focused) = self.windows.focused_app() {
            let on_screen = focused == Some(self.cfg.appid);
            if on_screen != self.last_game_window {
                self.last_game_window = on_screen;
                reports.push(Report::GameWindow { on_screen });
            }
        }

        if let Some(percent) = self.read_shader_log() {
            reports.push(Report::ShaderProgress(percent));
        }

        match self.windows.mapped() {
            Ok(windows) => {
                // The higher-ranked of the window and log reasons wins.
                let desired = [
                    classify_windows(&windows, self.cfg.appid),
                    self.held_log_needs(now),
                ]
                .into_iter()
                .flatten()
                .min_by_key(|reason| needs_rank(*reason));
                self.update_needs(desired, &mut reports);
                self.last_windows = windows;
            }
            Err(_) => {
                // The mapped list is skipped for this pass: no window-derived
                // reason changes, but a log-derived wait-for-user can still
                // be reported.
                if let Some(reason) = self.held_log_needs(now) {
                    self.update_needs(Some(reason), &mut reports);
                }
            }
        }

        reports
    }

    /// The target's latest shader percentage, when it changed this pass. Lines for other
    /// apps (background processing) are ignored.
    /// The figure only rises: a later line that reads lower (Steam's next stage, a recount)
    /// is not reported.
    fn read_shader_log(&mut self) -> Option<u8> {
        let mut rose = false;
        for line in self.shader.read().0 {
            if let Some(percent) = shader_percent(&line, self.cfg.appid) {
                if self.last_shader.is_none_or(|last| percent > last) {
                    self.last_shader = Some(percent);
                    rose = true;
                }
            }
        }
        if rose {
            self.last_shader
        } else {
            None
        }
    }

    /// The caller answers Steam's controller notice itself (`dismiss_safe`, #759).
    pub fn answer_notice(&mut self) {
        self.answers_notice = true;
    }

    /// The caller pressed its answer to the notice at `now`.
    pub fn notice_pressed(&mut self, now: Instant) {
        self.notice_pressed.get_or_insert(now);
    }

    /// The notice stands, and an answering caller has not yet given its press time to work.
    fn notice_held(&self, now: Instant) -> bool {
        self.answers_notice
            && self.interstitial_since.is_some()
            && self
                .notice_pressed
                .is_none_or(|at| now.saturating_duration_since(at) < USER_WAIT)
    }

    fn held_log_needs(&self, now: Instant) -> Option<NeedsUser> {
        let wait = self
            .log_waiting
            .filter(|(_, since)| now.saturating_duration_since(*since) >= USER_WAIT)
            .filter(|_| !self.notice_held(now))
            .map(|(reason, _)| reason);
        [wait, self.login.needs]
            .into_iter()
            .flatten()
            .min_by_key(|reason| needs_rank(*reason))
    }

    /// Steam waiting for credentials is its sign-in screen, which under Big Picture has no
    /// window of its own; any later login state clears it. #723: 75 of 76 signed-out starts
    /// logged `SetLoginState: WaitingForCredentials`, none of 38 signed-in ones. Tailed like
    /// `console_log.txt`, but a rotated tail is not chased.
    fn read_login_state(&mut self) {
        for line in self.login.tail.read().0 {
            if let Some(at) = line.find("SetLoginState: ") {
                self.login.needs = line[at + "SetLoginState: ".len()..]
                    .starts_with("WaitingForCredentials")
                    .then_some(NeedsUser::SignIn);
            }
        }
    }

    fn update_needs(&mut self, desired: Option<NeedsUser>, reports: &mut Vec<Report>) {
        if desired != self.active_needs {
            if let Some(reason) = desired {
                reports.push(Report::SteamNeedsUser(reason));
            }
            self.active_needs = desired;
        }
    }

    fn next_launch_state(&self, scan: &ProcScan) -> LaunchState {
        match self.state {
            // Terminal: a session that has seen its game exit does not revive.
            LaunchState::GameExited => LaunchState::GameExited,
            LaunchState::GameRunning => {
                if scan.reaper_alive {
                    LaunchState::GameRunning
                } else if self.running_via_reaper {
                    // The reaper was seen, so its loss is the exit — a stale
                    // "added" line does not keep the game alive. When the tree
                    // is unreadable, the log's removal line may end it instead.
                    if scan.readable || self.log_game == Some(false) {
                        LaunchState::GameExited
                    } else {
                        LaunchState::GameRunning
                    }
                } else if self.log_game == Some(false) {
                    // Running on the log's word alone: only its removal line
                    // ends it, so a readable-but-reaperless tree cannot flap
                    // a game the log says is still up.
                    LaunchState::GameExited
                } else {
                    LaunchState::GameRunning
                }
            }
            LaunchState::Starting | LaunchState::ClientOnly => {
                if scan.reaper_alive || self.log_game == Some(true) {
                    // The log alone may move this earlier than the tree.
                    LaunchState::GameRunning
                } else if scan.steam_client {
                    LaunchState::ClientOnly
                } else {
                    self.state
                }
            }
        }
    }

    fn read_console_log(&mut self, now: Instant) {
        let (lines, forgot) = self.console.read();
        if forgot {
            self.log_game = None;
            self.log_waiting = None;
            self.interstitial_since = None;
        }
        for line in lines {
            self.parse_log_line(line.trim_end(), now);
        }
    }

    fn parse_log_line(&mut self, line: &str, now: Instant) {
        // Exact spacing varies by client build (`Game process added : AppID`
        // beside `Game process removed: AppID` in the research notes), so the
        // phrases are matched without their colon and the id is taken from
        // the `AppID` that follows.
        // Install scripts (Proton's iscriptevaluator and friends) run under
        // their own Install=1 reaper before the game and are not the game:
        // their added/removed lines must neither start nor end it.
        let is_install_script =
            line.contains("Install=1") || line.to_lowercase().contains("iscriptevaluator");
        // The game starting or going is the step after any wait for the user: it ends it.
        if line.contains("Game process added") {
            if !is_install_script && extract_appid(line) == Some(self.cfg.appid) {
                self.log_game = Some(true);
                self.log_waiting = None;
                self.interstitial_since = None;
            }
        } else if line.contains("Game process removed") {
            if !is_install_script && extract_appid(line) == Some(self.cfg.appid) {
                self.log_game = Some(false);
                self.log_waiting = None;
                self.interstitial_since = None;
            }
        } else if line.contains("GameAction") {
            if extract_appid(line) != Some(self.cfg.appid) {
                return;
            }
            if let Some(task_at) = line.find("LaunchApp waiting for user response to") {
                // The one log line that means Steam is actually stopped for
                // the user. A bare `changed task to SynchronizingCloud` is a
                // routine step of every cloud-save launch and means nothing.
                let task = line[task_at + "LaunchApp waiting for user response to".len()..]
                    .split_whitespace()
                    .next()
                    .unwrap_or("");
                if task == "ShowInterstitials" {
                    self.interstitial_since.get_or_insert(now);
                } else {
                    self.interstitial_since = None;
                }
                // Reported only if no next task follows within USER_WAIT
                // (held_log_needs); a routine wait is never armed.
                self.log_waiting = (!ROUTINE_WAITS.contains(&task)).then(|| {
                    let reason = if task == "SynchronizingCloud" {
                        NeedsUser::CloudConflict
                    } else {
                        NeedsUser::Other
                    };
                    (reason, now)
                });
            } else if line.contains("LaunchApp continues with user response")
                || line.contains("LaunchApp changed task to")
            {
                self.log_waiting = None;
                self.interstitial_since = None;
            }
        }
    }
}

/// Up to [`MAX_LOG_BYTES_PER_PASS`] of `path` from `offset`, or `None` when it cannot be read
/// or is not a regular file. Opened non-blocking: a fifo the game puts there must never
/// stall a poll.
fn read_slice(path: &Path, offset: u64) -> Option<Vec<u8>> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
        .ok()?;
    if !file.metadata().ok()?.is_file() {
        return None;
    }
    file.seek(SeekFrom::Start(offset)).ok()?;
    let mut bytes = Vec::new();
    file.take(MAX_LOG_BYTES_PER_PASS)
        .read_to_end(&mut bytes)
        .ok()?;
    Some(bytes)
}

/// `steamui_login.txt` and the sign-in its last state means.
struct LoginLog {
    tail: LogTail,
    needs: Option<NeedsUser>,
}

/// An incremental reader of one Steam log: whole lines out (line ending stripped, nothing
/// else), the rest kept for the next pass. Lines over 64 KiB are dropped for every log it
/// reads. A rewrite in place of bytes already consumed (same inode, not shorter) is not
/// detected.
struct LogTail {
    path: PathBuf,
    /// Where Steam rotates this log to; its unread rest is finished before the new file.
    previous: Option<PathBuf>,
    seen: bool,
    offset: u64,
    file_id: Option<u64>,
    pending: Vec<u8>,
    discarding: bool,
}

impl LogTail {
    fn new(path: PathBuf, previous: Option<PathBuf>) -> Self {
        Self {
            path,
            previous,
            seen: false,
            offset: 0,
            file_id: None,
            pending: Vec::new(),
            discarding: false,
        }
    }

    /// The lines that arrived since the last call, and whether the tail followed a file it
    /// cannot connect to the one before (truncated or unknown): whatever the caller learnt
    /// from the old one no longer holds. `forgot` is decided before any line of the pass.
    fn read(&mut self) -> (Vec<String>, bool) {
        let mut lines = Vec::new();
        let mut forgot = false;
        self.read_pass(&mut lines, &mut forgot);
        (lines, forgot)
    }

    fn read_pass(&mut self, lines: &mut Vec<String>, forgot: &mut bool) {
        let metadata = std::fs::metadata(&self.path);

        // Decided on the first poll, not the first sighting: the home
        // persists between sessions and the helper polls before Steam
        // starts, so a file already present at the first poll predates this
        // session and starts at its end; a file created later is this
        // session's and is read from 0 (the inode check below sees
        // None != Some(new inode) and restarts it). Only a new file
        // (rotation) or a shorter one (truncation) restarts at 0 after that.
        // Steam rotates by size to `console_log.previous.txt` (every listing
        // of the logs dir in the #759 S0 spike; no `.txt.previous`): the rest
        // of the file being read is finished there first, keeping what was
        // learnt from it.
        if !self.seen {
            self.seen = true;
            if let Ok(seen) = &metadata {
                self.file_id = Some(seen.ino());
                self.offset = seen.len();
            }
            return;
        }
        let Ok(metadata) = metadata else {
            return;
        };
        let file_id = metadata.ino();
        if self.file_id != Some(file_id) {
            let old = self
                .previous
                .as_ref()
                .and_then(|previous| std::fs::metadata(previous).ok());
            match old {
                Some(old) if self.file_id == Some(old.ino()) => {
                    let unread = old.len() > self.offset;
                    if unread && self.read_from(true, lines) {
                        return;
                    }
                    // A tail that cannot be read is abandoned, with what it might have
                    // changed, so the new file is never held back behind it.
                    if unread {
                        self.reset(forgot);
                    } else {
                        self.restart();
                    }
                }
                _ => self.reset(forgot),
            }
            self.file_id = Some(file_id);
        }
        if metadata.len() < self.offset {
            self.reset(forgot);
        }
        if metadata.len() == self.offset {
            return;
        }
        self.read_from(false, lines);
    }

    /// One bounded slice of the log (or of `previous`) from the current offset, split into
    /// lines; the rest is read on the next pass. `false` when nothing could be read.
    fn read_from(&mut self, previous: bool, lines: &mut Vec<String>) -> bool {
        let path = match (&self.previous, previous) {
            (Some(previous), true) => previous,
            _ => &self.path,
        };
        let Some(bytes) = read_slice(path, self.offset).filter(|b| !b.is_empty()) else {
            return false;
        };
        self.offset += bytes.len() as u64;
        self.pending.extend_from_slice(&bytes);

        while let Some(newline) = self.pending.iter().position(|b| *b == b'\n') {
            let raw: Vec<u8> = self.pending.drain(..=newline).collect();
            if self.discarding {
                // The tail of a dropped over-long line ends here.
                self.discarding = false;
                continue;
            }
            if raw.len() <= MAX_LOG_LINE_BYTES {
                let line = String::from_utf8_lossy(&raw);
                let line = line.strip_suffix('\n').unwrap_or(&line);
                lines.push(line.strip_suffix('\r').unwrap_or(line).to_owned());
            }
        }
        if self.discarding {
            // Still inside the dropped line: drop what arrived, keep waiting
            // for its newline.
            self.pending.clear();
        } else if self.pending.len() > MAX_LOG_LINE_BYTES {
            // A partial line that grows past the cap is dropped, not buffered
            // forever on input the game controls, and its tail is ignored
            // up to and including its newline.
            self.pending.clear();
            self.discarding = true;
        }
        true
    }

    fn reset(&mut self, forgot: &mut bool) {
        self.restart();
        *forgot = true;
    }

    fn restart(&mut self) {
        self.offset = 0;
        self.pending.clear();
        self.discarding = false;
    }
}

fn extract_appid(line: &str) -> Option<u32> {
    let at = line.find("AppID")? + "AppID".len();
    let rest = line.get(at..)?;
    let digits: String = rest
        .chars()
        .skip_while(|c| !c.is_ascii_digit())
        .take_while(|c| c.is_ascii_digit())
        .collect();
    digits.parse::<u32>().ok()
}

// --- /proc scan -------------------------------------------------------------

struct ProcScan {
    readable: bool,
    reaper_alive: bool,
    steam_client: bool,
}

fn scan_proc(proc_root: &std::path::Path, appid: u32) -> ProcScan {
    let unreadable = ProcScan {
        readable: false,
        reaper_alive: false,
        steam_client: false,
    };
    let Ok(entries) = std::fs::read_dir(proc_root) else {
        return unreadable;
    };

    // Each cmdline is checked as it is read and nothing is kept: an argv can
    // be several MB, and the reaper's identity is in its first three args.
    let mut scan = ProcScan {
        readable: true,
        reaper_alive: false,
        steam_client: false,
    };
    for entry in entries {
        let Ok(entry) = entry else {
            return unreadable;
        };
        if entry.file_name().to_string_lossy().parse::<u32>().is_err() {
            continue;
        }
        let cmdline = match read_cmdline(&entry.path().join("cmdline")) {
            Ok(Some(cmdline)) => cmdline,
            // NotFound/ESRCH: the pid vanished mid-read, skip it alone.
            Ok(None) => continue,
            // Anything else means this pass cannot be trusted to say the
            // reaper is gone, so it must never produce GameExited.
            Err(()) => return unreadable,
        };
        let args: Vec<String> = cmdline
            .split(|b| *b == 0)
            .map(|part| String::from_utf8_lossy(part).into_owned())
            .filter(|arg| !arg.is_empty())
            .collect();
        if is_reaper_for(&args, appid) {
            scan.reaper_alive = true;
        }
        if args
            .first()
            .is_some_and(|first| basename(first).to_lowercase().starts_with("steam"))
        {
            scan.steam_client = true;
        }
    }
    scan
}

/// Ok(Some(bytes)) read, Ok(None) the pid vanished, Err anything else.
fn read_cmdline(path: &std::path::Path) -> Result<Option<Vec<u8>>, ()> {
    let vanished = |e: &std::io::Error| {
        e.kind() == std::io::ErrorKind::NotFound || e.raw_os_error() == Some(3)
    };
    let file = match std::fs::File::open(path) {
        Ok(file) => file,
        Err(e) if vanished(&e) => return Ok(None),
        Err(_) => return Err(()),
    };
    let mut bytes = Vec::new();
    match file.take(MAX_CMDLINE_BYTES).read_to_end(&mut bytes) {
        Ok(_) => Ok(Some(bytes)),
        Err(e) if vanished(&e) => Ok(None),
        Err(_) => Err(()),
    }
}

fn is_reaper_for(args: &[String], appid: u32) -> bool {
    // An Install=1 reaper runs an install script (Proton's
    // iscriptevaluator.exe) before the game; it is not the game, and its
    // exit must never read as the game's.
    if args.iter().any(|arg| arg == "Install=1")
        || args
            .iter()
            .any(|arg| basename(arg).to_lowercase() == "iscriptevaluator.exe")
    {
        return false;
    }
    let appid_arg = format!("AppId={appid}");
    args.iter().any(|arg| basename(arg) == "reaper")
        && args.iter().any(|arg| arg == "SteamLaunch")
        && args.iter().any(|arg| arg == &appid_arg)
}

fn basename(arg: &str) -> &str {
    arg.rsplit(['/', '\\']).next().unwrap_or(arg)
}

// --- window classification --------------------------------------------------

fn needs_rank(reason: NeedsUser) -> u8 {
    match reason {
        NeedsUser::SteamGuard => 0,
        NeedsUser::SignIn => 1,
        NeedsUser::CloudConflict => 2,
        NeedsUser::Eula => 3,
        NeedsUser::Install => 4,
        NeedsUser::Other => 5,
    }
}

/// The `STEAM_GAME` of Steam's own client windows, Big Picture included (#723, #759 spike).
pub const STEAM_CLIENT_APPID: u32 = 769;

fn classify_windows(windows: &[WindowInfo], appid: u32) -> Option<NeedsUser> {
    windows
        .iter()
        .filter_map(|window| classify_window(window, appid))
        .min_by_key(|reason| needs_rank(*reason))
}

/// The one place window strings are matched. Sources: the research notes for
/// #645 (old VGUI titles such as "Steam Guard - Computer Authorization
/// Required" and "Steam - Self Updater", carried in maintained window-manager
/// lists), plus the #723 spike's measurement on the current client: the
/// sign-in window under `-silent` is titled "Sign in to Steam" (WM_CLASS
/// steamwebhelper/steam, STEAM_GAME=769). The table stays deliberately small.
fn classify_window(window: &WindowInfo, appid: u32) -> Option<NeedsUser> {
    // The game's own window is never Steam asking for the user.
    if window.steam_game == Some(appid) {
        return None;
    }
    let title = window.title.to_lowercase();
    let class = window.wm_class.to_lowercase();
    // Only Steam's own windows may use the generic words; a game's "Setup" or "Log in"
    // never counts. Proton names a game's windows `steam_app_<id>`, which is not Steam.
    let steams_own = window.steam_game == Some(STEAM_CLIENT_APPID)
        || (class.contains("steam") && !class.contains("steam_app_"));
    let generic = |words: &[&str]| steams_own && words.iter().any(|w| title.contains(w));

    if title.contains("steam guard") {
        Some(NeedsUser::SteamGuard)
    } else if title.contains("cloud conflict")
        || (title.contains("steam cloud") && title.contains("conflict"))
    {
        Some(NeedsUser::CloudConflict)
    } else if title.contains("eula")
        || title.contains("end user license agreement")
        || title.contains("license agreement")
    {
        Some(NeedsUser::Eula)
    } else if title.contains("self updater")
        || title.contains("updating steam")
        || generic(&["install", "setup"])
    {
        Some(NeedsUser::Install)
    } else if title.contains("sign in to steam")
        || generic(&["sign in", "sign-in", "log in", "login"])
    {
        Some(NeedsUser::SignIn)
    } else if class.contains("steam") && (title.contains("dialog") || title.contains("prompt")) {
        // A Steam dialog we have no specific row for still needs the user.
        Some(NeedsUser::Other)
    } else {
        None
    }
}

// --- the agent's commands and what follows them (#759) ------------------------

/// One command of the agent's closed set, as it arrives on the socket (`launch <appid>`,
/// `bigpicture`, `quit <appid>`). Anything else is not a command.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Command {
    Launch(u32),
    BigPicture,
    Quit(u32),
}

/// One line, its line ending stripped. `None` for anything outside the closed set: an appid
/// is `^[1-9][0-9]{0,9}$` and must fit a `u32`.
pub fn parse_command(line: &str) -> Option<Command> {
    let appid = |id: &str| {
        let digits = id.len() <= 10
            && id.bytes().all(|b| b.is_ascii_digit())
            && id.bytes().next().is_some_and(|b| b != b'0');
        digits.then(|| id.parse().ok()).flatten()
    };
    match line.split_once(' ') {
        None if line == "bigpicture" => Some(Command::BigPicture),
        Some(("launch", id)) => appid(id).map(Command::Launch),
        Some(("quit", id)) => appid(id).map(Command::Quit),
        _ => None,
    }
}

fn cfg_shader_log(steam_log_dir: &Path) -> PathBuf {
    steam_log_dir.join("shader_log.txt")
}

/// The progress of `[stamp] Still replaying <appid> (NN%, a/b).` for `appid`: `100 * a / b`,
/// rounded down and at most 100, since Steam's own `NN` reads as a stage (it stood at 50% while
/// `a` went 0 to 3082 of ~144k, #759 S0). `NN` only for a line with no counters, `(NN%).`.
/// `None` for any other line or app.
fn shader_percent(line: &str, appid: u32) -> Option<u8> {
    let rest = after_timestamp(line)?.strip_prefix("Still replaying ")?;
    let (id, rest) = rest.split_once(' ')?;
    if id.parse::<u32>().ok()? != appid {
        return None;
    }
    let inside = rest.strip_prefix('(')?.strip_suffix(").")?;
    let number = |s: &str| {
        (!s.is_empty() && s.len() <= 12 && s.bytes().all(|b| b.is_ascii_digit()))
            .then(|| s.parse::<u64>().ok())
            .flatten()
    };
    match inside.split_once("%, ") {
        Some((_, counters)) => {
            let (done, total) = counters.split_once('/')?;
            let (done, total) = (number(done)?, number(total)?);
            (total > 0).then(|| (done.saturating_mul(100) / total).min(100) as u8)
        }
        None => number(inside.strip_suffix('%')?)
            .filter(|p| *p <= 100)
            .map(|p| p as u8),
    }
}

/// What follows Steam's `[YYYY-MM-DD HH:MM:SS] ` prefix, or `None` when the line has none.
fn after_timestamp(line: &str) -> Option<&str> {
    let (stamp, rest) = line.strip_prefix('[')?.split_once("] ")?;
    let (date, time) = stamp.split_once(' ')?;
    let all =
        |s: &str, extra: u8| !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit() || b == extra);
    (all(date, b'-') && all(time, b':')).then_some(rest)
}

/// How Steam's exit cloud sync for an app ended.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CloudEnd {
    /// `Upload complete in build list`: nothing to upload.
    NothingToUpload,
    /// `Upload complete, result <word>`: `OK`, or the failure word.
    Uploaded(String),
}

/// Steam's exit cloud sync for one app in `cloud_log.txt`, read from where the log stood
/// when this was made. The end counts only after `Starting sync (up,AC Exit,)` for that app,
/// and the read follows Steam's rotation to `cloud_log.previous.txt` mid-sync (#759 spike).
pub struct CloudSync {
    tail: LogTail,
    appid: u32,
    started: bool,
}

impl CloudSync {
    pub fn new(steam_log_dir: &Path, appid: u32) -> Self {
        let mut tail = LogTail::new(
            steam_log_dir.join("cloud_log.txt"),
            Some(steam_log_dir.join("cloud_log.previous.txt")),
        );
        // The first read only marks where the log ends now.
        tail.read();
        Self {
            tail,
            appid,
            started: false,
        }
    }

    /// The end of the sync, once it is in the log.
    pub fn poll(&mut self) -> Option<CloudEnd> {
        let start = format!("[AppID {}] Starting sync (up,AC Exit,)", self.appid);
        let end = format!("[AppID {}] Upload complete", self.appid);
        for line in self.tail.read().0 {
            let Some(rest) = after_timestamp(&line) else {
                continue;
            };
            if rest == start {
                self.started = true;
                continue;
            }
            if !self.started {
                continue;
            }
            match rest.strip_prefix(&end) {
                Some(" in build list") => return Some(CloudEnd::NothingToUpload),
                Some(tail) => {
                    if let Some(word) = tail.strip_prefix(", result ").filter(|w| {
                        !w.is_empty() && w.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
                    }) {
                        return Some(CloudEnd::Uploaded(word.to_owned()));
                    }
                }
                None => {}
            }
        }
        None
    }
}

/// Whether `appid`'s launch reaper is alive under `proc_root`; `None` when `/proc` could not
/// be read in full, which never counts as gone.
pub fn game_running(proc_root: &Path, appid: u32) -> Option<bool> {
    let scan = scan_proc(proc_root, appid);
    scan.readable.then_some(scan.reaper_alive)
}

/// The pids of `appid`'s launch reaper and every process under it, at most 4096. Parents
/// come from `/proc/<pid>/stat`; a pid that vanishes mid-read is skipped.
pub fn game_tree(proc_root: &Path, appid: u32) -> Vec<u32> {
    const MAX_TREE: usize = 4096;
    let Ok(entries) = std::fs::read_dir(proc_root) else {
        return Vec::new();
    };
    let mut parent_of = Vec::new();
    let mut tree = Vec::new();
    for entry in entries.flatten() {
        let Ok(pid) = entry.file_name().to_string_lossy().parse::<u32>() else {
            continue;
        };
        if let Ok(Some(cmdline)) = read_cmdline(&entry.path().join("cmdline")) {
            let args: Vec<String> = cmdline
                .split(|b| *b == 0)
                .map(|part| String::from_utf8_lossy(part).into_owned())
                .filter(|arg| !arg.is_empty())
                .collect();
            if is_reaper_for(&args, appid) {
                tree.push(pid);
            }
        }
        if let Some(ppid) = read_ppid(&entry.path().join("stat")) {
            parent_of.push((pid, ppid));
        }
    }
    let mut next = 0;
    while next < tree.len() && tree.len() < MAX_TREE {
        let parent = tree[next];
        for (pid, _) in parent_of.iter().filter(|(_, ppid)| *ppid == parent) {
            if !tree.contains(pid) && tree.len() < MAX_TREE {
                tree.push(*pid);
            }
        }
        next += 1;
    }
    tree
}

/// The fourth field of `/proc/<pid>/stat`, read after the last `)`: the command name in
/// between is the process's own text and may hold spaces and parentheses.
fn read_ppid(path: &Path) -> Option<u32> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)
        .ok()?
        .take(MAX_CMDLINE_BYTES)
        .read_to_end(&mut bytes)
        .ok()?;
    let text = String::from_utf8_lossy(&bytes);
    let (_, rest) = text.rsplit_once(')')?;
    rest.split_ascii_whitespace().nth(1)?.parse().ok()
}

/// Quitting a game the way a player would (#759): `WM_DELETE_WINDOW` first, then SIGTERM to
/// its process tree 20 s later, then SIGKILL 10 s after that, then Steam's exit cloud sync
/// for it, up to 60 s. Pure apart from the `/proc` and log reads: the caller does the X call
/// and the signals this hands back.
pub struct Quit {
    appid: u32,
    proc_root: PathBuf,
    cloud: CloudSync,
    step: QuitStep,
}

#[derive(Debug, Clone, Copy)]
enum QuitStep {
    /// The game was not running when the quit began: nothing to close or sync.
    NotRunning,
    Closing(Instant),
    Terminating(Instant),
    Killing(Instant),
    Syncing(Instant),
    Over,
}

/// What a [`Quit`] pass asks the caller to do.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum QuitAction {
    Terminate(Vec<u32>),
    Kill(Vec<u32>),
    /// The game is gone; whether its exit sync was seen to end, and how.
    Done(Option<CloudEnd>),
    /// The game outlived SIGKILL.
    Failed,
}

pub const QUIT_CLOSE_GRACE: Duration = Duration::from_secs(20);
pub const QUIT_TERM_GRACE: Duration = Duration::from_secs(10);
/// After SIGKILL the reaper should be gone within a pass or two.
pub const QUIT_KILL_GRACE: Duration = Duration::from_secs(3);
pub const QUIT_CLOUD_WAIT: Duration = Duration::from_secs(60);

impl Quit {
    /// Made right after the close request went out, so the cloud log is read from then.
    pub fn new(proc_root: &Path, steam_log_dir: &Path, appid: u32, now: Instant) -> Self {
        let step = match game_running(proc_root, appid) {
            Some(false) => QuitStep::NotRunning,
            _ => QuitStep::Closing(now),
        };
        Self {
            appid,
            proc_root: proc_root.to_owned(),
            cloud: CloudSync::new(steam_log_dir, appid),
            step,
        }
    }

    pub fn appid(&self) -> u32 {
        self.appid
    }

    pub fn poll(&mut self, now: Instant) -> Option<QuitAction> {
        let since = |at: Instant| now.saturating_duration_since(at);
        match self.step {
            QuitStep::NotRunning => {
                self.step = QuitStep::Over;
                Some(QuitAction::Done(None))
            }
            QuitStep::Closing(at) | QuitStep::Terminating(at) | QuitStep::Killing(at) => {
                if game_running(&self.proc_root, self.appid) == Some(false) {
                    self.step = QuitStep::Syncing(now);
                    return self.poll(now);
                }
                match self.step {
                    QuitStep::Closing(_) if since(at) >= QUIT_CLOSE_GRACE => {
                        self.step = QuitStep::Terminating(now);
                        Some(QuitAction::Terminate(game_tree(
                            &self.proc_root,
                            self.appid,
                        )))
                    }
                    QuitStep::Terminating(_) if since(at) >= QUIT_TERM_GRACE => {
                        self.step = QuitStep::Killing(now);
                        Some(QuitAction::Kill(game_tree(&self.proc_root, self.appid)))
                    }
                    QuitStep::Killing(_) if since(at) >= QUIT_KILL_GRACE => {
                        self.step = QuitStep::Over;
                        Some(QuitAction::Failed)
                    }
                    _ => None,
                }
            }
            QuitStep::Syncing(at) => {
                let end = self.cloud.poll();
                if end.is_some() || since(at) >= QUIT_CLOUD_WAIT {
                    self.step = QuitStep::Over;
                    return Some(QuitAction::Done(end));
                }
                None
            }
            QuitStep::Over => None,
        }
    }
}
