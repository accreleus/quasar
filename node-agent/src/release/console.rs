//! Console access on an owned install (RH-07 #395; agent-api.md amendment 18,
//! `capacity.console_capabilities.access`).
//!
//! On an `owned` host the recovery actor creates the node agent with the console additions
//! (display, sound, monitor control) only while console mode is on, and marks such an agent
//! with [`MARKER_ENV`]. So a change of `config_update.console_config.enabled` makes this
//! agent ask its actor, on the agent socket (`POST /v1/console`), to replace it; the actor
//! verifies the new agent and puts the old one back if it does not verify. This module is
//! that reconciler and the `access` report it keeps. It holds no state the actor does not:
//! every decision starts from a fresh `GET /v1/console`
//! (`testdata/recovery/agent-socket`, not frozen).
//!
//! **What starts a replacement** (the contract's rule): a received `enabled` that differs
//! from whether this agent has access (`on`, or `restored` from a failed turn-off), while
//! nothing is `applying` and the state is not `unsupported`. A resent or unchanged config
//! starts nothing. On top of the rule, no retry loop: a target whose attempt was put back
//! (or that the actor refused) is not asked for again until this process has received an
//! `enabled` that differs from it.
//!
//! A host with no recovery actor (Compose, source) reports no `access` at all, and never
//! asks for anything.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::{Arc, Mutex, RwLock, Weak};
use std::time::Duration;

use serde::Deserialize;
use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use super::unix_http;
use crate::messages::{ConsoleAccess, ConsoleAccessState, VideoTopology};

/// Set to `1` by the recovery actor's recipe on an agent created with console access
/// (`quasar_recovery::recipe::CONSOLE_ACCESS_ENV`).
pub const MARKER_ENV: &str = "QUASAR_CONSOLE_ACCESS";

/// How often the actor's status is read while an attempt is applying. The actor bounds
/// the attempt itself (its verification deadline), so this never polls for ever.
const POLL_APPLYING: Duration = Duration::from_secs(2);

/// How often the actor's status is read again while it says console mode is unsupported.
/// Its answer can change without a restart of this agent: an update this agent started
/// under reads the machine record before the actor has written the new recipe revision.
const POLL_UNSUPPORTED: Duration = Duration::from_secs(30);

/// How long one request to the actor may take.
const SOCKET_TIMEOUT: Duration = Duration::from_secs(10);

/// The `release_state` failure identifier for a put-back attempt the actor recorded no
/// reason for (it was never journalled, so the actor restarted before it began).
const REASON_UNKNOWN_RESTORE: &str = "interrupted";

/// The part of the actor's `GET /v1/console` this agent reads. Extra fields are ignored.
#[derive(Deserialize, Debug, Clone, PartialEq, Eq)]
struct ActorConsole {
    enabled: bool,
    #[serde(default)]
    in_flight: Option<String>,
    #[serde(default)]
    in_flight_target: Option<bool>,
    #[serde(default)]
    in_flight_started_at: Option<String>,
    #[serde(default)]
    last: Option<ActorConsoleLast>,
    #[serde(default = "yes")]
    supported: bool,
    #[serde(default)]
    why: Option<String>,
}

fn yes() -> bool {
    true
}

#[derive(Deserialize, Debug, Clone, PartialEq, Eq)]
struct ActorConsoleLast {
    #[serde(default)]
    request_id: Option<String>,
    target: bool,
    /// `applied`, `put_back` or `partial`.
    settled: String,
    #[serde(default)]
    reason: Option<String>,
    #[serde(default)]
    started_at: Option<String>,
    #[serde(default)]
    finished_at: Option<String>,
}

impl ActorConsoleLast {
    fn put_back(&self) -> bool {
        self.settled == "put_back"
    }

    /// What identifies the attempt for the no-retry rule.
    fn key(&self) -> String {
        self.request_id
            .clone()
            .or_else(|| self.finished_at.clone())
            .unwrap_or_default()
    }
}

#[derive(Deserialize, Debug)]
struct ActorRefusal {
    #[serde(default)]
    reason: String,
    #[serde(default)]
    message: String,
}

