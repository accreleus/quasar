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
//!   the reaper scan is unreadable;
//! - a caller-supplied window list and gamescope's published focus,
//!   classified here; the core makes no X11 call. "Game window on screen" is
//!   that focus (`GAMESCOPE_FOCUSED_APP` == the appid, amendment 1 from the
//!   #723 spike), not a mapped `STEAM_GAME` window, which under `-silent` is
//!   black until the baselayer is pinned. `registry.vdf` `RunningAppID` is
//!   never read (it stays 0 on Linux).

use std::io::{Read, Seek, SeekFrom};
use std::os::unix::fs::MetadataExt;
use std::path::PathBuf;
use std::time::{Duration, Instant};

// Bounds on input the game (same user, same container) controls.
const MAX_CMDLINE_BYTES: u64 = 4 * 1024;
const MAX_LOG_BYTES_PER_PASS: u64 = 1024 * 1024;
const MAX_LOG_LINE_BYTES: usize = 64 * 1024;

/// A `waiting for user response` means Steam needs the user only when no next task follows
/// within this. Measured by the #723 spike: routine waits continued within 0.11 s (70/70).
const USER_WAIT: Duration = Duration::from_secs(2);
/// Waits that never need the user: the first two always continued at once (#723), and the
/// shader cache clears itself (up to 17.4 s measured).
const ROUTINE_WAITS: [&str; 3] = ["CreatingProcess", "ShowInterstitials", "ProcessingShaderCache"];

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
    GameWindow { on_screen: bool },
    SteamNeedsUser(NeedsUser),
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
    log_offset: u64,
    log_file_id: Option<u64>,
    log_seen: bool,
    log_pending: Vec<u8>,
    log_game: Option<bool>,
    /// A wait for the user read from the log, and when it was read.
    log_waiting: Option<(NeedsUser, Instant)>,
    log_discarding: bool,
    running_via_reaper: bool,
    last_game_window: bool,
    active_needs: Option<NeedsUser>,
}

impl Watcher {
    pub fn new(cfg: WatchConfig, windows: Box<dyn Windows + Send>) -> Self {
        Self {
            cfg,
            windows,
            first_poll: true,
            state: LaunchState::Starting,
            log_offset: 0,
            log_file_id: None,
            log_seen: false,
            log_pending: Vec::new(),
            log_game: None,
            log_waiting: None,
            log_discarding: false,
            running_via_reaper: false,
            last_game_window: false,
            active_needs: None,
        }
    }

    /// One observation pass: /proc, the new tail of console_log.txt, the window list.
    /// Returns only CHANGES since the last call, in order. Never panics on odd input;
    /// an unreadable source is skipped for that pass.
    pub fn poll(&mut self) -> Vec<Report> {
        let mut reports = Vec::new();

        if self.first_poll {
            self.first_poll = false;
            reports.push(Report::LaunchState(LaunchState::Starting));
        }

        let scan = scan_proc(&self.cfg.proc_root, self.cfg.appid);
        if scan.reaper_alive {
            self.running_via_reaper = true;
        }
        self.read_log_tail();

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

        match self.windows.mapped() {
            Ok(windows) => {
                // The higher-ranked of the window and log reasons wins.
                let desired = [
                    classify_windows(&windows, self.cfg.appid),
                    self.held_log_needs(),
                ]
                .into_iter()
                .flatten()
                .min_by_key(|reason| needs_rank(*reason));
                self.update_needs(desired, &mut reports);
            }
            Err(_) => {
                // The mapped list is skipped for this pass: no window-derived
                // reason changes, but a log-derived wait-for-user can still
                // be reported.
                if let Some(reason) = self.held_log_needs() {
                    self.update_needs(Some(reason), &mut reports);
                }
            }
        }

        reports
    }

