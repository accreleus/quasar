//! What this agent binary IS, and how this host installed it.
//!
//! Two halves, because they are learned differently. The **build stamps**
//! (`SOURCE_COMMIT`, `BUILT_AT`) come from `build.rs` at compile time; the
//! **install mode** and **updater presence** are discovered at run time from
//! the agent's own container through the Quasar runtime API. All four
//! ride the optional identity fields on `register` (agent-api.md), and the
//! control plane stores them wholesale — an absent field is stored NULL, so
//! reporting nothing is always safe and never a lie.

use std::collections::BTreeMap;

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
const STAMP_BUILT_AT: &str = env!("QUASAR_STAMP_BUILT_AT");

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

/// When this binary was built (RFC3339), or None when unstamped. Not parsed:
/// the build passes through whatever the image build recorded, and the control
/// plane validates.
pub fn built_at() -> Option<&'static str> {
    (STAMP_BUILT_AT != "unknown" && !STAMP_BUILT_AT.is_empty()).then_some(STAMP_BUILT_AT)
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
    pub install_mode: Option<InstallMode>,
    pub updater_present: Option<bool>,
    /// Owned installs only: what the recovery actor said about itself and the seed.
    pub recovery_actor_version: Option<String>,
    pub recovery_actor_source_commit: Option<String>,
    pub seed_version: Option<String>,
}

/// Set by the recovery actor's recipe: the agent socket, whose presence in the
/// environment is what makes this an owned install. Unset, discovery is the Compose one.
pub use quasar_runtime::owned_install::AGENT_SOCKET_ENV as RECOVERY_SOCKET_ENV;

/// Well inside the actor's own status deadline plus one engine round trip.
const ACTOR_STATUS_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(8);

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

