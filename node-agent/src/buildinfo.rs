//! What this agent binary IS, and how this host installed it.
//!
//! Two halves, because they are learned differently. The **build stamps**
//! (`SOURCE_COMMIT`, `BUILT_AT`) come from `build.rs` at compile time; the
//! **install mode** and **updater presence** are discovered at run time: from
//! the recovery actor on an owned install, else from the agent's own container
//! image through the Quasar runtime API. All four
//! ride the optional identity fields on `register` (agent-api.md), and the
//! control plane stores them wholesale — an absent field is stored NULL, so
//! reporting nothing is always safe and never a lie.

use tracing::{debug, info, warn};

use crate::session::container::ContainerRuntime;

/// Release identity comes from the same tag stamp as the control plane. Branch
/// builds stay `dev`; the Cargo package version is not a platform release.
pub fn version() -> &'static str {
    normalized_version(env!("QUASAR_STAMP_VERSION"))
}

fn normalized_version(raw: &str) -> &str {
    let value = raw.trim().strip_prefix('v').unwrap_or(raw.trim());
    if value.is_empty() || value == "unknown" {
        "dev"
    } else {
        value
    }
}

#[cfg(test)]
mod version_tests {
    #[test]
    fn release_and_source_versions_are_honest() {
        for (raw, expected) in [
            ("v0.2.4", "0.2.4"),
            ("0.2.4", "0.2.4"),
            ("v0.2.4-rc.1", "0.2.4-rc.1"),
            (" v0.2.4 ", "0.2.4"),
            ("", "dev"),
            ("  ", "dev"),
            ("unknown", "dev"),
        ] {
            assert_eq!(super::normalized_version(raw), expected);
        }
    }
}

/// Compile-time stamps from `build.rs`. Literally `"unknown"` on a build that
/// had neither the env vars nor a git checkout.
const STAMP_SOURCE_COMMIT: &str = env!("QUASAR_STAMP_SOURCE_COMMIT");

/// The commit this binary was built from, or None when unstamped.
/// 7-40 lowercase hex is the wire's accepted shape; anything else is dropped
/// here rather than sent for the control plane to reject.
pub fn source_commit() -> Option<&'static str> {
    let c = STAMP_SOURCE_COMMIT;
    let hex = (7..=40).contains(&c.len())
        && c.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
    hex.then_some(c)
}

/// When this binary's image was built (RFC3339), or None when unstamped. Read
/// once from the image's `QUASAR_BUILT_AT` ENV (deploy/Dockerfile.vulkan, runtime
/// stage), not compiled in: a wall-clock compile stamp made every build of an
/// unchanged tree recompile the agent (#866). Not parsed: the build passes
/// through whatever the image build recorded, and the control plane validates.
pub fn built_at() -> Option<&'static str> {
    static BUILT_AT: std::sync::OnceLock<Option<String>> = std::sync::OnceLock::new();
    BUILT_AT
        .get_or_init(|| built_at_from(std::env::var("QUASAR_BUILT_AT").ok()))
        .as_deref()
}

fn built_at_from(raw: Option<String>) -> Option<String> {
    raw.map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty() && v != "unknown")
}

/// How this host got its platform images (`CONTEXT.md` "Install mode").
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InstallMode {
    /// Running a published image: the reference names a registry host, or is
    /// pinned to a digest.
    Registry,
    /// Built on the host: a bare local tag like `quasar-node-agent:latest`.
    Source,
    /// Created and replaced by this machine's recovery actor (amendment 14).
    Owned,
}

impl InstallMode {
    pub fn as_str(self) -> &'static str {
        match self {
            InstallMode::Registry => "registry",
            InstallMode::Source => "source",
            InstallMode::Owned => "owned",
        }
    }
}

/// What this host's own container says about its installation. Every field is
/// optional because discovery is best-effort: absent means "could not tell",
/// which the wire and the schema both model as unknown.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct InstallFacts {
    /// Amendment 17: the engine this agent drives, whatever the install mode.
    pub engine: EngineIdentity,
    pub install_mode: Option<InstallMode>,
    pub updater_present: Option<bool>,
    /// Owned installs only: what the recovery actor said about itself and the seed.
    pub recovery_actor_version: Option<String>,
    pub recovery_actor_source_commit: Option<String>,
    pub seed_version: Option<String>,
}

/// The container engine and engine mode, as the amendment-17 `register` fields carry
/// them. Every field is `None` when the engine could not be inspected, or cannot be
/// named: the wire then reads "engine unknown", never a guess.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct EngineIdentity {
    pub engine: Option<String>,
    pub engine_version: Option<String>,
    pub engine_mode: Option<String>,
}

