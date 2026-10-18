//! #759: the agent's commands, Steam's shader progress and controller notice, the exit
//! cloud sync and the quit sequence, against fixture `/proc` trees and Steam logs.

use quasar_launch_watch::{
    game_running, game_tree, parse_command, CloudEnd, CloudSync, Command, Quit, QuitAction, Report,
    WatchConfig, Watcher, WindowInfo, Windows, QUIT_CLOSE_GRACE, QUIT_CLOUD_WAIT, QUIT_KILL_GRACE,
    QUIT_TERM_GRACE,
};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

const APPID: u32 = 1145360;
const PAST_USER_WAIT: Duration = Duration::from_millis(2100);

struct NoWindows;

impl Windows for NoWindows {
    fn mapped(&mut self) -> std::io::Result<Vec<WindowInfo>> {
        Ok(vec![WindowInfo {
            steam_game: Some(APPID),
            wm_class: "steam_app_1145360".into(),
            title: "Hades".into(),
        }])
    }
    fn focused_app(&mut self) -> std::io::Result<Option<u32>> {
        Ok(None)
    }
}

struct Dirs {
    _temp: tempfile::TempDir,
    proc_root: PathBuf,
    logs: PathBuf,
}

fn dirs() -> Dirs {
    let temp = tempfile::tempdir().unwrap();
    let proc_root = temp.path().join("proc");
    let logs = temp.path().join("logs");
    std::fs::create_dir_all(&proc_root).unwrap();
    std::fs::create_dir_all(&logs).unwrap();
    Dirs {
        _temp: temp,
        proc_root,
        logs,
    }
}

fn append(path: &Path, line: &str) {
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .unwrap();
    writeln!(file, "{line}").unwrap();
}

fn process(proc_root: &Path, pid: u32, ppid: u32, args: &[&str]) {
    let dir = proc_root.join(pid.to_string());
    std::fs::create_dir_all(&dir).unwrap();
    let mut cmdline = Vec::new();
    for arg in args {
        cmdline.extend_from_slice(arg.as_bytes());
        cmdline.push(0);
    }
    std::fs::write(dir.join("cmdline"), cmdline).unwrap();
    // A command name with a space and a parenthesis, as a game may name itself.
    std::fs::write(
        dir.join("stat"),
        format!("{pid} (my game) (x) S {ppid} {pid} {pid} 0 -1"),
    )
    .unwrap();
}

fn reaper(proc_root: &Path, pid: u32) {
    let appid = format!("AppId={APPID}");
    process(
        proc_root,
        pid,
        1,
        &[
            "/home/u/.local/share/Steam/ubuntu12_32/reaper",
            "SteamLaunch",
            &appid,
            "--",
            "game",
        ],
    );
}

fn gone(proc_root: &Path, pid: u32) {
    std::fs::remove_dir_all(proc_root.join(pid.to_string())).unwrap();
}

#[test]
fn only_the_closed_command_set_parses() {
    assert_eq!(parse_command("launch 620"), Some(Command::Launch(620)));
    assert_eq!(
        parse_command("launch 4294967295"),
        Some(Command::Launch(u32::MAX))
    );
    assert_eq!(parse_command("quit 1145360"), Some(Command::Quit(1145360)));
    assert_eq!(parse_command("bigpicture"), Some(Command::BigPicture));
    for line in [
        "",
        "launch",
        "launch ",
        "launch 0",
        "launch 0620",
        "launch +620",
        "launch -620",
        "launch 62o",
        "launch 620 ",
        " launch 620",
        "launch  620",
        "launch 620 730",
        "launch 4294967296",
        "launch 12345678901",
        "launch ６２０",
        "LAUNCH 620",
        "quit",
        "quit abc",
        "bigpicture ",
        "bigpicture 620",
        "steam -applaunch 620",
        "launch 620; rm -rf /",
        "exec /bin/sh",
    ] {
        assert_eq!(parse_command(line), None, "{line:?}");
    }
}

fn watcher(d: &Dirs) -> Watcher {
    std::fs::write(d.logs.join("console_log.txt"), "").unwrap();
    std::fs::write(
        d.logs.join("shader_log.txt"),
        "[2026-10-18 12:00:00] Still replaying 1145360 (90%, 1/2).\n",
    )
    .unwrap();
    Watcher::new(
        WatchConfig {
            proc_root: d.proc_root.clone(),
            steam_log_dir: d.logs.clone(),
            appid: APPID,
        },
        Box::new(NoWindows),
    )
}

