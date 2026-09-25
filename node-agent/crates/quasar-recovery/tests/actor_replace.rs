//! The node agent's replacement through `submit`, `status` and `resume` (#360): the
//! in-memory engine, crash injection at every engine call, and a temporary machine-state
//! directory, observed only through engine state and `status`.

mod support;

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, ReplaceTiming, TrustConfig};
use quasar_recovery::engine::{
    Behaviour, EngineError, ErrorKind, FakeContainer, FakeEngine, FakeState, Fault, Image,
    RestartPolicy, When,
};
use quasar_recovery::recipe::names;
use quasar_recovery::socket::{Component, Reason, Release, Request, RequestKind, State, Status};
use quasar_recovery::trust::{
    Caller, SignatureEvidence, SignatureMode, SignaturePolicy, TrustedKey,
};
use support::*;

const NEW_AGENT: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const REPO: &str = "registry.example.invalid/quasar/quasar-node-agent";
const NEW_DIGEST: &str = "sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const OLD_DIGEST: &str = "sha256:bb22000000000000000000000000000000000000000000000000000000000000";
const ID: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";
const ID2: &str = "3c0a6f2e-8d1b-4f7e-9a55-2b8e1c0d9f41";
const COMMIT: &str = "cccccccccccccccccccccccccccccccccccccccc";
const KEPT: &str = "quasar-node-agent.kept";

fn fast() -> ReplaceTiming {
    ReplaceTiming {
        verify_timeout: Duration::from_millis(200),
        poll: Duration::from_millis(1),
        stop_grace: Duration::from_secs(1),
        retries: 2,
        retry_backoff: Duration::ZERO,
    }
}

fn config(dir: &std::path::Path, timing: ReplaceTiming) -> ActorConfig {
    let mut config = ActorConfig::new(dir, quasar_recovery::socket::MachineRole::Gpu, operator());
    config.self_container = Some(ACTOR_ID.into());
    config.new_installation_id = Box::new(|| INSTALLATION.to_string());
    config.now = Box::new(|| NOW.to_string());
    config.gpus_probe_backoff = Duration::ZERO;
    config.trust = TrustConfig {
        allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
        signature: SignaturePolicy::default(),
    };
    config.timing = timing;
    config
}

fn actor_with(
    engine: &Arc<FakeEngine>,
    dir: &std::path::Path,
    timing: ReplaceTiming,
) -> Arc<Actor> {
    Arc::new(Actor::new(engine.clone(), config(dir, timing)))
}

fn new_image(label: &str) -> Image {
    Image {
        id: "sha256:d0d0000000000000000000000000000000000000000000000000000000000000".into(),
        repo_digests: vec![NEW_AGENT.into()],
        labels: BTreeMap::from([("org.quasar.recipe".to_string(), label.to_string())]),
    }
}

/// A GPU host with its agent installed, the new agent image in the registry, and the
/// behaviour a started new agent shows.
fn installed(new: Behaviour) -> (Arc<FakeEngine>, tempfile::TempDir) {
    let mut state = amd_host();
    state.registry.insert(NEW_AGENT.into(), new_image("1"));
    state.behaviour.insert(NEW_AGENT.into(), new);
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    actor_with(&engine, dir.path(), fast())
        .resume()
        .expect("a clean install");
    (engine, dir)
}

fn healthy() -> Behaviour {
    Behaviour {
        health: Some("healthy".into()),
        ..Default::default()
    }
}

fn unhealthy() -> Behaviour {
    Behaviour {
        health: Some("unhealthy".into()),
        logs: "health-bind-failed: 127.0.0.1:9091 is in use\n".into(),
        ..Default::default()
    }
}

fn request(id: &str, name: &str, image: &str, digest: &str) -> Request {
    Request {
        request_id: id.into(),
        kind: RequestKind::Replace,
        components: vec![Component {
            name: name.into(),
            image: image.into(),
            digest: digest.into(),
        }],
        release: Release {
            id: "rel-0.4.0".into(),
            version: Some("0.4.0".into()),
            source_commit: COMMIT.into(),
        },
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
    }
}

fn agent_request(id: &str) -> Request {
    request(id, "node-agent", REPO, NEW_DIGEST)
}

fn result_of(status: &Status) -> quasar_recovery::socket::AttemptResult {
    status.result.clone().expect("the attempt has a result")
}

