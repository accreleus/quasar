//! Readiness checks for the update path: the facts preflight reads from this
//! host's own report, under the preflight check ids so the Hosts tab and the
//! Releases tab say the same words about the same thing
//! (control-api.md §"Self-update hardening").
//!
//! Collectors do the I/O once per probe (`ProbeEnv::live`); the checks are pure
//! over what they collected, so every branch is a unit test.

use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::path::Path;
use std::time::Duration;

use serde::Deserialize;

use crate::messages::ReadinessCheck;

pub const CHECK_UPDATER_SOCKET: &str = "updater_socket";
pub const CHECK_UPDATER_STACK_DIR: &str = "updater_stack_dir";
pub const CHECK_UPDATER_OVERLAYS: &str = "updater_overlays";
pub const CHECK_HEALTH_ADDR_BINDABLE: &str = "health_addr_bindable";

/// The compose service this agent runs as, for the overlay comparison.
const AGENT_SERVICE: &str = "quasar-node-agent";

/// Both peers are local (a unix socket, a loopback port). Short, because the
/// collectors run inside the register-prep budget too (#191).
const IO_TIMEOUT: Duration = Duration::from_secs(1);

/// The sliver of the updater's `GET /v1/self` these checks read. Unknown fields
/// are ignored; missing ones default, so an older updater still answers.
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct UpdaterSelf {
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub working_dir: String,
    #[serde(default)]
    pub config_files: Vec<String>,
    /// Per service, the compose files its running container was started with;
    /// absent on an updater that predates the report.
    #[serde(default)]
    pub service_config_files: Option<BTreeMap<String, Option<Vec<String>>>>,
}

/// What one probe learned about the updater beside this agent.
#[derive(Debug, Clone, Default)]
pub struct UpdaterView {
    pub socket_exists: bool,
    /// `None` when there was no socket to ask; `Err` when it did not answer.
    pub self_report: Option<Result<UpdaterSelf, String>>,
}

/// Ask the updater about itself. One local unix round trip, bounded.
pub fn collect_updater(socket: &Path) -> UpdaterView {
    if !socket.exists() {
        return UpdaterView::default();
    }
    let reply = crate::release::unix_http::request(socket, "GET", "/v1/self", None, IO_TIMEOUT);
    let report = match reply {
        Err(e) => Err(e.to_string()),
        Ok(r) if r.status != 200 => Err(format!("answered {}", r.status)),
        Ok(r) => serde_json::from_str::<UpdaterSelf>(&r.body)
            .map_err(|e| format!("unparsable self-report: {e}")),
    };
    UpdaterView {
        socket_exists: true,
        self_report: Some(report),
    }
}

/// Who answers the agent's health address right now (`/health` carries `node`
/// and `pid`, #152).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HealthIdentity {
    pub node: String,
    pub pid: u32,
}

#[derive(Debug, Clone, Default)]
pub struct HealthOwner {
    /// `None` when the endpoint is disabled (`QUASAR_HEALTH_ADDR` empty or 0).
    pub addr: Option<String>,
    /// `None` when nothing was asked; `Err` when nothing answered or the answer
    /// carried no identity.
    pub answer: Option<Result<HealthIdentity, String>>,
}

/// GET `/health` at the configured address and read who answered.
pub fn collect_health(addr: Option<String>) -> HealthOwner {
    let Some(addr) = addr else {
        return HealthOwner::default();
    };
    let answer = health_get(&addr).and_then(|body| parse_health(&body));
    HealthOwner {
        addr: Some(addr),
        answer: Some(answer),
    }
}

fn health_get(addr: &str) -> Result<String, String> {
    let mut stream = TcpStream::connect_timeout(
        &addr.parse().map_err(|e| format!("{addr}: {e}"))?,
        IO_TIMEOUT,
    )
    .map_err(|e| format!("nothing answers {addr}: {e}"))?;
    stream.set_read_timeout(Some(IO_TIMEOUT)).ok();
    stream.set_write_timeout(Some(IO_TIMEOUT)).ok();
    stream
        .write_all(b"GET /health HTTP/1.0\r\nHost: agent\r\n\r\n")
        .map_err(|e| e.to_string())?;
    let mut raw = Vec::new();
    stream
        .take(64 * 1024)
        .read_to_end(&mut raw)
        .map_err(|e| e.to_string())?;
    let text = String::from_utf8_lossy(&raw);
    Ok(text
        .find("\r\n\r\n")
        .map(|i| text[i + 4..].to_string())
        .unwrap_or_default())
}

/// The identity in a `/health` body. A body without `node`/`pid` is an answer
/// from something that is not a post-#152 agent, which is the finding.
pub fn parse_health(body: &str) -> Result<HealthIdentity, String> {
    #[derive(Deserialize)]
    struct Body {
        node: Option<String>,
        pid: Option<u32>,
    }
    let b: Body = serde_json::from_str(body).map_err(|_| "answered, but not with an agent's /health body".to_string())?;
    match (b.node, b.pid) {
        (Some(node), Some(pid)) => Ok(HealthIdentity { node, pid }),
        _ => Err("answered, but without an agent identity (an agent older than #152, or another program)".to_string()),
    }
}