impl EngineIdentity {
    /// From one engine inspection. An engine this runtime cannot name reports nothing at
    /// all, so a version or mode is never attributed to the wrong engine.
    pub fn from_facts(facts: &crate::runtime::EngineFacts) -> Self {
        let Some(engine) = facts.info.kind.wire() else {
            return Self::default();
        };
        let version = &facts.info.version;
        let version_ok = (1..=64).contains(&version.len())
            && version.bytes().all(|b| (0x21..=0x7e).contains(&b));
        Self {
            engine: Some(engine.to_string()),
            engine_version: version_ok.then(|| version.clone()),
            engine_mode: Some(facts.mode.wire().to_string()),
        }
    }

    /// One bounded inspection of the configured engine; unknown on any failure.
    pub fn observe() -> Self {
        crate::runtime::configured()
            .ok()
            .and_then(|client| client.inspect_engine().wait().ok())
            .map(|facts| Self::from_facts(&facts))
            .unwrap_or_default()
    }
}

/// Set by the recovery actor's recipe: the agent socket, whose presence in the
/// environment is what makes this an owned install. Unset, this host has no recovery actor.
pub use quasar_runtime::owned_install::AGENT_SOCKET_ENV as RECOVERY_SOCKET_ENV;

/// Well inside the actor's own status deadline plus one engine round trip.
const ACTOR_STATUS_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(8);

/// How long the work before `register` waits for the recovery actor, in all. A dark actor
/// never holds the dial: the agent registers without it and the readiness refresh reports
/// it once it answers ([`owned_identity_changed`]). Clearly above the actor's own engine
/// deadline (`STATUS_ENGINE_DEADLINE` in `quasar-recovery`, 3 s), so a healthy actor with a
/// slow engine still answers inside it.
pub const ACTOR_REGISTER_BUDGET: std::time::Duration = std::time::Duration::from_secs(5);

/// The part of the actor's `GET /v1/status` answer the agent reads; everything else is
/// ignored, since the actor moves first and may be a release ahead
/// (`testdata/recovery/socket/README.md`).
#[derive(serde::Deserialize)]
struct ActorStatus {
    actor: ActorSelf,
    #[serde(default)]
    seed: Option<SeedSelf>,
    #[serde(default)]
    conflicts: Vec<crate::readiness::owner_conflict::Conflict>,
}

#[derive(serde::Deserialize)]
struct ActorSelf {
    version: String,
    commit: String,
}

#[derive(serde::Deserialize)]
struct SeedSelf {
    version: String,
}

fn source_commit_shape(c: &str) -> bool {
    (7..=40).contains(&c.len())
        && c.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

pub fn actor_version_shape(v: &str) -> bool {
    static RE: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
        regex::Regex::new(
            r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$"
        )
        .expect("actor_version_shape regex is invalid")
    });
    RE.is_match(v)
}

/// This host's install facts: the recovery actor's answer on an owned install (`socket`,
/// which the actor put in this container's environment as [`RECOVERY_SOCKET_ENV`]), waiting
/// at most `actor_timeout` for it, else what the agent's own container image says, with
/// no recovery actor.
pub fn discover(
    runtime: &ContainerRuntime,
    socket: Option<&std::path::Path>,
    actor_timeout: std::time::Duration,
) -> InstallFacts {
    discover_observed(runtime, socket, actor_timeout).0
}

/// [`discover`], plus whether the recovery actor's own read missed (`false` with no
/// actor): the facts may still carry an older answer of its in place of that read.
pub fn discover_observed(
    runtime: &ContainerRuntime,
    socket: Option<&std::path::Path>,
    actor_timeout: std::time::Duration,
) -> (InstallFacts, bool) {
    let mut actor_missed = false;
    let mut facts = match socket {
        Some(socket) => {
            let (read, _) = observe_owned_within(socket, actor_timeout);
            actor_missed = read.updater_present != Some(true);
            // A register read that missed must not be less informed than the refresh that
            // corrects it only because it waited less: that refresh would find the
            // registered facts different on every dial and re-dial for ever.
            match last_answer() {
                Some(seen) if actor_missed => seen,
                _ => read,
            }
        }
        None => discover_install(&DockerFacts::new(runtime)),
    };
    facts.engine = EngineIdentity::observe();
    (facts, actor_missed)
}

/// An owned install asks its recovery actor over the agent socket. `updater_present` is
/// whether the actor answered (amendment 14); the actor's fields go absent when it did not.
pub fn discover_owned(socket: &std::path::Path) -> InstallFacts {
    observe_owned(socket).0
}

/// [`discover_owned`], plus the owner conflicts the actor reports (`None` when it did not
/// answer), from the same one status read.
pub fn observe_owned(
    socket: &std::path::Path,
) -> (InstallFacts, crate::readiness::owner_conflict::Observed) {
    observe_owned_within(socket, ACTOR_STATUS_TIMEOUT)
}

