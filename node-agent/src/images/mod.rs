//! Agent-side image ensure/build/remove: pull, build or rmi catalog images in this
//! host's docker daemon, reporting progress via `image_state`. Wire contract:
//! agent-api.md image-management; docker goes through
//! `session::container::ContainerRuntime`, never a second docker dependency.
//!
//! One operation per `image_id` at a time. Every op for an image serializes through
//! a per-image slot: one running, plus the single latest pending (a newer request
//! replaces an older pending one), promoted atomically when the running op finishes.
//! So no two workers ever mutate one `ImageRecord`, and a superseded worker must not
//! commit stale state — each carries the slot generation it started under and
//! re-checks it in [`ImageManager::commit`].

mod build;
mod disk;
mod errors;
mod progress;
mod semaphore;
mod state;

use std::collections::{BTreeMap, HashMap};
use std::io::BufRead;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use crate::messages::{AgentMsg, RegisterImageEntry};
use crate::session::container::ContainerRuntime;

pub use state::ImageState;

use progress::{ProgressThrottle, PullProgressTracker};
use semaphore::CountingSemaphore;
use state::{ImageRecord, StagedTarget};

/// Cap on simultaneous pulls, so an ensure storm never starves a live session's IO
/// (agent-api.md).
const MAX_CONCURRENT_PULLS: usize = 2;

/// Wall-clock bound on one pull. Wider than `container::RUNTIME_PULL_TIMEOUT`: an
/// ensure storm can leave a pull queued behind the semaphore before it starts.
const PULL_TIMEOUT: Duration = Duration::from_secs(30 * 60);

/// Watchdog tick. The deadline must be enforced by that loop, not by the stdout
/// reader: a registry that connects then goes silent emits no line, so a per-line
/// check never fires and the child runs forever holding a pull permit.
const PULL_POLL_INTERVAL: Duration = Duration::from_millis(100);

/// Field bounds. The control plane is trusted, but these strings become map keys,
/// log lines, persisted JSON and argv, and an unbounded one is unbounded memory.
/// Generous versus any real value; a violation is a loud `ack{ok:false}`.
const MAX_IMAGE_ID_LEN: usize = 128;
const MAX_VERSION_LEN: usize = 128;
const MAX_REGISTRY_REF_LEN: usize = 512;

/// Cap on distinct images with an in-flight op, which also bounds the threads this
/// module creates (one per slot; pull concurrency is capped separately). Only a NEW
/// `image_id` is refused when full, so re-acks and pending replacement keep working.
const MAX_CONCURRENT_OPS: usize = 128;

/// `image_build` field bounds; same rationale as the ensure bounds above.
const MAX_LOCAL_TAG_LEN: usize = 512;
const MAX_CONTEXT_URL_LEN: usize = 2048;
const MAX_CONTEXT_SUBDIR_LEN: usize = 512;
const MAX_DOCKERFILE_LEN: usize = 512;
const MAX_BUILD_ARGS: usize = 128;
const MAX_BUILD_ARG_LEN: usize = 4096;

/// Wall-clock bound on one `docker build`, enforced by the same output-independent
/// watchdog as [`PULL_TIMEOUT`].
const BUILD_TIMEOUT: Duration = Duration::from_secs(60 * 60);

/// Tail of each `docker build` stream kept for failure classification and the local
/// failure log. A tail, not a prefix: the failing step's error is the LAST thing docker
/// prints. Both streams are still drained to EOF past this so a verbose build never
/// wedges on a full pipe; what is kept is never sent upstream.
const BUILD_LOG_KEEP_BYTES: usize = 64 * 1024;

/// Per-stream cap on the tail included in the local `image-build-output` log line.
const BUILD_LOG_LOG_BYTES: usize = 4096;

/// Build-context staging dir under the state file's parent. Each build gets a unique
/// child, removed on completion either way.
const BUILD_SCRATCH_DIR: &str = "image-build-scratch";

/// Drop bytes from the front so `buf` holds at most the last `cap` bytes.
fn keep_tail(buf: &mut Vec<u8>, cap: usize) {
    if buf.len() > cap {
        buf.drain(..buf.len() - cap);
    }
}

/// Last `cap` bytes of `s`, advanced to a char boundary.
fn tail(s: &str, cap: usize) -> &str {
    if s.len() <= cap {
        return s;
    }
    let mut i = s.len() - cap;
    while !s.is_char_boundary(i) {
        i += 1;
    }
    &s[i..]
}

/// One managed-image operation: at most one running per `image_id`, at most one
/// pending behind it.
#[derive(Debug, Clone, PartialEq, Eq)]
enum ImageOp {
    Ensure {
        version: String,
        registry_ref: String,
    },
    /// Template build. `local_tag` lands in the same record field as `Ensure`'s
    /// `registry_ref`, so `image_remove` and `register.images[]` work unchanged.
    Build {
        version: String,
        local_tag: String,
        context_url: String,
        context_subdir: String,
        dockerfile: String,
        build_args: BTreeMap<String, String>,
    },
    Remove,
}

/// What is running now (generation stamped on its worker) and the one queued follow-up.
#[derive(Debug)]
struct OpSlot {
    running: ImageOp,
    generation: u64,
    pending: Option<ImageOp>,
}

/// What a command should do given the current record and op slot. Pure (no locks,
/// no I/O), so the serialization state machine is testable without threads or docker.
#[derive(Debug, Clone, PartialEq, Eq)]
enum OpDecision {
    /// The latest intent already equals the request: ack, re-emit, do nothing else.
    ReAck,
    Start(ImageOp),
    /// Becomes the latest pending op, replacing any older one.
    Queue(ImageOp),
    /// `image_remove` for an unrecorded `image_id` with nothing running: ack +
    /// synthetic `absent`, no record, no docker call (agent-api.md `image_remove`).
    AbsentUnknown,
}

fn decide_ensure(
    existing: Option<&ImageRecord>,
    running: Option<&ImageOp>,
    pending: Option<&ImageOp>,
    requested_version: &str,
    registry_ref: &str,
) -> OpDecision {
    let want = ImageOp::Ensure {
        version: requested_version.to_string(),
        registry_ref: registry_ref.to_string(),
    };
    match running {
        // Only an already-ready record at this version is idempotent: a prior failure
        // at the same version must retry, not no-op.
        None => match existing {
            Some(rec) if rec.state == ImageState::Ready && rec.version == requested_version => {
                OpDecision::ReAck
            }
            _ => OpDecision::Start(want),
        },
        // Compare against the LATEST intent: pending if there is one, else running.
        Some(run) => {
            let latest = pending.unwrap_or(run);
            if *latest == want {
                OpDecision::ReAck
            } else {
                OpDecision::Queue(want)
            }
        }
    }
}

/// Build analogue of [`decide_ensure`], same state machine.
#[allow(clippy::too_many_arguments)]
fn decide_build(
    existing: Option<&ImageRecord>,
    running: Option<&ImageOp>,
    pending: Option<&ImageOp>,
    want: ImageOp,
    requested_version: &str,
) -> OpDecision {
    match running {
        None => match existing {
            Some(rec) if rec.state == ImageState::Ready && rec.version == requested_version => {
                OpDecision::ReAck
            }
            _ => OpDecision::Start(want),
        },
        Some(run) => {
            let latest = pending.unwrap_or(run);
            if *latest == want {
                OpDecision::ReAck
            } else {
                OpDecision::Queue(want)
            }
        }
    }
}

fn decide_remove(
    existing: Option<&ImageRecord>,
    running: Option<&ImageOp>,
    pending: Option<&ImageOp>,
) -> OpDecision {
    match running {
        None => match existing {
            None => OpDecision::AbsentUnknown,
            Some(_) => OpDecision::Start(ImageOp::Remove),
        },
        Some(run) => {
            let latest = pending.unwrap_or(run);
            if *latest == ImageOp::Remove {
                OpDecision::ReAck
            } else {
                OpDecision::Queue(ImageOp::Remove)
            }
        }
    }
}

/// A worker may commit only while it still owns its image's op slot at the generation
/// it started under. Anything else means it was superseded and its result is stale;
/// it must not overwrite the newer state.
fn should_commit(slot_generation: Option<u64>, worker_generation: u64) -> bool {
    slot_generation == Some(worker_generation)
}

/// agent-api.md requires a concrete immutable `registry_ref`, never a floating tag,
/// so exactly two shapes are accepted (malformed ⇒ `ack{ok:false}`):
///
/// * `<name>@sha256:<64 hex>`
/// * `<name>:sha-<7..=64 hex>`
///
/// `<name>` must be non-empty, contain a `/`, hold no whitespace or control chars,
/// and not start with `-`, which the docker CLI would read as a flag.
fn looks_like_a_concrete_ref(registry_ref: &str) -> bool {
    let r = registry_ref;
    if r.is_empty() || r.starts_with('-') {
        return false;
    }
    if r.chars().any(|c| c.is_whitespace() || c.is_control()) {
        return false;
    }
    if let Some((name, digest)) = r.split_once('@') {
        return valid_name(name) && is_sha256_digest(digest);
    }
    match r.rfind(':') {
        Some(idx) => {
            let (name, tag) = (&r[..idx], &r[idx + 1..]);
            valid_name(name) && is_sha_tag(tag)
        }
        None => false,
    }
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.contains('/')
        && !name.starts_with('-')
        && !name.contains('@')
        // "/x", "x/", "a//b" are not refs.
        && name.split('/').all(|seg| !seg.is_empty())
}

fn is_sha256_digest(digest: &str) -> bool {
    match digest.strip_prefix("sha256:") {
        Some(hex) => hex.len() == 64 && hex.bytes().all(|b| b.is_ascii_hexdigit()),
        None => false,
    }
}

fn is_sha_tag(tag: &str) -> bool {
    match tag.strip_prefix("sha-") {
        Some(hex) => (7..=64).contains(&hex.len()) && hex.bytes().all(|b| b.is_ascii_hexdigit()),
        None => false,
    }
}

/// Guards `image_build.local_tag` only against values that would corrupt argv or the
/// record: empty, leading `-` (a docker CLI flag), or whitespace/control chars. The
/// `quasar-local/` prefix is a control-plane convention, not a wire invariant, so it
/// is not required here.
fn looks_like_a_local_tag(tag: &str) -> bool {
    !tag.is_empty()
        && !tag.starts_with('-')
        && !tag.chars().any(|c| c.is_whitespace() || c.is_control())
}

fn ack(id: String, ok: bool, error: Option<String>) -> AgentMsg {
    AgentMsg::Ack { id, ok, error }
}

/// #488: observer of this host's image lifecycle. A local observer, so the
/// golden-home warm-up needs no downstream message and no `agent-api.md` amendment.
///
/// Implementations must not block: called on an image worker thread between the
/// docker operation and its `image_state` emission.
pub trait ImageLifecycleObserver: Send + Sync {
    fn image_ready(&self, image_id: &str, registry_ref: &str, version: &str);
    fn image_removed(&self, image_id: &str);
}

