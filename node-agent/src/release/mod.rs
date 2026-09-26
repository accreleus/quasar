//! Agent-side client for this host's recovery actor (`CONTEXT.md`).
//!
//! The agent relays and does not author: `release_apply` is validated, acked on
//! acceptance, POSTed to the actor's agent socket, and every `release_state`
//! after that is a re-frame of the actor's result. This module keeps no apply
//! state and performs no session logic at any point (agent-api.md
//! `release_state`).
//!
//! Only an `owned` install has an actor (`QUASAR_RECOVERY_SOCKET`, set by the
//! actor's recipe): `POST /v1/submit`, `GET /v1/status?request_id=`, whose journal
//! outlives both the agent and the actor. A host with none answers every apply
//! `updater_absent`. The socket is not frozen (protocol/schema.md §"Not frozen").

pub(crate) mod unix_http;

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant, SystemTime};

use serde::Deserialize;
use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use crate::messages::{AgentMsg, ReleaseComponent, ReleaseInfo, ReleasePrevious};

/// Poll cadence against the actor's status. The wire's floor is one message per
/// 2 s per state; emitting only on CHANGE stays under it without a timer.
const POLL_INTERVAL: Duration = Duration::from_secs(1);

/// How long the result may stop advancing before the apply is called
/// `updater_unreachable`. Generous: it must survive the agent's own recreate
/// plus a health wait, and a false alarm about a working apply is worse than a
/// late one.
const UNREACHABLE_AFTER: Duration = Duration::from_secs(180);

/// Hard bound on one apply's observation, past which nothing more will be
/// learned by looking.
const POLL_DEADLINE: Duration = Duration::from_secs(2 * 3600);

/// What this build may apply. `control-plane` is absent: a control plane asking
/// an agent to replace the control plane is a confused deputy, and this makes it
/// unrepresentable (agent-api.md `release_apply`).
const APPLIABLE_COMPONENTS: &[&str] = &["node-agent", "recovery-actor"];

/// The part of the recovery actor's `GET /v1/status` the relay reads.
#[derive(Deserialize)]
struct ActorStatus {
    #[serde(default)]
    result: Option<ActorResult>,
}

/// One attempt's result, which carries `release_state`'s fields under the same
/// names. Extra fields are ignored.
#[derive(Deserialize, Debug, Clone)]
struct ActorResult {
    request_id: String,
    state: String,
    #[serde(default)]
    reason: Option<String>,
    #[serde(default)]
    components: Vec<ReleaseComponent>,
    #[serde(default)]
    previous: Vec<ReleasePrevious>,
    #[serde(default)]
    output: String,
    #[serde(default)]
    started_at: String,
    #[serde(default)]
    updated_at: String,
    #[serde(default)]
    finished_at: Option<String>,
    #[serde(default)]
    restored: bool,
}

impl ActorResult {
    fn into_msg(self) -> AgentMsg {
        AgentMsg::ReleaseState {
            request_id: self.request_id,
            state: self.state,
            reason: self.reason,
            components: self.components,
            previous: self.previous,
            output: self.output,
            started_at: self.started_at,
            updated_at: self.updated_at,
            finished_at: self.finished_at,
            restored: self.restored,
        }
    }
}

#[derive(Deserialize, Debug)]
struct ActorRefusal {
    #[serde(default)]
    reason: String,
}

pub struct ReleaseManager {
    /// The recovery actor's agent socket; `None` on a host with no actor.
    socket: Option<PathBuf>,
    upstream: RwLock<Option<mpsc::Sender<AgentMsg>>>,
    /// Single-flight per host: refuse, never queue. Holds the request id of the
    /// apply whose poller is still running.
    inflight: Mutex<Option<String>>,
    /// [`UNREACHABLE_AFTER`] in millis, overridable so tests do not sleep for
    /// three minutes to observe a timeout.
    unreachable_after_ms: AtomicU64,
    /// Set just before the terminal state of an apply that replaced the recovery actor
    /// under this still-connected agent (amendment 14, §register): the connection closes
    /// once it has forwarded that state and re-dials, so `register` carries the new actor.
    redial: AtomicBool,
    /// An attach found the recovery actor dark and keeps asking (`ADOPT_WINDOW`).
    adopting: AtomicBool,
}

/// How long an attach keeps asking a recovery actor that did not answer. A daemon restart
/// can start this agent before its actor serves again, and a successor verifying a
/// hand-over waits for exactly this contact (`quasar_recovery::handover`, 60 s).
const ADOPT_WINDOW: Duration = Duration::from_secs(90);
const ADOPT_BACKOFF_MAX: Duration = Duration::from_secs(5);

