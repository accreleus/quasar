//! Readiness checks for the update path: the facts preflight reads from this
//! host's own report, under the preflight check ids so the Hosts tab and the
//! Releases tab say the same words about the same thing
//! (control-api.md §"Self-update hardening").
//!
//! Collectors do the I/O once per probe (`ProbeEnv::live`); the checks are pure
//! over what they collected, so every branch is a unit test.

use std::io::{Read, Write};
use std::net::TcpStream;
use std::path::PathBuf;
use std::time::Duration;

use serde::Deserialize;

use crate::messages::ReadinessCheck;

/// Also the preflight check id. The Compose updater's `updater_stack_dir` and
/// `updater_overlays` are retired and stay reserved: never reuse them.
pub const CHECK_UPDATER_SOCKET: &str = "updater_socket";
pub const CHECK_HEALTH_ADDR_BINDABLE: &str = "health_addr_bindable";

/// The health probe's peer is a loopback port. Short, because the collectors run
/// inside the register-prep budget too (#191).
const IO_TIMEOUT: Duration = Duration::from_secs(1);

/// What one probe learned about this owned host's recovery actor, from the status
/// read that also reports its owner conflicts.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ActorView {
    pub socket: PathBuf,
    pub answered: bool,
    pub version: Option<String>,
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
    let b: Body = serde_json::from_str(body)
        .map_err(|_| "answered, but not with an agent's /health body".to_string())?;
    match (b.node, b.pid) {
        (Some(node), Some(pid)) => Ok(HealthIdentity { node, pid }),
        _ => Err("answered, but without an agent identity (an agent older than #152, or another program)".to_string()),
    }
}

/// `updater_socket`: on an owned host, the recovery actor answers on its agent
/// socket; a host with no actor has nothing that can replace its containers.
pub fn check_updater_socket(actor: Option<&ActorView>) -> ReadinessCheck {
    let Some(actor) = actor else {
        return skip(
            CHECK_UPDATER_SOCKET,
            "No recovery actor on this host: it was not installed with the seed, so the console cannot update it",
        );
    };
    if actor.answered {
        return pass(
            CHECK_UPDATER_SOCKET,
            format!(
                "Recovery actor {} answered on {}",
                actor.version.as_deref().unwrap_or("(unknown version)"),
                actor.socket.display()
            ),
        );
    }
    fail(
        CHECK_UPDATER_SOCKET,
        format!(
            "The recovery actor did not answer on {}",
            actor.socket.display()
        ),
        "Check it with docker ps -a --filter name=quasar-recovery. Exited means it was stopped with docker stop or docker kill, which Docker never restarts: docker start quasar-recovery finishes what it was doing. Otherwise read its log (docker logs quasar-recovery)".into(),
    )
}

/// `health_addr_bindable`: the configured health address is answered by this
/// agent. A squatter can only take the port while the agent is down, so on a
/// running post-#152 agent this passes by construction; its value is on an
/// older, tolerant agent (which reports the squatter) and on the next start,
/// which is exactly when an apply recreates the agent.
pub fn check_health_addr_bindable(h: &HealthOwner, me: &HealthIdentity) -> ReadinessCheck {
    let Some(addr) = &h.addr else {
        return skip(
            CHECK_HEALTH_ADDR_BINDABLE,
            "The health endpoint is disabled (QUASAR_HEALTH_ADDR)",
        );
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

    fn actor(answered: bool) -> ActorView {
        ActorView {
            socket: "/run/quasar-recovery/agent.sock".into(),
            answered,
            version: answered.then(|| "0.5.0".to_string()),
        }
    }

    #[test]
    fn no_recovery_actor_is_not_applicable_and_names_no_compose_command() {
        let c = check_updater_socket(None);
        assert_eq!(c.status, super::super::SKIP);
        assert!(c.summary.contains("seed"), "{}", c.summary);
        assert!(!c.summary.contains("compose"), "{}", c.summary);
    }

    #[test]
    fn an_owned_host_reports_whether_its_recovery_actor_answered() {
        let c = check_updater_socket(Some(&actor(true)));
        assert_eq!(c.status, super::super::PASS);
        assert!(c.summary.contains("0.5.0"), "{}", c.summary);

        let c = check_updater_socket(Some(&actor(false)));
        assert_eq!(c.status, super::super::FAIL);
        assert!(
            c.summary.contains("/run/quasar-recovery/agent.sock"),
            "{}",
            c.summary
        );
        assert!(
            c.remediation.contains("docker logs quasar-recovery")
                && !c.remediation.contains("compose"),
            "{}",
            c.remediation
        );
    }

    #[test]
    fn health_owner_must_be_this_agent() {
        let me = HealthIdentity {
            node: "gpu-01".into(),
            pid: 4242,
        };
        let mine = HealthOwner {
            addr: Some("127.0.0.1:9091".into()),
            answer: Some(Ok(me.clone())),
        };
        assert_eq!(
            check_health_addr_bindable(&mine, &me).status,
            super::super::PASS
        );

        let other = HealthOwner {
            addr: Some("127.0.0.1:9091".into()),
            answer: Some(Ok(HealthIdentity {
                node: "gpu-01".into(),
                pid: 4121,
            })),
        };
        let c = check_health_addr_bindable(&other, &me);
        assert_eq!(c.status, super::super::FAIL);
        assert!(c.summary.contains("pid 4121"), "{}", c.summary);
        assert!(
            c.remediation.contains("QUASAR_HEALTH_ADDR") && c.remediation.contains("9091"),
            "{}",
            c.remediation
        );

        let silent = HealthOwner {
            addr: Some("127.0.0.1:9091".into()),
            answer: Some(Err("nothing answers".into())),
        };
        assert_eq!(
            check_health_addr_bindable(&silent, &me).status,
            super::super::FAIL
        );

        assert_eq!(
            check_health_addr_bindable(&HealthOwner::default(), &me).status,
            super::super::SKIP
        );
    }

    #[test]
    fn health_body_parsing() {
        assert_eq!(
            parse_health(
                r#"{"status":"ok","sessions":0,"connected":true,"node":"gpu-01","pid":7}"#
            )
            .unwrap(),
            HealthIdentity {
                node: "gpu-01".into(),
                pid: 7
            }
        );
        assert!(parse_health(r#"{"status":"ok"}"#).is_err());
        assert!(parse_health("<html>").is_err());
    }
}
