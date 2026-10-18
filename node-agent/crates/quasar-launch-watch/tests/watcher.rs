use quasar_launch_watch::{
    appid_from_flags, LaunchState, NeedsUser, Report, WatchConfig, Watcher, WindowInfo, Windows,
};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Instant;

const APPID: u32 = 730;
/// Past the core's two seconds for a wait for the user to stand; tests step the clock, not sleep.
const PAST_USER_WAIT: std::time::Duration = std::time::Duration::from_millis(2100);
const STEAM: &[&str] = &[
    "/home/gamer/.local/share/Steam/ubuntu12_64/steam",
    "-bigpicture",
];

#[derive(Clone, Default)]
struct FakeWindows {
    state: Arc<Mutex<FakeState>>,
}

#[derive(Default)]
struct FakeState {
    windows: Vec<WindowInfo>,
    fail: bool,
    focused: Option<u32>,
    focus_fail: bool,
}

impl FakeWindows {
    fn set(&self, windows: Vec<WindowInfo>) {
        self.state.lock().unwrap().windows = windows;
    }
    fn set_fail(&self, fail: bool) {
        self.state.lock().unwrap().fail = fail;
    }
    fn set_focus(&self, focused: Option<u32>) {
        self.state.lock().unwrap().focused = focused;
    }
    fn set_focus_fail(&self, fail: bool) {
        self.state.lock().unwrap().focus_fail = fail;
    }
}

impl Windows for FakeWindows {
    fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>> {
        let state = self.state.lock().unwrap();
        if state.fail {
            Err(std::io::Error::other("fake window list failure"))
        } else {
            Ok(state.windows.clone())
        }
    }
    fn focused_app(&mut self) -> std::io::Result<Option<u32>> {
        let state = self.state.lock().unwrap();
        if state.focus_fail {
            Err(std::io::Error::other("fake focus failure"))
        } else {
            Ok(state.focused)
        }
    }
}

struct Rig {
    _temp: tempfile::TempDir,
    proc_root: PathBuf,
    log_dir: PathBuf,
    windows: FakeWindows,
    watcher: Watcher,
}

impl Rig {
    fn new() -> Self {
        let temp = tempfile::tempdir().unwrap();
        let proc_root = temp.path().join("proc");
        let log_dir = temp.path().join("logs");
        std::fs::create_dir_all(&proc_root).unwrap();
        std::fs::create_dir_all(&log_dir).unwrap();
        // Present from the start, as a persisted home's log would be (empty here).
        std::fs::write(log_dir.join("console_log.txt"), "").unwrap();
        let windows = FakeWindows::default();
        let watcher = Watcher::new(
            WatchConfig {
                proc_root: proc_root.clone(),
                steam_log_dir: log_dir.clone(),
                appid: APPID,
            },
            Box::new(windows.clone()),
        );
        Self {
            _temp: temp,
            proc_root,
            log_dir,
            windows,
            watcher,
        }
    }

    fn log_path(&self) -> PathBuf {
        self.log_dir.join("console_log.txt")
    }

    fn append_log(&self, line: &str) {
        self.append("console_log.txt", &format!("{line}\n"));
    }

    fn append(&self, file: &str, text: &str) {
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(self.log_dir.join(file))
            .unwrap();
        file.write_all(text.as_bytes()).unwrap();
    }

    fn login_state(&self, state: &str) {
        self.append(
            "steamui_login.txt",
            &format!("[2026-10-16 09:37:10] [ None ] SetLoginState: {state} - OK\n"),
        );
    }
}

fn write_proc(proc_root: &Path, pid: u32, args: &[&str]) {
    // Only cmdline: it is the one file the watcher reads per pid.
    let dir = proc_root.join(pid.to_string());
    std::fs::create_dir_all(&dir).unwrap();
    let mut cmdline = Vec::new();
    for arg in args {
        cmdline.extend_from_slice(arg.as_bytes());
        cmdline.push(0);
    }
    std::fs::write(dir.join("cmdline"), cmdline).unwrap();
}

fn remove_proc(proc_root: &Path, pid: u32) {
    std::fs::remove_dir_all(proc_root.join(pid.to_string())).unwrap();
}

