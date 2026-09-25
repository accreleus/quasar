//! The attempt journal (architecture §5.5, decision D8): one file per request in the
//! machine-state volume, `journal/<request-id>.json`, committed through [`DurableFile`]
//! (tmp + fsync + rename) **before** the phase it records is acted on. It is the only
//! record of an attempt: `status` projects it, `submit` answers a re-post from it, and
//! `resume` settles whatever it leaves open.
//!
//! Not a frozen interface (schema.md §"Not frozen"). A journal this build cannot read is
//! reported and left alone, never overwritten.

use std::io;
use std::path::{Path, PathBuf};

use quasar_runtime::DurableFile;
use serde::{Deserialize, Serialize};

use crate::engine::{ContainerSpec, RestartPolicy};
use crate::recipe::ImageRef;
use crate::socket::{AttemptResult, Reason, Request, State};

pub const FORMAT: u32 = 1;

/// How many finished attempts are kept for `status` and re-posts. The open attempt is
/// never pruned.
pub const KEEP_FINISHED: usize = 16;

/// Which socket submitted the attempt. A re-post of its request id is answered only to
/// the same caller (the #356 hand-off: an agent must not reuse the control plane's id).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum CallerTag {
    ControlPlane,
    Agent,
}

/// One component's progress. The phase names the step that is **about to be, or being,
/// acted on**: it is written before the step starts, so a crash anywhere leaves the step
/// the actor was in, and every step is idempotent when repeated.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    /// Journalled by `submit`; nothing has been done.
    Admitted,
    /// Pulling the image by digest.
    Pulling,
    /// Pulled; the recipe revision is supported and the specification rendered. The old
    /// container is still untouched.
    Checked,
    /// Taking the old container out of service: stop, disable its restart, rename it
    /// `.kept`. From here on an interruption continues rather than reports nothing.
    OldKept,
    /// Creating the new container.
    Created,
    /// Starting it.
    Started,
    /// Waiting for it to run and pass its health check.
    Verifying,
    /// Verified; recording its specification as the machine's.
    Verified,
    /// Removing the kept old container.
    OldDiscarded,
    /// Verification failed: removing the new container and bringing the kept one back.
    Restoring,
    /// This component is finished (replaced, or the attempt ended on it).
    Done,
}

impl Phase {
    /// Whether the old container has been (or is being) taken out of service. Before
    /// this, an interruption is settled as "nothing changed" (D8).
    pub fn touched_old(self) -> bool {
        !matches!(self, Phase::Admitted | Phase::Pulling | Phase::Checked)
    }

    /// agent-api.md `release_state.state` while this phase is current: `pulling` until
    /// the first container is taken out of service, `recreating` while one is replaced,
    /// `verifying` from the health wait on (a restore included).
    pub fn wire_state(self) -> State {
        match self {
            Phase::Admitted => State::Pending,
            Phase::Pulling | Phase::Checked => State::Pulling,
            Phase::OldKept | Phase::Created | Phase::Started => State::Recreating,
            Phase::Verifying
            | Phase::Verified
            | Phase::OldDiscarded
            | Phase::Restoring
            | Phase::Done => State::Verifying,
        }
    }
}

/// One component of the request, in request order.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Step {
    /// The component name (`node-agent`).
    pub name: String,
    pub image: ImageRef,
    pub phase: Phase,
    /// The container that ran before, by id, recorded before it is touched so a restore
    /// finds it whatever it has been renamed to.
    pub old_container: Option<String>,
    pub old_restart: Option<RestartPolicy>,
    /// The digest it ran, for `previous`.
    pub old_digest: Option<String>,
    /// The recipe revision the new image declares, and the specification rendered from
    /// it (written at `checked`).
    pub revision: Option<u32>,
    pub spec: Option<ContainerSpec>,
    pub new_container: Option<String>,
    /// Why verification failed, carried into `restoring` so the outcome keeps it.
    pub failure: Option<Failure>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Failure {
    pub reason: Reason,
    pub detail: String,
}

/// One attempt, as the actor last committed it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Journal {
    pub format: u32,
    /// Orders attempts: one more than the highest this machine had when it was admitted.
    pub seq: u64,
    pub caller: CallerTag,
    pub request: Request,
    pub steps: Vec<Step>,
    /// The attempt as `status` serves it: kept current with every phase, so the wire
    /// projection is never recomputed from a half-read journal.
    pub result: AttemptResult,
}