/// Every container carrying the node agent's role, by name.
fn agents(state: &FakeState) -> Vec<FakeContainer> {
    let mut out: Vec<_> = state
        .containers
        .values()
        .filter(|c| {
            c.spec
                .labels
                .get("io.quasar.platform-service")
                .map(String::as_str)
                == Some("node-agent")
        })
        .cloned()
        .collect();
    out.sort_by(|a, b| a.spec.name.cmp(&b.spec.name));
    out
}

/// The machine as it was before any attempt: the one old agent, running, restartable.
fn assert_unchanged(state: &FakeState, old: &FakeContainer, at: &str) {
    let now = agents(state);
    assert_eq!(now.len(), 1, "{at}: node-agent containers {now:#?}");
    assert_eq!(now[0].id, old.id, "{at}: the old container was replaced");
    assert_eq!(now[0].spec.name, names::NODE_AGENT, "{at}");
    assert_eq!(now[0].status, "running", "{at}");
    assert_eq!(now[0].restart, RestartPolicy::UnlessStopped, "{at}");
    assert_eq!(now[0].spec.image, AGENT_IMAGE, "{at}");
}

fn assert_replaced(state: &FakeState, at: &str) {
    let now = agents(state);
    assert_eq!(now.len(), 1, "{at}: node-agent containers {now:#?}");
    assert_eq!(now[0].spec.name, names::NODE_AGENT, "{at}");
    assert_eq!(now[0].spec.image, NEW_AGENT, "{at}");
    assert_eq!(now[0].status, "running", "{at}");
    assert_eq!(now[0].restart, RestartPolicy::UnlessStopped, "{at}");
    assert_eq!(now[0].spec.labels["io.quasar.attempt"], ID, "{at}");
}

fn old_agent(engine: &FakeEngine) -> FakeContainer {
    engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone()
}

#[test]
fn a_healthy_new_agent_replaces_the_old_one_and_the_old_one_is_discarded() {
    let (engine, dir) = installed(healthy());
    let actor = actor_with(&engine, dir.path(), fast());

    let accepted = actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    assert_eq!(accepted.request_id, ID);
    assert_eq!(accepted.previous.len(), 1);
    assert_eq!(accepted.previous[0].digest.as_deref(), Some(OLD_DIGEST));
    actor.wait_attempt();

    let result = result_of(&actor.status_for(Some(ID)));
    assert_eq!(result.state, State::Succeeded);
    assert_eq!(result.reason, None);
    assert!(!result.restored);
    assert!(result.finished_at.is_some());
    assert_eq!(result.components, agent_request(ID).components);
    assert_eq!(result.previous[0].digest.as_deref(), Some(OLD_DIGEST));
    assert_eq!(result.release.source_commit, COMMIT);
    assert_replaced(&engine.state(), "after the attempt");
    assert!(engine.state().container_named(KEPT).is_none());
    assert_eq!(actor.status().in_flight, None);

    // The new specification is the machine's: a later start leaves it alone.
    drop(actor);
    let before = engine.state();
    actor_with(&engine, dir.path(), fast()).resume().unwrap();
    assert_eq!(engine.state().by_name(), before.by_name());
}

#[test]
fn an_agent_that_never_becomes_healthy_is_restored_and_says_why() {
    let (engine, dir) = installed(unhealthy());
    let old = old_agent(&engine);
    let actor = actor_with(&engine, dir.path(), fast());

    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    actor.wait_attempt();

    let result = result_of(&actor.status_for(Some(ID)));
    assert_eq!(result.state, State::Failed);
    assert_eq!(result.reason, Some(Reason::Unhealthy));
    assert!(result.restored, "{}", result.output);
    assert!(
        result.output.contains("health-bind-failed"),
        "{}",
        result.output
    );
    assert!(
        result.output.contains("previous container was put back"),
        "{}",
        result.output
    );
    assert!(result.output.len() <= 8192);
    assert_unchanged(&engine.state(), &old, "after the restore");
}

#[test]
fn a_start_the_engine_refuses_never_started_and_is_restored() {
    let (engine, dir) = installed(Behaviour {
        refuse_start: Some(
            "driver failed programming external connectivity: port is already allocated".into(),
        ),
        ..Default::default()
    });
    let old = old_agent(&engine);
    let actor = actor_with(&engine, dir.path(), fast());

    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    actor.wait_attempt();

    let result = result_of(&actor.status_for(Some(ID)));
    assert_eq!(result.reason, Some(Reason::NeverStarted));
    assert!(result.restored);
    assert!(
        result.output.contains("port is already allocated"),
        "{}",
        result.output
    );
    assert_unchanged(&engine.state(), &old, "after the restore");
}

