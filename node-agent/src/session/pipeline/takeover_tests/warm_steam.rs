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
fn a_parked_session_is_never_reaped_or_revealed_and_a_claim_streams_on_it() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (runtime, mut session) = steam_session(
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

    // The library claim: the parked transport offered and was never answered; the claim's
    // viewer is the first peer it ever has.
    let mut helper = FakeHelper::connect(runtime.path(), &session.id);
    session.mute = false;
    let adopted_at = adopt(&mut session, "-applaunch 730");
    let mut b = Peer::new();
    assert_eq!(helper.command(&mut session, &mut b), "launch 730");
    let up = session.pump(&mut b, Duration::from_secs(20), |_, p| streaming(p));
    assert!(
        up,
        "the claim's viewer never streamed on the parked runtime"
    );
    assert_one_session_start(&session.log, adopted_at);
    helper.say(r#"{"type":"game_window","v":1,"on_screen":true}"#);
    let shown = session.pump(&mut b, Duration::from_secs(5), |s, _| {
        s.log
            .iter()
            .skip(adopted_at)
            .any(|e| matches!(e, SessionEvent::AppPresented))
    });
    assert!(shown, "the game on screen did not reveal it");
    end(session, &mut b);
}

/// Hand the runtime to a play session for `flags`; where its reports start in the log.
fn adopt(session: &mut Session, flags: &str) -> usize {
    adopt_as(session, flags, Some(SessionKind::Play))
}

fn adopt_as(session: &mut Session, flags: &str, kind: Option<SessionKind>) -> usize {
    let at = session.log.len();
    session
        .adopt
        .send(AdoptRequest {
            kind,
            container: Some(app(flags)),
            idle_timeout: Duration::from_secs(60),
            app_boot_timeout: Some(Duration::from_secs(300)),
        })
        .unwrap();
    at
}

/// One `app booting` (the adopting session's `running`: a `running` report without it would
/// let a client reveal a stream the game has not drawn on, #749) and one `effective_media`
/// for the adopting session, the restart offers notwithstanding.
fn assert_one_session_start(log: &[SessionEvent], from: usize) {
    let count = |what: fn(&SessionEvent) -> bool| log.iter().skip(from).filter(|e| what(e)).count();
    assert_eq!(
        count(|e| matches!(e, SessionEvent::Starting)),
        1,
        "starting"
    );
    assert_eq!(count(|e| matches!(e, SessionEvent::Running)), 0, "running");
    assert_eq!(
        count(|e| matches!(e, SessionEvent::AppBooting)),
        1,
        "app booting"
    );
    assert_eq!(
        count(|e| matches!(e, SessionEvent::EffectiveMedia(_))),
        1,
        "effective media"
    );
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
    // #749: a fresh launch that boots an app reports `running` AS `app booting`, before its
    // first `effective_media`; a plain `Running` would let a client reveal an undrawn stream.
    assert!(
        !session
            .log
            .iter()
            .any(|e| matches!(e, SessionEvent::Running)),
        "a fresh app launch reported a plain running: {:?}",
        session.log
    );
    let booting = after(&session.log, 0, |e| matches!(e, SessionEvent::AppBooting))
        .expect("the fresh launch's app booting");
    assert!(
        after(&session.log, 0, |e| matches!(
            e,
            SessionEvent::EffectiveMedia(_)
        ))
        .is_some_and(|media| media > booting),
        "effective media before app booting: {:?}",
        session.log
    );
    let mut helper = FakeHelper::connect(runtime.path(), &session.id);
    helper.launch_state("game_running");
    helper.launch_state("game_exited");
    let held = session.pump(&mut a, Duration::from_secs(5), |s, _| {
        s.log.iter().any(|e| matches!(e, SessionEvent::Held))
    });
    assert!(held, "the game exit did not hold the runtime");
    // The control plane parks only a session still `running`: bye, then the held stop, and
    // no `stopping` of any kind before it.
    let bye = after(&session.log, 0, |e| {
        matches!(e, SessionEvent::Signaling(SignalMsg::Bye))
    })
    .expect("bye");
    assert!(
        matches!(session.log.get(bye + 1), Some(SessionEvent::Held)),
        "the held stop right after the bye: {:?}",
        &session.log[bye..]
    );
    assert!(!session
        .log
        .iter()
        .any(|e| matches!(e, SessionEvent::Stopping | SessionEvent::StoppingWith(_))));
    assert_eq!(byes(&session.log), 1, "one bye for the ended session");

    // The old viewer goes; nothing ends while the runtime waits.
    drop(a);
    let mut quiet = Peer::new();
    session.mute = true;
    session.pump(&mut quiet, Duration::from_secs(2), |s, _| s.ended.is_some());
    assert!(session.ended.is_none(), "{:?}", session.ended);
    session.mute = false;

    // A swap that reaches the runner after the game exit goes nowhere.
    let before_swap = session.log.len();
    session
        .swap
        .send(SwapRequest {
            container: Some(app("-applaunch 440")),
            in_place: true,
        })
        .unwrap();
    session.pump(&mut quiet, Duration::from_secs(1), |_, _| false);
    assert!(
        !session
            .log
            .iter()
            .skip(before_swap)
            .any(|e| matches!(e, SessionEvent::Swapping)),
        "a held runtime takes no swap"
    );

    let offers = session.offers;
    let adopted_at = adopt(&mut session, "-applaunch 730");
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
    assert_one_session_start(&session.log, adopted_at);
    let starting = after(&session.log, adopted_at, |e| {
        matches!(e, SessionEvent::Starting)
    })
    .expect("starting");
    let booting = after(&session.log, starting, |e| {
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

/// The switch off: the control plane claims the parked runtime with no `kind`. That session's
/// game exit ends it as before #759 (#758's stopping, bye, stopped), and nothing is held.
#[test]
fn a_claim_without_kind_ends_on_its_game_exit_as_before() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (runtime, mut session) = steam_session(
        "kindless",
        SessionKind::Parked,
        "-silent",
        Duration::from_secs(60),
    );
    session.mute = true;
    let mut nobody = Peer::new();
    session.pump(&mut nobody, Duration::from_secs(2), |s, _| {
        s.log.iter().any(|e| matches!(e, SessionEvent::Running))
    });
    let mut helper = FakeHelper::connect(runtime.path(), &session.id);
    session.mute = false;
    let adopted_at = adopt_as(&mut session, "-applaunch 730", None);
    let mut b = Peer::new();
    assert_eq!(helper.command(&mut session, &mut b), "launch 730");
    assert!(session.pump(&mut b, Duration::from_secs(20), |_, p| streaming(p)));
    helper.launch_state("game_running");
    helper.launch_state("game_exited");
    let ended = session.pump(&mut b, Duration::from_secs(20), |s, _| s.ended.is_some());
    assert!(ended, "the game exit did not end the session");
    assert!(
        matches!(
            session.ended,
            Some(SessionEvent::Stopped {
                detail: Some("game exited"),
                ..
            })
        ),
        "{:?}",
        session.ended
    );
    let stopping = after(&session.log, adopted_at, |e| {
        matches!(e, SessionEvent::StoppingWith("game exited"))
    })
    .expect("stopping / game exited");
    assert!(
        matches!(
            session.log.get(stopping + 1),
            Some(SessionEvent::Signaling(SignalMsg::Bye))
        ),
        "bye right after the stopping: {:?}",
        &session.log[stopping..]
    );
    assert!(!session.log.iter().any(|e| matches!(e, SessionEvent::Held)));
    assert_eq!(byes(&session.log), 1);
    session.thread.take().unwrap().join().unwrap();
}

struct NoSound;

impl crate::session::teardown::Sidecar for NoSound {
    fn server_uri(&self) -> String {
        "unix:/nonexistent/native".into()
    }
    fn socket_dir(&self) -> std::path::PathBuf {
        "/nonexistent".into()
    }
    fn adopt_budget(&mut self, _: crate::session::teardown::RetryBudget) {}
    fn stop(&mut self) {}
}

/// After an adoption or an in-place swap the container runs another app; a rollback's
/// relaunch must hand the engine that app, not the one the container was built for.
#[test]
fn a_relaunch_after_set_app_spec_launches_the_app_the_container_now_runs() {
    use crate::session::source::{AppSource, SessionResources};
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let cfg = super::host_cfg(false);
    let res = SessionResources::with_sidecar("set-app-spec", Box::new(NoSound));
    let sink = format!("set-app-spec-{}", std::process::id());
    let mut source = AppSource::new(
        &cfg,
        "set-app-spec",
        &sink,
        &sink,
        &res,
        Some(app("-silent")),
    )
    .expect("app source");
    let flags = |source: &AppSource| {
        source
            .launch_spec()
            .map(|spec| spec.env["STEAM_STARTUP_FLAGS"].clone())
    };
    assert_eq!(flags(&source).as_deref(), Some("-silent"));
    source.set_app_spec(app("-applaunch 730"));
    assert_eq!(flags(&source).as_deref(), Some("-applaunch 730"));

    // A source with no app stays one.
    let mut bare = AppSource::new(&cfg, "set-app-spec", &sink, &sink, &res, None).expect("bare");
    bare.set_app_spec(app("-applaunch 730"));
    assert!(bare.launch_spec().is_none());
}
