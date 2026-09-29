//! Console access on an owned install (#395, agent-api.md amendment 18), against the real
//! recovery actor's agent socket and the in-memory engine, as `tests_owned` drives the
//! release relay. Each agent process is a [`ConsoleAccessManager`]; the one the actor
//! creates or puts back is a new manager on the same socket, with or without the marker.

use std::sync::Arc;
use std::time::{Duration, Instant};

use quasar_recovery::engine::Behaviour;

use super::console::{has_access, ConsoleAccessManager};
use super::tests_owned::{host, image, machine_on, Machine, OLD, REPO};
use crate::messages::{ConsoleAccessState, ConsoleCapabilities, VideoTopology};

/// An owned rootful GPU host whose agent image carries recipe revision 3 (console mode).
fn console_machine(agent: Behaviour) -> Machine {
    let mut state = host(Behaviour::default());
    let reference = format!("{REPO}@{OLD}");
    let mut img = image(&reference);
    img.labels
        .insert("org.quasar.recipe".to_string(), "3".to_string());
    state.registry.insert(reference.clone(), img);
    state.behaviour.insert(reference, agent);
    machine_on(state)
}

fn unhealthy() -> Behaviour {
    Behaviour {
        health: Some("unhealthy".into()),
        ..Default::default()
    }
}

/// An agent process on `m`: `marker` when the actor created it with console access.
fn agent(m: &Machine, marker: bool) -> Arc<ConsoleAccessManager> {
    let a = ConsoleAccessManager::owned_for_test(&m.socket, marker);
    a.set_engine_mode(Some("rootful"));
    a.refresh();
    a
}

fn state(a: &ConsoleAccessManager) -> ConsoleAccessState {
    a.report().expect("an owned host reports access").state
}

/// The actor's console attempts so far: the in-flight one, else the last settled one.
fn last_attempt(m: &Machine) -> Option<String> {
    let s = m.actor.console_status();
    s.in_flight.or(s.last.map(|l| l.request_id))
}

fn node_agent_marker(m: &Machine) -> bool {
    m.engine
        .state()
        .container_named(quasar_recovery::recipe::names::NODE_AGENT)
        .and_then(|c| c.spec.env.get(super::console::MARKER_ENV).cloned())
        .as_deref()
        == Some("1")
}

/// `on`, driven through the actor: the old agent asks, the new one verifies.
fn turn_on(m: &Machine) -> Arc<ConsoleAccessManager> {
    let old = agent(m, false);
    old.reconcile(true);
    m.actor.wait_attempt();
    agent(m, true)
}

fn json(a: &ConsoleAccessManager) -> serde_json::Value {
    serde_json::to_value(a.report().unwrap()).unwrap()
}

#[test]
fn enabling_submits_once_and_a_resent_config_starts_nothing() {
    let m = console_machine(Behaviour::default());
    let old = agent(&m, false);
    assert_eq!(
        json(&old),
        serde_json::json!({
            "state": "off", "target": null, "request_id": null, "reason": null,
            "started_at": null, "finished_at": null,
            "summary": "The node agent has no console access."
        })
    );
    // A resent `enabled: false` agrees with no access.
    old.reconcile(false);
    assert_eq!(last_attempt(&m), None);

    old.reconcile(true);
    let applying = old.report().unwrap();
    assert_eq!(applying.state, ConsoleAccessState::Applying, "{applying:?}");
    assert_eq!(applying.target, Some(true));
    let id = applying.request_id.clone().expect("the actor's attempt");
    assert!(applying.started_at.is_some() && applying.finished_at.is_none());
    assert_eq!(applying.reason, None);
    // The same config again while applying: no second submit.
    old.reconcile(true);
    assert_eq!(last_attempt(&m).as_deref(), Some(id.as_str()));

    m.actor.wait_attempt();
    assert!(node_agent_marker(&m), "the new agent carries the marker");
    let new = agent(&m, true);
    let on = new.report().unwrap();
    assert_eq!(on.state, ConsoleAccessState::On, "{on:?}");
    assert_eq!(
        (on.target, on.request_id.as_deref()),
        (Some(true), Some(id.as_str()))
    );
    assert!(on.started_at.is_some() && on.finished_at.is_some());
    assert!(has_access(&on));
    // The full config after the new agent's first connect: nothing to do.
    new.reconcile(true);
    assert_eq!(last_attempt(&m).as_deref(), Some(id.as_str()));
    assert_eq!(state(&new), ConsoleAccessState::On);
}

#[test]
fn a_failed_enable_is_restored_and_not_asked_for_again_until_enabled_flips() {
    let m = console_machine(unhealthy());
    let old = agent(&m, false);
    old.reconcile(true);
    let id = old.report().unwrap().request_id.unwrap();
    m.actor.wait_attempt();
    assert!(!node_agent_marker(&m), "the previous agent is back");

    // The agent put back is a new process.
    let back = agent(&m, false);
    let restored = back.report().unwrap();
    assert_eq!(restored.state, ConsoleAccessState::Restored, "{restored:?}");
    assert_eq!(restored.target, Some(true));
    assert_eq!(restored.request_id.as_deref(), Some(id.as_str()));
    assert_eq!(restored.reason.as_deref(), Some("unhealthy"));
    assert!(restored.started_at.is_some() && restored.finished_at.is_some());
    assert!(!has_access(&restored));

    // An older control plane resends `enabled: true`: no retry loop.
    back.reconcile(true);
    back.reconcile(true);
    assert_eq!(last_attempt(&m).as_deref(), Some(id.as_str()));
    assert_eq!(back.report().unwrap(), restored);
    // The reset agrees with the access kept, and lifts the hold.
    back.reconcile(false);
    assert_eq!(last_attempt(&m).as_deref(), Some(id.as_str()));
    // The admin's "try again".
    back.reconcile(true);
    let again = last_attempt(&m).expect("a new attempt");
    assert_ne!(again, id);
    m.actor.wait_attempt();
}

