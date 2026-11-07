//! Compatibility tools on this host (amendment 56): the `compat_tools.converge` runner that
//! installs GE-Proton under the trust rules, the `capacity.compat_tools` report, and
//! `session_assign.compat_tools` (the mounts and the mapping handed to the Steam image's launch
//! wrapper in `QUASAR_COMPAT_TOOLS`, which checks every name again before it writes Steam's
//! config). semantics: control-api.md §Compatibility tools, agent-api.md §session_assign.

pub mod extract;
pub mod fetch;
pub mod install;

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;
use std::sync::{Arc, OnceLock};

use serde_json::{json, Value};
use tracing::{info, warn};

use install::ToolStore;

pub const ENV_ENTRIES: &str = "QUASAR_COMPAT_TOOLS";
pub const ENV_TOOLS_PATH: &str = "STEAM_EXTRA_COMPAT_TOOLS_PATHS";
/// Where each mounted tool appears in the app container.
pub const MOUNT_DIR: &str = "/opt/quasar/compatibilitytools";
pub const DEFAULT_ROOT: &str = "/var/lib/quasar/compat-tools";

const MAX_ENTRIES: usize = 1001;
const MAX_MOUNTS: usize = 32;

/// A refused or failed run: `reason` is the contract's `summary.reason`.
#[derive(Debug)]
pub struct Fail {
    pub reason: &'static str,
    pub detail: String,
    /// More summary keys (`expected_sha512` / `actual_sha512` on a checksum mismatch).
    pub extra: Option<Value>,
}

impl Fail {
    pub fn new(reason: &'static str, detail: impl Into<String>) -> Self {
        Self {
            reason,
            detail: detail.into(),
            extra: None,
        }
    }

    fn into_outcome(self) -> crate::jobs::JobOutcome {
        let mut summary = json!({ "reason": self.reason });
        if let (Some(Value::Object(extra)), Some(map)) = (self.extra, summary.as_object_mut()) {
            map.extend(extra);
        }
        crate::jobs::JobOutcome::FailedWith(summary, format!("{}: {}", self.reason, self.detail))
    }
}

static STORE: OnceLock<Option<Arc<ToolStore>>> = OnceLock::new();
static CHANGED: OnceLock<tokio::sync::Notify> = OnceLock::new();

/// Opens the tools root (`QUASAR_COMPAT_TOOLS_ROOT`) once per process. A root that is not usable
/// leaves compatibility tools off on this host: `compat_tools_v1` is not advertised.
pub fn init() {
    STORE.get_or_init(|| {
        let root = std::env::var("QUASAR_COMPAT_TOOLS_ROOT")
            .ok()
            .filter(|r| !r.is_empty())
            .unwrap_or_else(|| DEFAULT_ROOT.to_owned());
        let home = std::env::var("QUASAR_HOME_ROOT").unwrap_or_default();
        let template = std::env::var("QUASAR_TEMPLATE_ROOT").unwrap_or_default();
        let mut others = vec![Path::new(&home), Path::new(&template)];
        let allow = std::env::var("QUASAR_APP_MOUNT_ALLOW").unwrap_or_default();
        others.extend(
            allow
                .split(',')
                .map(|a| Path::new(a.split(':').next().unwrap_or("").trim())),
        );
        match ToolStore::open(Path::new(&root), &others, true) {
            Ok(s) => {
                info!(
                    token = "compat-tools-ready",
                    "compat tools: tools directory {}/tools is usable", root
                );
                Some(Arc::new(s))
            }
            Err(why) => {
                warn!(
                    token = "compat-tools-unavailable",
                    "compat tools: off on this host: {why}"
                );
                None
            }
        }
    });
}

pub fn store() -> Option<&'static Arc<ToolStore>> {
    STORE.get().and_then(Option::as_ref)
}

/// `compat_tools_v1`: the runner, the report and the mounts all need a usable tools directory.
pub fn supported() -> bool {
    store().is_some()
}

/// Raised after every install or removal: the connection loop re-sends `capacity`.
pub fn changed() -> &'static tokio::sync::Notify {
    CHANGED.get_or_init(tokio::sync::Notify::new)
}

/// `capacity.compat_tools`, `None` when unsupported.
pub fn capacity() -> Option<Box<Value>> {
    let s = store()?;
    let tools: Vec<Value> = s
        .installed()
        .into_iter()
        .map(|e| json!({ "name": e.name, "tag": e.tag, "sha512": e.sha512, "bytes": e.bytes }))
        .collect();
    Some(Box::new(
        json!({ "free_bytes": s.free_bytes(), "tools": tools }),
    ))
}

