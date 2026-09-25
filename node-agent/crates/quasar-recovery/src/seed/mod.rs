//! The seed (ADR 0007): the one container an external manager or a `docker run` declares
//! for Quasar on a machine. It makes sure the machine's recovery actor exists and does
//! nothing else. Its whole interface, frozen as seed interface 1, is [`file`] (`seed.json`
//! format 1), the two labels below and [`profile`] (the compiled actor profile).
//!
//! Every [`INTERVAL`] it decides with [`decide`], which is pure and is what the contract
//! test (`tests/seed_contract.rs`) runs against the fixtures every released actor wrote:
//! - `seed.json` says `uninstalled`, has another format, or does not parse: idle;
//! - the only actor is one this seed created and never started: start it, finishing its
//!   own create (ADR 0007, "Finishing its own create");
//! - otherwise a container carrying both labels exists, in any state and under any name (a
//!   hand-over's kept and successor containers included): nothing;
//! - otherwise create the actor from the profile, from `seed.json`'s verified image or, on
//!   a first install (no `seed.json`), from the seed's own image with a new installation.
//!
//! It never stops, replaces or removes anything and never talks to the control plane.

pub mod file;
pub mod profile;

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use serde::Serialize;
use tracing::{error, info, warn};

use crate::actor::repository_of;
use crate::bootstrap::{self, Bootstrap};
use crate::engine::{Container, PlatformEngine};
use crate::recipe::ImageRef;
use crate::shutdown;
use file::{ActorImage, SeedRead, SeedState};

pub const INSTALLATION_LABEL: &str = "io.quasar.installation";
pub const PLATFORM_SERVICE_LABEL: &str = "io.quasar.platform-service";
pub const RECOVERY_ACTOR: &str = "recovery-actor";

pub const INTERVAL: Duration = Duration::from_secs(30);

/// What the seed does about one look at the machine.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "decision", rename_all = "snake_case")]
pub enum Decision {
    Uninstalled {
        installation_id: String,
    },
    UnknownFormat {
        format_version: String,
    },
    Unreadable {
        why: String,
    },
    /// The only recovery actor is this seed's own create, never started: start it.
    StartOwn {
        container: String,
    },
    /// A recovery actor of this installation exists.
    ActorPresent {
        container: String,
    },
    /// No actor, but a container holds the actor's name without its labels: never touched.
    NameTaken {
        container: String,
    },
    /// Create the actor. `None`, `None` is a first install: a new installation, from the
    /// seed's own image.
    Create {
        installation_id: Option<String>,
        image: Option<String>,
    },
}

/// A recovery actor of `installation`, or of any installation on a first install, when
/// no `seed.json` names one yet.
fn is_actor(c: &Container, installation: Option<&str>) -> bool {
    let labelled = c
        .labels
        .get(INSTALLATION_LABEL)
        .filter(|id| !id.trim().is_empty());
    c.labels.get(PLATFORM_SERVICE_LABEL).map(String::as_str) == Some(RECOVERY_ACTOR)
        && labelled.is_some_and(|id| installation.is_none_or(|want| id == want))
}

/// A container this seed created and never started. The profile stamps exactly the two
/// frozen labels in the `io.quasar.` namespace and this seed's full container id; every
/// actor container a recovery actor renders also carries its recipe and specification
/// labels, so a hand-over successor never matches, whatever it inherited.
fn is_own_unstarted(c: &Container, me: Option<&str>) -> bool {
    let Some(me) = me else { return false };
    let quasar_labels: Vec<&str> = c
        .labels
        .keys()
        .map(String::as_str)
        .filter(|k| k.starts_with("io.quasar."))
        .collect();
    c.name == profile::NAME
        && c.status == "created"
        && quasar_labels == [INSTALLATION_LABEL, PLATFORM_SERVICE_LABEL]
        && c.env
            .contains(&format!("{}={me}", profile::SEED_CONTAINER_ENV))
}

