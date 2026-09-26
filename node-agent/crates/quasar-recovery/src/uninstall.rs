//! Taking a machine's Quasar services away (#352 D12, R1-Q2): the operator's
//! `quasar-recovery uninstall [--purge]`, and the marker the console's "remove host"
//! ([`crate::remove`]) shares with it.
//!
//! **What it removes.** Every container this installation created, and nothing it did not
//! (the race guard, [`crate::race_guard`]): the node agent, the control plane, Postgres,
//! then the recovery actor, the reverse of the install order. Without `--purge` that is all:
//! the database volume, the machine-state volume and the homes stay, and so does the seed,
//! which is the operator's manager's. With `--purge` the installation's volumes and network
//! go too, after a typed confirmation and — for a Quasar-owned database — a final `pg_dump`
//! ([`crate::dump`]) that is itself never deleted. Homes are host directories and are never
//! deleted by Quasar.
//!
//! **The marker.** Before anything is removed, `uninstalled.json` is written to machine
//! state and `seed.json` is set `uninstalled` (ADR 0007), both durably. From then on the
//! seed does not bring the actor back, and an actor that is started anyway installs
//! nothing (`Actor::resume`). So an interrupted uninstall leaves a machine that stays
//! down, and running the command again finishes it: every step is "remove if present".
//!
//! **Where it runs.** In its own container (`docker run --rm … uninstall`), because it
//! stops the recovery actor, and a process cannot finish a job whose container it has just
//! stopped. It takes the machine's lease once the actor is stopped, so no actor can act
//! beside it.

use std::io;
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::dump;
use crate::engine::{Container, EngineError, PlatformEngine, RestartPolicy};
use crate::journal::JournalDir;
use crate::machine::{Machine, MachineDir};
use crate::recipe::{labels, names, DatabaseInputs, ImageRef, Role};
use crate::seed::{self, file::SeedFile};

pub const MARKER_FILE: &str = "uninstalled.json";
pub const MARKER_FORMAT: u32 = 1;

/// Who took the services away.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum By {
    /// `quasar-recovery uninstall` on the machine.
    Operator,
    /// The console's "remove host" (agent-api.md `host_remove`).
    Console,
}

/// `uninstalled.json`. Not a frozen interface (schema.md §"Not frozen").
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Marker {
    pub format: u32,
    pub by: By,
    /// The `host_remove` request that started a console removal. A later one, whatever its
    /// id, drives the same removal again.
    #[serde(default)]
    pub request_id: Option<String>,
    #[serde(default)]
    pub purge: bool,
    /// A purge's final dump, once taken, so a re-run does not take a second one.
    #[serde(default)]
    pub dump: Option<String>,
    pub started_at: String,
    #[serde(default)]
    pub finished_at: Option<String>,
}

/// Why this machine's recovery actor must install and replace nothing, when it must not:
/// an uninstall or a removal has started (or finished) here.
pub fn uninstalled(dir: &MachineDir) -> Option<String> {
    match dir.load_uninstall() {
        Ok(Some(m)) => {
            return Some(match m.by {
                By::Operator => format!("this machine was uninstalled (started {})", m.started_at),
                By::Console => format!(
                    "this host was removed from the console (started {})",
                    m.started_at
                ),
            })
        }
        Ok(None) => {}
        // Unreadable reads as uninstalled: it fails closed, never re-installs.
        Err(e) => return Some(format!("{MARKER_FILE} is unreadable ({e})")),
    }
    match dir.load_seed_file() {
        Ok(Some(f)) if f.state == seed::file::SeedState::Uninstalled => {
            Some("seed.json says this installation is uninstalled".into())
        }
        _ => None,
    }
}