fn observe_owned_within(
    socket: &std::path::Path,
    timeout: std::time::Duration,
) -> (InstallFacts, crate::readiness::owner_conflict::Observed) {
    let mut conflicts = None;
    let mut out = InstallFacts {
        install_mode: Some(InstallMode::Owned),
        updater_present: Some(false),
        ..Default::default()
    };
    let answer = crate::release::unix_http::request(socket, "GET", "/v1/status", None, timeout);
    match answer {
        Ok(reply) if reply.status == 200 => match serde_json::from_str::<ActorStatus>(&reply.body)
        {
            Ok(status) => {
                out.updater_present = Some(true);
                out.recovery_actor_version =
                    Some(status.actor.version).filter(|v| !v.trim().is_empty());
                out.recovery_actor_source_commit =
                    Some(status.actor.commit).filter(|c| source_commit_shape(c));
                out.seed_version = status
                    .seed
                    .map(|s| s.version)
                    .filter(|v| !v.trim().is_empty());
                conflicts = Some(status.conflicts);
            }
            Err(e) => warn!(
                token = "install-actor-status-unreadable",
                "install discovery: the recovery actor's status at {} is unreadable: {e}",
                socket.display()
            ),
        },
        Ok(reply) => warn!(
            token = "install-actor-status-refused",
            "install discovery: the recovery actor at {} answered HTTP {}",
            socket.display(),
            reply.status
        ),
        Err(e) => warn!(
            token = "install-actor-unreachable",
            "install discovery: no answer from the recovery actor at {}: {e}; this host reports updater_present=false until it answers",
            socket.display()
        ),
    }
    (out, conflicts)
}

/// The owned install's socket, when this agent's container has one.
pub fn owned_socket() -> Option<std::path::PathBuf> {
    std::env::var(RECOVERY_SOCKET_ENV)
        .ok()
        .map(|s| s.trim().to_owned())
        .filter(|s| !s.is_empty())
        .map(std::path::PathBuf::from)
}

/// The last owned-install facts a readiness refresh read, how many refreshes in a row read
/// them, and when the latest of those reads happened.
static OBSERVED: std::sync::Mutex<Option<(InstallFacts, u32, std::time::Instant)>> =
    std::sync::Mutex::new(None);

/// What the last readiness refresh read, when the actor answered it and that was no longer
/// ago than one refresh interval. Older, the process cannot say the actor is still there:
/// the refresh stops with the connection, and a reconnect would otherwise register a dead or
/// replaced actor as present at its old version and commit.
fn last_answer() -> Option<InstallFacts> {
    last_answer_at(std::time::Instant::now())
}

fn last_answer_at(now: std::time::Instant) -> Option<InstallFacts> {
    let slot = OBSERVED.lock().ok()?;
    let (seen, _, at) = slot.as_ref()?;
    (seen.updater_present == Some(true)
        && now.saturating_duration_since(*at) < crate::agent::READINESS_REFRESH_INTERVAL)
        .then(|| seen.clone())
}

#[cfg(test)]
pub(crate) fn forget_observed() {
    *OBSERVED.lock().unwrap() = None;
}

/// Records what a refresh read from the recovery actor. A run of equal reads keeps the
/// first one's facts and counts up; every read stamps the time.
pub fn note_observed(facts: &InstallFacts) {
    note_observed_at(facts, std::time::Instant::now());
}

fn note_observed_at(facts: &InstallFacts, at: std::time::Instant) {
    if let Ok(mut slot) = OBSERVED.lock() {
        *slot = match slot.take() {
            Some((last, n, _)) if !actor_identity_differs(&last, facts) => Some((last, n + 1, at)),
            _ => Some((facts.clone(), 1, at)),
        };
    }
}

/// The actor's reported identity (its version, commit, the seed it sees, whether it
/// answers) has differed from what this connection registered for two refreshes in a row.
/// `register` is the only way those fields reach the control plane (agent-api.md
/// §register, "Owned installs"), so the agent re-dials to report them; two in a row keeps
/// one slow answer from costing a reconnect.
pub fn owned_identity_changed() -> bool {
    let registered = install_facts();
    if registered.install_mode != Some(InstallMode::Owned) {
        return false;
    }
    OBSERVED
        .lock()
        .ok()
        .and_then(|slot| slot.clone())
        .is_some_and(|(seen, n, _)| n >= 2 && actor_identity_differs(&seen, &registered))
}

