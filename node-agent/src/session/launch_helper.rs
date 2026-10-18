//! The agent's half of the in-container Steam launch helper (#731).
//!
//! For a derived Steam tile (`STEAM_STARTUP_FLAGS` carries `-applaunch <appid>`), and for a
//! parked Steam (amendment 39, #759), the app container gets the helper binary this agent
//! installed under its runtime dir and a per-session socket directory, both bind-mounted
//! read-only. The helper reports what the target game is doing; the agent puts the launch
//! state on `session_metrics.app_launch_state` and the `app.launch_state` trace
//! (`protocol/agent-api.md`). The agent writes only [`HelperCommand`] lines back.
//!
//! The helper shares its container with untrusted game code, so every byte on the socket
//! is hostile input. Lines over [`MAX_LINE`] bytes and anything that is not the wire shape
//! are dropped, at most [`MAX_LINES_PER_SEC`] lines a second are read (the rest wait in the
//! socket, so a flooding writer blocks), one connection is served at a time and dropped
//! after [`IDLE`] without a line, launch states only move forward, and nothing here can
//! end, fail or block the session.

use std::collections::{BTreeMap, VecDeque};
use std::io::{Read, Write};
use std::net::Shutdown;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

use crate::owned_entry::{self, OwnedMarker};
use crate::session::metrics::SessionMetrics;
use crate::session::runner::{DiagnosticEventTx, TraceEvent};

/// The helper's file name, beside the agent binary in the image and in the runtime dir.
const HELPER_FILE: &str = "quasar-launch-helper";
const CONTAINER_HELPER: &str = "/run/quasar-launch-helper";
const CONTAINER_SOCKET_DIR: &str = "/run/quasar-launch";
const SOCKET_FILE: &str = "launch.sock";
const PREFIX: &str = "launch-";

/// Wire cap on one line, newline included (#725 shared interface).
const MAX_LINE: usize = 1024;
/// The helper sends changes only, a handful per session.
const MAX_LINES_PER_SEC: u32 = 10;
/// Trace events and info lines one session's helper may cause; the metric keeps updating.
const MAX_NOTABLE: u32 = 32;
const POLL: Duration = Duration::from_millis(100);
/// A served connection with no complete line for this long is dropped, so a silent holder
/// cannot keep the helper out; the helper reconnects when its next line fails.
const IDLE: Duration = Duration::from_secs(1);
const LOG_INTERVAL: Duration = Duration::from_secs(10);
/// About twice the slowest launch the #723 spike measured (43.9 s).
const REVEAL_CAP_DEFAULT_SECS: u64 = 90;
/// Commands are serialised by the runner; more waiting than this is a helper not reading.
const MAX_OUTBOX: usize = 4;
const WRITE_TIMEOUT: Duration = Duration::from_secs(1);
/// The `STEAM_GAME` of Steam's own windows, Big Picture's included (#759 S0).
pub const STEAM_CLIENT_APPID: u32 = 769;

/// Copy the helper shipped beside the agent binary into the runtime dir, the one path the
/// host shares 1:1 with the agent's container, so sessions can bind-mount it. Any older
/// copy goes first: a session mounts the helper only when this agent put it there.
pub fn install() {
    let runtime_dir = PathBuf::from(crate::session::default_runtime_dir());
    let dest = runtime_dir.join(HELPER_FILE);
    let _ = std::fs::remove_file(&dest);
    let source = std::env::current_exe()
        .map(|exe| exe.with_file_name(HELPER_FILE))
        .ok()
        .filter(|path| path.is_file());
    let Some(source) = source else {
        tracing::info!(
            "no {HELPER_FILE} beside the agent binary; Steam game tiles get no launch helper"
        );
        return;
    };
    // Copy then rename: a running session's bind mount keeps the inode it was given.
    let tmp = runtime_dir.join(format!(".{HELPER_FILE}.tmp"));
    let result = std::fs::copy(&source, &tmp)
        .and_then(|_| std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o755)))
        .and_then(|()| std::fs::rename(&tmp, &dest));
    match result {
        Ok(()) => tracing::info!("launch helper installed at {}", dest.display()),
        Err(error) => {
            let _ = std::fs::remove_file(&tmp);
            tracing::warn!(
                token = "launch-helper-install-failed",
                %error,
                "could not install the launch helper at {}; Steam game tiles get none",
                dest.display()
            );
        }
    }
}

/// The target appid of a derived Steam tile: the integer after the single `-applaunch` in
/// `STEAM_STARTUP_FLAGS`, as the control plane's `composeSteamFlags` writes it. `None` for
/// anything else, which is every app that is not a derived Steam tile.
pub fn appid_from_env(env: &BTreeMap<String, String>) -> Option<u32> {
    let mut tokens = env.get("STEAM_STARTUP_FLAGS")?.split_ascii_whitespace();
    let mut appid = None;
    while let Some(token) = tokens.next() {
        if token == "-applaunch" {
            let value = tokens.next()?;
            // A bare positive integer, no sign or leading zero (`steamAppIDPattern`).
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

/// `session_metrics.app_launch_state` (`protocol/agent-api.md`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LaunchState {
    Starting,
    ClientOnly,
    GameRunning,
    GameExited,
}

impl LaunchState {
    fn as_str(self) -> &'static str {
        match self {
            LaunchState::Starting => "starting",
            LaunchState::ClientOnly => "client_only",
            LaunchState::GameRunning => "game_running",
            LaunchState::GameExited => "game_exited",
        }
    }

    fn parse(s: &str) -> Option<Self> {
        [
            LaunchState::Starting,
            LaunchState::ClientOnly,
            LaunchState::GameRunning,
            LaunchState::GameExited,
        ]
        .into_iter()
        .find(|state| state.as_str() == s)
    }
}

/// Why Steam is shown to the user on purpose.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NeedsUser {
    SignIn,
    SteamGuard,
    CloudConflict,
    Eula,
    Install,
    Other,
}

impl NeedsUser {
    fn as_str(self) -> &'static str {
        match self {
            NeedsUser::SignIn => "sign_in",
            NeedsUser::SteamGuard => "steam_guard",
            NeedsUser::CloudConflict => "cloud_conflict",
            NeedsUser::Eula => "eula",
            NeedsUser::Install => "install",
            NeedsUser::Other => "other",
        }
    }

    fn parse(s: &str) -> Option<Self> {
        [
            NeedsUser::SignIn,
            NeedsUser::SteamGuard,
            NeedsUser::CloudConflict,
            NeedsUser::Eula,
            NeedsUser::Install,
            NeedsUser::Other,
        ]
        .into_iter()
        .find(|reason| reason.as_str() == s)
    }
}

/// A command for the helper (#759): the closed set its reader parses, and nothing else.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HelperCommand {
    Launch(u32),
    BigPicture,
    Quit(u32),
}

impl HelperCommand {
    fn line(self) -> String {
        match self {
            HelperCommand::Launch(appid) => format!("launch {appid}\n"),
            HelperCommand::BigPicture => "bigpicture\n".to_string(),
            HelperCommand::Quit(appid) => format!("quit {appid}\n"),
        }
    }
}

/// The helper's word that it carried out a command. `cloud_synced` comes with a quit: Steam's
/// exit cloud sync for the game was seen to end.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CommandDone {
    pub command: HelperCommand,
    pub ok: bool,
    pub cloud_synced: Option<bool>,
}

/// One accepted helper line.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Report {
    LaunchState(LaunchState),
    GameWindow { on_screen: bool },
    SteamNeedsUser(NeedsUser),
    ShaderProgress(u8),
    CommandDone(CommandDone),
}

/// The wire, kept apart from `quasar-launch-watch`'s types: the agent links neither helper
/// crate. Twin of the helper's writer (`crates/quasar-launch-helper`).
#[derive(serde::Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
enum Wire {
    LaunchState {
        v: u32,
        state: String,
    },
    GameWindow {
        v: u32,
        on_screen: bool,
    },
    SteamNeedsUser {
        v: u32,
        reason: String,
    },
    ShaderProgress {
        v: u32,
        percent: u8,
    },
    CommandDone {
        v: u32,
        command: String,
        #[serde(default)]
        appid: Option<u32>,
        ok: bool,
        #[serde(default)]
        cloud_synced: Option<bool>,
    },
}

