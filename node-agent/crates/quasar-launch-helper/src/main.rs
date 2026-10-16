//! The in-container Steam launch helper (#731).
//!
//! The Steam image's launcher starts it as the app user when the node agent mounted it
//! (`QUASAR_LAUNCH_HELPER`). It watches the derived tile's game with `quasar-launch-watch`
//! and writes each change to `QUASAR_LAUNCH_SOCKET` as one line of the #725 wire, whose
//! reader is `node-agent/src/session/launch_helper.rs`. Report-only: it reads nothing from
//! the socket and writes nothing to the X display.

use std::io::Write;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use quasar_launch_watch::{
    appid_from_flags, LaunchState, NeedsUser, Report, WatchConfig, Watcher, WindowInfo, Windows,
};
use x11rb::connection::Connection;
use x11rb::cookie::Cookie;
use x11rb::errors::{ConnectionError, ReplyError};
use x11rb::protocol::xproto::{Atom, AtomEnum, ConnectionExt, GetPropertyReply, MapState, Window};
use x11rb::rust_connection::RustConnection;
use x11rb::x11_utils::TryParse;

/// A few passes a second: the reveal (#727) and the game exit (#726) wait on this, and the
/// core reads at most 1 MiB of Steam's log a pass.
const POLL: Duration = Duration::from_millis(250);
const RECONNECT: Duration = Duration::from_secs(2);
const WRITE_TIMEOUT: Duration = Duration::from_secs(2);
const MAX_WINDOWS: usize = 256;
/// Longest property read, in 32-bit units.
const MAX_PROPERTY: u32 = 256;

fn main() {
    let flags = std::env::var("STEAM_STARTUP_FLAGS").unwrap_or_default();
    let Some(appid) = appid_from_flags(&flags) else {
        return;
    };
    let Some(socket) = std::env::var_os("QUASAR_LAUNCH_SOCKET") else {
        eprintln!("quasar-launch-helper: QUASAR_LAUNCH_SOCKET is not set; nothing to report to");
        return;
    };
    let home = PathBuf::from(std::env::var_os("HOME").unwrap_or_default());
    let mut watcher = Watcher::new(
        WatchConfig {
            proc_root: "/proc".into(),
            steam_log_dir: home.join(".local/share/Steam/logs"),
            appid,
        },
        Box::<X11Windows>::default(),
    );
    let mut agent = Agent::new(socket.into());
    eprintln!("quasar-launch-helper: watching Steam app {appid}");
    let mut ready = Some(std::io::stdout());
    loop {
        agent.connect();
        for report in watcher.poll() {
            agent.send(report);
        }
        // The core picks where to start reading Steam's log on its first pass, so the
        // launcher holds Steam back (at most 2 s) until this line.
        if let Some(mut out) = ready.take() {
            let _ = writeln!(out, "ready");
        }
        std::thread::sleep(POLL);
    }
}

/// The wire, as the agent's reader (`node-agent/src/session/launch_helper.rs`) takes it.
#[derive(serde::Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum Wire {
    LaunchState { v: u32, state: LaunchState },
    GameWindow { v: u32, on_screen: bool },
    SteamNeedsUser { v: u32, reason: NeedsUser },
}

/// One wire line for `report`, and which of the three kinds it is.
fn line(report: Report) -> (usize, String) {
    let (kind, wire) = match report {
        Report::LaunchState(state) => (0, Wire::LaunchState { v: 1, state }),
        Report::GameWindow { on_screen } => (1, Wire::GameWindow { v: 1, on_screen }),
        Report::SteamNeedsUser(reason) => (2, Wire::SteamNeedsUser { v: 1, reason }),
    };
    // Unit variants and a bool cannot fail to serialise.
    (
        kind,
        serde_json::to_string(&wire).unwrap_or_default() + "\n",
    )
}

/// The connection to the agent. The agent keeps what it was told across connections, so a
/// new one repeats the latest line of each kind and nothing is lost to a reconnect.
struct Agent {
    path: PathBuf,
    conn: Option<UnixStream>,
    next_try: Instant,
    latest: [Option<String>; 3],
}

impl Agent {
    fn new(path: PathBuf) -> Self {
        Agent {
            path,
            conn: None,
            next_try: Instant::now(),
            latest: Default::default(),
        }
    }

    fn connect(&mut self) {
        if self.conn.is_some() || Instant::now() < self.next_try {
            return;
        }
        self.next_try = Instant::now() + RECONNECT;
        let Ok(conn) = UnixStream::connect(&self.path) else {
            return;
        };
        if conn.set_write_timeout(Some(WRITE_TIMEOUT)).is_err() {
            return;
        }
        self.conn = Some(conn);
        for line in self.latest.clone().into_iter().flatten() {
            self.write(&line);
        }
    }

    fn send(&mut self, report: Report) {
        let (kind, line) = line(report);
        self.write(&line);
        self.latest[kind] = Some(line);
    }

    fn write(&mut self, line: &str) {
        if let Some(conn) = &mut self.conn {
            if conn.write_all(line.as_bytes()).is_err() {
                eprintln!("quasar-launch-helper: lost the agent's socket; reconnecting");
                self.conn = None;
            }
        }
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
        Ok(X11 {
            conn,
            root,
            steam_game,
            focused_app,
            net_wm_name,
            utf8_string,
        })
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
    fn a_closed_socket_is_dropped_and_retried() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("launch.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let mut agent = Agent::new(path.clone());
        agent.connect();
        let (first, _) = listener.accept().unwrap();
        drop(first);
        // The first write after the peer closed may still succeed; the next one cannot.
        for _ in 0..3 {
            agent.send(Report::GameWindow { on_screen: true });
        }
        assert!(agent.conn.is_none());
        agent.next_try = Instant::now();
        agent.connect();
        drop(agent);
        let (second, _) = listener.accept().unwrap();
        let lines: Vec<String> = BufReader::new(second).lines().map(Result::unwrap).collect();
        assert_eq!(lines, [r#"{"type":"game_window","v":1,"on_screen":true}"#]);
    }
}
