//! The settle table (architecture §5.5 and §5.6, decision D8): what `resume` does with an
//! attempt a restart left open. Pure: it reads only the journal and which process is
//! asking ([`Party`]). The engine is consulted afterwards by the continuation itself, whose
//! every step is idempotent.
//!
//! A service's component (the node agent):
//!
//! | Last journalled phase of the current component | Outcome |
//! |---|---|
//! | `admitted`, `pulling`, `checked`, `dumping` (the old container untouched) | **interrupted**: `failed` with reason `interrupted`; this component changed nothing (a dump it took is discarded), nothing is retried |
//! | `old_kept` … `verifying` | continue to verification; on failure restore (ADR 0004 amendment) |
//! | `restoring` | finish the restore; `failed`, `restored` as it turns out |
//! | `verified`, `old_discarded` | finish discarding the old container; **succeeded** |
//! | terminal | nothing |
//!
//! The recovery actor's own component, its hand-over (`crate::handover`), by party:
//!
//! | Phase of the actor's component | old actor | successor | neither (a re-created actor) |
//! |---|---|---|---|
//! | `admitted` … `awaiting_successor` | interrupted | continue: the old actor is gone | interrupted |
//! | `handing_over` | restore: the successor never took over | continue | restore |
//! | `successor_active` … `verifying` | restore: the successor died before verifying | continue, or restore after [`MAX_SUCCESSOR_STARTS`] | restore |
//! | `verified`, `old_discarded` | yield to the verified successor | continue | continue on the successor's image, else restore |
//! | `restoring` | restore | hand back | restore |
//! | `handing_back` | restore | yield to the old actor | restore |
//!
//! A later component, once the actor's is done, settles by the first table; an old actor
//! that finds it yields to its successor.

use crate::journal::{Journal, Phase};

/// How many times a successor may take the lease for one hand-over before it gives the
/// machine back to the old actor: a successor that crash-loops after the old actor was
/// stopped would otherwise leave no actor working.
pub const MAX_SUCCESSOR_STARTS: u32 = 3;

/// The component name of the recovery actor's own replacement.
pub const RECOVERY_ACTOR: &str = "recovery-actor";

/// Which process is settling, relative to the attempt's recovery-actor component.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Party {
    /// The actor that ran when the attempt was admitted.
    Old,
    /// The container the hand-over created.
    Successor,
    /// Any other actor: one a seed re-created after every actor container was removed.
    /// `on_new_image`: it runs the successor's image.
    Stranger { on_new_image: bool },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Settlement {
    /// Already terminal: nothing to do.
    Terminal,
    /// Interrupted before the current component's old container was taken out of service:
    /// end the attempt `failed`/`interrupted`, removing what the attempt created for it.
    Interrupted,
    /// Drive the attempt on from its current phase to its ordinary outcome.
    Continue,
    /// The hand-over failed: put the old actor back (each party has its own way).
    Restore,
    /// Another process finishes this attempt: start it, release the lease and exit.
    Yield,
}

pub fn settle(journal: &Journal, party: Party) -> Settlement {
    if !journal.is_open() {
        return Settlement::Terminal;
    }
    // A restore (`crate::restore`): nothing is touched before `stopping`.
    if let Some(restore) = &journal.restore {
        return if restore.phase.touched() {
            Settlement::Continue
        } else {
            Settlement::Interrupted
        };
    }
    let Some(i) = journal.current() else {
        // Every component finished but the terminal record was not written.
        return Settlement::Continue;
    };
    let step = &journal.steps[i];
    if step.name == RECOVERY_ACTOR {
        return actor_step(step.phase, step.successor_starts, party);
    }
    let actor_done = journal
        .steps
        .iter()
        .take(i)
        .any(|s| s.name == RECOVERY_ACTOR && s.phase == Phase::Done);
    if actor_done && party == Party::Old {
        return Settlement::Yield;
    }
    if step.phase.touched_old() {
        Settlement::Continue
    } else {
        Settlement::Interrupted
    }
}