/// Owns the managed-image record map, the per-image op serialization, and the pull
/// concurrency cap.
///
/// One instance per process, constructed OUTSIDE the WS reconnect loop
/// (`agent.rs::run`): pull threads outlive a disconnect, so a per-connection manager
/// would duplicate pulls across connection generations. The upstream `image_state`
/// sender is therefore attachable per connection.
///
/// `image_state` delivery:
/// * Progress emits are lossy (`try_send`, never blocking) — the next tick re-emits.
/// * Terminal emits (`ready`/`failed`/`absent`) must not be lost: nothing follows
///   them, so a dropped one strands the control plane at `pulling`. Workers send
///   them blocking; an undeliverable one is only logged.
/// * On attach, every record with no active op is re-emitted unconditionally. That
///   is the recovery path for a lost terminal, so no per-record failure bookkeeping
///   exists. Records with an active op are skipped; their worker emits their terminal.
pub struct ImageManager {
    runtime: ContainerRuntime,
    state_path: String,
    records: Mutex<BTreeMap<String, ImageRecord>>,
    /// The serialization guard, and the idempotency guard: a matching running or
    /// pending op re-acks instead of starting.
    ops: Mutex<HashMap<String, OpSlot>>,
    generations: AtomicU64,
    semaphore: Arc<CountingSemaphore>,
    upstream: RwLock<Option<mpsc::Sender<AgentMsg>>>,
    /// The golden-home warm-up scheduler, attached per connection alongside
    /// `upstream`; `None` on a host with the feature off.
    lifecycle: RwLock<Option<Arc<dyn ImageLifecycleObserver>>>,
}

/// Detaches the upstream sender on every connection-end path, `?` included, so pull
/// threads outliving the connection emit into nothing rather than a dead channel.
pub struct UpstreamGuard {
    mgr: Arc<ImageManager>,
}

impl Drop for UpstreamGuard {
    fn drop(&mut self) {
        *self.mgr.upstream.write().unwrap() = None;
    }
}

impl ImageManager {
    /// Load persisted state, then verify each record against the live docker daemon
    /// (agent-api.md). Blocking, N docker round-trips: call it ONCE at process start,
    /// off the async connect path, so a reconnect storm never pays it repeatedly.
    pub fn new(runtime: ContainerRuntime, state_path: String) -> Arc<Self> {
        let mut loaded = state::load(&state_path);
        for (image_id, rec) in loaded.iter_mut() {
            // A staged (in-flight) ensure did not survive the restart.
            rec.staged = None;
            if rec.registry_ref.is_empty() {
                rec.state = ImageState::Absent;
                continue;
            }
            let was = rec.state;
            match runtime.image_present(&rec.registry_ref) {
                Ok(true) => rec.state = ImageState::Ready,
                Ok(false) => rec.state = ImageState::Absent,
                Err(e) => {
                    // A daemon hiccup is not "the image is gone": never demote (and
                    // persist that demotion) on a transient inspect error.
                    warn!(
                        token = "image-startup-inspect-failed","image {image_id}: startup inspect failed ({e}); keeping recorded state {was:?}");
                    continue;
                }
            }
            if was != rec.state {
                info!(
                    "image {image_id}: reconciled {was:?} -> {:?} against docker daemon at startup",
                    rec.state
                );
            }
        }
        state::save(&state_path, &loaded);
        Arc::new(ImageManager {
            runtime,
            state_path,
            records: Mutex::new(loaded),
            ops: Mutex::new(HashMap::new()),
            generations: AtomicU64::new(0),
            semaphore: CountingSemaphore::new(MAX_CONCURRENT_PULLS),
            upstream: RwLock::new(None),
            lifecycle: RwLock::new(None),
        })
    }

    /// Attach the warm-up scheduler, replacing any previous one so reconnects
    /// re-point rather than accumulate observers.
    pub fn set_lifecycle_observer(&self, observer: Option<Arc<dyn ImageLifecycleObserver>>) {
        *self.lifecycle.write().unwrap() = observer;
    }

    /// The lock is released before the callback: a slow observer must never wedge a
    /// reconnect, which takes the write lock.
    fn notify_ready(&self, image_id: &str, registry_ref: &str, version: &str) {
        let observer = self.lifecycle.read().unwrap().clone();
        if let Some(o) = observer {
            o.image_ready(image_id, registry_ref, version);
        }
    }

    fn notify_removed(&self, image_id: &str) {
        let observer = self.lifecycle.read().unwrap().clone();
        if let Some(o) = observer {
            o.image_removed(image_id);
        }
    }

    /// Attach this connection's `image_state` channel and flush every op-free record
    /// onto it. The returned guard detaches on drop.
    pub fn attach_upstream(self: &Arc<Self>, tx: mpsc::Sender<AgentMsg>) -> UpstreamGuard {
        *self.upstream.write().unwrap() = Some(tx);
        self.flush_op_free_states();
        UpstreamGuard { mgr: self.clone() }
    }

    /// The delivery half of the terminal-state guarantee, and a resync for a
    /// reconnecting control plane. On the async connect path, so it must use the
    /// non-blocking emit; anything that still fails waits for the next attach.
    fn flush_op_free_states(&self) {
        let ids: Vec<String> = {
            let ops = self.ops.lock().unwrap();
            let records = self.records.lock().unwrap();
            records
                .keys()
                .filter(|id| !ops.contains_key(*id))
                .cloned()
                .collect()
        };
        for id in ids {
            self.emit(&id);
        }
    }

    /// `register.images[]` (agent-api.md). Always sent, even when empty: omission is
    /// reserved for pre-amendment agents, and the control plane only demotes stale
    /// `ready` rows when the field is present. An agent that lost its state file must
    /// report `[]`, or the host stays falsely `ready`.
    pub fn register_images(&self) -> Vec<RegisterImageEntry> {
        self.records
            .lock()
            .unwrap()
            .iter()
            .map(|(image_id, rec)| RegisterImageEntry {
                image_id: image_id.clone(),
                version: rec.wire_version().to_string(),
                state: rec.state.as_wire_str().to_string(),
            })
            .collect()
    }

    /// Reverse-lookup the `image_id` that ensured `registry_ref`. A launched container
    /// carries only the resolved ref (`AppSpec.image` has no `image_id`, and adding
    /// one is a frozen-interface change), but the golden-home template store is keyed
    /// by `image_id`, so this is the seeding hook's only way back.
    ///
    /// `None` (untracked launch, or a ref never ensured here) skips the template
    /// lookup; it is never a launch failure.
    pub fn image_id_for_ref(&self, registry_ref: &str) -> Option<String> {
        self.records
            .lock()
            .unwrap()
            .iter()
            .find(|(_, rec)| rec.registry_ref == registry_ref)
            .map(|(image_id, _)| image_id.clone())
    }

    /// [`Self::register_images`] after reconciling every op-free record against the
    /// daemon — the "on reconnect verify" half of agent-api.md. Without it an image
    /// `docker rmi`'d out from under a long-lived agent stays `ready` forever and the
    /// control plane keeps scheduling onto a host that lacks it.
    ///
    /// Blocking, N docker round-trips: call from `spawn_blocking`, never an async
    /// worker thread.
    ///
    /// Records with an active op are never touched (a pull in flight owns its record).
    /// The update is re-checked under the locks after the inspect, so an op that
    /// started during the round-trip cannot be clobbered.
    pub fn refresh_register_images(&self) -> Vec<RegisterImageEntry> {
        let snapshot: Vec<(String, String, ImageState)> = {
            let ops = self.ops.lock().unwrap();
            let records = self.records.lock().unwrap();
            records
                .iter()
                .filter(|(id, _)| !ops.contains_key(*id))
                .map(|(id, rec)| (id.clone(), rec.registry_ref.clone(), rec.state))
                .collect()
        };
        let mut changed = false;
        for (image_id, registry_ref, was) in snapshot {
            let now = if registry_ref.is_empty() {
                ImageState::Absent
            } else {
                match self.runtime.image_present(&registry_ref) {
                    Ok(true) => ImageState::Ready,
                    Ok(false) => ImageState::Absent,
                    Err(e) => {
                        // A daemon hiccup must not demote every managed image to
                        // absent; leave the record for the next reconcile.
                        warn!(
                            token = "image-reconcile-inspect-failed","image {image_id}: reconcile inspect failed ({e}); keeping recorded state {was:?}");
                        continue;
                    }
                }
            };
            if now == was {
                continue;
            }
            {
                let ops = self.ops.lock().unwrap();
                if ops.contains_key(&image_id) {
                    continue; // an op started during the inspect — it owns the record
                }
                let mut records = self.records.lock().unwrap();
                let Some(rec) = records.get_mut(&image_id) else {
                    continue;
                };
                if rec.registry_ref != registry_ref || rec.state != was {
                    continue; // the record moved under us
                }
                if now == ImageState::Ready {
                    rec.state = ImageState::Ready;
                } else {
                    rec.mark_absent();
                }
            }
            changed = true;
            info!("image {image_id}: reconciled {was:?} -> {now:?} against docker daemon");
        }
        if changed {
            self.persist();
        }
        self.register_images()
    }

    fn persist(&self) {
        let records = self.records.lock().unwrap();
        state::save(&self.state_path, &records);
    }

    fn next_generation(&self) -> u64 {
        self.generations.fetch_add(1, Ordering::SeqCst) + 1
    }

    /// Never blocks the caller, so it is safe from the async connection task.
    /// `false` means the message was dropped.
    fn send_upstream(&self, msg: AgentMsg) -> bool {
        let upstream = self.upstream.read().unwrap();
        let Some(tx) = upstream.as_ref() else {
            debug!("image_state emit dropped: no upstream connection attached");
            return false;
        };
        if let Err(e) = tx.try_send(msg) {
            debug!("image_state emit dropped: {e}");
            return false;
        }
        true
    }

    /// The lossy emit path: progress ticks and recoverable re-emits. A worker's
    /// terminal state uses [`Self::emit_terminal`] instead.
    fn emit(&self, image_id: &str) -> bool {
        let Some(msg) = self.state_msg(image_id) else {
            return false;
        };
        self.send_upstream(msg)
    }

    /// Emit a terminal (`ready`/`failed`/`absent`) state. Worker threads only: it
    /// blocks while the channel is full, which the async connection task must never
    /// do. An undelivered terminal is only logged — the next
    /// [`Self::attach_upstream`] re-emits every op-free record.
    fn emit_terminal(&self, image_id: &str) {
        let Some(msg) = self.state_msg(image_id) else {
            return;
        };
        // Clone the sender OUT of the lock: blocking under the read guard would wedge
        // a connection teardown, which takes the write lock.
        let tx = self.upstream.read().unwrap().clone();
        match tx {
            // Errs as soon as the receiver is gone, so this cannot block forever.
            Some(tx) => {
                if let Err(e) = tx.blocking_send(msg) {
                    debug!("image {image_id}: terminal image_state undeliverable ({e}); the next attach will resync it");
                }
            }
            None => {
                debug!("image {image_id}: terminal image_state not sent (no upstream attached); the next attach will resync it");
            }
        }
    }