/// Why a line was dropped. Logged by name only: the line itself is the attacker's text.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Rejected {
    Oversize,
    NotUtf8,
    Malformed,
    UnknownVersion,
    UnknownValue,
    /// A launch state that does not move forward from the last one.
    OutOfOrder,
}

impl Rejected {
    fn as_str(self) -> &'static str {
        match self {
            Rejected::Oversize => "oversize",
            Rejected::NotUtf8 => "not_utf8",
            Rejected::Malformed => "malformed",
            Rejected::UnknownVersion => "unknown_version",
            Rejected::UnknownValue => "unknown_value",
            Rejected::OutOfOrder => "out_of_order",
        }
    }
}

/// Parse one line, its newline already stripped.
fn parse(line: &[u8]) -> Result<Report, Rejected> {
    let text = std::str::from_utf8(line).map_err(|_| Rejected::NotUtf8)?;
    let (v, report) = match serde_json::from_str(text).map_err(|_| Rejected::Malformed)? {
        Wire::LaunchState { v, state } => (v, LaunchState::parse(&state).map(Report::LaunchState)),
        Wire::GameWindow { v, on_screen } => (v, Some(Report::GameWindow { on_screen })),
        Wire::SteamNeedsUser { v, reason } => {
            (v, NeedsUser::parse(&reason).map(Report::SteamNeedsUser))
        }
        Wire::ShaderProgress { v, percent } => (
            v,
            (percent <= 100).then_some(Report::ShaderProgress(percent)),
        ),
        Wire::CommandDone {
            v,
            command,
            appid,
            ok,
            cloud_synced,
        } => {
            let command = match (command.as_str(), appid, cloud_synced) {
                ("launch", Some(appid), None) => Some(HelperCommand::Launch(appid)),
                ("bigpicture", None, None) => Some(HelperCommand::BigPicture),
                ("quit", Some(appid), Some(_)) => Some(HelperCommand::Quit(appid)),
                _ => None,
            };
            let done = command.map(|command| CommandDone {
                command,
                ok,
                cloud_synced,
            });
            (v, done.map(Report::CommandDone))
        }
    };
    if v != 1 {
        return Err(Rejected::UnknownVersion);
    }
    report.ok_or(Rejected::UnknownValue)
}

/// The latest of each report the helper has made this session. A hint, never an
/// authority: what the reveal (#727) and game-exit (#726) work reads. Any process in the
/// app container runs as the helper's user and can write a well-formed line or replace the
/// helper; the bounds above limit what it can make the agent believe, nothing more.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LaunchReports {
    pub launch_state: Option<LaunchState>,
    pub game_window_on_screen: Option<bool>,
    /// The last reason reported; the wire has no "cleared".
    pub needs_user: Option<NeedsUser>,
    /// A `game_running` was taken since the reader was last armed, which is what makes
    /// a later `game_exited` legal.
    pub seen_running: bool,
    /// The helper connected since the reader was last armed. An image whose launcher does
    /// not start it never does, and its first frame is shown as before (#727).
    pub helper_connected: bool,
    /// Steam's latest shader percentage for the target (#759).
    pub shader_percent: Option<u8>,
    /// The helper's word on the last command sent, cleared when the next one is sent.
    pub last_command: Option<CommandDone>,
}

/// `QUASAR_STEAM_REVEAL_CAP_SECS`, no higher than the app boot watchdog's budget, or than
/// that budget's default when the watchdog is off.
pub fn reveal_cap(raw: Option<String>, boot_budget: Option<Duration>) -> Duration {
    let secs = match raw {
        Some(v) => v.trim().parse::<u64>().unwrap_or_else(|_| {
            tracing::warn!(
                token = "knob-invalid-steam-reveal-cap",
                "QUASAR_STEAM_REVEAL_CAP_SECS={v:?} is not a number; \
                 using the default {REVEAL_CAP_DEFAULT_SECS}s"
            );
            REVEAL_CAP_DEFAULT_SECS
        }),
        None => REVEAL_CAP_DEFAULT_SECS,
    };
    let ceiling = boot_budget.unwrap_or(Duration::from_secs(
        crate::session::settings::APP_BOOT_TIMEOUT_DEFAULT_SECS,
    ));
    let cap = Duration::from_secs(secs);
    if cap > ceiling {
        let limit = match boot_budget {
            Some(_) => format!("the app boot watchdog's {}s", ceiling.as_secs()),
            None => format!(
                "the {}s ceiling (the app boot watchdog is off)",
                ceiling.as_secs()
            ),
        };
        tracing::warn!(
            token = "knob-steam-reveal-cap-clamped",
            "QUASAR_STEAM_REVEAL_CAP_SECS={secs} is above {limit}; using that"
        );
        return ceiling;
    }
    cap
}

impl LaunchReports {
    /// Why a held first frame may be shown now (#727): the game is on screen, or Steam needs
    /// the user. A hint: a false one reveals the session's own stream early, nothing more.
    pub fn reveal_reason(&self) -> Option<&'static str> {
        if self.game_window_on_screen == Some(true) {
            Some("game window on screen")
        } else if self.needs_user.is_some() {
            Some("Steam needs the user")
        } else {
            None
        }
    }

    /// The game ran and has now exited (#726). `seen_running` is checked here as well as
    /// in `forward`: the reports are untrusted hints, and the end of a session is not
    /// something to take on a single field.
    pub fn game_exited(&self) -> bool {
        self.seen_running && self.launch_state == Some(LaunchState::GameExited)
    }
}

/// Launch states move forward (#725 amendment 2): `game_exited` only once `game_running`
/// was seen since the reader was armed, nothing after it. `starting` is taken from
/// anything but `game_exited` because a new helper process (the launcher restarts a dead
/// one) truthfully starts there; `game_running` from nothing is a reconnect's replay.
fn forward(from: Option<LaunchState>, to: LaunchState, seen_running: bool) -> bool {
    use LaunchState::*;
    match (from, to) {
        (Some(GameExited), _) => false,
        (_, Starting) => true,
        (None | Some(Starting), ClientOnly) => true,
        (None | Some(Starting | ClientOnly), GameRunning) => true,
        (_, GameExited) => seen_running,
        _ => false,
    }
}

/// `active` and the reports share one lock, so a report cannot land after `set_active(false)`.
#[derive(Default)]
struct State {
    /// False while the session's app is not its derived tile (after a swap away).
    active: bool,
    /// The appid the helper watches; `None` in watch-only mode, which takes no game report.
    target: Option<u32>,
    reports: LaunchReports,
    /// Command lines waiting for the helper's connection.
    outbox: VecDeque<String>,
}

/// The reader's state, shared with every generation's mount.
struct Shared {
    state: Mutex<State>,
    metrics: Arc<SessionMetrics>,
}