fn actor_step(phase: Phase, starts: u32, party: Party) -> Settlement {
    use Party::*;
    use Phase::*;
    match (phase, party) {
        (Restoring, _) => Settlement::Restore,
        (HandingBack, Successor) => Settlement::Yield,
        (HandingBack, _) => Settlement::Restore,
        (Verified | OldDiscarded, Old) => Settlement::Yield,
        (
            Verified | OldDiscarded,
            Stranger {
                on_new_image: false,
            },
        ) => Settlement::Restore,
        (Verified | OldDiscarded, _) => Settlement::Continue,
        (_, Successor) if phase.touched_old() && starts >= MAX_SUCCESSOR_STARTS => {
            Settlement::Restore
        }
        (_, Successor) => Settlement::Continue,
        (_, _) if phase.touched_old() => Settlement::Restore,
        (_, _) => Settlement::Interrupted,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::journal::{CallerTag, Phase, Step, FORMAT};
    use crate::recipe::ImageRef;
    use crate::socket::{AttemptResult, Reason, Release, Request, RequestKind, State};

    fn step(name: &str, phase: Phase) -> Step {
        Step {
            name: name.into(),
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
            successor_starts: 0,
            migrating: false,
            dump: None,
        }
    }

    fn journal_of(steps: Vec<Step>, state: State) -> Journal {
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
                from_version: None,
            },
            steps,
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
                dump: None,
            },
            restore: None,
        }
    }

    fn journal(phase: Phase, state: State) -> Journal {
        journal_of(vec![step("node-agent", phase)], state)
    }

    const PARTIES: [Party; 4] = [
        Party::Old,
        Party::Successor,
        Party::Stranger {
            on_new_image: false,
        },
        Party::Stranger { on_new_image: true },
    ];

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
            for party in [
                Party::Successor,
                Party::Stranger {
                    on_new_image: false,
                },
            ] {
                assert_eq!(settle(&j, party), want, "{phase:?} {party:?}");
            }
        }
        for state in [State::Succeeded, State::Failed] {
            for party in PARTIES {
                assert_eq!(
                    settle(&journal(Phase::Verifying, state), party),
                    Settlement::Terminal
                );
            }
        }
    }

    #[test]
    fn the_hand_over_table_holds_for_every_phase_and_party() {
        use Phase::*;
        use Settlement::*;
        let stranger = Party::Stranger {
            on_new_image: false,
        };
        let on_new = Party::Stranger { on_new_image: true };
        // (phase, old, successor, stranger, stranger on the new image)
        let rows = [
            (Admitted, Interrupted, Continue, Interrupted, Interrupted),
            (Pulling, Interrupted, Continue, Interrupted, Interrupted),
            (Checked, Interrupted, Continue, Interrupted, Interrupted),
            (
                CreatingSuccessor,
                Interrupted,
                Continue,
                Interrupted,
                Interrupted,
            ),
            (
                StartingSuccessor,
                Interrupted,
                Continue,
                Interrupted,
                Interrupted,
            ),
            (
                AwaitingSuccessor,
                Interrupted,
                Continue,
                Interrupted,
                Interrupted,
            ),
            (HandingOver, Restore, Continue, Restore, Restore),
            (SuccessorActive, Restore, Continue, Restore, Restore),
            (SuccessorRenaming, Restore, Continue, Restore, Restore),
            (Verifying, Restore, Continue, Restore, Restore),
            (Verified, Yield, Continue, Restore, Continue),
            (OldDiscarded, Yield, Continue, Restore, Continue),
            (Restoring, Restore, Restore, Restore, Restore),
            (HandingBack, Restore, Yield, Restore, Restore),
        ];
        for (phase, old, successor, other, other_new) in rows {
            let j = journal_of(vec![step(RECOVERY_ACTOR, phase)], State::Pulling);
            assert_eq!(settle(&j, Party::Old), old, "{phase:?} old");
            assert_eq!(
                settle(&j, Party::Successor),
                successor,
                "{phase:?} successor"
            );
            assert_eq!(settle(&j, stranger), other, "{phase:?} stranger");
            assert_eq!(settle(&j, on_new), other_new, "{phase:?} stranger on new");
        }
    }

    #[test]
    fn a_successor_that_keeps_restarting_gives_the_machine_back() {
        use Phase::*;
        for phase in [HandingOver, SuccessorActive, SuccessorRenaming, Verifying] {
            let mut s = step(RECOVERY_ACTOR, phase);
            s.successor_starts = MAX_SUCCESSOR_STARTS - 1;
            let j = journal_of(vec![s.clone()], State::Recreating);
            assert_eq!(
                settle(&j, Party::Successor),
                Settlement::Continue,
                "{phase:?}"
            );
            s.successor_starts = MAX_SUCCESSOR_STARTS;
            let j = journal_of(vec![s], State::Recreating);
            assert_eq!(
                settle(&j, Party::Successor),
                Settlement::Restore,
                "{phase:?}"
            );
        }
    }

    #[test]
    fn a_later_component_settles_by_its_own_phase_once_the_actor_moved() {
        use Phase::*;
        for (phase, want) in [
            (Admitted, Settlement::Interrupted),
            (Pulling, Settlement::Interrupted),
            (OldKept, Settlement::Continue),
            (Verifying, Settlement::Continue),
        ] {
            let j = journal_of(
                vec![step(RECOVERY_ACTOR, Done), step("node-agent", phase)],
                State::Recreating,
            );
            assert_eq!(settle(&j, Party::Successor), want, "{phase:?}");
            // The old actor, started again by hand, leaves it to its verified successor.
            assert_eq!(settle(&j, Party::Old), Settlement::Yield, "{phase:?}");
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
            (CreatingSuccessor, State::Pulling),
            (AwaitingSuccessor, State::Pulling),
            (HandingOver, State::Recreating),
            (SuccessorActive, State::Recreating),
            (HandingBack, State::Verifying),
        ] {
            let mut j = journal(phase, State::Pending);
            j.project();
            assert_eq!(j.result.state, want, "{phase:?}");
        }
    }
}