/// The seed container that created `c` and never started it, when `c` is such a create:
/// the shape [`is_own_unstarted`] recognises, whichever seed it names.
fn unstarted_create_of(c: &Container) -> Option<&str> {
    let seed = c
        .env
        .iter()
        .find_map(|kv| kv.strip_prefix(&format!("{}=", profile::SEED_CONTAINER_ENV)))?;
    is_own_unstarted(c, Some(seed)).then_some(seed)
}

/// `me` is the seed's own full container id, when known.
pub fn decide(read: &SeedRead, containers: &[Container], me: Option<&str>) -> Decision {
    let (installation, image) = match read {
        SeedRead::UnknownFormat(v) => {
            return Decision::UnknownFormat {
                format_version: v.clone(),
            }
        }
        SeedRead::Unreadable(why) => return Decision::Unreadable { why: why.clone() },
        SeedRead::Found(f) if f.state == SeedState::Uninstalled => {
            return Decision::Uninstalled {
                installation_id: f.installation_id.clone(),
            }
        }
        SeedRead::Found(f) => (
            Some(f.installation_id.as_str()),
            Some(f.recovery_actor_image.reference()),
        ),
        SeedRead::Missing => (None, None),
    };
    let actors: Vec<&Container> = containers
        .iter()
        .filter(|c| is_actor(c, installation))
        .collect();
    match actors.as_slice() {
        [only] if is_own_unstarted(only, me) => {
            return Decision::StartOwn {
                container: only.name.clone(),
            }
        }
        [first, ..] => {
            return Decision::ActorPresent {
                container: first.name.clone(),
            }
        }
        [] => {}
    }
    if let Some(taken) = containers.iter().find(|c| c.name == profile::NAME) {
        return Decision::NameTaken {
            container: taken.name.clone(),
        };
    }
    Decision::Create {
        installation_id: installation.map(str::to_owned),
        image,
    }
}

/// Whether `c` runs `quasar-recovery seed`.
pub fn is_seed(c: &Container) -> bool {
    let program = c.command.first().map(|p| p.rsplit('/').next().unwrap_or(p));
    program == Some("quasar-recovery") && c.command.get(1).map(String::as_str) == Some("seed")
}

/// The machine's seed, a running one first: the running seed, preferring the one that
/// created the actor (`hint`, from `QUASAR_SEED_CONTAINER`), else that one stopped, else any.
/// A manager redeploy gives the seed a new id, which is why the hint alone is not enough.
pub fn find<'a>(containers: &'a [Container], hint: Option<&str>) -> Option<&'a Container> {
    let seeds = || containers.iter().filter(|c| is_seed(c));
    let hinted = |c: &&Container| hint.is_some_and(|h| c.id == h || c.name == h);
    find_running(containers, hint)
        .or_else(|| seeds().find(hinted))
        .or_else(|| seeds().next())
}

/// The running seed, as the actor reports it. A stopped seed re-creates nothing, so it is
/// reported as no seed: the console's "not found" and its advice (start the seed again)
/// are then both true.
pub fn find_running<'a>(containers: &'a [Container], hint: Option<&str>) -> Option<&'a Container> {
    let running = || containers.iter().filter(|c| is_seed(c) && c.running);
    hint.and_then(|h| running().find(|c| c.id == h || c.name == h))
        .or_else(|| running().next())
}

/// What one look at the machine came to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    Present {
        container: String,
    },
    Created {
        id: String,
        image: String,
        installation_id: String,
    },
    /// Nothing is done until something outside the seed changes.
    Idle {
        token: &'static str,
        why: String,
    },
    /// Failed this time; the next look tries again.
    Retry {
        token: &'static str,
        why: String,
    },
}