/// Detaches the upstream sender on every connection-end path, so a poller
/// outliving the connection emits into nothing rather than a dead channel.
pub struct UpstreamGuard {
    mgr: Arc<ReleaseManager>,
}

impl Drop for UpstreamGuard {
    fn drop(&mut self) {
        *self.mgr.upstream.write().unwrap() = None;
    }
}

impl ReleaseManager {
    /// An owned install: the recovery actor on its agent socket.
    pub fn owned(socket: impl Into<PathBuf>) -> Arc<Self> {
        Self::with_socket(Some(socket.into()))
    }

    /// A host with no recovery actor: every apply is `updater_absent`.
    pub fn without_actor() -> Arc<Self> {
        Self::with_socket(None)
    }

    fn with_socket(socket: Option<PathBuf>) -> Arc<Self> {
        Arc::new(ReleaseManager {
            socket,
            upstream: RwLock::new(None),
            inflight: Mutex::new(None),
            unreachable_after_ms: AtomicU64::new(UNREACHABLE_AFTER.as_millis() as u64),
            redial: AtomicBool::new(false),
            adopting: AtomicBool::new(false),
        })
    }

    /// Whether the connection should re-dial now that it forwarded a terminal
    /// `release_state` (see `redial`). Consumed by the call.
    pub fn take_redial(&self) -> bool {
        self.redial.swap(false, Ordering::SeqCst)
    }

    /// The recovery actor answering now is not the one this agent registered: an apply
    /// named `recovery-actor` and the actor moved.
    fn actor_replaced(&self, res: &ActorResult) -> bool {
        let Some(socket) = self.socket.as_deref() else {
            return false;
        };
        if !res.components.iter().any(|c| c.name == "recovery-actor") {
            return false;
        }
        let now = crate::buildinfo::discover_owned(socket);
        let registered = crate::buildinfo::install_facts();
        now.updater_present == Some(true)
            && (now.recovery_actor_source_commit != registered.recovery_actor_source_commit
                || now.recovery_actor_version != registered.recovery_actor_version)
    }

    pub fn from_env() -> Arc<Self> {
        match crate::buildinfo::owned_socket() {
            Some(socket) => Self::owned(socket),
            None => Self::without_actor(),
        }
    }

    /// Attach this connection's channel and re-emit the actor's most recent attempt
    /// when this agent should still speak for it, so a control plane that missed
    /// frames catches up without asking and an agent replaced mid-apply still
    /// reports the apply that replaced it.
    ///
    /// Two kinds of result are left alone (`replay_worthy`): one whose components
    /// are not all in [`APPLIABLE_COMPONENTS`] (the control plane's own step on a
    /// combined host: a host speaking about another target's attempt is a
    /// trust-boundary event), and a terminal one older than [`POLL_DEADLINE`],
    /// which the control plane resolved long ago.
    ///
    /// A non-terminal result is not just re-emitted: it is adopted. The process
    /// that handed it to the actor is gone (replacing the agent is what the apply
    /// does), so nobody else will relay its final state, and with the actor's
    /// automatic restore (ADR 0004) the restored agent normally connects while
    /// the actor is still verifying that restore. Without a watcher the attempt
    /// would sit `verifying` on the control plane for ever.
    pub fn attach_upstream(self: &Arc<Self>, tx: mpsc::Sender<AgentMsg>) -> UpstreamGuard {
        *self.upstream.write().unwrap() = Some(tx);
        match self.replayable_results() {
            Some(results) => {
                for res in results {
                    self.replay(res);
                }
            }
            None => self.adopt_when_answered(),
        }
        UpstreamGuard { mgr: self.clone() }
    }

    /// The recovery actor did not answer at attach: ask again, backing off, until it does
    /// or [`ADOPT_WINDOW`] ends, then replay what it reports.
    fn adopt_when_answered(self: &Arc<Self>) {
        if self.adopting.swap(true, Ordering::SeqCst) {
            return;
        }
        let mgr = self.clone();
        std::thread::spawn(move || {
            let started = Instant::now();
            let mut backoff = Duration::from_millis(250);
            loop {
                std::thread::sleep(backoff);
                if let Some(results) = mgr.replayable_results() {
                    for res in results {
                        mgr.replay(res);
                    }
                    break;
                }
                if started.elapsed() > ADOPT_WINDOW {
                    warn!(
                        token = "release-actor-dark",
                        "the recovery actor did not answer on {} within {}s of connecting; an apply it was running is reported when it next answers",
                        mgr.socket_display(),
                        ADOPT_WINDOW.as_secs()
                    );
                    break;
                }
                backoff = (backoff * 2).min(ADOPT_BACKOFF_MAX);
            }
            mgr.adopting.store(false, Ordering::SeqCst);
        });
    }