/// The engine mode as `register` reports it (amendment 17).
const MODE_UNKNOWN: u8 = 0;
const MODE_ROOTFUL: u8 = 1;
const MODE_ROOTLESS: u8 = 2;

#[derive(Default)]
struct Inner {
    /// The actor's last answer; `None` until it answered once.
    actor: Option<ActorConsole>,
    report: Option<ConsoleAccess>,
    /// The put-back attempt whose target this process may ask for again: it has received
    /// an `enabled` that differs from that target since.
    lifted: Option<String>,
    /// A target the actor refused (`400`), not asked for again until `enabled` differs.
    refused: Option<bool>,
}

pub struct ConsoleAccessManager {
    /// The recovery actor's agent socket; `None` on a host with no actor.
    socket: Option<PathBuf>,
    /// This agent was created with console access ([`MARKER_ENV`]).
    marker: bool,
    engine_mode: AtomicU8,
    inner: Mutex<Inner>,
    /// Serialises the read-decide-submit sequence.
    op: Mutex<()>,
    /// The connection's wake-up to re-send `capacity`.
    upstream: Mutex<Option<mpsc::Sender<()>>>,
    /// The worker that runs requests and polls an applying attempt.
    worker: Mutex<Option<std::sync::mpsc::Sender<bool>>>,
    /// Publish the report for [`published`] (the process-wide manager only).
    publish: bool,
    poll: Duration,
    /// How often an `unsupported` answer from the actor is read again.
    poll_unsupported: Duration,
}

/// The process-wide manager's current report, for every `capacity` the agent sends.
static PUBLISHED: RwLock<Option<ConsoleAccess>> = RwLock::new(None);

/// The current `console_capabilities.access`; `None` on a host with no recovery actor.
pub fn published() -> Option<ConsoleAccess> {
    PUBLISHED.read().ok().and_then(|slot| slot.clone())
}

impl ConsoleAccessManager {
    /// The process-wide manager: the actor socket and the marker from the environment.
    pub fn from_env() -> Arc<Self> {
        let marker = std::env::var(MARKER_ENV).is_ok_and(|v| v.trim() == "1");
        let socket = crate::buildinfo::owned_socket();
        let mgr = Self::build(socket, marker, true, POLL_APPLYING);
        if mgr.socket.is_some() {
            info!(
                console_access = marker,
                "owned install: console access is managed through the recovery actor"
            );
        }
        mgr
    }

    /// A host with no recovery actor: no `access`, nothing asked for, nothing refused.
    pub fn without_actor() -> Arc<Self> {
        Self::build(None, false, false, POLL_APPLYING)
    }

    #[cfg(test)]
    pub fn owned_for_test(socket: impl Into<PathBuf>, marker: bool) -> Arc<Self> {
        Self::build(
            Some(socket.into()),
            marker,
            false,
            Duration::from_millis(20),
        )
    }

    fn build(socket: Option<PathBuf>, marker: bool, publish: bool, poll: Duration) -> Arc<Self> {
        Arc::new(ConsoleAccessManager {
            socket,
            marker,
            engine_mode: AtomicU8::new(MODE_UNKNOWN),
            inner: Mutex::new(Inner::default()),
            op: Mutex::new(()),
            upstream: Mutex::new(None),
            worker: Mutex::new(None),
            publish,
            poll,
            // Tests poll fast; a real agent re-reads an unsupported answer every half minute.
            poll_unsupported: if publish { POLL_UNSUPPORTED } else { poll },
        })
    }

    /// Whether this host's console access is managed through a recovery actor.
    pub fn owned(&self) -> bool {
        self.socket.is_some()
    }

    /// The engine mode `register` reports (`rootful`, `rootless`, or unknown).
    pub fn set_engine_mode(&self, mode: Option<&str>) {
        let m = match mode {
            Some("rootless") => MODE_ROOTLESS,
            Some("rootful") => MODE_ROOTFUL,
            _ => MODE_UNKNOWN,
        };
        self.engine_mode.store(m, Ordering::SeqCst);
    }

    fn rootless(&self) -> bool {
        self.engine_mode.load(Ordering::SeqCst) == MODE_ROOTLESS
    }