impl Shared {
    fn state(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// What a generation's app container is given when it runs the app the socket was opened for.
#[derive(Clone)]
pub struct LaunchMount {
    /// The tile's appid, or `None` for a parked Steam, opened on these flags.
    opened_for: Option<u32>,
    opened_flags: Option<String>,
    helper: PathBuf,
    socket_dir: PathBuf,
    shared: Arc<Shared>,
}

impl LaunchMount {
    /// The mount for a generation that runs `env`: the app the socket was opened for gets it
    /// and re-arms the reader on what its flags launch. Any other app gets none, the reports
    /// go back to unknown and the reader drops lines (a dead helper's may still be queued)
    /// until that app is back. The socket stays, so a rollback finds its bind source.
    pub fn for_app(&self, env: &BTreeMap<String, String>) -> Option<&Self> {
        let ours = match self.opened_for {
            Some(appid) => appid_from_env(env) == Some(appid),
            None => env.get("STEAM_STARTUP_FLAGS") == self.opened_flags.as_ref(),
        };
        if ours {
            self.arm(self.opened_for);
        } else {
            self.set_active(false);
        }
        ours.then_some(self)
    }

    /// Follow `target` from now on (`None`: watch-only), with the reports back to unknown:
    /// a launch, an adoption or an in-place swap starts a new game in the same container.
    pub fn arm(&self, target: Option<u32>) {
        let mut state = self.shared.state();
        state.active = true;
        state.target = target;
        state.reports = LaunchReports {
            helper_connected: state.reports.helper_connected,
            ..LaunchReports::default()
        };
        self.shared.metrics.set_app_launch_state(None);
    }

    /// The appid the reports are about, if any.
    pub fn target(&self) -> Option<u32> {
        self.shared.state().target
    }

    /// Queue `command` for the helper. Its last result is cleared, so the next one read is
    /// this command's (or a replay the caller tells apart by the command it names).
    pub fn send(&self, command: HelperCommand) {
        let mut state = self.shared.state();
        state.reports.last_command = None;
        if state.outbox.len() >= MAX_OUTBOX {
            state.outbox.pop_front();
            tracing::warn!(
                token = "launch-helper-outbox-full",
                "the launch helper is not reading its commands; dropped the oldest"
            );
        }
        state.outbox.push_back(command.line());
    }

    /// The session's app is about to be stopped by a swap: stop listening before the game
    /// closes under the helper, so that exit is not read as the player quitting (#726). A
    /// rollback to the tile re-arms through `for_app`.
    pub fn mute(&self) {
        self.set_active(false);
    }

    fn set_active(&self, active: bool) {
        let mut state = self.shared.state();
        state.active = active;
        if !active {
            state.reports = LaunchReports::default();
            state.outbox.clear();
            self.shared.metrics.set_app_launch_state(None);
        }
    }

    /// Container args, in the bind shape `container::application_request_from_args` takes.
    pub fn args(&self) -> Vec<String> {
        vec![
            "--mount".into(),
            format!(
                "type=bind,src={},dst={CONTAINER_HELPER},readonly",
                self.helper.display()
            ),
            "--mount".into(),
            format!(
                "type=bind,src={},dst={CONTAINER_SOCKET_DIR},readonly",
                self.socket_dir.display()
            ),
            "-e".into(),
            format!("QUASAR_LAUNCH_HELPER={CONTAINER_HELPER}"),
            "-e".into(),
            format!("QUASAR_LAUNCH_SOCKET={CONTAINER_SOCKET_DIR}/{SOCKET_FILE}"),
        ]
    }
}

#[derive(serde::Serialize, serde::Deserialize)]
struct Marker {
    owner: String,
    session: String,
}

impl OwnedMarker for Marker {
    fn owner(&self) -> &str {
        &self.owner
    }
}

fn malformed_id(id: &str) -> bool {
    crate::session::udev_export::malformed_session_id(id)
}

/// Where a session's helper socket is, for a test that plays the helper.
#[cfg(test)]
pub(crate) fn socket_path(runtime_dir: &str, session_id: &str) -> PathBuf {
    owned_entry::entry_dir(runtime_dir, PREFIX, session_id).join(SOCKET_FILE)
}

/// Boot-only: retire every socket directory this owner left behind. Same rule as
/// `udev_export::retire_all_owned`: only where "ours" implies "dead".
pub fn retire_all_owned(runtime_dir: &str, owner: &str) -> owned_entry::Summary {
    owned_entry::retire_all_owned::<Marker>(runtime_dir, PREFIX, owner, malformed_id)
}

/// A session's listening socket. Dropping it stops the reader and removes the directory.
pub struct LaunchSocket {
    mount: LaunchMount,
    stop: Arc<AtomicBool>,
    runtime_dir: String,
    session_id: String,
}

impl LaunchSocket {
    /// Listen for this session's helper. `None`, and the session launches as it would
    /// without one, unless the first app is a derived Steam tile, or `parked` (a warm Steam,
    /// whose helper starts watch-only), and this agent installed the helper; a setup error is
    /// logged and also gives `None`.
    pub fn open(
        runtime_dir: &str,
        session_id: &str,
        env: Option<&BTreeMap<String, String>>,
        parked: bool,
        metrics: Arc<SessionMetrics>,
        trace: DiagnosticEventTx,
    ) -> Option<Self> {
        let env = env?;
        let appid = appid_from_env(env);
        if appid.is_none() && !parked {
            return None;
        }
        let what = appid.map_or_else(|| "the parked Steam".to_string(), |id| format!("app {id}"));
        let helper = Path::new(runtime_dir).join(HELPER_FILE);
        if !helper.is_file() {
            return None;
        }
        let owner = match crate::container_ownership::token() {
            Ok(owner) => owner,
            Err(error) => {
                tracing::warn!(token = "launch-helper-no-owner", %error,
                    "no owner token; {what} launches without a launch helper");
                return None;
            }
        };
        match Self::open_in(
            runtime_dir,
            session_id,
            &owner,
            (appid, env.get("STEAM_STARTUP_FLAGS").cloned()),
            helper,
            metrics,
            trace,
        ) {
            Ok(socket) => Some(socket),
            Err(error) => {
                tracing::warn!(token = "launch-helper-socket-failed",
                    error = %format_args!("{error:#}"),
                    "{what} launches without a launch helper");
                None
            }
        }
    }

    fn open_in(
        runtime_dir: &str,
        session_id: &str,
        owner: &str,
        (appid, flags): (Option<u32>, Option<String>),
        helper: PathBuf,
        metrics: Arc<SessionMetrics>,
        trace: DiagnosticEventTx,
    ) -> anyhow::Result<Self> {
        anyhow::ensure!(!malformed_id(session_id), "malformed session id");
        owned_entry::write_marker(
            &owned_entry::marker_path(runtime_dir, PREFIX, session_id),
            &Marker {
                owner: owner.to_string(),
                session: session_id.to_string(),
            },
        )?;
        let socket = LaunchSocket {
            mount: LaunchMount {
                opened_for: appid,
                opened_flags: flags,
                helper,
                socket_dir: owned_entry::entry_dir(runtime_dir, PREFIX, session_id),
                shared: Arc::new(Shared {
                    state: Mutex::new(State {
                        active: true,
                        target: appid,
                        ..State::default()
                    }),
                    metrics,
                }),
            },
            stop: Arc::default(),
            runtime_dir: runtime_dir.to_string(),
            session_id: session_id.to_string(),
        };
        // From here a failure drops `socket`, which retires the marker and directory.
        let dir = &socket.mount.socket_dir;
        std::fs::create_dir(dir)?;
        // The app user must reach the socket; the directory is this session's alone and
        // is mounted read-only, so it cannot be replaced from inside.
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755))?;
        let path = dir.join(SOCKET_FILE);
        let listener = UnixListener::bind(&path)?;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o666))?;
        listener.set_nonblocking(true)?;
        let mut reader = Reader {
            stop: socket.stop.clone(),
            shared: socket.mount.shared.clone(),
            session_id: session_id.to_string(),
            trace,
            window: (Instant::now(), 0),
            notable_left: MAX_NOTABLE,
            dropped: Limiter::default(),
            held: Limiter::default(),
            idle: IDLE,
        };
        let log_span = tracing::Span::current();
        std::thread::Builder::new()
            .name("quasar-launch".into())
            .spawn(move || {
                let _log_span = log_span.enter();
                reader.serve(&listener);
            })?;
        match appid {
            Some(appid) => tracing::info!(
                appid,
                "listening for the launch helper of Steam app {appid}"
            ),
            None => tracing::info!("listening for the launch helper of the parked Steam"),
        }
        Ok(socket)
    }

    pub fn mount(&self) -> &LaunchMount {
        &self.mount
    }

    pub fn reports(&self) -> LaunchReports {
        self.mount.shared.state().reports
    }
}

impl Drop for LaunchSocket {
    fn drop(&mut self) {
        // The reader notices within `POLL`; it holds no path, so it is not waited for.
        self.stop.store(true, Ordering::Relaxed);
        if let Err(error) = owned_entry::retire_entry(&self.runtime_dir, PREFIX, &self.session_id) {
            tracing::warn!(token = "launch-helper-socket-retire-failed",
                error = %format_args!("{error:#}"),
                "launch helper socket directory left for the boot sweep");
        }
    }
}

/// At most one log line per [`LOG_INTERVAL`], counting the ones it held back.
#[derive(Default)]
struct Limiter {
    last: Option<Instant>,
    suppressed: u32,
}