    fn state_msg(&self, image_id: &str) -> Option<AgentMsg> {
        let records = self.records.lock().unwrap();
        let rec = records.get(image_id)?;
        Some(AgentMsg::ImageState {
            image_id: image_id.to_string(),
            version: rec.wire_version().to_string(),
            state: rec.state.as_wire_str().to_string(),
            progress_pct: rec.progress_pct,
            bytes: rec.bytes,
            error: rec.error.clone(),
        })
    }

    /// `image_ensure` (agent-api.md). Acks immediately; the pull runs on its own
    /// thread.
    pub fn handle_ensure(
        self: &Arc<Self>,
        id: String,
        image_id: String,
        registry_ref: String,
        version: String,
    ) -> AgentMsg {
        if image_id.trim().is_empty()
            || image_id.len() > MAX_IMAGE_ID_LEN
            || version.len() > MAX_VERSION_LEN
            || registry_ref.len() > MAX_REGISTRY_REF_LEN
            || !looks_like_a_concrete_ref(&registry_ref)
        {
            return ack(
                id,
                false,
                Some("malformed image_ensure: image_id/registry_ref/version".to_string()),
            );
        }
        if !self.has_op_capacity(&image_id) {
            warn!(
                token = "image-ensure-refused-busy","image_ensure for {image_id} refused: {MAX_CONCURRENT_OPS} image operations already in flight");
            return ack(id, false, Some("image operation queue full".to_string()));
        }
        if self.plan_ensure(&image_id, &registry_ref, &version) {
            self.spawn_worker(image_id);
        }
        ack(id, true, None)
    }

    /// Decision + bookkeeping half of [`Self::handle_ensure`] (no thread, no docker);
    /// `true` when a fresh worker must be started.
    fn plan_ensure(self: &Arc<Self>, image_id: &str, registry_ref: &str, version: &str) -> bool {
        let started = {
            let mut ops = self.ops.lock().unwrap();
            let mut records = self.records.lock().unwrap();
            let slot = ops.get(image_id);
            let decision = decide_ensure(
                records.get(image_id),
                slot.map(|s| &s.running),
                slot.and_then(|s| s.pending.as_ref()),
                version,
                registry_ref,
            );
            match decision {
                OpDecision::ReAck | OpDecision::AbsentUnknown => false,
                OpDecision::Queue(op) => {
                    if let Some(slot) = ops.get_mut(image_id) {
                        slot.pending = Some(op);
                    }
                    false
                }
                OpDecision::Start(op) => {
                    let generation = self.next_generation();
                    ops.insert(
                        image_id.to_string(),
                        OpSlot {
                            running: op,
                            generation,
                            pending: None,
                        },
                    );
                    // Stage synchronously, before the worker exists, so a duplicate
                    // ensure in the gap always has a record to re-emit. Only staged:
                    // committed on pull success, so a failed version bump keeps
                    // naming the ref the daemon has and orphans no multi-GB image.
                    stage_pulling(&mut records, image_id, registry_ref, version);
                    true
                }
            }
        };
        if started {
            self.persist();
        }
        self.emit(image_id);
        started
    }

    /// `image_build` (agent-api.md). Acks immediately; download + `docker build` run
    /// on a worker in the same per-image op slot as ensure/remove. A disallowed
    /// `context_url` or malformed field is `ack{ok:false}` and touches no state.
    #[allow(clippy::too_many_arguments)]
    pub fn handle_build(
        self: &Arc<Self>,
        id: String,
        image_id: String,
        context_url: String,
        context_subdir: String,
        dockerfile: String,
        build_args: BTreeMap<String, String>,
        local_tag: String,
        version: String,
    ) -> AgentMsg {
        // SSRF/allowlist guard must come first, before any state is touched.
        let allowed = build::allowed_source_hosts();
        if let Err(reason) = build::validate_context_url(&context_url, &allowed) {
            warn!(
                token = "image-build-rejected",
                "image_build for {image_id} rejected: {reason}"
            );
            return ack(id, false, Some("disallowed build source".to_string()));
        }
        if !build_fields_ok(
            &image_id,
            &context_url,
            &context_subdir,
            &dockerfile,
            &local_tag,
            &version,
            &build_args,
        ) {
            return ack(
                id,
                false,
                Some("malformed image_build: image_id/local_tag/context".to_string()),
            );
        }
        if !self.has_op_capacity(&image_id) {
            warn!(
                token = "image-build-refused-busy","image_build for {image_id} refused: {MAX_CONCURRENT_OPS} image operations already in flight");
            return ack(id, false, Some("image operation queue full".to_string()));
        }
        let want = ImageOp::Build {
            version: version.clone(),
            local_tag: local_tag.clone(),
            context_url,
            context_subdir,
            dockerfile,
            build_args,
        };
        if self.plan_build(&image_id, want, &local_tag, &version) {
            self.spawn_worker(image_id);
        }
        ack(id, true, None)
    }

    /// Mirrors [`Self::plan_ensure`], staging a `building` record synchronously.
    fn plan_build(
        self: &Arc<Self>,
        image_id: &str,
        want: ImageOp,
        local_tag: &str,
        version: &str,
    ) -> bool {
        let started = {
            let mut ops = self.ops.lock().unwrap();
            let mut records = self.records.lock().unwrap();
            let slot = ops.get(image_id);
            let decision = decide_build(
                records.get(image_id),
                slot.map(|s| &s.running),
                slot.and_then(|s| s.pending.as_ref()),
                want,
                version,
            );
            match decision {
                OpDecision::ReAck | OpDecision::AbsentUnknown => false,
                OpDecision::Queue(op) => {
                    if let Some(slot) = ops.get_mut(image_id) {
                        slot.pending = Some(op);
                    }
                    false
                }
                OpDecision::Start(op) => {
                    let generation = self.next_generation();
                    ops.insert(
                        image_id.to_string(),
                        OpSlot {
                            running: op,
                            generation,
                            pending: None,
                        },
                    );
                    // Same staging discipline as an ensure: committed only on success.
                    stage_building(&mut records, image_id, local_tag, version);
                    true
                }
            }
        };
        if started {
            self.persist();
        }
        self.emit(image_id);
        started
    }

    /// `image_remove` (agent-api.md). Acks immediately; the best-effort `rmi` runs on
    /// its own thread, serialized behind any in-flight op for the same image. An
    /// unrecorded `image_id` acks `ok:true` + `absent`, with no docker call.
    pub fn handle_remove(self: &Arc<Self>, id: String, image_id: String) -> AgentMsg {
        if image_id.trim().is_empty() || image_id.len() > MAX_IMAGE_ID_LEN {
            return ack(
                id,
                false,
                Some("malformed image_remove: image_id".to_string()),
            );
        }
        if !self.has_op_capacity(&image_id) {
            warn!(
                token = "image-remove-refused-busy","image_remove for {image_id} refused: {MAX_CONCURRENT_OPS} image operations already in flight");
            return ack(id, false, Some("image operation queue full".to_string()));
        }
        if self.plan_remove(&image_id) {
            self.spawn_worker(image_id);
        }
        ack(id, true, None)
    }

    /// Decision + bookkeeping half of [`Self::handle_remove`].
    fn plan_remove(self: &Arc<Self>, image_id: &str) -> bool {
        let decision = {
            let mut ops = self.ops.lock().unwrap();
            let records = self.records.lock().unwrap();
            let slot = ops.get(image_id);
            let decision = decide_remove(
                records.get(image_id),
                slot.map(|s| &s.running),
                slot.and_then(|s| s.pending.as_ref()),
            );
            match &decision {
                OpDecision::Queue(op) => {
                    if let Some(slot) = ops.get_mut(image_id) {
                        slot.pending = Some(op.clone());
                    }
                }
                OpDecision::Start(op) => {
                    let generation = self.next_generation();
                    ops.insert(
                        image_id.to_string(),
                        OpSlot {
                            running: op.clone(),
                            generation,
                            pending: None,
                        },
                    );
                }
                _ => {}
            }
            decision
        };
        match decision {
            OpDecision::AbsentUnknown => {
                self.send_upstream(AgentMsg::ImageState {
                    image_id: image_id.to_string(),
                    version: String::new(),
                    state: ImageState::Absent.as_wire_str().to_string(),
                    progress_pct: 0,
                    bytes: 0,
                    error: String::new(),
                });
                false
            }
            OpDecision::Start(_) => true,
            _ => {
                self.emit(image_id);
                false
            }
        }
    }

    /// An id already holding a slot always may (it only re-acks or replaces pending);
    /// a new id only under [`MAX_CONCURRENT_OPS`]. Checked before planning because a
    /// refusal is an `ack{ok:false}`, not a state transition. The single connection
    /// task dispatches sequentially, so the check-then-act gap cannot be raced.
    fn has_op_capacity(&self, image_id: &str) -> bool {
        let ops = self.ops.lock().unwrap();
        ops.contains_key(image_id) || ops.len() < MAX_CONCURRENT_OPS
    }

    fn spawn_worker(self: &Arc<Self>, image_id: String) {
        let mgr = self.clone();
        std::thread::spawn(move || mgr.run_worker(image_id));
    }

    /// Drains this image's op slot: run, then promote the pending op or clear the
    /// slot. Promotion happens under the same lock acquisition that would clear the
    /// slot, so a request racing completion either lands in `pending` and is picked up
    /// here, or finds an empty slot and starts its own worker. Never both.
    fn run_worker(self: Arc<Self>, image_id: String) {
        loop {
            let current = {
                let ops = self.ops.lock().unwrap();
                ops.get(&image_id)
                    .map(|s| (s.running.clone(), s.generation))
            };
            let Some((op, generation)) = current else {
                return;
            };
            match &op {
                ImageOp::Ensure {
                    version,
                    registry_ref,
                } => {
                    // Re-stage on promotion: a queued ensure becomes the record's
                    // target only when it starts.
                    {
                        let mut records = self.records.lock().unwrap();
                        stage_pulling(&mut records, &image_id, registry_ref, version);
                    }
                    self.persist();
                    self.emit(&image_id);
                    self.run_pull(&image_id, generation, version, registry_ref);
                }
                ImageOp::Build {
                    version,
                    local_tag,
                    context_url,
                    context_subdir,
                    dockerfile,
                    build_args,
                } => {
                    {
                        let mut records = self.records.lock().unwrap();
                        stage_building(&mut records, &image_id, local_tag, version);
                    }
                    self.persist();
                    self.emit(&image_id);
                    self.run_build(
                        &image_id,
                        generation,
                        version,
                        local_tag,
                        context_url,
                        context_subdir,
                        dockerfile,
                        build_args,
                    );
                }
                ImageOp::Remove => self.run_remove(&image_id, generation),
            }

            let promoted = {
                let mut ops = self.ops.lock().unwrap();
                match ops.get_mut(&image_id) {
                    Some(slot) => match slot.pending.take() {
                        Some(next) => {
                            slot.running = next;
                            slot.generation = self.next_generation();
                            true
                        }
                        None => {
                            ops.remove(&image_id);
                            false
                        }
                    },
                    None => false,
                }
            };
            if !promoted {
                return;
            }
        }
    }

