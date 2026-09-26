//! `quasar-recovery reconfigure` (#352 decision A2): changing a machine's inputs (its home
//! root, public host, ports, release trust, app-container defaults) after install.
//!
//! A reconfigure is a **Replacement with the same digests and new machine inputs**: each
//! service whose rendered specification the change moves is replaced through the attempt
//! machinery ([`crate::replace`]) exactly as an update replaces it: journalled, the old
//! container kept until the new one verifies, restored if it does not, with the image it
//! already runs. A change no container renders (the recovery actor's own signature policy,
//! say) needs no replacement and is simply recorded. The control plane goes first: a
//! combined host's agent dials it at the loopback of its HTTP port. Postgres and the
//! recovery actor are never replaced by a reconfigure (#352 R1), and the database and the
//! node name are not reconfigurable: they are changed by reinstalling.
//!
//! **Order, and why it is crash-safe.** `reconfigure.json` (the inputs before and after) is
//! committed first, then machine state takes the new inputs, then the attempt is journalled
//! and driven. `resume` settles the attempt first (D8), then [`Actor::settle_reconfigure`]
//! reads the record and writes its [`Outcome`] into it, from the services' own records
//! (`services/<role>.json` is written only once a replacement verified):
//!
//! | the attempt | services on the new inputs | machine state keeps | outcome |
//! |---|---|---|---|
//! | succeeded | all | the new inputs | `applied` |
//! | failed, interrupted, or never journalled | none | the old inputs, put back | `put_back` |
//! | failed after an earlier service verified | some | the new inputs | `partial` |
//!
//! A `partial` outcome names the services still on their previous specification
//! ([`Outcome::behind`]); the same reconfigure run again re-creates them. So machine state
//! never holds inputs no running service was verified with.

use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tracing::{error, info, warn};

use crate::actor::Actor;
use crate::bootstrap as var;
use crate::journal::{CallerTag, Journal, Phase, Step, FORMAT};
use crate::recipe::{self, labels, names, secrets, ImageRef, Inputs, Role, SecretMounts};
use crate::socket::{
    AttemptResult, MachineRole, Previous, Reason, Release, Request, RequestKind, State,
};

pub const RECORD_FILE: &str = "reconfigure.json";
const RECORD_FORMAT: u32 = 1;

/// `reconfigure.json`: the last reconfigure that replaced a service, and once settled its
/// outcome. Not a frozen interface; an older actor reads it and ignores `outcome`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Record {
    pub format: u32,
    pub request_id: String,
    pub changed: Vec<String>,
    pub before: Inputs,
    pub after: Inputs,
    pub replaced: Vec<Role>,
    pub started_at: String,
    /// `None` while the reconfigure is in flight or not yet settled.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub outcome: Option<Outcome>,
}

