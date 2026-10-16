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

fn write_proc(proc_root: &Path, pid: u32, name: &str, ppid: u32, args: &[&str]) {
    let dir = proc_root.join(pid.to_string());
    std::fs::create_dir_all(&dir).unwrap();
    let mut cmdline = Vec::new();
    for arg in args {
        cmdline.extend_from_slice(arg.as_bytes());
        cmdline.push(0);
    }
    std::fs::write(dir.join("cmdline"), cmdline).unwrap();
    std::fs::write(
        dir.join("status"),
        format!("Name:\t{name}\nPid:\t{pid}\nPPid:\t{ppid}\n"),
    )
    .unwrap();
    std::fs::write(
        dir.join("stat"),
        format!(
            "{pid} ({name}) S {ppid} {pid} {pid} 0 -1 4194304 100 0 0 0 0 0 0 0 20 0 1 0 100\n"
        ),
    )
    .unwrap();
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
fn launcher_shim_counts_as_running_while_anything_under_the_reaper_lives() {
    let mut rig = Rig::new();
    copy_dir(&fixture("launcher-shim").join("proc"), &rig.proc_root);
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
    );

    // The shim's child goes, the shim itself stays under the reaper: running.
    remove_proc(&rig.proc_root, 402);
    assert_eq!(rig.watcher.poll(), vec![]);

    remove_proc(&rig.proc_root, 401);
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

    rig.append_log(lines.next().unwrap());
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::ClientOnly),
            Report::SteamNeedsUser(NeedsUser::CloudConflict),
        ]
    );

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
    rig.append_log(&format!(
        "Game process added : AppID {APPID} \"/a/rather/long/game/command/line --with-many-arguments\", ProcID 201, IP 0.0.0.0:0"
    ));
    assert_eq!(
        rig.watcher.poll(),
        vec![
            Report::LaunchState(LaunchState::Starting),
            Report::LaunchState(LaunchState::GameRunning),
        ]
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

    // A pid whose cmdline cannot be read (here: a directory in its place),
    // and one with a cmdline but nothing readable for its parent.
    std::fs::create_dir_all(rig.proc_root.join("999/cmdline")).unwrap();
    let partial = rig.proc_root.join("998");
    std::fs::create_dir_all(&partial).unwrap();
    std::fs::write(partial.join("cmdline"), b"/mystery\0").unwrap();
    std::fs::create_dir_all(partial.join("status")).unwrap();
    std::fs::create_dir_all(partial.join("stat")).unwrap();

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
