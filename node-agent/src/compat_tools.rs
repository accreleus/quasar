//! `session_assign.compat_tools` (agent-api.md §session_assign, amendment 56): the effective
//! compatibility-tool mapping, handed to the Steam image's launch wrapper in
//! `QUASAR_COMPAT_TOOLS`. The wrapper checks every name again before it writes Steam's config.
//!
//! This build maps Valve's own Proton builds only. It mounts no tool, so a name that is not a Valve
//! name is dropped.

use std::collections::{BTreeMap, BTreeSet};

use serde::Deserialize;
use tracing::warn;

pub const ENV_ENTRIES: &str = "QUASAR_COMPAT_TOOLS";
pub const ENV_TOOLS_PATH: &str = "STEAM_EXTRA_COMPAT_TOOLS_PATHS";

const MAX_ENTRIES: usize = 1001;

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
pub struct CompatTools {
    #[serde(default)]
    pub mount: Vec<String>,
    #[serde(default)]
    pub entries: Vec<CompatEntry>,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct CompatEntry {
    pub appid: String,
    pub name: String,
    #[serde(default)]
    pub forced: bool,
}

/// `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`. It is a directory name and a token in an environment value.
fn valid_name(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 64
        && b[0].is_ascii_alphanumeric()
        && b.iter()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'-'))
}

/// `^[1-9][0-9]{0,9}$`, or `0` (the fleet default's slot).
fn valid_appid(s: &str) -> bool {
    s == "0"
        || (!s.is_empty()
            && s.len() <= 10
            && !s.starts_with('0')
            && s.bytes().all(|c| c.is_ascii_digit()))
}

/// `^proton_(experimental|hotfix|[0-9]{1,3})$`
fn valve_name(s: &str) -> bool {
    match s.strip_prefix("proton_") {
        Some("experimental" | "hotfix") => true,
        Some(n) => (1..=3).contains(&n.len()) && n.bytes().all(|c| c.is_ascii_digit()),
        None => false,
    }
}

/// Sets `QUASAR_COMPAT_TOOLS` from the valid entries and removes `STEAM_EXTRA_COMPAT_TOOLS_PATHS`,
/// replacing whatever `app.env` carried. An entry that fails is dropped and logged, never an
/// assignment failure.
pub fn apply(env: &mut BTreeMap<String, String>, tools: &CompatTools) {
    if !tools.mount.is_empty() {
        warn!(
            token = "compat-tools-mount-unsupported",
            "compat_tools.mount names {} tool(s); this agent mounts none, so only Valve names are mapped",
            tools.mount.len()
        );
    }
    let mut seen = BTreeSet::new();
    let mut tokens = Vec::new();
    for e in tools.entries.iter().take(MAX_ENTRIES) {
        let ok = valid_appid(&e.appid)
            && valid_name(&e.name)
            && valve_name(&e.name)
            && !(e.appid == "0" && e.forced)
            && seen.insert(e.appid.as_str());
        if !ok {
            warn!(
                token = "compat-tools-entry-dropped",
                "dropping compat_tools entry appid={:?} name={:?}", e.appid, e.name
            );
            continue;
        }
        tokens.push(format!(
            "{}={}{}",
            e.appid,
            e.name,
            if e.forced { "!" } else { "" }
        ));
    }
    env.insert(ENV_ENTRIES.to_string(), tokens.join(" "));
    env.remove(ENV_TOOLS_PATH);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(appid: &str, name: &str, forced: bool) -> CompatEntry {
        CompatEntry {
            appid: appid.into(),
            name: name.into(),
            forced,
        }
    }

    fn run(entries: Vec<CompatEntry>) -> BTreeMap<String, String> {
        let mut env = BTreeMap::from([
            (ENV_ENTRIES.to_string(), "9=stale".to_string()),
            (ENV_TOOLS_PATH.to_string(), "/somewhere".to_string()),
            ("KEEP".to_string(), "1".to_string()),
        ]);
        apply(
            &mut env,
            &CompatTools {
                mount: vec![],
                entries,
            },
        );
        env
    }

    #[test]
    fn writes_the_tokens_the_wrapper_reads() {
        let env = run(vec![
            entry("0", "proton_10", false),
            entry("1145360", "proton_hotfix", true),
        ]);
        assert_eq!(env[ENV_ENTRIES], "0=proton_10 1145360=proton_hotfix!");
        assert_eq!(env["KEEP"], "1");
        assert!(!env.contains_key(ENV_TOOLS_PATH));
    }

    #[test]
    fn no_entries_still_clears_what_app_env_carried() {
        assert_eq!(run(vec![])[ENV_ENTRIES], "");
    }

    #[test]
    fn drops_what_cannot_be_written_safely() {
        let env = run(vec![
            entry("0", "proton_9", true),
            entry("007", "proton_9", false),
            entry("1 -foo", "proton_9", false),
            entry("12", "proton_9 x", false),
            entry("13", "../proton_9", false),
            entry("14", "GE-Proton11-12-x86_64", false),
            entry("15", "proton_", false),
            entry("16", "proton_1234", false),
            entry("20", "proton_8", false),
            entry("20", "proton_7", true),
        ]);
        assert_eq!(env[ENV_ENTRIES], "20=proton_8");
    }
}
