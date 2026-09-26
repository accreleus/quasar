//! The agent socket serves exactly what `Actor::status` answers, in the shape the socket
//! fixtures pin, to both an HTTP/1.0 read-to-EOF client (the agent's) and the operator CLI.

mod support;

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::sync::Arc;

use quasar_recovery::engine::FakeEngine;
use quasar_recovery::server;
use quasar_recovery::socket::Status;
use support::*;

fn raw(path: &std::path::Path, request: &str) -> String {
    let mut stream = UnixStream::connect(path).unwrap();
    stream.write_all(request.as_bytes()).unwrap();
    let mut out = String::new();
    stream.read_to_string(&mut out).unwrap();
    out
}

#[test]
fn the_agent_socket_serves_status_and_nothing_else() {
    let engine = Arc::new(FakeEngine::new(amd_host()));
    let machine = tempfile::tempdir().unwrap();
    let actor = Arc::new(actor(&engine, machine.path(), operator()));
    actor.resume().unwrap();

    let sockets = tempfile::tempdir().unwrap();
    let path = sockets.path().join("agent.sock");
    let listener = server::bind(&path).unwrap();
    let serving = actor.clone();
    let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
    std::thread::spawn(move || {
        server::serve(
            listener,
            serving,
            quasar_recovery::trust::Caller::Agent,
            stop,
        )
    });

    let body = server::fetch_status(&path).unwrap();
    let served: Status = serde_json::from_str(&body).unwrap();
    assert_eq!(served, actor.status());

    let reply = raw(&path, "GET /v1/status HTTP/1.0\r\nHost: updater\r\n\r\n");
    assert!(reply.starts_with("HTTP/1.1 200 OK\r\n"), "{reply}");
    let (_, json) = reply.split_once("\r\n\r\n").unwrap();
    assert_eq!(serde_json::from_str::<Status>(json).unwrap(), served);

    let reply = raw(&path, "POST /v1/status HTTP/1.0\r\n\r\n");
    assert!(reply.starts_with("HTTP/1.1 405"), "{reply}");
    let reply = raw(&path, "GET /v1/apply HTTP/1.0\r\n\r\n");
    assert!(reply.starts_with("HTTP/1.1 404"), "{reply}");
}

#[test]
fn binding_replaces_a_stale_socket_file_and_leaves_it_owner_only() {
    use std::os::unix::fs::PermissionsExt;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("agent.sock");
    drop(server::bind(&path).unwrap());
    let _again = server::bind(&path).expect("a stale socket file is replaced");
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600);
}
