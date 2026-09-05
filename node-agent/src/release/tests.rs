use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixListener;
use std::sync::atomic::{AtomicBool, Ordering as AtomicOrdering};

use super::*;

const REQ: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";
const DIGEST: &str = "sha256:9f2c000000000000000000000000000000000000000000000000000000000abc";
const PREV: &str = "sha256:1b7e000000000000000000000000000000000000000000000000000000000def";

/// A real updater on a real unix socket, because the socket IS the interface:
/// a mocked transport would prove nothing about the hand-rolled HTTP/1.0 client.
struct FakeUpdater {
    _dir: tempfile::TempDir,
    socket: PathBuf,
    results: PathBuf,
    /// Every request line the fake saw, so a test can assert the socket was
    /// never touched at all.
    seen: Arc<Mutex<Vec<String>>>,
    /// Status + body for POST /v1/apply.
    apply_reply: Arc<Mutex<(u16, String)>>,
    /// The current result JSON, or None for 404.
    result: Arc<Mutex<Option<String>>>,
    stop: Arc<AtomicBool>,
}

impl FakeUpdater {
    fn start() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("u.sock");
        let results = dir.path().join("results");
        std::fs::create_dir_all(&results).unwrap();

        let listener = UnixListener::bind(&socket).unwrap();
        listener.set_nonblocking(false).unwrap();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let apply_reply = Arc::new(Mutex::new((
            202u16,
            serde_json::json!({"request_id": REQ, "previous": [], "commands": []}).to_string(),
        )));
        let result: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));
        let stop = Arc::new(AtomicBool::new(false));

        {
            let (seen, apply_reply, result, stop) = (
                seen.clone(),
                apply_reply.clone(),
                result.clone(),
                stop.clone(),
            );
            std::thread::spawn(move || {
                for conn in listener.incoming() {
                    if stop.load(AtomicOrdering::Relaxed) {
                        return;
                    }
                    let Ok(mut conn) = conn else { return };
                    let mut reader = BufReader::new(conn.try_clone().unwrap());
                    let mut line = String::new();
                    if reader.read_line(&mut line).is_err() {
                        continue;
                    }
                    let request_line = line.trim_end().to_string();
                    seen.lock().unwrap().push(request_line.clone());
                    let mut len = 0usize;
                    loop {
                        let mut h = String::new();
                        if reader.read_line(&mut h).unwrap_or(0) == 0 || h == "\r\n" || h == "\n" {
                            break;
                        }
                        if let Some(v) = h.to_ascii_lowercase().strip_prefix("content-length:") {
                            len = v.trim().parse().unwrap_or(0);
                        }
                    }
                    if len > 0 {
                        let mut body = vec![0u8; len];
                        let _ = reader.read_exact(&mut body);
                    }
                    let (status, body) = if request_line.starts_with("POST /v1/apply") {
                        apply_reply.lock().unwrap().clone()
                    } else if request_line.starts_with("GET /v1/results/") {
                        match result.lock().unwrap().clone() {
                            Some(r) => (200, r),
                            None => (404, "{\"reason\":\"invalid\"}".to_string()),
                        }
                    } else {
                        (404, "{}".to_string())
                    };
                    let _ = write!(
                        conn,
                        "HTTP/1.0 {status} X\r\nContent-Type: application/json\r\n\r\n{body}"
                    );
                    let _ = conn.flush();
                }
            });
        }

        FakeUpdater {
            _dir: dir,
            socket,
            results,
            seen,
            apply_reply,
            result,
            stop,
        }
    }

    fn mgr(&self) -> Arc<ReleaseManager> {
        ReleaseManager::new(self.socket.clone(), self.results.clone())
    }

    fn set_result(&self, state: &str, reason: Option<&str>) {
        *self.result.lock().unwrap() = Some(
            serde_json::json!({
                "request_id": REQ,
                "state": state,
                "reason": reason,
                "components": [{"name": "node-agent", "image": "ghcr.io/accreleus/quasar/quasar-node-agent", "digest": DIGEST}],
                "previous": [{"name": "node-agent", "digest": PREV}],
                "output": "",
                "started_at": "2026-09-05T11:04:02Z",
                "updated_at": "2026-09-05T11:04:02Z",
                "finished_at": null,
            })
            .to_string(),
        );
    }
}

