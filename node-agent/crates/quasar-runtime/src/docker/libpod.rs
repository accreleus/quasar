//! The two Podman-native endpoints the platform stop needs (#425): the libpod inspect, which
//! says whether Podman recorded a stop and whether a restart is pending, and `init`. The
//! Docker-compatible API carries neither, and Bollard speaks only that API, so these are
//! plain HTTP/1.0 requests on the engine's own socket (1.0: Podman's Go server then answers
//! without chunked encoding and closes, so the body is everything up to EOF).
//!
//! Nothing here returns daemon text to a caller.
use crate::{ErrorKind, RuntimeConfig, RuntimeError};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

/// The libpod API version every Podman this runtime supports answers (the agent's own
/// Podman read-backs use it too).
const LIBPOD: &str = "/v4.0.0/libpod";
/// An inspect carries the whole container configuration; anything past this is not one.
const MAX_RESPONSE: u64 = 1024 * 1024;

pub(crate) struct Response {
    pub status: u16,
    pub body: Vec<u8>,
}

/// One request; `Err` only when no answer came (refused connection, deadline, unreadable
/// reply): the caller decides what an unanswered request means.
async fn request(
    config: &RuntimeConfig,
    method: &str,
    path: &str,
) -> Result<Response, RuntimeError> {
    let exchange = async {
        let mut stream = tokio::net::UnixStream::connect(&config.socket).await?;
        let head = format!(
            "{method} {LIBPOD}{path} HTTP/1.0\r\nHost: podman\r\nContent-Length: 0\r\n\r\n"
        );
        stream.write_all(head.as_bytes()).await?;
        stream.flush().await?;
        let mut raw = Vec::new();
        stream.take(MAX_RESPONSE).read_to_end(&mut raw).await?;
        Ok::<_, std::io::Error>(raw)
    };
    let raw = tokio::time::timeout(config.deadline, exchange)
        .await
        .map_err(|_| RuntimeError::from(ErrorKind::Timeout))?
        .map_err(|e| super::classify(e.into()))?;
    let split = raw
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .ok_or(ErrorKind::Protocol)?;
    let status = std::str::from_utf8(&raw[..split])
        .ok()
        .and_then(|head| head.lines().next())
        .and_then(|line| line.split_whitespace().nth(1))
        .and_then(|code| code.parse::<u16>().ok())
        .ok_or(ErrorKind::Protocol)?;
    Ok(Response {
        status,
        body: raw[split + 4..].to_vec(),
    })
}

/// `GET /libpod/containers/{id}/json`. `Ok(None)`: the container is gone.
pub(crate) async fn inspect(
    config: &RuntimeConfig,
    id: &str,
) -> Result<Option<serde_json::Value>, RuntimeError> {
    let response = request(config, "GET", &format!("/containers/{id}/json")).await?;
    match response.status {
        200 => serde_json::from_slice(&response.body)
            .map(Some)
            .map_err(|_| ErrorKind::Protocol.into()),
        404 => Ok(None),
        _ => Err(ErrorKind::Engine.into()),
    }
}

/// `POST /libpod/containers/{id}/init`: create the container in the OCI runtime without
/// running its program. `Ok(true)`: it is now `created`. `Ok(false)`: the engine refused,
/// because the container is no longer where `init` applies (it was started meanwhile, or
/// is gone).
pub(crate) async fn init(config: &RuntimeConfig, id: &str) -> Result<bool, RuntimeError> {
    let response = request(config, "POST", &format!("/containers/{id}/init")).await?;
    Ok(matches!(response.status, 200..=299))
}
