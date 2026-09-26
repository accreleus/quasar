//! `owner_conflict` on an owned install (control-api.md amendment 14 §"Preflight"): a
//! container on this machine that looks like a Quasar platform service but was not created
//! by this installation (a leftover Compose stack, a definition a manager still holds).
//!
//! The recovery actor decides what is a conflict and reports each in its status
//! (`quasar_recovery::race_guard`); this lifts that report into the readiness check the
//! preflight reads under the same id. It never carries `blocks`: a conflict stops a
//! replacement, not a launch. Absent on a host that is not owned.

use serde::Deserialize;

use crate::messages::ReadinessCheck;

pub const CHECK_OWNER_CONFLICT: &str = "owner_conflict";

/// One look-alike, as the actor's `GET /v1/status` names it. Fields an older actor does
/// not send default to empty.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Conflict {
    pub container: String,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub image: String,
    #[serde(default)]
    pub role: String,
    #[serde(default)]
    pub why: String,
}

/// What the actor said on this refresh: `None` when it did not answer.
pub type Observed = Option<Vec<Conflict>>;

/// "a node agent": the role as the summary names it. Twin of `race_guard::noun`.
fn noun(role: &str) -> &'static str {
    match role {
        "node-agent" => "node agent",
        "control-plane" => "control plane",
        "postgres" => "database",
        "recovery-actor" => "recovery actor",
        "updater" => "updater",
        _ => "platform service",
    }
}

/// The check, on an owned install; `None` on any other.
pub fn check(owned: bool, observed: &Observed) -> Option<ReadinessCheck> {
    if !owned {
        return None;
    }
    let Some(conflicts) = observed else {
        return Some(super::unknown(
            CHECK_OWNER_CONFLICT,
            "The recovery actor did not answer, so this machine was not checked for containers it did not create",
        ));
    };
    if conflicts.is_empty() {
        return Some(super::pass(
            CHECK_OWNER_CONFLICT,
            "No container on this machine looks like a Quasar service without being this installation's".into(),
        ));
    }
    let each: Vec<String> = conflicts
        .iter()
        .map(|c| {
            let mut line = format!(
                "{} looks like a Quasar {}, but this installation did not create it",
                c.container,
                noun(&c.role)
            );
            if !c.why.is_empty() {
                line.push_str(&format!(" ({})", c.why));
            }
            line
        })
        .collect();
    // The summary only says what is in the way: the console adds what follows from it.
    let names: Vec<&str> = conflicts.iter().map(|c| c.container.as_str()).collect();
    let mut check = super::fail(
        CHECK_OWNER_CONFLICT,
        each.join("; "),
        format!("docker rm -f {}", names.join(" ")),
    );
    check.source = Some("local".into());
    Some(check)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn conflict(name: &str, role: &str) -> Conflict {
        Conflict {
            container: name.into(),
            id: "3e8b0c91d2f4".into(),
            image: "ghcr.io/accreleus/quasar/quasar-node-agent:0.3.0".into(),
            role: role.into(),
            why: "part of the Compose project quasar, probably left from an older Compose install"
                .into(),
        }
    }

    #[test]
    fn only_an_owned_install_carries_the_check_and_it_never_blocks() {
        assert!(check(false, &Some(vec![conflict("x", "node-agent")])).is_none());
        for observed in [None, Some(vec![]), Some(vec![conflict("x", "node-agent")])] {
            let c = check(true, &observed).expect("owned");
            assert_eq!(c.id, CHECK_OWNER_CONFLICT);
            assert!(c.blocks.is_none(), "{c:?}");
        }
    }

    #[test]
    fn a_conflict_fails_naming_the_container_and_the_fix() {
        let c = check(
            true,
            &Some(vec![conflict("quasar-node-agent-1", "node-agent")]),
        )
        .unwrap();
        assert_eq!(c.status, super::super::FAIL);
        assert!(
            c.summary.starts_with("quasar-node-agent-1 looks like a Quasar node agent, but this installation did not create it"),
            "{}",
            c.summary
        );
        assert!(
            c.summary.contains("Compose project quasar"),
            "{}",
            c.summary
        );
        assert_eq!(c.remediation, "docker rm -f quasar-node-agent-1");
        let two = check(
            true,
            &Some(vec![conflict("a", "node-agent"), conflict("b", "updater")]),
        )
        .unwrap();
        assert_eq!(two.remediation, "docker rm -f a b");
        assert!(
            two.summary.contains("b looks like a Quasar updater"),
            "{}",
            two.summary
        );
    }

    #[test]
    fn no_answer_is_unknown_and_nothing_found_passes() {
        assert_eq!(check(true, &None).unwrap().status, super::super::UNKNOWN);
        assert_eq!(
            check(true, &Some(vec![])).unwrap().status,
            super::super::PASS
        );
    }

    #[test]
    fn an_older_actor_without_id_or_role_still_reads() {
        let c: Conflict = serde_json::from_str(
            r#"{"container":"deploy-quasar-control-plane-1","image":"x","why":"y"}"#,
        )
        .unwrap();
        assert_eq!(c.role, "");
    }
}