/// Whether the recovery actor's part of two install observations differs: the fields a
/// refresh reads from the actor. The engine facts are read elsewhere (and not by the
/// refresh), so they never count here; comparing them made every owned install whose
/// engine answered look changed forever and reconnect in a loop (#396 review).
pub fn actor_identity_differs(seen: &InstallFacts, registered: &InstallFacts) -> bool {
    seen.install_mode != registered.install_mode
        || seen.updater_present != registered.updater_present
        || seen.recovery_actor_version != registered.recovery_actor_version
        || seen.recovery_actor_source_commit != registered.recovery_actor_source_commit
        || seen.seed_version != registered.seed_version
}

/// Read-only facts installation discovery needs. Production uses the shared Quasar
/// runtime interface; the trait keeps classification independent of a daemon.
pub trait ContainerFacts {
    /// This container's own id, as docker would accept it.
    fn self_reference(&self) -> Option<String>;
    /// The configured image reference of one container, distinct from its image ID.
    fn image_reference(&self, container: &str) -> Option<String>;
}

/// Installation facts from the configured Quasar runtime API.
pub struct DockerFacts;

impl DockerFacts {
    pub fn new(_runtime: &ContainerRuntime) -> Self {
        Self
    }
}

impl ContainerFacts for DockerFacts {
    /// `/proc/self/mountinfo` first, `$HOSTNAME` only when it LOOKS like a
    /// container id. A compose stack that sets `hostname:` makes `$HOSTNAME` a
    /// DNS name (`quasar-dev.local`), and `docker inspect -- quasar-dev.local`
    /// answers "No such object" — which is how install_mode and
    /// updater_present came back unknown on a source-built host.
    fn self_reference(&self) -> Option<String> {
        crate::nvidia_volume::self_container_id()
    }

    fn image_reference(&self, container: &str) -> Option<String> {
        Some(
            crate::runtime::configured()
                .ok()?
                .inspect_container(container)
                .wait()
                .ok()??
                .configured_image,
        )
    }
}

/// Classify an image reference. Registry when it names a registry host
/// (`ghcr.io/...`, `localhost:5000/...`) or pins a digest; source when it is a
/// bare local tag like `quasar-node-agent:latest`.
///
/// The host test is docker's own: the first path segment is a registry only if
/// it contains a `.` or a `:`, or is exactly `localhost`. `library/foo:tag` and
/// `myorg/foo:tag` are Docker Hub references by that rule — which is right,
/// they were pulled, not built here.
pub fn classify_image_reference(reference: &str) -> Option<InstallMode> {
    let reference = reference.trim();
    if reference.is_empty() {
        return None;
    }
    if reference.contains("@sha256:") {
        // A digest pin can only have come from a registry (ADR 0001).
        return Some(InstallMode::Registry);
    }
    let first = reference.split('/').next().unwrap_or_default();
    let has_host = reference.contains('/')
        && (first == "localhost" || first.contains('.') || first.contains(':'));
    Some(if has_host {
        InstallMode::Registry
    } else {
        InstallMode::Source
    })
}

/// Learn the install mode of a host with no recovery actor from its own container.
///
/// Such a host has nothing that can replace its containers (the Compose updater is
/// retired), so `updater_present` is a definite `false`. An unreadable image
/// reference leaves `install_mode` absent. Nothing here can fail a registration.
pub fn discover_install(facts: &dyn ContainerFacts) -> InstallFacts {
    let mut out = InstallFacts {
        updater_present: Some(false),
        ..Default::default()
    };

    // Every failure below is INFO, not debug: identity-unknown is a state a host
    // operator has to be able to explain, and the reason is only ever visible here.
    let Some(me) = facts.self_reference() else {
        info!(
            "install discovery: could not determine this process's own container id \
             (/proc/self/mountinfo carries none and $HOSTNAME is not a container id); \
             install mode stays unknown"
        );
        return out;
    };

    match facts.image_reference(&me) {
        Some(reference) => {
            out.install_mode = classify_image_reference(&reference);
            debug!(
                "install discovery: image {reference} => {:?}",
                out.install_mode
            );
        }
        None => info!(
            "install discovery: could not read container {me}'s image reference; \
             install mode stays unknown"
        ),
    }

    out
}

/// Re-discovered before every `register`, not once at boot: a boot-time
/// snapshot would pin an owned host's `updater_present=false` when its recovery
/// actor answered after the agent started. Unset reads
/// as "nothing discovered", the correct answer for the standalone session
/// subcommands that never register.
static INSTALL_FACTS: std::sync::RwLock<Option<InstallFacts>> = std::sync::RwLock::new(None);

/// Held by every test that writes [`INSTALL_FACTS`] and then reads it back, so two
/// such tests never interleave.
#[cfg(test)]
pub(crate) static INSTALL_FACTS_TEST_LOCK: tokio::sync::Mutex<()> =
    tokio::sync::Mutex::const_new(());