/// `updater_socket`: the socket exists and the updater answers on it.
pub fn check_updater_socket(v: &UpdaterView, updater_present: Option<bool>) -> ReadinessCheck {
    if !v.socket_exists {
        return match updater_present {
            Some(true) => fail(
                CHECK_UPDATER_SOCKET,
                "the updater service is in this stack but its socket volume is not mounted in this container: the agent was created before the volume existed".into(),
                "Recreate the agent so it mounts the volume: docker compose up -d --force-recreate --no-deps quasar-node-agent".into(),
            ),
            _ => skip(CHECK_UPDATER_SOCKET, "No updater service in this stack"),
        };
    }
    match &v.self_report {
        Some(Ok(s)) => pass(
            CHECK_UPDATER_SOCKET,
            format!("Updater {} answered on its socket", if s.version.is_empty() { "(unknown version)" } else { s.version.as_str() }),
        ),
        Some(Err(e)) => fail(
            CHECK_UPDATER_SOCKET,
            format!("The updater's socket exists but it did not answer: {e}"),
            "Check the updater: docker compose logs quasar-updater; restart it with docker compose up -d quasar-updater".into(),
        ),
        None => skip(CHECK_UPDATER_SOCKET, "The updater was not asked"),
    }
}

/// `updater_stack_dir`: the updater discovered the stack it sits beside.
pub fn check_updater_stack_dir(v: &UpdaterView) -> ReadinessCheck {
    let Some(Ok(s)) = &v.self_report else {
        return skip(CHECK_UPDATER_STACK_DIR, "Not evaluated: the updater did not answer");
    };
    if s.working_dir.is_empty() || s.config_files.is_empty() {
        return fail(
            CHECK_UPDATER_STACK_DIR,
            "The updater has not discovered the stack it sits beside".into(),
            "Set QUASAR_STACK_DIR in deploy/.env to the stack directory's absolute host path and recreate quasar-updater".into(),
        );
    }
    pass(
        CHECK_UPDATER_STACK_DIR,
        format!("Updater acts on {} ({} compose file(s))", s.working_dir, s.config_files.len()),
    )
}

/// `updater_overlays`: this agent's container was started with the same
/// compose files the updater will recreate it with.
pub fn check_updater_overlays(v: &UpdaterView) -> ReadinessCheck {
    let Some(Ok(s)) = &v.self_report else {
        return skip(CHECK_UPDATER_OVERLAYS, "Not evaluated: the updater did not answer");
    };
    let Some(services) = &s.service_config_files else {
        return skip(CHECK_UPDATER_OVERLAYS, "The updater does not report per-service compose files");
    };
    match services.get(AGENT_SERVICE) {
        Some(Some(mine)) if mine != &s.config_files => fail(
            CHECK_UPDATER_OVERLAYS,
            format!(
                "This agent was started with [{}] but the updater with [{}]; an apply would recreate it with the updater's set",
                mine.join(", "),
                s.config_files.join(", ")
            ),
            "Bring the agent and the updater up with the same -f list, or recreate quasar-updater with the agent's".into(),
        ),
        Some(Some(_)) => pass(CHECK_UPDATER_OVERLAYS, "This agent was started with the updater's compose files".into()),
        _ => skip(CHECK_UPDATER_OVERLAYS, "The updater sees no running container for this agent's service"),
    }
}

/// `health_addr_bindable`: the configured health address is answered by this
/// agent. A squatter can only take the port while the agent is down, so on a
/// running post-#152 agent this passes by construction; its value is on an
/// older, tolerant agent (which reports the squatter) and on the next start,
/// which is exactly when an apply recreates the agent.
pub fn check_health_addr_bindable(h: &HealthOwner, me: &HealthIdentity) -> ReadinessCheck {
    let Some(addr) = &h.addr else {
        return skip(CHECK_HEALTH_ADDR_BINDABLE, "The health endpoint is disabled (QUASAR_HEALTH_ADDR)");
    };
    let port = addr.rsplit(':').next().unwrap_or(addr);
    let free_it = format!(
        "Find the owner (ss -ltnp | grep {port}) and stop it, or set QUASAR_HEALTH_ADDR to a free address in deploy/.env and recreate the agent"
    );
    match &h.answer {
        Some(Ok(id)) if id == me => {
            pass(CHECK_HEALTH_ADDR_BINDABLE, format!("{addr} is answered by this agent"))
        }
        Some(Ok(id)) => fail(
            CHECK_HEALTH_ADDR_BINDABLE,
            format!("{addr} is answered by node {} pid {}, not this agent (pid {}); the next agent start will fail to bind it", id.node, id.pid, me.pid),
            free_it,
        ),
        Some(Err(e)) => fail(
            CHECK_HEALTH_ADDR_BINDABLE,
            format!("{addr}: {e}; the next agent start will fail to bind it if another program holds the port"),
            free_it,
        ),
        None => skip(CHECK_HEALTH_ADDR_BINDABLE, "The health address was not probed"),
    }
}