#[test]
fn a_digest_that_cannot_be_pulled_changes_nothing() {
    let (engine, dir) = installed(healthy());
    let old = old_agent(&engine);
    let actor = actor_with(&engine, dir.path(), fast());
    let missing = "sha256:ee55000000000000000000000000000000000000000000000000000000000000";

    actor
        .submit(Caller::Agent, request(ID, "node-agent", REPO, missing))
        .unwrap();
    actor.wait_attempt();

    let result = result_of(&actor.status_for(Some(ID)));
    assert_eq!(result.reason, Some(Reason::PullFailed));
    assert!(!result.restored);
    assert_unchanged(&engine.state(), &old, "after the failed pull");
    assert_eq!(
        engine
            .state()
            .container_named(names::NODE_AGENT)
            .unwrap()
            .starts,
        old.starts
    );
}

#[test]
fn an_image_whose_recipe_revision_this_actor_lacks_is_refused_before_anything_stops() {
    let (engine, dir) = installed(healthy());
    engine.with_state(|s| {
        s.registry.insert(NEW_AGENT.into(), new_image("7"));
    });
    let old = old_agent(&engine);
    let actor = actor_with(&engine, dir.path(), fast());

    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    actor.wait_attempt();

    let result = result_of(&actor.status_for(Some(ID)));
    assert_eq!(result.reason, Some(Reason::RecipeUnsupported));
    assert!(!result.restored);
    assert_unchanged(&engine.state(), &old, "after recipe_unsupported");
}

/// Crash the actor at every engine call of a replacement, then start a new one: the
/// attempt always reaches the outcome the settle table names, and the machine always has
/// exactly one node agent, running.
fn sweep(
    new: Behaviour,
    expect_after_touch: fn(&FakeState, &FakeContainer, &str),
    daemon_restart: bool,
) {
    let (reference, reference_dir) = installed(new.clone());
    let start = reference.calls();
    let actor = actor_with(&reference, reference_dir.path(), fast());
    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    actor.wait_attempt();
    let total = reference.calls() - start;
    assert!(total > 10, "the sweep must not be vacuous ({total} calls)");

    let mut interrupted = 0;
    let mut continued = 0;
    for call in start..start + total {
        for when in [When::Before, When::After] {
            let at = format!("call {} {when:?}", call - start);
            let (engine, dir) = installed(new.clone());
            assert_eq!(engine.calls(), start, "{at}: installs must be identical");
            let old = old_agent(&engine);
            engine.inject(Fault {
                call,
                when,
                error: EngineError::Crashed,
            });
            let actor = actor_with(&engine, dir.path(), fast());
            let submitted = actor.submit(Caller::Agent, agent_request(ID));
            actor.wait_attempt();
            engine.clear_faults();
            if submitted.is_err() {
                // The crash hit admission: nothing was journalled, nothing changed.
                assert_eq!(actor.status_for(Some(ID)).result, None, "{at}");
                assert_unchanged(&engine.state(), &old, &at);
                continue;
            }
            let before = result_of(&actor.status_for(Some(ID)));
            assert!(
                !before.state.is_terminal(),
                "{at}: the crash was swallowed ({before:?})"
            );
            drop(actor);
            if daemon_restart {
                engine.restart_daemon();
            }

            actor_with(&engine, dir.path(), fast())
                .resume()
                .unwrap_or_else(|e| panic!("{at}: the next start failed: {e}"));
            let after = actor_with(&engine, dir.path(), fast());
            let result = result_of(&after.status_for(Some(ID)));
            assert!(
                result.state.is_terminal(),
                "{at}: still open after resume ({result:?})"
            );
            assert_eq!(after.status().in_flight, None, "{at}");
            match before.state {
                State::Pending | State::Pulling => {
                    interrupted += 1;
                    assert_eq!(result.state, State::Failed, "{at}");
                    assert_eq!(result.reason, Some(Reason::Interrupted), "{at}");
                    assert!(!result.restored, "{at}");
                    assert_unchanged(&engine.state(), &old, &at);
                }
                State::Recreating | State::Verifying => {
                    continued += 1;
                    expect_after_touch(&engine.state(), &old, &at);
                    let want = if agents(&engine.state())[0].spec.image == NEW_AGENT {
                        State::Succeeded
                    } else {
                        State::Failed
                    };
                    assert_eq!(result.state, want, "{at}: {result:?}");
                }
                other => panic!("{at}: unexpected state before the crash {other:?}"),
            }
            // Nothing is retried on its own: a further start changes nothing.
            let settled = engine.state();
            actor_with(&engine, dir.path(), fast()).resume().unwrap();
            assert_eq!(
                engine.state().by_name(),
                settled.by_name(),
                "{at}: a second start acted"
            );
        }
    }
    assert!(
        interrupted > 0 && continued > 0,
        "interrupted {interrupted}, continued {continued}"
    );
}