    /// Re-emit one result after a connect, adopting it when it is still in flight.
    fn replay(self: &Arc<Self>, res: ActorResult) {
        info!(
            "release apply {}: re-emitting state {} after connect",
            res.request_id, res.state
        );
        if is_terminal(&res.state) {
            self.send(res.into_msg());
            return;
        }
        let adopt = {
            let mut inflight = self.inflight.lock().unwrap();
            match inflight.as_deref() {
                // This process's own poller is already relaying it (a
                // reconnect, not a restart); a second watcher would only
                // duplicate frames.
                Some(cur) if cur == res.request_id => false,
                Some(_) => false,
                None => {
                    *inflight = Some(res.request_id.clone());
                    true
                }
            }
        };
        if adopt {
            info!(
                "release apply {}: adopting the in-flight apply of the agent this one replaced",
                res.request_id
            );
            self.spawn_poller(res.request_id);
        } else {
            self.send(res.into_msg());
        }
    }

    #[cfg(test)]
    pub fn set_unreachable_after(&self, d: Duration) {
        self.unreachable_after_ms
            .store(d.as_millis() as u64, Ordering::Relaxed);
    }

    /// Whether this host has a recovery actor socket to hand a request to.
    pub fn present(&self) -> bool {
        self.socket.as_deref().is_some_and(Path::exists)
    }

    fn socket_display(&self) -> String {
        self.socket
            .as_deref()
            .map(|s| s.display().to_string())
            .unwrap_or_else(|| "(no recovery actor)".into())
    }

    /// `release_apply`: validate, ack acceptance, hand off, then relay.
    pub fn handle_apply(
        self: &Arc<Self>,
        id: String,
        request_id: String,
        release: ReleaseInfo,
        components: Vec<ReleaseComponent>,
        force: bool,
    ) -> AgentMsg {
        if let Some(reason) = validate(&request_id, &components, APPLIABLE_COMPONENTS) {
            warn!(
                token = "release-apply-rejected",
                "release_apply {request_id} rejected: {reason}"
            );
            return nack(id, reason);
        }
        let Some(socket) = self.socket.clone().filter(|s| s.exists()) else {
            warn!(
                token = "release-apply-no-updater",
                "release_apply {request_id}: no recovery actor socket at {}",
                self.socket_display()
            );
            return nack(id, "updater_absent");
        };

        // Re-acking an id already in flight is idempotent: no second apply, and
        // the current state is re-emitted rather than a new one started.
        {
            let mut inflight = self.inflight.lock().unwrap();
            match inflight.as_deref() {
                Some(cur) if cur == request_id => {
                    drop(inflight);
                    if let Some(res) = self.read_result(&request_id) {
                        self.send(res.into_msg());
                    }
                    return ack(id);
                }
                Some(cur) => {
                    warn!(
                        token = "release-apply-busy",
                        "release_apply {request_id} refused: {cur} is still in flight"
                    );
                    return nack(id, "busy");
                }
                None => *inflight = Some(request_id.clone()),
            }
        }

        // The actor's request shape (testdata/recovery/socket). An agent only ever
        // asks for a replacement; no migration, dump or purge can come from here.
        let body = serde_json::json!({
            "request_id": request_id,
            "kind": "replace",
            "components": components,
            "release": release,
            "migrates": false,
            "schema_version": null,
            "external_backup_confirmed": false,
            "dump": null,
            "purge": false,
        });
        let body = body.to_string();

        let reply = unix_http::request(
            &socket,
            "POST",
            "/v1/submit",
            Some(&body),
            Duration::from_secs(30),
        );
        match reply {
            // A socket with nobody listening is "there is no actor" =
            // `updater_absent`; `updater_unreachable` is for losing sight of an
            // apply already accepted.
            Err(e) => {
                self.clear_inflight(&request_id);
                warn!(
                    token = "release-apply-socket-error",
                    "release_apply {request_id}: {e}"
                );
                nack(id, "updater_absent")
            }
            Ok(r) if r.status == 202 => {
                info!("release_apply {request_id} accepted by the recovery actor (force={force})");
                self.spawn_poller(request_id);
                ack(id)
            }
            Ok(r) => {
                self.clear_inflight(&request_id);
                let reason = serde_json::from_str::<ActorRefusal>(&r.body)
                    .ok()
                    .map(|e| e.reason)
                    .filter(|s| !s.is_empty())
                    .unwrap_or_else(|| "invalid".to_string());
                warn!(
                    token = "release-apply-updater-rejected",
                    "release_apply {request_id} rejected by the recovery actor ({}): {reason}",
                    r.status
                );
                nack(id, &reason)
            }
        }
    }

