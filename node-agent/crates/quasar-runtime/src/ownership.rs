//! Ownership labels: how a container on a shared engine is proved to be ours.
//!
//! The owner token itself is persisted and guarded by the process that owns it
//! (the agent keeps its identity under a [`crate::StateLease`]); this module is
//! only the label vocabulary and the proof rule every sweep applies.

/// The label carrying the owner token of every managed container.
pub const LABEL: &str = "io.quasar.agent-owner";

/// Inspect data is verified independently of Docker's listing filters. A label
/// alone never authorizes deletion of an unrelated prefix; a prefix alone never
/// authorizes deletion of another agent's or legacy unlabelled containers.
pub fn owned_id(value: &serde_json::Value, owner: &str, prefixes: &[&str]) -> Option<String> {
    let id = value["Id"].as_str()?;
    let name = value["Name"].as_str()?.strip_prefix('/')?;
    if id.len() != 64
        || !id.bytes().all(|b| b.is_ascii_hexdigit())
        || !prefixes.iter().any(|prefix| name.starts_with(prefix))
        || value["Labels"][LABEL].as_str() != Some(owner)
    {
        return None;
    }
    Some(id.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn cleanup_requires_both_exact_prefix_and_matching_owner_for_all_states() {
        let prefixes = ["quasar-sess-", "quasar-pulse-"];
        for running in [true, false] {
            for name in ["/quasar-sess-sid", "/quasar-pulse-sid"] {
                let mut value = json!({"Id": "a".repeat(64), "Name": name,
                    "Labels": {LABEL: "one"}, "State": {"Running": running}});
                assert!(owned_id(&value, "one", &prefixes).is_some());
                assert!(owned_id(&value, "two", &prefixes).is_none());
                value["Labels"] = json!({});
                assert!(owned_id(&value, "one", &prefixes).is_none());
            }
        }
        for name in ["/other-quasar-sess-sid", "/database", "/quasar-session"] {
            let value = json!({"Id": "b".repeat(64), "Name": name, "Labels": {LABEL: "one"}});
            assert!(owned_id(&value, "one", &prefixes).is_none());
        }
    }
}
