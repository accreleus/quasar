//! Connection-scoped Steam preparation authorization. Desired policy never
//! authorizes a job by itself: exact image identity, host permission and current
//! revision are checked again while committing a template or a seeded home.
use crate::session::{
    template::{CloneMode, TemplateStore},
    warmup::WarmupControl,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::{Arc, Mutex};

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct ImageIdentity {
    pub image_id: String,
    pub registry_ref: String,
    pub version: String,
}
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct Snapshot {
    revision: String,
    enabled: bool,
    images: Vec<ImageIdentity>,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Permission {
    Enabled,
    Disabled,
    Invalid,
}
pub fn permission(value: Option<&str>) -> Permission {
    match value.map(|v| v.trim().to_ascii_lowercase()).as_deref() {
        None | Some("") => Permission::Enabled,
        Some("1" | "true" | "yes" | "on") => Permission::Enabled,
        Some("0" | "false" | "no" | "off") => Permission::Disabled,
        _ => Permission::Invalid,
    }
}
fn valid_image(image: &ImageIdentity) -> bool {
    image.image_id == "steam"
        && !image.version.is_empty()
        && image.version.len() <= 256
        && image
            .version
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-+".contains(&b))
        && image
            .registry_ref
            .strip_prefix("ghcr.io/accreleus/quasar-steam@sha256:")
            .is_some_and(|digest| {
                digest.len() == 64
                    && digest
                        .bytes()
                        .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
            })
}
struct State {
    snapshot: Option<Snapshot>,
    authorized: bool,
    generation: u64,
    store: Option<TemplateStore>,
    home_root: String,
    store_key: String,
    phase: String,
    reason: String,
    detail: String,
}
fn bounded(mut text: String) -> String {
    let mut end = text.len().min(1024);
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text.truncate(end);
    text
}
impl State {
    fn safe_detail(&self, text: &str) -> String {
        let mut text = text.to_owned();
        if !self.home_root.is_empty() {
            text = text.replace(&self.home_root, "[home storage]");
        }
        if let Some(store) = &self.store {
            text = text.replace(
                store.template_root().to_string_lossy().as_ref(),
                "[template storage]",
            );
        }
        bounded(text)
    }
}
pub struct SourcePolicy {
    state: Mutex<State>,
    control: Arc<WarmupControl>,
    images: Arc<crate::images::ImageManager>,
    preparation: Permission,
    consumption: Permission,
}
impl std::fmt::Debug for SourcePolicy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("SourcePolicy")
    }
}
impl SourcePolicy {
    pub fn new(
        home_root: &str,
        control: Arc<WarmupControl>,
        images: Arc<crate::images::ImageManager>,
    ) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                snapshot: None,
                authorized: false,
                generation: 0,
                store: TemplateStore::resolve_from_env(std::path::Path::new(home_root)),
                home_root: home_root.into(),
                store_key: String::new(),
                phase: "deferred".into(),
                reason: "none".into(),
                detail: String::new(),
            }),
            control,
            images,
            preparation: permission(std::env::var("QUASAR_TEMPLATE_WARMUP").ok().as_deref()),
            consumption: permission(std::env::var("QUASAR_HOME_TEMPLATES").ok().as_deref()),
        })
    }
    pub fn apply(&self, value: &Value) {
        let block = match value.get("steam_preparation") {
            Some(block) => block,
            None if value.is_object() => return,
            None => value,
        };
        let parsed = serde_json::from_value::<Snapshot>(block.clone())
            .ok()
            .filter(|s| {
                s.revision
                    .parse::<u64>()
                    .is_ok_and(|r| r > 0 && r.to_string() == s.revision)
                    && s.images.len() <= 1
                    && s.images.iter().all(valid_image)
            });
        let mut state = self.state.lock().unwrap();
        if let (Some(old), Some(new)) = (&state.snapshot, &parsed) {
            let old_rev: u64 = old.revision.parse().unwrap();
            let new_rev: u64 = new.revision.parse().unwrap();
            if new_rev < old_rev {
                return;
            }
            if new_rev == old_rev && old == new {
                return;
            }
        }
        let conflict = state
            .snapshot
            .as_ref()
            .zip(parsed.as_ref())
            .is_some_and(|(a, b)| a.revision == b.revision && a != b);
        state.generation += 1;
        state.authorized = parsed.is_some() && !conflict;
        self.control.abort_for_policy_change();
        if state.authorized {
            state.snapshot = parsed;
            state.phase = "deferred".into();
            state.reason = "none".into();
            state.detail.clear();
        } else {
            state.phase = "failed".into();
            state.reason = "stale_policy".into();
            state.detail = "Steam preparation policy is malformed or conflicts with its revision; waiting for a new valid snapshot".into();
            tracing::warn!(token = "steam-source-policy-invalid", "{}", state.detail);
        }
    }
    pub fn invalidate(&self) {
        let mut state = self.state.lock().unwrap();
        state.authorized = false;
        state.generation += 1;
        self.control.abort_for_connection_lost();
    }
    pub fn update_root(&self, root: &str) {
        let mut state = self.state.lock().unwrap();
        if state.home_root != root {
            state.generation += 1;
            self.control.abort_for_policy_change();
            state.home_root = root.into();
            state.store_key.clear();
            state.store = TemplateStore::resolve_from_env(std::path::Path::new(root));
            state.phase = "deferred".into();
            state.reason = "none".into();
            state.detail.clear();
        }
    }
    fn refresh_store(&self) {
        use std::os::unix::fs::MetadataExt;
        let mut state = self.state.lock().unwrap();
        let configured = std::env::var("QUASAR_TEMPLATE_ROOT").unwrap_or_default();
        let root = if configured.trim().is_empty() {
            std::path::Path::new(&state.home_root)
                .parent()
                .unwrap_or(std::path::Path::new("/"))
                .join("templates")
        } else {
            configured.into()
        };
        let stamp = |path: &std::path::Path| {
            std::fs::metadata(path)
                .map(|m| {
                    format!(
                        "{}:{}:{}:{}:{}",
                        m.dev(),
                        m.ino(),
                        m.mode(),
                        m.uid(),
                        m.gid()
                    )
                })
                .unwrap_or_default()
        };
        let key = format!(
            "{}:{}:{}:{}",
            root.display(),
            stamp(&root),
            stamp(std::path::Path::new(&state.home_root)),
            std::env::var("QUASAR_TEMPLATE_CLONE_MODE").unwrap_or_default()
        );
        if state.store_key != key {
            if !state.store_key.is_empty() {
                state.generation += 1;
                self.control.abort_for_policy_change();
            }
            state.store = TemplateStore::resolve_from_env(std::path::Path::new(&state.home_root));
            if state.store.is_some() && state.reason == "storage_unavailable" {
                // A skipped/failed job has no deferred run left to wake itself.
                // Report the repair edge so the control plane can reconcile once;
                // ordinary unchanged reports must preserve dispatcher backoff.
                state.phase = "deferred".into();
                state.reason = "none".into();
                state.detail = "Template storage changed; preparation can be retried".into();
            }
            state.store_key = key;
        }
    }
    pub fn store(&self) -> Option<TemplateStore> {
        self.refresh_store();
        self.state.lock().unwrap().store.clone()
    }
    pub fn authorize(
        self: &Arc<Self>,
        image: &ImageIdentity,
        revision: Option<&str>,
        consume: bool,
    ) -> Result<PolicyLease, String> {
        self.refresh_store();
        let state = self.state.lock().unwrap();
        if !self.allowed(&state, image, revision, consume) {
            return Err("Steam preparation is disabled, image is not ready, or source policy is stale/unavailable".into());
        }
        Ok(PolicyLease {
            policy: self.clone(),
            image: image.clone(),
            generation: state.generation,
            consume,
        })
    }
    fn allowed(
        &self,
        state: &State,
        image: &ImageIdentity,
        revision: Option<&str>,
        consume: bool,
    ) -> bool {
        state.authorized
            && state.snapshot.as_ref().is_some_and(|s| {
                s.enabled && s.images.contains(image) && revision.is_none_or(|r| r == s.revision)
            })
            && (!consume
                || state
                    .store
                    .as_ref()
                    .is_none_or(|s| s.clone_mode() != CloneMode::Off))
            && (if consume {
                self.consumption
            } else {
                self.preparation
            }) == Permission::Enabled
            && self
                .images
                .is_exact_ready(&image.image_id, &image.registry_ref, &image.version)
    }
    pub fn seed(
        self: &Arc<Self>,
        image_id: &str,
        registry_ref: &str,
    ) -> Option<(
        TemplateStore,
        crate::session::template::TemplateSeed,
        PolicyLease,
    )> {
        let image = self
            .state
            .lock()
            .unwrap()
            .snapshot
            .as_ref()?
            .images
            .iter()
            .find(|i| i.image_id == image_id && i.registry_ref == registry_ref)?
            .clone();
        let lease = self.authorize(&image, None, true).ok()?;
        let store = self.store()?;
        let meta = store.meta(image_id)?;
        if meta.version != image.version || meta.registry_ref != image.registry_ref {
            return None;
        }
        let seed = store.seed(image_id)?;
        Some((store, seed, lease))
    }
    pub fn status(&self, phase: &str, reason: &str, detail: &str) {
        let mut state = self.state.lock().unwrap();
        state.phase = phase.into();
        state.reason = reason.into();
        state.detail = state.safe_detail(detail);
    }
    pub fn report(&self) -> Option<Value> {
        self.refresh_store();
        let state = self.state.lock().unwrap();
        let policy = state.snapshot.as_ref()?;
        let images = policy.images.iter().map(|image| {
            let prep = state.authorized && policy.enabled && self.preparation == Permission::Enabled;
            let clone_off = state.store.as_ref().is_some_and(|s| s.clone_mode() == CloneMode::Off);
            let consume = state.authorized && policy.enabled && self.consumption == Permission::Enabled && !clone_off;
            let meta = state.store.as_ref().and_then(|s| s.meta(&image.image_id));
            let matching = meta.as_ref().is_some_and(|m| m.registry_ref == image.registry_ref && m.version == image.version)
                && state.store.as_ref().and_then(|s| s.seed(&image.image_id)).is_some();
            let ready = self.images.is_exact_ready(&image.image_id, &image.registry_ref, &image.version);
            let (phase, reason) = if !state.authorized { ("failed", "stale_policy") }
                else if !policy.enabled { ("disabled", "source_disabled") }
                else if self.preparation == Permission::Invalid || self.consumption == Permission::Invalid { ("disabled", "host_setting_invalid") }
                else if !prep && !consume { ("disabled", "host_permissions_disabled") }
                else if !prep { (if matching { "ready" } else { "disabled" }, "host_warmup_disabled") }
                else if state.store.is_none() { ("deferred", "storage_unavailable") }
                else if !ready { ("waiting_image", "image_not_ready") }
                else if clone_off { (if matching { "ready" } else { state.phase.as_str() }, if state.reason == "storage_unavailable" { "storage_unavailable" } else { "host_templates_disabled" }) }
                else if matching { ("ready", if consume { "none" } else { "host_templates_disabled" }) }
                else { (state.phase.as_str(), state.reason.as_str()) };
            let clone_mode = state.store.as_ref().and_then(|s| match s.clone_mode() { CloneMode::Reflink => Some("reflink"), CloneMode::Copy => Some("copy"), CloneMode::Off => None });
            json!({"image_id":image.image_id,"registry_ref":image.registry_ref,"version":image.version,
                "preparation_enabled":prep,"consumption_enabled":consume,"state":phase,"reason":reason,
                "template":meta.filter(|_| matching).map(|m|json!({"registry_ref":m.registry_ref,"version":m.version})),
                "clone_mode":clone_mode,"clone_reason":state.store.as_ref().and_then(|s| s.clone_reason()).map(|reason| state.safe_detail(reason)), "detail":state.detail})
        }).collect::<Vec<_>>();
        Some(json!({"steam":{"policy_revision":policy.revision,"images":images}}))
    }
}
#[derive(Clone)]
pub struct PolicyLease {
    policy: Arc<SourcePolicy>,
    image: ImageIdentity,
    generation: u64,
    consume: bool,
}
impl PolicyLease {
    pub fn phase(&self, detail: &str) {
        let mut state = self.policy.state.lock().unwrap();
        if state.generation == self.generation {
            state.phase = "preparing".into();
            state.reason = "none".into();
            state.detail = state.safe_detail(detail);
        }
    }
    pub fn commit<T>(&self, action: impl FnOnce() -> std::io::Result<T>) -> std::io::Result<T> {
        self.policy.refresh_store();
        let state = self.policy.state.lock().unwrap();
        if state.generation != self.generation
            || !self.policy.allowed(&state, &self.image, None, self.consume)
        {
            return Err(std::io::Error::other(
                "Steam source policy changed before commit",
            ));
        }
        action()
    }
}
pub struct ConnectionGuard(pub Arc<SourcePolicy>);
impl Drop for ConnectionGuard {
    fn drop(&mut self) {
        self.0.invalidate();
    }
}

