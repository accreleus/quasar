# Triage Labels

The engineering skills speak in five canonical triage roles. This file maps those roles to the label strings actually used on `accreleus/quasar`.

Referenced from `CLAUDE.md` → "Agent skills".

## State roles

| Canonical role    | Label in this tracker | Meaning                                  |
| ----------------- | --------------------- | ---------------------------------------- |
| `needs-triage`    | `needs-triage`        | Maintainer needs to evaluate this issue  |
| `needs-info`      | `needs-info`          | Waiting on reporter for more information |
| `ready-for-agent` | `ready-for-agent`     | Fully specified, ready for an AFK agent  |
| `ready-for-human` | `ready-for-human`     | Requires human implementation            |
| `wontfix`         | `wontfix`             | Will not be actioned                     |

The defaults are unchanged — every label string equals its canonical role name. All five exist in the tracker; none need creating.

## Category roles

Orthogonal to state. Every triaged issue carries exactly one of:

| Role          | Label         |
| ------------- | ------------- |
| `bug`         | `bug`         |
| `enhancement` | `enhancement` |

So a fully triaged issue has **one category label and one state label** — e.g. `bug` + `ready-for-agent`. If two state labels are ever set at once, flag it and ask the maintainer before changing anything.

## State transitions

An untriaged issue starts at `needs-triage` and moves to `needs-info`, `ready-for-agent`, `ready-for-human`, or `wontfix`. `needs-info` returns to `needs-triage` once the reporter replies.

Two transitions worth naming because they are easy to get wrong:

- **`ready-for-agent` → `needs-info`** is correct when an issue stops being actionable by an agent and starts waiting on a human — e.g. #86, where the symptom stopped reproducing and the next step is the reporter testing other machines. The issue is not fixed and must not be closed; it is simply not agent work any more.
- A fix being **merged but not deployed** is not a state change. Leave the issue open in its existing state until the change is live and verified; note the merge in a comment.

## Other labels in the tracker

Not part of the triage state machine — don't treat them as state:

- `dependencies`, `github_actions`, `rust`, `go`, `javascript` — applied by Dependabot.
- `documentation`, `duplicate`, `good first issue`, `help wanted`, `invalid`, `question` — GitHub stock labels, largely unused here.