    fn run_remove(&self, image_id: &str, generation: u64) {
        let registry_ref = {
            let records = self.records.lock().unwrap();
            records
                .get(image_id)
                .map(|r| r.registry_ref.clone())
                .unwrap_or_default()
        };
        if registry_ref.is_empty() {
            // Nothing was ever pulled for this id, so it is already gone.
            self.commit(image_id, generation, |rec| rec.mark_absent());
            self.persist();
            self.emit_terminal(image_id);
            return;
        }
        // NEVER force: agent-api.md forbids removing an image backing a live
        // container, so no `-f`. `--` stops a ref being parsed as a flag.
        match self.runtime.run_raw(&["rmi", "--", &registry_ref]) {
            Ok(_) => {
                self.commit(image_id, generation, |rec| rec.mark_absent());
                info!("image {image_id} ({registry_ref}) removed");
                self.notify_removed(image_id);
            }
            Err(e) => {
                // `run_raw`'s error embeds the docker command line, so it is
                // classified here and never forwarded upstream verbatim.
                let raw = e.to_string();
                let msg = errors::map_rmi_error(&raw);
                warn!(
                    token = "image-rmi-failed",
                    "image {image_id} ({registry_ref}) rmi failed: {raw}"
                );
                if msg == errors::RMI_ALREADY_ABSENT {
                    self.commit(image_id, generation, |rec| rec.mark_absent());
                } else {
                    self.commit(image_id, generation, |rec| rec.mark_failed(&msg));
                    tracing::error!(
                        token = "image-remove-failed",
                        "image {image_id} ({registry_ref}) remove failed: {msg}"
                    );
                }
            }
        }
        self.persist();
        self.emit_terminal(image_id);
    }

    fn run_pull(&self, image_id: &str, generation: u64, version: &str, registry_ref: &str) {
        let _permit = self.semaphore.acquire();

        // Disk guard before pulling: a doomed pull fails fast here rather than
        // mid-transfer with an obscure daemon error.
        if let Err(err) = disk::check(&self.runtime, None) {
            self.commit(image_id, generation, |rec| rec.mark_failed(&err));
            self.persist();
            self.emit_terminal(image_id);
            warn!(token = "image-op-error", "image {image_id}: {err}");
            return;
        }

        self.set_pulling(image_id, generation, 0, 0);
        self.persist();
        self.emit(image_id);

        match self.pull_with_progress(image_id, generation, registry_ref) {
            Ok(()) => {
                let bytes = self.image_size_bytes(registry_ref).unwrap_or(0);
                self.commit(image_id, generation, |rec| rec.mark_ready(bytes));
                info!("image {image_id} ({registry_ref}) pulled: ready ({bytes} bytes), version {version}");
                // The one choke-point where an image becomes usable on this host, so
                // where a golden-home warm-up is scheduled.
                self.notify_ready(image_id, registry_ref, version);
            }
            Err(err) => {
                self.commit(image_id, generation, |rec| rec.mark_failed(&err));
                tracing::error!(
                    token = "image-pull-failed",
                    "image {image_id} ({registry_ref}) pull failed: {err}"
                );
            }
        }
        self.persist();
        self.emit_terminal(image_id);
    }

    /// `docker pull`, translating per-layer progress into throttled `image_state`.
    ///
    /// stdout and stderr each get their own draining thread: a full pipe must never
    /// wedge the other or the watchdog. [`PULL_TIMEOUT`] is enforced independently of
    /// output — a registry that connects then goes silent emits no line, so a per-line
    /// check never fires and two such stalls would deadlock every later pull with the
    /// control plane stuck at `pulling`. On expiry the watchdog kills AND reaps.
    fn pull_with_progress(
        &self,
        image_id: &str,
        generation: u64,
        registry_ref: &str,
    ) -> Result<(), String> {
        // `ContainerRuntime` owns the command; building it here would duplicate its
        // `QUASAR_CONTAINER_RUNTIME` resolution and drift from it.
        let mut cmd = self.runtime.pull_command(registry_ref);
        cmd.stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .stdin(Stdio::null());
        let mut child = cmd
            .spawn()
            .map_err(|e| format!("failed to exec docker pull: {e}"))?;
        let stdout = child.stdout.take().expect("piped stdout");
        let stderr = child.stderr.take().expect("piped stderr");

        // The scraper thread only reports samples; record mutation stays on this
        // worker thread.
        let (progress_tx, progress_rx) = std::sync::mpsc::channel::<(u8, u64)>();
        let stdout_thread = std::thread::spawn(move || {
            let mut tracker = PullProgressTracker::new();
            for line in std::io::BufReader::new(stdout).lines() {
                let Ok(line) = line else { break };
                if let Some((layer_id, p)) = progress::parse_pull_line(&line) {
                    tracker.observe(layer_id, p);
                    if progress_tx.send(tracker.snapshot()).is_err() {
                        break;
                    }
                }
            }
        });
        let stderr_buf = Arc::new(Mutex::new(String::new()));
        let stderr_buf2 = stderr_buf.clone();
        let stderr_thread = std::thread::spawn(move || {
            use std::io::Read;
            let mut buf = String::new();
            // Bounded: an unbounded read lets a misbehaving registry pin memory for
            // the whole PULL_TIMEOUT window by emitting stderr the entire time.
            let _ = std::io::BufReader::new(stderr)
                .take(64 * 1024)
                .read_to_string(&mut buf);
            *stderr_buf2.lock().unwrap() = buf;
        });

        let deadline = Instant::now() + PULL_TIMEOUT;
        let mut throttle = ProgressThrottle::new();
        let mut wait_err: Option<String> = None;
        let mut timed_out = false;
        let status = loop {
            let mut latest = None;
            while let Ok(sample) = progress_rx.try_recv() {
                latest = Some(sample);
            }
            if let Some((pct, bytes)) = latest {
                if throttle.should_emit(Instant::now(), pct) {
                    self.set_pulling(image_id, generation, pct, bytes);
                    self.emit(image_id);
                }
            }

            match child.try_wait() {
                Ok(Some(status)) => break Some(status),
                Ok(None) => {}
                Err(e) => {
                    wait_err = Some(format!("failed to wait on docker pull: {e}"));
                    let _ = child.kill();
                    let _ = child.wait();
                    break None;
                }
            }
            if Instant::now() >= deadline {
                warn!(
                    token = "image-pull-timeout",
                    "image {image_id}: docker pull exceeded {}s — killing the child",
                    PULL_TIMEOUT.as_secs()
                );
                let _ = child.kill();
                // Reap: a permit-holding worker must never leave a zombie.
                let _ = child.wait();
                timed_out = true;
                break None;
            }
            std::thread::sleep(PULL_POLL_INTERVAL);
        };

        // Both readers hit EOF once the child closes its pipes, so these joins are
        // bounded by the child's lifetime above.
        let _ = stdout_thread.join();
        let _ = stderr_thread.join();

        if timed_out {
            return Err("registry pull timed out".to_string());
        }
        if let Some(e) = wait_err {
            return Err(e);
        }
        let status = status.expect("loop breaks with a status, an error, or a timeout");
        if status.success() {
            Ok(())
        } else {
            let raw = stderr_buf.lock().unwrap().clone();
            // Raw docker output is logged locally, never sent upstream. ERROR, not
            // debug: the mapped wire error tells the operator to inspect this log,
            // and the default filter is `info` — at debug this line never exists.
            tracing::error!(
                token = "image-pull-stderr",
                "image {image_id}: docker pull stderr: {}",
                raw.trim()
            );
            Err(errors::map_pull_error(&raw))
        }
    }

    /// Run a template build: take a shared pull/build permit, disk-guard, download +
    /// extract the context, then `docker build`. Mirrors [`Self::run_pull`].
    #[allow(clippy::too_many_arguments)]
    fn run_build(
        &self,
        image_id: &str,
        generation: u64,
        version: &str,
        local_tag: &str,
        context_url: &str,
        context_subdir: &str,
        dockerfile: &str,
        build_args: &BTreeMap<String, String>,
    ) {
        // A build takes one slot in the same 2-permit budget as a pull (agent-api.md).
        let _permit = self.semaphore.acquire();

        // One whole-op deadline over download + extract + build. The HTTP timeout is
        // per-read, so without this a slow-drip source could hold a shared permit far
        // longer than BUILD_TIMEOUT by staying just under it on every read.
        let deadline = Instant::now() + BUILD_TIMEOUT;

        if let Err(err) = disk::check_build(&self.runtime) {
            self.commit(image_id, generation, |rec| rec.mark_failed(&err));
            self.persist();
            self.emit_terminal(image_id);
            warn!(token = "image-op-error", "image {image_id}: {err}");
            return;
        }

        self.set_building(image_id, generation, None);
        self.persist();
        self.emit(image_id);

        match self.build_image(
            image_id,
            generation,
            local_tag,
            context_url,
            context_subdir,
            dockerfile,
            build_args,
            deadline,
        ) {
            Ok(()) => {
                let bytes = self.image_size_bytes(local_tag).unwrap_or(0);
                self.commit(image_id, generation, |rec| rec.mark_ready(bytes));
                info!("image {image_id} ({local_tag}) built: ready ({bytes} bytes), version {version}");
                self.notify_ready(image_id, local_tag, version);
            }
            Err(err) => {
                self.commit(image_id, generation, |rec| rec.mark_failed(&err));
                tracing::error!(
                    token = "image-build-failed",
                    "image {image_id} ({local_tag}) build failed: {err}"
                );
            }
        }
        self.persist();
        self.emit_terminal(image_id);
    }

    /// Download + extract the context into a scratch dir, then build. The scratch dir
    /// is always removed via [`ScratchGuard`]. Errors are short and bounded.
    #[allow(clippy::too_many_arguments)]
    fn build_image(
        &self,
        image_id: &str,
        generation: u64,
        local_tag: &str,
        context_url: &str,
        context_subdir: &str,
        dockerfile: &str,
        build_args: &BTreeMap<String, String>,
        deadline: Instant,
    ) -> Result<(), String> {
        let scratch = self.build_scratch_dir(image_id, generation);
        // Clear any stale dir from a crashed prior build.
        let _ = std::fs::remove_dir_all(&scratch);
        let _cleanup = ScratchGuard(scratch.clone());
        std::fs::create_dir_all(&scratch)
            .map_err(|e| format!("could not create build scratch dir: {e}"))?;

        let tarball = scratch.join("context.tar.gz");
        build::download_context(context_url, &tarball, deadline)?;

        let ctx = scratch.join("ctx");
        std::fs::create_dir_all(&ctx)
            .map_err(|e| format!("could not create build context dir: {e}"))?;
        build::extract_context(&tarball, context_subdir, &ctx, deadline)?;

        let df_rel =
            build::sanitize_relative(Path::new(dockerfile)).ok_or("invalid dockerfile path")?;
        let df_path = ctx.join(&df_rel);
        // symlink_metadata does not follow the link: a followed symlink would escape
        // the is_file() check onto an arbitrary host path.
        let df_meta = std::fs::symlink_metadata(&df_path)
            .map_err(|_| "dockerfile not found in build context")?;
        if df_meta.file_type().is_symlink() {
            return Err("dockerfile must not be a symlink".to_string());
        }
        if !df_meta.is_file() {
            return Err("dockerfile not found in build context".to_string());
        }
        let df_str = df_path.to_string_lossy().into_owned();
        let ctx_str = ctx.to_string_lossy().into_owned();

        self.build_with_progress(
            image_id, generation, local_tag, &df_str, &ctx_str, build_args, deadline,
        )
    }

