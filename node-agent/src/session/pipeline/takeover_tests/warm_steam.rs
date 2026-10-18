//! The real runner with the test playing the in-container launch helper on its socket: a
//! parked session, a held runtime and its adoption, and the in-place swap (amendment 39).
//! The source is a test pattern, so no app container ever starts.

use std::collections::BTreeMap;
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use super::{live_webrtc_available, serial, streaming, Peer, Session};
use crate::messages::SessionKind;
use crate::session::container::ContainerSpec;
use crate::session::runner::{AdoptRequest, SessionEvent, SwapRequest};
use crate::session::signaling::SignalMsg;

fn app(flags: &str) -> ContainerSpec {
    ContainerSpec {
        image: "steam".into(),
        env: BTreeMap::from([("STEAM_STARTUP_FLAGS".to_string(), flags.to_string())]),
        ..Default::default()
    }
}

/// A Steam session whose runtime dir has the helper installed.
fn steam_session(
    tag: &str,
    kind: SessionKind,
    flags: &str,
    idle_timeout: Duration,
) -> (tempfile::TempDir, Session) {
    let runtime = tempfile::tempdir().unwrap();
    std::fs::write(runtime.path().join("quasar-launch-helper"), b"").unwrap();
    let dir = runtime.path().to_str().unwrap().to_string();
    let flags = flags.to_string();
    let session = Session::start_with(tag, idle_timeout, move |cfg| {
        cfg.runtime_dir = dir;
        cfg.kind = Some(kind);
        cfg.container = Some(app(&flags));
    });
    (runtime, session)
}

/// The in-container helper: its connection kept alive as the real one keeps it.
struct FakeHelper {
    writer: Arc<Mutex<UnixStream>>,
    reader: BufReader<UnixStream>,
    stop: Arc<AtomicBool>,
}

impl FakeHelper {
    fn connect(runtime: &Path, session_id: &str) -> Self {
        let path =
            crate::session::launch_helper::socket_path(runtime.to_str().unwrap(), session_id);
        let deadline = Instant::now() + Duration::from_secs(10);
        let stream = loop {
            match UnixStream::connect(&path) {
                Ok(stream) => break stream,
                Err(e) if Instant::now() < deadline => {
                    let _ = e;
                    std::thread::sleep(Duration::from_millis(20));
                }
                Err(e) => panic!("the runner never opened its helper socket: {e}"),
            }
        };
        stream
            .set_read_timeout(Some(Duration::from_millis(20)))
            .unwrap();
        let reader = BufReader::new(stream.try_clone().unwrap());
        let writer = Arc::new(Mutex::new(stream));
        let stop = Arc::new(AtomicBool::new(false));
        let (keep, quit) = (writer.clone(), stop.clone());
        std::thread::spawn(move || {
            while !quit.load(Ordering::Relaxed) {
                let _ = keep.lock().unwrap().write_all(b"\n");
                std::thread::sleep(Duration::from_millis(300));
            }
        });
        FakeHelper {
            writer,
            reader,
            stop,
        }
    }

    fn say(&self, line: &str) {
        self.writer
            .lock()
            .unwrap()
            .write_all(format!("{line}\n").as_bytes())
            .unwrap();
    }

    fn launch_state(&self, state: &str) {
        self.say(&format!(
            r#"{{"type":"launch_state","v":1,"state":"{state}"}}"#
        ));
    }

    /// The next command the runner sent, while `session` and `peer` keep talking.
    fn command(&mut self, session: &mut Session, peer: &mut Peer) -> String {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            let mut line = String::new();
            if matches!(self.reader.read_line(&mut line), Ok(n) if n > 0) {
                return line.trim_end().to_string();
            }
            assert!(Instant::now() < deadline, "no command reached the helper");
            session.pump(peer, Duration::from_millis(30), |_, _| false);
        }
    }
}

impl Drop for FakeHelper {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
    }
}

/// Where in the log `what` first holds after `from`.
fn after(log: &[SessionEvent], from: usize, what: impl Fn(&SessionEvent) -> bool) -> Option<usize> {
    log.iter()
        .enumerate()
        .skip(from)
        .find(|(_, e)| what(e))
        .map(|(i, _)| i)
}

fn byes(log: &[SessionEvent]) -> usize {
    log.iter()
        .filter(|e| matches!(e, SessionEvent::Signaling(SignalMsg::Bye)))
        .count()
}

fn end(mut session: Session, peer: &mut Peer) {
    session.stop.store(true, Ordering::SeqCst);
    let ended = session.pump(peer, Duration::from_secs(20), |s, _| s.ended.is_some());
    assert!(ended, "the runner never ended");
    session.thread.take().unwrap().join().unwrap();
}