/// Record this process's discovered install facts, replacing any earlier answer.
pub fn set_install_facts(facts: InstallFacts) {
    if let Ok(mut slot) = INSTALL_FACTS.write() {
        *slot = Some(facts);
    }
}

/// The last discovered install facts, or all-unknown when discovery never ran
/// or found nothing.
pub fn install_facts() -> InstallFacts {
    INSTALL_FACTS
        .read()
        .ok()
        .and_then(|slot| slot.clone())
        .unwrap_or_default()
}

/// Log what this binary is, and warn once when it does not know — an
/// unstamped agent can never be given a platform release, and an operator
/// should learn that from the log rather than from an empty column.
pub fn log_startup_identity(facts: &InstallFacts) {
    info!(
        "build identity: version={} source_commit={} built_at={} install_mode={} updater_present={}",
        version(),
        source_commit().unwrap_or("unknown"),
        built_at().unwrap_or("unknown"),
        facts.install_mode.map(InstallMode::as_str).unwrap_or("unknown"),
        facts
            .updater_present
            .map(|p| if p { "yes" } else { "no" })
            .unwrap_or("unknown"),
    );
    if facts.install_mode == Some(InstallMode::Owned) {
        info!(
            "recovery actor: version={} source_commit={} seed_version={}",
            facts.recovery_actor_version.as_deref().unwrap_or("unknown"),
            facts
                .recovery_actor_source_commit
                .as_deref()
                .unwrap_or("unknown"),
            facts.seed_version.as_deref().unwrap_or("none"),
        );
    }
    if source_commit().is_none() || built_at().is_none() {
        warn!(
            token = "buildinfo-unstamped",
            "this agent binary carries no build stamps; the host will read as identity-unknown \
             and is never eligible for a platform-release apply"
        );
    }
}

#[cfg(test)]
mod tests {
    /// #396 review: the engine facts a connection registered are not part of what the
    /// recovery actor reports, so they never make an owned install look changed.
    #[test]
    fn engine_facts_never_count_as_an_actor_identity_change() {
        let actor = InstallFacts {
            install_mode: Some(InstallMode::Owned),
            updater_present: Some(true),
            recovery_actor_version: Some("0.4.0".into()),
            recovery_actor_source_commit: Some("cccccccccccccccccccccccccccccccccccccccc".into()),
            seed_version: Some("0.4.0".into()),
            ..Default::default()
        };
        let registered = InstallFacts {
            engine: EngineIdentity {
                engine: Some("podman".into()),
                engine_version: Some("5.8.4".into()),
                engine_mode: Some("rootless".into()),
            },
            ..actor.clone()
        };
        assert!(!actor_identity_differs(&actor, &registered));
        let moved = InstallFacts {
            recovery_actor_version: Some("0.5.0".into()),
            ..actor.clone()
        };
        assert!(actor_identity_differs(&moved, &registered));
    }

    /// Amendment 17: the wire carries only what the engine really said. An engine that
    /// cannot be named sends nothing, and a version outside the contract's shape is
    /// dropped rather than sent.
    #[test]
    fn engine_identity_carries_only_what_the_contract_accepts() {
        use crate::runtime::{ApiVersion, EngineFacts, EngineInfo, EngineKind, EngineMode};
        let api = ApiVersion {
            major: 1,
            minor: 44,
        };
        let facts = |kind, version: &str, mode| EngineFacts {
            info: EngineInfo {
                kind,
                name: "x".into(),
                version: version.into(),
                api_version: api,
                server_min_api: api,
                server_max_api: api,
            },
            mode,
            operating_system: None,
            os_version: None,
            architecture: None,
            cgroup_version: None,
            cgroup_driver: None,
            security_options: Vec::new(),
            runtimes: Vec::new(),
            default_runtime: None,
            cdi: None,
        };
        let id =
            EngineIdentity::from_facts(&facts(EngineKind::Podman, "5.8.4", EngineMode::Rootless));
        assert_eq!(id.engine.as_deref(), Some("podman"));
        assert_eq!(id.engine_version.as_deref(), Some("5.8.4"));
        assert_eq!(id.engine_mode.as_deref(), Some("rootless"));
        assert_eq!(
            EngineIdentity::from_facts(&facts(EngineKind::Unknown, "1", EngineMode::Rootful)),
            EngineIdentity::default()
        );
        let spaced =
            EngineIdentity::from_facts(&facts(EngineKind::Docker, "29 beta", EngineMode::Rootful));
        assert_eq!(spaced.engine.as_deref(), Some("docker"));
        assert_eq!(spaced.engine_version, None);
    }

    use super::*;

    #[derive(Default)]
    struct FakeFacts {
        self_ref: Option<String>,
        image: Option<String>,
    }

