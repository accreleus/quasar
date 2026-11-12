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
    appid_from_flags, parse_command, steam_client_running, CloudEnd, CloudSync, Command,
    LaunchState, NeedsUser, Quit, QuitAction, Report, WatchConfig, Watcher, WindowInfo, Windows,
    STEAM_CLIENT_APPID,
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
/// A Steam still signing in accepts `-applaunch` and drops it (#1159), so a launch it has shown
/// no sign of taking is sent again this often, for this long. 120 s is past the 90 s reveal cap.
const RELAUNCH_EVERY: Duration = Duration::from_secs(5);
const RELAUNCH_FOR: Duration = Duration::from_secs(120);
/// The retry waits out a need for the user (a sign-in), which does not count against
/// [`RELAUNCH_FOR`], but never beyond this after the launch.
const RELAUNCH_MAX: Duration = Duration::from_secs(600);
/// A launch Steam took up can still be lost: it moved the app into a shader or depot update,
/// another app's update suspended that, and Steam dropped the app from its schedule without
/// running it (#1314). That launch is sent again, at most this many times.
const DROPPED_RETRIES: u8 = 2;
/// How long the dropped state must stand before the launch is sent again. Steam writes
/// "removed from schedule" a moment before the launch's next step when it does carry on, and a
/// pass that lands between the two lines must not launch a second time.
const DROPPED_SETTLE: Duration = Duration::from_secs(5);
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
    /// A `launch` Steam has not yet been seen to take up; sent again until it is.
    relaunch: Option<Relaunch>,
    /// A `launch` waiting for the entrypoint's own `steam -silent` to start: one that ran
    /// `steam -applaunch` before a client exists would start a second Steam, and the Steam
    /// image starts its client only after this helper's `ready`.
    pending: Option<u32>,
    /// The watched game's cloud log, read from when the watch began, and whether this watch has
    /// reported the game's exit. Only a quit after that exit waits on it for the exit sync (#810).
    exit_sync: Option<(u32, CloudSync)>,
    game_exited: bool,
    children: Vec<Child>,
    screen: Box<dyn Screen>,
    /// `steam` from `PATH`; a test points it at a stand-in.
    steam: PathBuf,
    /// A test waits for the stand-in to finish, so what it writes lands before `run_steam` returns.
    #[cfg(test)]
    wait_for_steam: bool,
}