    /// The current `access`; `None` on a host with no recovery actor.
    pub fn report(&self) -> Option<ConsoleAccess> {
        self.socket.as_ref()?;
        let inner = self.inner.lock().unwrap();
        Some(inner.report.clone().unwrap_or_else(|| {
            derive(
                self.marker,
                self.rootless(),
                inner.actor.as_ref().unwrap_or(&unknown(self.marker)),
            )
        }))
    }

    /// This connection's wake-up: one message whenever `access` changes, so the
    /// connection re-sends `capacity`. Replaces the previous connection's.
    pub fn subscribe(&self) -> mpsc::Receiver<()> {
        let (tx, rx) = mpsc::channel(1);
        *self.upstream.lock().unwrap() = Some(tx);
        rx
    }

    /// Why a console session (`local_only`, `dual_output`) is refused on this agent:
    /// on an owned host, an agent created without console access cannot drive the
    /// display, so the launch fails closed here rather than half-way through its build.
    /// Reads how the agent was created, never the `access` state.
    pub fn launch_refusal(&self, topology: VideoTopology) -> Option<String> {
        if topology == VideoTopology::StreamOnly || !self.owned() || self.marker {
            return None;
        }
        Some(
            "console mode is not available on this node agent: it was created without \
             console access (turn console mode on for this host and wait for it to apply)"
                .to_string(),
        )
    }

    /// Re-read the actor (the start of every connection). Blocking.
    pub fn refresh(self: &Arc<Self>) {
        if !self.owned() {
            return;
        }
        let _op = self.op.lock().unwrap();
        self.read_actor();
        self.recompute();
    }

    /// A `config_update` carried `console_config`: reconcile toward its `enabled` on the
    /// worker. Returns at once.
    pub fn request(self: &Arc<Self>, enabled: bool) {
        if !self.owned() {
            return;
        }
        let tx = self.worker_tx();
        if tx.send(enabled).is_err() {
            warn!(
                token = "console-access-worker-gone",
                "the console access worker is gone; console mode is not reconciled"
            );
        }
    }

    fn worker_tx(self: &Arc<Self>) -> std::sync::mpsc::Sender<bool> {
        let mut slot = self.worker.lock().unwrap();
        if let Some(tx) = slot.as_ref() {
            return tx.clone();
        }
        let (tx, rx) = std::sync::mpsc::channel::<bool>();
        let weak = Arc::downgrade(self);
        std::thread::Builder::new()
            .name("quasar-console-access".into())
            .spawn(move || worker(weak, rx))
            .expect("failed to spawn the console access worker");
        *slot = Some(tx.clone());
        tx
    }

    /// Start the worker if the actor's answer is still expected to change (an attempt is
    /// applying, or the actor said unsupported), so it is read again until it settles.
    pub fn watch_if_applying(self: &Arc<Self>) {
        if self.recheck().is_some() {
            let _ = self.worker_tx();
        }
    }

    /// How soon the actor's status should be read again, if at all.
    fn recheck(&self) -> Option<Duration> {
        if self.applying() {
            return Some(self.poll);
        }
        // A rootless engine is this agent's own finding and cannot change while it runs.
        let unsupported = self
            .inner
            .lock()
            .unwrap()
            .report
            .as_ref()
            .is_some_and(|r| r.state == ConsoleAccessState::Unsupported);
        (unsupported && !self.rootless()).then_some(self.poll_unsupported)
    }

    fn applying(&self) -> bool {
        self.inner
            .lock()
            .unwrap()
            .report
            .as_ref()
            .is_some_and(|r| r.state == ConsoleAccessState::Applying)
    }

