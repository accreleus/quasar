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
    // The recovery actor's own replacement, its hand-over (`crate::handover`). Two
    // processes share it: the old actor drives it to `handing_over`, the successor from
    // `successor_active` on. Appended, so a journal an older actor wrote still reads.
    /// Creating the successor, `quasar-recovery.next`, beside the running actor.
    CreatingSuccessor,
    /// Starting it. It self-checks and writes its ready marker.
    StartingSuccessor,
    /// Waiting for the ready marker; the old actor still holds the lease and serves.
    AwaitingSuccessor,
    /// The old actor stopped serving and released the lease; the successor takes it.
    HandingOver,
    /// The successor holds the lease and owns the attempt: taking the old actor out of
    /// service (stop, disable its restart, rename it `.kept`).
    SuccessorActive,
    /// The successor renaming itself to the actor's name.
    SuccessorRenaming,
    /// The successor has put the old actor back under its name and is starting it; the
    /// old actor finishes the restore once it holds the lease.
    HandingBack,
}

impl Phase {
    /// Whether the old container has been (or is being) taken out of service. Before
    /// this, an interruption is settled as "nothing changed" (D8).
    pub fn touched_old(self) -> bool {
        !matches!(
            self,
            Phase::Admitted
                | Phase::Pulling
                | Phase::Checked
                | Phase::CreatingSuccessor
                | Phase::StartingSuccessor
                | Phase::AwaitingSuccessor
        )
    }

    /// agent-api.md `release_state.state` while this phase is current: `pulling` until
    /// the first container is taken out of service, `recreating` while one is replaced,
    /// `verifying` from the health wait on (a restore included).
    pub fn wire_state(self) -> State {
        match self {
            Phase::Admitted => State::Pending,
            // A successor running beside the old actor has taken nothing out of service.
            Phase::Pulling
            | Phase::Checked
            | Phase::CreatingSuccessor
            | Phase::StartingSuccessor
            | Phase::AwaitingSuccessor => State::Pulling,
            Phase::OldKept
            | Phase::Created
            | Phase::Started
            | Phase::HandingOver
            | Phase::SuccessorActive
            | Phase::SuccessorRenaming => State::Recreating,
            Phase::Verifying
            | Phase::Verified
            | Phase::OldDiscarded
            | Phase::Restoring
            | Phase::HandingBack
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
    /// A hand-over only: how many times a successor process has taken the lease for this
    /// step. Bounds a successor that crash-loops once the old actor is stopped.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub successor_starts: u32,
}

fn is_zero(n: &u32) -> bool {
    *n == 0
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
            // The new directory entry is durable only once its parent is synced.
            Ok(()) => match self.dir.parent() {
                Some(parent) => std::fs::File::open(parent)?.sync_all(),
                None => Ok(()),
            },
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => Ok(()),
            Err(e) => Err(e),
        }
    }

    /// Every request id this machine ever admitted, kept after its journal is pruned,
    /// so an id is never admitted twice.
    fn used_file(&self) -> DurableFile<Vec<String>> {
        DurableFile::new(self.dir.join("used-ids"), "tmp")
    }

    pub fn was_used(&self, request_id: &str) -> io::Result<bool> {
        if !self.dir.join("used-ids").exists() {
            return Ok(false);
        }
        Ok(self
            .used_file()
            .load()?
            .is_some_and(|ids| ids.iter().any(|id| id == request_id)))
    }

    pub fn mark_used(&self, request_id: &str) -> io::Result<()> {
        self.ensure()?;
        let mut ids = if self.dir.join("used-ids").exists() {
            self.used_file().load()?.unwrap_or_default()
        } else {
            Vec::new()
        };
        if ids.iter().any(|id| id == request_id) {
            return Ok(());
        }
        ids.push(request_id.to_owned());
        self.used_file().store(&ids)
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

    /// Every journal file, oldest first, and the ids of those that exist but cannot be
    /// read. An unreadable one fails closed: it counts as an open attempt everywhere
    /// ([`Scan::open_id`]), because it may be exactly the attempt a restore depends on.
    pub fn scan(&self) -> Scan {
        let mut scan = Scan::default();
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return scan;
        };
        for e in entries.flatten() {
            let Ok(name) = e.file_name().into_string() else {
                continue;
            };
            let Some(id) = name.strip_suffix(".json") else {
                continue;
            };
            match self.load(id) {
                Ok(Some(j)) => scan.journals.push(j),
                Ok(None) => {}
                Err(_) => scan.unreadable.push(id.to_owned()),
            }
        }
        scan.journals.sort_by_key(|j| j.seq);
        scan.unreadable.sort();
        scan
    }