/// Write the marker, then set `seed.json` to `uninstalled`. `own_image` is the recovery
/// image to record in a `seed.json` no actor has written yet; without one the file is left
/// absent, and the marker alone keeps any actor the seed creates from installing.
pub(crate) fn mark(
    dir: &MachineDir,
    machine: &Machine,
    marker: &Marker,
    own_image: Option<seed::file::ActorImage>,
) -> io::Result<()> {
    dir.store_uninstall(marker)?;
    let file = match dir.load_seed_file() {
        Ok(Some(mut f)) => {
            f.state = seed::file::SeedState::Uninstalled;
            f
        }
        Ok(None) | Err(_) => match own_image {
            Some(image) => SeedFile {
                format_version: seed::file::FORMAT_VERSION,
                installation_id: machine.installation_id.clone(),
                recovery_actor_image: image,
                state: seed::file::SeedState::Uninstalled,
            },
            None => {
                warn!(
                    token = "uninstall-seed-file-unwritten",
                    "seed.json could not be set to uninstalled (no recovery image is known); {MARKER_FILE} keeps any recovery actor a seed creates from installing"
                );
                return Ok(());
            }
        },
    };
    dir.store_seed_file(&file)
}

/// The reverse of the install order. The recovery actor is last: it is stopped first, so it
/// cannot act beside the removal, but removed only once everything it created is gone.
pub const ORDER: [Role; 4] = [
    Role::NodeAgent,
    Role::ControlPlane,
    Role::Postgres,
    Role::RecoveryActor,
];

/// This installation's containers of one role, whatever their name (`.kept` and `.next`
/// included).
pub(crate) fn ours<'a>(
    containers: &'a [Container],
    installation: &str,
    role: Role,
) -> Vec<&'a Container> {
    containers
        .iter()
        .filter(|c| {
            c.labels.get(labels::INSTALLATION).map(String::as_str) == Some(installation)
                && c.labels.get(labels::PLATFORM_SERVICE).map(String::as_str) == Some(role.as_str())
        })
        .collect()
}

/// The actor's disposable helpers left by a crash: named as the actor names them and
/// carrying its helper label.
pub(crate) fn helpers(containers: &[Container]) -> Vec<&Container> {
    containers
        .iter()
        .filter(|c| {
            names::HELPERS.contains(&c.name.as_str()) && c.labels.contains_key(labels::HELPER)
        })
        .collect()
}

/// Stop (with its grace), then remove. A container already gone is not an error.
pub(crate) fn stop_and_remove(
    engine: &dyn PlatformEngine,
    c: &Container,
    grace: Duration,
) -> Result<(), EngineError> {
    if c.running {
        match engine.stop_container(&c.id, grace) {
            Ok(()) => {}
            Err(EngineError::Runtime(crate::engine::ErrorKind::Missing)) => return Ok(()),
            Err(e) => return Err(e),
        }
    }
    engine.remove_container(&c.id)
}

/// Every volume a purge may delete, by name: the installation's own, or (for the socket
/// volume, which the engine creates unlabelled when the seed first mounts it) unlabelled.
/// The machine-state volume is emptied rather than deleted: this command has it mounted.
const PURGED_VOLUMES: &[&str] = &[
    names::NODE_AGENT_SECRETS_VOLUME,
    names::AGENT_DATA_VOLUME,
    names::NVIDIA_DRIVER_VOLUME,
    names::CONTROL_PLANE_SECRETS_VOLUME,
    names::CONTROL_DATA_VOLUME,
    names::POSTGRES_SECRETS_VOLUME,
    names::POSTGRES_DATA_VOLUME,
    names::AGENT_SOCKET_VOLUME,
];

/// What the operator asked for.
#[derive(Debug, Clone, Default)]
pub struct Options {
    pub purge: bool,
    /// The typed confirmation: the machine's node name.
    pub confirm: Option<String>,
    /// An absolute host directory for the final dump; the dump volume otherwise.
    pub dump_to: Option<String>,
}

/// How an uninstall ended short of done.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum UninstallError {
    /// Refused before anything was changed.
    Refused(String),
    /// Stopped part-way; the marker is written, so the machine stays down, and running the
    /// command again continues from here.
    Stopped(String),
}