#[test]
fn every_crash_point_of_a_healthy_replacement_settles_to_the_table() {
    sweep(healthy(), |state, _, at| assert_replaced(state, at), false);
}

#[test]
fn every_crash_point_of_an_unhealthy_replacement_settles_to_the_table() {
    sweep(
        unhealthy(),
        |state, old, at| {
            let now = agents(state);
            assert_eq!(now.len(), 1, "{at}");
            assert_eq!(now[0].id, old.id, "{at}: the kept agent was not restored");
            assert_eq!(now[0].status, "running", "{at}");
            assert_eq!(now[0].spec.name, names::NODE_AGENT, "{at}");
            assert_eq!(now[0].restart, RestartPolicy::UnlessStopped, "{at}");
        },
        false,
    );
}

#[test]
fn a_daemon_restart_at_any_point_of_a_replacement_ends_in_a_stated_outcome() {
    sweep(healthy(), |state, _, at| assert_replaced(state, at), true);
}

#[test]
fn a_crash_while_settling_is_settled_by_the_start_after_it() {
    let (reference, dir) = installed(healthy());
    let start = reference.calls();
    let actor = actor_with(&reference, dir.path(), fast());
    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    actor.wait_attempt();
    let total = reference.calls() - start;

    // Every first crash that leaves the attempt past the point of no return, then a second
    // crash early in the start that continues it.
    let mut twice = 0;
    for first in start..start + total {
        let (engine, dir) = installed(healthy());
        engine.inject(Fault {
            call: first,
            when: When::After,
            error: EngineError::Crashed,
        });
        let actor = actor_with(&engine, dir.path(), fast());
        if actor.submit(Caller::Agent, agent_request(ID)).is_err() {
            continue;
        }
        actor.wait_attempt();
        let before = result_of(&actor.status_for(Some(ID))).state;
        drop(actor);
        engine.clear_faults();
        if !matches!(before, State::Recreating | State::Verifying) {
            continue;
        }
        let second = engine.calls() + 3;
        engine.inject(Fault {
            call: second,
            when: When::Before,
            error: EngineError::Crashed,
        });
        let _ = actor_with(&engine, dir.path(), fast()).resume();
        engine.clear_faults();
        actor_with(&engine, dir.path(), fast()).resume().unwrap();
        let result = result_of(&actor_with(&engine, dir.path(), fast()).status_for(Some(ID)));
        assert_eq!(
            result.state,
            State::Succeeded,
            "first crash at {}: {result:?}",
            first - start
        );
        assert_replaced(
            &engine.state(),
            &format!("first crash at {}", first - start),
        );
        twice += 1;
    }
    assert!(twice > 3, "only {twice} double-crash cases ran");
}

#[test]
fn a_transiently_unavailable_engine_does_not_end_the_attempt() {
    let (engine, dir) = installed(healthy());
    let start = engine.calls();
    for offset in [2, 5, 9] {
        engine.inject(Fault {
            call: start + offset,
            when: When::Before,
            error: EngineError::Runtime(ErrorKind::Unavailable),
        });
    }
    let actor = actor_with(&engine, dir.path(), fast());
    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    actor.wait_attempt();
    assert_eq!(
        result_of(&actor.status_for(Some(ID))).state,
        State::Succeeded
    );
    assert_replaced(&engine.state(), "after transient failures");
}

#[test]
fn a_repost_by_the_same_caller_is_the_same_attempt_and_starts_nothing() {
    let (engine, dir) = installed(healthy());
    let actor = actor_with(&engine, dir.path(), fast());
    let first = actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    let again = actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    assert_eq!(first, again);
    actor.wait_attempt();
    let settled = engine.state();
    let calls = engine.calls();

    let later = actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    assert_eq!(later, first);
    actor.wait_attempt();
    assert_eq!(
        engine.state(),
        settled,
        "a re-post of a finished attempt acted"
    );
    assert_eq!(engine.calls(), calls, "a re-post reached the engine");
    assert_eq!(result_of(&actor.status()).request_id, ID);
}