#[test]
fn a_parked_session_is_never_reaped_or_revealed() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (_runtime, mut session) = steam_session(
        "parked",
        SessionKind::Parked,
        "-silent",
        Duration::from_secs(1),
    );
    session.mute = true;
    let mut nobody = Peer::new();
    session.pump(&mut nobody, Duration::from_secs(4), |s, _| {
        s.ended.is_some()
    });
    assert!(
        session.ended.is_none(),
        "a parked session outlived its 1 s idle window: {:?}",
        session.ended
    );
    assert!(session
        .log
        .iter()
        .any(|e| matches!(e, SessionEvent::Running)));
    assert!(!session
        .log
        .iter()
        .any(|e| matches!(e, SessionEvent::AppBooting | SessionEvent::AppPresented)));
    end(session, &mut nobody);
}

#[test]
fn a_game_exit_holds_the_runtime_and_an_adoption_launches_the_next_game_for_a_new_viewer() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (runtime, mut session) = steam_session(
        "held",
        SessionKind::Play,
        "-applaunch 620",
        Duration::from_secs(60),
    );
    let mut a = Peer::new();
    let up = session.pump(&mut a, Duration::from_secs(20), |_, p| streaming(p));
    assert!(up, "the first viewer never streamed");
    let mut helper = FakeHelper::connect(runtime.path(), &session.id);
    helper.launch_state("game_running");
    helper.launch_state("game_exited");
    let held = session.pump(&mut a, Duration::from_secs(5), |s, _| {
        s.log.iter().any(|e| matches!(e, SessionEvent::Held))
    });
    assert!(held, "the game exit did not hold the runtime");
    let stopping = after(&session.log, 0, |e| {
        matches!(e, SessionEvent::StoppingWith("game exited"))
    })
    .expect("stopping / game exited");
    let bye = after(&session.log, stopping, |e| {
        matches!(e, SessionEvent::Signaling(SignalMsg::Bye))
    })
    .expect("bye after stopping");
    after(&session.log, bye, |e| matches!(e, SessionEvent::Held)).expect("held after bye");
    assert_eq!(byes(&session.log), 1, "one bye for the ended session");

    // The old viewer goes; nothing ends while the runtime waits.
    drop(a);
    let mut quiet = Peer::new();
    session.mute = true;
    session.pump(&mut quiet, Duration::from_secs(2), |s, _| s.ended.is_some());
    assert!(session.ended.is_none(), "{:?}", session.ended);
    session.mute = false;

    let adopted_at = session.log.len();
    let offers = session.offers;
    session
        .adopt
        .send(AdoptRequest {
            kind: SessionKind::Play,
            container: Some(app("-applaunch 730")),
            idle_timeout: Duration::from_secs(60),
            app_boot_timeout: Some(Duration::from_secs(300)),
        })
        .unwrap();
    let mut b = Peer::new();
    assert_eq!(helper.command(&mut session, &mut b), "launch 730");
    let up = session.pump(&mut b, Duration::from_secs(20), |_, p| streaming(p));
    assert!(up, "the adopting session's viewer never streamed");
    assert!(
        session.offers[0] > offers[0] && session.offers[1] > offers[1],
        "a fresh offer per PC: {:?} -> {:?}",
        offers,
        session.offers
    );
    let starting = after(&session.log, adopted_at, |e| {
        matches!(e, SessionEvent::Starting)
    })
    .expect("starting");
    let running = after(&session.log, starting, |e| {
        matches!(e, SessionEvent::Running)
    })
    .expect("running");
    let booting = after(&session.log, running, |e| {
        matches!(e, SessionEvent::AppBooting)
    })
    .expect("app booting");
    assert!(
        after(&session.log, booting, |e| matches!(
            e,
            SessionEvent::AppPresented
        ))
        .is_none(),
        "held until the game shows"
    );

    // Steam's shader figure while it boots, then the game on screen.
    helper.say(r#"{"type":"shader_progress","v":1,"percent":40}"#);
    let progress = session.pump(&mut b, Duration::from_secs(5), |s, _| {
        s.log
            .iter()
            .skip(booting)
            .any(|e| matches!(e, SessionEvent::LaunchProgress(40)))
    });
    assert!(progress, "no launch progress while booting");
    helper.say(r#"{"type":"game_window","v":1,"on_screen":true}"#);
    let shown = session.pump(&mut b, Duration::from_secs(5), |s, _| {
        s.log
            .iter()
            .skip(booting)
            .any(|e| matches!(e, SessionEvent::AppPresented))
    });
    assert!(shown, "the game on screen did not reveal it");
    assert_eq!(byes(&session.log), 1, "the adoption says no bye");
    end(session, &mut b);
}

#[test]
fn a_held_runtime_stopped_before_anyone_adopts_it_ends_with_its_runner() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (runtime, mut session) = steam_session(
        "released",
        SessionKind::Play,
        "-applaunch 620",
        Duration::from_secs(60),
    );
    let mut a = Peer::new();
    assert!(session.pump(&mut a, Duration::from_secs(20), |_, p| streaming(p)));
    let helper = FakeHelper::connect(runtime.path(), &session.id);
    helper.launch_state("game_running");
    helper.launch_state("game_exited");
    assert!(session.pump(&mut a, Duration::from_secs(5), |s, _| {
        s.log.iter().any(|e| matches!(e, SessionEvent::Held))
    }));
    assert!(session.ended.is_none());
    end(session, &mut a);
}

