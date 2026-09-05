//! Agent-side client for this host's updater (`CONTEXT.md`).
//!
//! The agent relays and does not author: `release_apply` is validated, acked on
//! acceptance, POSTed to the local socket, and every `release_state` after that
//! is a re-frame of the updater's result file. This module runs no compose
//! command, keeps no apply state, and performs no session logic at any point
//! (agent-api.md `release_state`).
//!
//! The socket and the result file are NOT a frozen interface
//! (protocol/schema.md §"Not frozen: the updater's local socket").

mod unix_http;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use serde::Deserialize;
use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use crate::messages::{AgentMsg, ReleaseComponent, ReleaseInfo, ReleasePrevious};

pub const DEFAULT_SOCKET: &str = "/run/quasar-updater/updater.sock";
pub const DEFAULT_RESULTS_DIR: &str = "/run/quasar-updater/results";

/// Poll cadence against the result file. The wire's floor is one message per
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
const APPLIABLE_COMPONENTS: &[&str] = &["node-agent"];

/// The updater's result file, which carries `release_state`'s fields under the
/// same names. Extra fields (`restored`, `commands`, `release`) are ignored.
#[derive(Deserialize, Debug, Clone)]
struct UpdaterResult {
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
}

impl UpdaterResult {
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
        }
    }
}

#[derive(Deserialize, Debug)]
struct UpdaterError {
    #[serde(default)]
    reason: String,
}

pub struct ReleaseManager {
    socket: PathBuf,
    results_dir: PathBuf,
    upstream: RwLock<Option<mpsc::Sender<AgentMsg>>>,
    /// Single-flight per host: refuse, never queue. Holds the request id of the
    /// apply whose poller is still running.
    inflight: Mutex<Option<String>>,
    /// [`UNREACHABLE_AFTER`] in millis, overridable so tests do not sleep for
    /// three minutes to observe a timeout.
    unreachable_after_ms: AtomicU64,
}

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
    pub fn new(socket: impl Into<PathBuf>, results_dir: impl Into<PathBuf>) -> Arc<Self> {
        Arc::new(ReleaseManager {
            socket: socket.into(),
            results_dir: results_dir.into(),
            upstream: RwLock::new(None),
            inflight: Mutex::new(None),
            unreachable_after_ms: AtomicU64::new(UNREACHABLE_AFTER.as_millis() as u64),
        })
    }

    pub fn from_env() -> Arc<Self> {
        let sock = std::env::var("QUASAR_UPDATER_SOCKET")
            .ok()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| DEFAULT_SOCKET.to_string());
        let results = std::env::var("QUASAR_UPDATER_RESULTS_DIR")
            .ok()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| DEFAULT_RESULTS_DIR.to_string());
        Self::new(sock, results)
    }

    /// Attach this connection's channel and re-emit the current state of every
    /// result file still present, so a control plane that missed frames catches
    /// up without asking and an agent replaced mid-apply still reports the
    /// apply that replaced it.
    pub fn attach_upstream(self: &Arc<Self>, tx: mpsc::Sender<AgentMsg>) -> UpstreamGuard {
        *self.upstream.write().unwrap() = Some(tx);
        for res in self.read_all_results() {
            info!(
                "release apply {}: re-emitting state {} after connect",
                res.request_id, res.state
            );
            self.send(res.into_msg());
        }
        UpstreamGuard { mgr: self.clone() }
    }

    #[cfg(test)]
    pub fn set_unreachable_after(&self, d: Duration) {
        self.unreachable_after_ms
            .store(d.as_millis() as u64, Ordering::Relaxed);
    }

    /// Whether this host has an updater to hand a request to. The compose
    /// service's presence is reported separately on `register`
    /// (`buildinfo::updater_present`); this is the socket itself.
    pub fn present(&self) -> bool {
        self.socket.exists()
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
        if let Some(reason) = validate(&request_id, &components) {
            warn!(
                token = "release-apply-rejected",
                "release_apply {request_id} rejected: {reason}"
            );
            return nack(id, reason);
        }
        if !self.present() {
            warn!(
                token = "release-apply-no-updater",
                "release_apply {request_id}: no updater socket at {}",
                self.socket.display()
            );
            return nack(id, "updater_absent");
        }

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

        let body = serde_json::json!({
            "request_id": request_id,
            "components": components,
            "release": release,
        })
        .to_string();

        let reply = unix_http::request(
            &self.socket,
            "POST",
            "/v1/apply",
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
                info!("release_apply {request_id} accepted by the updater (force={force})");
                self.spawn_poller(request_id);
                ack(id)
            }
            Ok(r) => {
                self.clear_inflight(&request_id);
                let reason = serde_json::from_str::<UpdaterError>(&r.body)
                    .ok()
                    .map(|e| e.reason)
                    .filter(|s| !s.is_empty())
                    .unwrap_or_else(|| "invalid".to_string());
                warn!(
                    token = "release-apply-updater-rejected",
                    "release_apply {request_id} rejected by the updater ({}): {reason}", r.status
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
                        let terminal = matches!(res.state.as_str(), "succeeded" | "failed");
                        if last.as_deref() != Some(res.state.as_str()) {
                            last = Some(res.state.clone());
                            info!("release apply {request_id}: {}", res.state);
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

    /// The result file over the socket, or straight off the shared volume when
    /// the socket is unavailable — the same file either way, and the direct read
    /// keeps an apply observable while the updater itself restarts.
    fn read_result(&self, request_id: &str) -> Option<UpdaterResult> {
        let over_socket = unix_http::request(
            &self.socket,
            "GET",
            &format!("/v1/results/{request_id}"),
            None,
            Duration::from_secs(10),
        );
        if let Ok(r) = over_socket {
            if r.status == 200 {
                match serde_json::from_str::<UpdaterResult>(&r.body) {
                    Ok(res) => return Some(res),
                    Err(e) => {
                        debug!("release apply {request_id}: unparsable result over the socket: {e}")
                    }
                }
            } else if r.status == 404 {
                return None;
            }
        }
        self.read_result_file(&self.results_dir.join(format!("{request_id}.json")))
    }

    fn read_result_file(&self, path: &Path) -> Option<UpdaterResult> {
        let body = std::fs::read_to_string(path).ok()?;
        match serde_json::from_str::<UpdaterResult>(&body) {
            Ok(res) => Some(res),
            Err(e) => {
                debug!("unparsable updater result {}: {e}", path.display());
                None
            }
        }
    }

    /// Every result file present, one per request id, ordered so the re-emit is
    /// deterministic.
    fn read_all_results(&self) -> Vec<UpdaterResult> {
        let Ok(entries) = std::fs::read_dir(&self.results_dir) else {
            return Vec::new();
        };
        let mut by_id: BTreeMap<String, UpdaterResult> = BTreeMap::new();
        for entry in entries.flatten() {
            let path = entry.path();
            if path.extension().and_then(|e| e.to_str()) != Some("json") {
                continue;
            }
            if let Some(res) = self.read_result_file(&path) {
                by_id.insert(res.request_id.clone(), res);
            }
        }
        by_id.into_values().collect()
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
fn validate(request_id: &str, components: &[ReleaseComponent]) -> Option<&'static str> {
    if !is_uuid(request_id) {
        return Some("invalid");
    }
    if components.is_empty() {
        return Some("invalid");
    }
    for c in components {
        if !APPLIABLE_COMPONENTS.contains(&c.name.as_str()) {
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
