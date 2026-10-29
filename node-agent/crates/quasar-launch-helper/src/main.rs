//! The in-container Steam launch helper (#731, #759).
//!
//! The Steam image's launcher starts it as the app user when the node agent mounted it
//! (`QUASAR_LAUNCH_HELPER`). It watches the target game with `quasar-launch-watch` and writes
//! each change to `QUASAR_LAUNCH_SOCKET` as one line of the #725 wire, whose reader is
//! `node-agent/src/session/launch_helper.rs`. A derived tile's appid comes from
//! `STEAM_STARTUP_FLAGS`; with none (a parked Steam) it watches nothing until the agent's
//! first `launch`.
//!
//! The agent writes commands on the same connection, a closed set
//! ([`quasar_launch_watch::Command`]). The helper runs only fixed argv as the app user,
//! `steam -applaunch <appid>` or `steam steam://open/bigpicture`, never a shell. On the X
//! display it pins gamescope's baselayer to a game it launched, sends a quit's
//! `WM_DELETE_WINDOW`, and under `QUASAR_STEAM_DIALOGS=dismiss_safe` presses Return once on
//! Steam's controller notice. Anything else on the socket is dropped.

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::{Child, Stdio};
use std::time::{Duration, Instant};

use quasar_launch_watch::{
    appid_from_flags, parse_command, CloudEnd, CloudSync, Command, LaunchState, NeedsUser, Quit,
    QuitAction, Report, WatchConfig, Watcher, WindowInfo, Windows, STEAM_CLIENT_APPID,
};
use x11rb::connection::Connection;
use x11rb::cookie::Cookie;
use x11rb::errors::{ConnectionError, ReplyError};
use x11rb::protocol::xproto::{
    Atom, AtomEnum, ClientMessageEvent, ConnectionExt, EventMask, GetPropertyReply, MapState,
    PropMode, Window, KEY_PRESS_EVENT, KEY_RELEASE_EVENT,
};
use x11rb::protocol::xtest::ConnectionExt as _;
use x11rb::rust_connection::RustConnection;
use x11rb::wrapper::ConnectionExt as _;
use x11rb::x11_utils::TryParse;

/// A few passes a second: the reveal (#727) and the game exit (#726) wait on this, and the
/// core reads at most 1 MiB of Steam's log a pass.
const POLL: Duration = Duration::from_millis(250);
const RECONNECT: Duration = Duration::from_secs(2);
const WRITE_TIMEOUT: Duration = Duration::from_secs(2);
/// An empty line this often keeps the agent from dropping a quiet connection, so a command
/// finds the helper connected.
const KEEPALIVE: Duration = Duration::from_millis(250);
/// The longest command is `launch 4294967295`.
const MAX_COMMAND: usize = 64;
/// `steam` hands a command to the running client and exits; more than this many still
/// running means something is wrong, and further launches are refused.
const MAX_CHILDREN: usize = 8;
const MAX_WINDOWS: usize = 256;
/// Longest property read, in 32-bit units.
const MAX_PROPERTY: u32 = 256;
const STEAM: &str = "/usr/bin/steam";
/// X keysym `Return`.
const XK_RETURN: u32 = 0xff0d;

fn main() {
    let Some(socket) = std::env::var_os("QUASAR_LAUNCH_SOCKET") else {
        eprintln!("quasar-launch-helper: QUASAR_LAUNCH_SOCKET is not set; nothing to report to");
        return;
    };
    let home = PathBuf::from(std::env::var_os("HOME").unwrap_or_default());
    let mut helper = Helper::new(
        "/proc".into(),
        home.join(".local/share/Steam/logs"),
        std::env::var("QUASAR_STEAM_DIALOGS").is_ok_and(|v| v == "dismiss_safe"),
    );
    let flags = std::env::var("STEAM_STARTUP_FLAGS").unwrap_or_default();
    match appid_from_flags(&flags) {
        Some(appid) => {
            eprintln!("quasar-launch-helper: watching Steam app {appid}");
            helper.watch(appid, false);
        }
        // ponytail: a helper the launcher restarts (a crash, or a game killing it) forgets a
        // target armed by `launch` and comes back watch-only or on the flags' game; observe in
        // S7, and add a `watch <appid>` command if it matters.
        None => eprintln!("quasar-launch-helper: no game in the flags; waiting for a launch"),
    }
    if helper.dismiss {
        eprintln!("quasar-launch-helper: QUASAR_STEAM_DIALOGS=dismiss_safe: Steam's controller notice is answered");
    }
    let mut agent = Agent::new(socket.into());
    let mut ready = Some(std::io::stdout());
    loop {
        agent.connect();
        for command in agent.commands() {
            helper.command(command, &mut agent);
        }
        helper.pass(Instant::now(), &mut agent);
        agent.keepalive();
        // The core picks where to start reading Steam's log on its first pass, so the
        // launcher holds Steam back (at most 2 s) until this line.
        if let Some(mut out) = ready.take() {
            let _ = writeln!(out, "ready");
        }
        std::thread::sleep(POLL);
    }
}

/// What a game this helper launched needs pinned, so gamescope shows it under `-silent`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Pin {
    Game(u32),
    /// Steam's own window: a dialog the user must see, or the notice being answered.
    Steam,
}

struct Helper {
    proc_root: PathBuf,
    log_dir: PathBuf,
    dismiss: bool,
    watcher: Option<Watcher>,
    /// The target came from a `launch`: pin it, since a `-silent` Steam does not (#759 S0).
    launched: bool,
    pinned: Option<Pin>,
    needs_user: bool,
    /// The controller notice: pinned to Steam's window, then answered, at most once a launch.
    notice: Notice,
    quit: Option<Quit>,
    /// The watched game's cloud log, read from when the watch began, and whether this watch has
    /// reported the game's exit. Only a quit after that exit waits on it for the exit sync (#810).
    exit_sync: Option<(u32, CloudSync)>,
    game_exited: bool,
    children: Vec<Child>,
    screen: Box<dyn Screen>,
    /// `steam` from `PATH`; a test points it at a stand-in.
    steam: PathBuf,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Notice {
    Waiting,
    /// Steam's window was pinned at this instant; the press waits for its focus.
    Pinned(Instant),
    Answered,
    /// Something else was showing, or Steam's window never took focus: the user answers.
    Refused,
}

/// How long the press waits for Steam's window to take focus after its pin. S0 measured the
/// focus moving within 1.2 s of a pin.
const NOTICE_FOCUS_WAIT: Duration = Duration::from_secs(2);

impl Helper {
    fn new(proc_root: PathBuf, log_dir: PathBuf, dismiss: bool) -> Self {
        Helper {
            proc_root,
            log_dir,
            dismiss,
            watcher: None,
            launched: false,
            pinned: None,
            needs_user: false,
            notice: Notice::Waiting,
            quit: None,
            exit_sync: None,
            game_exited: false,
            children: Vec::new(),
            screen: Box::<X11Windows>::default(),
            // The image's `steam` RPM puts it here; `PATH` only for an image without it.
            steam: if Path::new(STEAM).is_file() {
                STEAM
            } else {
                "steam"
            }
            .into(),
        }
    }

    fn watch(&mut self, appid: u32, launched: bool) {
        self.watch_with(appid, launched, Box::<X11Windows>::default());
    }

    fn watch_with(&mut self, appid: u32, launched: bool, windows: Box<dyn Windows + Send>) {
        let mut watcher = Watcher::new(
            WatchConfig {
                proc_root: self.proc_root.clone(),
                steam_log_dir: self.log_dir.clone(),
                appid,
            },
            windows,
        );
        if self.dismiss {
            watcher.answer_notice();
        }
        self.watcher = Some(watcher);
        self.exit_sync = Some((appid, CloudSync::new(&self.log_dir, appid)));
        self.game_exited = false;
        self.launched = launched;
        self.needs_user = false;
        self.notice = Notice::Waiting;
    }