impl Outcome {
    fn log(&self) {
        match self {
            Outcome::Present { container } => {
                info!(container = %container, "a recovery actor exists; nothing to do")
            }
            Outcome::Created {
                id,
                image,
                installation_id,
            } => info!(
                token = "seed-actor-created",
                id = %id,
                image = %image,
                installation = %installation_id,
                "created and started the recovery actor"
            ),
            // One literal per token: node-agent/tests/log_convention.rs reads them.
            Outcome::Idle { token, why } | Outcome::Retry { token, why } => match *token {
                "seed-uninstalled" => info!(token = "seed-uninstalled", "{why}"),
                "seed-stopping" => info!(token = "seed-stopping", "{why}"),
                "seed-inputs-invalid" => error!(token = "seed-inputs-invalid", "{why}"),
                "seed-self-invalid" => error!(token = "seed-self-invalid", "{why}"),
                "seed-file-unknown-format" => warn!(token = "seed-file-unknown-format", "{why}"),
                "seed-file-unreadable" => warn!(token = "seed-file-unreadable", "{why}"),
                "seed-name-taken" => warn!(token = "seed-name-taken", "{why}"),
                "seed-actor-unstarted" => warn!(token = "seed-actor-unstarted", "{why}"),
                "seed-engine-unreachable" => warn!(token = "seed-engine-unreachable", "{why}"),
                "seed-pull-failed" => warn!(token = "seed-pull-failed", "{why}"),
                "seed-agent-image-unavailable" => {
                    warn!(token = "seed-agent-image-unavailable", "{why}")
                }
                "seed-create-failed" => warn!(token = "seed-create-failed", "{why}"),
                "seed-start-failed" => warn!(token = "seed-start-failed", "{why}"),
                other => warn!(token = "seed-look-failed", "{other}: {why}"),
            },
        }
    }

    /// Whether the seed's health check passes after this look. Idle on something only the
    /// operator can clear is unhealthy, so a stack manager shows what the log line says;
    /// an uninstalled machine is idle by intent, and a retry is transient.
    pub fn healthy(&self) -> bool {
        !matches!(self, Outcome::Idle { token, .. } if *token != "seed-uninstalled")
    }

    /// One line for `quasar-recovery status` in the seed's container.
    pub fn summary(&self) -> String {
        match self {
            Outcome::Present { container } => format!("recovery actor present ({container})"),
            Outcome::Created { id, .. } => format!("recovery actor created ({id})"),
            Outcome::Idle { why, .. } => format!("idle: {why}"),
            Outcome::Retry { why, .. } => format!("retrying: {why}"),
        }
    }
}

const UNHEALTHY: &str = "unhealthy";

/// The seed's status file, rewritten after every look. Read back only by
/// [`status_report`] in the same container; not an interface.
pub fn status_body(now_secs: u64, outcome: &Outcome) -> String {
    let health = if outcome.healthy() {
        "healthy"
    } else {
        UNHEALTHY
    };
    format!("{now_secs}\n{}\n{health}\n", outcome.summary())
}

/// What `quasar-recovery status` prints in the seed's container, and whether its health
/// check passes: the last look is at most three intervals old and was
/// [`Outcome::healthy`]. A file without the health line reads as healthy.
pub fn status_report(body: &str, now_secs: u64) -> (String, bool) {
    let mut lines = body.lines();
    let at: u64 = lines
        .next()
        .and_then(|l| l.trim().parse().ok())
        .unwrap_or(0);
    let summary = lines.next().unwrap_or("no look yet");
    let healthy = lines.next().map(str::trim) != Some(UNHEALTHY);
    let age = now_secs.saturating_sub(at);
    let fresh = age <= 3 * INTERVAL.as_secs();
    (format!("seed: {summary} ({age} s ago)"), fresh && healthy)
}

pub struct SeedConfig {
    /// Where the machine-state volume is mounted (read-only is enough).
    pub machine_dir: PathBuf,
    /// This process's own container.
    pub self_container: Option<String>,
    pub new_installation_id: Box<dyn Fn() -> String + Send + Sync>,
    /// Rewritten after every look, for the image's health check (`quasar-recovery status`).
    pub status_file: Option<PathBuf>,
}