/// The `compat_tools.converge` runner over the real github.com source.
pub fn runner() -> Option<install::ConvergeRunner> {
    let s = store()?.clone();
    Some(install::ConvergeRunner::new(
        s,
        Arc::new(fetch::HttpsSource::new(std::time::Duration::from_secs(
            50 * 60,
        ))),
        Arc::new(|| changed().notify_one()),
    ))
}

/// Drops the mount holds of sessions that are neither pending nor running any more.
pub fn retain_holds(live: &dyn Fn(&str) -> bool) {
    if let Some(s) = store() {
        s.retain(live);
    }
}

pub fn adopt(from: &str, to: &str, parked: bool) {
    if let Some(s) = store() {
        s.adopt(from, to, parked);
    }
}

pub fn release(session_id: &str) {
    if let Some(s) = store() {
        s.release(session_id);
    }
}

/// The `--mount` argument for one mounted tool: read-only, and a missing source fails the
/// container rather than being created.
pub fn mount_arg(name: &str) -> Option<String> {
    let s = store()?;
    Some(format!(
        "type=bind,src={}/{name},dst={MOUNT_DIR}/{name},readonly",
        s.tools_dir().display()
    ))
}

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
pub(crate) fn valid_name(s: &str) -> bool {
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

/// What an assign does to the app's env, and the tools to mount. With none sent (or one that cannot
/// be read) any `QUASAR_COMPAT_TOOLS` and tools path the app's own env carried are removed, so the
/// wrapper never sees a stale value.
/// The tools returned are held for `session_id` until it ends (removal waits for them).
pub fn apply_assign(
    env: &mut BTreeMap<String, String>,
    sent: Option<&Value>,
    session_id: &str,
    parked: bool,
) -> Vec<String> {
    match sent.and_then(parse) {
        Some(tools) => {
            let wanted: Vec<String> = tools.mount.iter().take(MAX_MOUNTS).cloned().collect();
            let mounted = store()
                .map(|s| s.mountable(session_id, &wanted, parked))
                .unwrap_or_default();
            apply(env, &tools, &mounted);
            mounted
        }
        None => {
            if sent.is_some() {
                warn!(
                    token = "compat-tools-unreadable",
                    "session_assign.compat_tools has a shape this agent cannot read; ignoring it"
                );
            }
            env.remove(ENV_ENTRIES);
            env.remove(ENV_TOOLS_PATH);
            Vec::new()
        }
    }
}

/// Sets `QUASAR_COMPAT_TOOLS` from the valid entries, replacing whatever `app.env` carried, and
/// the tools path only when a tool is mounted. An entry naming neither a Valve build nor a tool in
/// `mounted` is dropped and logged, never an assignment failure.
pub fn apply(env: &mut BTreeMap<String, String>, tools: &CompatTools, mounted: &[String]) {
    let mut seen = BTreeSet::new();
    let mut tokens = Vec::new();
    for e in tools.entries.iter().take(MAX_ENTRIES) {
        let ok = valid_appid(&e.appid)
            && valid_name(&e.name)
            && (valve_name(&e.name) || mounted.contains(&e.name))
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
    if mounted.is_empty() {
        env.remove(ENV_TOOLS_PATH);
    } else {
        env.insert(ENV_TOOLS_PATH.to_string(), MOUNT_DIR.to_string());
    }
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
            &[],
        );
        env
    }

    #[test]
    fn a_mounted_ge_tool_is_mapped_and_sets_the_tools_path() {
        let mut env = BTreeMap::new();
        let tools = CompatTools {
            mount: vec!["GE-Proton11-12-x86_64".into()],
            entries: vec![
                entry("0", "GE-Proton11-12-x86_64", false),
                entry("1145360", "GE-Proton11-13-x86_64", true),
            ],
        };
        apply(&mut env, &tools, &["GE-Proton11-12-x86_64".to_string()]);
        assert_eq!(env[ENV_ENTRIES], "0=GE-Proton11-12-x86_64");
        assert_eq!(env[ENV_TOOLS_PATH], MOUNT_DIR);
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
        apply_assign(&mut env, None, "s", false);
        assert_eq!(env, BTreeMap::from([("KEEP".to_string(), "1".to_string())]));
    }

    #[test]
    fn a_value_of_the_wrong_shape_is_dropped_and_strips_too() {
        for bad in [json!({"forced": "yes"}), json!("x"), json!(7), json!([1])] {
            let mut env = seeded();
            apply_assign(&mut env, Some(&bad), "s", false);
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
