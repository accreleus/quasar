//! Minimal liveness/health HTTP endpoint for the node-agent.
//!
//! Serves `GET /health` -> `200 {"status":"ok","sessions":<n>,"connected":<bool>}`
//! on a small hand-rolled HTTP/1.1 listener (no framework — a health check does
//! not need one, and the crate deliberately avoids axum/hyper). Bind address is
//! `QUASAR_HEALTH_ADDR` (default `127.0.0.1:9091`); an empty string or `"0"`
//! disables the feature entirely.

use std::io::{Read, Write};
use std::net::TcpListener;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tracing::{info, warn};

/// #519: after this many consecutive failed connect/register attempts with no
/// intervening `Registered`, health flips unhealthy — a rejected enrollment
/// cannot self-heal by retrying. 5 attempts is ~30s of the 1s→2s→4s→8s→16s
/// backoff: long enough a transient blip recovers first, short enough an
/// operator isn't left guessing.
pub const UNHEALTHY_AFTER_CONSECUTIVE_FAILURES: usize = 5;

/// #410: read/write deadline on an accepted health connection — without it a
/// prober that never sends a FIN parks its handler thread (and stack) forever.
const IO_TIMEOUT: Duration = Duration::from_secs(2);

/// #410: max concurrently-handled health connections. One OS thread per
/// connection, so an unbounded accept loop on a host-networked port lets any
/// process on the box convert idle sockets into permanent threads. Beyond the
/// cap gets an immediate 503 + close on the accept thread.
const MAX_INFLIGHT: usize = 16;

const SERVICE_UNAVAILABLE: &str = "HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: 4\r\nConnection: close\r\n\r\nbusy";

/// Drain time for a shed connection's in-flight bytes before closing: an
/// unread receive queue sends an RST that destroys the 503 in flight, a brief
/// drain gets a clean FIN. Must stay much shorter than [`IO_TIMEOUT`] since
/// this runs on the accept thread and must never stall accepts.
const SHED_DRAIN_TIMEOUT: Duration = Duration::from_millis(100);

/// Decrements the in-flight counter however the handler thread ends (normal
/// return, early return on a read error/timeout, or a panic).
struct InflightGuard(Arc<AtomicUsize>);

impl Drop for InflightGuard {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

/// Shared counters the agent loop updates at the natural state-change points
/// (session insert/remove, WS connect/disconnect); the health listener only reads.
#[derive(Debug, Default)]
pub struct HealthState {
    sessions: AtomicUsize,
    connected: AtomicBool,
    /// #519: consecutive connect/register attempts since the last `Registered`.
    /// A count, not a bool, so the threshold and the reported number share one
    /// source of truth.
    consecutive_registration_failures: AtomicUsize,
    /// #519: most recent registration-failure reason, surfaced in `/health` so
    /// an operator sees why without a log tail. `None` once registered again.
    last_registration_error: Mutex<Option<String>>,
}

impl HealthState {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    pub fn set_sessions(&self, n: usize) {
        self.sessions.store(n, Ordering::Relaxed);
    }

    pub fn set_connected(&self, connected: bool) {
        self.connected.store(connected, Ordering::Relaxed);
    }

    /// Records one failed connect/register cycle; returns the new count so the
    /// caller can log a one-shot transition at [`UNHEALTHY_AFTER_CONSECUTIVE_FAILURES`]
    /// instead of re-warning on every retry.
    pub fn record_registration_failure(&self, reason: &str) -> usize {
        let failures = self
            .consecutive_registration_failures
            .fetch_add(1, Ordering::Relaxed)
            + 1;
        if let Ok(mut slot) = self.last_registration_error.lock() {
            *slot = Some(reason.to_string());
        }
        failures
    }

    pub fn record_registered(&self) {
        self.consecutive_registration_failures
            .store(0, Ordering::Relaxed);
        if let Ok(mut slot) = self.last_registration_error.lock() {
            *slot = None;
        }
    }

    fn consecutive_registration_failures(&self) -> usize {
        self.consecutive_registration_failures
            .load(Ordering::Relaxed)
    }