/// How a reconfigure ended (the module's table).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Outcome {
    pub settled: Settled,
    /// The attempt's terminal state and reason; `None` when it was never journalled.
    pub state: Option<State>,
    pub reason: Option<Reason>,
    /// The attempt put a kept container back.
    pub restored: bool,
    /// The services not on the inputs machine state keeps (always on `partial`): the same
    /// reconfigure run again re-creates them.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub behind: Vec<Role>,
    pub settled_at: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Settled {
    Applied,
    PutBack,
    Partial,
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
    /// The services re-created with the new inputs, in order (`control-plane`, `node-agent`).
    pub replaced: Vec<String>,
    /// The replacement's attempt, once admitted: `GET /v1/status?request_id=` follows it.
    pub request_id: Option<String>,
    pub dry_run: bool,
    /// What the operator should know before saying `--yes`: sessions it ends, hosts it
    /// cuts off, what it does not change.
    #[serde(default)]
    pub notes: Vec<String>,
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
pub const ENROLL_SEED_IMAGE: &str = var::ENROLL_SEED_IMAGE;
pub const ENROLL_AGENT_IMAGE: &str = var::ENROLL_AGENT_IMAGE;
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

/// Why `key` is not reconfigurable, naming what changes it instead.
fn not_reconfigurable(key: &str) -> &'static str {
    match key {
        var::NODE_NAME => "the node name is this machine's identity, which the control plane knows its host by. To change it, reinstall: `quasar-recovery uninstall`, then install again with the seed and the new QUASAR_NODE_NAME (a GPU host is then added again from Add host)",
        var::ROLE => "a machine's role is fixed at install. To change it, reinstall: `quasar-recovery uninstall`, then install again with the seed and the new QUASAR_ROLE",
        var::AGENT_IMAGE | var::CONTROL_PLANE_IMAGE | var::POSTGRES_IMAGE => {
            "an image changes by an update (Fleet ▸ Releases), never by a reconfigure"
        }
        var::ENROLLMENT => "an installed machine keeps its identity",
        k if k.starts_with("QUASAR_DATABASE_") => "the database is fixed at install. Changing the database mode (Quasar's own Postgres or your own database) or the database itself moves data, which a reconfigure never does. To change it, reinstall: back the database up, run `quasar-recovery uninstall`, install again with the seed and the new QUASAR_DATABASE_* inputs, and load your data into the new database",
        _ => "unknown variable",
    }
}

/// The inputs `changes` gives, and which variables actually change a value. Refuses a
/// variable a reconfigure does not take, naming the ones it does, and inputs that fail the
/// install's own checks.
pub fn apply(
    before: &Inputs,
    role: MachineRole,
    changes: &BTreeMap<String, String>,
) -> Result<(Inputs, Vec<String>), String> {
    let after = apply_fields(before, role, changes)?;
    checked(before, role, changes, after)
}

fn apply_fields(
    before: &Inputs,
    role: MachineRole,
    changes: &BTreeMap<String, String>,
) -> Result<Inputs, String> {
    let mut after = before.clone();
    for (key, value) in changes {
        let control_key = CONTROL_ONLY.contains(&key.as_str());
        if !EVERYWHERE.contains(&key.as_str()) && !control_key {
            return Err(format!(
                "{key} is not changed by a reconfigure: {}. Reconfigurable here: {}",
                not_reconfigurable(key),
                variables(Some(role)).join(", ")
            ));
        }
        if control_key && after.control.is_none() {
            return Err(format!(
                "{key} configures a control plane, and this machine runs none"
            ));
        }
        if key == var::HOME_ROOT && role == MachineRole::ControlOnly {
            return Err(format!(
                "{key}: a control-only machine runs no node agent, so it has no home root"
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
                    // The seed's `QUASAR_ENROLL_*` are the operator's overrides (#365); the
                    // install-time images stay the fallback.
                    ENROLL_SEED_IMAGE | ENROLL_AGENT_IMAGE => {
                        let image = match opt(value) {
                            None => None,
                            Some(v) => {
                                Some(ImageRef::parse(&v).map_err(|e| format!("{key}: {e}"))?)
                            }
                        };
                        if key == ENROLL_SEED_IMAGE {
                            after.enroll.seed_override = image;
                        } else {
                            after.enroll.agent_override = image;
                        }
                    }
                    _ => unreachable!("every control key is matched"),
                }
            }
        }
    }
    Ok(after)
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
        let one = BTreeMap::from([(key.clone(), value.clone())]);
        if apply_fields(before, role, &one)? != *before {
            changed.push(key.clone());
        }
    }
    Ok((after, changed))
}

/// Why a role cannot be replaced by a reconfigure, if it cannot.
fn not_replaceable(role: Role) -> Option<&'static str> {
    match role {
        Role::NodeAgent | Role::ControlPlane => None,
        Role::Postgres => {
            Some("re-create Quasar's Postgres, which is created once and never replaced (#352 R1)")
        }
        Role::RecoveryActor => {
            Some("re-create the recovery actor itself, which a reconfigure does not do")
        }
    }
}