impl Limiter {
    fn ready(&mut self) -> Option<u32> {
        if self.last.is_some_and(|last| last.elapsed() < LOG_INTERVAL) {
            self.suppressed = self.suppressed.saturating_add(1);
            return None;
        }
        self.last = Some(Instant::now());
        Some(std::mem::take(&mut self.suppressed))
    }
}

struct Reader {
    stop: Arc<AtomicBool>,
    shared: Arc<Shared>,
    session_id: String,
    trace: DiagnosticEventTx,
    /// Start of the current one-second window and the lines read in it.
    window: (Instant, u32),
    notable_left: u32,
    dropped: Limiter,
    held: Limiter,
    /// [`IDLE`]; a field so a test can close a connection at once.
    idle: Duration,
}

impl Reader {
    fn stopped(&self) -> bool {
        self.stop.load(Ordering::Relaxed)
    }

    fn serve(&mut self, listener: &UnixListener) {
        while !self.stopped() {
            match listener.accept() {
                Ok((stream, _)) => {
                    let mut state = self.shared.state();
                    if state.active {
                        state.reports.helper_connected = true;
                    }
                    drop(state);
                    self.read_connection(stream)
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => std::thread::sleep(POLL),
                Err(error) => {
                    if let Some(suppressed) = self.dropped.ready() {
                        tracing::warn!(token = "launch-helper-accept-failed", %error, suppressed,
                            "launch helper socket accept failed");
                    }
                    std::thread::sleep(POLL);
                }
            }
        }
    }

    /// One connection until it closes. Every complete line, and every [`MAX_LINE`] bytes
    /// of an oversize one, spends one unit of the per-second budget.
    fn read_connection(&mut self, mut stream: UnixStream) {
        if stream.set_nonblocking(false).is_err()
            || stream.set_read_timeout(Some(POLL)).is_err()
            || stream.set_write_timeout(Some(WRITE_TIMEOUT)).is_err()
        {
            return;
        }
        let mut buf = [0u8; MAX_LINE];
        let mut len = 0;
        // Inside a line already dropped as oversize, up to its newline.
        let mut skipping = false;
        let mut last_line = Instant::now();
        let mut closing = false;
        while !self.stopped() {
            if !closing && last_line.elapsed() >= self.idle {
                // Shut reading, then drain to EOF: a line queued before this is still read,
                // and a write after it fails, so the helper reconnects and replays it.
                if stream.shutdown(Shutdown::Read).is_err() {
                    return;
                }
                tracing::debug!(
                    "launch helper connection quiet for {:?}; closing",
                    self.idle
                );
                closing = true;
            }
            if !closing && !self.write_commands(&mut stream) {
                return;
            }
            match stream.read(&mut buf[len..]) {
                Ok(0) => return,
                Ok(n) => len += n,
                Err(e)
                    if matches!(
                        e.kind(),
                        std::io::ErrorKind::WouldBlock
                            | std::io::ErrorKind::TimedOut
                            | std::io::ErrorKind::Interrupted
                    ) =>
                {
                    continue
                }
                Err(_) => return,
            }
            let mut start = 0;
            while let Some(end) = buf[start..len].iter().position(|&b| b == b'\n') {
                let line = start..start + end;
                start += end + 1;
                last_line = Instant::now();
                if std::mem::take(&mut skipping) {
                    continue;
                }
                if !self.spend() {
                    return;
                }
                // The helper's keepalive.
                if line.is_empty() {
                    continue;
                }
                match parse(&buf[line]) {
                    Ok(report) => self.apply(report),
                    Err(reason) => self.drop_line(reason),
                }
            }
            buf.copy_within(start..len, 0);
            len -= start;
            if len == MAX_LINE {
                if !skipping {
                    self.drop_line(Rejected::Oversize);
                }
                skipping = true;
                len = 0;
                if !self.spend() {
                    return;
                }
            }
        }
    }

    /// Write every queued command. `false` when the connection is gone; the command that
    /// failed goes back to the front, for the helper's next connection.
    fn write_commands(&mut self, stream: &mut UnixStream) -> bool {
        loop {
            let Some(line) = self.shared.state().outbox.pop_front() else {
                return true;
            };
            if stream.write_all(line.as_bytes()).is_err() {
                self.shared.state().outbox.push_front(line);
                return false;
            }
            tracing::info!(
                command = line.trim_end(),
                "sent the launch helper `{}`",
                line.trim_end()
            );
        }
    }

    /// Take one line of this second's budget, waiting for the next second when it is
    /// spent. `false` when the session stopped meanwhile.
    fn spend(&mut self) -> bool {
        loop {
            if self.stopped() {
                return false;
            }
            if self.window.0.elapsed() >= Duration::from_secs(1) {
                self.window = (Instant::now(), 0);
            }
            if self.window.1 < MAX_LINES_PER_SEC {
                self.window.1 += 1;
                return true;
            }
            if let Some(suppressed) = self.held.ready() {
                tracing::warn!(token = "launch-helper-flooding", suppressed,
                    "launch helper is sending over {MAX_LINES_PER_SEC} lines a second; reading is held back");
            }
            std::thread::sleep(POLL);
        }
    }

    fn drop_line(&mut self, reason: Rejected) {
        if let Some(suppressed) = self.dropped.ready() {
            tracing::warn!(token = "launch-helper-line-dropped", reason = reason.as_str(), suppressed,
                "dropped a launch helper line ({}); {suppressed} more held back since the last report",
                reason.as_str());
        }
    }

    /// One trace event or info line, while this session's allowance lasts.
    fn notable(&mut self) -> bool {
        if self.notable_left == 0 {
            return false;
        }
        self.notable_left -= 1;
        if self.notable_left == 0 {
            tracing::warn!(
                token = "launch-helper-chatty",
                "launch helper made {MAX_NOTABLE} reports; later ones update the metric only"
            );
        }
        true
    }

    fn apply(&mut self, report: Report) {
        let mut guard = self.shared.state();
        if !guard.active {
            return;
        }
        let Some(appid) = guard.target.or_else(|| {
            // Watch-only: a command's result is all there is to take.
            matches!(report, Report::CommandDone(_)).then_some(0)
        }) else {
            return;
        };
        let reports = &mut guard.reports;
        match report {
            Report::LaunchState(state) if reports.launch_state != Some(state) => {
                if !forward(reports.launch_state, state, reports.seen_running) {
                    drop(guard);
                    return self.drop_line(Rejected::OutOfOrder);
                }
                reports.launch_state = Some(state);
                reports.seen_running |= state == LaunchState::GameRunning;
                // Under the state lock, so a swap's reset cannot land in between.
                self.shared
                    .metrics
                    .set_app_launch_state(Some(state.as_str()));
                drop(guard);
                if self.notable() {
                    tracing::info!(
                        state = state.as_str(),
                        appid,
                        "Steam app {appid} is {}",
                        state.as_str()
                    );
                    self.trace.try_emit(
                        self.session_id.clone(),
                        TraceEvent {
                            ts_unix_ms: crate::session::runner::now_unix_ms(),
                            event: "app.launch_state",
                            payload: serde_json::json!({
                                "state": state.as_str(),
                                "appid": appid.to_string(),
                            }),
                        },
                    );
                }
            }
            Report::GameWindow { on_screen }
                if reports.game_window_on_screen != Some(on_screen) =>
            {
                reports.game_window_on_screen = Some(on_screen);
                tracing::debug!(on_screen, "launch helper: game window on screen");
            }
            Report::SteamNeedsUser(reason) if reports.needs_user != Some(reason) => {
                reports.needs_user = Some(reason);
                drop(guard);
                if self.notable() {
                    tracing::info!(
                        reason = reason.as_str(),
                        "launch helper: Steam needs the user"
                    );
                }
            }
            Report::ShaderProgress(percent) if reports.shader_percent != Some(percent) => {
                reports.shader_percent = Some(percent);
                tracing::debug!(percent, "launch helper: Steam is processing shaders");
            }
            Report::CommandDone(done) => {
                reports.last_command = Some(done);
                drop(guard);
                tracing::info!(
                    ok = done.ok,
                    cloud_synced = ?done.cloud_synced,
                    "launch helper: {:?} done (ok={})",
                    done.command,
                    done.ok
                );
            }
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::sync::atomic::AtomicU64;

    fn env(flags: &str) -> BTreeMap<String, String> {
        BTreeMap::from([("STEAM_STARTUP_FLAGS".to_string(), flags.to_string())])
    }

    #[test]
    fn the_appid_is_the_one_integer_after_applaunch() {
        assert_eq!(
            appid_from_env(&env("-bigpicture -applaunch 620")),
            Some(620)
        );
        assert_eq!(appid_from_env(&env("-applaunch 620 -silent")), Some(620));
        assert_eq!(appid_from_env(&env("  -applaunch\t4000  ")), Some(4000));
        for flags in [
            "",
            "-bigpicture",
            "-applaunch",
            "-applaunch -silent",
            "-applaunch 0",
            "-applaunch 0620",
            "-applaunch +620",
            "-applaunch -620",
            "-applaunch 62o",
            "-applaunch 620x",
            "-applaunch 4294967296",
            "-applaunch 620 -applaunch 620",
            "-applaunch 620 -applaunch 730",
            "-applaunch=620",
        ] {
            assert_eq!(appid_from_env(&env(flags)), None, "{flags:?}");
        }
        assert_eq!(appid_from_env(&BTreeMap::new()), None);
    }

    #[test]
    fn only_the_wire_shape_parses() {
        assert_eq!(
            parse(br#"{"type":"launch_state","v":1,"state":"client_only"}"#),
            Ok(Report::LaunchState(LaunchState::ClientOnly))
        );
        assert_eq!(
            parse(br#"{"type":"game_window","v":1,"on_screen":true}"#),
            Ok(Report::GameWindow { on_screen: true })
        );
        assert_eq!(
            parse(br#" {"v":1,"reason":"sign_in","type":"steam_needs_user"} "#),
            Ok(Report::SteamNeedsUser(NeedsUser::SignIn))
        );
        for (line, why) in [
            (&b"\xff\xfe"[..], Rejected::NotUtf8),
            (b"", Rejected::Malformed),
            (b"not json", Rejected::Malformed),
            (b"[]", Rejected::Malformed),
            (br#"{"type":"launch_state","v":1}"#, Rejected::Malformed),
            (
                br#"{"type":"launch","v":1,"state":"starting"}"#,
                Rejected::Malformed,
            ),
            (br#"{"v":1,"state":"starting"}"#, Rejected::Malformed),
            (
                br#"{"type":"launch_state","v":1,"state":"starting","x":1}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"launch_state","v":1,"v":1,"state":"starting"}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"launch_state","v":"1","state":"starting"}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"launch_state","v":1.0,"state":"starting"}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"game_window","v":1,"on_screen":1}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"launch_state","v":1,"state":"starting"} {}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"launch_state","v":2,"state":"starting"}"#,
                Rejected::UnknownVersion,
            ),
            (
                br#"{"type":"launch_state","v":1,"state":"Starting"}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"steam_needs_user","v":1,"reason":"x"}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"shader_progress","v":1,"percent":101}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"shader_progress","v":1,"percent":-1}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"shader_progress","v":1,"percent":4.5}"#,
                Rejected::Malformed,
            ),
            (
                br#"{"type":"command_done","v":1,"command":"launch","ok":true}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"command_done","v":1,"command":"bigpicture","appid":620,"ok":true}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"command_done","v":1,"command":"quit","appid":620,"ok":true}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"command_done","v":1,"command":"exec","appid":620,"ok":true}"#,
                Rejected::UnknownValue,
            ),
            (
                br#"{"type":"command_done","v":1,"command":"launch","appid":620,"ok":"yes"}"#,
                Rejected::Malformed,
            ),
        ] {
            assert_eq!(parse(line), Err(why), "{}", String::from_utf8_lossy(line));
        }
    }

    #[test]
    fn the_warm_steam_lines_parse() {
        assert_eq!(
            parse(br#"{"type":"shader_progress","v":1,"percent":42}"#),
            Ok(Report::ShaderProgress(42))
        );
        assert_eq!(
            parse(br#"{"type":"command_done","v":1,"command":"launch","appid":620,"ok":true}"#),
            Ok(Report::CommandDone(CommandDone {
                command: HelperCommand::Launch(620),
                ok: true,
                cloud_synced: None,
            }))
        );
        assert_eq!(
            parse(br#"{"type":"command_done","v":1,"command":"bigpicture","ok":false}"#),
            Ok(Report::CommandDone(CommandDone {
                command: HelperCommand::BigPicture,
                ok: false,
                cloud_synced: None,
            }))
        );
        assert_eq!(
            parse(br#"{"type":"command_done","v":1,"command":"quit","appid":620,"ok":true,"cloud_synced":false}"#),
            Ok(Report::CommandDone(CommandDone {
                command: HelperCommand::Quit(620),
                ok: true,
                cloud_synced: Some(false),
            }))
        );
        assert_eq!(HelperCommand::Launch(620).line(), "launch 620\n");
        assert_eq!(HelperCommand::BigPicture.line(), "bigpicture\n");
        assert_eq!(HelperCommand::Quit(4294967295).line(), "quit 4294967295\n");
    }

    #[test]
    fn the_mount_fits_the_runtime_bind_shape() {
        let mount = LaunchMount {
            opened_for: Some(620),
            opened_flags: Some("-applaunch 620".into()),
            helper: "/run/quasar-agent/quasar-launch-helper".into(),
            socket_dir: "/run/quasar-agent/launch-s1".into(),
            shared: Arc::new(Shared {
                state: Mutex::new(State {
                    active: true,
                    ..State::default()
                }),
                metrics: Arc::new(SessionMetrics::new("off", 60)),
            }),
        };
        let mut args: Vec<String> = [
            "run",
            "--name",
            "quasar-sess-x",
            "--network",
            "none",
            "--pids-limit",
            "8192",
            "--shm-size",
            "1g",
        ]
        .map(String::from)
        .to_vec();
        args.extend(mount.args());
        args.push("image".into());
        let request = crate::session::container::application_request_for_test(&args);
        assert_eq!(
            request.typed_mounts,
            vec![
                crate::runtime::ApplicationMount::Bind {
                    source: "/run/quasar-agent/quasar-launch-helper".into(),
                    target: CONTAINER_HELPER.into(),
                    read_only: true,
                    consistency: None,
                },
                crate::runtime::ApplicationMount::Bind {
                    source: "/run/quasar-agent/launch-s1".into(),
                    target: CONTAINER_SOCKET_DIR.into(),
                    read_only: true,
                    consistency: None,
                },
            ]
        );
        assert_eq!(
            request.environment,
            [
                "QUASAR_LAUNCH_HELPER=/run/quasar-launch-helper",
                "QUASAR_LAUNCH_SOCKET=/run/quasar-launch/launch.sock",
            ]
        );
    }

    struct Session {
        // First, so it drops before the directory it lives in.
        socket: Option<LaunchSocket>,
        runtime_dir: String,
        metrics: Arc<SessionMetrics>,
        trace_rx: tokio::sync::mpsc::Receiver<(String, TraceEvent)>,
        _dir: tempfile::TempDir,
    }

    impl Session {
        fn open(appid: u32) -> Self {
            Self::open_for(Some(appid), &format!("-applaunch {appid}"))
        }

        /// A parked Steam's socket: watch-only until armed.
        fn open_parked() -> Self {
            Self::open_for(None, "-silent")
        }

        fn open_for(appid: Option<u32>, flags: &str) -> Self {
            let dir = tempfile::tempdir().unwrap();
            let runtime_dir = dir.path().to_str().unwrap().to_string();
            let metrics = Arc::new(SessionMetrics::new("off", 60));
            let (tx, trace_rx) = tokio::sync::mpsc::channel(128);
            let trace = DiagnosticEventTx::new(
                tx,
                Arc::new(AtomicU64::new(0)),
                Arc::new(AtomicU64::new(0)),
            );
            let socket = LaunchSocket::open_in(
                &runtime_dir,
                "s1",
                "owner-a",
                (appid, Some(flags.to_string())),
                dir.path().join(HELPER_FILE),
                metrics.clone(),
                trace,
            )
            .unwrap();
            Session {
                socket: Some(socket),
                runtime_dir,
                metrics,
                trace_rx,
                _dir: dir,
            }
        }

        fn connect(&self) -> UnixStream {
            UnixStream::connect(
                owned_entry::entry_dir(&self.runtime_dir, PREFIX, "s1").join(SOCKET_FILE),
            )
            .unwrap()
        }

        fn socket(&self) -> &LaunchSocket {
            self.socket.as_ref().unwrap()
        }

        /// The `session_metrics` value the next heartbeat would send.
        fn wire_state(&self) -> Option<String> {
            let window = self.metrics.drain_window(Instant::now());
            let msg = crate::messages::AgentMsg::session_metrics("s1".into(), 0, &window, None);
            let v = serde_json::to_value(msg).unwrap();
            v.get("app_launch_state")
                .map(|s| s.as_str().unwrap().to_string())
        }

        fn traces(&mut self) -> Vec<serde_json::Value> {
            let mut out = Vec::new();
            while let Ok((sid, event)) = self.trace_rx.try_recv() {
                assert_eq!((sid.as_str(), event.event), ("s1", "app.launch_state"));
                out.push(event.payload);
            }
            out
        }

        fn wait_for(&self, what: impl Fn(&LaunchReports) -> bool) {
            let deadline = Instant::now() + Duration::from_secs(5);
            while !what(&self.socket().reports()) {
                assert!(Instant::now() < deadline, "{:?}", self.socket().reports());
                std::thread::sleep(Duration::from_millis(10));
            }
        }
    }

    const CLIENT_ONLY: &[u8] = b"{\"type\":\"launch_state\",\"v\":1,\"state\":\"client_only\"}\n";
    const STARTING: &[u8] = b"{\"type\":\"launch_state\",\"v\":1,\"state\":\"starting\"}\n";
    const GAME_WINDOW: &[u8] = b"{\"type\":\"game_window\",\"v\":1,\"on_screen\":true}\n";

    #[test]
    fn a_fake_helper_moves_the_metric_and_the_trace_through_the_four_states() {
        let mut s = Session::open(620);
        assert_eq!(s.wire_state(), None, "absent until the helper reports");
        let mut helper = s.connect();
        for state in ["starting", "client_only", "client_only", "game_running"] {
            writeln!(
                helper,
                r#"{{"type":"launch_state","v":1,"state":"{state}"}}"#
            )
            .unwrap();
        }
        writeln!(helper, r#"{{"type":"game_window","v":1,"on_screen":true}}"#).unwrap();
        writeln!(
            helper,
            r#"{{"type":"steam_needs_user","v":1,"reason":"cloud_conflict"}}"#
        )
        .unwrap();
        s.wait_for(|r| r.needs_user.is_some());
        assert_eq!(
            s.socket().reports(),
            LaunchReports {
                launch_state: Some(LaunchState::GameRunning),
                game_window_on_screen: Some(true),
                needs_user: Some(NeedsUser::CloudConflict),
                seen_running: true,
                helper_connected: true,
                ..LaunchReports::default()
            }
        );
        assert_eq!(s.wire_state().as_deref(), Some("game_running"));
        assert_eq!(
            s.wire_state().as_deref(),
            Some("game_running"),
            "every window once set"
        );

        // A dropped connection keeps the state; the helper's reconnect carries on.
        drop(helper);
        let mut helper = s.connect();
        writeln!(
            helper,
            r#"{{"type":"launch_state","v":1,"state":"game_exited"}}"#
        )
        .unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::GameExited));
        assert_eq!(s.wire_state().as_deref(), Some("game_exited"));
        let states: Vec<_> = s.traces().iter().map(|p| p["state"].clone()).collect();
        assert_eq!(
            states,
            ["starting", "client_only", "game_running", "game_exited"],
            "one event per transition, the repeat dropped"
        );
    }

    #[test]
    fn the_trace_payload_is_the_contract_shape() {
        let mut s = Session::open(620);
        s.connect().write_all(CLIENT_ONLY).unwrap();
        s.wait_for(|r| r.launch_state.is_some());
        assert_eq!(
            s.traces(),
            [serde_json::json!({"state": "client_only", "appid": "620"})]
        );
    }

    #[test]
    fn garbage_changes_nothing_and_a_good_line_after_it_still_lands() {
        let mut s = Session::open(620);
        let mut helper = s.connect();
        helper.write_all(b"\xff\xfe\n").unwrap();
        helper
            .write_all(b"{\"type\":\"launch_state\",\"v\":2,\"state\":\"starting\"}\n")
            .unwrap();
        helper
            .write_all(b"{\"type\":\"launch_state\",\"v\":1,\"state\":\"nope\"}\n")
            .unwrap();
        helper.write_all(&[b'x'; 5000]).unwrap();
        helper
            .write_all(b"{\"type\":\"launch_state\",\"v\":1,\"state\":\"starting\"}\n")
            .unwrap();
        // The oversize line ends at that newline, so its tail went with it; this lands.
        helper.write_all(CLIENT_ONLY).unwrap();
        s.wait_for(|r| r.launch_state.is_some());
        assert_eq!(
            s.socket().reports().launch_state,
            Some(LaunchState::ClientOnly)
        );
        assert_eq!(s.traces().len(), 1);
    }

    #[test]
    fn a_line_of_exactly_the_cap_is_accepted_and_one_byte_more_is_not() {
        let s = Session::open(620);
        let mut helper = s.connect();
        let line = |pad: usize| {
            let mut l = format!(
                r#"{{"type":"game_window","v":1,"on_screen":true}}{}"#,
                " ".repeat(pad)
            );
            l.push('\n');
            l
        };
        let base = line(0).len();
        helper
            .write_all(line(MAX_LINE - base + 1).as_bytes())
            .unwrap();
        helper.write_all(CLIENT_ONLY).unwrap();
        s.wait_for(|r| r.launch_state.is_some());
        assert_eq!(
            s.socket().reports().game_window_on_screen,
            None,
            "1025 bytes"
        );
        helper.write_all(line(MAX_LINE - base).as_bytes()).unwrap();
        s.wait_for(|r| r.game_window_on_screen.is_some());
    }

    #[test]
    fn a_flood_is_read_at_the_rate_cap() {
        let mut s = Session::open(620);
        let mut helper = s.connect();
        helper.set_nonblocking(true).unwrap();
        // Every line read is a transition, so each one would be a trace event.
        let lines = [
            CLIENT_ONLY,
            &b"{\"type\":\"launch_state\",\"v\":1,\"state\":\"starting\"}\n"[..],
        ];
        let started = Instant::now();
        let mut held_off = false;
        let mut sent = 0;
        while started.elapsed() < Duration::from_millis(1300) {
            match helper.write_all(lines[sent % 2]) {
                Ok(()) => sent += 1,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    held_off = true;
                    std::thread::sleep(Duration::from_millis(5));
                }
                Err(e) => panic!("{e}"),
            }
        }
        let read = s.traces().len() as u32;
        assert!(
            held_off,
            "the writer was never held off ({sent} lines sent)"
        );
        // At most two one-second windows have opened since the socket did.
        assert!(
            (1..=2 * MAX_LINES_PER_SEC).contains(&read),
            "{read} lines read in 1.3 s"
        );
        assert!(read < MAX_NOTABLE, "the cap, not the allowance, bounded it");
    }

    #[test]
    fn a_quiet_connection_is_dropped_and_the_next_one_is_served() {
        let s = Session::open(620);
        // A holder that said one thing and then went quiet, the way a process that killed
        // the helper and took its place could.
        let mut holder = s.connect();
        holder.write_all(STARTING).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::Starting));
        let mut helper = s.connect();
        helper.write_all(CLIENT_ONLY).unwrap();
        std::thread::sleep(POLL * 3);
        assert_eq!(
            s.socket().reports().launch_state,
            Some(LaunchState::Starting),
            "one connection at a time"
        );
        s.wait_for(|r| r.launch_state == Some(LaunchState::ClientOnly));
        holder.set_read_timeout(Some(POLL)).unwrap();
        assert_eq!(
            holder.read(&mut [0u8; 1]).unwrap(),
            0,
            "the holder was closed"
        );
    }

    #[test]
    fn launch_states_only_move_forward() {
        use LaunchState::*;
        let always = [
            (None, Starting),
            (None, ClientOnly),
            (None, GameRunning),
            (Some(Starting), ClientOnly),
            (Some(Starting), GameRunning),
            (Some(ClientOnly), Starting),
            (Some(ClientOnly), GameRunning),
            (Some(GameRunning), Starting),
        ];
        for seen_running in [false, true] {
            for from in [
                None,
                Some(Starting),
                Some(ClientOnly),
                Some(GameRunning),
                Some(GameExited),
            ] {
                for to in [Starting, ClientOnly, GameRunning, GameExited] {
                    if from == Some(to) {
                        continue;
                    }
                    let exit_after_running =
                        to == GameExited && seen_running && from != Some(GameExited);
                    assert_eq!(
                        forward(from, to, seen_running),
                        always.contains(&(from, to)) || exit_after_running,
                        "{from:?} -> {to:?}, seen_running={seen_running}"
                    );
                }
            }
        }
    }

    fn state_line(state: &str) -> Vec<u8> {
        format!("{{\"type\":\"launch_state\",\"v\":1,\"state\":\"{state}\"}}\n").into_bytes()
    }

    #[test]
    fn a_launch_state_out_of_order_changes_nothing() {
        let mut s = Session::open(620);
        let mut helper = s.connect();
        // A title that never ran cannot have exited.
        helper.write_all(&state_line("game_exited")).unwrap();
        // A reconnect replays only the latest state, so running from nothing is fine.
        helper.write_all(&state_line("game_running")).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::GameRunning));
        helper.write_all(&state_line("client_only")).unwrap();
        helper.write_all(&state_line("game_exited")).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::GameExited));
        // Nothing after the exit.
        for state in ["starting", "client_only", "game_running"] {
            helper.write_all(&state_line(state)).unwrap();
        }
        helper.write_all(GAME_WINDOW).unwrap();
        s.wait_for(|r| r.game_window_on_screen.is_some());
        assert_eq!(
            s.socket().reports().launch_state,
            Some(LaunchState::GameExited)
        );
        assert_eq!(s.wire_state().as_deref(), Some("game_exited"));
        let states: Vec<_> = s.traces().iter().map(|p| p["state"].clone()).collect();
        assert_eq!(states, ["game_running", "game_exited"]);
    }

    #[test]
    fn a_real_exit_after_a_restarted_helpers_starting_is_accepted() {
        let mut s = Session::open(620);
        let mut helper = s.connect();
        // The helper died mid-game; the launcher's new one first says starting.
        for state in ["game_running", "starting", "game_exited"] {
            helper.write_all(&state_line(state)).unwrap();
        }
        s.wait_for(|r| r.launch_state == Some(LaunchState::GameExited));
        let states: Vec<_> = s.traces().iter().map(|p| p["state"].clone()).collect();
        assert_eq!(states, ["game_running", "starting", "game_exited"]);
    }

    #[test]
    fn a_swap_away_from_the_tile_clears_the_reports_until_it_is_back() {
        let s = Session::open(620);
        let mount = s.socket().mount().clone();
        let mut helper = s.connect();
        helper.write_all(&state_line("game_running")).unwrap();
        s.wait_for(|r| r.launch_state.is_some());

        // Another tile, the plain Steam app or a non-Steam app: no mount for any of them.
        for other in [env("-applaunch 730"), env("-bigpicture"), BTreeMap::new()] {
            assert!(mount.for_app(&other).is_none());
        }
        assert_eq!(s.socket().reports(), LaunchReports::default());
        assert_eq!(s.wire_state(), None, "absent: the app no longer reports");
        // A dead helper's queued line is dropped while the tile is away.
        helper.write_all(&state_line("game_exited")).unwrap();
        helper.write_all(GAME_WINDOW).unwrap();
        std::thread::sleep(POLL * 3);
        assert_eq!(s.socket().reports(), LaunchReports::default());
        assert_eq!(s.wire_state(), None);

        // A rollback or a swap back to the tile gets the mount and re-arms the reader, and
        // the game it saw running before the swap does not make an exit legal now.
        assert!(mount.for_app(&env("-bigpicture -applaunch 620")).is_some());
        helper.write_all(&state_line("game_exited")).unwrap();
        helper.write_all(CLIENT_ONLY).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::ClientOnly));
        assert_eq!(s.wire_state().as_deref(), Some("client_only"));
    }

    #[test]
    fn a_game_exit_counts_only_after_a_running_game() {
        let s = Session::open(620);
        let mut helper = s.connect();
        assert!(!s.socket().reports().game_exited(), "no helper report yet");
        helper.write_all(&state_line("game_exited")).unwrap();
        helper
            .write_all(b"{\"type\":\"launch_state\",\"v\":1,\"state\"\n")
            .unwrap();
        helper.write_all(&state_line("client_only")).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::ClientOnly));
        assert!(!s.socket().reports().game_exited());
        helper.write_all(&state_line("game_running")).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::GameRunning));
        assert!(!s.socket().reports().game_exited());
        helper.write_all(&state_line("game_exited")).unwrap();
        s.wait_for(|r| r.game_exited());
        // A forged report state that skips the forward rule is still not an exit.
        let forged = LaunchReports {
            launch_state: Some(LaunchState::GameExited),
            ..LaunchReports::default()
        };
        assert!(!forged.game_exited());
    }

    #[test]
    fn muting_for_a_swap_drops_the_old_games_exit_and_a_rollback_starts_clean() {
        let s = Session::open(620);
        let mount = s.socket().mount().clone();
        let mut helper = s.connect();
        helper.write_all(&state_line("game_running")).unwrap();
        s.wait_for(|r| r.launch_state.is_some());
        mount.mute();
        // Steam closes the game the swap is stopping; the helper reports it.
        helper.write_all(&state_line("game_exited")).unwrap();
        std::thread::sleep(POLL * 3);
        assert!(!s.socket().reports().game_exited());
        assert_eq!(s.socket().reports(), LaunchReports::default());
        // The rolled-back relaunch of the tile arms a reader that has not seen it run.
        assert!(mount.for_app(&env("-bigpicture -applaunch 620")).is_some());
        helper.write_all(&state_line("game_exited")).unwrap();
        helper.write_all(CLIENT_ONLY).unwrap();
        s.wait_for(|r| r.launch_state == Some(LaunchState::ClientOnly));
        assert!(!s.socket().reports().game_exited());
    }

    #[test]
    fn a_line_queued_as_a_quiet_connection_closes_is_still_read() {
        let shared = Arc::new(Shared {
            state: Mutex::new(State {
                active: true,
                ..State::default()
            }),
            metrics: Arc::new(SessionMetrics::new("off", 60)),
        });
        let (tx, _rx) = tokio::sync::mpsc::channel(8);
        shared.state().target = Some(620);
        let mut reader = Reader {
            stop: Arc::default(),
            shared: shared.clone(),
            session_id: "s1".into(),
            trace: DiagnosticEventTx::new(tx, Arc::default(), Arc::default()),
            window: (Instant::now(), 0),
            notable_left: MAX_NOTABLE,
            dropped: Limiter::default(),
            held: Limiter::default(),
            // Quiet from the start: the close begins before the first read.
            idle: Duration::ZERO,
        };
        let (ours, mut helper) = UnixStream::pair().unwrap();
        helper.write_all(CLIENT_ONLY).unwrap();
        reader.read_connection(ours);
        assert_eq!(
            shared.state().reports.launch_state,
            Some(LaunchState::ClientOnly),
            "the line the helper wrote before the close was read"
        );
        assert_eq!(
            helper.write_all(STARTING).unwrap_err().kind(),
            std::io::ErrorKind::BrokenPipe,
            "the next write fails, so the helper reconnects and replays"
        );
    }

    #[test]
    fn the_log_limiter_lets_one_line_through_an_interval_and_counts_the_rest() {
        let mut limiter = Limiter::default();
        assert_eq!(limiter.ready(), Some(0));
        assert_eq!(limiter.ready(), None);
        assert_eq!(limiter.ready(), None);
        limiter.last = limiter.last.and_then(|t| t.checked_sub(LOG_INTERVAL));
        assert_eq!(
            limiter.ready(),
            Some(2),
            "the next line says how many it held back"
        );
        assert_eq!(limiter.ready(), None);
    }

    #[test]
    fn a_connection_marks_the_helper_connected_and_no_connection_does_not() {
        let s = Session::open(620);
        std::thread::sleep(POLL * 2);
        assert!(!s.socket().reports().helper_connected, "nobody connected");
        let _helper = s.connect();
        s.wait_for(|r| r.helper_connected);
    }

    #[test]
    fn a_helper_that_never_connects_changes_nothing_and_the_end_removes_the_directory() {
        let mut s = Session::open(620);
        let dir = owned_entry::entry_dir(&s.runtime_dir, PREFIX, "s1");
        let marker = owned_entry::marker_path(&s.runtime_dir, PREFIX, "s1");
        assert!(dir.join(SOCKET_FILE).exists() && marker.exists());
        std::thread::sleep(POLL * 2);
        assert_eq!(s.socket().reports(), LaunchReports::default());
        assert_eq!(s.wire_state(), None);
        assert!(s.traces().is_empty());
        s.socket = None;
        assert!(!dir.exists() && !marker.exists());
    }

    #[test]
    fn the_socket_and_its_directory_are_reachable_by_the_app_user() {
        let s = Session::open(620);
        let dir = owned_entry::entry_dir(&s.runtime_dir, PREFIX, "s1");
        let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&dir), 0o755);
        assert_eq!(mode(&dir.join(SOCKET_FILE)), 0o666);
        assert_eq!(s.socket().mount().target(), Some(620));
    }

    #[test]
    fn the_reports_reveal_on_the_game_on_screen_or_steam_needing_the_user() {
        let reports = |on_screen, needs_user| LaunchReports {
            game_window_on_screen: on_screen,
            needs_user,
            ..LaunchReports::default()
        };
        assert_eq!(reports(None, None).reveal_reason(), None);
        assert_eq!(
            reports(Some(false), None).reveal_reason(),
            None,
            "Steam on screen is not the game"
        );
        assert_eq!(
            reports(Some(true), None).reveal_reason(),
            Some("game window on screen")
        );
        assert_eq!(
            reports(None, Some(NeedsUser::SignIn)).reveal_reason(),
            Some("Steam needs the user")
        );
    }

    #[test]
    fn the_reveal_cap_defaults_to_90_s_and_stays_within_the_boot_watchdog() {
        let budget = Some(Duration::from_secs(300));
        let secs = |raw: Option<&str>, budget| reveal_cap(raw.map(String::from), budget).as_secs();
        assert_eq!(secs(None, budget), 90);
        assert_eq!(secs(Some("0"), budget), 0, "0 holds nothing");
        assert_eq!(secs(Some(" 45 "), budget), 45);
        assert_eq!(secs(Some("soon"), budget), 90);
        assert_eq!(
            secs(Some("200"), Some(Duration::from_secs(120))),
            120,
            "clamped to the watchdog"
        );
        assert_eq!(
            secs(Some("400"), None),
            300,
            "the watchdog's default with it off"
        );
    }

    fn read_line(stream: &mut UnixStream) -> String {
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut line = Vec::new();
        let mut byte = [0u8; 1];
        while byte[0] != b'\n' {
            stream.read_exact(&mut byte).unwrap();
            line.push(byte[0]);
        }
        String::from_utf8(line).unwrap()
    }

    #[test]
    fn a_command_waits_for_the_helper_and_goes_out_on_its_connection() {
        let s = Session::open_parked();
        let mount = s.socket().mount().clone();
        mount.send(HelperCommand::Launch(620));
        let mut helper = s.connect();
        assert_eq!(read_line(&mut helper), "launch 620\n");
        mount.send(HelperCommand::Quit(620));
        assert_eq!(read_line(&mut helper), "quit 620\n");
        // Its result, and a new command clearing it.
        helper
            .write_all(br#"{"type":"command_done","v":1,"command":"quit","appid":620,"ok":true,"cloud_synced":true}"#)
            .unwrap();
        helper.write_all(b"\n").unwrap();
        s.wait_for(|r| r.last_command.is_some());
        assert_eq!(
            s.socket().reports().last_command,
            Some(CommandDone {
                command: HelperCommand::Quit(620),
                ok: true,
                cloud_synced: Some(true),
            })
        );
        mount.send(HelperCommand::BigPicture);
        assert_eq!(s.socket().reports().last_command, None);
        assert_eq!(read_line(&mut helper), "bigpicture\n");
    }

    #[test]
    fn a_helper_sending_keepalives_keeps_its_connection() {
        let s = Session::open_parked();
        let mount = s.socket().mount().clone();
        let mut helper = s.connect();
        let until = Instant::now() + IDLE * 2;
        while Instant::now() < until {
            helper.write_all(b"\n").unwrap();
            std::thread::sleep(IDLE / 4);
        }
        mount.send(HelperCommand::Launch(730));
        assert_eq!(
            read_line(&mut helper),
            "launch 730\n",
            "still the served connection"
        );
    }

    #[test]
    fn a_parked_socket_takes_no_game_report_until_armed_and_then_follows_its_target() {
        let mut s = Session::open_parked();
        let mount = s.socket().mount().clone();
        assert_eq!(mount.target(), None);
        let mut helper = s.connect();
        helper.write_all(GAME_WINDOW).unwrap();
        helper.write_all(&state_line("game_running")).unwrap();
        helper
            .write_all(b"{\"type\":\"shader_progress\",\"v\":1,\"percent\":5}\n")
            .unwrap();
        s.wait_for(|r| r.helper_connected);
        std::thread::sleep(POLL * 3);
        assert_eq!(
            s.socket().reports().game_window_on_screen,
            None,
            "watch-only"
        );
        assert_eq!(s.socket().reports().launch_state, None);
        assert!(s.traces().is_empty());

        mount.arm(Some(620));
        helper.write_all(&state_line("game_running")).unwrap();
        helper
            .write_all(b"{\"type\":\"shader_progress\",\"v\":1,\"percent\":40}\n")
            .unwrap();
        s.wait_for(|r| r.shader_percent == Some(40));
        assert!(
            s.socket().reports().helper_connected,
            "arming keeps the connection's word"
        );
        let traces = s.traces();
        assert_eq!(traces.len(), 1);
        assert_eq!(traces[0]["appid"], "620");

        // A new target starts from nothing: the old game's running and window go.
        mount.arm(Some(730));
        assert_eq!(s.socket().reports().launch_state, None);
        assert_eq!(s.socket().reports().shader_percent, None);
        assert!(!s.socket().reports().game_exited());
    }

    #[test]
    fn a_parked_mount_is_for_its_own_flags_only() {
        let s = Session::open_parked();
        let mount = s.socket().mount().clone();
        assert!(mount.for_app(&env("-silent")).is_some());
        for other in [env("-applaunch 620"), env("-bigpicture"), BTreeMap::new()] {
            assert!(mount.for_app(&other).is_none());
        }
        assert!(
            mount.for_app(&env("-silent")).is_some(),
            "a rollback re-arms it"
        );
        assert_eq!(mount.target(), None);
    }

    #[test]
    fn open_mounts_nothing_without_an_appid_or_an_installed_helper() {
        let dir = tempfile::tempdir().unwrap();
        let runtime_dir = dir.path().to_str().unwrap();
        let open = |flags: Option<&BTreeMap<String, String>>| {
            let (tx, _rx) = tokio::sync::mpsc::channel(1);
            LaunchSocket::open(
                runtime_dir,
                "s1",
                flags,
                flags.is_some_and(|env| {
                    env.get("STEAM_STARTUP_FLAGS")
                        .is_some_and(|f| f == "-silent")
                }),
                Arc::new(SessionMetrics::new("off", 60)),
                DiagnosticEventTx::new(tx, Arc::default(), Arc::default()),
            )
            .is_some()
        };
        assert!(!open(None), "no app");
        assert!(!open(Some(&env("-bigpicture"))), "the plain Steam app");
        assert!(!open(Some(&env("-applaunch 620"))), "no helper installed");
        assert!(!open(Some(&env("-silent"))), "parked, no helper installed");
        assert!(std::fs::read_dir(runtime_dir).unwrap().next().is_none());
    }
}