    fn held_log_needs(&self) -> Option<NeedsUser> {
        self.log_waiting
            .filter(|(_, since)| since.elapsed() >= USER_WAIT)
            .map(|(reason, _)| reason)
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

    fn read_log_tail(&mut self) {
        let path = self.cfg.steam_log_dir.join("console_log.txt");
        let previous = self.cfg.steam_log_dir.join("console_log.txt.previous");
        let metadata = std::fs::metadata(&path);

        // Decided on the first poll, not the first sighting: the home
        // persists between sessions and the helper polls before Steam
        // starts, so a file already present at the first poll predates this
        // session and starts at its end; a file created later is this
        // session's and is read from 0 (the inode check below sees
        // None != Some(new inode) and restarts it). Only a new file
        // (rotation) or a shorter one (truncation) restarts at 0 after that.
        // Steam rotates by size to `console_log.txt.previous` (#723): the
        // rest of the file being read is finished there first, keeping what
        // was learnt from it.
        if !self.log_seen {
            self.log_seen = true;
            if let Ok(seen) = &metadata {
                self.log_file_id = Some(seen.ino());
                self.log_offset = seen.len();
            }
            return;
        }
        let Ok(metadata) = metadata else {
            return;
        };
        let file_id = metadata.ino();
        if self.log_file_id != Some(file_id) {
            match std::fs::metadata(&previous) {
                Ok(old) if self.log_file_id == Some(old.ino()) => {
                    if old.len() > self.log_offset {
                        self.read_log_from(&previous);
                        return;
                    }
                    self.restart_log();
                }
                _ => self.reset_log(),
            }
            self.log_file_id = Some(file_id);
        }
        if metadata.len() < self.log_offset {
            self.reset_log();
        }
        if metadata.len() == self.log_offset {
            return;
        }
        self.read_log_from(&path);
    }

    /// One bounded slice of `path` from the current offset, parsed line by line.
    fn read_log_from(&mut self, path: &std::path::Path) {
        let Ok(mut file) = std::fs::File::open(path) else {
            return;
        };
        if file.seek(SeekFrom::Start(self.log_offset)).is_err() {
            return;
        }
        // A bounded slice per pass; the rest is read on the next pass.
        let mut bytes = Vec::new();
        if file
            .take(MAX_LOG_BYTES_PER_PASS)
            .read_to_end(&mut bytes)
            .is_err()
        {
            return;
        }
        self.log_offset += bytes.len() as u64;
        self.log_pending.extend_from_slice(&bytes);

        while let Some(newline) = self.log_pending.iter().position(|b| *b == b'\n') {
            let raw: Vec<u8> = self.log_pending.drain(..=newline).collect();
            if self.log_discarding {
                // The tail of a dropped over-long line ends here.
                self.log_discarding = false;
                continue;
            }
            if raw.len() <= MAX_LOG_LINE_BYTES {
                let line = String::from_utf8_lossy(&raw);
                self.parse_log_line(line.trim_end());
            }
        }
        if self.log_discarding {
            // Still inside the dropped line: drop what arrived, keep waiting
            // for its newline.
            self.log_pending.clear();
        } else if self.log_pending.len() > MAX_LOG_LINE_BYTES {
            // A partial line that grows past the cap is dropped, not buffered
            // forever on input the game controls, and its tail is ignored
            // up to and including its newline.
            self.log_pending.clear();
            self.log_discarding = true;
        }
    }

    /// A truncated or unknown file: what was learnt from the old one no longer holds.
    fn reset_log(&mut self) {
        self.restart_log();
        self.log_game = None;
        self.log_waiting = None;
    }

    fn restart_log(&mut self) {
        self.log_offset = 0;
        self.log_pending.clear();
        self.log_discarding = false;
    }

    fn parse_log_line(&mut self, line: &str) {
        // Exact spacing varies by client build (`Game process added : AppID`
        // beside `Game process removed: AppID` in the research notes), so the
        // phrases are matched without their colon and the id is taken from
        // the `AppID` that follows.
        // Install scripts (Proton's iscriptevaluator and friends) run under
        // their own Install=1 reaper before the game and are not the game:
        // their added/removed lines must neither start nor end it.
        let is_install_script =
            line.contains("Install=1") || line.to_lowercase().contains("iscriptevaluator");
        if line.contains("Game process added") {
            if !is_install_script && extract_appid(line) == Some(self.cfg.appid) {
                self.log_game = Some(true);
            }
        } else if line.contains("Game process removed") {
            if !is_install_script && extract_appid(line) == Some(self.cfg.appid) {
                self.log_game = Some(false);
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
                // Reported only if no next task follows within USER_WAIT
                // (held_log_needs); a routine wait is never armed.
                self.log_waiting = (!ROUTINE_WAITS.contains(&task)).then(|| {
                    let reason = if task == "SynchronizingCloud" {
                        NeedsUser::CloudConflict
                    } else {
                        NeedsUser::Other
                    };
                    (reason, Instant::now())
                });
            } else if line.contains("LaunchApp continues with user response")
                || line.contains("LaunchApp changed task to")
            {
                self.log_waiting = None;
            }
        }
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

/// The `STEAM_GAME` of Steam's own client windows (measured by the #723 spike).
const STEAM_CLIENT_APPID: u32 = 769;

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