    fn command(&mut self, command: Command, agent: &mut Agent) {
        match command {
            Command::Launch(appid) => {
                let ok = self.run_steam(&["-applaunch", &appid.to_string()]);
                if ok {
                    eprintln!("quasar-launch-helper: launched Steam app {appid}");
                    agent.forget_game();
                    self.watch(appid, true);
                }
                agent.done(Done::Launch { appid, ok });
            }
            Command::BigPicture => {
                let ok = self.run_steam(&["steam://open/bigpicture"]);
                if ok {
                    eprintln!("quasar-launch-helper: opened Big Picture");
                    agent.forget_game();
                    // Steam pins its own window for Big Picture; nothing to pin here.
                    self.watch(STEAM_CLIENT_APPID, false);
                }
                agent.done(Done::BigPicture { ok });
            }
            Command::Quit(appid) => {
                if self.quit.is_some() {
                    eprintln!(
                        "quasar-launch-helper: a quit is already running; quit {appid} dropped"
                    );
                    return;
                }
                match self.screen.close(appid) {
                    Ok(sent) => eprintln!(
                        "quasar-launch-helper: asked {sent} window(s) of Steam app {appid} to close"
                    ),
                    Err(error) => eprintln!(
                        "quasar-launch-helper: could not ask Steam app {appid} to close ({error}); \
                         its tree is signalled after the grace"
                    ),
                }
                // Commands run before the pass: an exit in this same tick must count as seen (#872).
                self.poll_watcher(Instant::now(), agent);
                self.quit = Some(Quit::new(
                    &self.proc_root,
                    &self.log_dir,
                    appid,
                    Instant::now(),
                    self.exit_sync
                        .take_if(|(watched, _)| self.game_exited && *watched == appid)
                        .map(|(_, cloud)| cloud),
                ));
            }
        }
    }

    /// `steam` with fixed argv, as this user, detached from our stdio.
    fn run_steam(&mut self, args: &[&str]) -> bool {
        self.children
            .retain_mut(|child| matches!(child.try_wait(), Ok(None)));
        if self.children.len() >= MAX_CHILDREN {
            eprintln!("quasar-launch-helper: {MAX_CHILDREN} steam commands still running; refused");
            return false;
        }
        match std::process::Command::new(&self.steam)
            .args(args)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
        {
            Ok(child) => {
                self.children.push(child);
                true
            }
            Err(error) => {
                eprintln!("quasar-launch-helper: could not run steam: {error}");
                false
            }
        }
    }

    fn pass(&mut self, now: Instant, agent: &mut Agent) {
        self.children
            .retain_mut(|child| matches!(child.try_wait(), Ok(None)));
        self.poll_watcher(now, agent);
        self.answer_notice(now);
        self.pin();
        self.step_quit(now, agent);
    }

    /// Poll the watcher and forward its reports, noting the ones the helper acts on.
    fn poll_watcher(&mut self, now: Instant, agent: &mut Agent) {
        let Some(watcher) = &mut self.watcher else {
            return;
        };
        for report in watcher.poll_at(now) {
            if matches!(report, Report::SteamNeedsUser(_)) {
                self.needs_user = true;
            }
            if matches!(report, Report::LaunchState(LaunchState::GameExited)) {
                self.game_exited = true;
            }
            agent.send(report);
        }
    }

    /// `dismiss_safe`: answer Steam's controller notice with one Return, and only when nothing
    /// else can take the key: the notice is the one standing wait, nothing else of Steam's
    /// needs the user, Steam's window was pinned a pass before and gamescope reports it
    /// focused. Otherwise the notice is shown to the user and nothing is pressed.
    fn answer_notice(&mut self, now: Instant) {
        if !self.dismiss || matches!(self.notice, Notice::Answered | Notice::Refused) {
            return;
        }
        let Some(watcher) = self.watcher.as_mut() else {
            return;
        };
        if !watcher.interstitial_standing(now) {
            return;
        }
        let refusal = if self.needs_user || !watcher.notice_alone(now) {
            Some("Steam shows something else too")
        } else {
            match self.notice {
                Notice::Waiting => {
                    self.notice = Notice::Pinned(now);
                    None
                }
                Notice::Pinned(_) if self.pinned != Some(Pin::Steam) => {
                    Some("Steam's window could not be pinned")
                }
                // Only on facts read this pass: a failed window read waits for the next.
                Notice::Pinned(_)
                    if watcher.windows_read() && watcher.focused() == Some(STEAM_CLIENT_APPID) =>
                {
                    match self.screen.press_return() {
                        Ok(()) => eprintln!(
                            "quasar-launch-helper: answered Steam's controller notice with Return (dismiss_safe)"
                        ),
                        Err(error) => eprintln!(
                            "quasar-launch-helper: could not answer Steam's controller notice: {error}"
                        ),
                    }
                    self.notice = Notice::Answered;
                    // Still standing 2 s after the press, the notice is shown to the user.
                    watcher.notice_pressed(now);
                    None
                }
                Notice::Pinned(at) if now.saturating_duration_since(at) >= NOTICE_FOCUS_WAIT => {
                    Some("Steam's window never took focus")
                }
                _ => None,
            }
        };
        if let Some(why) = refusal {
            eprintln!(
                "quasar-launch-helper: not answering Steam's controller notice: {why}; it is shown"
            );
            watcher.reveal_notice();
            self.notice = Notice::Refused;
        }
    }

    /// The pin a launched game wants: the game once its window exists, else Steam's window
    /// while Steam needs the user or its notice is being answered.
    fn wanted_pin(&self) -> Option<Pin> {
        let watcher = self.watcher.as_ref()?;
        let answering = matches!(self.notice, Notice::Pinned(_));
        // A tile launched from its flags is pinned only once the helper has pinned it for the
        // notice; from then on the game must be pinned back too.
        if !(self.launched || answering || self.pinned.is_some()) {
            return None;
        }
        if watcher.has_window(watcher.appid()) {
            Some(Pin::Game(watcher.appid()))
        } else if answering || (self.launched && self.needs_user) {
            Some(Pin::Steam)
        } else {
            None
        }
    }

    fn pin(&mut self) {
        let Some(pin) = self.wanted_pin().filter(|pin| Some(*pin) != self.pinned) else {
            return;
        };
        let appid = match pin {
            Pin::Game(appid) => appid,
            Pin::Steam => STEAM_CLIENT_APPID,
        };
        match self.screen.pin(appid) {
            Ok(()) => {
                eprintln!("quasar-launch-helper: pinned the baselayer to {appid}");
                self.pinned = Some(pin);
            }
            Err(error) => eprintln!("quasar-launch-helper: could not pin the baselayer: {error}"),
        }
    }

    fn step_quit(&mut self, now: Instant, agent: &mut Agent) {
        let Some(quit) = &mut self.quit else {
            return;
        };
        let appid = quit.appid();
        match quit.poll(now) {
            None => {}
            Some(QuitAction::Terminate(pids)) => {
                eprintln!("quasar-launch-helper: Steam app {appid} did not close; SIGTERM to {} process(es)", pids.len());
                signal(&pids, libc::SIGTERM);
            }
            Some(QuitAction::Kill(pids)) => {
                eprintln!("quasar-launch-helper: Steam app {appid} outlived SIGTERM; SIGKILL to {} process(es)", pids.len());
                signal(&pids, libc::SIGKILL);
            }
            Some(QuitAction::Done(end)) => {
                let synced = matches!(&end, Some(CloudEnd::NothingToUpload))
                    || matches!(&end, Some(CloudEnd::Uploaded(word)) if word == "OK");
                eprintln!("quasar-launch-helper: Steam app {appid} quit; exit cloud sync {end:?}");
                agent.done(Done::Quit {
                    appid,
                    ok: true,
                    cloud_synced: synced,
                    cloud_sync_started: end.is_some(),
                });
                self.quit = None;
            }
            Some(QuitAction::Failed) => {
                eprintln!("quasar-launch-helper: Steam app {appid} outlived SIGKILL");
                agent.done(Done::Quit {
                    appid,
                    ok: false,
                    cloud_synced: false,
                    cloud_sync_started: true,
                });
                self.quit = None;
            }
        }
    }
}

// ponytail: a pid that exits between the `/proc` scan and kill(2) can be reused by another of
// this user's processes in the container, which then gets the signal; the window is one pass.
fn signal(pids: &[u32], signal: libc::c_int) {
    for &pid in pids {
        let Ok(pid) = libc::pid_t::try_from(pid) else {
            continue;
        };
        // Never 0 or 1: those signal a group or init.
        if pid > 1 {
            // SAFETY: kill(2) takes plain integers and touches no memory of ours.
            unsafe { libc::kill(pid, signal) };
        }
    }
}

/// The result of one command, for the agent.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Done {
    Launch {
        appid: u32,
        ok: bool,
    },
    BigPicture {
        ok: bool,
    },
    Quit {
        appid: u32,
        ok: bool,
        cloud_synced: bool,
        cloud_sync_started: bool,
    },
}