    fn last_registration_error(&self) -> Option<String> {
        self.last_registration_error
            .lock()
            .ok()
            .and_then(|g| g.clone())
    }
}

/// Pure threshold decision, unit-testable without a `HealthState` or sockets.
fn is_unhealthy(consecutive_failures: usize) -> bool {
    consecutive_failures >= UNHEALTHY_AFTER_CONSECUTIVE_FAILURES
}

/// Resolve `QUASAR_HEALTH_ADDR` into a bind address. `None` means the feature
/// is disabled (empty string, `"0"`, or unset-with-no-default — though the
/// documented default is `127.0.0.1:9091`).
pub fn addr_from_env() -> Option<String> {
    let raw = std::env::var("QUASAR_HEALTH_ADDR").unwrap_or_else(|_| "127.0.0.1:9091".to_string());
    let trimmed = raw.trim();
    if trimmed.is_empty() || trimmed == "0" {
        None
    } else {
        Some(trimmed.to_string())
    }
}

/// Spawn the health listener on a blocking thread if enabled by env. No-op
/// (returns immediately, nothing spawned) when disabled.
pub fn spawn_if_enabled(state: Arc<HealthState>) {
    let Some(addr) = addr_from_env() else {
        info!("health endpoint disabled (QUASAR_HEALTH_ADDR empty/0)");
        return;
    };
    std::thread::spawn(move || serve(&addr, state));
}

/// Blocking accept loop; one thread per connection (health checks are rare and
/// tiny, so simplicity wins over pooling).
fn serve(addr: &str, state: Arc<HealthState>) {
    let listener = match TcpListener::bind(addr) {
        Ok(l) => l,
        Err(e) => {
            tracing::error!(
                token = "health-bind-failed",
                "health: failed to bind {addr}: {e}"
            );
            return;
        }
    };
    info!("health endpoint listening on {addr}");
    serve_listener(listener, state);
}

/// The accept loop proper, split out from `serve` so the tests drive the real
/// path (timeouts + in-flight cap included) on an ephemeral port instead of a
/// copy of it.
fn serve_listener(listener: TcpListener, state: Arc<HealthState>) {
    let inflight = Arc::new(AtomicUsize::new(0));
    for conn in listener.incoming() {
        match conn {
            Ok(mut stream) => {
                let _ = stream.set_read_timeout(Some(IO_TIMEOUT));
                let _ = stream.set_write_timeout(Some(IO_TIMEOUT));
                // fetch_add returns the PREVIOUS value, so >= MAX_INFLIGHT means
                // the cap was already reached; undo the increment and shed here.
                if inflight.fetch_add(1, Ordering::AcqRel) >= MAX_INFLIGHT {
                    inflight.fetch_sub(1, Ordering::AcqRel);
                    let _ = stream.write_all(SERVICE_UNAVAILABLE.as_bytes());
                    let _ = stream.set_read_timeout(Some(SHED_DRAIN_TIMEOUT));
                    let mut discard = [0u8; 1024];
                    let _ = stream.read(&mut discard);
                    continue;
                }
                let guard = InflightGuard(inflight.clone());
                let state = state.clone();
                std::thread::spawn(move || {
                    let _guard = guard;
                    handle_conn(stream, &state)
                });
            }
            Err(e) => warn!(token = "health-accept-error", "health: accept error: {e}"),
        }
    }
}

fn handle_conn(mut stream: std::net::TcpStream, state: &HealthState) {
    // Only the request line matters for routing; no keep-alive support needed.
    let mut buf = [0u8; 1024];
    let n = match stream.read(&mut buf) {
        Ok(n) => n,
        Err(_) => return,
    };
    let request = String::from_utf8_lossy(&buf[..n]);
    let is_health_get = request
        .lines()
        .next()
        .map(|line| line.starts_with("GET /health"))
        .unwrap_or(false);

    let response = if is_health_get {
        let sessions = state.sessions.load(Ordering::Relaxed);
        let connected = state.connected.load(Ordering::Relaxed);
        let failures = state.consecutive_registration_failures();
        // Sustained failure flips both the status word and the HTTP status line —
        // the prod HEALTHCHECK's `curl -f` treats any >=400 as unhealthy.
        let (status_line, body) = if is_unhealthy(failures) {
            let reason = state.last_registration_error().unwrap_or_default();
            let reason_json = serde_json::to_string(&reason).unwrap_or_else(|_| "\"\"".to_string());
            (
                "HTTP/1.1 503 Service Unavailable",
                format!(
                    "{{\"status\":\"unhealthy\",\"sessions\":{sessions},\"connected\":{connected},\
                     \"consecutive_registration_failures\":{failures},\"reason\":{reason_json}}}"
                ),
            )
        } else {
            (
                "HTTP/1.1 200 OK",
                format!("{{\"status\":\"ok\",\"sessions\":{sessions},\"connected\":{connected}}}"),
            )
        };
        format!(
            "{status_line}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        )
    } else {
        let body = "not found";
        format!(
            "HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        )
    };
    let _ = stream.write_all(response.as_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::TcpStream;

    /// Ephemeral port, so tests are deterministic and parallel-safe. Runs the
    /// real accept loop so timeouts and the in-flight cap are under test.
    fn spawn_test_server(state: Arc<HealthState>) -> String {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind ephemeral port");
        let addr = listener.local_addr().expect("local_addr").to_string();
        std::thread::spawn(move || serve_listener(listener, state));
        std::thread::sleep(std::time::Duration::from_millis(50));
        addr
    }

    fn get(addr: &str, path: &str) -> (String, String) {
        let mut stream = TcpStream::connect(addr).expect("connect");
        let req = format!("GET {path} HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n");
        stream.write_all(req.as_bytes()).expect("write request");
        let mut resp = String::new();
        stream.read_to_string(&mut resp).expect("read response");
        let mut parts = resp.splitn(2, "\r\n\r\n");
        let head = parts.next().unwrap_or_default().to_string();
        let body = parts.next().unwrap_or_default().to_string();
        (head, body)
    }

    #[test]
    fn health_reports_ok_shape_and_defaults() {
        let state = HealthState::new();
        let addr = spawn_test_server(state);
        let (head, body) = get(&addr, "/health");
        assert!(head.starts_with("HTTP/1.1 200"), "head: {head}");
        assert_eq!(body, r#"{"status":"ok","sessions":0,"connected":false}"#);
    }

    #[test]
    fn health_reflects_shared_state() {
        let state = HealthState::new();
        state.set_sessions(3);
        state.set_connected(true);
        let addr = spawn_test_server(state);
        let (_head, body) = get(&addr, "/health");
        assert_eq!(body, r#"{"status":"ok","sessions":3,"connected":true}"#);
    }

    #[test]
    fn unknown_path_is_404() {
        let state = HealthState::new();
        let addr = spawn_test_server(state);
        let (head, _body) = get(&addr, "/nope");
        assert!(head.starts_with("HTTP/1.1 404"), "head: {head}");
    }

    // --- #519: sustained registration failure flips health unhealthy ---

    #[test]
    fn is_unhealthy_threshold_is_exact() {
        assert!(!is_unhealthy(0));
        assert!(!is_unhealthy(UNHEALTHY_AFTER_CONSECUTIVE_FAILURES - 1));
        assert!(is_unhealthy(UNHEALTHY_AFTER_CONSECUTIVE_FAILURES));
        assert!(is_unhealthy(UNHEALTHY_AFTER_CONSECUTIVE_FAILURES + 1));
    }

    #[test]
    fn record_registration_failure_increments_and_returns_running_count() {
        let state = HealthState::default();
        assert_eq!(state.record_registration_failure("boom 1"), 1);
        assert_eq!(state.record_registration_failure("boom 2"), 2);
        assert_eq!(state.consecutive_registration_failures(), 2);
        assert_eq!(state.last_registration_error().as_deref(), Some("boom 2"));
    }

    #[test]
    fn record_registered_resets_the_failure_streak() {
        let state = HealthState::default();
        for _ in 0..UNHEALTHY_AFTER_CONSECUTIVE_FAILURES {
            state.record_registration_failure("control plane rejected register: unauthorized");
        }
        assert!(is_unhealthy(state.consecutive_registration_failures()));

        state.record_registered();
        assert_eq!(state.consecutive_registration_failures(), 0);
        assert_eq!(state.last_registration_error(), None);
        assert!(!is_unhealthy(state.consecutive_registration_failures()));
    }

    /// Past the threshold, /health returns non-2xx with status:"unhealthy" and
    /// the rejection reason in the body.
    #[test]
    fn health_endpoint_flips_unhealthy_after_sustained_registration_failure() {
        let state = HealthState::new();
        for _ in 0..UNHEALTHY_AFTER_CONSECUTIVE_FAILURES {
            state.record_registration_failure(
                "control plane rejected register: unauthorized: bad enrollment token",
            );
        }
        let addr = spawn_test_server(state);
        let (head, body) = get(&addr, "/health");
        assert!(head.starts_with("HTTP/1.1 503"), "head: {head}");
        assert!(body.contains(r#""status":"unhealthy""#), "body: {body}");
        assert!(
            body.contains("bad enrollment token"),
            "reason missing from body: {body}"
        );
    }

    /// Below the threshold must stay healthy (transient reconnect case).
    #[test]
    fn health_endpoint_stays_ok_below_the_failure_threshold() {
        let state = HealthState::new();
        for _ in 0..(UNHEALTHY_AFTER_CONSECUTIVE_FAILURES - 1) {
            state.record_registration_failure("connection refused");
        }
        let addr = spawn_test_server(state);
        let (head, body) = get(&addr, "/health");
        assert!(head.starts_with("HTTP/1.1 200"), "head: {head}");
        assert!(body.contains(r#""status":"ok""#), "body: {body}");
    }

    /// A silent prober must not park its handler thread forever.
    #[test]
    fn silent_connection_is_closed_by_the_read_timeout() {
        let addr = spawn_test_server(HealthState::new());
        let mut idle = TcpStream::connect(&addr).expect("connect");
        // Bound the client side too so a regression fails the test, not the suite.
        idle.set_read_timeout(Some(IO_TIMEOUT * 4)).unwrap();
        let started = std::time::Instant::now();
        let mut sink = Vec::new();
        let read = idle.read_to_end(&mut sink);
        assert!(
            read.is_ok(),
            "server never closed the idle connection: {read:?}"
        );
        assert!(sink.is_empty(), "idle connection got a body: {sink:?}");
        assert!(
            started.elapsed() < IO_TIMEOUT * 3,
            "close took {:?}, well past the {IO_TIMEOUT:?} read deadline",
            started.elapsed()
        );
    }

    /// Beyond the in-flight cap the listener sheds immediately with 503.
    #[test]
    fn connections_beyond_the_cap_are_shed_with_503() {
        let addr = spawn_test_server(HealthState::new());
        let mut held = Vec::new();
        for _ in 0..MAX_INFLIGHT {
            held.push(TcpStream::connect(&addr).expect("connect"));
        }
        std::thread::sleep(std::time::Duration::from_millis(200));

        let mut extra = TcpStream::connect(&addr).expect("connect over cap");
        extra.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
        let req = "GET /health HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n";
        let _ = extra.write_all(req.as_bytes());
        let mut resp = String::new();
        extra.read_to_string(&mut resp).expect("read shed response");
        assert!(
            resp.starts_with("HTTP/1.1 503"),
            "expected a 503 beyond the cap of {MAX_INFLIGHT}, got: {resp}"
        );
        drop(held);
    }

    #[test]
    fn addr_from_env_disabled_variants() {
        std::env::set_var("QUASAR_HEALTH_ADDR", "");
        assert_eq!(addr_from_env(), None);
        std::env::set_var("QUASAR_HEALTH_ADDR", "0");
        assert_eq!(addr_from_env(), None);
        std::env::set_var("QUASAR_HEALTH_ADDR", "127.0.0.1:9999");
        assert_eq!(addr_from_env(), Some("127.0.0.1:9999".to_string()));
        std::env::remove_var("QUASAR_HEALTH_ADDR");
    }
}