fn shader(reports: Vec<Report>) -> Vec<u8> {
    reports
        .into_iter()
        .filter_map(|r| match r {
            Report::ShaderProgress(p) => Some(p),
            _ => None,
        })
        .collect()
}

#[test]
fn shader_progress_is_steams_figure_for_the_target_on_change_only() {
    let d = dirs();
    let mut w = watcher(&d);
    // A line already in the log belongs to an earlier session.
    assert_eq!(shader(w.poll()), Vec::<u8>::new());
    let log = d.logs.join("shader_log.txt");
    append(
        &log,
        "[2026-10-18 12:59:25] Starting replay of FOZ databases for AppID 1145360 with x in:",
    );
    append(
        &log,
        "[2026-10-18 12:59:25] Still replaying 1145360 (50%, 0/13339).",
    );
    assert_eq!(shader(w.poll()), vec![50]);
    // The same figure again, another app's, malformed ones: no report.
    append(
        &log,
        "[2026-10-18 12:59:30] Still replaying 1145360 (50%, 982/13339).",
    );
    append(
        &log,
        "[2026-10-18 12:59:31] Still replaying 2344520 (70%, 0/140815).",
    );
    append(
        &log,
        "[2026-10-18 12:59:31] Still replaying 1145360 (101%, 0/1).",
    );
    append(
        &log,
        "[2026-10-18 12:59:31] Still replaying 1145360 (-5%, 0/1).",
    );
    append(&log, "Still replaying 1145360 (60%, 0/1).");
    append(
        &log,
        "[2026-10-18 12:59:31] Still replaying 11453600 (60%, 0/1).",
    );
    assert_eq!(shader(w.poll()), Vec::<u8>::new());
    // Two in one pass: the latest is what is reported.
    append(
        &log,
        "[2026-10-18 12:59:34] Still replaying 1145360 (75%, 1000/1378).",
    );
    append(
        &log,
        "[2026-10-18 12:59:35] Still replaying 1145360 (100%, 1378/1378).",
    );
    assert_eq!(shader(w.poll()), vec![100]);
}

#[test]
fn a_standing_controller_notice_is_told_from_a_quick_one() {
    let d = dirs();
    let mut w = watcher(&d);
    w.poll();
    assert!(
        w.has_window(APPID),
        "the window list of the last pass is kept"
    );
    let log = d.logs.join("console_log.txt");
    let wait = format!(
        "GameAction [AppID {APPID}, ActionID 1] : LaunchApp waiting for user response to ShowInterstitials \"\""
    );
    let t0 = Instant::now();
    append(&log, &wait);
    w.poll_at(t0);
    assert!(!w.interstitial_standing(t0));
    assert!(w.interstitial_standing(t0 + PAST_USER_WAIT));
    // Answered: no longer standing.
    append(
        &log,
        &format!("GameAction [AppID {APPID}, ActionID 1] : LaunchApp continues with user response \"ShowInterstitials\""),
    );
    w.poll_at(t0 + PAST_USER_WAIT);
    assert!(!w.interstitial_standing(t0 + PAST_USER_WAIT * 10));
    // Another app's notice, and another wait for the target, are not this one.
    append(
        &log,
        "GameAction [AppID 620, ActionID 2] : LaunchApp waiting for user response to ShowInterstitials \"\"",
    );
    append(
        &log,
        &format!("GameAction [AppID {APPID}, ActionID 3] : LaunchApp waiting for user response to SynchronizingCloud \"\""),
    );
    let t1 = t0 + PAST_USER_WAIT * 11;
    w.poll_at(t1);
    assert!(!w.interstitial_standing(t1 + PAST_USER_WAIT));
    // The game starting ends a notice too.
    append(&log, &wait);
    w.poll_at(t1);
    append(
        &log,
        &format!("Game process added : AppID {APPID} \"game\", ProcID 201, IP 0.0.0.0:0"),
    );
    w.poll_at(t1);
    assert!(!w.interstitial_standing(t1 + PAST_USER_WAIT));
}

fn cloud_line(appid: u32, what: &str) -> String {
    format!("[2026-10-18 13:13:18] [AppID {appid}] {what}")
}