/// The wire, as the agent's reader (`node-agent/src/session/launch_helper.rs`) takes it.
#[derive(serde::Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum Wire {
    LaunchState {
        v: u32,
        state: LaunchState,
    },
    GameWindow {
        v: u32,
        on_screen: bool,
    },
    SteamNeedsUser {
        v: u32,
        reason: NeedsUser,
    },
    ShaderProgress {
        v: u32,
        percent: u8,
    },
    CommandDone {
        v: u32,
        command: &'static str,
        #[serde(skip_serializing_if = "Option::is_none")]
        appid: Option<u32>,
        ok: bool,
        #[serde(skip_serializing_if = "Option::is_none")]
        cloud_synced: Option<bool>,
        /// Sent only as `false` (no exit sync started): an agent that predates it rejects the
        /// line and waits out its bound, as it did before.
        #[serde(skip_serializing_if = "Option::is_none")]
        cloud_sync_started: Option<bool>,
    },
}

/// Kinds of line, each replayed on a new connection: the last four describe the game.
const KINDS: usize = 5;
const COMMAND_DONE: usize = 4;

/// One wire line for `report`, and which kind it is.
fn line(report: Report) -> (usize, String) {
    let (kind, wire) = match report {
        Report::LaunchState(state) => (0, Wire::LaunchState { v: 1, state }),
        Report::GameWindow { on_screen } => (1, Wire::GameWindow { v: 1, on_screen }),
        Report::SteamNeedsUser(reason) => (2, Wire::SteamNeedsUser { v: 1, reason }),
        Report::ShaderProgress(percent) => (3, Wire::ShaderProgress { v: 1, percent }),
    };
    (kind, to_line(&wire))
}

fn done_line(done: Done) -> String {
    let (command, appid, ok, cloud_synced, cloud_sync_started) = match done {
        Done::Launch { appid, ok } => ("launch", Some(appid), ok, None, None),
        Done::BigPicture { ok } => ("bigpicture", None, ok, None, None),
        Done::Quit {
            appid,
            ok,
            cloud_synced,
            cloud_sync_started,
        } => (
            "quit",
            Some(appid),
            ok,
            Some(cloud_synced),
            (!cloud_sync_started).then_some(false),
        ),
    };
    to_line(&Wire::CommandDone {
        v: 1,
        command,
        appid,
        ok,
        cloud_synced,
        cloud_sync_started,
    })
}

fn to_line(wire: &Wire) -> String {
    // Unit variants, integers and bools cannot fail to serialise.
    serde_json::to_string(wire).unwrap_or_default() + "\n"
}

/// The connection to the agent. The agent keeps what it was told across connections, so a
/// new one repeats the latest line of each kind and nothing is lost to a reconnect.
struct Agent {
    path: PathBuf,
    conn: Option<UnixStream>,
    next_try: Instant,
    latest: [Option<String>; KINDS],
    last_write: Instant,
    /// A command line read in part.
    partial: Vec<u8>,
    /// Inside a line already dropped as too long, up to its newline.
    skipping: bool,
}

impl Agent {
    fn new(path: PathBuf) -> Self {
        Agent {
            path,
            conn: None,
            next_try: Instant::now(),
            latest: Default::default(),
            last_write: Instant::now(),
            partial: Vec::new(),
            skipping: false,
        }
    }

    fn connect(&mut self) {
        if self.conn.is_some() || Instant::now() < self.next_try {
            return;
        }
        let conn = UnixStream::connect(&self.path).and_then(|conn| {
            conn.set_write_timeout(Some(WRITE_TIMEOUT))?;
            // Reads are polled once a pass and must not hold the pass up.
            conn.set_read_timeout(Some(Duration::from_millis(1)))?;
            Ok(conn)
        });
        let Ok(conn) = conn else {
            // Only a failed connect waits: the agent drops a quiet connection, and the
            // next line has to go out on a new one at once.
            self.next_try = Instant::now() + RECONNECT;
            return;
        };
        self.conn = Some(conn);
        self.partial.clear();
        self.skipping = false;
        for line in self.latest.clone().into_iter().flatten() {
            if !self.write(&line) {
                return;
            }
        }
    }

    /// The game lines describe the target that was: a new one starts from nothing.
    fn forget_game(&mut self) {
        for kind in &mut self.latest[..COMMAND_DONE] {
            *kind = None;
        }
    }

    fn send(&mut self, report: Report) {
        let (kind, line) = line(report);
        self.put(kind, line);
    }

    fn done(&mut self, done: Done) {
        self.put(COMMAND_DONE, done_line(done));
    }

    fn put(&mut self, kind: usize, line: String) {
        self.latest[kind] = Some(line.clone());
        if !self.write(&line) {
            self.connect();
        }
    }

    fn keepalive(&mut self) {
        if self.last_write.elapsed() >= KEEPALIVE {
            self.write("\n");
        }
    }

    fn write(&mut self, line: &str) -> bool {
        let Some(conn) = &mut self.conn else {
            return false;
        };
        let written = conn.write_all(line.as_bytes()).is_ok();
        if written {
            self.last_write = Instant::now();
        } else {
            self.conn = None;
        }
        written
    }

    /// The commands that arrived since the last pass. A line that is not one is dropped and
    /// logged by its length only: whatever wrote it, it is not ours to echo.
    fn commands(&mut self) -> Vec<Command> {
        let mut commands = Vec::new();
        let Some(conn) = &mut self.conn else {
            return commands;
        };
        let mut buf = [0u8; 256];
        let closed = loop {
            match conn.read(&mut buf) {
                Ok(0) => break true,
                Ok(n) => {
                    for &byte in &buf[..n] {
                        if byte != b'\n' {
                            if self.partial.len() < MAX_COMMAND {
                                self.partial.push(byte);
                            } else {
                                self.skipping = true;
                            }
                            continue;
                        }
                        let line = std::mem::take(&mut self.partial);
                        if std::mem::take(&mut self.skipping) {
                            eprintln!("quasar-launch-helper: dropped an over-long command line");
                            continue;
                        }
                        match std::str::from_utf8(&line).ok().and_then(parse_command) {
                            Some(command) => commands.push(command),
                            None => eprintln!(
                                "quasar-launch-helper: dropped a command line that is not one ({} bytes)",
                                line.len()
                            ),
                        }
                    }
                }
                Err(e)
                    if matches!(
                        e.kind(),
                        std::io::ErrorKind::WouldBlock
                            | std::io::ErrorKind::TimedOut
                            | std::io::ErrorKind::Interrupted
                    ) =>
                {
                    break false
                }
                Err(_) => break true,
            }
        };
        if closed {
            // The agent closed it: reconnect on this pass, not after the backoff.
            self.conn = None;
            self.next_try = Instant::now();
            self.connect();
        }
        commands
    }
}

/// [`Windows`] over gamescope's Xwayland (`DISPLAY`), reconnecting after any error.
#[derive(Default)]
struct X11Windows {
    x11: Option<X11>,
}

impl X11Windows {
    fn read<T>(&mut self, read: impl FnOnce(&X11) -> std::io::Result<T>) -> std::io::Result<T> {
        let x11 = match &self.x11 {
            Some(x11) => x11,
            None => self.x11.insert(X11::connect()?),
        };
        let result = read(x11);
        if result.is_err() {
            self.x11 = None;
        }
        result
    }
}