impl crate::images::ImageLifecycleObserver for SourcePolicy {
    fn image_ready(&self, _image_id: &str, _registry_ref: &str, _version: &str) {}
    fn image_removed(&self, image_id: &str) {
        if image_id == "steam" {
            self.control.abort_for_policy_change();
            if let Some(store) = self.store() {
                if let Err(error) = store.remove(image_id) {
                    tracing::warn!(token = "steam-template-remove-failed", "{error}");
                }
            }
        }
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    pub(crate) fn identity() -> ImageIdentity {
        ImageIdentity {
            image_id: "steam".into(),
            registry_ref: format!("ghcr.io/accreleus/quasar-steam@sha256:{}", "a".repeat(64)),
            version: "1".into(),
        }
    }
    pub(crate) fn snapshot(revision: &str, enabled: bool) -> Value {
        json!({"steam_preparation":{"revision":revision,"enabled":enabled,"images":[identity()]}})
    }
    pub(crate) fn fixture() -> (tempfile::TempDir, Arc<SourcePolicy>) {
        let dir = tempfile::tempdir().unwrap();
        let homes = dir.path().join("homes");
        std::fs::create_dir(&homes).unwrap();
        let path = dir.path().join("images.json");
        let image = identity();
        std::fs::write(&path,json!({"images":{"steam":{"registry_ref":image.registry_ref,"version":image.version,"state":"ready"}}}).to_string()).unwrap();
        let images = crate::images::ImageManager::new(
            crate::session::container::ContainerRuntime::test_runtime("/bin/true"),
            path.to_str().unwrap().into(),
        );
        let policy = SourcePolicy::new(
            homes.to_str().unwrap(),
            Arc::new(WarmupControl::new()),
            images,
        );
        (dir, policy)
    }
    #[test]
    fn absent_and_compose_empty_permissions_default_enabled_but_explicit_false_and_invalid_do_not()
    {
        for value in [None, Some(""), Some(" "), Some("true"), Some("1")] {
            assert_eq!(permission(value), Permission::Enabled);
        }
        for value in ["0", "false", "off", "no"] {
            assert_eq!(permission(Some(value)), Permission::Disabled);
        }
        assert_eq!(permission(Some("perhaps")), Permission::Invalid);
    }
    #[test]
    fn same_path_storage_repair_clears_the_outcome_and_emits_one_reconciliation_edge() {
        use std::os::unix::fs::PermissionsExt;
        let (_dir, policy) = fixture();
        policy.apply(&snapshot("1", true));
        let store = policy.store().unwrap();
        {
            let mut state = policy.state.lock().unwrap();
            state.store = None;
            state.phase = "deferred".into();
            state.reason = "storage_unavailable".into();
            state.detail = "prior storage failure".into();
        }
        let before = policy.report().unwrap();
        assert_eq!(
            before["steam"]["images"][0]["reason"],
            "storage_unavailable"
        );
        // The configured path is unchanged; its real permission fingerprint changes.
        std::fs::set_permissions(store.home_root(), std::fs::Permissions::from_mode(0o750))
            .unwrap();
        let repaired = policy.report().unwrap();
        assert_eq!(repaired["steam"]["images"][0]["state"], "deferred");
        assert_eq!(repaired["steam"]["images"][0]["reason"], "none");
        assert_eq!(repaired["steam"]["policy_revision"], "1");
        assert!(policy.store().is_some());
        assert_eq!(
            policy.report().unwrap(),
            repaired,
            "unchanged reports must not repeatedly trigger recovery"
        );
    }
    #[test]
    fn disabled_or_failed_forced_clone_reports_no_consumption_but_allows_preparation() {
        use crate::session::template::TemplateCloneMode;
        for forced_failure in [false, true] {
            let (dir, policy) = fixture();
            policy.apply(&snapshot("1", true));
            let normal = policy.store().unwrap();
            let missing = dir.path().join("missing-home");
            let store = if forced_failure {
                TemplateStore::resolve(&missing, None, TemplateCloneMode::Reflink).unwrap()
            } else {
                TemplateStore::resolve(normal.home_root(), None, TemplateCloneMode::Off).unwrap()
            };
            assert_eq!(store.clone_mode(), CloneMode::Off);
            policy.state.lock().unwrap().store = Some(store);
            let report = policy.report().unwrap();
            let row = &report["steam"]["images"][0];
            assert_eq!(row["preparation_enabled"], true);
            assert_eq!(row["consumption_enabled"], false);
            assert_eq!(row["reason"], "host_templates_disabled");
            assert!(row["clone_reason"].as_str().is_some_and(|r| !r.is_empty()));
            assert!(policy.authorize(&identity(), Some("1"), false).is_ok());
            assert!(policy.authorize(&identity(), Some("1"), true).is_err());
            policy.status(
                "failed",
                "storage_unavailable",
                "preparation storage failed",
            );
            assert_eq!(
                policy.report().unwrap()["steam"]["images"][0]["reason"],
                "storage_unavailable"
            );
            policy.status("deferred", "none", "preparation storage recovered");
            assert_eq!(
                policy.report().unwrap()["steam"]["images"][0]["reason"],
                "host_templates_disabled"
            );
        }
    }
    #[test]
    fn human_text_is_bounded_by_utf8_bytes_not_characters() {
        let text = bounded("😀".repeat(1024));
        assert_eq!(text.len(), 1024);
        assert_eq!(text.chars().count(), 256);
    }
    #[test]
    fn exact_steam_identity_only_never_home_metadata_or_repository_prefix() {
        let mut image = identity();
        assert!(valid_image(&image));
        image.image_id = "custom".into();
        assert!(!valid_image(&image));
        image = identity();
        image.registry_ref = image
            .registry_ref
            .replace("quasar-steam@", "quasar-steam-fork@");
        assert!(!valid_image(&image));
        image.registry_ref = "ghcr.io/accreleus/quasar-steam:latest".into();
        assert!(!valid_image(&image));
        image = identity();
        image.registry_ref.push('a');
        assert!(!valid_image(&image));
    }
    #[test]
    fn revision_order_duplicates_conflicts_missing_policy_and_reconnect_fail_closed() {
        let (_dir, policy) = fixture();
        let image = identity();
        assert!(policy.authorize(&image, Some("1"), false).is_err());
        policy.apply(&snapshot("8", true));
        assert!(policy.authorize(&image, Some("8"), false).is_ok());
        policy.apply(&json!({"unrelated":{}}));
        policy.apply(&snapshot("7", false));
        assert!(policy.authorize(&image, Some("8"), true).is_ok());
        policy.apply(&snapshot("8", true));
        assert!(policy.authorize(&image, Some("8"), false).is_ok());
        policy.apply(&snapshot("8", false));
        assert!(policy.authorize(&image, Some("8"), false).is_err());
        policy.apply(&snapshot("9", true));
        let lease = policy.authorize(&image, Some("9"), false).unwrap();
        policy.invalidate();
        assert!(lease.commit(|| Ok(())).is_err());
        let (_newdir, new) = fixture();
        new.apply(&snapshot("1", true));
        assert!(new.authorize(&image, Some("1"), false).is_ok());
    }
    #[test]
    fn disabled_policy_wins_over_host_true_and_host_optouts_are_independent() {
        let (_dir, mut policy) = fixture();
        Arc::get_mut(&mut policy).unwrap().preparation = Permission::Disabled;
        policy.apply(&snapshot("1", true));
        let image = identity();
        assert!(policy.authorize(&image, Some("1"), false).is_err());
        assert!(policy.authorize(&image, None, true).is_ok());
        let report = policy.report().unwrap();
        assert_eq!(
            report["steam"]["images"][0]["reason"],
            "host_warmup_disabled"
        );
        policy.apply(&snapshot("2", false));
        assert!(policy.authorize(&image, None, true).is_err());
        assert_eq!(
            policy.report().unwrap()["steam"]["images"][0]["reason"],
            "source_disabled"
        );
    }
    #[test]
    fn disable_and_root_change_between_work_and_commit_cannot_publish_or_seed() {
        let (dir, policy) = fixture();
        policy.apply(&snapshot("1", true));
        let image = identity();
        for consume in [true, false] {
            let lease = policy.authorize(&image, Some("1"), consume).unwrap();
            policy.apply(&snapshot("2", false));
            assert!(lease
                .commit(|| -> std::io::Result<()> { panic!("a stale commit must never execute") })
                .is_err());
            policy.apply(&snapshot("3", true));
            let lease = policy.authorize(&image, Some("3"), consume).unwrap();
            let root = dir.path().join(if consume {
                "new-homes-one"
            } else {
                "new-homes-two"
            });
            std::fs::create_dir(&root).unwrap();
            policy.update_root(root.to_str().unwrap());
            assert!(lease.commit(|| Ok(())).is_err());
            // Fresh connection is the only mechanism allowed to reset revision.
            {
                let mut state = policy.state.lock().unwrap();
                state.snapshot = None;
            }
            policy.apply(&snapshot("1", true));
        }
    }
    #[test]
    fn status_requires_matching_published_template_and_reports_real_clone_mode() {
        let (_dir, policy) = fixture();
        policy.apply(&snapshot("1", true));
        let image = identity();
        let store = policy.store().unwrap();
        let build = store.begin_build("steam", "1").unwrap();
        std::fs::write(build.home_dir().join("steam.sh"), "public bootstrap").unwrap();
        store
            .publish(
                build,
                crate::session::template::TemplateMeta {
                    image_id: "steam".into(),
                    registry_ref: image.registry_ref.clone(),
                    version: "1".into(),
                    digest: String::new(),
                    built_at: 1,
                    bytes: 16,
                    files: 1,
                    agent_version: "test".into(),
                    schema: 1,
                },
            )
            .unwrap();
        assert!(policy.seed("steam", &image.registry_ref).is_some());
        let homes = policy.state.lock().unwrap().home_root.clone();
        for user in ["one", "two"] {
            let (store, seed, lease) = policy.seed("steam", &image.registry_ref).unwrap();
            let mount = format!("{homes}/{user}:/home/steam:rw");
            crate::session::home::provision_home_dirs(
                &[mount],
                &homes,
                Some(crate::session::home::TemplateSeeder {
                    store: &store,
                    seed: &seed,
                    authorization: Some(&lease),
                }),
            );
            assert_eq!(
                std::fs::read_to_string(format!("{homes}/{user}/steam.sh")).unwrap(),
                "public bootstrap"
            );
        }
        std::fs::write(format!("{homes}/one/steam.sh"), "private user edit").unwrap();
        assert_eq!(
            std::fs::read_to_string(format!("{homes}/two/steam.sh")).unwrap(),
            "public bootstrap"
        );
        let (seed_store, seed, lease) = policy.seed("steam", &image.registry_ref).unwrap();
        policy.apply(&snapshot("2", false));
        crate::session::home::provision_home_dirs(
            &[format!("{homes}/cold:/home/steam:rw")],
            &homes,
            Some(crate::session::home::TemplateSeeder {
                store: &seed_store,
                seed: &seed,
                authorization: Some(&lease),
            }),
        );
        assert!(std::path::Path::new(&format!("{homes}/cold"))
            .read_dir()
            .unwrap()
            .next()
            .is_none());
        policy.apply(&snapshot("3", true));

        let report = policy.report().unwrap();
        assert_eq!(report["steam"]["images"][0]["state"], "ready");
        assert!(matches!(
            report["steam"]["images"][0]["clone_mode"].as_str(),
            Some("reflink" | "copy")
        ));
        assert!(policy.seed("custom", &image.registry_ref).is_none());
        let mut next = snapshot("4", true);
        next["steam_preparation"]["images"][0]["version"] = json!("2");
        policy.apply(&next);
        assert!(policy.seed("steam", &image.registry_ref).is_none());
        assert_eq!(
            policy.report().unwrap()["steam"]["images"][0]["state"],
            "waiting_image"
        );
        assert!(
            policy.report().unwrap()["steam"]["images"][0]["template"].is_null(),
            "stale template must not prevent acknowledgement of a new identity"
        );
    }
}