fn copy_dir(from: &Path, to: &Path) {
    std::fs::create_dir_all(to).unwrap();
    for entry in std::fs::read_dir(from).unwrap() {
        let entry = entry.unwrap();
        let target = to.join(entry.file_name());
        if entry.file_type().unwrap().is_dir() {
            copy_dir(&entry.path(), &target);
        } else {
            std::fs::copy(entry.path(), &target).unwrap();
        }
    }
}

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name)
}

fn game_window() -> WindowInfo {
    WindowInfo {
        steam_game: Some(APPID),
        wm_class: "game".into(),
        title: "Game".into(),
    }
}

fn steam_window(title: &str) -> WindowInfo {
    WindowInfo {
        steam_game: None,
        wm_class: "Steam".into(),
        title: title.into(),
    }
}

#[test]
fn appid_from_flags_cases() {
    assert_eq!(appid_from_flags(""), None);
    assert_eq!(appid_from_flags("-bigpicture"), None);
    assert_eq!(appid_from_flags("junk"), None);
    assert_eq!(appid_from_flags("-applaunch abc"), None);
    assert_eq!(appid_from_flags("-applaunch +730"), None);
    assert_eq!(appid_from_flags("-applaunch 0"), None);
    assert_eq!(appid_from_flags("-applaunch 73x"), None);
    assert_eq!(appid_from_flags("-bigpicture -applaunch"), None);
    assert_eq!(appid_from_flags("-applaunch 0730"), None);
    assert_eq!(appid_from_flags("-applaunch 730 -applaunch 730"), None);
    assert_eq!(appid_from_flags("-applaunch 730 -applaunch 620"), None);
    // ASCII whitespace only, as the agent and the launcher split it.
    assert_eq!(appid_from_flags("x\u{2003}-applaunch 730"), None);
    assert_eq!(appid_from_flags("-applaunch\u{2003}730"), None);
    assert_eq!(appid_from_flags("-applaunch 730"), Some(730));
    assert_eq!(appid_from_flags("  -applaunch\t730  "), Some(730));
    assert_eq!(
        appid_from_flags("-bigpicture -applaunch 730 -silent extra"),
        Some(730)
    );
}

#[test]
fn first_poll_is_starting_then_client_only_and_never_exits_before_running() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);

    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );
    assert_eq!(rig.watcher.poll(), vec![]);

    // Steam going away before any game ran is not a game exit.
    remove_proc(&rig.proc_root, 100);
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn fixture_trees_run_and_exit() {
    for name in ["native", "proton", "launcher-shim"] {
        let mut rig = Rig::new();
        copy_dir(&fixture(name).join("proc"), &rig.proc_root);

        assert_eq!(
            rig.watcher.poll(),
            vec![
                Report::LaunchState(LaunchState::Starting),
                Report::LaunchState(LaunchState::GameRunning),
            ],
            "fixture {name}"
        );
        assert_eq!(rig.watcher.poll(), vec![], "fixture {name}");

        // Leave only the Steam client (pid 100): the reaper tree is gone.
        let pids: Vec<u32> = std::fs::read_dir(&rig.proc_root)
            .unwrap()
            .filter_map(|entry| {
                entry
                    .unwrap()
                    .file_name()
                    .to_string_lossy()
                    .parse::<u32>()
                    .ok()
            })
            .filter(|pid| *pid != 100)
            .collect();
        for pid in pids {
            remove_proc(&rig.proc_root, pid);
        }
        assert_eq!(
            rig.watcher.poll(),
            vec![Report::LaunchState(LaunchState::GameExited)],
            "fixture {name}"
        );
        assert_eq!(rig.watcher.poll(), vec![], "fixture {name}");
    }
}

#[test]
fn launcher_shim_reaper_alone_is_running_until_it_goes() {
    let mut rig = Rig::new();
    copy_dir(&fixture("launcher-shim").join("proc"), &rig.proc_root);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
    );

    // Every descendant goes; the reaper alone still counts (it is a
    // subreaper that outlives them), so the shim title is still running.
    remove_proc(&rig.proc_root, 402);
    remove_proc(&rig.proc_root, 401);
    assert_eq!(rig.watcher.poll(), vec![]);

    remove_proc(&rig.proc_root, 400);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
}