#[test]
fn a_failed_disable_keeps_access_and_counts_as_having_it() {
    let m = console_machine(Behaviour::default());
    let on = turn_on(&m);
    let enabled_id = last_attempt(&m).unwrap();
    m.engine.with_state(|s| {
        s.behaviour.insert(format!("{REPO}@{OLD}"), unhealthy());
    });
    on.reconcile(false);
    let id = last_attempt(&m).unwrap();
    assert_ne!(id, enabled_id);
    m.actor.wait_attempt();
    assert!(
        node_agent_marker(&m),
        "the agent with console access is back"
    );

    let back = agent(&m, true);
    let restored = back.report().unwrap();
    assert_eq!(restored.state, ConsoleAccessState::Restored, "{restored:?}");
    assert_eq!(restored.target, Some(false));
    assert!(
        has_access(&restored),
        "restored with target false has access"
    );
    // `enabled: true` agrees with the access kept.
    back.reconcile(true);
    assert_eq!(last_attempt(&m).as_deref(), Some(id.as_str()));
    // Before the reset lands, a resent `enabled: false` asks for nothing again.
    let fresh = agent(&m, true);
    fresh.reconcile(false);
    assert_eq!(last_attempt(&m).as_deref(), Some(id.as_str()));
}

#[test]
fn a_rootless_engine_is_no_longer_forced_unsupported() {
    // #407: a rootless engine is a fully supported console host now — the client no
    // longer short-circuits to `unsupported` on its own; it reads the actor like any
    // other agent, and can ask for console mode.
    let m = console_machine(Behaviour::default());
    let a = ConsoleAccessManager::owned_for_test(&m.socket, false);
    a.set_engine_mode(Some("rootless"));
    a.refresh();
    let report = json(&a);
    assert_eq!(report["state"], "off", "{report}");
    a.reconcile(true);
    m.actor.wait_attempt();
    assert!(
        last_attempt(&m).is_some(),
        "a rootless-engine agent must be able to ask the actor for console mode"
    );
    // An agent without the marker still refuses a console launch, same as rootful —
    // asking for console mode is independent of already having it.
    assert!(a.launch_refusal(VideoTopology::LocalOnly).is_some());
}

#[test]
fn a_host_with_no_actor_reports_no_access_and_asks_for_nothing() {
    let a = ConsoleAccessManager::without_actor();
    a.refresh();
    a.reconcile(true);
    assert!(a.report().is_none());
    assert!(a.launch_refusal(VideoTopology::LocalOnly).is_none());
    let caps = serde_json::to_value(ConsoleCapabilities::default()).unwrap();
    assert!(caps.get("access").is_none(), "{caps}");
}

#[test]
fn a_change_of_access_wakes_the_connection_and_nothing_else_does() {
    let m = console_machine(Behaviour::default());
    let a = ConsoleAccessManager::owned_for_test(&m.socket, false);
    a.set_engine_mode(Some("rootful"));
    let mut rx = a.subscribe();
    a.refresh();
    assert!(rx.try_recv().is_ok(), "the first report");
    a.refresh();
    a.reconcile(false);
    assert!(rx.try_recv().is_err(), "unchanged: no re-send");
    a.reconcile(true);
    assert!(rx.try_recv().is_ok(), "applying");
    m.actor.wait_attempt();
}

#[test]
fn the_worker_watches_an_applying_attempt_until_it_settles() {
    let m = console_machine(unhealthy());
    let a = ConsoleAccessManager::owned_for_test(&m.socket, false);
    a.set_engine_mode(Some("rootful"));
    a.refresh();
    let mut rx = a.subscribe();
    a.request(true);
    let deadline = Instant::now() + Duration::from_secs(20);
    let mut seen = Vec::new();
    loop {
        let s = state(&a);
        if seen.last() != Some(&s) {
            seen.push(s);
        }
        if s == ConsoleAccessState::Restored {
            break;
        }
        assert!(Instant::now() < deadline, "never settled: {seen:?}");
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(seen.contains(&ConsoleAccessState::Applying), "{seen:?}");
    let mut wakes = 0;
    while rx.try_recv().is_ok() {
        wakes += 1;
    }
    assert!(wakes >= 1, "the settled report woke the connection");
}

#[test]
fn only_an_owned_agent_without_the_marker_refuses_a_console_launch() {
    let m = console_machine(Behaviour::default());
    let without = ConsoleAccessManager::owned_for_test(&m.socket, false);
    let with = ConsoleAccessManager::owned_for_test(&m.socket, true);
    for t in [VideoTopology::LocalOnly, VideoTopology::DualOutput] {
        assert!(without.launch_refusal(t).is_some(), "{t:?}");
        assert!(with.launch_refusal(t).is_none(), "{t:?}");
    }
    assert!(without.launch_refusal(VideoTopology::StreamOnly).is_none());
}
