//! The agent socket and the control socket (D6(a), #352 decision 8): unix HTTP sockets the
//! actor creates in the `quasar-recovery-agent` volume. Each is given only to the one
//! container it is for (the agent socket to the node agent, the control socket to the
//! control plane), so the socket a request arrives on is its caller. Not a frozen
//! interface. Both serve `GET /v1/status[?request_id=<uuid>]`, whose `result` is only ever an
//! attempt submitted on the same socket, and `POST /v1/submit` (a `socket::Request`).
//!
//! Answers are `HTTP/1.1` with `Content-Length` and `Connection: close`, and the
//! connection is closed after one response, so an HTTP/1.0 client reading to EOF (the
//! agent's `release::unix_http`) and an HTTP/1.1 client both work.

use std::io::{self, Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tracing::{debug, warn};

use crate::actor::Actor;
use crate::socket::{Reason, Rejection, Request};
use crate::trust::Caller;

const MAX_HEAD: usize = 8 * 1024;
const MAX_BODY: usize = 64 * 1024;
const MAX_CONNECTIONS: usize = 8;
const IO_TIMEOUT: Duration = Duration::from_secs(10);

/// Bind the socket, replacing a stale one. Only the lease holder may call this: the lease
/// is what makes a leftover socket file certainly stale.
pub fn bind(path: &Path) -> io::Result<UnixListener> {
    bind_owned(path, None)
}

/// [`bind`], the socket owned by `owner` (uid, gid) so a container running as that user can
/// connect: the control plane does not run as root. Its directory is created if need be.
pub fn bind_owned(path: &Path, owner: Option<(u32, u32)>) -> io::Result<UnixListener> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    match std::fs::remove_file(path) {
        Ok(()) => {}
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }
    let listener = UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    if let Some((uid, gid)) = owner {
        std::os::unix::fs::chown(path, Some(uid), Some(gid))?;
    }
    Ok(listener)
}

/// How often a serving loop looks at its stop flag between connections.
const ACCEPT_POLL: Duration = Duration::from_millis(20);

/// Serve `caller`'s socket until `stop` is set (`Ok`) or the listener fails. One thread per
/// connection, at most [`MAX_CONNECTIONS`] at once; a connection over the limit is closed
/// unanswered. A hand-over stops the old actor's loops before it releases the lease, so the
/// successor binds paths nobody else serves.
pub fn serve(
    listener: UnixListener,
    actor: Arc<Actor>,
    caller: Caller,
    stop: Arc<AtomicBool>,
) -> io::Result<()> {
    listener.set_nonblocking(true)?;
    let open = Arc::new(AtomicUsize::new(0));
    loop {
        if stop.load(Ordering::SeqCst) {
            return Ok(());
        }
        let stream = match listener.accept() {
            Ok((stream, _)) => stream,
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                std::thread::sleep(ACCEPT_POLL);
                continue;
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        stream.set_nonblocking(false)?;
        if open.fetch_add(1, Ordering::SeqCst) >= MAX_CONNECTIONS {
            open.fetch_sub(1, Ordering::SeqCst);
            warn!(
                token = "actor-socket-busy",
                socket = socket_name(caller),
                "too many connections; one closed unanswered"
            );
            continue;
        }
        let actor = actor.clone();
        let open = open.clone();
        std::thread::spawn(move || {
            if let Err(e) = answer(stream, &actor, caller) {
                debug!(socket = socket_name(caller), "{e}");
            }
            open.fetch_sub(1, Ordering::SeqCst);
        });
    }
}

fn socket_name(caller: Caller) -> &'static str {
    match caller {
        Caller::Agent => "agent",
        Caller::ControlPlane => "control",
    }
}