/// A session on game 620 with its helper connected and the game running.
fn on_620(tag: &str) -> (tempfile::TempDir, Session, Peer, FakeHelper) {
    let (runtime, mut session) = steam_session(
        tag,
        SessionKind::Play,
        "-applaunch 620",
        Duration::from_secs(60),
    );
    let mut a = Peer::new();
    assert!(session.pump(&mut a, Duration::from_secs(20), |_, p| streaming(p)));
    let helper = FakeHelper::connect(runtime.path(), &session.id);
    helper.launch_state("game_running");
    session.pump(&mut a, Duration::from_millis(300), |_, _| false);
    session
        .swap
        .send(SwapRequest {
            container: Some(app("-applaunch 730")),
            in_place: true,
        })
        .unwrap();
    (runtime, session, a, helper)
}

#[test]
fn an_in_place_swap_quits_the_game_waits_for_its_sync_and_launches_the_next() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (_runtime, mut session, mut a, mut helper) = on_620("in-place");
    assert_eq!(helper.command(&mut session, &mut a), "quit 620");
    // A going away is the swap, not the player quitting.
    helper.launch_state("game_exited");
    helper.say(
        r#"{"type":"command_done","v":1,"command":"quit","appid":620,"ok":true,"cloud_synced":true}"#,
    );
    assert_eq!(helper.command(&mut session, &mut a), "launch 730");
    helper.say(r#"{"type":"command_done","v":1,"command":"launch","appid":730,"ok":true}"#);
    let swapped = session.pump(&mut a, Duration::from_secs(5), |s, _| s.swapped);
    assert!(swapped, "no swap complete");
    let swapping = after(&session.log, 0, |e| matches!(e, SessionEvent::Swapping)).unwrap();
    let done = after(&session.log, swapping, |e| {
        matches!(e, SessionEvent::SwapDone)
    })
    .unwrap();
    let booting = after(&session.log, done, |e| {
        matches!(e, SessionEvent::AppBooting)
    })
    .expect("app booting after swap complete");
    assert!(session.ended.is_none());
    assert!(!session
        .log
        .iter()
        .any(|e| matches!(e, SessionEvent::StoppingWith(_) | SessionEvent::Held)));
    helper.say(r#"{"type":"game_window","v":1,"on_screen":true}"#);
    assert!(session.pump(&mut a, Duration::from_secs(5), |s, _| {
        s.log
            .iter()
            .skip(booting)
            .any(|e| matches!(e, SessionEvent::AppPresented))
    }));
    end(session, &mut a);
}

#[test]
fn an_in_place_swap_whose_next_game_cannot_launch_ends_the_session() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (_runtime, mut session, mut a, mut helper) = on_620("in-place-fails");
    assert_eq!(helper.command(&mut session, &mut a), "quit 620");
    helper.say(
        r#"{"type":"command_done","v":1,"command":"quit","appid":620,"ok":true,"cloud_synced":false}"#,
    );
    assert_eq!(helper.command(&mut session, &mut a), "launch 730");
    helper.say(r#"{"type":"command_done","v":1,"command":"launch","appid":730,"ok":false}"#);
    let ended = session.pump(&mut a, Duration::from_secs(20), |s, _| s.ended.is_some());
    assert!(ended);
    assert!(matches!(
        session.ended,
        Some(SessionEvent::Stopped {
            detail: Some("game exited"),
            ..
        })
    ));
    assert!(!session.swapped);
    assert!(!session
        .log
        .iter()
        .any(|e| matches!(e, SessionEvent::SwapRolledBack(_))));
    assert_eq!(byes(&session.log), 1);
    session.thread.take().unwrap().join().unwrap();
}

#[test]
fn an_in_place_swap_without_a_connected_helper_swaps_the_container() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (_runtime, mut session) = steam_session(
        "in-place-no-helper",
        SessionKind::Play,
        "-applaunch 620",
        Duration::from_secs(60),
    );
    let mut a = Peer::new();
    assert!(session.pump(&mut a, Duration::from_secs(20), |_, p| streaming(p)));
    session
        .swap
        .send(SwapRequest {
            container: Some(app("-applaunch 730")),
            in_place: true,
        })
        .unwrap();
    let resolved = session.pump(&mut a, Duration::from_secs(30), |s, _| {
        s.swapped
            || s.log
                .iter()
                .any(|e| matches!(e, SessionEvent::SwapRolledBack(_)))
    });
    assert!(resolved, "the container swap never resolved");
    assert!(session.ended.is_none());
    end(session, &mut a);
}
