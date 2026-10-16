use quasar_launch_watch::{
    appid_from_flags, LaunchState, NeedsUser, Report, WatchConfig, Watcher, WindowInfo, Windows,
};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

const APPID: u32 = 730;
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
}

impl FakeWindows {
    fn set(&self, windows: Vec<WindowInfo>) {
        self.state.lock().unwrap().windows = windows;
    }
    fn set_fail(&self, fail: bool) {
        self.state.lock().unwrap().fail = fail;
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
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(self.log_path())
            .unwrap();
        writeln!(file, "{line}").unwrap();
    }
}

fn write_proc(proc_root: &Path, pid: u32, _name: &str, _ppid: u32, args: &[&str]) {
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
    assert_eq!(appid_from_flags("-applaunch 730"), Some(730));
    assert_eq!(
        appid_from_flags("-bigpicture -applaunch 730 -silent extra"),
        Some(730)
    );
}

#[test]
fn first_poll_is_starting_then_client_only_and_never_exits_before_running() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);

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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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

    rig.windows.set_fail(false);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: true }]
    );
}

#[test]
fn game_window_is_reported_on_each_change() {
    let mut rig = Rig::new();
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::Starting)]
    );

    rig.windows.set(vec![game_window()]);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: true }]
    );
    assert_eq!(rig.watcher.poll(), vec![]);

    rig.windows.set(vec![]);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: false }]
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
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::GameWindow { on_screen: true }]
    );
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

    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::ClientOnly)]
    );
}

#[test]
fn routine_cloud_sync_is_not_needs_user() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::CloudConflict)]
    );
    assert_eq!(rig.watcher.poll(), vec![]);

    // Cleared by the continues line; a later wait reports again.
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"SynchronizingCloud\""
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to CreatingProcess \"\""
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::Other)]
    );

    // Cleared by the next changed-task line for the app.
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp changed task to WaitingGameWindow"
    ));
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log(&format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to CreatingProcess \"\""
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::SteamNeedsUser(NeedsUser::Other)]
    );
}

#[test]
fn reaper_seen_then_gone_on_a_readable_tree_exits_despite_a_stale_added_line() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
    write_proc(&rig.proc_root, 200, "reaper", 100, REAPER);
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
    write_proc(&rig.proc_root, 200, "reaper", 100, REAPER);
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
    write_proc(
        &rig.proc_root,
        200,
        "reaper",
        100,
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
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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
fn a_partial_log_line_past_the_cap_is_dropped() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
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

    // 70 KiB with no newline: dropped, and the next line still parses.
    {
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(rig.log_path())
            .unwrap();
        file.write_all(&vec![b'x'; 70 * 1024]).unwrap();
    }
    assert_eq!(rig.watcher.poll(), vec![]);
    rig.append_log(&format!(
        "\nGame process removed: AppID {APPID} \"game\", ProcID 201"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![Report::LaunchState(LaunchState::GameExited)]
    );
}

#[test]
fn a_non_notfound_cmdline_error_marks_the_pass_unreadable() {
    let mut rig = Rig::new();
    write_proc(&rig.proc_root, 100, "steam", 1, STEAM);
    write_proc(&rig.proc_root, 200, "reaper", 100, REAPER);
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