/// What the helper does on gamescope's display; a test stands in for it.
trait Screen {
    /// Pin gamescope's baselayer to `appid`'s windows: gamescope re-reads the root
    /// `GAMESCOPECTRL_BASELAYER_APPID` on any change and focuses the first listed appid with
    /// a window (#723 spike).
    fn pin(&mut self, appid: u32) -> std::io::Result<()>;
    /// `WM_DELETE_WINDOW` to every viewable top-level window of `appid` that lists it in
    /// `WM_PROTOCOLS`, as a player closing it would. How many were asked.
    fn close(&mut self, appid: u32) -> std::io::Result<usize>;
    /// One XTEST Return press and release, to whatever window has the input focus.
    fn press_return(&mut self) -> std::io::Result<()>;
}

impl Screen for X11Windows {
    fn pin(&mut self, appid: u32) -> std::io::Result<()> {
        self.read(|x| x.pin(appid))
    }

    fn close(&mut self, appid: u32) -> std::io::Result<usize> {
        self.read(|x| x.close(appid))
    }

    fn press_return(&mut self) -> std::io::Result<()> {
        self.read(X11::press_return)
    }
}

impl Windows for X11Windows {
    fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>> {
        self.read(X11::mapped)
    }

    fn focused_app(&mut self) -> std::io::Result<Option<u32>> {
        self.read(X11::focused_app)
    }
}

struct X11 {
    conn: RustConnection,
    root: Window,
    steam_game: Atom,
    focused_app: Atom,
    net_wm_name: Atom,
    utf8_string: Atom,
    baselayer: Atom,
    wm_protocols: Atom,
    wm_delete_window: Atom,
}

impl X11 {
    fn connect() -> std::io::Result<Self> {
        let (conn, screen) = x11rb::connect(None).map_err(std::io::Error::other)?;
        let root = conn
            .setup()
            .roots
            .get(screen)
            .ok_or_else(|| std::io::Error::other("DISPLAY names no such screen"))?
            .root;
        let atom = |name: &[u8]| -> std::io::Result<Atom> {
            Ok(reply(conn.intern_atom(false, name))?
                .ok_or_else(|| std::io::Error::other("InternAtom refused"))?
                .atom)
        };
        let (steam_game, focused_app, net_wm_name, utf8_string) = (
            atom(b"STEAM_GAME")?,
            atom(b"GAMESCOPE_FOCUSED_APP")?,
            atom(b"_NET_WM_NAME")?,
            atom(b"UTF8_STRING")?,
        );
        let (baselayer, wm_protocols, wm_delete_window) = (
            atom(b"GAMESCOPECTRL_BASELAYER_APPID")?,
            atom(b"WM_PROTOCOLS")?,
            atom(b"WM_DELETE_WINDOW")?,
        );
        Ok(X11 {
            conn,
            root,
            steam_game,
            focused_app,
            net_wm_name,
            utf8_string,
            baselayer,
            wm_protocols,
            wm_delete_window,
        })
    }

    fn pin(&self, appid: u32) -> std::io::Result<()> {
        self.conn
            .change_property32(
                PropMode::REPLACE,
                self.root,
                self.baselayer,
                AtomEnum::CARDINAL,
                &[appid],
            )
            .map_err(std::io::Error::other)?;
        self.conn.flush().map_err(std::io::Error::other)
    }

    fn close(&self, appid: u32) -> std::io::Result<usize> {
        let Some(tree) = reply(self.conn.query_tree(self.root))? else {
            return Ok(0);
        };
        let mut sent = 0;
        for &window in tree.children.iter().take(MAX_WINDOWS) {
            match reply(self.conn.get_window_attributes(window))? {
                Some(attrs) if attrs.map_state == MapState::VIEWABLE => {}
                _ => continue,
            }
            let game = self
                .property(window, self.steam_game, AtomEnum::ANY.into())?
                .and_then(|p| p.value32().and_then(|mut v| v.next()));
            if game != Some(appid) {
                continue;
            }
            let lists_delete = self
                .property(window, self.wm_protocols, AtomEnum::ATOM.into())?
                .and_then(|p| {
                    p.value32()
                        .map(|mut v| v.any(|a| a == self.wm_delete_window))
                })
                .unwrap_or(false);
            if !lists_delete {
                continue;
            }
            let event = ClientMessageEvent::new(
                32,
                window,
                self.wm_protocols,
                [self.wm_delete_window, x11rb::CURRENT_TIME, 0, 0, 0],
            );
            self.conn
                .send_event(false, window, EventMask::NO_EVENT, event)
                .map_err(std::io::Error::other)?;
            sent += 1;
        }
        self.conn.flush().map_err(std::io::Error::other)?;
        Ok(sent)
    }

    fn press_return(&self) -> std::io::Result<()> {
        let setup = self.conn.setup();
        let (first, last) = (setup.min_keycode, setup.max_keycode);
        let mapping = reply(
            self.conn
                .get_keyboard_mapping(first, last.saturating_sub(first).saturating_add(1)),
        )?
        .ok_or_else(|| std::io::Error::other("no keyboard mapping"))?;
        let per = usize::from(mapping.keysyms_per_keycode.max(1));
        let keycode = mapping
            .keysyms
            .chunks(per)
            .position(|syms| syms.contains(&XK_RETURN))
            .and_then(|at| u8::try_from(at).ok())
            .and_then(|at| first.checked_add(at))
            .ok_or_else(|| std::io::Error::other("no keycode for Return"))?;
        for kind in [KEY_PRESS_EVENT, KEY_RELEASE_EVENT] {
            self.conn
                .xtest_fake_input(kind, keycode, x11rb::CURRENT_TIME, self.root, 0, 0, 0)
                .map_err(std::io::Error::other)?;
        }
        self.conn.sync().map_err(std::io::Error::other)
    }

    /// The appid gamescope published as focused on the root window; `None` while unset.
    fn focused_app(&self) -> std::io::Result<Option<u32>> {
        Ok(self
            .property(self.root, self.focused_app, AtomEnum::CARDINAL.into())?
            .and_then(|p| p.value32().and_then(|mut v| v.next())))
    }

    /// Mapped top-level windows. A window gone mid-pass is skipped; a lost connection
    /// fails the pass, which the core skips.
    fn mapped(&self) -> std::io::Result<Vec<WindowInfo>> {
        let Some(tree) = reply(self.conn.query_tree(self.root))? else {
            return Ok(Vec::new());
        };
        let mut windows = Vec::new();
        for &window in tree.children.iter().take(MAX_WINDOWS) {
            match reply(self.conn.get_window_attributes(window))? {
                Some(attrs) if attrs.map_state == MapState::VIEWABLE => {}
                _ => continue,
            }
            let steam_game = self
                .property(window, self.steam_game, AtomEnum::ANY.into())?
                .and_then(|p| p.value32().and_then(|mut v| v.next()));
            // WM_CLASS is "instance\0class\0".
            let wm_class = self
                .text(window, AtomEnum::WM_CLASS.into(), AtomEnum::STRING.into())?
                .split('\0')
                .filter(|part| !part.is_empty())
                .collect::<Vec<_>>()
                .join(" ");
            let mut title = self.text(window, self.net_wm_name, self.utf8_string)?;
            if title.is_empty() {
                title = self.text(window, AtomEnum::WM_NAME.into(), AtomEnum::ANY.into())?;
            }
            windows.push(WindowInfo {
                steam_game,
                wm_class,
                title,
            });
        }
        Ok(windows)
    }

    fn property(
        &self,
        window: Window,
        property: Atom,
        type_: Atom,
    ) -> std::io::Result<Option<GetPropertyReply>> {
        reply(
            self.conn
                .get_property(false, window, property, type_, 0, MAX_PROPERTY),
        )
    }

    fn text(&self, window: Window, property: Atom, type_: Atom) -> std::io::Result<String> {
        Ok(self
            .property(window, property, type_)?
            .map(|p| String::from_utf8_lossy(&p.value).into_owned())
            .unwrap_or_default())
    }
}