#[test]
fn a_known_request_id_posted_by_another_caller_is_refused_before_admission() {
    let (engine, dir) = installed(Behaviour {
        health: Some("starting".into()),
        ..Default::default()
    });
    let slow = ReplaceTiming {
        verify_timeout: Duration::from_millis(600),
        ..fast()
    };
    let actor = actor_with(&engine, dir.path(), slow);
    actor.submit(Caller::Agent, agent_request(ID)).unwrap();

    // In flight: the other socket gets `busy`, never the attempt's `Accepted`.
    let refused = actor
        .submit(Caller::ControlPlane, agent_request(ID))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Busy, "{}", refused.message);
    actor.wait_attempt();

    // Finished: still not the other caller's to re-post.
    let refused = actor
        .submit(Caller::ControlPlane, agent_request(ID))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid, "{}", refused.message);
}

#[test]
fn one_attempt_at_a_time() {
    let (engine, dir) = installed(Behaviour {
        health: Some("starting".into()),
        ..Default::default()
    });
    let slow = ReplaceTiming {
        verify_timeout: Duration::from_millis(600),
        ..fast()
    };
    let actor = actor_with(&engine, dir.path(), slow);
    actor.submit(Caller::Agent, agent_request(ID)).unwrap();
    assert_eq!(actor.status().in_flight.as_deref(), Some(ID));

    let refused = actor.submit(Caller::Agent, agent_request(ID2)).unwrap_err();
    assert_eq!(refused.reason, Reason::Busy);
    assert_eq!(refused.request_id, ID2);
    assert_eq!(
        actor.status_for(Some(ID2)).result,
        None,
        "a refusal was journalled"
    );
    actor.wait_attempt();
    assert_eq!(
        result_of(&actor.status_for(Some(ID))).reason,
        Some(Reason::Unhealthy)
    );

    engine.with_state(|s| {
        s.behaviour.insert(NEW_AGENT.into(), healthy());
    });
    actor.submit(Caller::Agent, agent_request(ID2)).unwrap();
    actor.wait_attempt();
    assert_eq!(
        result_of(&actor.status_for(Some(ID2))).state,
        State::Succeeded
    );
    // The latest attempt is the status default; the older one is still answerable.
    assert_eq!(result_of(&actor.status()).request_id, ID2);
    assert_eq!(result_of(&actor.status_for(Some(ID))).state, State::Failed);
}

#[test]
fn the_agent_socket_may_ask_only_to_replace_the_agent_or_the_actor() {
    let (engine, dir) = installed(healthy());
    let old = old_agent(&engine);
    let actor = actor_with(&engine, dir.path(), fast());
    let invalid = |req: Request, why: &str| {
        let refused = actor.submit(Caller::Agent, req).unwrap_err();
        assert_eq!(
            refused.reason,
            Reason::Invalid,
            "{why}: {}",
            refused.message
        );
        refused.message
    };

    let deputy = invalid(
        request(
            ID,
            "control-plane",
            "registry.example.invalid/quasar/quasar-control-plane",
            NEW_DIGEST,
        ),
        "control plane",
    );
    assert!(deputy.contains("confused deputy"), "{deputy}");
    let actor_itself = invalid(
        request(
            ID,
            "recovery-actor",
            "registry.example.invalid/quasar/quasar-recovery",
            NEW_DIGEST,
        ),
        "recovery actor",
    );
    assert!(actor_itself.contains("#362"), "{actor_itself}");
    invalid(
        request(
            ID,
            "postgres",
            "registry.example.invalid/quasar/postgres",
            NEW_DIGEST,
        ),
        "postgres",
    );
    invalid(request(ID, "quasar-updater", REPO, NEW_DIGEST), "updater");

    let mut restore = agent_request(ID);
    restore.kind = RequestKind::Restore;
    assert!(invalid(restore, "restore").contains("restore"));
    let mut remove = agent_request(ID);
    remove.kind = RequestKind::Remove;
    remove.components.clear();
    assert!(invalid(remove, "remove").contains("#366"));

    let refused = actor
        .submit(
            Caller::Agent,
            request(
                ID,
                "node-agent",
                "elsewhere.example.invalid/x/quasar-node-agent",
                NEW_DIGEST,
            ),
        )
        .unwrap_err();
    assert_eq!(refused.reason, Reason::NamespaceRejected);
    let refused = actor
        .submit(
            Caller::Agent,
            request(ID, "node-agent", REPO, "sha256:NOTHEX"),
        )
        .unwrap_err();
    assert_eq!(refused.reason, Reason::DigestMalformed);
    let refused = actor
        .submit(
            Caller::Agent,
            request(ID, "node-agent", &format!("{REPO}:latest"), NEW_DIGEST),
        )
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid);
    let refused = actor
        .submit(
            Caller::Agent,
            request("../../etc/passwd", "node-agent", REPO, NEW_DIGEST),
        )
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid);

    // Every refusal changed nothing and journalled nothing.
    assert_eq!(actor.status().result, None);
    assert_unchanged(&engine.state(), &old, "after the refusals");
    assert!(
        !dir.path().join("journal").exists()
            || std::fs::read_dir(dir.path().join("journal"))
                .unwrap()
                .next()
                .is_none()
    );
}