use super::{fail, pass, skip};

#[cfg(test)]
mod tests {
    use super::*;

    fn healthy_self() -> UpdaterSelf {
        let mut services = BTreeMap::new();
        services.insert(AGENT_SERVICE.to_string(), Some(vec!["/srv/deploy/docker-compose.yml".to_string()]));
        UpdaterSelf {
            version: "0.2.5".into(),
            working_dir: "/srv/deploy".into(),
            config_files: vec!["/srv/deploy/docker-compose.yml".into()],
            service_config_files: Some(services),
        }
    }

    fn answered(s: UpdaterSelf) -> UpdaterView {
        UpdaterView { socket_exists: true, self_report: Some(Ok(s)) }
    }

    #[test]
    fn socket_absent_is_skip_without_an_updater_service_and_fail_with_one() {
        let none = UpdaterView::default();
        assert_eq!(check_updater_socket(&none, Some(false)).status, super::super::SKIP);
        assert_eq!(check_updater_socket(&none, None).status, super::super::SKIP);
        let c = check_updater_socket(&none, Some(true));
        assert_eq!(c.status, super::super::FAIL);
        assert!(c.remediation.contains("--force-recreate"), "{}", c.remediation);
    }

    #[test]
    fn socket_present_reports_the_answer() {
        assert_eq!(check_updater_socket(&answered(healthy_self()), Some(true)).status, super::super::PASS);
        let dead = UpdaterView { socket_exists: true, self_report: Some(Err("connection refused".into())) };
        let c = check_updater_socket(&dead, Some(true));
        assert_eq!(c.status, super::super::FAIL);
        assert!(c.summary.contains("connection refused"));
    }

    #[test]
    fn stack_dir_and_overlays() {
        let v = answered(healthy_self());
        assert_eq!(check_updater_stack_dir(&v).status, super::super::PASS);
        assert_eq!(check_updater_overlays(&v).status, super::super::PASS);

        let mut drift = healthy_self();
        drift.service_config_files.as_mut().unwrap().insert(
            AGENT_SERVICE.into(),
            Some(vec!["/srv/deploy/docker-compose.yml".into(), "/srv/deploy/overlays/dev.yml".into()]),
        );
        let c = check_updater_overlays(&answered(drift));
        assert_eq!(c.status, super::super::FAIL);
        assert!(c.summary.contains("overlays/dev.yml"), "{}", c.summary);

        let mut undiscovered = healthy_self();
        undiscovered.working_dir.clear();
        let c = check_updater_stack_dir(&answered(undiscovered));
        assert_eq!(c.status, super::super::FAIL);
        assert!(c.remediation.contains("QUASAR_STACK_DIR"));

        let mut old = healthy_self();
        old.service_config_files = None;
        assert_eq!(check_updater_overlays(&answered(old)).status, super::super::SKIP);
        assert_eq!(check_updater_overlays(&UpdaterView::default()).status, super::super::SKIP);
    }

    #[test]
    fn health_owner_must_be_this_agent() {
        let me = HealthIdentity { node: "gpu-01".into(), pid: 4242 };
        let mine = HealthOwner { addr: Some("127.0.0.1:9091".into()), answer: Some(Ok(me.clone())) };
        assert_eq!(check_health_addr_bindable(&mine, &me).status, super::super::PASS);

        let other = HealthOwner {
            addr: Some("127.0.0.1:9091".into()),
            answer: Some(Ok(HealthIdentity { node: "gpu-01".into(), pid: 4121 })),
        };
        let c = check_health_addr_bindable(&other, &me);
        assert_eq!(c.status, super::super::FAIL);
        assert!(c.summary.contains("pid 4121"), "{}", c.summary);
        assert!(c.remediation.contains("QUASAR_HEALTH_ADDR") && c.remediation.contains("9091"), "{}", c.remediation);

        let silent = HealthOwner { addr: Some("127.0.0.1:9091".into()), answer: Some(Err("nothing answers".into())) };
        assert_eq!(check_health_addr_bindable(&silent, &me).status, super::super::FAIL);

        assert_eq!(check_health_addr_bindable(&HealthOwner::default(), &me).status, super::super::SKIP);
    }

    #[test]
    fn health_body_parsing() {
        assert_eq!(
            parse_health(r#"{"status":"ok","sessions":0,"connected":true,"node":"gpu-01","pid":7}"#).unwrap(),
            HealthIdentity { node: "gpu-01".into(), pid: 7 }
        );
        assert!(parse_health(r#"{"status":"ok"}"#).is_err());
        assert!(parse_health("<html>").is_err());
    }

    #[test]
    fn self_report_tolerates_an_older_updater() {
        let s: UpdaterSelf = serde_json::from_str(r#"{"version":"0.2.4","working_dir":"/x","config_files":["/x/a.yml"],"images":{}}"#).unwrap();
        assert!(s.service_config_files.is_none());
    }
}