#[test]
fn the_exit_sync_ends_only_after_its_start_for_that_app() {
    let d = dirs();
    let log = d.logs.join("cloud_log.txt");
    // An earlier session's end, already in the log.
    append(&log, &cloud_line(APPID, "Starting sync (up,AC Exit,)"));
    append(&log, &cloud_line(APPID, "Upload complete in build list"));
    let mut sync = CloudSync::new(&d.logs, APPID);
    assert_eq!(sync.poll(), None);
    // An end with no start, another app's sync, and near misses.
    append(&log, &cloud_line(APPID, "Upload complete in build list"));
    append(&log, &cloud_line(241100, "Starting sync (up,AC Exit,)"));
    append(&log, &cloud_line(241100, "Upload complete in build list"));
    append(&log, &cloud_line(APPID, "Starting sync (down,AC Launch,)"));
    assert_eq!(sync.poll(), None);
    append(&log, &cloud_line(APPID, "Starting sync (up,AC Exit,)"));
    append(
        &log,
        &cloud_line(APPID, "Upload batch initiated, ID: 1, ChangeNumber: 20"),
    );
    append(&log, &cloud_line(APPID, "Upload complete, result "));
    append(&log, &cloud_line(APPID, "Upload complete, result OK!"));
    append(&log, "[AppID 1145360] Upload complete, result OK");
    assert_eq!(sync.poll(), None);
    append(&log, &cloud_line(APPID, "Upload complete, result OK"));
    assert_eq!(sync.poll(), Some(CloudEnd::Uploaded("OK".into())));

    let mut nothing = CloudSync::new(&d.logs, APPID);
    append(&log, &cloud_line(APPID, "Starting sync (up,AC Exit,)"));
    append(&log, &cloud_line(APPID, "Upload complete in build list"));
    assert_eq!(nothing.poll(), Some(CloudEnd::NothingToUpload));
}

#[test]
fn the_exit_sync_is_followed_across_steams_rotation() {
    let d = dirs();
    let log = d.logs.join("cloud_log.txt");
    append(&log, "[2026-10-18 13:00:00] old line");
    let mut sync = CloudSync::new(&d.logs, APPID);
    append(&log, &cloud_line(APPID, "Starting sync (up,AC Exit,)"));
    // Steam rotates mid-sync: the start went to the previous file, the end to the new one.
    std::fs::rename(&log, d.logs.join("cloud_log.previous.txt")).unwrap();
    append(&log, &cloud_line(APPID, "Upload complete, result OK"));
    let mut end = None;
    for _ in 0..3 {
        end = end.or(sync.poll());
    }
    assert_eq!(end, Some(CloudEnd::Uploaded("OK".into())));
}

#[test]
fn the_game_tree_is_the_reaper_and_everything_under_it() {
    let d = dirs();
    process(&d.proc_root, 10, 1, &["steam"]);
    reaper(&d.proc_root, 20);
    process(&d.proc_root, 21, 20, &["proton"]);
    process(&d.proc_root, 22, 21, &["game.exe"]);
    process(&d.proc_root, 30, 10, &["steamwebhelper"]);
    let mut tree = game_tree(&d.proc_root, APPID);
    tree.sort();
    assert_eq!(tree, vec![20, 21, 22]);
    assert_eq!(game_tree(&d.proc_root, 620), Vec::<u32>::new());
    assert_eq!(game_running(&d.proc_root, APPID), Some(true));
    gone(&d.proc_root, 20);
    assert_eq!(game_running(&d.proc_root, APPID), Some(false));
    assert_eq!(game_running(&d.proc_root.join("missing"), APPID), None);
}

#[test]
fn a_game_that_closes_ends_the_quit_at_its_exit_sync() {
    let d = dirs();
    reaper(&d.proc_root, 20);
    let log = d.logs.join("cloud_log.txt");
    let t0 = Instant::now();
    let mut quit = Quit::new(&d.proc_root, &d.logs, APPID, t0);
    assert_eq!(quit.poll(t0 + Duration::from_secs(2)), None);
    gone(&d.proc_root, 20);
    assert_eq!(
        quit.poll(t0 + Duration::from_secs(3)),
        None,
        "waits for the sync"
    );
    append(&log, &cloud_line(APPID, "Starting sync (up,AC Exit,)"));
    append(&log, &cloud_line(APPID, "Upload complete in build list"));
    assert_eq!(
        quit.poll(t0 + Duration::from_secs(4)),
        Some(QuitAction::Done(Some(CloudEnd::NothingToUpload)))
    );
    assert_eq!(quit.poll(t0 + Duration::from_secs(5)), None, "once");
}