impl SeedConfig {
    pub fn new(machine_dir: impl Into<PathBuf>) -> Self {
        SeedConfig {
            machine_dir: machine_dir.into(),
            self_container: None,
            new_installation_id: Box::new(crate::actor::random_uuid),
            status_file: None,
        }
    }
}

/// The seed's own container, as far as creating an actor needs it.
struct Me {
    id: String,
    image: ActorImage,
    engine_socket: String,
    env: Vec<String>,
}

pub struct Seed {
    engine: Arc<dyn PlatformEngine>,
    config: SeedConfig,
    last: Option<Outcome>,
}

fn invalid_self(why: String) -> Outcome {
    Outcome::Idle {
        token: "seed-self-invalid",
        why,
    }
}

fn inputs_invalid(why: String) -> Outcome {
    Outcome::Idle {
        token: "seed-inputs-invalid",
        why: format!("{why}; nothing was installed, and this seed stays idle until it is started again with corrected inputs"),
    }
}

fn unreachable(e: impl std::fmt::Display) -> Outcome {
    Outcome::Retry {
        token: "seed-engine-unreachable",
        why: format!("the container engine did not answer: {e}"),
    }
}

impl Seed {
    pub fn new(engine: Arc<dyn PlatformEngine>, config: SeedConfig) -> Self {
        Seed {
            engine,
            config,
            last: None,
        }
    }