impl std::fmt::Display for UninstallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            UninstallError::Refused(why) => write!(f, "{why}; nothing was changed"),
            UninstallError::Stopped(why) => write!(
                f,
                "{why}. The uninstall stopped part-way: nothing Quasar removed comes back on its own, and running the same command again finishes it"
            ),
        }
    }
}

/// What was done, as lines for the operator.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Report {
    pub lines: Vec<String>,
}

impl Report {
    fn say(&mut self, line: impl Into<String>) {
        let line = line.into();
        info!(token = "uninstall-step", "{line}");
        self.lines.push(line);
    }
}

/// Asks one question on the terminal; `None` when nothing was typed.
pub type Prompt = Box<dyn FnMut(&str) -> Option<String>>;

pub struct Uninstall {
    engine: Arc<dyn PlatformEngine>,
    dir: MachineDir,
    /// This command's own container, when it runs in one: never a candidate for removal.
    pub self_container: Option<String>,
    pub stop_grace: Duration,
    pub now: Box<dyn Fn() -> String + Send + Sync>,
    /// Asks the operator a question and returns the line typed, for the purge confirmation
    /// when none was given on the command line. `None` (no terminal) refuses.
    pub prompt: Option<Prompt>,
    /// Between looks while waiting for a stopped actor to let go of the lease.
    pub lease_wait: Duration,
}

impl Uninstall {
    pub fn new(
        engine: Arc<dyn PlatformEngine>,
        machine_dir: impl Into<std::path::PathBuf>,
    ) -> Self {
        Uninstall {
            engine,
            dir: MachineDir::new(machine_dir.into()),
            self_container: None,
            stop_grace: Duration::from_secs(30),
            now: Box::new(crate::actor::rfc3339_now),
            prompt: None,
            lease_wait: Duration::from_secs(10),
        }
    }