    pub fn latest(&self) -> Option<Journal> {
        self.scan().journals.pop()
    }

    pub fn next_seq(&self) -> u64 {
        self.scan().journals.last().map(|j| j.seq + 1).unwrap_or(1)
    }

    /// Drops the oldest finished journals beyond [`KEEP_FINISHED`]; their ids stay in the
    /// used-id record.
    pub fn prune(&self) {
        let finished: Vec<Journal> = self
            .scan()
            .journals
            .into_iter()
            .filter(|j| !j.is_open())
            .collect();
        let excess = finished.len().saturating_sub(KEEP_FINISHED);
        for j in finished.into_iter().take(excess) {
            let id = &j.request.request_id;
            if self.mark_used(id).is_ok() {
                let _ = std::fs::remove_file(self.dir.join(format!("{id}.json")));
            }
        }
    }
}

#[derive(Debug, Default)]
pub struct Scan {
    pub journals: Vec<Journal>,
    /// Ids of journal files that exist but do not parse, or are of a format this build
    /// does not read.
    pub unreadable: Vec<String>,
}

impl Scan {
    /// The attempt that is still open, if any. The journal says so, not memory.
    pub fn open(&self) -> Option<&Journal> {
        self.journals.iter().rev().find(|j| j.is_open())
    }

    /// The open attempt's id, an unreadable journal's first: either stops a new one.
    pub fn open_id(&self) -> Option<String> {
        self.unreadable
            .first()
            .cloned()
            .or_else(|| self.open().map(|j| j.request.request_id.clone()))
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

/// A container's log tail as an attempt's output embeds it: without terminal escape
/// sequences (a service logging in colour to a pipe), cut to [`LOG_TAIL_LIMIT`].
pub fn embedded_log_tail(tail: &str) -> String {
    tail_output(strip_ansi(tail).trim_end(), LOG_TAIL_LIMIT)
}

/// `text` without ANSI escape sequences: CSI (`ESC [ … final`), OSC (`ESC ] … BEL|ESC \`)
/// and two-byte escapes.
pub fn strip_ansi(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        if c != '\u{1b}' {
            out.push(c);
            continue;
        }
        match chars.next() {
            Some('[') => {
                for c in chars.by_ref() {
                    if ('\u{40}'..='\u{7e}').contains(&c) {
                        break;
                    }
                }
            }
            Some(']') => {
                while let Some(c) = chars.next() {
                    if c == '\u{7}' || (c == '\u{1b}' && chars.next_if_eq(&'\\').is_some()) {
                        break;
                    }
                }
            }
            _ => {}
        }
    }
    out
}

#[cfg(test)]
mod ansi_tests {
    use super::*;

    #[test]
    fn a_coloured_log_line_is_embedded_as_plain_text() {
        let line = "\u{1b}[2m2026-09-26T04:41:52Z\u{1b}[0m \u{1b}[31mERROR\u{1b}[0m \u{1b}[2mquasar_recovery\u{1b}[0m\u{1b}[2m:\u{1b}[0m lease held \u{1b}[3mtoken\u{1b}[0m\u{1b}[2m=\u{1b}[0m\"x\"\n";
        assert_eq!(
            embedded_log_tail(line),
            "2026-09-26T04:41:52Z ERROR quasar_recovery: lease held token=\"x\""
        );
        assert_eq!(
            strip_ansi("a\u{1b}]0;title\u{7}b\u{1b}]8;;u\u{1b}\\c\u{1b}7d"),
            "abcd"
        );
        assert_eq!(strip_ansi("plain: ünïcode"), "plain: ünïcode");
    }
}
