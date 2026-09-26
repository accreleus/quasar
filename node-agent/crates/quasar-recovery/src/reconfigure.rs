//! `quasar-recovery reconfigure` (#352 decision A2): changing a machine's inputs (its home
//! root, release trust, app-container defaults) after install.
//!
//! A reconfigure is a **Replacement with the same digests and new machine inputs**: each
//! service whose rendered specification the change moves is replaced through the attempt
//! machinery ([`crate::replace`]) exactly as an update replaces it — journalled, the old
//! container kept until the new one verifies, restored if it does not — with the image it
//! already runs. A change no container renders (the recovery actor's own signature policy,
//! say) needs no replacement and is simply recorded.
//!
//! **Order, and why it is crash-safe.** `reconfigure.json` (the inputs before and after) is
//! committed first, then machine state takes the new inputs, then the attempt is journalled
//! and driven. `resume` settles the attempt first (D8), then [`Actor::settle_reconfigure`]
//! reads the record: the attempt succeeded → the new inputs stay; it failed, was
//! interrupted, or was never journalled → the old inputs are put back. So machine state
//! never holds inputs a running service was not verified with.
//!
//! What this build replaces for a reconfigure is the node agent. A change that moves the
//! control plane's container is refused: control-plane replacement (RH06-11, #363) serves
//! updates, and a reconfigure does not drive it yet. Postgres is never replaced (#352 R1).

use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tracing::{error, info, warn};

use crate::actor::Actor;
use crate::bootstrap as var;
use crate::journal::{CallerTag, Journal, Phase, Step, FORMAT};
use crate::machine::Machine;
use crate::recipe::{self, names, secrets, ImageRef, Inputs, Role, SecretMounts};
use crate::socket::{
    AttemptResult, MachineRole, Previous, Reason, Release, Request, RequestKind, State,
};

pub const RECORD_FILE: &str = "reconfigure.json";
const RECORD_FORMAT: u32 = 1;

/// `reconfigure.json`. Not a frozen interface.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Record {
    pub format: u32,
    pub request_id: String,
    pub changed: Vec<String>,
    pub before: Inputs,
    pub after: Inputs,
    pub replaced: Vec<Role>,
    pub started_at: String,
}

/// The operator's request on the operator socket.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReconfigureRequest {
    /// Variable → value, by the seed's variable names. An empty value unsets an optional one.
    pub changes: BTreeMap<String, String>,
    /// Plan only: say what would change and what would be replaced, and change nothing.
    #[serde(default)]
    pub dry_run: bool,
}

/// What a reconfigure changes, or changed.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Planned {
    /// The variables whose value changes.
    pub changed: Vec<String>,
    /// The services re-created with the new inputs, in order (`node-agent`).
    pub replaced: Vec<String>,
    /// The replacement's attempt, once admitted: `GET /v1/status?request_id=` follows it.
    pub request_id: Option<String>,
    pub dry_run: bool,
}

/// Every variable a reconfigure takes, and where it may be given.
const EVERYWHERE: &[&str] = &[
    var::HOME_ROOT,
    var::TEMPLATE_ROOT,
    var::ALLOWED_NAMESPACES,
    var::SIGNATURE_MODE,
    var::TRUSTED_KEYS,
    var::MANIFEST_BASE_URL,
    var::MANIFEST_TIMEOUT_S,
    var::INSECURE_REGISTRIES,
    var::APP_PUID,
    var::APP_PGID,
    var::CONTAINER_NETWORK,
];
pub const ENROLL_SEED_IMAGE: &str = "QUASAR_ENROLL_SEED_IMAGE";
pub const ENROLL_AGENT_IMAGE: &str = "QUASAR_ENROLL_AGENT_IMAGE";
const CONTROL_ONLY: &[&str] = &[
    var::PUBLIC_HOST,
    var::TLS_HOSTS,
    var::TRUSTED_PROXIES,
    var::HTTP_PORT,
    var::TLS_PORT,
    ENROLL_SEED_IMAGE,
    ENROLL_AGENT_IMAGE,
];