    fn clear_inflight(&self, request_id: &str) {
        let mut inflight = self.inflight.lock().unwrap();
        if inflight.as_deref() == Some(request_id) {
            *inflight = None;
        }
    }

    /// Relay every state change until the apply is terminal. A std thread, not
    /// a task: it normally outlives the connection and the process, since
    /// recreating the agent is what the apply does.
    fn spawn_poller(self: &Arc<Self>, request_id: String) {
        let mgr = self.clone();
        std::thread::spawn(move || {
            let started = Instant::now();
            let unreachable_after =
                Duration::from_millis(mgr.unreachable_after_ms.load(Ordering::Relaxed));
            let mut last_seen = Instant::now();
            let mut last: Option<String> = None;
            loop {
                match mgr.read_result(&request_id) {
                    Some(res) => {
                        last_seen = Instant::now();
                        let terminal = is_terminal(&res.state);
                        if last.as_deref() != Some(res.state.as_str()) {
                            last = Some(res.state.clone());
                            info!("release apply {request_id}: {}", res.state);
                            if terminal && mgr.actor_replaced(&res) {
                                mgr.redial.store(true, Ordering::SeqCst);
                            }
                            mgr.send_blocking(res.into_msg());
                        }
                        if terminal {
                            break;
                        }
                    }
                    None if last_seen.elapsed() > unreachable_after => {
                        warn!(
                            token = "release-updater-unreachable",
                            "release apply {request_id}: no result for {:?}", unreachable_after
                        );
                        mgr.send_blocking(unreachable_msg(&request_id));
                        break;
                    }
                    None => {}
                }
                if started.elapsed() > POLL_DEADLINE {
                    warn!(
                        token = "release-poll-deadline",
                        "release apply {request_id}: still non-terminal after {}s; giving up watching it",
                        POLL_DEADLINE.as_secs()
                    );
                    mgr.send_blocking(unreachable_msg(&request_id));
                    break;
                }
                std::thread::sleep(POLL_INTERVAL);
            }
            mgr.clear_inflight(&request_id);
        });
    }

    fn read_result(&self, request_id: &str) -> Option<ActorResult> {
        self.actor_result(Some(request_id))
    }

    /// One attempt's result from the recovery actor's status (the most recent attempt's
    /// when `request_id` is `None`); `None` when the actor has none or did not answer.
    fn actor_result(&self, request_id: Option<&str>) -> Option<ActorResult> {
        self.actor_answer(request_id).flatten()
    }

    /// [`Self::actor_result`], `None` when the actor did not answer at all.
    fn actor_answer(&self, request_id: Option<&str>) -> Option<Option<ActorResult>> {
        let socket = self.socket.as_deref()?;
        let path = match request_id {
            Some(id) => format!("/v1/status?request_id={id}"),
            None => "/v1/status".to_string(),
        };
        let reply = unix_http::request(socket, "GET", &path, None, Duration::from_secs(10)).ok()?;
        if reply.status != 200 {
            return None;
        }
        Some(match serde_json::from_str::<ActorStatus>(&reply.body) {
            Ok(status) => status
                .result
                .filter(|r| request_id.is_none_or(|id| r.request_id == id)),
            Err(e) => {
                debug!("recovery actor status unparsable: {e}");
                None
            }
        })
    }

    /// The actor's most recent attempt when [`replay_worthy`] keeps it: the only one
    /// an agent can still speak for (single flight). Age is its `updated_at`. An
    /// empty list on a host with no actor; `None` when the actor did not answer.
    fn replayable_results(&self) -> Option<Vec<ActorResult>> {
        if self.socket.is_none() {
            return Some(Vec::new());
        }
        Some(
            self.actor_answer(None)?
                .filter(|r| replay_worthy(r, rfc3339_age(&r.updated_at)))
                .into_iter()
                .collect(),
        )
    }

    /// Lossy: the connect path must never block.
    fn send(&self, msg: AgentMsg) {
        let tx = self.upstream.read().unwrap().clone();
        match tx {
            Some(tx) => {
                if let Err(e) = tx.try_send(msg) {
                    debug!("release_state not sent: {e}");
                }
            }
            None => debug!("release_state not sent: no upstream attached"),
        }
    }