/// `None` when the server answered with an X error (a window that went away); `Err` only
/// when the connection itself failed.
fn reply<R: TryParse>(
    request: Result<Cookie<'_, RustConnection, R>, ConnectionError>,
) -> std::io::Result<Option<R>> {
    match request.map_err(std::io::Error::other)?.reply() {
        Ok(reply) => Ok(Some(reply)),
        Err(ReplyError::X11Error(_)) => Ok(None),
        Err(ReplyError::ConnectionError(error)) => Err(std::io::Error::other(error)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader};
    use std::os::unix::net::UnixListener;
    use std::sync::{Arc, Mutex};

    /// Against a real X server on `DISPLAY` (Xvfb), not gamescope: run with `-- --ignored`.
    /// gamescope's and Steam's atoms are set here by hand, the way they publish them.
    #[test]
    #[ignore = "needs an X server on DISPLAY"]
    fn reads_the_windows_and_the_focused_app_from_x() {
        use x11rb::protocol::xproto::{CreateWindowAux, PropMode, WindowClass};
        use x11rb::wrapper::ConnectionExt as _;
        let (conn, screen) = x11rb::connect(None).unwrap();
        let root = conn.setup().roots[screen].root;
        let atom = |name: &[u8]| conn.intern_atom(false, name).unwrap().reply().unwrap().atom;
        let (steam_game, focused, net_wm_name, utf8) = (
            atom(b"STEAM_GAME"),
            atom(b"GAMESCOPE_FOCUSED_APP"),
            atom(b"_NET_WM_NAME"),
            atom(b"UTF8_STRING"),
        );
        let window = |appid: u32, class: &[u8], name: (Atom, &str), mapped: bool| {
            let w = conn.generate_id().unwrap();
            conn.create_window(
                0,
                w,
                root,
                0,
                0,
                64,
                64,
                0,
                WindowClass::INPUT_OUTPUT,
                0,
                &CreateWindowAux::new(),
            )
            .unwrap();
            conn.change_property32(
                PropMode::REPLACE,
                w,
                steam_game,
                AtomEnum::CARDINAL,
                &[appid],
            )
            .unwrap();
            conn.change_property8(
                PropMode::REPLACE,
                w,
                AtomEnum::WM_CLASS,
                AtomEnum::STRING,
                class,
            )
            .unwrap();
            let type_ = if name.0 == net_wm_name {
                utf8
            } else {
                AtomEnum::STRING.into()
            };
            conn.change_property8(PropMode::REPLACE, w, name.0, type_, name.1.as_bytes())
                .unwrap();
            if mapped {
                conn.map_window(w).unwrap();
            }
        };
        window(
            769,
            b"steamwebhelper\0steam\0",
            (net_wm_name, "Sign in to Steam"),
            true,
        );
        window(
            620,
            b"portal2_linux\0portal2_linux\0",
            (AtomEnum::WM_NAME.into(), "Portal 2"),
            true,
        );
        window(620, b"hidden\0hidden\0", (net_wm_name, "not mapped"), false);
        conn.sync().unwrap();

        let mut x = X11Windows::default();
        assert_eq!(x.focused_app().unwrap(), None, "unset");
        assert_eq!(
            x.mapped().unwrap(),
            [
                WindowInfo {
                    steam_game: Some(769),
                    wm_class: "steamwebhelper steam".into(),
                    title: "Sign in to Steam".into(),
                },
                WindowInfo {
                    steam_game: Some(620),
                    wm_class: "portal2_linux portal2_linux".into(),
                    title: "Portal 2".into(),
                },
            ]
        );
        conn.change_property32(PropMode::REPLACE, root, focused, AtomEnum::CARDINAL, &[620])
            .unwrap();
        conn.sync().unwrap();
        assert_eq!(x.focused_app().unwrap(), Some(620));

        x.pin(620).unwrap();
        let baselayer = atom(b"GAMESCOPECTRL_BASELAYER_APPID");
        let pinned = conn
            .get_property(false, root, baselayer, AtomEnum::CARDINAL, 0, 4)
            .unwrap()
            .reply()
            .unwrap();
        assert_eq!(pinned.value32().unwrap().collect::<Vec<_>>(), [620]);
        // Neither window lists WM_DELETE_WINDOW; one that does is asked.
        assert_eq!(x.close(620).unwrap(), 0);
        let (protocols, delete) = (atom(b"WM_PROTOCOLS"), atom(b"WM_DELETE_WINDOW"));
        let w = conn.generate_id().unwrap();
        conn.create_window(
            0,
            w,
            root,
            0,
            0,
            8,
            8,
            0,
            WindowClass::INPUT_OUTPUT,
            0,
            &CreateWindowAux::new(),
        )
        .unwrap();
        conn.change_property32(PropMode::REPLACE, w, steam_game, AtomEnum::CARDINAL, &[620])
            .unwrap();
        conn.change_property32(PropMode::REPLACE, w, protocols, AtomEnum::ATOM, &[delete])
            .unwrap();
        conn.map_window(w).unwrap();
        conn.sync().unwrap();
        assert_eq!(x.close(620).unwrap(), 1);
    }

    #[test]
    fn each_report_is_one_line_of_the_wire() {
        // The #725 shared interface's own examples, byte for byte.
        assert_eq!(
            line(Report::LaunchState(LaunchState::ClientOnly)),
            (
                0,
                "{\"type\":\"launch_state\",\"v\":1,\"state\":\"client_only\"}\n".into()
            )
        );
        assert_eq!(
            line(Report::GameWindow { on_screen: true }),
            (
                1,
                "{\"type\":\"game_window\",\"v\":1,\"on_screen\":true}\n".into()
            )
        );
        assert_eq!(
            line(Report::SteamNeedsUser(NeedsUser::SignIn)),
            (
                2,
                "{\"type\":\"steam_needs_user\",\"v\":1,\"reason\":\"sign_in\"}\n".into()
            )
        );
    }

    #[test]
    fn the_new_lines_are_the_agents_wire() {
        assert_eq!(
            line(Report::ShaderProgress(42)),
            (
                3,
                "{\"type\":\"shader_progress\",\"v\":1,\"percent\":42}\n".into()
            )
        );
        assert_eq!(
            done_line(Done::Launch { appid: 620, ok: true }),
            "{\"type\":\"command_done\",\"v\":1,\"command\":\"launch\",\"appid\":620,\"ok\":true}\n"
        );
        assert_eq!(
            done_line(Done::BigPicture { ok: false }),
            "{\"type\":\"command_done\",\"v\":1,\"command\":\"bigpicture\",\"ok\":false}\n"
        );
        assert_eq!(
            done_line(Done::Quit { appid: 620, ok: true, cloud_synced: false, cloud_sync_started: true }),
            "{\"type\":\"command_done\",\"v\":1,\"command\":\"quit\",\"appid\":620,\"ok\":true,\"cloud_synced\":false}\n"
        );
        assert_eq!(
            done_line(Done::Quit { appid: 620, ok: true, cloud_synced: false, cloud_sync_started: false }),
            "{\"type\":\"command_done\",\"v\":1,\"command\":\"quit\",\"appid\":620,\"ok\":true,\"cloud_synced\":false,\"cloud_sync_started\":false}\n"
        );
    }

    /// The helper's end of a connection, and the agent's.
    fn connected() -> (tempfile::TempDir, Agent, UnixStream) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("launch.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let mut agent = Agent::new(path);
        agent.connect();
        let (theirs, _) = listener.accept().unwrap();
        (dir, agent, theirs)
    }

    #[test]
    fn commands_are_read_off_the_connection_and_anything_else_is_dropped() {
        let (_dir, mut agent, mut theirs) = connected();
        assert_eq!(agent.commands(), vec![]);
        theirs
            .write_all(b"launch 620\nbigpicture\nlaunch 0620\nexec sh\nqu")
            .unwrap();
        // A line split across reads is joined; one past the cap is dropped whole.
        let long = format!("launch {}\n", "1".repeat(MAX_COMMAND));
        let mut got = Vec::new();
        for _ in 0..20 {
            got.extend(agent.commands());
        }
        theirs.write_all(b"it 730\n").unwrap();
        theirs.write_all(long.as_bytes()).unwrap();
        theirs.write_all(b"\xff\xfe\nquit 620\n").unwrap();
        for _ in 0..20 {
            got.extend(agent.commands());
        }
        assert_eq!(
            got,
            vec![
                Command::Launch(620),
                Command::BigPicture,
                Command::Quit(730),
                Command::Quit(620),
            ]
        );
    }

    #[test]
    fn a_quiet_helper_keeps_its_connection_with_an_empty_line() {
        let (_dir, mut agent, theirs) = connected();
        agent.keepalive();
        agent.last_write = Instant::now() - KEEPALIVE;
        agent.keepalive();
        drop(agent);
        let lines: Vec<String> = BufReader::new(theirs).lines().map(Result::unwrap).collect();
        assert_eq!(lines, [""], "one keepalive, and only once it was due");
    }

    #[test]
    fn a_connection_the_agent_closes_is_made_again_at_once() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("launch.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let mut agent = Agent::new(path);
        agent.connect();
        agent.send(Report::LaunchState(LaunchState::ClientOnly));
        let (first, _) = listener.accept().unwrap();
        drop(first);
        assert_eq!(agent.commands(), vec![]);
        listener.set_nonblocking(true).unwrap();
        let (second, _) = listener
            .accept()
            .expect("reconnected on the read that saw the close");
        second.set_nonblocking(false).unwrap();
        drop(agent);
        let lines: Vec<String> = BufReader::new(second).lines().map(Result::unwrap).collect();
        assert_eq!(
            lines,
            [r#"{"type":"launch_state","v":1,"state":"client_only"}"#]
        );
    }

    /// A helper watching 620 with an empty `/proc` and `Steam`'s logs, and a way to append to
    /// the cloud log the way Steam does.
    fn quitting(dir: &std::path::Path) -> (Helper, Agent, std::path::PathBuf, impl Fn(&str) + '_) {
        let (proc, logs) = (dir.join("proc"), dir.join("logs"));
        std::fs::create_dir_all(&proc).unwrap();
        std::fs::create_dir_all(&logs).unwrap();
        let mut helper = Helper::new(proc.clone(), logs.clone(), false);
        helper.watch_with(620, true, Box::new(Fake(vec![])));
        helper.screen = Box::new(Scene::default());
        let agent = Agent::new("/nonexistent/launch.sock".into());
        let append = move |what: &str| {
            use std::io::Write as _;
            let mut file = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(logs.join("cloud_log.txt"))
                .unwrap();
            writeln!(file, "[2026-10-26 12:00:00] [AppID 620] {what}").unwrap();
        };
        (helper, agent, proc, append)
    }

    /// The game's launch reaper, as the watcher's `/proc` scan reads it.
    fn reaper_620(proc: &std::path::Path) {
        let pid = proc.join("20");
        std::fs::create_dir_all(&pid).unwrap();
        std::fs::write(
            pid.join("cmdline"),
            b"/home/u/.local/share/Steam/ubuntu12_32/reaper\0SteamLaunch\0AppId=620\0--\0game\0",
        )
        .unwrap();
        std::fs::write(pid.join("stat"), "20 (reaper) S 1 20 20 0 -1").unwrap();
    }

    /// #810: a `quit` after this watch saw the game exit waits on the exit sync tailed since the
    /// watch began, whose start line was written before the command arrived.
    #[test]
    fn a_quit_after_the_game_exited_waits_on_the_sync_tailed_since_the_watch() {
        let dir = tempfile::tempdir().unwrap();
        let (mut helper, mut agent, proc, append) = quitting(dir.path());
        reaper_620(&proc);
        let t = Instant::now();
        helper.pass(t, &mut agent);
        std::fs::remove_dir_all(proc.join("20")).unwrap();
        helper.pass(t + PASS, &mut agent);
        assert!(helper.game_exited, "the watch saw the exit");
        append("Starting sync (up,AC Exit,)");
        helper.command(Command::Quit(620), &mut agent);
        helper.step_quit(t + Duration::from_secs(1), &mut agent);
        assert!(
            agent.latest[COMMAND_DONE].is_none(),
            "the upload is awaited"
        );
        append("Upload complete in build list");
        helper.step_quit(t + Duration::from_secs(2), &mut agent);
        let line = agent.latest[COMMAND_DONE].clone().expect("quit answered");
        assert!(line.contains("\"cloud_synced\":true"), "{line}");
    }

    /// #872: the game exits and a session end's `quit` arrives before the helper's next pass.
    /// The command polls the watcher first, so the exit counts as seen and the sync is awaited.
    #[test]
    fn a_quit_in_the_same_tick_as_the_game_exit_waits_for_the_exit_sync() {
        let dir = tempfile::tempdir().unwrap();
        let (mut helper, mut agent, proc, append) = quitting(dir.path());
        reaper_620(&proc);
        let t = Instant::now();
        helper.pass(t, &mut agent);
        std::fs::remove_dir_all(proc.join("20")).unwrap();
        append("Starting sync (up,AC Exit,)");
        assert!(!helper.game_exited, "no pass has run since the exit");
        helper.command(Command::Quit(620), &mut agent);
        assert!(helper.game_exited, "the quit polled the watcher");
        helper.step_quit(Instant::now() + Duration::from_secs(1), &mut agent);
        assert!(
            agent.latest[COMMAND_DONE].is_none(),
            "the upload is awaited"
        );
    }

    /// An in-place swap's `quit` for a title that never reached a reaper (still loading, shader
    /// replay, a sign-in wait) is answered at once, as it was before #810.
    #[test]
    fn a_quit_for_a_game_that_never_ran_is_answered_at_once() {
        let dir = tempfile::tempdir().unwrap();
        let (mut helper, mut agent, _proc, append) = quitting(dir.path());
        let t = Instant::now();
        helper.pass(t, &mut agent);
        append("Starting sync (up,AC Exit,)");
        helper.command(Command::Quit(620), &mut agent);
        helper.step_quit(t, &mut agent);
        let line = agent.latest[COMMAND_DONE]
            .clone()
            .expect("answered at once");
        assert!(line.contains("\"cloud_sync_started\":false"), "{line}");
    }

    #[test]
    fn a_new_target_forgets_the_old_games_lines_but_not_a_result() {
        let mut agent = Agent::new("/nonexistent/launch.sock".into());
        agent.send(Report::GameWindow { on_screen: true });
        agent.send(Report::ShaderProgress(50));
        agent.done(Done::Quit {
            appid: 620,
            ok: true,
            cloud_synced: true,
            cloud_sync_started: true,
        });
        agent.forget_game();
        assert!(agent.latest[..COMMAND_DONE].iter().all(Option::is_none));
        assert!(agent.latest[COMMAND_DONE].is_some());
    }

    /// A stand-in `steam` that writes its argv to `argv.txt` beside it.
    /// Tests that write a script and run it take this: a fork in another test thread while
    /// the script is still open for writing inherits that descriptor, and the exec then fails
    /// with ETXTBSY.
    static SCRIPTS: Mutex<()> = Mutex::new(());

    fn scripts() -> std::sync::MutexGuard<'static, ()> {
        SCRIPTS.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn fake_steam(dir: &std::path::Path) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let steam = dir.join("steam");
        std::fs::write(
            &steam,
            format!(
                "#!/bin/sh\nprintf '%s|' \"$@\" >> '{}'\n",
                dir.join("argv.txt").display()
            ),
        )
        .unwrap();
        std::fs::set_permissions(&steam, std::fs::Permissions::from_mode(0o755)).unwrap();
        steam
    }

    fn argv(dir: &std::path::Path) -> String {
        for _ in 0..200 {
            if let Ok(text) = std::fs::read_to_string(dir.join("argv.txt")) {
                return text;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        panic!("the stand-in steam never ran");
    }

    #[test]
    fn a_launch_runs_steam_with_fixed_argv_and_arms_the_watch() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        let mut helper = Helper::new(dir.path().join("proc"), dir.path().join("logs"), false);
        helper.steam = fake_steam(dir.path());
        let mut agent = Agent::new(dir.path().join("none.sock"));
        helper.command(Command::Launch(1145360), &mut agent);
        assert_eq!(argv(dir.path()), "-applaunch|1145360|");
        assert_eq!(helper.watcher.as_ref().map(Watcher::appid), Some(1145360));
        assert!(helper.launched);
        assert_eq!(
            agent.latest[COMMAND_DONE].as_deref(),
            Some("{\"type\":\"command_done\",\"v\":1,\"command\":\"launch\",\"appid\":1145360,\"ok\":true}\n")
        );
    }

    #[test]
    fn big_picture_runs_steams_own_url_and_pins_nothing() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        let mut helper = Helper::new(dir.path().join("proc"), dir.path().join("logs"), false);
        helper.steam = fake_steam(dir.path());
        let mut agent = Agent::new(dir.path().join("none.sock"));
        helper.command(Command::BigPicture, &mut agent);
        assert_eq!(argv(dir.path()), "steam://open/bigpicture|");
        assert_eq!(
            helper.watcher.as_ref().map(Watcher::appid),
            Some(STEAM_CLIENT_APPID)
        );
        assert!(!helper.launched, "Steam pins Big Picture itself");
        assert_eq!(helper.wanted_pin(), None);
    }

    #[test]
    fn a_launch_that_cannot_run_steam_says_so_and_watches_nothing_new() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        let mut helper = Helper::new(dir.path().join("proc"), dir.path().join("logs"), false);
        helper.steam = dir.path().join("missing");
        let mut agent = Agent::new(dir.path().join("none.sock"));
        helper.command(Command::Launch(620), &mut agent);
        assert!(helper.watcher.is_none());
        assert!(agent.latest[COMMAND_DONE]
            .as_deref()
            .unwrap()
            .contains("\"ok\":false"));
    }

    #[test]
    fn at_most_eight_steam_commands_run_at_once() {
        let _scripts = scripts();
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let steam = dir.path().join("steam");
        std::fs::write(&steam, "#!/bin/sh\nexec sleep 5\n").unwrap();
        std::fs::set_permissions(&steam, std::fs::Permissions::from_mode(0o755)).unwrap();
        let mut helper = Helper::new(dir.path().join("proc"), dir.path().join("logs"), false);
        helper.steam = steam;
        for n in 0..MAX_CHILDREN {
            assert!(helper.run_steam(&["-applaunch", "620"]), "command {n}");
        }
        assert!(
            !helper.run_steam(&["-applaunch", "620"]),
            "the ninth is refused"
        );
        for mut child in std::mem::take(&mut helper.children) {
            let _ = child.kill();
            let _ = child.wait();
        }
        assert!(
            helper.run_steam(&["-applaunch", "620"]),
            "room again once they are gone"
        );
        for mut child in std::mem::take(&mut helper.children) {
            let _ = child.kill();
            let _ = child.wait();
        }
    }

    struct Fake(Vec<WindowInfo>);

    impl Windows for Fake {
        fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>> {
            Ok(self.0.clone())
        }
        fn focused_app(&mut self) -> std::io::Result<Option<u32>> {
            Ok(None)
        }
    }

    fn watching(dir: &std::path::Path, windows: Vec<WindowInfo>) -> Helper {
        std::fs::create_dir_all(dir.join("logs")).unwrap();
        let mut helper = Helper::new(dir.join("proc"), dir.join("logs"), true);
        let mut watcher = Watcher::new(
            WatchConfig {
                proc_root: dir.join("proc"),
                steam_log_dir: dir.join("logs"),
                appid: 620,
            },
            Box::new(Fake(windows)),
        );
        watcher.poll();
        helper.watcher = Some(watcher);
        helper.launched = true;
        helper
    }

    #[test]
    fn a_launched_game_is_pinned_once_its_window_exists_and_steam_while_it_needs_the_user() {
        let dir = tempfile::tempdir().unwrap();
        let mut helper = watching(dir.path(), vec![]);
        assert_eq!(helper.wanted_pin(), None, "never before the window exists");
        helper.needs_user = true;
        assert_eq!(helper.wanted_pin(), Some(Pin::Steam));
        helper.needs_user = false;
        helper.notice = Notice::Pinned(Instant::now());
        assert_eq!(helper.wanted_pin(), Some(Pin::Steam));
        helper.launched = false;
        assert_eq!(
            helper.wanted_pin(),
            Some(Pin::Steam),
            "answering the notice pins a tile Steam launched from its flags too"
        );
        helper.notice = Notice::Waiting;
        assert_eq!(
            helper.wanted_pin(),
            None,
            "otherwise a game Steam launched from its flags is not ours to pin"
        );

        let dir = tempfile::tempdir().unwrap();
        let mut helper = watching(
            dir.path(),
            vec![WindowInfo {
                steam_game: Some(620),
                wm_class: "portal2_linux".into(),
                title: "Portal 2".into(),
            }],
        );
        helper.needs_user = true;
        assert_eq!(helper.wanted_pin(), Some(Pin::Game(620)), "the game wins");
    }

    /// gamescope as a test sets it: its windows and focus, and what the helper did to it.
    #[derive(Clone, Default)]
    struct Scene {
        windows: Arc<Mutex<Vec<WindowInfo>>>,
        focus: Arc<Mutex<Option<u32>>>,
        actions: Arc<Mutex<Vec<String>>>,
        pin_fails: bool,
        /// (the focus read fails, the window read fails)
        reads_fail: Arc<Mutex<(bool, bool)>>,
    }

    impl Windows for Scene {
        fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>> {
            if self.reads_fail.lock().unwrap().1 {
                return Err(std::io::Error::other("X went away"));
            }
            Ok(self.windows.lock().unwrap().clone())
        }
        fn focused_app(&mut self) -> std::io::Result<Option<u32>> {
            if self.reads_fail.lock().unwrap().0 {
                return Err(std::io::Error::other("X went away"));
            }
            Ok(*self.focus.lock().unwrap())
        }
    }

    impl Screen for Scene {
        fn pin(&mut self, appid: u32) -> std::io::Result<()> {
            if self.pin_fails {
                return Err(std::io::Error::other("no display"));
            }
            self.actions.lock().unwrap().push(format!("pin {appid}"));
            Ok(())
        }
        fn close(&mut self, appid: u32) -> std::io::Result<usize> {
            self.actions.lock().unwrap().push(format!("close {appid}"));
            Ok(0)
        }
        fn press_return(&mut self) -> std::io::Result<()> {
            self.actions.lock().unwrap().push("return".into());
            Ok(())
        }
    }

    impl Scene {
        fn did(&self) -> Vec<String> {
            self.actions.lock().unwrap().clone()
        }
        fn focus(&self, appid: Option<u32>) {
            *self.focus.lock().unwrap() = appid;
        }
    }

    struct Notice620 {
        _dir: tempfile::TempDir,
        logs: PathBuf,
        helper: Helper,
        agent: Agent,
        scene: Scene,
        t0: Instant,
    }

    impl Notice620 {
        fn pass(&mut self, after: Duration) {
            self.helper.pass(self.t0 + after, &mut self.agent);
        }
        fn log(&self, file: &str, line: &str) {
            let mut log = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(self.logs.join(file))
                .unwrap();
            writeln!(log, "{line}").unwrap();
        }
        /// The notice reported to the agent, by the reason the helper sent.
        fn reported(&self) -> Option<String> {
            self.agent.latest[NEEDS].clone()
        }
    }

    const NEEDS: usize = 2;
    const GRACE: Duration = Duration::from_millis(2100);
    const PASS: Duration = Duration::from_millis(250);

    /// A helper watching 620 (`launched`: from a `launch` command, else from the tile's
    /// flags), with Steam's controller notice standing since `t0`.
    fn notice(dismiss: bool, launched: bool, scene: Scene) -> Notice620 {
        let dir = tempfile::tempdir().unwrap();
        let logs = dir.path().join("logs");
        std::fs::create_dir_all(&logs).unwrap();
        for file in ["console_log.txt", "steamui_login.txt"] {
            std::fs::write(logs.join(file), "").unwrap();
        }
        let mut helper = Helper::new(dir.path().join("proc"), logs.clone(), dismiss);
        helper.watch_with(620, launched, Box::new(scene.clone()));
        helper.screen = Box::new(scene.clone());
        let mut n = Notice620 {
            _dir: dir,
            logs,
            helper,
            agent: Agent::new("/nonexistent/launch.sock".into()),
            scene,
            t0: Instant::now(),
        };
        n.pass(Duration::ZERO);
        n.log(
            "console_log.txt",
            "GameAction [AppID 620, ActionID 1] : LaunchApp waiting for user response to ShowInterstitials \"\"",
        );
        n.pass(Duration::ZERO);
        n
    }

    #[test]
    fn under_show_the_notice_is_revealed_at_the_grace_and_never_answered() {
        let mut n = notice(false, false, Scene::default());
        n.pass(GRACE);
        assert!(n
            .reported()
            .is_some_and(|l| l.contains("\"reason\":\"other\"")));
        assert_eq!(n.helper.notice, Notice::Waiting);
        assert!(
            n.scene.did().is_empty(),
            "show: nothing is pinned or pressed"
        );
    }

    #[test]
    fn under_dismiss_safe_steams_window_is_pinned_then_answered_once_it_has_focus() {
        for launched in [false, true] {
            let mut n = notice(true, launched, Scene::default());
            n.pass(GRACE);
            assert_eq!(n.scene.did(), ["pin 769"], "pinned first ({launched})");
            assert!(n.reported().is_none(), "not revealed while answering");
            n.pass(GRACE + PASS);
            assert_eq!(
                n.scene.did(),
                ["pin 769"],
                "no press before Steam has focus"
            );
            n.scene.focus(Some(STEAM_CLIENT_APPID));
            let pressed = GRACE + PASS * 2;
            n.pass(pressed);
            assert_eq!(n.scene.did(), ["pin 769", "return"]);
            assert_eq!(n.helper.notice, Notice::Answered);
            n.pass(pressed + Duration::from_millis(1900));
            assert!(n.reported().is_none());
            n.pass(pressed + GRACE);
            assert!(
                n.reported()
                    .is_some_and(|l| l.contains("\"reason\":\"other\"")),
                "still the wait 2 s after the press: shown"
            );
            // The game's window then takes the pin back, launched from its flags or not.
            n.scene.windows.lock().unwrap().push(WindowInfo {
                steam_game: Some(620),
                wm_class: "portal2_linux".into(),
                title: "Portal 2".into(),
            });
            n.pass(pressed + GRACE + PASS);
            assert_eq!(n.scene.did(), ["pin 769", "return", "pin 620"]);
        }
    }

    /// The press is refused: the notice is shown, and nothing gets the key.
    fn assert_refused(n: &mut Notice620, at: Duration) {
        assert_eq!(n.helper.notice, Notice::Refused, "refused");
        n.pass(at + PASS);
        assert!(
            n.reported().is_some(),
            "the notice, or what refused it, is shown"
        );
        assert!(
            !n.scene.did().contains(&"return".to_string()),
            "{:?}",
            n.scene.did()
        );
    }

    #[test]
    fn under_dismiss_safe_nothing_is_pressed_while_steam_shows_anything_else() {
        let dialog = |title: &str| WindowInfo {
            steam_game: Some(STEAM_CLIENT_APPID),
            wm_class: "steamwebhelper steam".into(),
            title: title.into(),
        };
        for (why, window, login) in [
            (
                "a cloud conflict",
                Some(dialog("Steam Cloud Conflict")),
                None,
            ),
            (
                "Steam Guard",
                Some(dialog("Steam Guard - Computer Authorization Required")),
                None,
            ),
            ("an EULA", Some(dialog("End User License Agreement")), None),
            (
                "a sign-in",
                None,
                Some("[2026-10-16 09:37:10] [ None ] SetLoginState: WaitingForCredentials - OK"),
            ),
        ] {
            let scene = Scene::default();
            scene.focus(Some(STEAM_CLIENT_APPID));
            if let Some(window) = window {
                scene.windows.lock().unwrap().push(window);
            }
            let mut n = notice(true, true, scene);
            if let Some(line) = login {
                n.log("steamui_login.txt", line);
            }
            n.pass(GRACE);
            assert_refused(&mut n, GRACE);
            assert!(n.helper.needs_user, "{why}");
        }
    }

    #[test]
    fn under_dismiss_safe_an_earlier_needs_user_report_refuses_the_press() {
        let scene = Scene::default();
        scene.focus(Some(STEAM_CLIENT_APPID));
        let mut n = notice(true, true, scene);
        // Steam needed the user for something this launch already.
        n.helper.needs_user = true;
        n.pass(GRACE);
        assert_refused(&mut n, GRACE);
    }

    #[test]
    fn under_dismiss_safe_a_window_that_never_takes_focus_is_not_pressed() {
        for focus in [None, Some(620), Some(413091)] {
            let mut n = notice(true, false, Scene::default());
            n.scene.focus(focus);
            n.pass(GRACE);
            assert_eq!(n.scene.did(), ["pin 769"]);
            n.pass(GRACE + Duration::from_millis(1900));
            assert!(
                matches!(n.helper.notice, Notice::Pinned(_)),
                "still waiting for focus"
            );
            n.pass(GRACE + NOTICE_FOCUS_WAIT);
            assert_refused(&mut n, GRACE + NOTICE_FOCUS_WAIT);
        }
    }

    #[test]
    fn under_dismiss_safe_a_pass_whose_x_read_failed_does_not_press() {
        for (focus_fails, mapped_fails) in [(true, false), (false, true)] {
            let mut n = notice(true, false, Scene::default());
            n.pass(GRACE);
            assert_eq!(n.scene.did(), ["pin 769"]);
            // Steam's window took focus, then the next pass reads X with an error.
            n.scene.focus(Some(STEAM_CLIENT_APPID));
            *n.scene.reads_fail.lock().unwrap() = (focus_fails, mapped_fails);
            n.pass(GRACE + PASS);
            assert_eq!(
                n.scene.did(),
                ["pin 769"],
                "no press on stale facts ({focus_fails}, {mapped_fails})"
            );
            assert!(matches!(n.helper.notice, Notice::Pinned(_)), "it waits");
            *n.scene.reads_fail.lock().unwrap() = (false, false);
            n.pass(GRACE + PASS * 2);
            assert_eq!(
                n.scene.did(),
                ["pin 769", "return"],
                "pressed once read again"
            );
        }
    }

    #[test]
    fn under_dismiss_safe_a_pin_that_fails_is_not_pressed() {
        let scene = Scene {
            pin_fails: true,
            ..Scene::default()
        };
        scene.focus(Some(STEAM_CLIENT_APPID));
        let mut n = notice(true, false, scene);
        n.pass(GRACE);
        n.pass(GRACE + PASS);
        assert_refused(&mut n, GRACE + PASS);
    }

    #[test]
    fn a_new_connection_repeats_the_latest_of_each_kind() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("launch.sock");
        let mut agent = Agent::new(path.clone());
        // Nothing listens yet: reports are kept, not lost.
        agent.connect();
        agent.send(Report::LaunchState(LaunchState::Starting));
        agent.send(Report::LaunchState(LaunchState::ClientOnly));
        agent.send(Report::GameWindow { on_screen: false });

        let listener = UnixListener::bind(&path).unwrap();
        agent.next_try = Instant::now();
        agent.connect();
        agent.send(Report::SteamNeedsUser(NeedsUser::Eula));
        drop(agent);
        let (stream, _) = listener.accept().unwrap();
        let lines: Vec<String> = BufReader::new(stream).lines().map(Result::unwrap).collect();
        assert_eq!(
            lines,
            [
                r#"{"type":"launch_state","v":1,"state":"client_only"}"#,
                r#"{"type":"game_window","v":1,"on_screen":false}"#,
                r#"{"type":"steam_needs_user","v":1,"reason":"eula"}"#,
            ]
        );
    }

    #[test]
    fn a_dropped_connection_is_replaced_at_the_next_line() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("launch.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let mut agent = Agent::new(path.clone());
        agent.connect();
        agent.send(Report::LaunchState(LaunchState::Starting));
        // The agent drops a connection that goes quiet.
        let (first, _) = listener.accept().unwrap();
        drop(first);
        agent.send(Report::LaunchState(LaunchState::ClientOnly));
        agent.send(Report::GameWindow { on_screen: true });
        drop(agent);
        // Already queued, or the helper never reconnected: fail, do not hang.
        listener.set_nonblocking(true).unwrap();
        let (second, _) = listener.accept().expect("the helper reconnected");
        second.set_nonblocking(false).unwrap();
        let mut lines: Vec<String> = BufReader::new(second).lines().map(Result::unwrap).collect();
        lines.dedup();
        assert_eq!(
            lines,
            [
                r#"{"type":"launch_state","v":1,"state":"client_only"}"#,
                r#"{"type":"game_window","v":1,"on_screen":true}"#,
            ]
        );
    }
}