fn opt(v: &str) -> Option<String> {
    let v = v.trim();
    (!v.is_empty()).then(|| v.to_owned())
}

/// The inputs `changes` gives, and which variables actually change a value. Refuses a
/// variable a reconfigure does not take, naming the ones it does, and inputs that fail the
/// install's own checks.
pub fn apply(
    before: &Inputs,
    role: MachineRole,
    changes: &BTreeMap<String, String>,
) -> Result<(Inputs, Vec<String>), String> {
    let (after, ()) = apply_fields(before, role, changes)?;
    checked(before, role, changes, after)
}

fn apply_fields(
    before: &Inputs,
    role: MachineRole,
    changes: &BTreeMap<String, String>,
) -> Result<(Inputs, ()), String> {
    let mut after = before.clone();
    for (key, value) in changes {
        let control_key = CONTROL_ONLY.contains(&key.as_str());
        if !EVERYWHERE.contains(&key.as_str()) && !control_key {
            let mut allowed: Vec<&str> = EVERYWHERE.to_vec();
            if role != MachineRole::Gpu {
                allowed.extend_from_slice(CONTROL_ONLY);
            }
            return Err(format!(
                "{key} is not changed by a reconfigure ({}). A machine's role, node name and database are fixed at install, and its images move by an update. Reconfigurable here: {}",
                match key.as_str() {
                    var::ROLE | var::NODE_NAME => "it is the machine's identity",
                    var::AGENT_IMAGE | var::CONTROL_PLANE_IMAGE | var::POSTGRES_IMAGE => "an image changes by an update",
                    var::ENROLLMENT => "an installed machine keeps its identity",
                    k if k.starts_with("QUASAR_DATABASE_") => "the database is fixed at install",
                    _ => "unknown variable",
                },
                allowed.join(", ")
            ));
        }
        if control_key && after.control.is_none() {
            return Err(format!(
                "{key} configures a control plane, and this machine runs none"
            ));
        }
        let trust = &mut after.trust;
        match key.as_str() {
            var::HOME_ROOT => after.home_root = value.trim().to_owned(),
            var::TEMPLATE_ROOT => after.template_root = value.trim().to_owned(),
            var::ALLOWED_NAMESPACES => trust.allowed_namespaces = opt(value),
            var::SIGNATURE_MODE => trust.signature_mode = opt(value),
            var::TRUSTED_KEYS => trust.trusted_keys = opt(value),
            var::MANIFEST_BASE_URL => trust.manifest_base_url = opt(value),
            var::MANIFEST_TIMEOUT_S => trust.manifest_timeout_s = opt(value),
            var::INSECURE_REGISTRIES => trust.insecure_registries = opt(value),
            var::APP_PUID | var::APP_PGID => {
                let id = match opt(value) {
                    None => None,
                    Some(v) => Some(
                        v.parse::<u32>()
                            .map_err(|_| format!("{key}={v:?} is not a numeric id"))?,
                    ),
                };
                if key == var::APP_PUID {
                    after.app.puid = id;
                } else {
                    after.app.pgid = id;
                }
            }
            var::CONTAINER_NETWORK => after.app.container_network = opt(value),
            _ => {
                let control = after.control.as_mut().expect("checked above");
                match key.as_str() {
                    var::PUBLIC_HOST => control.public_host = opt(value),
                    var::TLS_HOSTS => control.tls_hosts = opt(value),
                    var::TRUSTED_PROXIES => control.trusted_proxies = opt(value),
                    var::HTTP_PORT | var::TLS_PORT => {
                        let port = value
                            .trim()
                            .parse::<u16>()
                            .ok()
                            .filter(|p| *p > 0)
                            .ok_or_else(|| format!("{key}={value:?} is not a port"))?;
                        if key == var::HTTP_PORT {
                            control.http_port = port;
                        } else {
                            control.tls_port = port;
                        }
                    }
                    ENROLL_SEED_IMAGE | ENROLL_AGENT_IMAGE => {
                        let image = match opt(value) {
                            None => None,
                            Some(v) => {
                                Some(ImageRef::parse(&v).map_err(|e| format!("{key}: {e}"))?)
                            }
                        };
                        if key == ENROLL_SEED_IMAGE {
                            after.enroll.seed = image;
                        } else {
                            after.enroll.agent = image;
                        }
                    }
                    _ => unreachable!("every control key is matched"),
                }
            }
        }
    }
    Ok((after, ()))
}