    /// `<state-file-parent>/image-build-scratch/<sanitized-image_id>-<generation>`,
    /// or the OS temp dir when there is no state path.
    fn build_scratch_dir(&self, image_id: &str, generation: u64) -> PathBuf {
        let base = if self.state_path.is_empty() {
            std::env::temp_dir()
        } else {
            Path::new(&self.state_path)
                .parent()
                .map(Path::to_path_buf)
                .unwrap_or_else(|| PathBuf::from("."))
        };
        let safe_id: String = image_id
            .chars()
            .map(|c| {
                if c.is_ascii_alphanumeric() || c == '-' || c == '_' {
                    c
                } else {
                    '_'
                }
            })
            .collect();
        base.join(BUILD_SCRATCH_DIR)
            .join(format!("{safe_id}-{generation}"))
    }

    /// `docker build` (classic builder), translating `Step N/M` into throttled
    /// `building` emits. Mirrors [`Self::pull_with_progress`]: output-independent
    /// [`BUILD_TIMEOUT`] watchdog that kills and reaps, separate bounded reader
    /// threads, and a mapped, never-raw error string.
    #[allow(clippy::too_many_arguments)]
    fn build_with_progress(
        &self,
        image_id: &str,
        generation: u64,
        local_tag: &str,
        dockerfile_path: &str,
        context_dir: &str,
        build_args: &BTreeMap<String, String>,
        deadline: Instant,
    ) -> Result<(), String> {
        let mut cmd =
            self.runtime
                .build_command(local_tag, dockerfile_path, context_dir, build_args);
        cmd.stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .stdin(Stdio::null());
        let mut child = cmd
            .spawn()
            .map_err(|e| format!("failed to exec docker build: {e}"))?;
        let stdout = child.stdout.take().expect("piped stdout");
        let stderr = child.stderr.take().expect("piped stderr");

        // Both streams must be drained to EOF: a reader that stops at its byte cap
        // leaves the pipe full, giving the child a broken pipe mid-build. Each thread
        // keeps only the last BUILD_LOG_KEEP_BYTES (the failing step's error is the
        // last thing docker prints) and discards the rest.
        let (progress_tx, progress_rx) = std::sync::mpsc::channel::<u8>();
        let stdout_buf = Arc::new(Mutex::new(String::new()));
        let stdout_buf2 = stdout_buf.clone();
        let stdout_thread = std::thread::spawn(move || {
            let mut kept: Vec<u8> = Vec::new();
            for line in std::io::BufReader::new(stdout).lines() {
                let Ok(line) = line else { break };
                if let Some(pct) = progress::parse_build_step(&line) {
                    // Keep draining past a send error: the pipe must reach EOF whether
                    // or not progress is still wanted.
                    let _ = progress_tx.send(pct);
                }
                kept.extend_from_slice(line.as_bytes());
                kept.push(b'\n');
                keep_tail(&mut kept, BUILD_LOG_KEEP_BYTES);
            }
            *stdout_buf2.lock().unwrap() = String::from_utf8_lossy(&kept).into_owned();
        });
        let stderr_buf = Arc::new(Mutex::new(String::new()));
        let stderr_buf2 = stderr_buf.clone();
        let stderr_thread = std::thread::spawn(move || {
            use std::io::Read;
            let mut reader = std::io::BufReader::new(stderr);
            let mut kept: Vec<u8> = Vec::new();
            let mut chunk = [0u8; 8192];
            loop {
                match reader.read(&mut chunk) {
                    Ok(0) => break,
                    Ok(n) => {
                        kept.extend_from_slice(&chunk[..n]);
                        keep_tail(&mut kept, BUILD_LOG_KEEP_BYTES);
                    }
                    Err(_) => break,
                }
            }
            *stderr_buf2.lock().unwrap() = String::from_utf8_lossy(&kept).into_owned();
        });

        let mut throttle = ProgressThrottle::new();
        let mut wait_err: Option<String> = None;
        let mut timed_out = false;
        let status = loop {
            let mut latest = None;
            while let Ok(pct) = progress_rx.try_recv() {
                latest = Some(pct);
            }
            if let Some(pct) = latest {
                if throttle.should_emit(Instant::now(), pct) {
                    self.set_building(image_id, generation, Some(pct));
                    self.emit(image_id);
                }
            }

            match child.try_wait() {
                Ok(Some(status)) => break Some(status),
                Ok(None) => {}
                Err(e) => {
                    wait_err = Some(format!("failed to wait on docker build: {e}"));
                    let _ = child.kill();
                    let _ = child.wait();
                    break None;
                }
            }
            if Instant::now() >= deadline {
                warn!(
                    token = "image-build-timeout",
                    "image {image_id}: docker build exceeded {}s — killing the child",
                    BUILD_TIMEOUT.as_secs()
                );
                let _ = child.kill();
                let _ = child.wait();
                timed_out = true;
                break None;
            }
            std::thread::sleep(PULL_POLL_INTERVAL);
        };

        let _ = stdout_thread.join();
        let _ = stderr_thread.join();

        // Local-only diagnostics for any failed/killed build: exit status, resolved
        // context path, and the tail of both streams. The wire error stays the short
        // mapped string (agent-api.md: "never a raw build-log blob"); the raw tail is
        // exactly what "inspect node-agent logs" promises the operator will be here.
        let log_output = |what: &str| {
            let out = stdout_buf.lock().unwrap();
            let err = stderr_buf.lock().unwrap();
            tracing::error!(
                token = "image-build-output",
                "image {image_id}: docker build {what} (dockerfile {dockerfile_path}, \
                 context {context_dir}); stdout tail:\n{}\nstderr tail:\n{}",
                tail(&out, BUILD_LOG_LOG_BYTES),
                tail(&err, BUILD_LOG_LOG_BYTES),
            );
        };

        if timed_out {
            log_output("timed out");
            return Err("docker build timed out".to_string());
        }
        if let Some(e) = wait_err {
            log_output("wait failed");
            return Err(e);
        }
        let status = status.expect("loop breaks with a status, an error, or a timeout");
        if status.success() {
            Ok(())
        } else {
            log_output(&format!("exited with {status}"));
            // The wire error is classified from both buffers (docker writes
            // diagnostics to either), never the raw text.
            let combined = {
                let out = stdout_buf.lock().unwrap();
                let err = stderr_buf.lock().unwrap();
                format!("{out}\n{err}")
            };
            let mapped = errors::map_build_error(&combined);
            tracing::error!(
                token = "image-build-failed-mapped",
                "image {image_id}: docker build failed: {mapped}"
            );
            Err(mapped)
        }
    }

    fn image_size_bytes(&self, registry_ref: &str) -> Option<u64> {
        let out = self
            .runtime
            .run_raw(&[
                "image",
                "inspect",
                "--format",
                "{{.Size}}",
                "--",
                registry_ref,
            ])
            .ok()?;
        out.trim().parse().ok()
    }

    /// Apply a transition only if this worker still owns the slot at its generation.
    fn commit<F: FnOnce(&mut ImageRecord)>(&self, image_id: &str, generation: u64, f: F) {
        let slot_generation = {
            let ops = self.ops.lock().unwrap();
            ops.get(image_id).map(|s| s.generation)
        };
        if !should_commit(slot_generation, generation) {
            debug!(
                "image {image_id}: superseded worker (generation {generation}) dropped its result"
            );
            return;
        }
        if let Some(rec) = self.records.lock().unwrap().get_mut(image_id) {
            f(rec);
        }
    }

    fn set_pulling(&self, image_id: &str, generation: u64, pct: u8, bytes: u64) {
        self.commit(image_id, generation, |rec| {
            rec.state = ImageState::Pulling;
            rec.progress_pct = pct;
            rec.bytes = bytes;
        });
    }

    /// Wire `progress_pct` is a non-optional `u8`, so an unparseable step reports 0;
    /// agent-api.md's "omit progress_pct otherwise" is met by not advancing it.
    fn set_building(&self, image_id: &str, generation: u64, pct: Option<u8>) {
        self.commit(image_id, generation, |rec| {
            rec.state = ImageState::Building;
            rec.progress_pct = pct.unwrap_or(0);
            rec.bytes = 0;
        });
    }
}

/// Removes a build's scratch dir on drop, so agent-api.md's always-clean guarantee
/// holds on every early return, `?`, or panic in [`ImageManager::build_image`].
struct ScratchGuard(PathBuf);

impl Drop for ScratchGuard {
    fn drop(&mut self) {
        if let Err(e) = std::fs::remove_dir_all(&self.0) {
            if e.kind() != std::io::ErrorKind::NotFound {
                debug!(
                    "image build: could not remove scratch dir {}: {e}",
                    self.0.display()
                );
            }
        }
    }
}

/// The non-URL `image_build` fields; the `context_url` guard runs separately, first.
/// Bounds are inclusive and generous, and a violation is a loud `ack{ok:false}`.
fn build_fields_ok(
    image_id: &str,
    context_url: &str,
    context_subdir: &str,
    dockerfile: &str,
    local_tag: &str,
    version: &str,
    build_args: &BTreeMap<String, String>,
) -> bool {
    if image_id.trim().is_empty() || image_id.len() > MAX_IMAGE_ID_LEN {
        return false;
    }
    if context_url.len() > MAX_CONTEXT_URL_LEN || version.len() > MAX_VERSION_LEN {
        return false;
    }
    if local_tag.len() > MAX_LOCAL_TAG_LEN || !looks_like_a_local_tag(local_tag) {
        return false;
    }
    // context_subdir + dockerfile go through the extractor's zip-slip guard.
    if context_subdir.is_empty()
        || context_subdir.len() > MAX_CONTEXT_SUBDIR_LEN
        || build::sanitize_relative(Path::new(context_subdir)).is_none()
    {
        return false;
    }
    if dockerfile.is_empty()
        || dockerfile.len() > MAX_DOCKERFILE_LEN
        || build::sanitize_relative(Path::new(dockerfile)).is_none()
    {
        return false;
    }
    if build_args.len() > MAX_BUILD_ARGS {
        return false;
    }
    for (k, v) in build_args {
        if k.is_empty()
            || k.len() > MAX_BUILD_ARG_LEN
            || v.len() > MAX_BUILD_ARG_LEN
            || k.chars()
                .any(|c| c.is_whitespace() || c.is_control() || c == '=')
        {
            return false;
        }
    }
    true
}

/// Stage an ensure's `(version, registry_ref)` and mark it `pulling`. The committed
/// fields stay untouched until the pull succeeds ([`ImageRecord::mark_ready`]).
fn stage_pulling(
    records: &mut BTreeMap<String, ImageRecord>,
    image_id: &str,
    registry_ref: &str,
    version: &str,
) {
    let rec = records
        .entry(image_id.to_string())
        .or_insert_with(ImageRecord::empty);
    rec.staged = Some(StagedTarget {
        registry_ref: registry_ref.to_string(),
        version: version.to_string(),
    });
    rec.state = ImageState::Pulling;
    rec.progress_pct = 0;
    rec.bytes = 0;
    rec.error.clear();
}

