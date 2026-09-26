//! The operator socket: how `quasar-recovery reconfigure`, run inside the recovery actor's
//! container (`docker exec`), reaches the running actor that holds the machine's lease.
//!
//! It lives on the actor container's own filesystem, never in a volume, so no other
//! container can reach it: only someone who can `docker exec` into the actor, which is
//! the engine's root. That is why it may change machine inputs, which neither the agent
//! socket nor the control socket can. Not a frozen interface.
//!
//! `POST /v1/reconfigure` (a [`ReconfigureRequest`]) answers `200` with a [`Planned`] when
//! nothing needed re-creating (or for a dry run), `202` with one naming the attempt,
//! `409`/`400` with a `Rejection`. `POST /v1/restore` (a `socket::Request` of kind
//! `restore`, `crate::restore`) answers `202` with an `Accepted`, `409`/`400` with a
//! `Rejection`. `GET /v1/status?request_id=` is the actor's status.

use std::io::{self, Read, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use tracing::{debug, warn};

use crate::actor::Actor;
use crate::reconfigure::{Planned, ReconfigureRequest};
use crate::socket::{Reason, Rejection, Request};

pub const SOCKET: &str = "/run/quasar-operator/operator.sock";

const MAX_HEAD: usize = 8 * 1024;
const MAX_BODY: usize = 64 * 1024;
const IO_TIMEOUT: Duration = Duration::from_secs(10);

/// Serve until the listener fails, one connection at a time: an operator types one command.
pub fn serve(listener: UnixListener, actor: Arc<Actor>) -> io::Error {
    loop {
        match listener.accept() {
            Ok((stream, _)) => {
                if let Err(e) = answer(stream, &actor) {
                    debug!(socket = "operator", "{e}");
                }
            }
            Err(e) => return e,
        }
    }
}

fn answer(mut stream: UnixStream, actor: &Arc<Actor>) -> io::Result<()> {
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;
    let mut head = Vec::new();
    let mut byte = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") {
        if stream.read(&mut byte)? == 0 {
            break;
        }
        head.push(byte[0]);
        if head.len() > MAX_HEAD {
            return respond(&mut stream, 431, r#"{"error":"request_too_large"}"#);
        }
    }
    let head = String::from_utf8_lossy(&head).into_owned();
    let mut first = head.lines().next().unwrap_or("").split_whitespace();
    let (method, target) = (first.next().unwrap_or(""), first.next().unwrap_or(""));
    let (path, query) = target.split_once('?').unwrap_or((target, ""));
    match (method, path) {
        ("GET", "/v1/status") => {
            let request_id = query
                .split('&')
                .find_map(|kv| kv.strip_prefix("request_id="));
            let body =
                serde_json::to_string(&actor.status_for(request_id)).map_err(io::Error::other)?;
            respond(&mut stream, 200, &body)
        }
        ("POST", "/v1/restore") => {
            let Some(body) = read_body(&mut stream, &head)? else {
                return respond(&mut stream, 413, r#"{"error":"request_too_large"}"#);
            };
            let answer = match serde_json::from_slice::<Request>(&body) {
                Ok(req) => actor.submit_restore(req),
                Err(e) => Err(Rejection {
                    request_id: String::new(),
                    reason: Reason::Invalid,
                    message: format!("not a restore request: {e}"),
                }),
            };
            match answer {
                Ok(accepted) => {
                    let body = serde_json::to_string(&accepted).map_err(io::Error::other)?;
                    respond(&mut stream, 202, &body)
                }
                Err(rejection) => {
                    warn!(token = "actor-restore-refused", reason = %rejection.reason, "a restore was refused: {}", rejection.message);
                    let status = if rejection.reason == Reason::Busy {
                        409
                    } else {
                        400
                    };
                    let body = serde_json::to_string(&rejection).map_err(io::Error::other)?;
                    respond(&mut stream, status, &body)
                }
            }
        }
        ("POST", "/v1/reconfigure") => {
            let Some(body) = read_body(&mut stream, &head)? else {
                return respond(&mut stream, 413, r#"{"error":"request_too_large"}"#);
            };
            let answer = match serde_json::from_slice::<ReconfigureRequest>(&body) {
                Ok(req) => actor.reconfigure(req),
                Err(e) => Err(Rejection {
                    request_id: String::new(),
                    reason: Reason::Invalid,
                    message: format!("not a reconfigure request: {e}"),
                }),
            };
            match answer {
                Ok(planned) => {
                    let status = if planned.request_id.is_some() {
                        202
                    } else {
                        200
                    };
                    let body = serde_json::to_string(&planned).map_err(io::Error::other)?;
                    respond(&mut stream, status, &body)
                }
                Err(rejection) => {
                    warn!(token = "actor-reconfigure-refused", reason = %rejection.reason, "a reconfigure was refused: {}", rejection.message);
                    let status = if rejection.reason == Reason::Busy {
                        409
                    } else {
                        400
                    };
                    let body = serde_json::to_string(&rejection).map_err(io::Error::other)?;
                    respond(&mut stream, status, &body)
                }
            }
        }
        _ => respond(&mut stream, 404, r#"{"error":"not_found"}"#),
    }
}

/// The request body, by its `Content-Length`; `None` when it is over the limit.
fn read_body(stream: &mut UnixStream, head: &str) -> io::Result<Option<Vec<u8>>> {
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
        return Ok(None);
    }
    let mut body = vec![0u8; length];
    stream.read_exact(&mut body)?;
    Ok(Some(body))
}

fn respond(stream: &mut UnixStream, status: u16, body: &str) -> io::Result<()> {
    let reason = match status {
        200 => "OK",
        202 => "Accepted",
        400 => "Bad Request",
        404 => "Not Found",
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

/// One request to the operator socket: the status code and the body.
pub fn call(
    socket: &Path,
    method: &str,
    path: &str,
    body: Option<&str>,
) -> io::Result<(u16, String)> {
    let mut stream = UnixStream::connect(socket)?;
    // A reconfigure answers once it is admitted, well inside this.
    stream.set_read_timeout(Some(Duration::from_secs(60)))?;
    let mut request = format!("{method} {path} HTTP/1.0\r\nHost: recovery\r\n");
    if let Some(b) = body {
        request.push_str(&format!(
            "Content-Type: application/json\r\nContent-Length: {}\r\n",
            b.len()
        ));
    }
    request.push_str("\r\n");
    stream.write_all(request.as_bytes())?;
    if let Some(b) = body {
        stream.write_all(b.as_bytes())?;
    }
    let mut raw = String::new();
    stream.take(4 * 1024 * 1024).read_to_string(&mut raw)?;
    let (head, body) = raw.split_once("\r\n\r\n").unwrap_or((raw.as_str(), ""));
    let status = head
        .lines()
        .next()
        .and_then(|l| l.split_whitespace().nth(1))
        .and_then(|s| s.parse().ok())
        .ok_or_else(|| io::Error::other("the actor's answer has no status line"))?;
    Ok((status, body.to_owned()))
}

/// The answer to a reconfigure, as the CLI reads it.
pub enum Answer {
    Planned(Planned),
    Refused(Rejection),
}

pub fn reconfigure(socket: &Path, req: &ReconfigureRequest) -> io::Result<Answer> {
    let body = serde_json::to_string(req).map_err(io::Error::other)?;
    let (status, body) = call(socket, "POST", "/v1/reconfigure", Some(&body))?;
    match status {
        200 | 202 => Ok(Answer::Planned(
            serde_json::from_str(&body).map_err(io::Error::other)?,
        )),
        _ => Ok(Answer::Refused(serde_json::from_str(&body).map_err(
            |_| io::Error::other(format!("the actor answered {status}: {body}")),
        )?)),
    }
}