fn checked(
    before: &Inputs,
    role: MachineRole,
    changes: &BTreeMap<String, String>,
    after: Inputs,
) -> Result<(Inputs, Vec<String>), String> {
    recipe::validate(&after).map_err(|e| e.to_string())?;
    var::trust_config(&after.trust)?;
    let mut changed = Vec::new();
    for (key, value) in changes {
        let mut alone = before.clone();
        let one = BTreeMap::from([(key.clone(), value.clone())]);
        assign_all(&mut alone, role, &one)?;
        if alone != *before {
            changed.push(key.clone());
        }
    }
    Ok((after, changed))
}

/// The field assignments of [`apply`], without its whole-input checks.
fn assign_all(
    inputs: &mut Inputs,
    role: MachineRole,
    changes: &BTreeMap<String, String>,
) -> Result<(), String> {
    let (after, _) = apply_fields(inputs, role, changes)?;
    *inputs = after;
    Ok(())
}

/// Why a role cannot be replaced for a reconfigure in this build, if it cannot.
fn not_replaceable(role: Role) -> Option<&'static str> {
    match role {
        Role::NodeAgent => None,
        Role::ControlPlane => Some("re-create the control plane, and a reconfigure does not use control-plane replacement (#363) yet"),
        Role::Postgres => Some("re-create Quasar's Postgres, which is created once and never replaced (#352 R1)"),
        Role::RecoveryActor => Some("re-create the recovery actor itself, which a reconfigure does not do"),
    }
}

fn refuse(reason: Reason, message: impl Into<String>) -> crate::socket::Rejection {
    crate::socket::Rejection {
        request_id: String::new(),
        reason,
        message: message.into(),
    }
}

impl Actor {
    fn secrets_for(&self, role: Role) -> io::Result<SecretMounts> {
        Ok(match role {
            Role::NodeAgent => self.node_agent_secrets().map_err(io::Error::other)?,
            Role::ControlPlane => self.control_plane_secrets().map_err(io::Error::other)?,
            Role::Postgres => SecretMounts {
                volume: Some(names::POSTGRES_SECRETS_VOLUME.into()),
                files: [secrets::DATABASE_PASSWORD.to_string()].into(),
            },
            Role::RecoveryActor => SecretMounts::default(),
        })
    }

    /// The services whose rendered specification `after` moves, in replacement order.
    fn moved_by(&self, before: &Inputs, after: &Inputs) -> Result<Vec<Role>, String> {
        let mut moved = Vec::new();
        for role in [
            Role::NodeAgent,
            Role::ControlPlane,
            Role::Postgres,
            Role::RecoveryActor,
        ] {
            let Some(record) = self.dir.load_service(role).map_err(|e| e.to_string())? else {
                continue;
            };
            let secrets = self.secrets_for(role).map_err(|e| e.to_string())?;
            let render = |inputs: &Inputs| {
                recipe::render(
                    role,
                    record.recipe_revision,
                    inputs,
                    &record.image,
                    &secrets,
                )
                .map(|s| s.labels.get(recipe::labels::SPEC).cloned())
                .map_err(|e| format!("{}: {e}", role.as_str()))
            };
            if render(before)? != render(after)? {
                moved.push(role);
            }
        }
        Ok(moved)
    }