#[test]
fn a_sync_that_never_ends_is_waited_for_sixty_seconds() {
    let d = dirs();
    reaper(&d.proc_root, 20);
    let t0 = Instant::now();
    let mut quit = Quit::new(&d.proc_root, &d.logs, APPID, t0);
    gone(&d.proc_root, 20);
    assert_eq!(quit.poll(t0), None);
    append(
        &d.logs.join("cloud_log.txt"),
        &cloud_line(APPID, "Starting sync (up,AC Exit,)"),
    );
    assert_eq!(
        quit.poll(t0 + QUIT_CLOUD_WAIT - Duration::from_millis(1)),
        None
    );
    assert_eq!(
        quit.poll(t0 + QUIT_CLOUD_WAIT),
        Some(QuitAction::Done(None))
    );
}

#[test]
fn a_quit_for_a_game_that_is_not_running_is_done_at_once() {
    let d = dirs();
    let t0 = Instant::now();
    let mut quit = Quit::new(&d.proc_root, &d.logs, APPID, t0);
    assert_eq!(
        quit.poll(t0),
        Some(QuitAction::Done(None)),
        "no sync to wait for"
    );
    assert_eq!(quit.poll(t0 + QUIT_CLOUD_WAIT), None);
}

#[test]
fn a_game_that_will_not_close_is_terminated_then_killed() {
    let d = dirs();
    reaper(&d.proc_root, 20);
    process(&d.proc_root, 21, 20, &["game.exe"]);
    let t0 = Instant::now();
    let mut quit = Quit::new(&d.proc_root, &d.logs, APPID, t0);
    assert_eq!(
        quit.poll(t0 + QUIT_CLOSE_GRACE - Duration::from_millis(1)),
        None
    );
    let t1 = t0 + QUIT_CLOSE_GRACE;
    let Some(QuitAction::Terminate(mut pids)) = quit.poll(t1) else {
        panic!("SIGTERM after the close grace");
    };
    pids.sort();
    assert_eq!(pids, vec![20, 21]);
    assert_eq!(
        quit.poll(t1 + QUIT_TERM_GRACE - Duration::from_millis(1)),
        None
    );
    let t2 = t1 + QUIT_TERM_GRACE;
    assert!(matches!(quit.poll(t2), Some(QuitAction::Kill(_))));
    assert_eq!(quit.poll(t2 + QUIT_KILL_GRACE), Some(QuitAction::Failed));
    assert_eq!(quit.poll(t2 + QUIT_KILL_GRACE * 2), None);
}

#[test]
fn a_game_gone_after_sigterm_still_waits_for_its_sync() {
    let d = dirs();
    reaper(&d.proc_root, 20);
    let t0 = Instant::now();
    let mut quit = Quit::new(&d.proc_root, &d.logs, APPID, t0);
    assert!(matches!(
        quit.poll(t0 + QUIT_CLOSE_GRACE),
        Some(QuitAction::Terminate(_))
    ));
    gone(&d.proc_root, 20);
    let t1 = t0 + QUIT_CLOSE_GRACE + Duration::from_secs(1);
    assert_eq!(quit.poll(t1), None);
    append(
        &d.logs.join("cloud_log.txt"),
        &cloud_line(APPID, "Starting sync (up,AC Exit,)"),
    );
    append(
        &d.logs.join("cloud_log.txt"),
        &cloud_line(APPID, "Upload complete, result OK"),
    );
    assert_eq!(
        quit.poll(t1 + Duration::from_secs(1)),
        Some(QuitAction::Done(Some(CloudEnd::Uploaded("OK".into()))))
    );
}

#[test]
fn an_unreadable_proc_never_reads_as_gone() {
    let d = dirs();
    let t0 = Instant::now();
    let mut quit = Quit::new(&d.proc_root.join("missing"), &d.logs, APPID, t0);
    assert_eq!(quit.poll(t0 + Duration::from_secs(5)), None);
    assert!(matches!(
        quit.poll(t0 + QUIT_CLOSE_GRACE),
        Some(QuitAction::Terminate(_))
    ));
}