impl Journal {
    pub fn is_open(&self) -> bool {
        !self.result.state.is_terminal()
    }

    /// The first component that is not finished.
    pub fn current(&self) -> Option<usize> {
        self.steps.iter().position(|s| s.phase != Phase::Done)
    }

    /// The projected state of an open attempt: the state stays monotonic over the whole
    /// request, so a later component's pull does not move it back from `recreating`.
    pub fn project(&mut self) {
        if !self.is_open() {
            return;
        }
        let mut state = State::Pending;
        for step in &self.steps {
            let s = step.phase.wire_state();
            if rank(s) > rank(state) {
                state = s;
            }
            if step.phase != Phase::Done {
                break;
            }
        }
        if rank(state) > rank(self.result.state) {
            self.result.state = state;
        }
    }
}

fn rank(s: State) -> u8 {
    match s {
        State::Pending => 0,
        State::Pulling => 1,
        State::Recreating => 2,
        State::Verifying => 3,
        State::Succeeded | State::Failed => 4,
    }
}

/// The journal directory, inside machine state.
pub struct JournalDir {
    dir: PathBuf,
}

impl JournalDir {
    pub fn new(machine_root: &Path) -> Self {
        JournalDir {
            dir: machine_root.join("journal"),
        }
    }

    fn ensure(&self) -> io::Result<()> {
        use std::os::unix::fs::DirBuilderExt;
        match std::fs::DirBuilder::new().mode(0o700).create(&self.dir) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => Ok(()),
            Err(e) => Err(e),
        }
    }

    /// `request_id` must already be a uuid: it becomes a file name.
    fn file(&self, request_id: &str) -> DurableFile<Journal> {
        DurableFile::new(self.dir.join(format!("{request_id}.json")), "json.tmp")
    }

    pub fn load(&self, request_id: &str) -> io::Result<Option<Journal>> {
        if !self.dir.join(format!("{request_id}.json")).exists() {
            return Ok(None);
        }
        let journal = self.file(request_id).load()?;
        if let Some(j) = &journal {
            if j.format != FORMAT {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!(
                        "journal {request_id} is format {}, this actor reads {FORMAT}",
                        j.format
                    ),
                ));
            }
        }
        Ok(journal)
    }

    /// Committed (fsync'd) when this returns.
    pub fn store(&self, journal: &Journal) -> io::Result<()> {
        self.ensure()?;
        self.file(&journal.request.request_id).store(journal)
    }

    /// Every readable journal, oldest first. One that cannot be read is skipped here and
    /// reported by the caller that needs it.
    pub fn all(&self) -> Vec<Journal> {
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return Vec::new();
        };
        let mut out: Vec<Journal> = entries
            .flatten()
            .filter_map(|e| {
                let name = e.file_name().into_string().ok()?;
                let id = name.strip_suffix(".json")?;
                self.load(id).ok().flatten()
            })
            .collect();
        out.sort_by_key(|j| j.seq);
        out
    }

    /// The attempt that is still open, if any. The journal says so, not memory.
    pub fn open(&self) -> Option<Journal> {
        self.all().into_iter().rev().find(Journal::is_open)
    }

    pub fn latest(&self) -> Option<Journal> {
        self.all().pop()
    }

    pub fn next_seq(&self) -> u64 {
        self.all().last().map(|j| j.seq + 1).unwrap_or(1)
    }

    /// Drops the oldest finished journals beyond [`KEEP_FINISHED`].
    pub fn prune(&self) {
        let finished: Vec<Journal> = self.all().into_iter().filter(|j| !j.is_open()).collect();
        let excess = finished.len().saturating_sub(KEEP_FINISHED);
        for j in finished.into_iter().take(excess) {
            let _ = std::fs::remove_file(self.dir.join(format!("{}.json", j.request.request_id)));
        }
    }
}

/// The last `limit` bytes of `text`, cut from the front at a line boundary: the error is
/// at the end (agent-api.md `release_state.output`).
pub fn tail_output(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.to_owned();
    }
    let mut start = text.len() - limit;
    while !text.is_char_boundary(start) {
        start += 1;
    }
    let cut = &text[start..];
    match cut.find('\n') {
        Some(i) if i + 1 < cut.len() => cut[i + 1..].to_owned(),
        _ => cut.to_owned(),
    }
}

pub const OUTPUT_LIMIT: usize = 8192;
pub const LOG_TAIL_LIMIT: usize = 3072;