/// Stage a build's `(version, local_tag)` and mark it `building`. `local_tag` uses the
/// same staged slot as an ensure's ref, so a successful build commits it identically
/// and `image_remove` rmi's it unchanged.
fn stage_building(
    records: &mut BTreeMap<String, ImageRecord>,
    image_id: &str,
    local_tag: &str,
    version: &str,
) {
    let rec = records
        .entry(image_id.to_string())
        .or_insert_with(ImageRecord::empty);
    rec.staged = Some(StagedTarget {
        registry_ref: local_tag.to_string(),
        version: version.to_string(),
    });
    rec.state = ImageState::Building;
    rec.progress_pct = 0;
    rec.bytes = 0;
    rec.error.clear();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keep_tail_keeps_the_last_cap_bytes() {
        let mut buf = b"abcdefgh".to_vec();
        keep_tail(&mut buf, 3);
        assert_eq!(buf, b"fgh");
        let mut small = b"ab".to_vec();
        keep_tail(&mut small, 3);
        assert_eq!(small, b"ab");
    }

    #[test]
    fn tail_respects_char_boundaries() {
        assert_eq!(tail("hello", 10), "hello");
        assert_eq!(tail("hello", 3), "llo");
        // "é" is 2 bytes; a cut landing mid-char advances past it.
        let s = "aéb";
        assert_eq!(tail(s, 2), "b");
        assert_eq!(tail(s, 3), "éb");
    }

    fn mgr() -> Arc<ImageManager> {
        // An empty state_path means no file touched and no docker call.
        Arc::new(ImageManager {
            runtime: ContainerRuntime::from_env(),
            state_path: String::new(),
            records: Mutex::new(BTreeMap::new()),
            ops: Mutex::new(HashMap::new()),
            generations: AtomicU64::new(0),
            semaphore: CountingSemaphore::new(MAX_CONCURRENT_PULLS),
            upstream: RwLock::new(None),
            lifecycle: RwLock::new(None),
        })
    }

    fn mgr_with(records: BTreeMap<String, ImageRecord>) -> Arc<ImageManager> {
        let m = mgr();
        *m.records.lock().unwrap() = records;
        m
    }

    fn ensure(version: &str, registry_ref: &str) -> ImageOp {
        ImageOp::Ensure {
            version: version.to_string(),
            registry_ref: registry_ref.to_string(),
        }
    }

    const REF1: &str = "ghcr.io/x/steam:sha-abc1234";
    const REF2: &str = "ghcr.io/x/steam:sha-def5678";

    fn ready(version: &str) -> ImageRecord {
        ImageRecord {
            registry_ref: REF1.to_string(),
            version: version.to_string(),
            state: ImageState::Ready,
            progress_pct: 100,
            bytes: 123,
            error: String::new(),
            staged: None,
        }
    }

    fn failed(version: &str) -> ImageRecord {
        ImageRecord {
            state: ImageState::Failed,
            error: "insufficient disk".to_string(),
            ..ready(version)
        }
    }

    // pure decision logic: ensure

    #[test]
    fn no_record_nothing_running_starts_a_pull() {
        assert_eq!(
            decide_ensure(None, None, None, "v1", REF1),
            OpDecision::Start(ensure("v1", REF1))
        );
    }

    #[test]
    fn ready_at_the_requested_version_is_idempotent() {
        let rec = ready("v1");
        assert_eq!(
            decide_ensure(Some(&rec), None, None, "v1", REF1),
            OpDecision::ReAck
        );
    }

    #[test]
    fn ready_at_a_different_version_starts_a_new_pull() {
        let rec = ready("v1");
        assert_eq!(
            decide_ensure(Some(&rec), None, None, "v2", REF2),
            OpDecision::Start(ensure("v2", REF2))
        );
    }

    #[test]
    fn the_same_ensure_already_running_is_idempotent() {
        let running = ensure("v1", REF1);
        assert_eq!(
            decide_ensure(None, Some(&running), None, "v1", REF1),
            OpDecision::ReAck
        );
    }

    #[test]
    fn a_different_version_behind_a_running_pull_queues_instead_of_starting_a_second_pull() {
        let running = ensure("v1", REF1);
        assert_eq!(
            decide_ensure(None, Some(&running), None, "v2", REF2),
            OpDecision::Queue(ensure("v2", REF2))
        );
    }

    #[test]
    fn a_newer_pending_ensure_replaces_an_older_one() {
        let running = ensure("v1", REF1);
        let pending = ensure("v2", REF2);
        assert_eq!(
            decide_ensure(None, Some(&running), Some(&pending), "v3", REF1),
            OpDecision::Queue(ensure("v3", REF1))
        );
        assert_eq!(
            decide_ensure(None, Some(&running), Some(&pending), "v2", REF2),
            OpDecision::ReAck
        );
    }

    #[test]
    fn an_ensure_behind_a_running_remove_queues() {
        assert_eq!(
            decide_ensure(None, Some(&ImageOp::Remove), None, "v1", REF1),
            OpDecision::Queue(ensure("v1", REF1))
        );
    }

    #[test]
    fn a_prior_failure_at_the_same_version_is_retried_not_deduped() {
        let rec = failed("v1");
        assert_eq!(
            decide_ensure(Some(&rec), None, None, "v1", REF1),
            OpDecision::Start(ensure("v1", REF1))
        );
    }

    // pure decision logic: remove

    #[test]
    fn remove_of_an_unknown_id_is_absent_with_no_record() {
        assert_eq!(decide_remove(None, None, None), OpDecision::AbsentUnknown);
    }

    #[test]
    fn remove_of_a_known_idle_image_starts() {
        let rec = ready("v1");
        assert_eq!(
            decide_remove(Some(&rec), None, None),
            OpDecision::Start(ImageOp::Remove)
        );
    }

    #[test]
    fn remove_behind_an_in_flight_ensure_queues_rather_than_racing_it() {
        let rec = ready("v1");
        let running = ensure("v2", REF2);
        assert_eq!(
            decide_remove(Some(&rec), Some(&running), None),
            OpDecision::Queue(ImageOp::Remove)
        );
    }

    #[test]
    fn a_second_remove_behind_a_running_remove_is_idempotent() {
        let rec = ready("v1");
        assert_eq!(
            decide_remove(Some(&rec), Some(&ImageOp::Remove), None),
            OpDecision::ReAck
        );
        let running = ensure("v2", REF2);
        assert_eq!(
            decide_remove(Some(&rec), Some(&running), Some(&ImageOp::Remove)),
            OpDecision::ReAck
        );
    }

    // obsolete-worker guard

    #[test]
    fn only_the_worker_owning_the_current_generation_may_commit() {
        assert!(should_commit(Some(7), 7));
        assert!(!should_commit(Some(8), 7)); // slot promoted to a newer op
        assert!(!should_commit(None, 7)); // slot cleared
    }

    #[test]
    fn a_superseded_worker_cannot_overwrite_newer_record_state() {
        let mut records = BTreeMap::new();
        records.insert("steam".to_string(), ready("v2"));
        let m = mgr_with(records);
        m.ops.lock().unwrap().insert(
            "steam".to_string(),
            OpSlot {
                running: ensure("v2", REF2),
                generation: 9,
                pending: None,
            },
        );
        // A superseded worker (generation 3) tries to fail the record.
        m.commit("steam", 3, |rec| rec.mark_failed("stale failure"));
        let records = m.records.lock().unwrap();
        assert_eq!(records["steam"].state, ImageState::Ready);
        assert!(records["steam"].error.is_empty());
    }

    // serialization bookkeeping (no threads, no docker)

    #[test]
    fn plan_ensure_stages_a_pulling_record_synchronously_so_reack_can_emit() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);

        assert!(m.plan_ensure("steam", REF1, "v1"), "first ensure starts");
        // The record exists before any worker runs, with the ref only staged.
        {
            let records = m.records.lock().unwrap();
            let rec = &records["steam"];
            assert_eq!(rec.state, ImageState::Pulling);
            assert_eq!(rec.wire_version(), "v1");
            assert!(rec.registry_ref.is_empty());
        }
        match rx.try_recv().expect("start emits current state") {
            AgentMsg::ImageState { state, version, .. } => {
                assert_eq!(state, "pulling");
                assert_eq!(version, "v1");
            }
            other => panic!("expected ImageState, got {other:?}"),
        }

        // A duplicate ensure in the gap re-acks, never starting a second worker.
        assert!(!m.plan_ensure("steam", REF1, "v1"));
        match rx.try_recv().expect("re-ack re-emits current state") {
            AgentMsg::ImageState { state, .. } => assert_eq!(state, "pulling"),
            other => panic!("expected ImageState, got {other:?}"),
        }
    }

    #[test]
    fn a_second_version_never_starts_a_second_worker_it_becomes_pending() {
        let m = mgr();
        assert!(m.plan_ensure("steam", REF1, "v1"));
        assert!(!m.plan_ensure("steam", REF2, "v2"));
        let ops = m.ops.lock().unwrap();
        let slot = &ops["steam"];
        assert_eq!(slot.running, ensure("v1", REF1));
        assert_eq!(slot.pending, Some(ensure("v2", REF2)));
    }

    #[test]
    fn remove_behind_an_in_flight_ensure_becomes_pending_not_a_second_worker() {
        let m = mgr();
        assert!(m.plan_ensure("steam", REF1, "v1"));
        assert!(!m.plan_remove("steam"));
        let ops = m.ops.lock().unwrap();
        assert_eq!(ops["steam"].pending, Some(ImageOp::Remove));
    }

    #[test]
    fn a_failed_pull_keeps_the_previously_committed_ref_recorded() {
        let mut records = BTreeMap::new();
        records.insert("steam".to_string(), ready("v1"));
        let m = mgr_with(records);
        assert!(m.plan_ensure("steam", REF2, "v2"));
        let generation = m.ops.lock().unwrap()["steam"].generation;
        m.commit("steam", generation, |rec| rec.mark_failed("network error"));

        let records = m.records.lock().unwrap();
        let rec = &records["steam"];
        assert_eq!(rec.state, ImageState::Failed);
        assert_eq!(rec.error, "network error");
        // v2 was never committed, so a later remove rmi's the ref the daemon has.
        assert_eq!(rec.registry_ref, REF1);
        assert_eq!(rec.version, "v1");
        assert!(rec.staged.is_none());
    }

    #[test]
    fn a_successful_pull_commits_the_staged_ref_and_version() {
        let mut records = BTreeMap::new();
        records.insert("steam".to_string(), ready("v1"));
        let m = mgr_with(records);
        assert!(m.plan_ensure("steam", REF2, "v2"));
        let generation = m.ops.lock().unwrap()["steam"].generation;
        m.commit("steam", generation, |rec| rec.mark_ready(999));

        let records = m.records.lock().unwrap();
        let rec = &records["steam"];
        assert_eq!(rec.state, ImageState::Ready);
        assert_eq!(rec.registry_ref, REF2);
        assert_eq!(rec.version, "v2");
        assert_eq!(rec.bytes, 999);
        assert!(rec.staged.is_none());
    }

    // ref validation

    #[test]
    fn ref_validation_accepts_the_contract_immutable_forms() {
        for r in [
            "ghcr.io/accreleus/quasar-steam:sha-969cc14ea168",
            "ghcr.io/x/y:sha-abc1234",
            &format!("ghcr.io/x/y@sha256:{}", "a".repeat(64)),
            "registry.local:5000/x/y:sha-0123456789abcdef",
        ] {
            assert!(looks_like_a_concrete_ref(r), "should accept {r}");
        }
    }

    #[test]
    fn ref_validation_rejects_everything_else() {
        for r in [
            "",
            "steam",                                     // bare name
            "ghcr.io/x/steam",                           // no tag or digest
            "ghcr.io/x/steam:latest",                    // floating tag
            "ghcr.io/x/steam:v1.2.3",                    // floating tag
            "steam:sha-abc1234",                         // no '/' in the name
            "-ghcr.io/x/steam:sha-abc1234",              // leading '-' (argv flag)
            "ghcr.io/x/steam:sha-abc",                   // digest too short
            "ghcr.io/x/steam:sha-zzzzzzz",               // not hex
            "ghcr.io/x/steam@sha256:deadbeef",           // digest not 64 hex
            "ghcr.io/x/steam@sha1:0123456789abcdef0123", // wrong algorithm
            "ghcr.io/x/steam :sha-abc1234",              // whitespace
            "ghcr.io/x/steam:sha-abc1234\n",             // control char
            "/x:sha-abc1234",                            // empty-ish name is still nameless
            ":sha-abc1234",
        ] {
            assert!(!looks_like_a_concrete_ref(r), "should reject {r:?}");
        }
        // A 65-hex tag is over the bound.
        assert!(!looks_like_a_concrete_ref(&format!(
            "ghcr.io/x/y:sha-{}",
            "a".repeat(65)
        )));
    }

    // register.images[]

    #[test]
    fn register_images_is_always_present_even_when_empty() {
        // Empty or lost state must serialize as `[]`, never omitted.
        let m = mgr();
        assert!(m.register_images().is_empty());
        let msg = AgentMsg::Register {
            node_name: "n".to_string(),
            agent_version: "v".to_string(),
            auth: crate::messages::Auth::Enrollment {
                enrollment_token: "tok".to_string(),
            },
            images: m.register_images(),
            source_commit: None,
            built_at: None,
            install_mode: None,
            updater_present: None,
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["images"], serde_json::json!([]));
    }

    #[test]
    fn register_images_reports_recorded_state() {
        let mut records = BTreeMap::new();
        records.insert("steam".to_string(), ready("2026.08.07"));
        let m = mgr_with(records);
        let entries = m.register_images();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].image_id, "steam");
        assert_eq!(entries[0].version, "2026.08.07");
        assert_eq!(entries[0].state, "ready");
    }

    // command handling

    // An unknown id must create no record: `register.images[]` stays empty for an id
    // the agent was never told about (agent-api.md).
    #[test]
    fn handle_remove_unknown_id_is_idempotent_absent_with_no_record() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);
        let reply = m.handle_remove("cmd-1".to_string(), "ghost".to_string());
        match reply {
            AgentMsg::Ack { id, ok, error } => {
                assert_eq!(id, "cmd-1");
                assert!(ok);
                assert!(error.is_none());
            }
            other => panic!("expected Ack, got {other:?}"),
        }
        let emitted = rx.try_recv().expect("expected an image_state emission");
        match emitted {
            AgentMsg::ImageState {
                image_id, state, ..
            } => {
                assert_eq!(image_id, "ghost");
                assert_eq!(state, "absent");
            }
            other => panic!("expected ImageState, got {other:?}"),
        }
        assert!(m.register_images().is_empty());
    }

    #[test]
    fn handle_ensure_rejects_malformed_ref_without_touching_state() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);
        let reply = m.handle_ensure(
            "cmd-1".to_string(),
            "steam".to_string(),
            "ghcr.io/x/steam:latest".to_string(),
            "v1".to_string(),
        );
        match reply {
            AgentMsg::Ack { ok, error, .. } => {
                assert!(!ok);
                assert!(error.is_some());
            }
            other => panic!("expected Ack, got {other:?}"),
        }
        assert!(
            rx.try_recv().is_err(),
            "no image_state should be emitted for a rejected ensure"
        );
        assert!(m.register_images().is_empty());
        assert!(m.ops.lock().unwrap().is_empty());
    }

    // terminal-state delivery

    /// The cleared slot `run_worker` leaves behind.
    fn finish_op(m: &Arc<ImageManager>, image_id: &str) {
        m.ops.lock().unwrap().remove(image_id);
    }

    #[test]
    fn a_terminal_state_reached_while_disconnected_is_flushed_on_the_next_attach() {
        let m = mgr();
        // A pull completing with no connection attached: `ready` has nothing behind
        // it, so it must not vanish. emit_terminal cannot deliver and must not panic
        // or track it; attach re-syncs every op-free record regardless.
        assert!(m.plan_ensure("steam", REF1, "v1"));
        let generation = m.ops.lock().unwrap()["steam"].generation;
        m.commit("steam", generation, |rec| rec.mark_ready(4242));
        m.emit_terminal("steam");
        finish_op(&m, "steam");

        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);
        match rx.try_recv().expect("attach re-emits the pending terminal") {
            AgentMsg::ImageState {
                image_id,
                state,
                bytes,
                ..
            } => {
                assert_eq!(image_id, "steam");
                assert_eq!(state, "ready");
                assert_eq!(bytes, 4242);
            }
            other => panic!("expected ImageState, got {other:?}"),
        }
    }

    #[test]
    fn attach_re_emits_every_op_free_record_but_never_one_with_an_op_in_flight() {
        let mut records = BTreeMap::new();
        records.insert("steam".to_string(), ready("v1"));
        records.insert("retro".to_string(), ready("v9"));
        let m = mgr_with(records);
        // "retro" has a worker in flight; re-emitting a stale `ready` would race it.
        assert!(m.plan_ensure("retro", REF2, "v10"));

        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);
        let mut seen = Vec::new();
        while let Ok(AgentMsg::ImageState {
            image_id, state, ..
        }) = rx.try_recv()
        {
            seen.push((image_id, state));
        }
        assert_eq!(seen, vec![("steam".to_string(), "ready".to_string())]);
    }

    #[test]
    fn a_terminal_state_is_delivered_when_an_upstream_is_attached() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);
        assert!(m.plan_ensure("steam", REF1, "v1"));
        let generation = m.ops.lock().unwrap()["steam"].generation;
        let _pulling = rx.try_recv().expect("the staged pulling emit");
        m.commit("steam", generation, |rec| rec.mark_failed("network error"));
        m.emit_terminal("steam");
        match rx.try_recv().expect("terminal delivered") {
            AgentMsg::ImageState { state, error, .. } => {
                assert_eq!(state, "failed");
                assert_eq!(error, "network error");
            }
            other => panic!("expected ImageState, got {other:?}"),
        }
    }

    // reconnect reconciliation

    #[test]
    fn refresh_demotes_a_record_whose_image_the_daemon_no_longer_has() {
        // An empty committed ref exercises the reconcile path with no docker call.
        let mut records = BTreeMap::new();
        let mut rec = ready("v1");
        rec.registry_ref = String::new();
        records.insert("steam".to_string(), rec);
        let m = mgr_with(records);

        let entries = m.refresh_register_images();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].state, "absent");
        assert_eq!(
            m.records.lock().unwrap()["steam"].state,
            ImageState::Absent,
            "the record itself is updated, not just the report"
        );
    }

    #[test]
    fn refresh_never_touches_a_record_with_an_op_in_flight() {
        let m = mgr();
        assert!(m.plan_ensure("steam", REF1, "v1"));
        let entries = m.refresh_register_images();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].state, "pulling", "the in-flight pull is intact");
        assert_eq!(
            m.records.lock().unwrap()["steam"].state,
            ImageState::Pulling
        );
    }

    // command bounds

    fn ack_error(msg: &AgentMsg) -> Option<String> {
        match msg {
            AgentMsg::Ack {
                ok: false, error, ..
            } => error.clone(),
            other => panic!("expected a rejecting Ack, got {other:?}"),
        }
    }

    #[test]
    fn oversized_or_empty_command_fields_are_rejected() {
        let long_id = "a".repeat(MAX_IMAGE_ID_LEN + 1);
        let long_version = "v".repeat(MAX_VERSION_LEN + 1);
        // Structurally valid but too long, so length rejects it, not the shape check.
        let long_ref = format!("ghcr.io/x/{}:sha-abc1234", "y".repeat(MAX_REGISTRY_REF_LEN));
        assert!(looks_like_a_concrete_ref(&long_ref));

        for (image_id, registry_ref, version) in [
            ("", REF1, "v1"),
            ("   ", REF1, "v1"),
            (long_id.as_str(), REF1, "v1"),
            ("steam", long_ref.as_str(), "v1"),
            ("steam", REF1, long_version.as_str()),
        ] {
            let m = mgr();
            let reply = m.handle_ensure(
                "cmd".to_string(),
                image_id.to_string(),
                registry_ref.to_string(),
                version.to_string(),
            );
            assert_eq!(
                ack_error(&reply).as_deref(),
                Some("malformed image_ensure: image_id/registry_ref/version"),
                "should reject ensure({image_id:?}, len(ref)={}, len(version)={})",
                registry_ref.len(),
                version.len()
            );
            assert!(m.ops.lock().unwrap().is_empty());
            assert!(m.register_images().is_empty());
        }

        for image_id in ["", "   ", long_id.as_str()] {
            let m = mgr();
            let reply = m.handle_remove("cmd".to_string(), image_id.to_string());
            assert_eq!(
                ack_error(&reply).as_deref(),
                Some("malformed image_remove: image_id"),
                "should reject remove({image_id:?})"
            );
            assert!(m.ops.lock().unwrap().is_empty());
        }

        // Bounds are inclusive: at-the-limit values are accepted.
        let m = mgr();
        let at_limit = "a".repeat(MAX_IMAGE_ID_LEN);
        m.ops.lock().unwrap().insert(
            at_limit.clone(),
            OpSlot {
                running: ensure("v1", REF1),
                generation: 1,
                pending: None,
            },
        );
        match m.handle_ensure(
            "cmd".to_string(),
            at_limit,
            REF1.to_string(),
            "v".repeat(MAX_VERSION_LEN),
        ) {
            AgentMsg::Ack { ok, .. } => assert!(ok),
            other => panic!("expected Ack, got {other:?}"),
        }
    }

    #[test]
    fn a_new_image_id_is_refused_once_the_op_slots_are_full() {
        let m = mgr();
        {
            let mut ops = m.ops.lock().unwrap();
            for i in 0..MAX_CONCURRENT_OPS {
                ops.insert(
                    format!("img-{i}"),
                    OpSlot {
                        running: ensure("v1", REF1),
                        generation: i as u64,
                        pending: None,
                    },
                );
            }
        }
        let reply = m.handle_ensure(
            "cmd-1".to_string(),
            "steam".to_string(),
            REF1.to_string(),
            "v1".to_string(),
        );
        assert_eq!(
            ack_error(&reply).as_deref(),
            Some("image operation queue full")
        );
        let reply = m.handle_remove("cmd-2".to_string(), "steam".to_string());
        assert_eq!(
            ack_error(&reply).as_deref(),
            Some("image operation queue full")
        );
        assert!(m.register_images().is_empty(), "no record was created");

        // An id already holding a slot is unaffected by the cap.
        match m.handle_ensure(
            "cmd-3".to_string(),
            "img-7".to_string(),
            REF1.to_string(),
            "v1".to_string(),
        ) {
            AgentMsg::Ack { ok, error, .. } => {
                assert!(ok, "existing ids stay accepted under the cap");
                assert!(error.is_none());
            }
            other => panic!("expected Ack, got {other:?}"),
        }
        match m.handle_remove("cmd-4".to_string(), "img-7".to_string()) {
            AgentMsg::Ack { ok, .. } => assert!(ok),
            other => panic!("expected Ack, got {other:?}"),
        }
        assert_eq!(
            m.ops.lock().unwrap()["img-7"].pending,
            Some(ImageOp::Remove)
        );
        assert_eq!(m.ops.lock().unwrap().len(), MAX_CONCURRENT_OPS);
    }

    #[test]
    fn emissions_without_an_attached_upstream_are_dropped_not_fatal() {
        let m = mgr();
        m.plan_ensure("steam", REF1, "v1");
        assert_eq!(
            m.records.lock().unwrap()["steam"].state,
            ImageState::Pulling
        );
    }

    #[test]
    fn detaching_the_upstream_stops_emissions_to_the_old_connection() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        {
            let _guard = m.attach_upstream(tx);
            m.plan_ensure("steam", REF1, "v1");
            assert!(rx.try_recv().is_ok());
        }
        m.plan_ensure("steam", REF2, "v2");
        assert!(
            rx.try_recv().is_err(),
            "a detached connection must receive nothing"
        );
    }

    // image-management P4: template build

    const TAG1: &str = "quasar-local/x:v1";
    const TAG2: &str = "quasar-local/x:v2";
    const CTX_URL: &str = "https://codeload.github.com/accreleus/quasar-images/tar.gz/deadbeef";

    fn build_op(version: &str, local_tag: &str) -> ImageOp {
        ImageOp::Build {
            version: version.to_string(),
            local_tag: local_tag.to_string(),
            context_url: CTX_URL.to_string(),
            context_subdir: "x".to_string(),
            dockerfile: "Dockerfile".to_string(),
            build_args: BTreeMap::new(),
        }
    }

    // pure decision logic: build (mirrors the ensure state machine)

    #[test]
    fn build_no_record_nothing_running_starts() {
        assert_eq!(
            decide_build(None, None, None, build_op("v1", TAG1), "v1"),
            OpDecision::Start(build_op("v1", TAG1))
        );
    }

    #[test]
    fn build_ready_at_requested_version_is_idempotent() {
        let mut rec = ready("v1");
        rec.registry_ref = TAG1.to_string();
        assert_eq!(
            decide_build(Some(&rec), None, None, build_op("v1", TAG1), "v1"),
            OpDecision::ReAck
        );
    }

    #[test]
    fn build_same_op_running_is_idempotent_and_newer_version_queues() {
        let running = build_op("v1", TAG1);
        assert_eq!(
            decide_build(None, Some(&running), None, build_op("v1", TAG1), "v1"),
            OpDecision::ReAck
        );
        assert_eq!(
            decide_build(None, Some(&running), None, build_op("v2", TAG2), "v2"),
            OpDecision::Queue(build_op("v2", TAG2))
        );
    }

    #[test]
    fn build_newer_pending_replaces_and_prior_failure_retries() {
        let running = build_op("v1", TAG1);
        let pending = build_op("v2", TAG2);
        assert_eq!(
            decide_build(
                None,
                Some(&running),
                Some(&pending),
                build_op("v3", TAG1),
                "v3"
            ),
            OpDecision::Queue(build_op("v3", TAG1))
        );
        // A prior failure at the same version retries rather than dedupes.
        let mut rec = failed("v1");
        rec.registry_ref = TAG1.to_string();
        assert_eq!(
            decide_build(Some(&rec), None, None, build_op("v1", TAG1), "v1"),
            OpDecision::Start(build_op("v1", TAG1))
        );
    }

    // serialization bookkeeping (no threads, no docker)

    #[test]
    fn plan_build_stages_a_building_record_synchronously() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);

        assert!(m.plan_build("tmpl", build_op("v1", TAG1), TAG1, "v1"));
        {
            let records = m.records.lock().unwrap();
            let rec = &records["tmpl"];
            assert_eq!(rec.state, ImageState::Building);
            assert_eq!(rec.wire_version(), "v1");
            // local_tag is staged, not committed.
            assert!(rec.registry_ref.is_empty());
        }
        match rx.try_recv().expect("start emits current state") {
            AgentMsg::ImageState { state, version, .. } => {
                assert_eq!(state, "building");
                assert_eq!(version, "v1");
            }
            other => panic!("expected ImageState, got {other:?}"),
        }
        // A duplicate build in the gap re-acks (no second worker).
        assert!(!m.plan_build("tmpl", build_op("v1", TAG1), TAG1, "v1"));
    }

    #[test]
    fn a_successful_build_commits_the_staged_local_tag() {
        let m = mgr();
        assert!(m.plan_build("tmpl", build_op("v2", TAG2), TAG2, "v2"));
        let generation = m.ops.lock().unwrap()["tmpl"].generation;
        m.commit("tmpl", generation, |rec| rec.mark_ready(777));

        let records = m.records.lock().unwrap();
        let rec = &records["tmpl"];
        assert_eq!(rec.state, ImageState::Ready);
        // local_tag lands in the same field a pulled registry_ref would.
        assert_eq!(rec.registry_ref, TAG2);
        assert_eq!(rec.version, "v2");
        assert_eq!(rec.bytes, 777);
        assert!(rec.staged.is_none());
    }

    #[test]
    fn build_shares_the_op_slot_with_ensure_and_remove() {
        let m = mgr();
        assert!(m.plan_build("tmpl", build_op("v1", TAG1), TAG1, "v1"));
        assert!(!m.plan_remove("tmpl"));
        assert_eq!(m.ops.lock().unwrap()["tmpl"].pending, Some(ImageOp::Remove));
    }

    // handle_build: host allowlist + field validation

    #[test]
    fn handle_build_rejects_a_disallowed_or_non_https_source() {
        for url in [
            "http://codeload.github.com/x/y/tar.gz/abc",  // not HTTPS
            "https://evil.example/x/y/tar.gz/abc",        // not allowlisted
            "https://codeload.github.com@evil.example/x", // userinfo bypass
        ] {
            let m = mgr();
            let (tx, mut rx) = mpsc::channel(8);
            let _guard = m.attach_upstream(tx);
            let reply = m.handle_build(
                "cmd".to_string(),
                "tmpl".to_string(),
                url.to_string(),
                "x".to_string(),
                "Dockerfile".to_string(),
                BTreeMap::new(),
                TAG1.to_string(),
                "v1".to_string(),
            );
            assert_eq!(
                ack_error(&reply).as_deref(),
                Some("disallowed build source"),
                "should reject {url}"
            );
            assert!(rx.try_recv().is_err(), "no state emitted for {url}");
            assert!(m.ops.lock().unwrap().is_empty());
            assert!(m.register_images().is_empty());
        }
    }

    #[test]
    fn handle_build_rejects_malformed_fields_without_touching_state() {
        // (context_subdir, dockerfile, local_tag) triples that must be rejected.
        let cases = [
            ("../escape", "Dockerfile", TAG1), // subdir traversal
            ("x", "../../etc/pwn", TAG1),      // dockerfile traversal
            ("/abs", "Dockerfile", TAG1),      // absolute subdir
            ("", "Dockerfile", TAG1),          // empty subdir
            ("x", "Dockerfile", "-badtag"),    // local_tag looks like a flag
            ("x", "Dockerfile", "bad tag"),    // whitespace in local_tag
            ("x", "Dockerfile", ""),           // empty local_tag
        ];
        for (subdir, dockerfile, tag) in cases {
            let m = mgr();
            let reply = m.handle_build(
                "cmd".to_string(),
                "tmpl".to_string(),
                CTX_URL.to_string(),
                subdir.to_string(),
                dockerfile.to_string(),
                BTreeMap::new(),
                tag.to_string(),
                "v1".to_string(),
            );
            assert_eq!(
                ack_error(&reply).as_deref(),
                Some("malformed image_build: image_id/local_tag/context"),
                "should reject (subdir={subdir:?}, dockerfile={dockerfile:?}, tag={tag:?})"
            );
            assert!(m.ops.lock().unwrap().is_empty());
            assert!(m.register_images().is_empty());
        }
    }

    #[test]
    fn build_fields_ok_accepts_a_well_formed_command() {
        let mut args = BTreeMap::new();
        args.insert("BASE".to_string(), "alpine:3".to_string());
        assert!(build_fields_ok(
            "xfce-desktop",
            CTX_URL,
            "xfce-desktop",
            "Dockerfile",
            "quasar-local/xfce-desktop:2026.08.08",
            "2026.08.08",
            &args
        ));
        // A build-arg key with '=' or whitespace is rejected (argv safety).
        let mut bad = BTreeMap::new();
        bad.insert("A=B".to_string(), "v".to_string());
        assert!(!build_fields_ok(
            "x",
            CTX_URL,
            "x",
            "Dockerfile",
            TAG1,
            "v1",
            &bad
        ));
    }

    #[test]
    fn handle_build_accepts_a_well_formed_command_and_queues_behind_a_running_op() {
        let m = mgr();
        let (tx, mut rx) = mpsc::channel(8);
        let _guard = m.attach_upstream(tx);
        // Pre-occupy the slot so plan_build queues and spawns no worker: the accept
        // path is exercised without invoking docker.
        m.ops.lock().unwrap().insert(
            "tmpl".to_string(),
            OpSlot {
                running: build_op("v0", TAG1),
                generation: 1,
                pending: None,
            },
        );
        let reply = m.handle_build(
            "cmd".to_string(),
            "tmpl".to_string(),
            CTX_URL.to_string(),
            "x".to_string(),
            "Dockerfile".to_string(),
            BTreeMap::new(),
            TAG2.to_string(),
            "v2".to_string(),
        );
        match reply {
            AgentMsg::Ack { ok, error, .. } => {
                assert!(ok);
                assert!(error.is_none());
            }
            other => panic!("expected Ack, got {other:?}"),
        }
        assert_eq!(
            m.ops.lock().unwrap()["tmpl"].pending,
            Some(build_op("v2", TAG2))
        );
        while rx.try_recv().is_ok() {}
    }
}