/// This host's install facts: the recovery actor's answer on an owned install (the
/// actor put [`RECOVERY_SOCKET_ENV`] in this container's environment), else Compose
/// discovery exactly as before.
pub fn discover(runtime: &ContainerRuntime) -> InstallFacts {
    match std::env::var(RECOVERY_SOCKET_ENV)
        .ok()
        .map(|s| s.trim().to_owned())
        .filter(|s| !s.is_empty())
    {
        Some(socket) => discover_owned(std::path::Path::new(&socket)),
        None => discover_install(&DockerFacts::new(runtime)),
    }
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
    let mut conflicts = None;
    let mut out = InstallFacts {
        install_mode: Some(InstallMode::Owned),
        updater_present: Some(false),
        ..Default::default()
    };
    let answer =
        crate::release::unix_http::request(socket, "GET", "/v1/status", None, ACTOR_STATUS_TIMEOUT);
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

/// The last owned-install facts a readiness refresh read, and how many refreshes in a row
/// read them.
static OBSERVED: std::sync::Mutex<Option<(InstallFacts, u32)>> = std::sync::Mutex::new(None);

/// Records what a refresh read from the recovery actor.
pub fn note_observed(facts: &InstallFacts) {
    if let Ok(mut slot) = OBSERVED.lock() {
        *slot = match slot.take() {
            Some((last, n)) if last == *facts => Some((last, n + 1)),
            _ => Some((facts.clone(), 1)),
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
        .is_some_and(|(seen, n)| n >= 2 && seen != registered)
}

/// The compose label naming a service within a project. The updater is found by
/// service name, not by container name, which an operator may rename freely.
const LABEL_SERVICE: &str = "com.docker.compose.service";
const LABEL_PROJECT: &str = "com.docker.compose.project";
/// The service name the updater is deployed under (`CONTEXT.md` "Updater").
const UPDATER_SERVICE: &str = "quasar-updater";

/// Read-only facts needed by installation discovery. Production uses the shared
/// Quasar runtime interface; the trait keeps classification independent of a daemon.
pub trait ContainerFacts {
    /// This container's own id, as docker would accept it.
    fn self_reference(&self) -> Option<String>;
    /// The configured image reference of one container, distinct from its image ID.
    fn image_reference(&self, container: &str) -> Option<String>;
    /// The compose labels on one container.
    fn labels(&self, container: &str) -> Option<BTreeMap<String, String>>;
    /// Compose service names of the RUNNING containers in one compose project.
    fn services_in_project(&self, project: &str) -> Option<Vec<String>>;
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

    fn labels(&self, container: &str) -> Option<BTreeMap<String, String>> {
        Some(
            crate::runtime::configured()
                .ok()?
                .inspect_container(container)
                .wait()
                .ok()??
                .labels,
        )
    }

    fn services_in_project(&self, project: &str) -> Option<Vec<String>> {
        Some(
            crate::runtime::configured()
                .ok()?
                .live_containers()
                .wait()
                .ok()?
                .into_iter()
                .filter(|container| {
                    container
                        .labels
                        .get(LABEL_PROJECT)
                        .is_some_and(|value| value == project)
                })
                .filter_map(|container| {
                    container
                        .labels
                        .get(LABEL_SERVICE)
                        .filter(|value| !value.is_empty())
                        .cloned()
                })
                .collect(),
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

/// Learn this host's install mode and updater presence from its own container.
///
/// Every step is independently optional: an unreadable image reference leaves
/// `install_mode` absent without costing the updater answer, and a container
/// with no compose project leaves `updater_present` absent (nothing can be said
/// about a stack that is not a stack). Nothing here can fail a registration.
pub fn discover_install(facts: &dyn ContainerFacts) -> InstallFacts {
    let mut out = InstallFacts::default();

    // Every failure below is INFO, not debug: identity-unknown is a state a host
    // operator has to be able to explain, and the reason is only ever visible here.
    let Some(me) = facts.self_reference() else {
        info!(
            "install discovery: could not determine this process's own container id \
             (/proc/self/mountinfo carries none and $HOSTNAME is not a container id); \
             install mode and updater presence stay unknown"
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

    match facts
        .labels(&me)
        .and_then(|l| l.get(LABEL_PROJECT).cloned())
    {
        Some(project) if !project.is_empty() => match facts.services_in_project(&project) {
            Some(services) => {
                out.updater_present = Some(services.iter().any(|s| s == UPDATER_SERVICE));
            }
            None => info!(
                "install discovery: could not list compose project {project}; \
                 updater presence stays unknown"
            ),
        },
        _ => info!(
            "install discovery: container {me} carries no {LABEL_PROJECT} label, \
             so it is not part of a compose stack; updater presence stays unknown"
        ),
    }

    out
}

/// Re-discovered before every `register`, not once at boot: a boot-time
/// snapshot pinned `updater_present=false` on a host whose updater started
/// after the agent, and the host then read as ineligible forever. Unset reads
/// as "nothing discovered", the correct answer for the standalone session
/// subcommands that never register.
static INSTALL_FACTS: std::sync::RwLock<Option<InstallFacts>> = std::sync::RwLock::new(None);

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
    use super::*;

    #[derive(Default)]
    struct FakeFacts {
        self_ref: Option<String>,
        image: Option<String>,
        labels: Option<BTreeMap<String, String>>,
        services: Option<Vec<String>>,
    }

    impl ContainerFacts for FakeFacts {
        fn self_reference(&self) -> Option<String> {
            self.self_ref.clone()
        }
        fn image_reference(&self, _c: &str) -> Option<String> {
            self.image.clone()
        }
        fn labels(&self, _c: &str) -> Option<BTreeMap<String, String>> {
            self.labels.clone()
        }
        fn services_in_project(&self, _p: &str) -> Option<Vec<String>> {
            self.services.clone()
        }
    }

    fn compose_labels(project: &str) -> BTreeMap<String, String> {
        BTreeMap::from([(LABEL_PROJECT.to_string(), project.to_string())])
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

    #[test]
    fn discovery_reports_both_facts_when_docker_answers() {
        let facts = FakeFacts {
            self_ref: Some("abc123".into()),
            image: Some("ghcr.io/accreleus/quasar/quasar-node-agent:latest".into()),
            labels: Some(compose_labels("quasar")),
            services: Some(vec!["quasar-node-agent".into(), UPDATER_SERVICE.into()]),
        };
        assert_eq!(
            discover_install(&facts),
            InstallFacts {
                install_mode: Some(InstallMode::Registry),
                updater_present: Some(true),
                ..Default::default()
            }
        );
    }

    // `false` is a real answer ("I looked, there is none") and must not be
    // collapsed into absent, which means "nobody has said".
    #[test]
    fn a_stack_without_an_updater_reports_false_not_absent() {
        let facts = FakeFacts {
            self_ref: Some("abc123".into()),
            image: Some("quasar-node-agent:latest".into()),
            labels: Some(compose_labels("quasar")),
            services: Some(vec!["quasar-node-agent".into()]),
        };
        assert_eq!(
            discover_install(&facts),
            InstallFacts {
                install_mode: Some(InstallMode::Source),
                updater_present: Some(false),
                ..Default::default()
            }
        );
    }

    #[test]
    fn no_compose_project_leaves_updater_unknown_without_costing_install_mode() {
        let facts = FakeFacts {
            self_ref: Some("abc123".into()),
            image: Some("quasar-node-agent:latest".into()),
            labels: Some(BTreeMap::new()),
            services: None,
        };
        assert_eq!(
            discover_install(&facts),
            InstallFacts {
                install_mode: Some(InstallMode::Source),
                updater_present: None,
                ..Default::default()
            }
        );
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
    fn no_self_reference_reports_nothing_at_all() {
        assert_eq!(
            discover_install(&FakeFacts::default()),
            InstallFacts::default()
        );
    }

    // Not a stamped build, so the constants must read as absent rather than as
    // the literal "unknown" reaching the wire.
    #[test]
    fn an_unknown_stamp_is_absent_not_the_word_unknown() {
        if STAMP_SOURCE_COMMIT == "unknown" {
            assert_eq!(source_commit(), None);
        }
        if STAMP_BUILT_AT == "unknown" {
            assert_eq!(built_at(), None);
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
}