    impl ContainerFacts for FakeFacts {
        fn self_reference(&self) -> Option<String> {
            self.self_ref.clone()
        }
        fn image_reference(&self, _c: &str) -> Option<String> {
            self.image.clone()
        }
    }

    #[test]
    fn registry_references_are_recognised_by_host_or_digest() {
        for reference in [
            "ghcr.io/accreleus/quasar/quasar-node-agent:latest",
            "ghcr.io/accreleus/quasar/quasar-node-agent@sha256:abc",
            "quasar-node-agent@sha256:abc",
            "localhost:5000/quasar-node-agent:dev",
            "docker.io/library/postgres:16",
        ] {
            assert_eq!(
                classify_image_reference(reference),
                Some(InstallMode::Registry),
                "{reference}"
            );
        }
    }

    #[test]
    fn bare_local_tags_are_source_builds() {
        for reference in [
            "quasar-node-agent:latest",
            "quasar-node-agent",
            "quasar-vulkan:prev",
        ] {
            assert_eq!(
                classify_image_reference(reference),
                Some(InstallMode::Source),
                "{reference}"
            );
        }
    }

    #[test]
    fn an_empty_reference_says_nothing() {
        assert_eq!(classify_image_reference("   "), None);
    }

    // A host with no recovery actor has nothing to replace its containers:
    // `updater_present` is a definite false, never absent.
    #[test]
    fn a_host_with_no_recovery_actor_reports_its_image_and_no_updater() {
        for (image, mode) in [
            (
                "ghcr.io/accreleus/quasar/quasar-node-agent:latest",
                InstallMode::Registry,
            ),
            ("quasar-node-agent:latest", InstallMode::Source),
        ] {
            let facts = FakeFacts {
                self_ref: Some("abc123".into()),
                image: Some(image.into()),
            };
            assert_eq!(
                discover_install(&facts),
                InstallFacts {
                    install_mode: Some(mode),
                    updater_present: Some(false),
                    ..Default::default()
                }
            );
        }
    }

    /// A compose stack that sets `hostname:` gives the agent container a DNS
    /// name, not its id — `docker inspect -- quasar-dev.local` then answers "No
    /// such object" and identity comes back unknown on a perfectly healthy
    /// host. `self_container_id` reads /proc/self/mountinfo first and accepts
    /// `$HOSTNAME` only when it looks like an id.
    #[test]
    fn a_hostname_that_is_not_a_container_id_is_never_used_as_one() {
        // The live case: compose sets `hostname:` on this stack, so $HOSTNAME is
        // a DNS name and `docker inspect -- quasar-dev.local` answers "No such
        // object" — which is exactly how install_mode and updater_present came
        // back unknown on a healthy source-built host.
        for hostname in ["quasar-dev.local", "gpu-host-01", "abc123", ""] {
            assert!(
                !crate::nvidia_volume::hostname_is_container_id(hostname),
                "{hostname} would have been handed to `docker inspect` as an id"
            );
        }
        for hostname in ["0123456789ab", "0123456789abcdef0123456789abcdef01234567"] {
            assert!(crate::nvidia_volume::hostname_is_container_id(hostname));
        }
    }

    #[test]
    fn the_container_id_comes_from_mountinfo_before_any_hostname() {
        let id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        let body =
            format!("2079 1856 0:132 /var/lib/docker/containers/{id}/hostname /etc/hostname rw\n");
        assert_eq!(
            crate::nvidia_volume::parse_container_id_from_mountinfo(&body).as_deref(),
            Some(id)
        );
    }

    #[test]
    fn no_self_reference_leaves_the_install_mode_unknown() {
        assert_eq!(
            discover_install(&FakeFacts::default()),
            InstallFacts {
                updater_present: Some(false),
                ..Default::default()
            }
        );
    }

    // Not a stamped build, so the constants must read as absent rather than as
    // the literal "unknown" reaching the wire.
    #[test]
    fn an_unknown_stamp_is_absent_not_the_word_unknown() {
        if STAMP_SOURCE_COMMIT == "unknown" {
            assert_eq!(source_commit(), None);
        }
    }

    #[test]
    fn built_at_comes_from_the_image_env_and_unknown_is_absent() {
        assert_eq!(
            built_at_from(Some(" 2026-10-27T10:00:00Z ".into())).as_deref(),
            Some("2026-10-27T10:00:00Z")
        );
        for raw in [
            None,
            Some(String::new()),
            Some(" ".into()),
            Some("unknown".into()),
        ] {
            assert_eq!(built_at_from(raw), None);
        }
    }

