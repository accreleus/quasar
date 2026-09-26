use super::*;

const REQ: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";
const DIGEST: &str = "sha256:9f2c000000000000000000000000000000000000000000000000000000000abc";

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

fn parsed(request_id: &str, state: &str, components: &[&str]) -> ActorResult {
    ActorResult {
        request_id: request_id.into(),
        state: state.into(),
        reason: None,
        components: components
            .iter()
            .map(|name| ReleaseComponent {
                name: (*name).into(),
                image: "ghcr.io/accreleus/quasar/x".into(),
                digest: DIGEST.into(),
            })
            .collect(),
        previous: Vec::new(),
        output: String::new(),
        started_at: String::new(),
        updated_at: String::new(),
        finished_at: None,
        restored: false,
    }
}

/// `restored` rides through the relay (agent-api.md, amendment 9): the control
/// plane keys its auto_revert row off it. Omitted on the wire when false so an
/// older control plane sees the shape it knows.
#[test]
fn restored_is_relayed_and_omitted_when_false() {
    let mut res = parsed(
        "11111111-1111-4111-8111-111111111111",
        "failed",
        &["node-agent"],
    );
    res.restored = true;
    let msg = res.into_msg();
    let json = serde_json::to_string(&msg).unwrap();
    assert!(json.contains("\"restored\":true"), "{json}");

    let res = parsed(
        "11111111-1111-4111-8111-111111111111",
        "failed",
        &["node-agent"],
    );
    let json = serde_json::to_string(&res.into_msg()).unwrap();
    assert!(!json.contains("restored"), "{json}");

    // And the actor's field is read.
    let file: ActorResult = serde_json::from_str(
        r#"{"request_id":"x","state":"failed","reason":"unhealthy","restored":true}"#,
    )
    .unwrap();
    assert!(file.restored);
}

/// A host with no recovery actor has nothing to hand an apply to: every request
/// is `updater_absent`, and a control-plane component is still refused first.
#[tokio::test]
async fn a_host_with_no_recovery_actor_answers_updater_absent() {
    let mgr = ReleaseManager::without_actor();
    assert!(!mgr.present());
    let (ok, err) =
        ack_of(&mgr.handle_apply("c1".into(), REQ.into(), release(), components(), false));
    assert!(!ok);
    assert_eq!(err.as_deref(), Some("updater_absent"));

    let mut c = components();
    c[0].name = "control-plane".into();
    let (ok, err) = ack_of(&mgr.handle_apply("c2".into(), REQ.into(), release(), c, false));
    assert!(!ok);
    assert_eq!(err.as_deref(), Some("invalid"));

    // Attaching replays nothing and asks nothing.
    let (tx, mut rx) = mpsc::channel(4);
    let _guard = mgr.attach_upstream(tx);
    assert!(rx.try_recv().is_err());
}

#[tokio::test]
async fn rejects_malformed_requests() {
    let mgr = ReleaseManager::without_actor();

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

const OLD: Duration = Duration::from_secs(3 * 24 * 3600);
const RECENT: Duration = Duration::from_secs(5 * 60);

/// The replay decision, without a filesystem. It errs towards replaying: only a
/// result that is provably not this agent's, or provably long resolved, is
/// dropped (#193).
#[test]
fn replay_worthy_keeps_only_this_agents_live_or_recent_results() {
    // Not this agent's: the control plane's own step on a combined host.
    assert!(!replay_worthy(
        &parsed(REQ, "succeeded", &["control-plane"]),
        Some(RECENT)
    ));
    assert!(!replay_worthy(
        &parsed(REQ, "pulling", &["control-plane"]),
        Some(RECENT)
    ));
    // Not all ours is not ours.
    assert!(!replay_worthy(
        &parsed(REQ, "succeeded", &["node-agent", "control-plane"]),
        Some(RECENT)
    ));
    // Nothing to speak for.
    assert!(!replay_worthy(&parsed(REQ, "succeeded", &[]), Some(RECENT)));

    // Terminal + older than the poll deadline: resolved long ago.
    assert!(!replay_worthy(
        &parsed(REQ, "succeeded", &["node-agent"]),
        Some(OLD)
    ));
    assert!(!replay_worthy(
        &parsed(REQ, "failed", &["node-agent"]),
        Some(OLD)
    ));
    // Terminal + recent: the control plane may have missed the final frame.
    assert!(replay_worthy(
        &parsed(REQ, "succeeded", &["node-agent"]),
        Some(RECENT)
    ));
    assert!(replay_worthy(
        &parsed(REQ, "failed", &["node-agent"]),
        Some(RECENT)
    ));
    // Non-terminal is live at any age.
    assert!(replay_worthy(
        &parsed(REQ, "recreating", &["node-agent"]),
        Some(OLD)
    ));
    // Unknown age: keep — a duplicate is a no-op, a dropped live one is not.
    assert!(replay_worthy(
        &parsed(REQ, "succeeded", &["node-agent"]),
        None
    ));
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