    /// The operator's reconfigure. `Ok` with no `request_id`: nothing needed re-creating
    /// (or a dry run); with one, the replacement is being driven.
    pub fn reconfigure(
        self: &Arc<Self>,
        req: ReconfigureRequest,
    ) -> Result<Planned, crate::socket::Rejection> {
        use std::sync::atomic::Ordering;
        let _gate = self.gate.lock().unwrap();
        if self.resuming.load(Ordering::SeqCst) {
            return Err(refuse(
                Reason::Busy,
                "the recovery actor is still settling this machine after a start; try again in a moment",
            ));
        }
        if let Some(why) = crate::uninstall::uninstalled(&self.dir) {
            return Err(refuse(
                Reason::Invalid,
                format!("{why}; there is nothing to reconfigure"),
            ));
        }
        let machine = match self.dir.load_machine() {
            Ok(Some(m)) => m,
            Ok(None) => return Err(refuse(Reason::Invalid, "this machine is not installed yet")),
            Err(e) => {
                return Err(refuse(
                    Reason::Invalid,
                    format!("machine state is unreadable: {e}"),
                ))
            }
        };
        if req.changes.is_empty() {
            return Err(refuse(
                Reason::Invalid,
                "name at least one VARIABLE=value to change",
            ));
        }
        let (after, changed) = apply(&machine.inputs, machine.role, &req.changes)
            .map_err(|why| refuse(Reason::Invalid, format!("{why}; nothing was changed")))?;
        let replaced = self
            .moved_by(&machine.inputs, &after)
            .map_err(|why| refuse(Reason::Invalid, format!("{why}; nothing was changed")))?;
        if let Some(why) = replaced.iter().find_map(|r| not_replaceable(*r)) {
            return Err(refuse(
                Reason::Invalid,
                format!(
                    "changing {} would {why}, so this build cannot apply it; nothing was changed",
                    changed.join(", ")
                ),
            ));
        }
        let planned = |request_id: Option<String>| Planned {
            changed: changed.clone(),
            replaced: replaced.iter().map(|r| r.as_str().to_string()).collect(),
            request_id,
            dry_run: req.dry_run,
        };
        if req.dry_run || changed.is_empty() {
            return Ok(planned(None));
        }
        let scan = self.journals.scan();
        if let Some(open) = scan.open_id() {
            return Err(refuse(
                Reason::Busy,
                format!("attempt {open} is in flight; reconfigure once it has finished"),
            ));
        }
        // No attempt is open, so a record left here belongs to a finished attempt: settle it
        // now rather than answer busy until the next start. One that cannot be read is
        // never overwritten.
        self.settle_reconfigure();
        match self.dir.reconfigure_file().load() {
            Ok(None) => {}
            Ok(Some(r)) => {
                return Err(refuse(
                    Reason::Busy,
                    format!(
                        "reconfigure {} could not be settled yet (see this actor's log); nothing was changed",
                        r.request_id
                    ),
                ))
            }
            Err(e) => {
                return Err(refuse(
                    Reason::Invalid,
                    format!("{RECORD_FILE} is unreadable ({e}); nothing was changed. The recovery actor's next start sets it aside"),
                ))
            }
        }
        let containers = self.engine.list_containers().map_err(|e| {
            refuse(
                Reason::Busy,
                format!("the container engine did not answer ({e}); nothing was changed"),
            )
        })?;
        let conflicts = crate::race_guard::conflicts(
            &containers,
            Some(&machine.installation_id),
            self.config.self_container.as_deref(),
        );
        if !replaced.is_empty() && !conflicts.is_empty() {
            return Err(refuse(
                Reason::OwnerConflict,
                crate::race_guard::refusal(&conflicts),
            ));
        }

        if replaced.is_empty() {
            let mut next = machine.clone();
            next.inputs = after;
            self.dir.machine().store(&next).map_err(|e| {
                refuse(
                    Reason::Busy,
                    format!("machine state could not be written ({e}); nothing was changed"),
                )
            })?;
            info!(changed = ?changed, "reconfigured: no service needed re-creating");
            return Ok(planned(None));
        }

        let request_id = crate::actor::random_uuid();
        let record = Record {
            format: RECORD_FORMAT,
            request_id: request_id.clone(),
            changed: changed.clone(),
            before: machine.inputs.clone(),
            after: after.clone(),
            replaced: replaced.clone(),
            started_at: (self.config.now)(),
        };
        let journal = self
            .reconfigure_journal(&machine, &request_id, &replaced)
            .map_err(|why| refuse(Reason::Invalid, format!("{why}; nothing was changed")))?;
        // The record, then the inputs, then the attempt (module documentation).
        self.dir.reconfigure_file().store(&record).map_err(|e| {
            refuse(
                Reason::Busy,
                format!("the reconfigure could not be recorded ({e}); nothing was changed"),
            )
        })?;
        let mut next = machine.clone();
        next.inputs = after;
        if let Err(e) = self.dir.machine().store(&next) {
            let _ = remove_record(&self.dir);
            return Err(refuse(
                Reason::Busy,
                format!("machine state could not be written ({e}); nothing was changed"),
            ));
        }
        if let Err(e) = self.journals.store(&journal) {
            self.put_back(&record);
            return Err(refuse(
                Reason::Busy,
                format!("the attempt could not be journalled ({e}); nothing was changed"),
            ));
        }
        let _ = self.journals.mark_used(&request_id);
        info!(request = %request_id, changed = ?changed, replaced = ?record.replaced, "reconfigure admitted");

        let actor = self.clone();
        let id = request_id.clone();
        let mut worker = self.worker.lock().unwrap();
        if let Some(done) = worker.take() {
            let _ = done.join();
        }
        *worker = Some(std::thread::spawn(move || {
            actor.drive(&id);
            actor.settle_reconfigure();
        }));
        Ok(planned(Some(request_id)))
    }

