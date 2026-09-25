//! The agent socket (D6(a), #352 decision 8): a unix HTTP socket the actor creates in the
//! `quasar-recovery-agent` volume, which it mounts only into the agent container it
//! creates. Not a frozen interface. In this build it serves `GET /v1/status` only.
//!
//! Answers are `HTTP/1.1` with `Content-Length` and `Connection: close`, and the
//! connection is closed after one response, so an HTTP/1.0 client reading to EOF (the
//! agent's `release::unix_http`) and an HTTP/1.1 client both work.

use std::io::{self, Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tracing::{debug, warn};

use crate::actor::Actor;

const MAX_HEAD: usize = 8 * 1024;
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

/// Serve until the listener fails. One thread per connection, at most
/// [`MAX_CONNECTIONS`] at once; a connection over the limit is closed unanswered.
pub fn serve(listener: UnixListener, actor: Arc<Actor>) -> io::Error {
    let open = Arc::new(AtomicUsize::new(0));
    loop {
        let stream = match listener.accept() {
            Ok((stream, _)) => stream,
            Err(e) => return e,
        };
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

fn answer(mut stream: UnixStream, actor: &Actor) -> io::Result<()> {
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
    let path = target.split('?').next().unwrap_or("");
    match (method, path) {
        ("GET", "/v1/status") => {
            let body = serde_json::to_string(&actor.status()).map_err(io::Error::other)?;
            respond(&mut stream, 200, &body)
        }
        (_, "/v1/status") => respond(&mut stream, 405, r#"{"error":"method_not_allowed"}"#),
        _ => respond(&mut stream, 404, r#"{"error":"not_found"}"#),
    }
}

fn respond(stream: &mut UnixStream, status: u16, body: &str) -> io::Result<()> {
    let reason = match status {
        200 => "OK",
        404 => "Not Found",
        405 => "Method Not Allowed",
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