impl Drop for FakeUpdater {
    fn drop(&mut self) {
        self.stop.store(true, AtomicOrdering::Relaxed);
    }
}

fn components() -> Vec<ReleaseComponent> {
    vec![ReleaseComponent {
        name: "node-agent".into(),
        image: "ghcr.io/accreleus/quasar/quasar-node-agent".into(),
        digest: DIGEST.into(),
    }]
}

fn release() -> ReleaseInfo {
    ReleaseInfo {
        id: "r1".into(),
        version: Some("0.2.0".into()),
        source_commit: "1".repeat(40),
    }
}

fn ack_of(msg: &AgentMsg) -> (bool, Option<String>) {
    match msg {
        AgentMsg::Ack { ok, error, .. } => (*ok, error.clone()),
        other => panic!("expected an ack, got {other:?}"),
    }
}

fn state_of(msg: &AgentMsg) -> (String, Option<String>, Vec<ReleasePrevious>) {
    match msg {
        AgentMsg::ReleaseState {
            state,
            reason,
            previous,
            ..
        } => (state.clone(), reason.clone(), previous.clone()),
        other => panic!("expected a release_state, got {other:?}"),
    }
}

#[tokio::test]
async fn accepts_and_relays_every_state_change() {
    let fake = FakeUpdater::start();
    let mgr = fake.mgr();
    let (tx, mut rx) = mpsc::channel(32);
    let _guard = mgr.attach_upstream(tx);

    fake.set_result("pending", None);
    let (ok, err) =
        ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), components(), false));
    assert!(ok, "expected acceptance, got {err:?}");

    let mut states = Vec::new();
    for next in ["pulling", "recreating", "succeeded"] {
        let msg = rx.recv().await.expect("a release_state per transition");
        let (state, _reason, previous) = state_of(&msg);
        // `previous` is present in EVERY state, not only on failure: it is what
        // makes the restore recipe copy-paste from any observation.
        assert_eq!(previous[0].digest.as_deref(), Some(PREV));
        states.push(state);
        fake.set_result(next, None);
    }
    let (last, _, _) = state_of(&rx.recv().await.unwrap());
    states.push(last);
    assert_eq!(states, ["pending", "pulling", "recreating", "succeeded"]);
}

#[tokio::test]
async fn rejects_a_control_plane_component_without_contacting_the_updater() {
    let fake = FakeUpdater::start();
    let mgr = fake.mgr();
    let mut c = components();
    c[0].name = "control-plane".into();
    let (ok, err) = ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), c, false));
    assert!(!ok);
    assert_eq!(err.as_deref(), Some("invalid"));
    assert!(
        fake.seen.lock().unwrap().is_empty(),
        "a control-plane component must be refused unconditionally and without contacting the updater"
    );
}

#[tokio::test]
async fn rejects_malformed_requests() {
    let fake = FakeUpdater::start();
    let mgr = fake.mgr();

    let mut bad_digest = components();
    bad_digest[0].digest = "sha256:nope".into();
    assert_eq!(
        ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), bad_digest, false)).1,
        Some("digest_malformed".into())
    );

    let mut tagged = components();
    tagged[0].image = "ghcr.io/accreleus/quasar/quasar-node-agent:latest".into();
    assert_eq!(
        ack_of(&mgr.handle_apply("c2".into(), REQ.into(), release(), tagged, false)).1,
        Some("invalid".into())
    );

    assert_eq!(
        ack_of(&mgr.handle_apply(
            "c3".into(),
            "not-a-uuid".into(),
            release(),
            components(),
            false
        ))
        .1,
        Some("invalid".into())
    );
    assert_eq!(
        ack_of(&mgr.handle_apply("c4".into(), REQ.into(), release(), vec![], false)).1,
        Some("invalid".into())
    );
}