#[derive(Debug, Clone, Copy)]
struct Relaunch {
    appid: u32,
    next: Instant,
    /// The end of the retry window; it moves out while Steam needs the user.
    until: Instant,
    /// The hard end, however long Steam needed the user.
    last: Instant,
    /// The previous step, to count how long Steam needed the user.
    stepped: Instant,
    /// Steam had taken the launch up at the previous step; a failure of it re-arms the retry.
    taken: bool,
    /// How many times a launch Steam took up and then dropped with a suspended update has been
    /// sent again (#1314).
    dropped_retries: u8,
    /// Since when the watcher has read the launch as dropped, without a break.
    dropped_since: Option<Instant>,
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
            relaunch: None,
            pending: None,
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
            #[cfg(test)]
            wait_for_steam: false,
        }
    }

    fn watch(&mut self, appid: u32, launched: bool) {
        self.watch_with(appid, launched, Box::<X11Windows>::default());
    }

    fn new_watcher(&self, appid: u32, windows: Box<dyn Windows + Send>) -> Watcher {
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
        watcher
    }

    fn watch_with(&mut self, appid: u32, launched: bool, windows: Box<dyn Windows + Send>) {
        let watcher = self.new_watcher(appid, windows);
        self.arm(watcher, appid, launched);
    }

    fn arm(&mut self, watcher: Watcher, appid: u32, launched: bool) {
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
                // A launch that does not run leaves no retry of an earlier target behind,
                // and no earlier held launch either.
                self.relaunch = None;
                self.pending = None;
                if !self.steam_running() {
                    eprintln!(
                        "quasar-launch-helper: no Steam client yet; app {appid} waits for it"
                    );
                    self.pending = Some(appid);
                    return;
                }
                self.launch(appid, agent);
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
                self.relaunch = None;
                // A held launch of the game being quit must never run after it (#1243).
                self.pending = None;
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

    /// Whether a Steam client process exists yet. An unreadable `/proc` never holds a
    /// launch back: the helper runs inside the container's own pid namespace, where it is
    /// always there.
    fn steam_running(&self) -> bool {
        steam_client_running(&self.proc_root) != Some(false)
    }

    /// Run the target game through the client that is already up.
    fn launch(&mut self, appid: u32, agent: &mut Agent) {
        // The watcher's baseline in Steam's logs is taken before Steam can write
        // anything for this launch, or the first thing it logs would be skipped.
        let mut watcher = self.new_watcher(appid, Box::<X11Windows>::default());
        let now = Instant::now();
        let baseline = watcher.poll_at(now);
        let ok = self.run_steam(&["-applaunch", &appid.to_string()]);
        if ok {
            eprintln!("quasar-launch-helper: launched Steam app {appid}");
            agent.forget_game();
            self.arm(watcher, appid, true);
            self.forward(baseline, agent);
            self.relaunch = Some(Relaunch {
                appid,
                next: now + RELAUNCH_EVERY,
                until: now + RELAUNCH_FOR,
                last: now + RELAUNCH_MAX,
                stepped: now,
                taken: false,
                dropped_retries: 0,
                dropped_since: None,
            });
        }
        agent.done(Done::Launch { appid, ok });
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
                #[cfg(test)]
                let child = {
                    let mut child = child;
                    if self.wait_for_steam {
                        let _ = child.wait();
                    }
                    child
                };
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
        if let Some(appid) = self.pending {
            if self.steam_running() {
                self.pending = None;
                self.launch(appid, agent);
            }
        }
        self.poll_watcher(now, agent);
        self.step_relaunch(now);
        self.answer_notice(now);
        self.pin();
        self.step_quit(now, agent);
    }

    /// Send the armed `launch` again while Steam shows no sign of having taken it. The helper
    /// relaunches only on positive evidence the launch was not taken, and never against what
    /// the user is doing now: not once the game runs or exited, another game or launch showed
    /// up, a quit runs, the target changed, or the window is over. While Steam's log shows the
    /// launch moving it holds; if Steam then logs that action as failed, the retry goes on.
    /// While Steam needs the user (a sign-in) it waits, and that time is not counted, up to
    /// [`RELAUNCH_MAX`] in all.
    ///
    /// A launch Steam did take up is also sent again when Steam then loses it: the app went into
    /// a shader or depot update, another app's update suspended that, and Steam dropped the app
    /// from its schedule without running it (#1314). [`DROPPED_RETRIES`] bounds that, and the
    /// watcher's evidence is all of it against a launch in progress.
    fn step_relaunch(&mut self, now: Instant) {
        let Some(mut relaunch) = self.relaunch else {
            return;
        };
        let Some(watcher) = self.watcher.as_ref() else {
            self.relaunch = None;
            return;
        };
        let live = watcher.appid() == relaunch.appid
            && !watcher.elsewhere()
            && matches!(
                watcher.launch_state(),
                LaunchState::Starting | LaunchState::ClientOnly
            );
        let paused = watcher.needs_now().is_some();
        let taken = watcher.launch_taken();
        // A launch Steam took up can still be lost (#1314): it moved the app into a shader or
        // depot update, another app's update suspended that, and Steam dropped the app from its
        // schedule without running it. Read here, before anything in the pass changes it, and
        // only for a launch Steam did take up.
        let dropped = taken && watcher.launch_dropped();
        relaunch.dropped_since = dropped.then(|| relaunch.dropped_since.unwrap_or(now));
        let dropped = relaunch
            .dropped_since
            .is_some_and(|since| now.saturating_duration_since(since) >= DROPPED_SETTLE);
        // Progress on the launch stops the retry for as long as it stands; Steam then failing
        // that action starts a fresh window, inside the same hard end.
        if relaunch.taken && !taken {
            relaunch.until = now + RELAUNCH_FOR;
            relaunch.next = now + RELAUNCH_EVERY;
        }
        relaunch.taken = taken;
        if paused {
            relaunch.until += now.saturating_duration_since(relaunch.stepped);
        }
        relaunch.stepped = now;
        // The soft window ends the sending, not the watch: while the launch is taken the retry
        // stays armed until the hard end, so a failure Steam logs later still gets it sent again.
        let over = now >= relaunch.last || (!taken && now >= relaunch.until);
        if !live || self.quit.is_some() || over {
            self.relaunch = None;
            return;
        }
        if !paused && !taken && now >= relaunch.next {
            relaunch.next = now + RELAUNCH_EVERY;
            if self.run_steam(&["-applaunch", &relaunch.appid.to_string()]) {
                eprintln!(
                    "quasar-launch-helper: Steam has not taken up app {}; launched it again",
                    relaunch.appid
                );
            }
        } else if !paused
            && now >= relaunch.next
            && dropped
            && relaunch.dropped_retries < DROPPED_RETRIES
        {
            // Steam took the launch up and then lost it: sending it again is what runs the game.
            // `dropped` clears on any newer line for the app or when the game runs, so this is
            // once per detection, and the count bounds a lost launch that keeps being lost.
            relaunch.dropped_retries += 1;
            relaunch.next = now + RELAUNCH_EVERY;
            // The next retry waits for the state to stand again after this one.
            relaunch.dropped_since = Some(now);
            if self.run_steam(&["-applaunch", &relaunch.appid.to_string()]) {
                eprintln!(
                    "quasar-launch-helper: token=\"launch-dropped\" Steam dropped app {}'s launch \
                     with a suspended shader or depot update; launched it again (retry {}/{DROPPED_RETRIES})",
                    relaunch.appid, relaunch.dropped_retries
                );
            }
        }
        self.relaunch = Some(relaunch);
    }

    /// Poll the watcher and forward its reports, noting the ones the helper acts on.
    fn poll_watcher(&mut self, now: Instant, agent: &mut Agent) {
        let Some(watcher) = &mut self.watcher else {
            return;
        };
        let reports = watcher.poll_at(now);
        self.forward(reports, agent);
    }

    /// Send the watcher's reports on, noting the ones the helper acts on.
    fn forward(&mut self, reports: Vec<Report>, agent: &mut Agent) {
        for report in reports {
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
                "#!/bin/sh\nprintf '%s|' \"$@\" >> '{0}'\n[ -f '{1}' ] && cat '{1}' >> '{2}'\nexit 0\n",
                dir.join("argv.txt").display(),
                dir.join("steam_says.txt").display(),
                dir.join("logs/console_log.txt").display()
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

    /// A Steam client process, as the helper's `/proc` scan reads it: the entrypoint's own
    /// `steam -silent`.
    fn steam_client(proc: &std::path::Path) {
        let pid = proc.join("7");
        std::fs::create_dir_all(&pid).unwrap();
        std::fs::write(pid.join("cmdline"), b"/usr/bin/steam\0-silent\0").unwrap();
        std::fs::write(pid.join("stat"), "7 (steam) S 1 7 7 0 -1").unwrap();
    }

    /// #1243: the Steam image starts its client only after this helper printed `ready`, so
    /// a `launch` queued before that waits for a Steam process instead of starting a second
    /// Steam. `ready` itself is never held back by the wait.
    #[test]
    fn a_launch_waits_for_a_steam_client_before_running_steam() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        let proc = dir.path().join("proc");
        std::fs::create_dir_all(&proc).unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let mut helper = Helper::new(proc.clone(), dir.path().join("logs"), false);
        helper.steam = fake_steam(dir.path());
        helper.wait_for_steam = true;
        let mut agent = Agent::new(dir.path().join("none.sock"));
        let t = Instant::now();
        helper.command(Command::Launch(1145360), &mut agent);
        helper.pass(t, &mut agent);
        assert!(
            !dir.path().join("argv.txt").exists(),
            "no steam before a client runs"
        );
        assert!(
            agent.latest[COMMAND_DONE].is_none(),
            "and the launch is not answered yet"
        );
        // A newer launch replaces the held one: only the latest game ever runs.
        helper.command(Command::Launch(620), &mut agent);
        steam_client(&proc);
        helper.pass(t + PASS, &mut agent);
        assert_eq!(argv(dir.path()), "-applaunch|620|");
        assert!(agent.latest[COMMAND_DONE]
            .as_deref()
            .unwrap()
            .contains("\"ok\":true"));
    }

    /// A helper whose `launch 1145360` has been sent to the stand-in `steam`, and its clock.
    fn launched(dir: &std::path::Path) -> (Helper, Agent, Instant) {
        let mut helper = Helper::new(dir.join("proc"), dir.join("logs"), false);
        helper.steam = fake_steam(dir);
        helper.wait_for_steam = true;
        let mut agent = Agent::new(dir.join("none.sock"));
        helper.command(Command::Launch(1145360), &mut agent);
        (helper, agent, Instant::now())
    }

    /// Wait for the stand-in `steam` to have run `n` times, then for any extra run to show.
    fn runs(dir: &std::path::Path, n: usize) -> usize {
        let count = || {
            std::fs::read_to_string(dir.join("argv.txt"))
                .map_or(0, |t| t.matches("-applaunch").count())
        };
        for _ in 0..200 {
            if count() >= n {
                break;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        std::thread::sleep(Duration::from_millis(150));
        count()
    }

    fn log_raw(dir: &std::path::Path, text: &str) {
        use std::io::Write;
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(dir.join("logs/console_log.txt"))
            .unwrap();
        file.write_all(text.as_bytes()).unwrap();
    }

    fn log_line(dir: &std::path::Path, line: &str) {
        use std::io::Write;
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(dir.join("logs/console_log.txt"))
            .unwrap();
        writeln!(file, "{line}").unwrap();
    }

    #[test]
    fn a_launch_steam_ignored_is_sent_again_until_it_takes() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        assert_eq!(runs(dir.path(), 1), 1);
        helper.pass(t0 + Duration::from_secs(1), &mut agent);
        assert_eq!(runs(dir.path(), 1), 1, "not before the interval");
        helper.pass(t0 + Duration::from_secs(6), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "Steam signing in dropped the first");
        helper.pass(t0 + Duration::from_secs(8), &mut agent);
        helper.pass(t0 + Duration::from_secs(12), &mut agent);
        assert_eq!(runs(dir.path(), 3), 3);
        // Steam takes the launch up: it logs an action for the game.
        log_line(
            dir.path(),
            "GameAction [AppID 1145360, ActionID 1] : LaunchApp changed task to CreatingProcess",
        );
        helper.pass(t0 + Duration::from_secs(13), &mut agent);
        helper.pass(t0 + Duration::from_secs(30), &mut agent);
        assert_eq!(
            runs(dir.path(), 4),
            3,
            "a launch Steam took is not sent again while it stands"
        );
    }

    const PROGRESS: &str =
        "GameAction [AppID 1145360, ActionID 1] : LaunchApp changed task to CreatingProcess";

    #[test]
    fn a_launch_is_not_sent_again_once_it_is_no_longer_the_users_to_repeat() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        let after = Duration::from_secs(6);
        type Stop<'a> = Box<dyn Fn(&mut Helper, &mut Agent) + 'a>;
        let stops: [(&str, Stop); 6] = [
            (
                "the game runs",
                Box::new(|_, _| log_line(dir.path(), "Game process added : AppID 1145360 \"x\"")),
            ),
            (
                "a quit runs",
                Box::new(|h, a| h.command(Command::Quit(1145360), a)),
            ),
            (
                "the window is over",
                Box::new(|h, _| h.relaunch.as_mut().unwrap().until = Instant::now()),
            ),
            (
                "another game runs",
                Box::new(|_, _| {
                    let proc = dir.path().join("proc/42");
                    std::fs::create_dir_all(&proc).unwrap();
                    std::fs::write(
                        proc.join("cmdline"),
                        b"/steam/reaper\0SteamLaunch\0AppId=620\0--\0game\0",
                    )
                    .unwrap();
                }),
            ),
            (
                "Steam launches another app",
                Box::new(|_, _| {
                    log_line(
                        dir.path(),
                        "GameAction [AppID 620, ActionID 1] : LaunchApp changed task to CreatingProcess",
                    )
                }),
            ),
            (
                "another launch cannot run",
                Box::new(|h, a| {
                    h.steam = dir.path().join("missing");
                    h.command(Command::Launch(620), a);
                }),
            ),
        ];
        for (why, stop) in stops {
            let _ = std::fs::remove_dir_all(dir.path().join("proc"));
            let _ = std::fs::remove_file(dir.path().join("argv.txt"));
            let _ = std::fs::remove_file(dir.path().join("logs/console_log.txt"));
            std::fs::create_dir_all(dir.path().join("logs")).unwrap();
            std::fs::write(dir.path().join("logs/console_log.txt"), "old\n").unwrap();
            let (mut helper, mut agent, t0) = launched(dir.path());
            assert_eq!(runs(dir.path(), 1), 1);
            stop(&mut helper, &mut agent);
            helper.pass(t0 + after, &mut agent);
            helper.pass(t0 + after * 2, &mut agent);
            assert_eq!(runs(dir.path(), 2), 1, "no second launch once {why}");
        }
    }

    #[test]
    fn a_launch_that_cannot_run_clears_the_retry_of_the_one_before() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        let (mut helper, mut agent, _) = launched(dir.path());
        assert!(helper.relaunch.is_some());
        helper.steam = dir.path().join("missing");
        helper.command(Command::Launch(620), &mut agent);
        assert!(helper.relaunch.is_none());
    }

    #[test]
    fn what_steam_logs_right_after_the_launch_counts_as_the_launch_taken() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        std::fs::write(
            dir.path().join("logs/console_log.txt"),
            "an earlier session\n",
        )
        .unwrap();
        // The stand-in `steam` logs the progress as part of the launch itself, before the
        // helper's next step: only a baseline taken before the spawn sees it.
        std::fs::write(dir.path().join("steam_says.txt"), format!("{PROGRESS}\n")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        helper.pass(t0, &mut agent);
        helper.pass(t0 + Duration::from_secs(6), &mut agent);
        helper.pass(t0 + Duration::from_secs(12), &mut agent);
        assert_eq!(runs(dir.path(), 2), 1, "no retry of a launch Steam took");
    }

    #[test]
    fn a_refusal_that_logs_progress_then_failure_gets_the_launch_sent_again() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        let action = |id: u32, text: &str| {
            format!("GameAction [AppID 1145360, ActionID {id}] : LaunchApp {text}\n")
        };
        log_raw(
            dir.path(),
            &action(1, "changed task to RequestingLicense with \"\""),
        );
        helper.pass(at(1), &mut agent);
        helper.pass(at(8), &mut agent);
        assert_eq!(runs(dir.path(), 2), 1, "while it moves, it holds");
        // The failure, its first half in one poll and the rest in the next.
        let failed = action(1, "failed with AppError_5 with \"\"");
        let (head, tail) = failed.split_at(40);
        log_raw(dir.path(), head);
        helper.pass(at(9), &mut agent);
        assert_eq!(runs(dir.path(), 2), 1, "half a line says nothing");
        log_raw(dir.path(), tail);
        helper.pass(at(10), &mut agent);
        helper.pass(at(16), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "a failed action is sent again");
        log_raw(dir.path(), &action(1, "changed task to Failed with \"\""));
        helper.pass(at(17), &mut agent);
        // The next action moves: held again.
        log_raw(
            dir.path(),
            &action(2, "changed task to CreatingProcess with \"\""),
        );
        helper.pass(at(18), &mut agent);
        helper.pass(at(40), &mut agent);
        assert_eq!(runs(dir.path(), 3), 2);
    }

    #[test]
    fn a_failure_after_progress_does_not_reset_the_hard_end() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        log_raw(
            dir.path(),
            "GameAction [AppID 1145360, ActionID 1] : LaunchApp changed task to RequestingLicense with \"\"\n",
        );
        helper.pass(at(1), &mut agent);
        // The production loop keeps polling past the first soft window while the launch stands.
        helper.pass(at(121), &mut agent);
        helper.pass(at(300), &mut agent);
        log_raw(
            dir.path(),
            "GameAction [AppID 1145360, ActionID 1] : LaunchApp failed with AppError_5 with \"\"\n",
        );
        helper.pass(at(590), &mut agent);
        helper.pass(at(596), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "a late failure is still retried");
        helper.pass(at(RELAUNCH_MAX.as_secs() + 1), &mut agent);
        assert!(helper.relaunch.is_none());
    }

    struct Unreadable;

    impl Windows for Unreadable {
        fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>> {
            Err(std::io::Error::other("no window list"))
        }
        fn focused_app(&mut self) -> std::io::Result<Option<u32>> {
            Ok(None)
        }
    }

    #[test]
    fn a_finished_sign_in_resumes_the_retry_even_when_the_windows_cannot_be_read() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let mut watcher = helper.new_watcher(1145360, Box::new(Unreadable));
        watcher.poll_at(t0);
        helper.watcher = Some(watcher);
        let at = |secs| t0 + Duration::from_secs(secs);
        let login =
            |text: &str| std::fs::write(dir.path().join("logs/steamui_login.txt"), text).unwrap();
        login("SetLoginState: WaitingForCredentials\n");
        helper.pass(at(6), &mut agent);
        helper.pass(at(60), &mut agent);
        assert_eq!(runs(dir.path(), 2), 1, "paused at the sign-in");
        login("SetLoginState: WaitingForCredentials\nSetLoginState: LoggedIn\n");
        helper.pass(at(65), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "signed in: sent again");
    }

    #[test]
    fn a_line_that_is_not_progress_does_not_count_as_the_launch_taken() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        log_line(
            dir.path(),
            "GameAction [AppID 1145360, ActionID 1] : LaunchApp failed with \"x\"",
        );
        helper.pass(t0 + Duration::from_secs(6), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2);
    }

    #[test]
    fn the_retry_waits_while_steam_needs_a_sign_in_and_resumes_when_it_is_done() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        assert_eq!(runs(dir.path(), 1), 1);
        // X is absent here, and an unreadable window list never clears a need: use a list.
        let mut watcher = helper.new_watcher(1145360, Box::new(Fake(vec![])));
        watcher.poll_at(t0);
        helper.watcher = Some(watcher);
        let at = |secs| t0 + Duration::from_secs(secs);
        std::fs::write(
            dir.path().join("logs/steamui_login.txt"),
            "SetLoginState: WaitingForCredentials\n",
        )
        .unwrap();
        helper.pass(at(6), &mut agent);
        helper.pass(at(200), &mut agent);
        assert_eq!(runs(dir.path(), 2), 1, "nothing is sent at a sign-in");
        assert!(helper.relaunch.is_some(), "and the retry is kept");
        std::fs::write(
            dir.path().join("logs/steamui_login.txt"),
            "SetLoginState: WaitingForCredentials\nSetLoginState: LoggedIn\n",
        )
        .unwrap();
        helper.pass(at(205), &mut agent);
        assert_eq!(
            runs(dir.path(), 2),
            2,
            "signed in: the launch is sent again"
        );
        // A sign-in that never ends does not hold the retry past its hard end.
        std::fs::write(
            dir.path().join("logs/steamui_login.txt"),
            "SetLoginState: WaitingForCredentials\nSetLoginState: LoggedIn\nSetLoginState: WaitingForCredentials\n",
        )
        .unwrap();
        helper.pass(at(210), &mut agent);
        helper.pass(at(RELAUNCH_MAX.as_secs() + 1), &mut agent);
        assert!(helper.relaunch.is_none());
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

    /// A `content_log.txt` line for a helper whose logs are in `dir`.
    fn content_line(dir: &std::path::Path, line: &str) {
        use std::io::Write;
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(dir.join("logs/content_log.txt"))
            .unwrap();
        writeln!(file, "{line}").unwrap();
    }

    /// #1314's failed launch, as Steam wrote it, for the game this helper launched: the launch
    /// moved into the shader download (`console_log.txt`), another app's update took priority
    /// and suspended it (`content_log.txt`). The other app's lines are kept because they are
    /// what suspended it.
    fn shader_download_suspended(dir: &std::path::Path) {
        log_line(
            dir,
            "[2026-11-11 18:58:06] GameAction [AppID 1145360, ActionID 1] : LaunchApp changed task to CheckShaderDepotManifest with \"\"",
        );
        log_line(
            dir,
            "[2026-11-11 18:58:07] GameAction [AppID 1145360, ActionID 1] : LaunchApp changed task to DownloadingDepots with \"\"",
        );
        for line in [
            "[2026-11-11 18:58:06] AppID 1145360 state changed : Fully Installed,Update Queued,Update Running,",
            "[2026-11-11 18:58:06] AppID 1145360 Shader update changed : Running Update,Reconfiguring,",
            "[2026-11-11 18:58:07] AppID 1145360 scheduler update : Priority First, not played for 150781 seconds, update disabled for 0 seconds",
            "[2026-11-11 18:58:07] AppID 1493710 scheduler update : Priority First, not played for 5695302 seconds, update disabled for 0 seconds",
            "[2026-11-11 18:58:07] AppID 1493710 state changed : Update Required,Fully Installed,Update Queued, (Update delayed for 465794 secs)",
            "[2026-11-11 18:58:07] AppID 1145360 Shader update changed : Running Update,Reconfiguring,Stopping,",
            "[2026-11-11 18:58:07] AppID 1145360 Shader update changed : Running Update,Stopping,",
            "[2026-11-11 18:58:07] AppID 1145360 Shader update changed : None",
            "[2026-11-11 18:58:07] AppID 1145360 state changed : Fully Installed,Update Queued,",
            "[2026-11-11 18:58:07] AppID 1145360 scheduler finished : staying in schedule (result Suspended, state 0xc) ",
        ] {
            content_line(dir, line);
        }
    }

    /// What followed on the same failed launch: the other app's update finished, the shader
    /// download completed, and Steam dropped the app from its schedule with nothing after it.
    fn schedule_dropped(dir: &std::path::Path) {
        for line in [
            "[2026-11-11 18:58:34] AppID 1493710 state changed : Fully Installed,",
            "[2026-11-11 18:58:34] AppID 1493710 scheduler finished : removed from schedule (result No Error, state 0xc) ",
            "[2026-11-11 18:58:35] AppID 1145360 state changed : Fully Installed,Update Queued,",
            "[2026-11-11 18:58:35] AppID 1145360 state changed : Fully Installed,",
            "[2026-11-11 18:58:35] AppID 1145360 scheduler finished : removed from schedule (result No Error, state 0xc) ",
        ] {
            content_line(dir, line);
        }
    }

    /// #1314: Steam took the launch up, another app's update suspended the shader download, and
    /// Steam then dropped the app from its schedule without ever running it. That launch request
    /// is lost, so the helper sends it again once the drop is in the content log.
    #[test]
    fn a_launch_dropped_with_its_shader_download_suspended_is_sent_again() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        assert_eq!(runs(dir.path(), 1), 1);
        shader_download_suspended(dir.path());
        helper.pass(at(2), &mut agent);
        assert_eq!(
            runs(dir.path(), 1),
            1,
            "a suspended update is not yet a dropped launch"
        );
        assert!(helper.relaunch.is_some(), "and the retry stays armed");
        schedule_dropped(dir.path());
        helper.pass(at(30), &mut agent);
        assert_eq!(
            runs(dir.path(), 1),
            1,
            "just dropped: Steam's next step may be a line away"
        );
        helper.pass(at(36), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "the lost launch is sent again");
    }

    /// #1314's successful launch of the same game, for comparison: the same shader task, then
    /// the whole launch through to the game running. Nothing of it is a lost launch, so the
    /// helper sends nothing again.
    #[test]
    fn a_normal_launch_of_the_same_game_is_not_sent_again() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        for text in [
            "changed task to CheckShaderDepotManifest with \"\"",
            "changed task to ProcessingInstallScript with \"\"",
            "changed task to SynchronizingCloud with \"\"",
            "changed task to SynchronizingStats with \"\"",
            "changed task to ShowInterstitials with \"\"",
            "waiting for user response to ShowInterstitials \"\"",
            "continues with user response \"ShowInterstitials\"",
            "changed task to SynchronizingControllerConfig with \"\"",
            "changed task to ProcessingShaderCache with \"\"",
            "waiting for user response to ProcessingShaderCache \"\"",
            "changed task to SiteLicenseSeatCheckout with \"\"",
            "changed task to DelayLaunch with \"\"",
            "changed task to CreatingProcess with \"\"",
            "waiting for user response to CreatingProcess \"\"",
            "continues with user response \"CreatingProcess\"",
            "changed task to WaitingGameWindow with \"\"",
            "changed task to Completed with \"\"",
        ] {
            log_line(
                dir.path(),
                &format!("[2026-11-11 19:34:15] GameAction [AppID 1145360, ActionID 1] : LaunchApp {text}"),
            );
        }
        content_line(
            dir.path(),
            "[2026-11-11 19:34:25] AppID 1145360 state changed : Fully Installed,App Running,",
        );
        helper.pass(at(1), &mut agent);
        helper.pass(at(6), &mut agent);
        helper.pass(at(30), &mut agent);
        assert_eq!(runs(dir.path(), 1), 1, "nothing is sent again");
    }

    /// #1314: the same dropped sequence, but the game is running. A launch of a game that is
    /// already up is never sent again, and the old outcome is kept.
    #[test]
    fn a_launch_of_a_game_already_running_is_not_sent_again() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        shader_download_suspended(dir.path());
        schedule_dropped(dir.path());
        log_line(
            dir.path(),
            "[2026-11-11 18:58:36] Game process added : AppID 1145360 \"reaper SteamLaunch AppId=1145360 -- game\"",
        );
        helper.pass(at(2), &mut agent);
        helper.pass(at(30), &mut agent);
        assert_eq!(runs(dir.path(), 1), 1);
        assert!(
            helper.relaunch.is_none(),
            "and there is nothing left to retry"
        );
    }

    /// #1314: the launch was dropped while Steam waits for the user (a sign-in). The retry must
    /// not run over the dialog; it goes once the need is gone.
    #[test]
    fn a_dropped_launch_waits_out_a_sign_in_before_it_is_sent_again() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        // X is absent here, and an unreadable window list never clears a need: use a list.
        let mut watcher = helper.new_watcher(1145360, Box::new(Fake(vec![])));
        watcher.poll_at(t0);
        helper.watcher = Some(watcher);
        let login =
            |text: &str| std::fs::write(dir.path().join("logs/steamui_login.txt"), text).unwrap();
        shader_download_suspended(dir.path());
        schedule_dropped(dir.path());
        login("SetLoginState: WaitingForCredentials\n");
        helper.pass(at(2), &mut agent);
        helper.pass(at(30), &mut agent);
        assert_eq!(runs(dir.path(), 1), 1, "nothing is sent at a sign-in");
        assert!(helper.relaunch.is_some(), "and the retry is kept");
        login("SetLoginState: WaitingForCredentials\nSetLoginState: LoggedIn\n");
        helper.pass(at(35), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "signed in: the lost launch is sent");
    }

    /// #1314: a launch that is dropped again after each retry is sent at most
    /// [`DROPPED_RETRIES`] times, and then nothing more: the reveal at the cap is the old
    /// outcome, unchanged.
    #[test]
    fn a_dropped_launch_is_sent_again_at_most_twice() {
        let _scripts = scripts();
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("logs")).unwrap();
        let (mut helper, mut agent, t0) = launched(dir.path());
        let at = |secs| t0 + Duration::from_secs(secs);
        shader_download_suspended(dir.path());
        schedule_dropped(dir.path());
        helper.pass(at(2), &mut agent);
        assert_eq!(runs(dir.path(), 1), 1, "not before it has stood a while");
        helper.pass(at(8), &mut agent);
        assert_eq!(runs(dir.path(), 2), 2, "the first retry");
        assert!(helper.relaunch.is_some(), "still armed while it stands");
        helper.pass(at(10), &mut agent);
        assert_eq!(
            runs(dir.path(), 2),
            2,
            "not again before it has stood a while"
        );
        helper.pass(at(14), &mut agent);
        assert_eq!(runs(dir.path(), 3), 3, "the second retry");
        helper.pass(at(20), &mut agent);
        helper.pass(at(60), &mut agent);
        assert_eq!(runs(dir.path(), 4), 3, "and no more than two");
        helper.pass(at(RELAUNCH_MAX.as_secs() + 1), &mut agent);
        assert!(helper.relaunch.is_none(), "the hard end is unchanged");
    }
}