#[test]
fn fixture_console_log_drives_transitions_line_by_line() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    let log = std::fs::read_to_string(fixture("console_log.txt")).unwrap();
    let mut lines = log.lines();

    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    // A routine SynchronizingCloud step reports nothing.
    rig.append_log(lines.next().unwrap());
    assert_eq!(rig.watcher.poll(), vec![]);

    // A wait answered within two seconds never reports.
    rig.append_log(lines.next().unwrap());
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log(lines.next().unwrap());
    assert_eq!(rig.watcher.poll(), vec![]);

    // The log alone moves ClientOnly to GameRunning, before any reaper.
    rig.append_log(lines.next().unwrap());
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );

    rig.append_log(lines.next().unwrap());
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.append_log(lines.next().unwrap());
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );

    // Remaining lines (including another appid's) change nothing.
    for line in lines {
        rig.append_log(line);
    }
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn log_removal_ends_the_game_when_the_tree_is_unreadable() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );

    std::fs::remove_dir_all(&rig.proc_root).unwrap();
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.append_log(&format!(
        "Game process removed: AppID {APPID} \"game\", ProcID 201"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
}

#[test]
fn truncated_log_restarts_at_zero() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"/a/rather/long/game/command/line --with-many-arguments\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );

    // A truncate to empty restarts the offset without inventing an exit.
    std::fs::remove_dir_all(&rig.proc_root).unwrap();
    std::fs::write(rig.log_path(), "").unwrap();
    assert_eq!(rig.watcher.poll(), vec![]);

    // And a shorter file written over a longer one is read from the start.
    rig.append_log(&format!(
        "Game process removed: AppID {APPID} \"game\", ProcID 201"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
}

#[test]
fn a_vanished_pid_mid_read_is_skipped() {
    let mut rig = Rig::new();
    copy_dir(&fixture("native").join("proc"), &rig.proc_root);

    // A pid that vanished mid-read: its cmdline is a dangling symlink, so
    // the open fails NotFound and only that pid is skipped.
    let gone = rig.proc_root.join("999");
    std::fs::create_dir_all(&gone).unwrap();
    std::os::unix::fs::symlink("/nonexistent-vanished-pid", gone.join("cmdline")).unwrap();

    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
    );
}

#[test]
fn a_window_list_error_skips_only_the_window_source() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.windows.set_fail(true);

    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    rig.windows.set(vec![game_window()]);
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.windows.set_focus(Some(APPID));
    rig.windows.set_fail(false);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: true }]
    );
}

#[test]
fn game_window_follows_focus_not_the_mapped_list() {
    let mut rig = Rig::new();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::Starting)]
    );

    // Mapped, but gamescope's focus is elsewhere: not on screen.
    rig.windows.set(vec![game_window()]);
    rig.windows.set_focus(Some(769));
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.windows.set_focus(Some(APPID));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: true }]
    );
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.windows.set_focus(None);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: false }]
    );
}

#[test]
fn a_focused_app_error_skips_the_on_screen_check() {
    let mut rig = Rig::new();
    rig.windows.set_focus(Some(APPID));
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::GameWindow { on_screen: true },
        ]
    );

    // The focus read fails and the focus has in fact left: nothing is
    // reported for that pass, and the change lands on the next good read.
    rig.windows.set_focus(None);
    rig.windows.set_focus_fail(true);
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.windows.set_focus_fail(false);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: false }]
    );
}

#[test]
fn the_measured_sign_in_to_steam_window_needs_the_user() {
    let mut rig = Rig::new();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::Starting)]
    );

    // The #723 measurement: STEAM_GAME=769, WM_CLASS steamwebhelper/steam.
    rig.windows.set(vec![WindowInfo {
        steam_game: Some(769),
        wm_class: "steamwebhelper".into(),
        title: "Sign in to Steam".into(),
    }]);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
}

