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
//! - a caller-supplied window list, classified here; the core makes no X11
//!   call. `registry.vdf` `RunningAppID` is never read (it stays 0 on Linux).

use std::io::{Read, Seek, SeekFrom};
use std::os::unix::fs::MetadataExt;
use std::path::PathBuf;

// Bounds on input the game (same user, same container) controls.
const MAX_CMDLINE_BYTES: u64 = 4 * 1024;
const MAX_LOG_BYTES_PER_PASS: u64 = 1024 * 1024;
const MAX_LOG_LINE_BYTES: usize = 64 * 1024;

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
}

pub struct WatchConfig {
    pub proc_root: PathBuf,     // "/proc" in production, a fixture tree in tests
    pub steam_log_dir: PathBuf, // contains console_log.txt
    pub appid: u32,
}

/// `-applaunch <id>` out of `STEAM_STARTUP_FLAGS`; None for anything else (plain Steam app).
pub fn appid_from_flags(flags: &str) -> Option<u32> {
    let mut tokens = flags.split_whitespace();
    while let Some(token) = tokens.next() {
        if token == "-applaunch" {
            // A trailing `-applaunch` with nothing after it, or a next token
            // that is not a plain non-zero integer, is not a launch: refuse
            // rather than guess. Digits only, because `u32::from_str` would
            // otherwise accept a leading `+`.
            return tokens.next().and_then(|next| {
                if !next.is_empty() && next.bytes().all(|b| b.is_ascii_digit()) {
                    next.parse::<u32>().ok().filter(|id| *id != 0)
                } else {
                    None
                }
            });
        }
    }
    None
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
    log_waiting_needs: Option<NeedsUser>,
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
            log_waiting_needs: None,
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

        match self.windows.mapped() {
            Ok(windows) => {
                let on_screen = windows.iter().any(|w| w.steam_game == Some(self.cfg.appid));
                if on_screen != self.last_game_window {
                    self.last_game_window = on_screen;
                    reports.push(Report::GameWindow { on_screen });
                }
                let desired = classify_windows(&windows, self.cfg.appid).or(self.log_waiting_needs);
                self.update_needs(desired, &mut reports);
            }
            Err(_) => {
                // The window source is skipped for this pass: neither the
                // on-screen flag nor a window-derived reason changes, but a
                // log-derived wait-for-user can still be reported.
                if let Some(reason) = self.log_waiting_needs {
                    self.update_needs(Some(reason), &mut reports);
                }
            }
        }

        reports
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
        let Ok(metadata) = std::fs::metadata(&path) else {
            return;
        };
        let file_id = metadata.ino();

        // The home persists between sessions and the helper starts before
        // Steam, so whatever the file already holds on first sight predates
        // this session: start at its current end. Only a new file (rotation,
        // a different inode) or a shorter one (truncation) restarts at 0.
        if !self.log_seen {
            self.log_seen = true;
            self.log_file_id = Some(file_id);
            self.log_offset = metadata.len();
            return;
        }
        if self.log_file_id != Some(file_id) {
            self.reset_log();
            self.log_file_id = Some(file_id);
        }
        if metadata.len() < self.log_offset {
            self.reset_log();
        }
        if metadata.len() == self.log_offset {
            return;
        }

        let Ok(mut file) = std::fs::File::open(&path) else {
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
            if raw.len() <= MAX_LOG_LINE_BYTES {
                let line = String::from_utf8_lossy(&raw);
                self.parse_log_line(line.trim_end());
            }
        }
        // A partial line that grows past the cap is dropped, not buffered
        // forever on input the game controls.
        if self.log_pending.len() > MAX_LOG_LINE_BYTES {
            self.log_pending.clear();
        }
    }

    fn reset_log(&mut self) {
        self.log_offset = 0;
        self.log_pending.clear();
        self.log_game = None;
        self.log_waiting_needs = None;
    }

    fn parse_log_line(&mut self, line: &str) {
        // Exact spacing varies by client build (`Game process added : AppID`
        // beside `Game process removed: AppID` in the research notes), so the
        // phrases are matched without their colon and the id is taken from
        // the `AppID` that follows.
        if line.contains("Game process added") {
            if extract_appid(line) == Some(self.cfg.appid) {
                self.log_game = Some(true);
            }
        } else if line.contains("Game process removed") {
            if extract_appid(line) == Some(self.cfg.appid) {
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
                self.log_waiting_needs = Some(if task == "SynchronizingCloud" {
                    NeedsUser::CloudConflict
                } else {
                    NeedsUser::Other
                });
            } else if line.contains("LaunchApp continues with user response")
                || line.contains("LaunchApp changed task to")
            {
                self.log_waiting_needs = None;
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

fn classify_windows(windows: &[WindowInfo], appid: u32) -> Option<NeedsUser> {
    windows
        .iter()
        .filter_map(|window| classify_window(window, appid))
        .min_by_key(|reason| needs_rank(*reason))
}

/// The one place window strings are matched. Sources: the research notes for
/// #645 (old VGUI titles such as "Steam Guard - Computer Authorization
/// Required" and "Steam - Self Updater", carried in maintained window-manager
/// lists). The current CEF
/// client's titles are not authoritatively documented anywhere, so this table
/// is deliberately small and the lab spike (#723) will correct it.
fn classify_window(window: &WindowInfo, appid: u32) -> Option<NeedsUser> {
    // The game's own window is never Steam asking for the user.
    if window.steam_game == Some(appid) {
        return None;
    }
    let title = window.title.to_lowercase();
    let class = window.wm_class.to_lowercase();

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
        || title.contains("install")
        || title.contains("setup")
    {
        Some(NeedsUser::Install)
    } else if title.contains("sign in")
        || title.contains("sign-in")
        || title.contains("log in")
        || title.contains("login")
    {
        Some(NeedsUser::SignIn)
    } else if class.contains("steam") && (title.contains("dialog") || title.contains("prompt")) {
        // A Steam dialog we have no specific row for still needs the user.
        Some(NeedsUser::Other)
    } else {
        None
    }
}