/// The order a reconfigure replaces in: the control plane before the agent that dials it.
const ORDER: [Role; 4] = [
    Role::ControlPlane,
    Role::NodeAgent,
    Role::Postgres,
    Role::RecoveryActor,
];

fn refuse(reason: Reason, message: impl Into<String>) -> crate::socket::Rejection {
    crate::socket::Rejection {
        request_id: String::new(),
        reason,
        message: message.into(),
    }
}

/// What the operator is told before a reconfigure of `changed` replacing `replaced`.
fn notes(role: MachineRole, changed: &[String], replaced: &[Role]) -> Vec<String> {
    let mut out = Vec::new();
    let changes = |k: &str| changed.iter().any(|c| c == k);
    if replaced.contains(&Role::ControlPlane) {
        out.push("Re-creating the control plane restarts the console and drops every agent's connection for a moment; running sessions keep streaming and agents reconnect.".to_string());
    }
    if replaced.contains(&Role::NodeAgent) {
        out.push("Re-creating the node agent ends this host's sessions.".to_string());
    }
    if changes(var::TLS_PORT) {
        out.push(format!(
            "GPU hosts added with Add host dial the HTTPS port in their enrollment string: after {} changes they stop connecting until they are added again{}.",
            var::TLS_PORT,
            if role == MachineRole::ControlOnly {
                ""
            } else {
                " (this machine's own agent is not affected)"
            }
        ));
    }
    if changes(var::PUBLIC_HOST) || changes(var::TLS_HOSTS) {
        out.push(format!(
            "The control plane keeps its certificate, which agents pin, so {} and {} reach the certificate only when it is re-issued (docs/configuration.md, QUASAR_TLS_DIR).",
            var::PUBLIC_HOST,
            var::TLS_HOSTS
        ));
    }
    out
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

    /// The `io.quasar.spec` of `role` rendered with `inputs` on its recorded revision and
    /// image; `None` when machine state has no record of it.
    fn spec_with(&self, role: Role, inputs: &Inputs) -> Result<Option<String>, String> {
        let Some(record) = self.dir.load_service(role).map_err(|e| e.to_string())? else {
            return Ok(None);
        };
        let secrets = self.secrets_for(role).map_err(|e| e.to_string())?;
        recipe::render(
            role,
            record.recipe_revision,
            inputs,
            &record.image,
            &secrets,
        )
        .map(|s| s.labels.get(labels::SPEC).cloned())
        .map_err(|e| format!("{}: {e}", role.as_str()))
    }

    /// Whether `role`'s verified specification is the one `inputs` render.
    fn runs(&self, role: Role, inputs: &Inputs) -> bool {
        match (self.dir.load_service(role), self.spec_with(role, inputs)) {
            (Ok(Some(record)), Ok(Some(spec))) => spec == record.spec_digest,
            _ => false,
        }
    }

    /// The services whose rendered specification `after` moves, in replacement order,
    /// with those a partly applied reconfigure left behind that `after` still moves.
    fn moved_by(
        &self,
        before: &Inputs,
        after: &Inputs,
        behind: &[Role],
    ) -> Result<Vec<Role>, String> {
        let mut moved = Vec::new();
        for role in ORDER {
            let Some(new) = self.spec_with(role, after)? else {
                continue;
            };
            let catch_up = behind.contains(&role) && !self.runs(role, after);
            if self.spec_with(role, before)? != Some(new) || catch_up {
                moved.push(role);
            }
        }
        Ok(moved)
    }

    /// The services the last reconfigure left on their previous specification.
    fn left_behind(&self) -> Vec<Role> {
        match self.dir.reconfigure_file().load() {
            Ok(Some(Record {
                outcome: Some(o), ..
            })) => o.behind,
            _ => Vec::new(),
        }
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
            .moved_by(&machine.inputs, &after, &self.left_behind())
            .map_err(|why| refuse(Reason::Invalid, format!("{why}; nothing was changed")))?;
        if let Some(why) = replaced.iter().find_map(|r| not_replaceable(*r)) {
            return Err(refuse(
                Reason::Invalid,
                format!(
                    "changing {} would {why}, so a reconfigure cannot apply it; nothing was changed",
                    changed.join(", ")
                ),
            ));
        }
        let planned = |request_id: Option<String>| Planned {
            changed: changed.clone(),
            replaced: replaced.iter().map(|r| r.as_str().to_string()).collect(),
            request_id,
            dry_run: req.dry_run,
            notes: notes(machine.role, &changed, &replaced),
        };
        if req.dry_run || (changed.is_empty() && replaced.is_empty()) {
            return Ok(planned(None));
        }
        let scan = self.journals.scan();
        if let Some(open) = scan.open_id() {
            return Err(refuse(
                Reason::Busy,
                format!("attempt {open} is in flight; reconfigure once it has finished"),
            ));
        }
        // No attempt is open, so a record left unsettled belongs to a finished attempt:
        // settle it now rather than answer busy until the next start. One that cannot be
        // read is never overwritten.
        self.settle_reconfigure();
        match self.dir.reconfigure_file().load() {
            Ok(None) => {}
            Ok(Some(r)) if r.outcome.is_some() => {}
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
        if replaced.contains(&Role::ControlPlane) {
            match crate::database::load_hold(self.dir.root()) {
                Ok(None) => {}
                Ok(Some(_)) => {
                    return Err(refuse(
                        Reason::Invalid,
                        "a restore holds this machine's database (it has not finished), so no control plane is started; run the restore command again first. Nothing was changed",
                    ))
                }
                Err(e) => {
                    return Err(refuse(
                        Reason::Busy,
                        format!("database-hold.json cannot be read ({e}); nothing was changed"),
                    ))
                }
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
            outcome: None,
        };
        let journal = self
            .reconfigure_journal(&request_id, &replaced)
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
            self.undo_admission(&record);
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

    /// The attempt: one step per service, each on the image machine state records for it,
    /// which must be the image it runs, so a reconfigure can never be a migration.
    fn reconfigure_journal(&self, request_id: &str, roles: &[Role]) -> Result<Journal, String> {
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
            if let Some(digest) = old_digest.as_ref().filter(|d| **d != record.image.digest) {
                return Err(format!(
                    "the running {} is on {digest}, not on {}, which machine state records for it; a reconfigure keeps a service's image, so settle that first (an update, or `quasar-recovery restore`)",
                    role.as_str(),
                    record.image.digest
                ));
            }
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

    /// An admission that could not journal its attempt: nothing happened, so the inputs go
    /// back and the record goes.
    fn undo_admission(&self, record: &Record) {
        if self.write_inputs(&record.before) {
            let _ = remove_record(&self.dir);
        }
    }

    /// Machine state's inputs set to `inputs`; `false` (logged) when they could not be.
    fn write_inputs(&self, inputs: &Inputs) -> bool {
        match self.dir.load_machine() {
            Ok(Some(mut machine)) => {
                if machine.inputs == *inputs {
                    return true;
                }
                machine.inputs = inputs.clone();
                if let Err(e) = self.dir.machine().store(&machine) {
                    warn!(token = "reconfigure-inputs-not-restored", "the previous machine inputs could not be written back ({e}); the next start tries again");
                    return false;
                }
                true
            }
            Ok(None) => true,
            Err(e) => {
                warn!(
                    token = "reconfigure-machine-unreadable",
                    "machine state is unreadable ({e}); the previous inputs are put back on the next start"
                );
                false
            }
        }
    }

    /// Settles a reconfigure whose attempt has reached its outcome, or was never journalled
    /// (the module's table). Idempotent; `resume` calls it after settling the open attempt.
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
        let mut record = match self.dir.reconfigure_file().load() {
            Ok(Some(r)) if r.outcome.is_some() => return,
            Ok(Some(r)) => r,
            Ok(None) => return,
            Err(e) if set_aside => {
                let from = self.dir.root().join(RECORD_FILE);
                let to = self.dir.root().join(format!("{RECORD_FILE}.unreadable"));
                match std::fs::rename(&from, &to) {
                    Ok(()) => error!(
                        token = "reconfigure-record-set-aside",
                        "{RECORD_FILE} was unreadable ({e}) and is kept as {}; machine state keeps the inputs of that reconfigure, which its services may not run if it did not succeed. Run reconfigure again with the values you want",
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
        let journal = match self.journals.load(&record.request_id) {
            Ok(Some(j)) if j.is_open() => return,
            Ok(j) => j,
            Err(e) => {
                warn!(token = "reconfigure-journal-unreadable", "{e}");
                return;
            }
        };
        let outcome = self.outcome_of(&record, journal.as_ref());
        let inputs = match outcome.settled {
            Settled::PutBack => &record.before,
            Settled::Applied | Settled::Partial => &record.after,
        };
        if !self.write_inputs(inputs) {
            return;
        }
        match outcome.settled {
            Settled::Applied => {
                info!(request = %record.request_id, changed = ?record.changed, "reconfigured: the new inputs are in force")
            }
            Settled::PutBack => warn!(
                token = "reconfigure-reverted",
                request = %record.request_id,
                "the reconfigure's replacement did not succeed; the previous machine inputs are back in force"
            ),
            Settled::Partial => warn!(
                token = "reconfigure-partial",
                request = %record.request_id,
                behind = ?outcome.behind,
                "the reconfigure was partly applied: the new inputs stay in force, and the services it could not re-create run their previous specification until the same reconfigure is run again"
            ),
        }
        record.outcome = Some(outcome);
        if let Err(e) = self.dir.reconfigure_file().store(&record) {
            warn!(
                token = "reconfigure-outcome-unrecorded",
                "the reconfigure's outcome could not be written to {RECORD_FILE} ({e}); the next start settles it again"
            );
        }
    }

    /// The module's table, from the attempt's result and the services' verified records.
    fn outcome_of(&self, record: &Record, journal: Option<&Journal>) -> Outcome {
        let (state, reason, restored) = match journal {
            Some(j) => (
                Some(j.result.state),
                j.result.reason.clone(),
                j.result.restored,
            ),
            None => (None, None, false),
        };
        let not_moved = record
            .replaced
            .iter()
            .filter(|role| !self.runs(**role, &record.after))
            .count();
        let settled = if state == Some(State::Succeeded) {
            Settled::Applied
        } else if journal.is_none() || not_moved == record.replaced.len() {
            Settled::PutBack
        } else if not_moved == 0 {
            Settled::Applied
        } else {
            Settled::Partial
        };
        let in_force = match settled {
            Settled::PutBack => &record.before,
            Settled::Applied | Settled::Partial => &record.after,
        };
        // Whatever the outcome: a failed catch-up of a partial reconfigure is put back to
        // inputs its service still does not run.
        let behind = [Role::ControlPlane, Role::NodeAgent]
            .into_iter()
            .filter(|role| matches!(self.dir.load_service(*role), Ok(Some(_))))
            .filter(|role| !self.runs(*role, in_force))
            .collect();
        Outcome {
            behind,
            settled,
            state,
            reason,
            restored,
            settled_at: (self.config.now)(),
        }
    }

    /// The last reconfigure's record, for the operator socket.
    pub fn reconfigure_record(&self) -> io::Result<Option<Record>> {
        self.dir.reconfigure_file().load()
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
    if role == Some(MachineRole::ControlOnly) {
        out.remove(var::HOME_ROOT);
    }
    out.into_iter().collect()
}
