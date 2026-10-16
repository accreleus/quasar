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
//! - the `reaper SteamLaunch AppId=<id> -- …` process tree under a `/proc`
//!   root (Steam's per-game lifecycle root, also through Proton and launcher
//!   shims) — the primary game signal;
//! - `console_log.txt` in Steam's log directory, read incrementally — the
//!   second source, which may move the state earlier than the tree and may
//!   end it when the tree is unreadable;
//! - a caller-supplied window list, classified here; the core makes no X11
//!   call. `registry.vdf` `RunningAppID` is never read (it stays 0 on Linux).

use std::collections::{HashMap, HashSet};
use std::io::{Read, Seek, SeekFrom};
use std::path::PathBuf;

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
            // A trailing `-applaunch` with nothing after it, or a non-integer
            // next token, is not a launch: refuse rather than guess.
            return tokens.next().and_then(|next| next.parse::<u32>().ok());
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
    log_pending: Vec<u8>,
    log_game: Option<bool>,
    log_cloud_hold: bool,
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
            log_pending: Vec::new(),
            log_game: None,
            log_cloud_hold: false,
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
                let desired =
                    classify_windows(&windows, self.cfg.appid).or_else(|| self.log_cloud_needs());
                self.update_needs(desired, &mut reports);
            }
            Err(_) => {
                // The window source is skipped for this pass: neither the
                // on-screen flag nor a window-derived reason changes, but a
                // log-derived cloud conflict can still be reported.
                if self.log_cloud_hold {
                    self.update_needs(Some(NeedsUser::CloudConflict), &mut reports);
                }
            }
        }

        reports
    }

    fn log_cloud_needs(&self) -> Option<NeedsUser> {
        if self.log_cloud_hold {
            Some(NeedsUser::CloudConflict)
        } else {
            None
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

    fn read_log_tail(&mut self) {
        let path = self.cfg.steam_log_dir.join("console_log.txt");
        let Ok(metadata) = std::fs::metadata(&path) else {
            return;
        };

        // A replaced file (rotation) restarts at 0, as does a truncated one;
        // lines replayed from the start rebuild the log-derived state.
        #[cfg(unix)]
        let file_id = {
            use std::os::unix::fs::MetadataExt;
            Some(metadata.ino())
        };
        #[cfg(not(unix))]
        let file_id = None;
        if self.log_file_id.is_some() && self.log_file_id != file_id {
            self.reset_log();
        }
        self.log_file_id = file_id;
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
        let mut bytes = Vec::new();
        if file.read_to_end(&mut bytes).is_err() {
            return;
        }
        self.log_offset += bytes.len() as u64;
        self.log_pending.extend_from_slice(&bytes);

        while let Some(newline) = self.log_pending.iter().position(|b| *b == b'\n') {
            let raw: Vec<u8> = self.log_pending.drain(..=newline).collect();
            let line = String::from_utf8_lossy(&raw);
            self.parse_log_line(line.trim_end());
        }
    }

    fn reset_log(&mut self) {
        self.log_offset = 0;
        self.log_pending.clear();
        self.log_game = None;
        self.log_cloud_hold = false;
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
            let Some(task_at) = line.find("LaunchApp changed task to") else {
                return;
            };
            if extract_appid(line) != Some(self.cfg.appid) {
                return;
            }
            let task = line[task_at + "LaunchApp changed task to".len()..]
                .split_whitespace()
                .next()
                .unwrap_or("");
            // SynchronizingCloud held at LaunchApp, and any CloudConflict-like
            // task, are the cheap cloud-conflict signal; any later task for
            // this app clears the hold.
            self.log_cloud_hold = task == "SynchronizingCloud" || is_cloud_conflict_task(task);
        }
    }
}

fn is_cloud_conflict_task(task: &str) -> bool {
    let lower = task.to_lowercase();
    lower.contains("cloudconflict") || (lower.contains("cloud") && lower.contains("conflict"))
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

struct ProcInfo {
    ppid: u32,
    args: Vec<String>,
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

    let mut procs: HashMap<u32, ProcInfo> = HashMap::new();
    for entry in entries {
        let Ok(entry) = entry else { continue };
        let Ok(pid) = entry.file_name().to_string_lossy().parse::<u32>() else {
            continue;
        };
        // A pid that vanishes mid-read is skipped, never fatal to the pass.
        let Ok(cmdline) = std::fs::read(entry.path().join("cmdline")) else {
            continue;
        };
        let args: Vec<String> = cmdline
            .split(|b| *b == 0)
            .map(|part| String::from_utf8_lossy(part).into_owned())
            .filter(|arg| !arg.is_empty())
            .collect();
        let ppid = read_ppid(&entry.path()).unwrap_or(0);
        procs.insert(pid, ProcInfo { ppid, args });
    }

    let reaper_pids: Vec<u32> = procs
        .iter()
        .filter(|(_, info)| is_reaper_for(&info.args, appid))
        .map(|(pid, _)| *pid)
        .collect();
    let reaper_alive = reaper_pids.iter().any(|reaper| {
        procs
            .keys()
            .any(|pid| ancestry_reaches(&procs, *pid, *reaper))
    });
    let steam_client = procs.values().any(|info| {
        info.args
            .first()
            .is_some_and(|first| basename(first).to_lowercase().starts_with("steam"))
    });

    ProcScan {
        readable: true,
        reaper_alive,
        steam_client,
    }
}

fn ancestry_reaches(procs: &HashMap<u32, ProcInfo>, mut pid: u32, reaper: u32) -> bool {
    let mut visited = HashSet::new();
    loop {
        if pid == reaper {
            return true;
        }
        if !visited.insert(pid) {
            return false;
        }
        let Some(info) = procs.get(&pid) else {
            return false;
        };
        if info.ppid == 0 {
            return false;
        }
        pid = info.ppid;
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

fn read_ppid(pid_dir: &std::path::Path) -> Option<u32> {
    if let Ok(status) = std::fs::read_to_string(pid_dir.join("status")) {
        for line in status.lines() {
            if let Some(rest) = line.strip_prefix("PPid:") {
                if let Ok(ppid) = rest.trim().parse::<u32>() {
                    return Some(ppid);
                }
            }
        }
    }
    // stat's comm is parenthesised and may itself contain spaces or parens,
    // so the fields are counted from the last ')'.
    let stat = std::fs::read_to_string(pid_dir.join("stat")).ok()?;
    let after_comm = stat.rsplit_once(')')?.1;
    let mut fields = after_comm.split_whitespace();
    let _state = fields.next()?;
    fields.next()?.parse::<u32>().ok()
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
/// lists) and the `GameAction … SynchronizingCloud` log line. The current CEF
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