    /// The contract's trigger, then the submit. Blocking.
    pub fn reconcile(self: &Arc<Self>, want: bool) {
        if !self.owned() {
            return;
        }
        let _op = self.op.lock().unwrap();
        self.read_actor();
        self.recompute();
        let (report, suppressed) = {
            let mut inner = self.inner.lock().unwrap();
            // Receiving an `enabled` that differs from a put-back or refused target lifts
            // the hold on asking for that target again.
            if let Some(last) = inner.actor.as_ref().and_then(|a| a.last.clone()) {
                if last.put_back() && last.target != want {
                    inner.lifted = Some(last.key());
                }
            }
            if inner.refused.is_some_and(|t| t != want) {
                inner.refused = None;
            }
            let held_put_back = inner
                .actor
                .as_ref()
                .and_then(|a| a.last.as_ref())
                .filter(|l| l.put_back() && l.target == want)
                .is_some_and(|l| inner.lifted.as_deref() != Some(l.key().as_str()));
            let suppressed = held_put_back || inner.refused == Some(want);
            (inner.report.clone(), suppressed)
        };
        let Some(report) = report else { return };
        if !starts_replacement(&report, want) {
            debug!(
                want,
                state = ?report.state,
                "console access: no replacement (already in agreement, applying, or unsupported)"
            );
            return;
        }
        if suppressed {
            info!(
                token = "console-access-held",
                want,
                "console mode {} is not asked for again: the last attempt toward it was put back or refused; turn it {} and back to try again",
                on_off(want),
                on_off(!want)
            );
            return;
        }
        self.submit(want);
        self.recompute();
    }

    fn submit(&self, want: bool) {
        let Some(socket) = self.socket.as_deref() else {
            return;
        };
        let body = serde_json::json!({ "enabled": want }).to_string();
        info!(
            token = "console-access-submit",
            want,
            "asking the recovery actor to replace this agent with console mode {}",
            on_off(want)
        );
        match unix_http::request(socket, "POST", "/v1/console", Some(&body), SOCKET_TIMEOUT) {
            Ok(r) if r.status == 202 || r.status == 200 => {
                match serde_json::from_str::<ActorConsole>(&r.body) {
                    Ok(status) => self.inner.lock().unwrap().actor = Some(status),
                    Err(e) => {
                        debug!("console status unparsable: {e}");
                        self.read_actor();
                    }
                }
            }
            Ok(r) => {
                let refusal = serde_json::from_str::<ActorRefusal>(&r.body).ok();
                let (reason, message) = refusal
                    .map(|e| (e.reason, e.message))
                    .unwrap_or_else(|| ("invalid".into(), r.body.clone()));
                warn!(
                    token = "console-access-refused",
                    status = r.status,
                    "the recovery actor refused console mode {} ({reason}): {message}",
                    on_off(want)
                );
                if reason != "busy" {
                    self.inner.lock().unwrap().refused = Some(want);
                }
                self.read_actor();
            }
            Err(e) => warn!(
                token = "console-access-actor-unreachable",
                "console mode {}: the recovery actor at {} did not answer: {e}",
                on_off(want),
                socket.display()
            ),
        }
    }

    /// `GET /v1/console` into `inner.actor`; kept as it was when the actor did not answer.
    fn read_actor(&self) {
        let Some(socket) = self.socket.as_deref() else {
            return;
        };
        if let Some(status) = get_status(socket) {
            self.inner.lock().unwrap().actor = Some(status);
        }
    }

    /// Recompute the report from what is known, and publish and wake the connection
    /// when it changed.
    fn recompute(&self) {
        let rootless = self.rootless();
        let changed = {
            let mut inner = self.inner.lock().unwrap();
            let actor = inner.actor.clone().unwrap_or_else(|| unknown(self.marker));
            let next = derive(self.marker, rootless, &actor);
            if inner.report.as_ref() == Some(&next) {
                None
            } else {
                inner.report = Some(next.clone());
                Some(next)
            }
        };
        let Some(next) = changed else { return };
        info!(
            token = "console-access-state",
            state = ?next.state,
            target = ?next.target,
            request_id = next.request_id.as_deref().unwrap_or(""),
            reason = next.reason.as_deref().unwrap_or(""),
            "console access: {}",
            next.summary
        );
        if self.publish {
            if let Ok(mut slot) = PUBLISHED.write() {
                *slot = Some(next);
            }
        }
        if let Some(tx) = self.upstream.lock().unwrap().as_ref() {
            // Full: a re-send is already due, and it reads the report fresh.
            let _ = tx.try_send(());
        }
    }
}