    pub fn run(&mut self, opts: &Options) -> Result<Report, UninstallError> {
        let refused = |why: String| UninstallError::Refused(why);
        let engine_err =
            |what: &str, e: EngineError| format!("{what}: the container engine said {e}");
        let mut report = Report::default();

        let machine = match self.dir.load_machine() {
            Ok(Some(m)) => m,
            Ok(None) => return self.without_machine_state(opts, report),
            Err(e) => return Err(refused(format!("machine state is unreadable: {e}"))),
        };
        let id = machine.installation_id.clone();
        let containers = self
            .engine
            .list_containers()
            .map_err(|e| refused(engine_err("list containers", e)))?;
        let image = self.image_to_run(&containers);
        let me = self.self_container.clone();
        let is_me = |c: &Container| {
            me.as_deref()
                .is_some_and(|m| c.id.starts_with(m) || m.starts_with(&c.id))
        };

        if let Some(me) = containers.iter().find(|c| is_me(c)) {
            if ours(std::slice::from_ref(me), &id, Role::RecoveryActor).len() == 1 {
                return Err(refused(format!(
                    "uninstall stops the recovery actor, so it cannot run inside it; run it in its own container: {}",
                    command_line(&image, "uninstall")
                )));
            }
        }
        let marker = self.dir.load_uninstall().ok().flatten();
        let resuming = marker.is_some();

        // An attempt in flight is the recovery actor's to finish; a stopped actor's is not
        // going anywhere, and an uninstall removes what it was replacing either way.
        let actor_running = ours(&containers, &id, Role::RecoveryActor)
            .iter()
            .any(|c| c.running && !is_me(c));
        let scan = JournalDir::new(self.dir.root()).scan();
        if !resuming && actor_running {
            if let Some(open) = scan.open_id() {
                return Err(refused(format!(
                    "the recovery actor has attempt {open} in flight; wait for it to finish (quasar-recovery status) and run uninstall again"
                )));
            }
        }

        let mut dump_name = marker.as_ref().and_then(|m| m.dump.clone());
        if opts.purge {
            let node = machine.inputs.node_name.clone();
            let typed = match &opts.confirm {
                Some(t) => Some(t.clone()),
                None => match self.prompt.as_mut() {
                    Some(ask) => ask(&format!(
                        "--purge deletes this machine's Quasar data: the database, machine state and the agent's identity. Type the node name ({node}) to confirm: "
                    )),
                    None => None,
                },
            };
            match typed.as_deref().map(str::trim) {
                Some(t) if t == node || t == id => {}
                Some(_) => {
                    return Err(refused(format!(
                        "the confirmation did not match the node name {node}"
                    )))
                }
                None => {
                    return Err(refused(format!(
                        "--purge needs a typed confirmation: run it in a terminal (docker run -it …), or add --confirm {node}"
                    )))
                }
            }
            if let Some(s) = containers.iter().find(|c| seed::is_seed(c)) {
                return Err(refused(format!(
                    "a seed ({}) is still on this machine; a purge deletes the state that tells it this machine is uninstalled, so it would install Quasar again. Remove the seed first, the way you started it (from your manager, or docker rm -f {}), then run this again",
                    s.name, s.name
                )));
            }
        }

        // The marker first: from here the seed and any actor leave this machine down.
        let started = marker
            .as_ref()
            .map(|m| m.started_at.clone())
            .unwrap_or_else(|| (self.now)());
        let mut current = Marker {
            format: MARKER_FORMAT,
            by: marker.as_ref().map(|m| m.by).unwrap_or(By::Operator),
            request_id: marker.as_ref().and_then(|m| m.request_id.clone()),
            purge: opts.purge || marker.as_ref().is_some_and(|m| m.purge),
            dump: dump_name.clone(),
            started_at: started,
            finished_at: None,
        };
        let own_image = containers
            .iter()
            .find(|c| is_me(c))
            .and_then(|c| seed::file::ActorImage::parse(&c.image));
        mark(&self.dir, &machine, &current, own_image)
            .map_err(|e| refused(format!("the uninstall marker could not be written: {e}")))?;
        if !resuming {
            report.say("Marked this installation uninstalled: the seed will not re-create the recovery actor.");
        }
        let stopped = |why: String| UninstallError::Stopped(why);

        // The actor stops first, and stays stopped: it never acts beside the removal.
        for actor in ours(&containers, &id, Role::RecoveryActor) {
            if is_me(actor) {
                continue;
            }
            if actor.restart != Some(RestartPolicy::No) {
                self.engine
                    .set_restart_policy(&actor.id, RestartPolicy::No)
                    .map_err(|e| stopped(engine_err("disable the recovery actor's restart", e)))?;
            }
            if actor.running {
                self.engine
                    .stop_container(&actor.id, self.stop_grace)
                    .map_err(|e| stopped(engine_err("stop the recovery actor", e)))?;
                report.say(format!("Stopped the recovery actor ({}).", actor.name));
            }
        }
        let _lease = self.lease().map_err(stopped)?;

        for role in ORDER {
            if opts.purge && role == Role::Postgres && dump_name.is_none() {
                if let Some(name) = self.final_dump(&machine, opts, &containers, &mut report)? {
                    dump_name = Some(name);
                    current.dump = dump_name.clone();
                    self.dir.store_uninstall(&current).map_err(|e| {
                        stopped(format!("the uninstall marker could not be updated: {e}"))
                    })?;
                }
            }
            for c in ours(&containers, &id, role) {
                if is_me(c) {
                    continue;
                }
                stop_and_remove(self.engine.as_ref(), c, self.stop_grace)
                    .map_err(|e| stopped(engine_err(&format!("remove {}", c.name), e)))?;
                report.say(format!("Removed the {} ({}).", noun(role), c.name));
            }
        }
        for h in helpers(&containers) {
            let _ = self.engine.remove_container(&h.id);
        }

        if opts.purge {
            self.purge_data(&machine, &mut report)?;
            report.say(match &dump_name {
                Some(name) => format!(
                    "Purged. The final dump of the database is {}. Homes under {} are host directories and were not deleted.",
                    self.dest(opts).describe(name),
                    machine.inputs.home_root
                ),
                None => format!(
                    "Purged. Homes under {} are host directories and were not deleted.",
                    if machine.inputs.home_root.is_empty() { "the home root" } else { machine.inputs.home_root.as_str() }
                ),
            });
            report.say(format!(
                "Remove the emptied machine-state volume once this command has exited: docker volume rm {}",
                names::MACHINE_VOLUME
            ));
        } else {
            current.finished_at = Some((self.now)());
            if let Err(e) = self.dir.store_uninstall(&current) {
                warn!(token = "uninstall-marker-unfinished", "{e}");
            }
            report.say(format!(
                "Uninstalled. Kept: the database volume ({}), machine state ({}), the agent's identity and the homes{}. The seed, if you run one, stays idle; remove it the way you started it.",
                names::POSTGRES_DATA_VOLUME,
                names::MACHINE_VOLUME,
                if machine.inputs.home_root.is_empty() { String::new() } else { format!(" under {}", machine.inputs.home_root) }
            ));
            if machine.role == crate::socket::MachineRole::Gpu {
                report.say(
                    "To bring this host back, add it from Admin → Fleet → Add host with the same node name: its history and homes are kept.",
                );
            }
            report.say(format!(
                "To delete the data as well: {}",
                command_line(&image, "uninstall --purge")
            ));
        }
        Ok(report)
    }