    /// A recovery actor on a real unix socket answering one fixed reply per connection.
    fn actor_answering(status: u16, body: String) -> (tempfile::TempDir, std::path::PathBuf) {
        use std::io::{Read, Write};
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("agent.sock");
        let listener = std::os::unix::net::UnixListener::bind(&socket).unwrap();
        std::thread::spawn(move || {
            for conn in listener.incoming() {
                let Ok(mut conn) = conn else { return };
                let mut head = Vec::new();
                let mut byte = [0u8; 1];
                while !head.ends_with(b"\r\n\r\n") && conn.read(&mut byte).unwrap_or(0) == 1 {
                    head.push(byte[0]);
                }
                let _ = write!(
                    conn,
                    "HTTP/1.1 {status} X\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
            }
        });
        (dir, socket)
    }

    fn socket_fixture(name: &str) -> String {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../testdata/recovery/socket")
            .join(name);
        let fixture: serde_json::Value =
            serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        fixture["body"].to_string()
    }

    /// A paused actor accepts the connection and never answers: discovery gives up at its
    /// budget, as an owned install whose actor does not answer, and the actor's answer
    /// reaching the control plane later is `owned_identity_changed` (two refreshes in a row).
    #[test]
    fn a_dark_actor_costs_discovery_its_budget_and_is_reported_when_it_answers() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("dark.sock");
        let _dark = std::os::unix::net::UnixListener::bind(&socket).unwrap();
        let started = std::time::Instant::now();
        let (registered, _) = observe_owned_within(&socket, std::time::Duration::from_millis(300));
        assert!(started.elapsed() < std::time::Duration::from_secs(3));
        assert_eq!(registered.install_mode, Some(InstallMode::Owned));
        assert_eq!(registered.updater_present, Some(false));

        let _facts = INSTALL_FACTS_TEST_LOCK.blocking_lock();
        forget_observed();
        set_install_facts(registered);
        let (_d, answering) = actor_answering(200, socket_fixture("status-gpu-host-applying.json"));
        let (seen, _) = observe_owned(&answering);
        note_observed(&seen);
        assert!(!owned_identity_changed(), "one answer is not yet a change");
        note_observed(&seen);
        assert!(owned_identity_changed());
    }

    /// An actor that answers after the register budget but inside the refresh's wait: the
    /// first dial registers it absent, the refresh sees it twice and the agent re-dials
    /// once; that dial carries what the refresh saw, so nothing differs and it stays put.
    #[test]
    fn an_actor_slower_than_the_register_budget_costs_one_redial_not_a_loop() {
        use std::io::{Read, Write};
        let _facts = INSTALL_FACTS_TEST_LOCK.blocking_lock();
        forget_observed();
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("slow.sock");
        let listener = std::os::unix::net::UnixListener::bind(&socket).unwrap();
        let body = socket_fixture("status-gpu-host-applying.json");
        std::thread::spawn(move || {
            for conn in listener.incoming() {
                let Ok(mut conn) = conn else { return };
                let body = body.clone();
                std::thread::spawn(move || {
                    let mut head = Vec::new();
                    let mut byte = [0u8; 1];
                    while !head.ends_with(b"\r\n\r\n") && conn.read(&mut byte).unwrap_or(0) == 1 {
                        head.push(byte[0]);
                    }
                    std::thread::sleep(std::time::Duration::from_millis(400));
                    let _ = write!(
                        conn,
                        "HTTP/1.1 200 X\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                        body.len()
                    );
                });
            }
        });
        let budget = std::time::Duration::from_millis(100);
        let runtime = ContainerRuntime::from_env();

        set_install_facts(discover(&runtime, Some(&socket), budget));
        assert_eq!(install_facts().updater_present, Some(false));
        for _ in 0..2 {
            note_observed(&observe_owned(&socket).0);
        }
        assert!(
            owned_identity_changed(),
            "the refresh sees what register missed"
        );

        set_install_facts(discover(&runtime, Some(&socket), budget));
        assert_eq!(install_facts().updater_present, Some(true));
        assert!(
            !owned_identity_changed(),
            "the re-dial registers what the refresh saw, so it is not a change again"
        );
        forget_observed();
    }