    /// Poller threads only: blocks while the channel is full, which the async
    /// connection task must never do.
    fn send_blocking(&self, msg: AgentMsg) {
        let tx = self.upstream.read().unwrap().clone();
        match tx {
            Some(tx) => {
                if let Err(e) = tx.blocking_send(msg) {
                    debug!("release_state undeliverable ({e}); the next attach re-emits it");
                }
            }
            None => debug!("release_state not sent: no upstream attached"),
        }
    }
}

/// The `release_state` vocabulary's two terminal states (agent-api.md).
fn is_terminal(state: &str) -> bool {
    matches!(state, "succeeded" | "failed")
}

/// Whether the actor's result is worth re-emitting on connect; `age` is its
/// `updated_at` age, `None` when unknown. See [`ReleaseManager::attach_upstream`]
/// for why the two exclusions exist.
///
/// Errs towards replaying: a duplicate is a documented no-op on the control
/// plane, a dropped live attempt is not. So a non-terminal result is kept at any
/// age, and a terminal one whose age is unknown is kept too.
fn replay_worthy(res: &ActorResult, age: Option<Duration>) -> bool {
    let all_ours = !res.components.is_empty()
        && res
            .components
            .iter()
            .all(|c| APPLIABLE_COMPONENTS.contains(&c.name.as_str()));
    if !all_ours {
        return false;
    }
    match age {
        Some(age) if is_terminal(&res.state) => age <= POLL_DEADLINE,
        _ => true,
    }
}

/// The age of an RFC 3339 timestamp; `None` when unparseable or in the future, which
/// [`replay_worthy`] treats as "unknown, keep".
fn rfc3339_age(stamp: &str) -> Option<Duration> {
    let at =
        time::OffsetDateTime::parse(stamp, &time::format_description::well_known::Rfc3339).ok()?;
    let at = SystemTime::UNIX_EPOCH + Duration::from_secs(u64::try_from(at.unix_timestamp()).ok()?);
    SystemTime::now().duration_since(at).ok()
}

fn unreachable_msg(request_id: &str) -> AgentMsg {
    AgentMsg::ReleaseState {
        request_id: request_id.to_string(),
        state: "failed".to_string(),
        reason: Some("updater_unreachable".to_string()),
        components: Vec::new(),
        previous: Vec::new(),
        output: String::new(),
        started_at: String::new(),
        updated_at: String::new(),
        finished_at: None,
        restored: false,
    }
}

fn ack(id: String) -> AgentMsg {
    AgentMsg::Ack {
        id,
        ok: true,
        error: None,
    }
}

/// One identifier from the `release_state` `reason` vocabulary, never a
/// sentence: the admin UI maps identifiers to text.
fn nack(id: String, reason: &str) -> AgentMsg {
    AgentMsg::Ack {
        id,
        ok: false,
        error: Some(reason.to_string()),
    }
}

/// Ack-time validation; the rejection reason, or None to proceed. The namespace
/// allowlist is host configuration the agent does not hold, so
/// `namespace_rejected` reaches the ack by relay rather than from here.
fn validate(
    request_id: &str,
    components: &[ReleaseComponent],
    appliable: &[&str],
) -> Option<&'static str> {
    if !is_uuid(request_id) {
        return Some("invalid");
    }
    if components.is_empty() {
        return Some("invalid");
    }
    for c in components {
        if !appliable.contains(&c.name.as_str()) {
            return Some("invalid");
        }
        if c.image.is_empty() || image_has_tag_or_digest(&c.image) {
            return Some("invalid");
        }
        if !is_digest(&c.digest) {
            return Some("digest_malformed");
        }
    }
    None
}

/// A tag is a `:` after the last `/`; `registry:5000/repo` is a port.
fn image_has_tag_or_digest(image: &str) -> bool {
    if image.contains('@') {
        return true;
    }
    match image.rfind('/') {
        Some(i) => image[i + 1..].contains(':'),
        None => image.contains(':'),
    }
}

fn is_digest(d: &str) -> bool {
    let Some(hex) = d.strip_prefix("sha256:") else {
        return false;
    };
    hex.len() == 64
        && hex
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

fn is_uuid(s: &str) -> bool {
    let groups = [8usize, 4, 4, 4, 12];
    let parts: Vec<&str> = s.split('-').collect();
    parts.len() == groups.len()
        && parts
            .iter()
            .zip(groups)
            .all(|(p, n)| p.len() == n && p.bytes().all(|b| b.is_ascii_hexdigit()))
}

#[cfg(test)]
mod tests;

#[cfg(test)]
mod tests_owned;