    /// No machine state: nothing is known of an installation but its label, as when an
    /// install stopped before it wrote machine state. Only `--purge --confirm
    /// <installation id>` removes anything then: what carries that id.
    fn without_machine_state(
        &mut self,
        opts: &Options,
        mut report: Report,
    ) -> Result<Report, UninstallError> {
        let none = format!(
            "No Quasar installation is on this machine (no machine state in {}): nothing to remove.",
            self.dir.root().display()
        );
        let id = match (opts.purge, opts.confirm.as_deref().map(str::trim)) {
            (true, Some(id)) if !id.is_empty() => id.to_owned(),
            _ => {
                report.say(none);
                return Ok(report);
            }
        };
        let refused = |why: String| UninstallError::Refused(why);
        let stopped = |why: String| UninstallError::Stopped(why);
        let containers = self
            .engine
            .list_containers()
            .map_err(|e| refused(format!("list containers: {e}")))?;
        let me = self.self_container.clone();
        let is_me = |c: &Container| {
            me.as_deref()
                .is_some_and(|m| c.id.starts_with(m) || m.starts_with(&c.id))
        };
        let mut volumes = Vec::new();
        for name in PURGED_VOLUMES {
            match self.engine.inspect_volume(name) {
                Ok(Some(v))
                    if v.labels.get(labels::INSTALLATION).map(String::as_str)
                        == Some(id.as_str()) =>
                {
                    volumes.push(*name)
                }
                Ok(_) => {}
                Err(e) => return Err(refused(format!("inspect the {name} volume: {e}"))),
            }
        }
        let labelled = containers.iter().any(|c| {
            !is_me(c) && c.labels.get(labels::INSTALLATION).map(String::as_str) == Some(id.as_str())
        });
        if !labelled && volumes.is_empty() {
            report.say(none);
            return Ok(report);
        }
        if volumes.contains(&names::POSTGRES_DATA_VOLUME) {
            return Err(refused(format!(
                "installation {id} has a database volume and no machine state, so its final dump cannot be taken"
            )));
        }
        if let Some(s) = containers.iter().find(|c| seed::is_seed(c)) {
            return Err(refused(format!(
                "a seed ({}) is still on this machine and would install Quasar again; remove it first (docker rm -f {})",
                s.name, s.name
            )));
        }
        for actor in ours(&containers, &id, Role::RecoveryActor) {
            if is_me(actor) {
                continue;
            }
            let _ = self.engine.set_restart_policy(&actor.id, RestartPolicy::No);
            if actor.running {
                self.engine
                    .stop_container(&actor.id, self.stop_grace)
                    .map_err(|e| stopped(format!("stop {}: {e}", actor.name)))?;
            }
        }
        for role in ORDER {
            for c in ours(&containers, &id, role) {
                if is_me(c) {
                    continue;
                }
                stop_and_remove(self.engine.as_ref(), c, self.stop_grace)
                    .map_err(|e| stopped(format!("remove {}: {e}", c.name)))?;
                report.say(format!("Removed the {} ({}).", noun(role), c.name));
            }
        }
        for name in volumes {
            self.engine
                .remove_volume(name)
                .map_err(|e| stopped(format!("delete the {name} volume: {e}")))?;
            report.say(format!("Deleted the {name} volume."));
        }
        report.say(format!(
            "Purged what installation {id} left. Remove the machine-state volume once this command has exited: docker volume rm {}",
            names::MACHINE_VOLUME
        ));
        Ok(report)
    }