#[test]
fn steam_needs_user_is_once_per_reason_while_it_holds() {
    let mut rig = Rig::new();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::Starting)]
    );

    for (title, reason) in [
        ("Sign in to Steam", NeedsUser::SignIn),
        (
            "Steam Guard - Computer Authorization Required",
            NeedsUser::SteamGuard,
        ),
        ("Steam Cloud Conflict", NeedsUser::CloudConflict),
        ("End User License Agreement", NeedsUser::Eula),
        ("Steam - Self Updater", NeedsUser::Install),
        ("Steam dialog", NeedsUser::Other),
    ] {
        rig.windows.set(vec![steam_window(title)]);
        assert_eq!(
            rig.watcher.poll(),
            vec![Report::SteamNeedsUser(reason)],
            "{title}"
        );
        assert_eq!(rig.watcher.poll(), vec![], "{title} must not repeat");

        rig.windows.set(vec![]);
        assert_eq!(rig.watcher.poll(), vec![], "{title} clearing is silent");
    }

    // The game's own window never counts as Steam needing the user.
    rig.windows.set(vec![WindowInfo {
        steam_game: Some(APPID),
        wm_class: "Steam".into(),
        title: "Sign in to Steam".into(),
    }]);
    rig.windows.set_focus(Some(APPID));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: true }]
    );
}

#[test]
fn a_games_own_setup_or_login_window_is_not_steam_needing_the_user() {
    let mut rig = Rig::new();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::Starting)]
    );
    for title in [
        "Setup",
        "Installing shaders",
        "Install",
        "Login",
        "Log in to continue",
        "Sign in",
        "Sign-in required",
    ] {
        // The game's own windows: untagged, tagged with the tile's appid, or Proton's.
        for (steam_game, wm_class) in [
            (None, "game game"),
            (Some(APPID), "game game"),
            (None, "steam_app_730 steam_app_730"),
        ] {
            rig.windows.set(vec![WindowInfo {
                steam_game,
                wm_class: wm_class.into(),
                title: title.into(),
            }]);
            assert_eq!(
                rig.watcher.poll(),
                vec![],
                "{title} ({steam_game:?}, {wm_class})"
            );
        }
    }
}

#[test]
fn steams_own_windows_may_use_the_generic_words() {
    for (window, reason) in [
        (
            WindowInfo {
                steam_game: Some(769),
                wm_class: "steamwebhelper steam".into(),
                title: "Install".into(),
            },
            NeedsUser::Install,
        ),
        (
            WindowInfo {
                steam_game: Some(769),
                wm_class: "steamwebhelper steam".into(),
                title: "Log in".into(),
            },
            NeedsUser::SignIn,
        ),
        (
            WindowInfo {
                steam_game: None,
                wm_class: "steam steam".into(),
                title: "Setup".into(),
            },
            NeedsUser::Install,
        ),
        (
            WindowInfo {
                steam_game: Some(769),
                wm_class: String::new(),
                title: "Sign-in required".into(),
            },
            NeedsUser::SignIn,
        ),
    ] {
        let mut rig = Rig::new();
        assert_eq!(
            rig.watcher.poll(),
            vec![Report::LaunchState(LaunchState::Starting)]
        );
        let title = window.title.clone();
        rig.windows.set(vec![window]);
        assert_eq!(
            rig.watcher.poll(),
            vec![Report::SteamNeedsUser(reason)],
            "{title}"
        );
    }
}

const REAPER: &[&str] = &[
    "/home/gamer/.local/share/Steam/ubuntu12_32/reaper",
    "SteamLaunch",
    "AppId=730",
    "--",
    "/games/Game/Game.exe",
];

#[test]
fn stale_log_from_the_previous_session_is_not_replayed() {
    let mut rig = Rig::new();
    // Left by the previous session, before this watcher ever polled.
    std::fs::write(
        rig.log_path(),
        format!("Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0\n"),
    )
    .unwrap();

    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::Starting)]
    );

    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::ClientOnly)]
    );
}

#[test]
fn routine_cloud_sync_is_not_needs_user() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    for task in ["SynchronizingCloud", "SynchronizingControllerConfig"] {
        rig.append_log(&format!(
            "GameAction [AppID {APPID}, ActionID 1] : LaunchApp changed task to {task}"
        ));
        assert_eq!(rig.watcher.poll(), vec![], "{task}");
    }
}

