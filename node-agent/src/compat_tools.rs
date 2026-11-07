//! `session_assign.compat_tools` (agent-api.md §session_assign, amendment 56): the effective
//! compatibility-tool mapping, handed to the Steam image's launch wrapper in
//! `QUASAR_COMPAT_TOOLS`. The wrapper checks every name again before it writes Steam's config.
//!
//! This build maps Valve's own Proton builds only. It mounts no tool, so a name that is not a Valve
//! name is dropped.

use std::collections::{BTreeMap, BTreeSet};

use serde_json::Value;
use tracing::warn;

pub const ENV_ENTRIES: &str = "QUASAR_COMPAT_TOOLS";
pub const ENV_TOOLS_PATH: &str = "STEAM_EXTRA_COMPAT_TOOLS_PATHS";

const MAX_ENTRIES: usize = 1001;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CompatTools {
    pub mount: Vec<String>,
    pub entries: Vec<CompatEntry>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CompatEntry {
    pub appid: String,
    pub name: String,
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

/// Reads `compat_tools` leniently: an entry of the wrong shape is skipped, any other shape of the
/// whole value is `None`. It never fails the assignment (a field of the wrong shape must drop only
/// itself).
pub fn parse(v: &Value) -> Option<CompatTools> {
    let o = v.as_object()?;
    let strings = |k: &str| -> Vec<String> {
        o.get(k)
            .and_then(Value::as_array)
            .map(|a| {
                a.iter()
                    .filter_map(|x| x.as_str().map(str::to_owned))
                    .collect()
            })
            .unwrap_or_default()
    };
    // `entries` is always sent (empty when there are none), and an empty list tells the wrapper to
    // remove what it wrote earlier: a value without one is not a mapping.
    let entries = o
        .get("entries")?
        .as_array()?
        .iter()
        .filter_map(|e| {
            Some(CompatEntry {
                appid: e.get("appid")?.as_str()?.to_owned(),
                name: e.get("name")?.as_str()?.to_owned(),
                forced: match e.get("forced") {
                    None => false,
                    Some(f) => f.as_bool()?,
                },
            })
        })
        .collect();
    Some(CompatTools {
        mount: strings("mount"),
        entries,
    })
}

/// What an assign does to the app's env: the sent mapping is applied, and with none sent (or one
/// that cannot be read) any `QUASAR_COMPAT_TOOLS` and tools path the app's own env carried are
/// removed, so the wrapper never sees a stale value.
pub fn apply_assign(env: &mut BTreeMap<String, String>, sent: Option<&Value>) {
    match sent.and_then(parse) {
        Some(tools) => apply(env, &tools),
        None => {
            if sent.is_some() {
                warn!(
                    token = "compat-tools-unreadable",
                    "session_assign.compat_tools has a shape this agent cannot read; ignoring it"
                );
            }
            env.remove(ENV_ENTRIES);
            env.remove(ENV_TOOLS_PATH);
        }
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
    use serde_json::json;

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

    fn seeded() -> BTreeMap<String, String> {
        BTreeMap::from([
            (ENV_ENTRIES.to_string(), "9=stale".to_string()),
            (ENV_TOOLS_PATH.to_string(), "/somewhere".to_string()),
            ("KEEP".to_string(), "1".to_string()),
        ])
    }

    #[test]
    fn nothing_sent_strips_what_the_apps_own_env_carried() {
        let mut env = seeded();
        apply_assign(&mut env, None);
        assert_eq!(env, BTreeMap::from([("KEEP".to_string(), "1".to_string())]));
    }

    #[test]
    fn a_value_of_the_wrong_shape_is_dropped_and_strips_too() {
        for bad in [json!({"forced": "yes"}), json!("x"), json!(7), json!([1])] {
            let mut env = seeded();
            apply_assign(&mut env, Some(&bad));
            assert!(!env.contains_key(ENV_ENTRIES), "{bad}");
            assert_eq!(env["KEEP"], "1");
        }
    }

    #[test]
    fn parse_skips_only_the_entries_of_the_wrong_shape() {
        let v = json!({"mount": ["a", 3], "entries": [
            {"appid": "0", "name": "proton_10"},
            {"appid": "1", "name": "proton_9", "forced": "yes"},
            {"appid": 2, "name": "proton_9"},
            {"appid": "3", "name": "proton_8", "forced": true}]});
        let t = parse(&v).unwrap();
        assert_eq!(t.mount, vec!["a".to_string()]);
        let got: Vec<_> = t
            .entries
            .iter()
            .map(|e| (e.appid.as_str(), e.forced))
            .collect();
        assert_eq!(got, vec![("0", false), ("3", true)]);
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
