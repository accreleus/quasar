//! The settle table (architecture §5.5, decision D8): what `resume` does with an attempt a
//! restart left open. Pure: it reads only the journal. The engine is consulted afterwards
//! by the continuation itself, whose every step is idempotent.
//!
//! | Last journalled phase of the current component | Outcome |
//! |---|---|
//! | `admitted`, `pulling`, `checked` (the old container untouched) | **interrupted**: `failed` with reason `interrupted`, nothing changed, nothing retried |
//! | `old_kept` … `verifying` | continue to verification; on failure restore (ADR 0004 amendment) |
//! | `restoring` | finish the restore; `failed`, `restored` as it turns out |
//! | `verified`, `old_discarded` | finish discarding the old container; **succeeded** |
//! | terminal | nothing |

use crate::journal::Journal;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Settlement {
    /// Already terminal: nothing to do.
    Terminal,
    /// Interrupted before the old container was taken out of service: end the attempt
    /// `failed`/`interrupted`, removing anything the attempt created.
    Interrupted,
    /// Drive the attempt on from its current phase to its ordinary outcome.
    Continue,
}

pub fn settle(journal: &Journal) -> Settlement {
    if !journal.is_open() {
        return Settlement::Terminal;
    }
    match journal.current().map(|i| journal.steps[i].phase) {
        // Every component finished but the terminal record was not written.
        None => Settlement::Continue,
        Some(phase) if phase.touched_old() => Settlement::Continue,
        Some(_) => Settlement::Interrupted,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::journal::{CallerTag, Phase, Step, FORMAT};
    use crate::recipe::ImageRef;
    use crate::socket::{AttemptResult, Reason, Release, Request, RequestKind, State};

    fn journal(phase: Phase, state: State) -> Journal {
        let release = Release {
            id: String::new(),
            version: None,
            source_commit: "c".repeat(40),
        };
        Journal {
            format: FORMAT,
            seq: 1,
            caller: CallerTag::Agent,
            request: Request {
                request_id: "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22".into(),
                kind: RequestKind::Replace,
                components: Vec::new(),
                release: release.clone(),
                migrates: false,
                schema_version: None,
                external_backup_confirmed: false,
                dump: None,
                purge: false,
                wait_timeout_s: 0,
            },
            steps: vec![Step {
                name: "node-agent".into(),
                image: ImageRef {
                    repository: "r".into(),
                    digest: "d".into(),
                },
                phase,
                old_container: None,
                old_restart: None,
                old_digest: None,
                revision: None,
                spec: None,
                new_container: None,
                failure: None,
            }],
            result: AttemptResult {
                request_id: "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22".into(),
                state,
                reason: (state == State::Failed).then_some(Reason::Unhealthy),
                components: Vec::new(),
                previous: Vec::new(),
                output: String::new(),
                started_at: String::new(),
                updated_at: String::new(),
                finished_at: None,
                restored: false,
                release,
            },
        }
    }

    #[test]
    fn the_table_holds_for_every_phase() {
        use Phase::*;
        let rows = [
            (Admitted, Settlement::Interrupted),
            (Pulling, Settlement::Interrupted),
            (Checked, Settlement::Interrupted),
            (OldKept, Settlement::Continue),
            (Created, Settlement::Continue),
            (Started, Settlement::Continue),
            (Verifying, Settlement::Continue),
            (Verified, Settlement::Continue),
            (OldDiscarded, Settlement::Continue),
            (Restoring, Settlement::Continue),
            (Done, Settlement::Continue),
        ];
        for (phase, want) in rows {
            let mut j = journal(phase, State::Pending);
            j.project();
            assert_eq!(settle(&j), want, "{phase:?}");
        }
        for state in [State::Succeeded, State::Failed] {
            assert_eq!(
                settle(&journal(Phase::Verifying, state)),
                Settlement::Terminal
            );
        }
    }

    #[test]
    fn the_projected_state_is_pulling_until_the_old_container_is_touched() {
        use Phase::*;
        for (phase, want) in [
            (Admitted, State::Pending),
            (Pulling, State::Pulling),
            (Checked, State::Pulling),
            (OldKept, State::Recreating),
            (Started, State::Recreating),
            (Verifying, State::Verifying),
            (Restoring, State::Verifying),
        ] {
            let mut j = journal(phase, State::Pending);
            j.project();
            assert_eq!(j.result.state, want, "{phase:?}");
        }
    }
}
