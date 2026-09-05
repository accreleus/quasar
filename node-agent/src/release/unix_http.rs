//! Minimal HTTP/1.0 over a unix socket. Hand-rolled rather than a new
//! dependency: `ureq` has no unix-socket transport and the surface here is two
//! verbs against a local socket with no TLS, redirects or auth.
//!
//! HTTP/1.0 in the request line, not 1.1: Go's server then answers without
//! chunked transfer-encoding and closes, so the body is everything up to EOF
//! and there is no de-chunker to get wrong.

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::time::Duration;

/// A result file carries at most an 8 KiB output tail, so anything past this
/// is not one.
const MAX_BODY: usize = 256 * 1024;

pub struct Response {
    pub status: u16,
    pub body: String,
}

pub fn request(
    socket: &Path,
    method: &str,
    path: &str,
    body: Option<&str>,
    timeout: Duration,
) -> std::io::Result<Response> {
    let mut stream = UnixStream::connect(socket)?;
    stream.set_read_timeout(Some(timeout))?;
    stream.set_write_timeout(Some(timeout))?;

    let mut req = format!("{method} {path} HTTP/1.0\r\nHost: updater\r\n");
    if let Some(b) = body {
        req.push_str("Content-Type: application/json\r\n");
        req.push_str(&format!("Content-Length: {}\r\n", b.len()));
    }
    req.push_str("\r\n");
    stream.write_all(req.as_bytes())?;
    if let Some(b) = body {
        stream.write_all(b.as_bytes())?;
    }
    stream.flush()?;

    let mut raw = Vec::new();
    stream.take(MAX_BODY as u64).read_to_end(&mut raw)?;
    let text = String::from_utf8_lossy(&raw).into_owned();

    let (head, body) = match text.find("\r\n\r\n") {
        Some(i) => (&text[..i], text[i + 4..].to_string()),
        None => (text.as_str(), String::new()),
    };
    let status = head
        .lines()
        .next()
        .and_then(|l| l.split_whitespace().nth(1))
        .and_then(|s| s.parse::<u16>().ok())
        .ok_or_else(|| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                format!("no status line in the updater's reply: {head:?}"),
            )
        })?;
    Ok(Response { status, body })
}