#[test]
fn waiting_for_user_response_reports_once_and_clears() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    // Not reported until no next task has followed for two seconds, then once.
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to SynchronizingCloud \"\""
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    assert_eq!(rig.watcher.poll(), vec![]);
    let later = Instant::now() + PAST_USER_WAIT;
    assert_eq!(
        rig.watcher.poll_at(later),
        vec![Report::SteamNeedsUser(NeedsUser::CloudConflict)]
    );
    assert_eq!(rig.watcher.poll(), vec![]);

    // Cleared by the continues line; a later wait reports again.
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"SynchronizingCloud\""
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to SiteLicenseSeatCheckout \"\""
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    let later = Instant::now() + PAST_USER_WAIT;
    assert_eq!(
        rig.watcher.poll_at(later),
        vec![Report::SteamNeedsUser(NeedsUser::Other)]
    );
}

#[test]
fn a_wait_answered_within_two_seconds_reports_nothing_across_polls() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();

    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to SynchronizingCloud \"\""
    ));
    for _ in 0..4 {
        assert_eq!(rig.watcher.poll(), vec![]);
    }
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp changed task to WaitingGameWindow"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    let later = Instant::now() + PAST_USER_WAIT;
    assert_eq!(rig.watcher.poll_at(later), vec![]);
}

#[test]
fn the_game_starting_or_going_ends_a_wait_for_the_user() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    let wait = format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to SynchronizingCloud \"\""
    );

    rig.append_log(&wait);
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
    let later = Instant::now() + PAST_USER_WAIT;
    assert_eq!(
        rig.watcher.poll_at(later),
        vec![],
        "the game starting ended the wait"
    );
    rig.append_log(&wait);
    rig.append_log(&format!(
        "Game process removed: AppID {APPID} \"game\", ProcID 201"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
    let later = Instant::now() + PAST_USER_WAIT;
    assert_eq!(rig.watcher.poll_at(later), vec![]);
}

#[test]
fn routine_waits_never_report_however_long_they_stand() {
    for task in ["CreatingProcess", "ProcessingShaderCache"] {
        let mut rig = Rig::new();
        write_proc(&rig.proc_root, 100, STEAM);
        rig.watcher.poll();
        rig.append_log(&format!(
            "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to {task} \"\""
        ));
        assert_eq!(rig.watcher.poll(), vec![], "{task}");
        let later = Instant::now() + PAST_USER_WAIT;
        assert_eq!(rig.watcher.poll_at(later), vec![], "{task}");
    }
}

/// #755: Steam's controller notice (`ShowInterstitials`) stands until answered and is Steam
/// needing the user; the shader wait after it stands for minutes and is not. Replays the lines
/// of that launch onto a persistent home's log with an earlier session in it.
#[test]
fn steams_controller_notice_reveals_after_the_grace_and_the_shader_wait_never_does() {
    let wait = |task: &str| {
        format!(
            "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to {task} \"\""
        )
    };
    let changed = |task: &str| {
        format!(
            "GameAction [AppID {APPID}, ActionID 1] : LaunchApp changed task to {task} with \"\""
        )
    };
    let mut rig = Rig::new();
    // An earlier session of the same home: its Steam answered both waits and the game ran.
    std::fs::write(
        rig.log_path(),
        [
            wait("ShowInterstitials"),
            format!("GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"ShowInterstitials\""),
            wait("ProcessingShaderCache"),
            format!("Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"),
            format!("Game process removed: AppID {APPID} \"game\", ProcID 201"),
        ]
        .join("\n")
            + "\n",
    )
    .unwrap();
    rig.watcher.poll();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();

    let t0 = Instant::now();
    rig.append_log(&changed("SynchronizingStats"));
    rig.append_log(&changed("ShowInterstitials"));
    rig.append_log(&wait("ShowInterstitials"));
    assert_eq!(rig.watcher.poll_at(t0), vec![]);
    // The user has had two seconds and Steam has not moved on: it is the notice.
    assert_eq!(
        rig.watcher.poll_at(t0 + PAST_USER_WAIT),
        vec![Report::SteamNeedsUser(NeedsUser::Other)]
    );
    assert_eq!(rig.watcher.poll_at(t0 + PAST_USER_WAIT * 40), vec![]);

    // Answered; the shader wait that follows stands for minutes and never reports.
    let t1 = t0 + PAST_USER_WAIT * 41;
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"ShowInterstitials\""
    ));
    rig.append_log(&changed("ProcessingShaderCache"));
    rig.append_log(&wait("ProcessingShaderCache"));
    assert_eq!(rig.watcher.poll_at(t1), vec![]);
    assert_eq!(rig.watcher.poll_at(t1 + PAST_USER_WAIT), vec![]);
    assert_eq!(rig.watcher.poll_at(t1 + PAST_USER_WAIT * 400), vec![]);
    // Steam moves on by itself, as it writes it.
    rig.append_log(&changed("SiteLicenseSeatCheckout"));
    assert_eq!(rig.watcher.poll_at(t1 + PAST_USER_WAIT * 401), vec![]);
}