    /// The machine's lease, once the stopped actor has let go of it.
    fn lease(&self) -> Result<quasar_runtime::StateLease, String> {
        let deadline = std::time::Instant::now() + self.lease_wait;
        loop {
            match self.dir.lease() {
                Ok(lease) => return Ok(lease),
                Err(quasar_runtime::LeaseError::Held(_)) if std::time::Instant::now() < deadline => {
                    std::thread::sleep(Duration::from_millis(200));
                }
                Err(quasar_runtime::LeaseError::Held(_)) => {
                    return Err("a recovery actor still holds this machine's lease (actor.lease) after being stopped; stop it (docker stop quasar-recovery) and run again".into())
                }
                Err(quasar_runtime::LeaseError::Open(e)) => {
                    return Err(format!("the machine's lease cannot be opened: {e}"))
                }
            }
        }
    }

    fn dest(&self, opts: &Options) -> dump::Dest {
        match &opts.dump_to {
            Some(dir) => dump::Dest::HostDir(dir.clone()),
            None => dump::Dest::Volume(names::FINAL_DUMP_VOLUME.into()),
        }
    }

    /// The final dump, for a Quasar-owned database whose data volume exists. `None`: there
    /// is nothing of Quasar's to dump.
    fn final_dump(
        &self,
        machine: &Machine,
        opts: &Options,
        containers: &[Container],
        report: &mut Report,
    ) -> Result<Option<String>, UninstallError> {
        let Some(control) = &machine.inputs.control else {
            return Ok(None);
        };
        if control.database != DatabaseInputs::Owned {
            report.say("The database is your own: Quasar never dumps or deletes it.");
            return Ok(None);
        }
        let stopped = |why: String| UninstallError::Stopped(why);
        let exists = self
            .engine
            .inspect_volume(names::POSTGRES_DATA_VOLUME)
            .map_err(|e| stopped(format!("inspect the database volume: {e}")))?
            .is_some();
        if !exists {
            report.say("There is no database volume left to dump.");
            return Ok(None);
        }
        let image = self.postgres_image(machine).ok_or_else(|| {
            stopped("machine state names no Postgres image to dump the database with, so nothing was deleted".into())
        })?;
        // The service stops first: two postmasters never share a data directory.
        for c in ours(containers, &machine.installation_id, Role::Postgres) {
            if c.running {
                self.engine
                    .stop_container(&c.id, self.stop_grace)
                    .map_err(|e| stopped(format!("stop Postgres for its final dump: {e}")))?;
            }
        }
        let dest = self.dest(opts);
        let name = dump::file_name(&(self.now)());
        report.say("Taking the final dump of the database…");
        match dump::take(
            self.engine.as_ref(),
            &image,
            &machine.installation_id,
            &dest,
            &name,
        ) {
            Ok(name) => {
                report.say(format!("Final dump taken: {}.", dest.describe(&name)));
                Ok(Some(name))
            }
            Err(why) => Err(stopped(format!(
                "the final dump of the database failed ({why}), so no data was deleted; the database volume {} is intact",
                names::POSTGRES_DATA_VOLUME
            ))),
        }
    }

    fn postgres_image(&self, machine: &Machine) -> Option<ImageRef> {
        self.dir
            .load_service(Role::Postgres)
            .ok()
            .flatten()
            .map(|r| r.image)
            .or_else(|| machine.install_images.get(&Role::Postgres).cloned())
    }