#[tokio::test]
async fn reports_updater_absent_when_there_is_no_socket() {
    let dir = tempfile::tempdir().unwrap();
    let mgr = ReleaseManager::new(dir.path().join("missing.sock"), dir.path().join("results"));
    let (ok, err) =
        ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), components(), false));
    assert!(!ok);
    assert_eq!(err.as_deref(), Some("updater_absent"));
}

#[tokio::test]
async fn relays_the_updaters_own_rejection_reason() {
    let fake = FakeUpdater::start();
    *fake.apply_reply.lock().unwrap() = (422, "{\"reason\":\"namespace_rejected\"}".to_string());
    let mgr = fake.mgr();
    let (ok, err) =
        ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), components(), false));
    assert!(!ok);
    // The allowlist is host configuration the agent does not hold, so this
    // reason can only reach the ack by relay.
    assert_eq!(err.as_deref(), Some("namespace_rejected"));
}

#[tokio::test]
async fn refuses_a_second_apply_while_one_is_in_flight() {
    let fake = FakeUpdater::start();
    let mgr = fake.mgr();
    let (tx, _rx) = mpsc::channel(32);
    let _guard = mgr.attach_upstream(tx);
    fake.set_result("pulling", None);

    assert!(ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), components(), false)).0);
    let other = "11111111-2222-3333-4444-555555555555";
    let (ok, err) =
        ack_of(&mgr.handle_apply("c2".into(), other.into(), release(), components(), false));
    assert!(!ok);
    assert_eq!(
        err.as_deref(),
        Some("busy"),
        "single-flight per host: refuse, never queue"
    );
}

#[tokio::test]
async fn reports_updater_unreachable_when_the_result_stops_advancing() {
    let fake = FakeUpdater::start();
    let mgr = fake.mgr();
    mgr.set_unreachable_after(Duration::from_millis(50));
    let (tx, mut rx) = mpsc::channel(32);
    let _guard = mgr.attach_upstream(tx);

    // Accepted, then nothing: no result file ever appears.
    assert!(ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), components(), false)).0);
    let (state, reason, _) = state_of(&rx.recv().await.unwrap());
    assert_eq!(state, "failed");
    assert_eq!(reason.as_deref(), Some("updater_unreachable"));
}

/// On reconnect the agent re-emits the current state of every result file still
/// present, so a control plane that missed frames catches up without asking —
/// including the frames the recreate destroyed the previous agent mid-send.
#[tokio::test]
async fn re_emits_every_result_on_attach() {
    let fake = FakeUpdater::start();
    fake.set_result("succeeded", None);
    std::fs::write(
        fake.results.join(format!("{REQ}.json")),
        fake.result.lock().unwrap().clone().unwrap(),
    )
    .unwrap();

    let mgr = fake.mgr();
    let (tx, mut rx) = mpsc::channel(32);
    let _guard = mgr.attach_upstream(tx);
    let (state, _, previous) = state_of(&rx.recv().await.unwrap());
    assert_eq!(state, "succeeded");
    assert_eq!(previous[0].digest.as_deref(), Some(PREV));
}

#[test]
fn image_and_digest_shapes() {
    assert!(image_has_tag_or_digest("repo:tag"));
    assert!(image_has_tag_or_digest("ghcr.io/a/b@sha256:x"));
    assert!(!image_has_tag_or_digest("ghcr.io/a/b"));
    // A registry port is not a tag.
    assert!(!image_has_tag_or_digest("registry:5000/a/b"));
    assert!(is_digest(DIGEST));
    assert!(!is_digest(&DIGEST.to_uppercase()));
    assert!(!is_digest("sha256:short"));
    assert!(is_uuid(REQ));
    assert!(!is_uuid("7a1f6f1e2c334a589a5e0b6b0f7a1c22"));
}