#[test]
fn a_controller_notice_that_continues_at_once_reports_nothing() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    let t0 = Instant::now();
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to ShowInterstitials \"\""
    ));
    assert_eq!(rig.watcher.poll_at(t0), vec![]);
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"ShowInterstitials\""
    ));
    assert_eq!(rig.watcher.poll_at(t0 + PAST_USER_WAIT), vec![]);
}

#[test]
fn waiting_and_continue_in_one_poll_report_nothing() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to SynchronizingCloud \"\""
    ));
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"SynchronizingCloud\""
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn reaper_seen_then_gone_on_a_readable_tree_exits_despite_a_stale_added_line() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    write_proc(&rig.proc_root, 200, REAPER);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
    );

    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);

    remove_proc(&rig.proc_root, 200);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
}

#[test]
fn a_removed_log_line_while_the_reaper_lives_keeps_running() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    write_proc(&rig.proc_root, 200, REAPER);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
    );

    rig.append_log(&format!(
        "Game process removed: AppID {APPID} \"game\", ProcID 201"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn an_appid_sharing_leading_digits_is_ignored() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    write_proc(
        &rig.proc_root,
        200,
        &[
            "/home/gamer/.local/share/Steam/ubuntu12_32/reaper",
            "SteamLaunch",
            "AppId=7300",
            "--",
            "/games/Other/Other.exe",
        ],
    );
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    rig.append_log("Game process added : AppID 7300 \"other\", ProcID 201, IP 0.0.0.0:0");
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn rotation_is_read_from_zero() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );
    rig.append_log("GameAction [AppID 730, ActionID 1] : LaunchApp changed task to Completed");
    assert_eq!(rig.watcher.poll(), vec![]);

    // Rename + a longer new file: read from 0, not from the old offset.
    let rotated = rig.log_dir.join("console_log.old");
    std::fs::rename(rig.log_path(), rotated).unwrap();
    std::fs::write(
        rig.log_path(),
        format!(
            "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0\n# a padding line that makes the new file longer than the old offset was -- plenty of bytes here\n"
        ),
    )
    .unwrap();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
}

#[test]
fn rotation_finishes_the_previous_file_from_its_offset_then_reads_the_new_one_from_zero() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    rig.append_log("GameAction [AppID 730, ActionID 1] : LaunchApp changed task to Completed");
    assert_eq!(rig.watcher.poll(), vec![]);

    // Written to the old file after the last pass, then Steam rotates it by size.
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));
    let previous = rig.log_dir.join("console_log.txt.previous");
    std::fs::rename(rig.log_path(), &previous).unwrap();
    rig.append_log(&format!(
        "Game process removed: AppID {APPID} \"game\", ProcID 201"
    ));

    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)],
        "the old file's tail first"
    );
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)],
        "then the new file from 0"
    );
}