    fn purge_data(&self, machine: &Machine, report: &mut Report) -> Result<(), UninstallError> {
        let stopped = |why: String| UninstallError::Stopped(why);
        for name in PURGED_VOLUMES {
            let Some(v) = self
                .engine
                .inspect_volume(name)
                .map_err(|e| stopped(format!("inspect the {name} volume: {e}")))?
            else {
                continue;
            };
            let label = v.labels.get(labels::INSTALLATION);
            let ours = label == Some(&machine.installation_id)
                || (label.is_none() && *name == names::AGENT_SOCKET_VOLUME);
            if !ours {
                report.say(format!(
                    "Kept the {name} volume: it is not this installation's."
                ));
                continue;
            }
            match self.engine.remove_volume(name) {
                Ok(()) => report.say(format!("Deleted the {name} volume.")),
                // It holds only sockets: one a container still mounts is kept, and said so.
                Err(e) if *name == names::AGENT_SOCKET_VOLUME => report.say(format!(
                    "Kept the {name} volume: it could not be deleted ({e})."
                )),
                Err(e) => return Err(stopped(format!("delete the {name} volume: {e}"))),
            }
        }
        if let Ok(Some(n)) = self.engine.inspect_network(names::PLATFORM_NETWORK) {
            if n.labels.get(labels::INSTALLATION) == Some(&machine.installation_id) {
                self.engine
                    .remove_network(names::PLATFORM_NETWORK)
                    .map_err(|e| {
                        stopped(format!(
                            "delete the {} network: {e}",
                            names::PLATFORM_NETWORK
                        ))
                    })?;
                report.say(format!("Deleted the {} network.", names::PLATFORM_NETWORK));
            }
        }
        // Machine state last: until it is gone, a re-run still knows what to purge.
        empty_machine_dir(self.dir.root())
            .map_err(|e| stopped(format!("empty machine state: {e}")))?;
        report.say("Deleted machine state (the secrets, journal and service records).");
        Ok(())
    }
}

/// Everything in the machine-state directory but the lease file, which this process holds.
fn empty_machine_dir(root: &std::path::Path) -> io::Result<()> {
    for entry in std::fs::read_dir(root)? {
        let entry = entry?;
        if entry.file_name() == "actor.lease" {
            continue;
        }
        let path = entry.path();
        if entry.file_type()?.is_dir() {
            std::fs::remove_dir_all(&path)?;
        } else {
            std::fs::remove_file(&path)?;
        }
    }
    std::fs::File::open(root)?.sync_all()
}

fn noun(role: Role) -> &'static str {
    crate::race_guard::noun(role.as_str())
}

/// The documented way to run an operator command beside the recovery actor: its own
/// container on the recovery image, with the engine socket and machine state. `image` is
/// printed as it is: after an uninstall no `quasar-recovery` container is left to inspect.
pub fn command_line(image: &str, command: &str) -> String {
    format!(
        "docker run --rm -it -v /var/run/docker.sock:/var/run/docker.sock -v {}:{} {image} {command}",
        names::MACHINE_VOLUME,
        crate::recipe::paths::MACHINE_DIR,
    )
}

impl Uninstall {
    /// The recovery image to run this command with again: the one it runs from, else the
    /// one `seed.json` names, else a placeholder.
    fn image_to_run(&self, containers: &[Container]) -> String {
        let me = self.self_container.as_deref();
        containers
            .iter()
            .find(|c| me.is_some_and(|m| c.id.starts_with(m) || m.starts_with(&c.id)))
            .and_then(|c| crate::actor::own_image(self.engine.as_ref(), c))
            .map(|i| i.reference())
            .or_else(|| {
                self.dir
                    .load_seed_file()
                    .ok()
                    .flatten()
                    .map(|s| s.recovery_actor_image.reference())
            })
            .unwrap_or_else(|| "<registry>/quasar-recovery@sha256:<digest>".to_owned())
    }
}