#[test]
fn a_container_this_installation_did_not_create_in_the_way_is_an_owner_conflict() {
    let (engine, dir) = installed(healthy());
    engine.with_state(|s| {
        let mut foreign = s.container_named(names::NODE_AGENT).unwrap().clone();
        foreign.id = "f0".repeat(32);
        foreign.spec.name = KEPT.into();
        foreign.spec.labels.clear();
        foreign
            .spec
            .labels
            .insert("com.docker.compose.service".into(), "node-agent".into());
        foreign.status = "exited".into();
        s.containers.insert(foreign.id.clone(), foreign);
    });
    let before = engine.state();
    let actor = actor_with(&engine, dir.path(), fast());
    let refused = actor.submit(Caller::Agent, agent_request(ID)).unwrap_err();
    assert_eq!(refused.reason, Reason::OwnerConflict);
    assert!(refused.message.contains(KEPT), "{}", refused.message);
    assert_eq!(engine.state(), before);
}

#[test]
fn a_release_under_signature_require_with_no_signature_is_refused() {
    let (engine, dir) = installed(healthy());
    let mut cfg = config(dir.path(), fast());
    cfg.trust.signature = SignaturePolicy {
        mode: SignatureMode::Require,
        keys: vec![TrustedKey {
            id: "k1".into(),
            key: [7u8; 32],
        }],
    };
    cfg.evidence = Box::new(|_| SignatureEvidence::Absent {
        why: "release 0.4.0 publishes no signature".into(),
    });
    let actor = Arc::new(Actor::new(engine.clone(), cfg));
    let refused = actor.submit(Caller::Agent, agent_request(ID)).unwrap_err();
    assert_eq!(
        refused.reason,
        Reason::SignatureMissing,
        "{}",
        refused.message
    );
}

#[test]
fn a_submit_while_a_start_settles_the_machine_is_busy() {
    let starting = || Behaviour {
        health: Some("starting".into()),
        ..Default::default()
    };
    let slow = ReplaceTiming {
        verify_timeout: Duration::from_millis(800),
        ..fast()
    };
    // Crash `offset` calls into an attempt; true when that leaves it open in `verifying`.
    let crash = |engine: &Arc<FakeEngine>, dir: &std::path::Path, offset: usize| {
        engine.inject(Fault {
            call: engine.calls() + offset,
            when: When::Before,
            error: EngineError::Crashed,
        });
        let actor = actor_with(engine, dir, slow);
        let open = actor.submit(Caller::Agent, agent_request(ID)).is_ok() && {
            actor.wait_attempt();
            result_of(&actor.status_for(Some(ID))).state == State::Verifying
        };
        engine.clear_faults();
        open
    };
    let offset = (0..60)
        .find(|&offset| {
            let (engine, dir) = installed(starting());
            crash(&engine, dir.path(), offset)
        })
        .expect("a crash point that leaves the attempt verifying");
    let (engine, dir) = installed(starting());
    assert!(crash(&engine, dir.path(), offset));

    let starting = actor_with(&engine, dir.path(), slow);
    let resuming = starting.clone();
    let settling = std::thread::spawn(move || resuming.resume());
    std::thread::sleep(Duration::from_millis(150));
    let refused = starting
        .submit(Caller::Agent, agent_request(ID2))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Busy, "{}", refused.message);
    settling.join().unwrap().unwrap();
    let result = result_of(&starting.status_for(Some(ID)));
    assert_eq!(result.reason, Some(Reason::Unhealthy));
    assert!(result.restored);
}