/// Runs requests in order, and polls the actor while an attempt is applying.
fn worker(mgr: Weak<ConsoleAccessManager>, rx: std::sync::mpsc::Receiver<bool>) {
    loop {
        let wait = match mgr.upgrade() {
            Some(m) => m.recheck().unwrap_or(Duration::from_secs(3600)),
            None => return,
        };
        match rx.recv_timeout(wait) {
            Ok(want) => match mgr.upgrade() {
                Some(m) => m.reconcile(want),
                None => return,
            },
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => match mgr.upgrade() {
                Some(m) if m.recheck().is_some() => m.refresh(),
                Some(_) => {}
                None => return,
            },
            Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => return,
        }
    }
}

fn get_status(socket: &Path) -> Option<ActorConsole> {
    let reply = match unix_http::request(socket, "GET", "/v1/console", None, SOCKET_TIMEOUT) {
        Ok(r) => r,
        Err(e) => {
            debug!("recovery actor console status: {e}");
            return None;
        }
    };
    if reply.status != 200 {
        debug!(
            "recovery actor console status answered {}: {}",
            reply.status, reply.body
        );
        return None;
    }
    serde_json::from_str(&reply.body)
        .map_err(|e| debug!("recovery actor console status unparsable: {e}"))
        .ok()
}

/// What is assumed before the actor first answers: the access this agent was created with.
fn unknown(marker: bool) -> ActorConsole {
    ActorConsole {
        enabled: marker,
        in_flight: None,
        in_flight_target: None,
        in_flight_started_at: None,
        last: None,
        supported: true,
        why: None,
    }
}

/// **Has access** (agent-api.md): `on`, or `restored` from a failed attempt to turn it off.
pub fn has_access(report: &ConsoleAccess) -> bool {
    match report.state {
        ConsoleAccessState::On => true,
        ConsoleAccessState::Restored => report.target == Some(false),
        _ => false,
    }
}

/// **What starts a replacement** (agent-api.md): `want` differs from has-access, nothing
/// is applying, and the state is not unsupported.
fn starts_replacement(report: &ConsoleAccess, want: bool) -> bool {
    !matches!(
        report.state,
        ConsoleAccessState::Applying | ConsoleAccessState::Unsupported
    ) && want != has_access(report)
}

fn on_off(b: bool) -> &'static str {
    if b {
        "on"
    } else {
        "off"
    }
}

fn rfc3339(stamp: Option<&str>) -> Option<String> {
    let s = stamp?;
    time::OffsetDateTime::parse(s, &time::format_description::well_known::Rfc3339)
        .ok()
        .map(|_| s.to_string())
}

/// The `access` report from how this agent was created (`marker`), the engine mode and
/// the actor's console status.
fn derive(marker: bool, rootless: bool, actor: &ActorConsole) -> ConsoleAccess {
    let unsupported = |summary: String| ConsoleAccess {
        state: ConsoleAccessState::Unsupported,
        target: None,
        request_id: None,
        reason: None,
        started_at: None,
        finished_at: None,
        summary,
    };
    if rootless {
        return unsupported(
            "Console mode needs a rootful container engine on this host for now; console \
             access on a rootless engine comes with later rootless work (RH07-15)."
                .into(),
        );
    }
    if let Some(id) = &actor.in_flight {
        let target = actor.in_flight_target.unwrap_or(!actor.enabled);
        return ConsoleAccess {
            state: ConsoleAccessState::Applying,
            target: Some(target),
            request_id: Some(id.clone()),
            reason: None,
            started_at: rfc3339(actor.in_flight_started_at.as_deref()),
            finished_at: None,
            summary: format!(
                "The recovery actor is replacing the node agent to turn console mode {}.",
                on_off(target)
            ),
        };
    }
    if !actor.supported {
        let why = actor
            .why
            .as_deref()
            .map(str::trim)
            .filter(|w| !w.is_empty())
            .unwrap_or("it gave no reason");
        return unsupported(format!(
            "This host's recovery actor cannot give the node agent console access: {why}."
        ));
    }
    let plain = |on: bool| ConsoleAccess {
        state: if on {
            ConsoleAccessState::On
        } else {
            ConsoleAccessState::Off
        },
        target: None,
        request_id: None,
        reason: None,
        started_at: None,
        finished_at: None,
        summary: if on {
            "The node agent has console access.".into()
        } else {
            "The node agent has no console access.".into()
        },
    };
    let Some(last) = &actor.last else {
        return plain(marker);
    };
    // The record is about this agent only when this agent has the access the attempt
    // left in force: the target once applied, the access it moved away from once put back.
    // (Applied also covers `partial`, whose new inputs are in force.)
    if (marker == last.target) == last.put_back() {
        return plain(marker);
    }
    let mut report = if last.put_back() {
        let reason = last
            .reason
            .clone()
            .filter(|r| !r.is_empty())
            .unwrap_or_else(|| REASON_UNKNOWN_RESTORE.into());
        ConsoleAccess {
            state: ConsoleAccessState::Restored,
            target: None,
            request_id: None,
            reason: Some(reason.clone()),
            started_at: None,
            finished_at: None,
            summary: format!(
                "Turning console mode {} did not complete ({reason}), so the recovery actor put \
                 the previous node agent back; console mode is {}.",
                on_off(last.target),
                on_off(!last.target)
            ),
        }
    } else {
        plain(marker)
    };
    report.target = Some(last.target);
    report.request_id = last.request_id.clone().filter(|id| !id.is_empty());
    report.started_at = rfc3339(last.started_at.as_deref());
    report.finished_at = rfc3339(last.finished_at.as_deref());
    report
}