    fn reconfigure_journal(
        &self,
        machine: &Machine,
        request_id: &str,
        roles: &[Role],
    ) -> Result<Journal, String> {
        let mut steps = Vec::new();
        let mut components = Vec::new();
        let mut previous = Vec::new();
        for role in roles {
            let record = self
                .dir
                .load_service(*role)
                .map_err(|e| e.to_string())?
                .ok_or_else(|| format!("machine state has no record of the {}", role.as_str()))?;
            let running = self
                .engine
                .inspect_container(role.container_name())
                .map_err(|e| e.to_string())?;
            let old_digest = running
                .as_ref()
                .and_then(|c| c.image.split_once('@').map(|(_, d)| d.to_owned()));
            components.push(crate::socket::Component {
                name: role.as_str().into(),
                image: record.image.repository.clone(),
                digest: record.image.digest.clone(),
            });
            previous.push(Previous {
                name: role.as_str().into(),
                digest: old_digest.clone(),
            });
            steps.push(Step {
                name: role.as_str().into(),
                image: record.image.clone(),
                phase: Phase::Admitted,
                old_container: None,
                old_restart: None,
                old_digest,
                revision: None,
                spec: None,
                new_container: None,
                failure: None,
                successor_starts: 0,
                migrating: false,
                dump: None,
            });
        }
        let _ = machine;
        let now = (self.config.now)();
        let release = Release {
            id: String::new(),
            version: None,
            source_commit: String::new(),
        };
        let request = Request {
            request_id: request_id.into(),
            kind: RequestKind::Replace,
            components: components.clone(),
            release: release.clone(),
            migrates: false,
            schema_version: None,
            external_backup_confirmed: false,
            dump: None,
            purge: false,
            wait_timeout_s: 0,
            from_version: None,
            force_again: false,
        };
        Ok(Journal {
            format: FORMAT,
            seq: self.journals.next_seq(),
            caller: CallerTag::Operator,
            request,
            steps,
            result: AttemptResult {
                request_id: request_id.into(),
                state: State::Pending,
                reason: None,
                components,
                previous,
                output: String::new(),
                started_at: now.clone(),
                updated_at: now,
                finished_at: None,
                restored: false,
                release,
                dump: None,
            },
            restore: None,
        })
    }