    /// One look, logged only when it differs from the previous one, so an idle seed says
    /// why once rather than every interval.
    pub fn step(&mut self) -> Outcome {
        let outcome = self.tick();
        if self.last.as_ref() != Some(&outcome) {
            outcome.log();
        }
        if let Some(path) = &self.config.status_file {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_secs())
                .unwrap_or(0);
            let tmp = path.with_extension("tmp");
            let body = status_body(now, &outcome);
            if std::fs::write(&tmp, body).is_ok() {
                let _ = std::fs::rename(&tmp, path);
            }
        }
        self.last = Some(outcome.clone());
        outcome
    }

    pub fn run(mut self) -> ! {
        loop {
            self.step();
            std::thread::sleep(INTERVAL);
        }
    }

    /// One look at the machine, and the one action it may call for.
    pub fn tick(&self) -> Outcome {
        let read = file::read(&self.config.machine_dir);
        let looks = matches!(read, SeedRead::Missing)
            || matches!(&read, SeedRead::Found(f) if f.state == SeedState::Active);
        let containers = if looks {
            match self.engine.list_containers() {
                Ok(c) => c,
                Err(e) => return unreachable(e),
            }
        } else {
            Vec::new()
        };
        // The profile carries the inspected full id, so the comparison must use it too:
        // $HOSTNAME, the fallback identity, is only its first 12 characters.
        let me = self.config.self_container.as_deref().and_then(|hint| {
            containers
                .iter()
                .find(|c| c.id == hint || (hint.len() >= 12 && c.id.starts_with(hint)))
                .map(|c| c.id.as_str())
        });
        match decide(&read, &containers, me) {
            Decision::Uninstalled { installation_id } => Outcome::Idle {
                token: "seed-uninstalled",
                why: format!(
                    "Quasar was uninstalled from this machine (installation {installation_id}), so this seed does nothing; remove it the way you started it"
                ),
            },
            Decision::UnknownFormat { format_version } => Outcome::Idle {
                token: "seed-file-unknown-format",
                why: format!(
                    "seed.json has format_version {format_version} and this seed reads only format 1, so it does nothing"
                ),
            },
            Decision::Unreadable { why } => Outcome::Idle {
                token: "seed-file-unreadable",
                why: format!("{why}; this seed does nothing until it is fixed"),
            },
            Decision::StartOwn { container } => {
                let Some(c) = containers.iter().find(|c| c.name == container) else {
                    return Outcome::Present { container };
                };
                let installation = c.labels.get(INSTALLATION_LABEL).cloned();
                self.start_own(c.id.clone(), c.image.clone(), installation.unwrap_or_default())
            }
            Decision::ActorPresent { container } => {
                let creator = containers
                    .iter()
                    .find(|c| c.name == container)
                    .and_then(unstarted_create_of);
                match creator {
                    Some(other) => Outcome::Idle {
                        token: "seed-actor-unstarted",
                        why: format!(
                            "{container} was created by another seed container ({other}) and never started; this seed does not start it (ADR 0007). Run docker start {container}"
                        ),
                    },
                    None => Outcome::Present { container },
                }
            }
            Decision::NameTaken { container } => Outcome::Idle {
                token: "seed-name-taken",
                why: format!(
                    "a container named {container} exists without this installation's labels; it is left untouched and no recovery actor is created until it is removed"
                ),
            },
            Decision::Create {
                installation_id,
                image,
            } => self.create(installation_id, image),
        }
    }

    fn create(&self, installation: Option<String>, image: Option<String>) -> Outcome {
        let me = match self.me() {
            Ok(me) => me,
            Err(outcome) => return outcome,
        };
        let (installation_id, image) = match installation {
            Some(id) => {
                let Some(image) = image.as_deref().and_then(ActorImage::parse) else {
                    return Outcome::Idle {
                        token: "seed-file-unreadable",
                        why: format!("seed.json names no usable recovery-actor image ({image:?})"),
                    };
                };
                if let Err(outcome) = self.ensure_image(&image) {
                    return outcome;
                }
                (id, image)
            }
            None => {
                let host = match self.engine.host() {
                    Ok(host) => host,
                    Err(e) => return unreachable(e),
                };
                let checked =
                    Bootstrap::from_env(&me.env).and_then(|boot| boot.check(host.name.as_deref()));
                let checked = match checked {
                    Ok(checked) => checked,
                    Err(why) => return inputs_invalid(why),
                };
                // The actor would refuse this image only after it had written machine state,
                // which then wins over corrected inputs: refuse it here, before anything exists.
                if let Err(outcome) = self.check_agent_image(&checked.agent_image) {
                    return outcome;
                }
                ((self.config.new_installation_id)(), me.image.clone())
            }
        };
        let spec = profile::actor(&installation_id, &image, &me.engine_socket, &me.id);

        let _critical = shutdown::critical();
        if shutdown::requested() {
            return Outcome::Retry {
                token: "seed-stopping",
                why: "stopping; nothing was created".into(),
            };
        }
        let id = match self.engine.create_container(&spec) {
            Ok(id) => id,
            Err(e) => {
                // A create whose outcome is unknown may have taken effect: the next look
                // finds it, never started and carrying this seed's id, and starts it.
                return Outcome::Retry {
                    token: "seed-create-failed",
                    why: format!("creating {} from {}: {e}", profile::NAME, image.reference()),
                };
            }
        };
        self.start_own(id, image.reference(), installation_id)
    }

    /// The agent image a first install names: on the engine (pulled if need be) and of a
    /// recipe revision this build's actor carries.
    fn check_agent_image(&self, image: &ImageRef) -> Result<(), Outcome> {
        let reference = image.reference();
        let unavailable = |e: String| {
            Outcome::Retry {
            token: "seed-agent-image-unavailable",
            why: format!(
                "{} {reference} cannot be pulled ({e}); nothing was installed, and it is tried again at the next look",
                bootstrap::AGENT_IMAGE
            ),
        }
        };
        let found = match self.engine.inspect_image(&reference) {
            Ok(Some(found)) => found,
            Ok(None) => {
                // Once: a pull that keeps failing is the one WARN line, not INFO every look.
                let retrying = matches!(
                    &self.last,
                    Some(Outcome::Retry {
                        token: "seed-agent-image-unavailable",
                        ..
                    })
                );
                if !retrying {
                    info!(image = %reference, "pulling the agent image the install names");
                }
                if let Err(e) = self.engine.pull(&reference) {
                    return Err(unavailable(e.to_string()));
                }
                match self.engine.inspect_image(&reference) {
                    Ok(Some(found)) => found,
                    Ok(None) => return Err(unavailable("not on the engine after the pull".into())),
                    Err(e) => return Err(unreachable(e)),
                }
            }
            Err(e) => return Err(unreachable(e)),
        };
        crate::actor::node_agent_revision(&found, image)
            .map(|_| ())
            .map_err(|e| inputs_invalid(format!("{}: {e}", bootstrap::AGENT_IMAGE)))
    }

    fn start_own(&self, id: String, image: String, installation_id: String) -> Outcome {
        let _critical = shutdown::critical();
        if let Err(e) = self.engine.start_container(&id) {
            return Outcome::Retry {
                token: "seed-start-failed",
                why: format!("starting {} ({id}): {e}", profile::NAME),
            };
        }
        Outcome::Created {
            id,
            image,
            installation_id,
        }
    }

    fn ensure_image(&self, image: &ActorImage) -> Result<(), Outcome> {
        let reference = image.reference();
        match self.engine.inspect_image(&reference) {
            Ok(Some(_)) => return Ok(()),
            Ok(None) => {}
            Err(e) => return Err(unreachable(e)),
        }
        info!(image = %reference, "pulling the last verified recovery-actor image");
        self.engine.pull(&reference).map_err(|e| Outcome::Retry {
            token: "seed-pull-failed",
            why: format!("pulling {reference}: {e}"),
        })
    }

    fn me(&self) -> Result<Me, Outcome> {
        let Some(id) = &self.config.self_container else {
            return Err(invalid_self(
                "this seed cannot tell which container it runs in, so it cannot create the recovery actor; run it as a container".into(),
            ));
        };
        let c = match self.engine.inspect_container(id) {
            Ok(Some(c)) => c,
            Ok(None) => {
                return Err(invalid_self(format!(
                    "this seed's own container {id} was not found"
                )))
            }
            Err(e) => return Err(unreachable(e)),
        };
        let engine_socket = c
            .mounts
            .iter()
            .find(|(_, target, _)| target == profile::ENGINE_SOCKET)
            .map(|(source, _, _)| source.clone())
            .ok_or_else(|| {
                invalid_self(format!(
                    "the engine socket is not mounted at {} (add -v /var/run/docker.sock:{})",
                    profile::ENGINE_SOCKET,
                    profile::ENGINE_SOCKET
                ))
            })?;
        let machine = c
            .mounts
            .iter()
            .find(|(_, target, _)| target == profile::MACHINE_DIR)
            .map(|(source, _, _)| source.as_str());
        if machine != Some(profile::MACHINE_VOLUME) {
            let found = match machine {
                Some(other) => format!("{other} is mounted there"),
                None => "nothing is mounted there".into(),
            };
            return Err(invalid_self(format!(
                "the {vol} volume must be mounted at {dir}, and {found}: with docker run add -v {vol}:{dir}:ro; in a Compose stack (Dockge, Arcane) declare the volume at the top level with `name: {vol}`, since a stack otherwise names it <project>_{vol}",
                vol = profile::MACHINE_VOLUME,
                dir = profile::MACHINE_DIR,
            )));
        }
        let image = match ActorImage::parse(&c.image) {
            Some(pinned) => pinned,
            None => {
                let repository = repository_of(&c.image);
                let found = match self.engine.inspect_image(&c.image_id) {
                    Ok(found) => found,
                    Err(e) => return Err(unreachable(e)),
                };
                found
                    .and_then(|i| {
                        i.repo_digests.iter().find_map(|d| {
                            let (repo, _) = d.split_once('@')?;
                            if repo == repository {
                                ActorImage::parse(d)
                            } else {
                                None
                            }
                        })
                    })
                    .ok_or_else(|| {
                        invalid_self(format!(
                            "this seed's image {} has no registry digest; start the seed by digest ({repository}@sha256:…)",
                            c.image
                        ))
                    })?
            }
        };
        Ok(Me {
            id: c.id,
            image,
            engine_socket,
            env: c.env,
        })
    }
}
