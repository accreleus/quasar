//! `host_remove` (agent-api.md, amendment 14): hand the console's "remove host" to this
//! host's recovery actor as a `kind: remove` request on the agent socket, and ack what it
//! answered. The actor then removes this agent and itself, so nothing follows the ack.
//!
//! The agent does no session logic: draining is the control plane's, done before it sends
//! the command.

use std::path::Path;
use std::time::Duration;

use tracing::{info, warn};

use crate::messages::AgentMsg;

/// The actor answers once the removal is recorded; well inside the control plane's 10 s
/// ack timeout.
const SUBMIT_TIMEOUT: Duration = Duration::from_secs(8);

#[derive(serde::Deserialize)]
struct Rejection {
    #[serde(default)]
    reason: String,
    #[serde(default)]
    message: String,
}

fn ack(id: String, error: Option<&str>) -> AgentMsg {
    AgentMsg::Ack {
        id,
        ok: error.is_none(),
        error: error.map(str::to_owned),
    }
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

/// Blocking: one local unix round trip, bounded by [`SUBMIT_TIMEOUT`].
pub fn handle(id: String, request_id: &str) -> AgentMsg {
    match crate::buildinfo::owned_socket() {
        Some(socket) => handle_on(id, request_id, &socket),
        None => {
            warn!(
                token = "host-remove-not-owned",
                "host_remove {request_id} refused: this host is not an owned install"
            );
            ack(id, Some("invalid"))
        }
    }
}

pub fn handle_on(id: String, request_id: &str, socket: &Path) -> AgentMsg {
    if !is_uuid(request_id) {
        return ack(id, Some("invalid"));
    }
    if !socket.exists() {
        warn!(
            token = "host-remove-no-actor",
            "host_remove {request_id}: no recovery actor socket at {}",
            socket.display()
        );
        return ack(id, Some("updater_absent"));
    }
    let body = serde_json::json!({
        "request_id": request_id,
        "kind": "remove",
        "components": [],
        "release": { "id": "", "version": null, "source_commit": "" },
        "migrates": false,
        "schema_version": null,
        "external_backup_confirmed": false,
        "dump": null,
        "purge": false,
    })
    .to_string();
    match crate::release::unix_http::request(
        socket,
        "POST",
        "/v1/submit",
        Some(&body),
        SUBMIT_TIMEOUT,
    ) {
        Err(e) => {
            warn!(
                token = "host-remove-actor-unreachable",
                "host_remove {request_id}: the recovery actor did not answer: {e}"
            );
            ack(id, Some("updater_unreachable"))
        }
        Ok(r) if r.status == 202 => {
            info!(token = "host-remove-accepted", "host_remove {request_id}: the recovery actor accepted; it removes this agent, then itself");
            ack(id, None)
        }
        Ok(r) => {
            let rejection = serde_json::from_str::<Rejection>(&r.body).ok();
            let reason = rejection
                .as_ref()
                .map(|j| j.reason.clone())
                .filter(|s| !s.is_empty())
                .unwrap_or_else(|| "invalid".into());
            warn!(
                token = "host-remove-refused",
                "host_remove {request_id} refused by the recovery actor ({}): {reason}: {}",
                r.status,
                rejection.map(|j| j.message).unwrap_or_default()
            );
            ack(id, Some(&reason))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::os::unix::net::UnixListener;

    const ID: &str = "3c0a6f2e-8d1b-4f7e-9a55-2b8e1c0d9f41";

    /// An actor on a real socket that answers one request with `reply`, and hands back the
    /// body it was sent.
    fn actor(
        dir: &Path,
        reply: &'static str,
    ) -> (std::path::PathBuf, std::thread::JoinHandle<String>) {
        let path = dir.join("agent.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let handle = std::thread::spawn(move || {
            let (mut s, _) = listener.accept().unwrap();
            let mut got = Vec::new();
            let mut buf = [0u8; 4096];
            // The head, then as much body as it announces.
            loop {
                let text = String::from_utf8_lossy(&got).into_owned();
                if let Some((head, body)) = text.split_once("\r\n\r\n") {
                    let want = head
                        .lines()
                        .find_map(|l| l.strip_prefix("Content-Length: "))
                        .and_then(|n| n.trim().parse::<usize>().ok())
                        .unwrap_or(0);
                    if body.len() >= want {
                        break;
                    }
                }
                let n = s.read(&mut buf).unwrap();
                if n == 0 {
                    break;
                }
                got.extend_from_slice(&buf[..n]);
            }
            s.write_all(reply.as_bytes()).unwrap();
            String::from_utf8_lossy(&got).into_owned()
        });
        (path, handle)
    }

    fn ok_of(msg: &AgentMsg) -> (bool, Option<String>) {
        match msg {
            AgentMsg::Ack { ok, error, .. } => (*ok, error.clone()),
            other => panic!("not an ack: {other:?}"),
        }
    }

    #[test]
    fn an_accepted_removal_acks_ok_and_asks_for_a_remove_with_no_components() {
        let dir = tempfile::tempdir().unwrap();
        let (path, sent) = actor(
            dir.path(),
            "HTTP/1.1 202 Accepted\r\nContent-Length: 2\r\n\r\n{}",
        );
        assert_eq!(ok_of(&handle_on("c1".into(), ID, &path)), (true, None));
        let sent = sent.join().unwrap();
        assert!(sent.starts_with("POST /v1/submit"), "{sent}");
        assert!(sent.contains(r#""kind":"remove""#), "{sent}");
        assert!(sent.contains(r#""components":[]"#), "{sent}");
        assert!(sent.contains(r#""purge":false"#), "{sent}");
    }

    #[test]
    fn a_refusal_acks_its_reason() {
        let dir = tempfile::tempdir().unwrap();
        let (path, _) = actor(
            dir.path(),
            "HTTP/1.1 409 Conflict\r\nContent-Length: 45\r\n\r\n{\"reason\":\"busy\",\"message\":\"in flight\"}   ",
        );
        assert_eq!(
            ok_of(&handle_on("c1".into(), ID, &path)),
            (false, Some("busy".into()))
        );
    }

    #[test]
    fn the_wire_command_decodes() {
        let msg: crate::messages::ControlMsg = serde_json::from_str(&format!(
            r#"{{"type":"host_remove","id":"c1","request_id":"{ID}"}}"#
        ))
        .unwrap();
        assert!(matches!(
            msg,
            crate::messages::ControlMsg::HostRemove { ref id, ref request_id } if id == "c1" && request_id == ID
        ));
    }

    #[test]
    fn no_socket_is_updater_absent_and_a_bad_id_is_invalid() {
        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("none.sock");
        assert_eq!(
            ok_of(&handle_on("c1".into(), ID, &missing)),
            (false, Some("updater_absent".into()))
        );
        assert_eq!(
            ok_of(&handle_on("c1".into(), "not-a-uuid", &missing)),
            (false, Some("invalid".into()))
        );
    }
}
