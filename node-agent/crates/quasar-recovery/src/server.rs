//! The agent socket (D6(a), #352 decision 8): a unix HTTP socket the actor creates in the
//! `quasar-recovery-agent` volume, which it mounts only into the agent container it
//! creates. Not a frozen interface. It serves `GET /v1/status[?request_id=<uuid>]` and
//! `POST /v1/submit` (a `socket::Request`; the caller is the agent).
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
    match std::fs::remove_file(path) {
        Ok(()) => {}
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }
    let listener = UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    Ok(listener)
}

/// How often a serving loop looks at its stop flag between connections.
const ACCEPT_POLL: Duration = Duration::from_millis(20);

/// Serve until `stop` is set (`Ok`) or the listener fails. One thread per connection, at
/// most [`MAX_CONNECTIONS`] at once; a connection over the limit is closed unanswered.
/// A hand-over stops the old actor's loop before it releases the lease, so the successor
/// binds a path nobody else serves.
pub fn serve(listener: UnixListener, actor: Arc<Actor>, stop: Arc<AtomicBool>) -> io::Result<()> {
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
                "agent socket: too many connections; one closed unanswered"
            );
            continue;
        }
        let actor = actor.clone();
        let open = open.clone();
        std::thread::spawn(move || {
            if let Err(e) = answer(stream, &actor) {
                debug!("agent socket: {e}");
            }
            open.fetch_sub(1, Ordering::SeqCst);
        });
    }
}

fn answer(mut stream: UnixStream, actor: &Arc<Actor>) -> io::Result<()> {
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
    let mut first = head.lines().next().unwrap_or("").split_whitespace();
    let (method, target) = (first.next().unwrap_or(""), first.next().unwrap_or(""));
    let (path, query) = target.split_once('?').unwrap_or((target, ""));
    match (method, path) {
        ("GET", "/v1/status") => {
            // TODO(#363): scope `result` by caller once the control socket exists, so the
            // agent socket answers only for attempts the agent submitted.
            let request_id = query
                .split('&')
                .find_map(|kv| kv.strip_prefix("request_id="));
            let body =
                serde_json::to_string(&actor.status_for(request_id)).map_err(io::Error::other)?;
            respond(&mut stream, 200, &body)
        }
        ("POST", "/v1/submit") => submit(&mut stream, &head, actor),
        (_, "/v1/status") | (_, "/v1/submit") => {
            respond(&mut stream, 405, r#"{"error":"method_not_allowed"}"#)
        }
        _ => respond(&mut stream, 404, r#"{"error":"not_found"}"#),
    }
}

/// `POST /v1/submit` on the agent socket: the caller is always [`Caller::Agent`], because
/// authority follows the mount (this socket is mounted only into the node agent).
/// `202` with the `Accepted`, `409` (`busy`) or `400` with the `Rejection`.
fn submit(stream: &mut UnixStream, head: &str, actor: &Arc<Actor>) -> io::Result<()> {
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
    match actor.submit(Caller::Agent, request) {
        Ok(accepted) => {
            let body = serde_json::to_string(&accepted).map_err(io::Error::other)?;
            respond(stream, 202, &body)
        }
        Err(rejection) => {
            warn!(
                token = "actor-submit-refused",
                reason = %rejection.reason,
                "a submit on the agent socket was refused: {}", rejection.message
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

/// The operator's `quasar-recovery status`: one `GET /v1/status`, the body as served.
pub fn fetch_status(path: &Path) -> io::Result<String> {
    let mut stream = UnixStream::connect(path)?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.write_all(b"GET /v1/status HTTP/1.0\r\nHost: recovery\r\n\r\n")?;
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