    /// Machine state's inputs back to what they were before `record`.
    fn put_back(&self, record: &Record) {
        match self.dir.load_machine() {
            Ok(Some(mut machine)) => {
                machine.inputs = record.before.clone();
                if let Err(e) = self.dir.machine().store(&machine) {
                    warn!(token = "reconfigure-inputs-not-restored", "the previous machine inputs could not be written back ({e}); the next start tries again");
                    return;
                }
            }
            Ok(None) => {}
            Err(e) => {
                warn!(
                    token = "reconfigure-machine-unreadable",
                    "machine state is unreadable ({e}); the previous inputs are put back on the next start"
                );
                return;
            }
        }
        if let Err(e) = remove_record(&self.dir) {
            warn!(
                token = "reconfigure-record-left",
                "{RECORD_FILE} could not be removed after putting the inputs back ({e})"
            );
        }
    }

    /// Settles a reconfigure whose attempt has reached its outcome, or was never journalled:
    /// the new inputs stay only if the replacement succeeded. Idempotent; `resume` calls it
    /// after settling the open attempt.
    pub(crate) fn settle_reconfigure(&self) {
        self.settle_reconfigure_record(false);
    }

    /// [`Actor::settle_reconfigure`] on a start: a record that cannot be read is set aside
    /// (never deleted or overwritten) and reported, so it does not refuse every later
    /// reconfigure. Machine state keeps its inputs, which are the new ones.
    pub(crate) fn settle_reconfigure_on_start(&self) {
        self.settle_reconfigure_record(true);
    }

    fn settle_reconfigure_record(&self, set_aside: bool) {
        let record = match self.dir.reconfigure_file().load() {
            Ok(Some(r)) => r,
            Ok(None) => return,
            Err(e) if set_aside => {
                let from = self.dir.root().join(RECORD_FILE);
                let to = self.dir.root().join(format!("{RECORD_FILE}.unreadable"));
                match std::fs::rename(&from, &to) {
                    Ok(()) => error!(
                        token = "reconfigure-record-set-aside",
                        "{RECORD_FILE} was unreadable ({e}) and is kept as {}; machine state keeps the inputs of that reconfigure, which the node agent may not run if it did not succeed. Run reconfigure again with the values you want",
                        to.display()
                    ),
                    Err(re) => error!(
                        token = "reconfigure-record-stuck",
                        "{RECORD_FILE} is unreadable ({e}) and could not be set aside ({re}); reconfigure is refused until it is removed"
                    ),
                }
                return;
            }
            Err(e) => {
                warn!(
                    token = "reconfigure-record-unreadable",
                    "{RECORD_FILE} is unreadable ({e}); the recovery actor's next start sets it aside"
                );
                return;
            }
        };
        match self.journals.load(&record.request_id) {
            Ok(Some(j)) if j.is_open() => {}
            Ok(Some(j)) if j.result.state == State::Succeeded => {
                info!(request = %record.request_id, changed = ?record.changed, "reconfigured: the new inputs are in force");
                if let Err(e) = remove_record(&self.dir) {
                    warn!(
                        token = "reconfigure-record-not-cleared",
                        "{RECORD_FILE} could not be removed after the reconfigure succeeded ({e})"
                    );
                }
            }
            Ok(_) => {
                warn!(
                    token = "reconfigure-reverted",
                    request = %record.request_id,
                    "the reconfigure's replacement did not succeed; the previous machine inputs are back in force"
                );
                self.put_back(&record);
            }
            Err(e) => warn!(token = "reconfigure-journal-unreadable", "{e}"),
        }
    }
}

fn remove_record(dir: &crate::machine::MachineDir) -> io::Result<()> {
    let path = dir.root().join(RECORD_FILE);
    match std::fs::remove_file(&path) {
        Ok(()) => std::fs::File::open(dir.root())?.sync_all(),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e),
    }
}

/// The variables a reconfigure takes on a machine of `role`, for help text.
pub fn variables(role: Option<MachineRole>) -> Vec<&'static str> {
    let mut out: BTreeSet<&str> = EVERYWHERE.iter().copied().collect();
    if role != Some(MachineRole::Gpu) {
        out.extend(CONTROL_ONLY.iter().copied());
    }
    out.into_iter().collect()
}