    /// A reconnect of a long-lived process must not register an actor nobody has seen lately.
    #[test]
    fn an_observation_older_than_a_refresh_interval_is_not_reused() {
        use crate::agent::READINESS_REFRESH_INTERVAL as INTERVAL;
        let second = std::time::Duration::from_secs(1);
        let _facts = INSTALL_FACTS_TEST_LOCK.blocking_lock();
        forget_observed();
        let (_d, answering) = actor_answering(200, socket_fixture("status-gpu-host-applying.json"));
        let (seen, _) = observe_owned(&answering);

        let t0 = std::time::Instant::now();
        note_observed_at(&seen, t0);
        assert_eq!(last_answer_at(t0 + second), Some(seen.clone()));
        assert_eq!(last_answer_at(t0 + INTERVAL + second), None);

        // A matching streak keeps its first facts and count, and every read re-stamps.
        note_observed_at(&seen, t0 + INTERVAL);
        assert_eq!(last_answer_at(t0 + INTERVAL + second), Some(seen.clone()));
        assert_eq!(last_answer_at(t0 + INTERVAL * 2 + second), None);
        assert!(OBSERVED.lock().unwrap().as_ref().is_some_and(|o| o.1 == 2));

        // The same through `discover`: an actor that misses its dial is registered absent
        // once the snapshot has aged out, and as the snapshot while it is fresh.
        let dir = tempfile::tempdir().unwrap();
        let dark = dir.path().join("gone.sock");
        let runtime = ContainerRuntime::from_env();
        let budget = std::time::Duration::from_millis(100);
        forget_observed();
        note_observed(&seen);
        assert_eq!(
            discover(&runtime, Some(&dark), budget).updater_present,
            Some(true)
        );
        // The facts carry the fresh observation, but this read missed and says so.
        let (facts, missed) = discover_observed(&runtime, Some(&dark), budget);
        assert_eq!(facts.updater_present, Some(true));
        assert!(missed, "a dark actor is dark whatever the fallback holds");
        let (_, missed) = discover_observed(&runtime, Some(&answering), budget);
        assert!(!missed);
        forget_observed();
        let old = std::time::Instant::now()
            .checked_sub(INTERVAL + second)
            .expect("the monotonic clock is older than a refresh interval");
        note_observed_at(&seen, old);
        let registered = discover(&runtime, Some(&dark), budget);
        assert_eq!(registered.updater_present, Some(false));
        assert_eq!(registered.recovery_actor_source_commit, None);
        forget_observed();
    }

    #[test]
    fn an_owned_install_reports_what_its_recovery_actor_says() {
        let (_dir, socket) = actor_answering(200, socket_fixture("status-gpu-host-applying.json"));
        assert_eq!(
            discover_owned(&socket),
            InstallFacts {
                install_mode: Some(InstallMode::Owned),
                updater_present: Some(true),
                recovery_actor_version: Some("0.4.0".into()),
                recovery_actor_source_commit: Some(
                    "cccccccccccccccccccccccccccccccccccccccc".into()
                ),
                seed_version: Some("0.4.0".into()),
                engine: Default::default(),
            }
        );
    }

    /// An actor that says nothing usable is still an owned install, with the actor's
    /// fields absent; `seed: null` is "no seed seen", not an error.
    #[test]
    fn an_owned_install_whose_actor_does_not_answer_reports_updater_present_false() {
        let dir = tempfile::tempdir().unwrap();
        let unreachable = InstallFacts {
            install_mode: Some(InstallMode::Owned),
            updater_present: Some(false),
            ..Default::default()
        };
        assert_eq!(discover_owned(&dir.path().join("absent.sock")), unreachable);
        let (_d, refusing) = actor_answering(500, "{}".into());
        assert_eq!(discover_owned(&refusing), unreachable);
        let (_d, garbled) = actor_answering(200, "not json".into());
        assert_eq!(discover_owned(&garbled), unreachable);

        let mut body: serde_json::Value =
            serde_json::from_str(&socket_fixture("status-control-only-stale.json")).unwrap();
        body["actor"]["commit"] = "unknown".into();
        body["actor"]["version"] = "dev".into();
        let (_d, dev) = actor_answering(200, body.to_string());
        assert_eq!(
            discover_owned(&dev),
            InstallFacts {
                install_mode: Some(InstallMode::Owned),
                updater_present: Some(true),
                recovery_actor_version: Some("dev".into()),
                ..Default::default()
            }
        );
    }

    #[test]
    fn actor_version_shape_accepts_semver_and_rejects_non_semver() {
        assert!(actor_version_shape("0.4.1"));
        assert!(actor_version_shape("1.0.0"));
        assert!(actor_version_shape("1.0.0-rc.1"));
        assert!(actor_version_shape("1.0.0-alpha"));
        assert!(actor_version_shape("1.0.0-0"));
        assert!(actor_version_shape("10.20.30"));
        assert!(actor_version_shape("0.0.0"));

        assert!(!actor_version_shape("dev"));
        assert!(!actor_version_shape("v0.4.1"));
        assert!(!actor_version_shape("0.4.1+meta"));
        assert!(!actor_version_shape("01.0.0"));
        assert!(!actor_version_shape("0.01.0"));
        assert!(!actor_version_shape("0.0.01"));
        assert!(!actor_version_shape(""));
        assert!(!actor_version_shape("0.4"));
    }
}