fn answer(mut stream: UnixStream, actor: &Arc<Actor>, caller: Caller) -> io::Result<()> {
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;
    let mut head = Vec::new();
    let mut byte = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") && !head.ends_with(b"\n\n") {
        if stream.read(&mut byte)? == 0 {
            break;
        }
        head.push(byte[0]);
        if head.len() > MAX_HEAD {
            return respond(&mut stream, 431, r#"{"error":"request_too_large"}"#);
        }
    }
    let head = String::from_utf8_lossy(&head);
    let own_probe = head.lines().any(|l| {
        l.split_once(':')
            .is_some_and(|(k, _)| k.trim().eq_ignore_ascii_case(SELF_PROBE_HEADER))
    });
    let mut first = head.lines().next().unwrap_or("").split_whitespace();
    let (method, target) = (first.next().unwrap_or(""), first.next().unwrap_or(""));
    let (path, query) = target.split_once('?').unwrap_or((target, ""));
    match (method, path) {
        ("GET", "/v1/status") => {
            let request_id = query
                .split('&')
                .find_map(|kv| kv.strip_prefix("request_id="));
            if let Some(id) = request_id.filter(|_| !own_probe) {
                actor.note_attempt_poll(id);
            }
            let body = serde_json::to_string(&actor.status_as(caller, request_id))
                .map_err(io::Error::other)?;
            respond(&mut stream, 200, &body)
        }
        ("POST", "/v1/submit") => submit(&mut stream, &head, actor, caller),
        (_, "/v1/status") | (_, "/v1/submit") => {
            respond(&mut stream, 405, r#"{"error":"method_not_allowed"}"#)
        }
        _ => respond(&mut stream, 404, r#"{"error":"not_found"}"#),
    }
}

/// `POST /v1/submit`: the caller is the socket's, because authority follows the mount.
/// `202` with the `Accepted`, `409` (`busy`) or `400` with the `Rejection`.
fn submit(
    stream: &mut UnixStream,
    head: &str,
    actor: &Arc<Actor>,
    caller: Caller,
) -> io::Result<()> {
    let length = head
        .lines()
        .find_map(|l| {
            let (k, v) = l.split_once(':')?;
            k.trim()
                .eq_ignore_ascii_case("content-length")
                .then(|| v.trim().parse::<usize>().ok())?
        })
        .unwrap_or(0);
    if length > MAX_BODY {
        return respond(stream, 413, r#"{"error":"request_too_large"}"#);
    }
    let mut body = vec![0u8; length];
    stream.read_exact(&mut body)?;
    let request: Request = match serde_json::from_slice(&body) {
        Ok(r) => r,
        Err(e) => {
            let rejection = Rejection {
                request_id: String::new(),
                reason: Reason::Invalid,
                message: format!("the request is not a submit this actor reads: {e}"),
            };
            let body = serde_json::to_string(&rejection).map_err(io::Error::other)?;
            return respond(stream, 400, &body);
        }
    };
    match actor.submit(caller, request) {
        Ok(accepted) => {
            let body = serde_json::to_string(&accepted).map_err(io::Error::other)?;
            respond(stream, 202, &body)
        }
        Err(rejection) => {
            warn!(
                token = "actor-submit-refused",
                reason = %rejection.reason,
                socket = socket_name(caller),
                "a submit was refused: {}", rejection.message
            );
            let status = if rejection.reason == Reason::Busy {
                409
            } else {
                400
            };
            let body = serde_json::to_string(&rejection).map_err(io::Error::other)?;
            respond(stream, status, &body)
        }
    }
}

fn respond(stream: &mut UnixStream, status: u16, body: &str) -> io::Result<()> {
    let reason = match status {
        200 => "OK",
        202 => "Accepted",
        400 => "Bad Request",
        404 => "Not Found",
        405 => "Method Not Allowed",
        409 => "Conflict",
        413 => "Payload Too Large",
        _ => "Request Header Fields Too Large",
    };
    write!(
        stream,
        "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )?;
    stream.flush()
}

/// One `GET /v1/status`, the body as served: the operator's `quasar-recovery status`, the
/// image's healthcheck, and an actor probing its own sockets. Always marked, so it is never
/// taken for the node agent (`crate::handover`, agent contact).
pub fn fetch_status(path: &Path) -> io::Result<String> {
    status_request(path, "/v1/status", &format!("{SELF_PROBE_HEADER}: 1\r\n"))
}

/// Marks a request made by this binary rather than by the node agent.
const SELF_PROBE_HEADER: &str = "X-Quasar-Self-Probe";

/// The node agent relay's poll of one attempt, as its own client sends it (unmarked). For
/// tests standing in for the agent.
#[cfg(any(test, feature = "test-support"))]
pub fn poll_attempt_as_agent(path: &Path, request_id: &str) -> io::Result<String> {
    status_request(path, &format!("/v1/status?request_id={request_id}"), "")
}

fn status_request(path: &Path, target: &str, extra_header: &str) -> io::Result<String> {
    let mut stream = UnixStream::connect(path)?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    let request = format!("GET {target} HTTP/1.0\r\nHost: recovery\r\n{extra_header}\r\n");
    stream.write_all(request.as_bytes())?;
    let mut raw = String::new();
    stream.take(4 * 1024 * 1024).read_to_string(&mut raw)?;
    let (head, body) = raw.split_once("\r\n\r\n").unwrap_or((raw.as_str(), ""));
    if !head.starts_with("HTTP/1.1 200") {
        return Err(io::Error::other(format!(
            "the actor answered {}",
            head.lines().next().unwrap_or("nothing")
        )));
    }
    Ok(body.to_owned())
}