#[cfg(test)]
mod tests_derive {
    use super::*;

    fn last(target: bool, settled: &str, reason: Option<&str>) -> ActorConsoleLast {
        ActorConsoleLast {
            request_id: Some("0b7e3c52-6f0e-4d0a-9c1e-5a2b7d8e9f10".into()),
            target,
            settled: settled.into(),
            reason: reason.map(Into::into),
            started_at: Some("2026-09-29T10:00:01Z".into()),
            finished_at: Some("2026-09-29T10:00:05Z".into()),
        }
    }

    fn status(enabled: bool, last: Option<ActorConsoleLast>) -> ActorConsole {
        ActorConsole {
            last,
            ..unknown(enabled)
        }
    }

    #[test]
    fn has_access_is_on_or_a_restored_turn_off() {
        let on = derive(
            true,
            false,
            &status(true, Some(last(true, "applied", None))),
        );
        assert!(has_access(&on));
        let restored_off = derive(
            true,
            false,
            &status(true, Some(last(false, "put_back", Some("unhealthy")))),
        );
        assert_eq!(restored_off.state, ConsoleAccessState::Restored);
        assert!(has_access(&restored_off));
        let restored_on = derive(
            false,
            false,
            &status(false, Some(last(true, "put_back", Some("unhealthy")))),
        );
        assert!(!has_access(&restored_on));
        assert!(!has_access(&derive(false, false, &status(false, None))));
    }

    #[test]
    fn a_restored_trigger_reads_has_access_not_the_state_name() {
        // Turning it off failed: the host kept access, so `enabled: true` agrees.
        let restored_off = derive(
            true,
            false,
            &status(true, Some(last(false, "put_back", Some("unhealthy")))),
        );
        assert!(!starts_replacement(&restored_off, true));
        assert!(starts_replacement(&restored_off, false));
        // Turning it on failed: no access, so `enabled: false` agrees.
        let restored_on = derive(
            false,
            false,
            &status(false, Some(last(true, "put_back", Some("unhealthy")))),
        );
        assert!(!starts_replacement(&restored_on, false));
        assert!(starts_replacement(&restored_on, true));
    }

    #[test]
    fn an_unjournalled_put_back_reads_interrupted() {
        let r = derive(
            false,
            false,
            &status(false, Some(last(true, "put_back", None))),
        );
        assert_eq!(r.reason.as_deref(), Some("interrupted"));
    }

    #[test]
    fn a_timestamp_that_is_not_rfc3339_is_null() {
        let mut l = last(true, "applied", None);
        l.finished_at = Some("yesterday".into());
        let r = derive(true, false, &status(true, Some(l)));
        assert_eq!(r.finished_at, None);
        assert_eq!(r.started_at.as_deref(), Some("2026-09-29T10:00:01Z"));
    }
}