/// Rotates the log with a wait parsed and more lines unread, makes the old file unreadable
/// (`.previous` itself, or the file a `.previous` symlink points at), and checks the new
/// file is still read and the wait does not outlive its abandoned file.
fn an_unreadable_previous_is_abandoned(through_a_symlink: bool) {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to SynchronizingCloud \"\""
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log("# unread when Steam rotates");

    let previous = rig.log_dir.join("console_log.txt.previous");
    let old = if through_a_symlink {
        rig.log_dir.join("console_log.old")
    } else {
        previous.clone()
    };
    std::fs::rename(rig.log_path(), &old).unwrap();
    if through_a_symlink {
        std::os::unix::fs::symlink(&old, &previous).unwrap();
    }
    std::fs::set_permissions(&old, std::os::unix::fs::PermissionsExt::from_mode(0o000)).unwrap();
    if std::fs::File::open(&old).is_ok() {
        eprintln!("SKIP: a mode 000 file is still readable here (root)");
        return;
    }
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));

    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)],
        "the new file is read in the same pass"
    );
    let later = Instant::now() + PAST_USER_WAIT;
    assert_eq!(rig.watcher.poll_at(later), vec![]);
}

#[test]
fn an_unreadable_previous_file_is_abandoned_for_the_new_one() {
    an_unreadable_previous_is_abandoned(false);
}

#[test]
fn a_previous_symlink_to_an_unreadable_file_is_abandoned_for_the_new_one() {
    an_unreadable_previous_is_abandoned(true);
}

#[test]
fn a_fifo_in_place_of_a_log_never_blocks_a_poll() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    for name in ["steamui_login.txt", "console_log.txt.previous"] {
        let status = std::process::Command::new("mkfifo")
            .arg(rig.log_dir.join(name))
            .status()
            .unwrap();
        assert!(status.success());
    }
    std::fs::remove_file(rig.log_path()).unwrap();
    let status = std::process::Command::new("mkfifo")
        .arg(rig.log_path())
        .status()
        .unwrap();
    assert!(status.success());

    let (done, polled) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let reports = rig.watcher.poll();
        let _ = done.send(reports);
    });
    assert_eq!(
        polled
            .recv_timeout(std::time::Duration::from_secs(5))
            .expect("poll blocked on a fifo"),
        vec![]
    );
}

#[test]
fn a_previous_file_that_is_not_the_one_being_read_is_ignored() {
    let mut rig = Rig::new();
    let previous = rig.log_dir.join("console_log.txt.previous");
    std::fs::write(
        &previous,
        format!("Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0\n"),
    )
    .unwrap();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();

    // Replaced without a rotation: the stale `.previous` is not this file's tail.
    std::fs::remove_file(rig.log_path()).unwrap();
    rig.append_log("GameAction [AppID 730, ActionID 1] : LaunchApp changed task to Completed");
    assert_eq!(rig.watcher.poll(), vec![]);
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn steam_waiting_for_credentials_needs_a_sign_in_until_a_later_state() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();

    rig.login_state("WaitingForCredentials");
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.login_state("WaitingForServerResponse");
    assert_eq!(rig.watcher.poll(), vec![]);
    // Signed out again (a wrong password, say): reported again.
    rig.login_state("WaitingForCredentials");
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
}

#[test]
fn a_login_state_with_nothing_after_it_clears_the_sign_in() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();

    rig.login_state("WaitingForCredentials");
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
    // Trailing space, nothing after: a state that is not WaitingForCredentials.
    rig.append("steamui_login.txt", "[ None ] SetLoginState: \n");
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.login_state("WaitingForCredentials");
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
}

#[test]
fn a_login_line_over_64_kib_is_dropped_and_the_next_one_is_read() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();

    let long = format!(
        "SetLoginState: WaitingForCredentials {}\n",
        "x".repeat(70 * 1024)
    );
    rig.append("steamui_login.txt", &long);
    assert_eq!(rig.watcher.poll(), vec![], "the long line is not parsed");
    rig.login_state("WaitingForCredentials");
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
}

#[test]
fn a_signed_in_start_never_needs_a_sign_in() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    for state in [
        "WaitingForNetwork",
        "WaitingForServerResponse",
        "WaitingForLibraryReady",
        "Success",
    ] {
        rig.login_state(state);
        assert_eq!(rig.watcher.poll(), vec![], "{state}");
    }
}

#[test]
fn a_sign_in_wait_left_by_an_earlier_session_is_not_replayed() {
    let mut rig = Rig::new();
    rig.login_state("WaitingForCredentials");
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );
    assert_eq!(rig.watcher.poll(), vec![]);
}

#[test]
fn a_login_line_is_read_once_its_newline_is_written() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    rig.watcher.poll();
    rig.append("steamui_login.txt", "[ None ] SetLoginState: WaitingFor");
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append("steamui_login.txt", "Credentials - OK\n");
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::SignIn)]
    );
}

#[test]
fn a_dropped_partial_lines_tail_is_not_parsed() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    // 70 KiB with no newline is dropped; the rest of that same line, even
    // carrying a parseable phrase, is ignored up to and including its \n.
    {
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(rig.log_path())
            .unwrap();
        file.write_all(&vec![b'x'; 70 * 1024]).unwrap();
    }
    assert_eq!(rig.watcher.poll(), vec![]);
    {
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(rig.log_path())
            .unwrap();
        writeln!(
            file,
            " Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
        )
        .unwrap();
    }
    assert_eq!(rig.watcher.poll(), vec![]);

    // The discard ends at that newline: a clean line after it parses.
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
}

#[test]
fn a_non_notfound_cmdline_error_marks_the_pass_unreadable() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    write_proc(&rig.proc_root, 200, REAPER);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
    );

    // A directory where cmdline should be: the read fails with EISDIR, not
    // NotFound, so this pass cannot say the reaper is gone.
    std::fs::create_dir_all(rig.proc_root.join("999/cmdline")).unwrap();
    remove_proc(&rig.proc_root, 200);
    assert_eq!(rig.watcher.poll(), vec![]);

    std::fs::remove_dir_all(rig.proc_root.join("999")).unwrap();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
}

const INSTALL_REAPER: &[&str] = &[
    "/home/gamer/.local/share/Steam/ubuntu12_32/reaper",
    "SteamLaunch",
    "AppId=730",
    "Install=1",
    "--",
    "/home/gamer/.local/share/Steam/steamapps/common/Proton/files/bin/iscriptevaluator.exe",
];

#[test]
fn an_install_script_reaper_is_not_the_game() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    write_proc(&rig.proc_root, 200, INSTALL_REAPER);
    assert_eq!(rig.watcher.poll(), vec![]);

    // Its exit is not the game's: no GameExited, still ClientOnly.
    remove_proc(&rig.proc_root, 200);
    assert_eq!(rig.watcher.poll(), vec![]);

    write_proc(&rig.proc_root, 300, REAPER);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
}

#[test]
fn install_script_log_lines_are_ignored() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"/home/gamer/.local/share/Steam/ubuntu12_32/reaper SteamLaunch AppId={APPID} Install=1 -- iscriptevaluator.exe\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log(&format!(
        "Game process removed: AppID {APPID} \"iscriptevaluator.exe\", ProcID 201"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 301, IP 0.0.0.0:0"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
}

#[test]
fn a_log_file_created_after_the_first_poll_is_read_from_zero() {
    let mut rig = Rig::new();
    std::fs::remove_file(rig.log_path()).unwrap();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    std::fs::write(
        rig.log_path(),
        format!("Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0\n"),
    )
    .unwrap();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
}

#[test]
fn cmdline_read_is_capped_at_four_kibibytes() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);

    // The reaper tokens sit past the 4 KiB cap, behind a padding arg: the
    // capped read never sees them, so this process is not the reaper.
    let dir = rig.proc_root.join("200");
    std::fs::create_dir_all(&dir).unwrap();
    let mut cmdline = Vec::new();
    cmdline.extend_from_slice(b"/home/gamer/.local/share/Steam/ubuntu12_64/steam\0");
    cmdline.extend_from_slice(&vec![b'x'; 5 * 1024]);
    cmdline.extend_from_slice(
        b"\0/home/gamer/.local/share/Steam/ubuntu12_32/reaper\0SteamLaunch\0AppId=730\0",
    );
    std::fs::write(dir.join("cmdline"), cmdline).unwrap();

    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );
}

#[test]
fn a_log_tail_over_one_mib_is_read_over_two_passes() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
        ]
    );

    // A 1.2 MiB line, then the added line: the first pass reads only its
    // 1 MiB slice, the second pass reaches the added line.
    {
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(rig.log_path())
            .unwrap();
        let mut padding = vec![b'#'; 1_200_000];
        padding.push(b'\n');
        file.write_all(&padding).unwrap();
    }
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameRunning)]
    );
}
