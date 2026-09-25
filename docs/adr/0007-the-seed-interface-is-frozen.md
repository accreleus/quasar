---
status: accepted
date: 2026-09-25
---
# The seed's interface is one state file, two labels and a compiled actor profile, frozen at version 1

On an owned machine the only container an operator's manager (Dockge, Arcane, a `docker run`
line) declares for Quasar is the **seed**. It makes sure the machine's **recovery actor** exists
and does nothing else; the recovery actor then creates and replaces every other platform service,
itself included (#352 decisions 3 and 12). The manager, not Quasar, updates the seed, and a
manager may keep running a seed image for years. So whatever the seed reads from the machine, and
whatever it creates, must keep meaning the same thing to every recovery actor released after it.
We decided that the seed's whole interface is exactly three things, frozen together as **seed
interface 1**, and that every released recovery actor is tested against it.

## What is frozen

1. **The state file, `seed.json`, format 1**, in the recovery actor's machine-state volume. It is
   written only by a recovery actor — atomically, by temporary file, fsync and rename — and only
   read by the seed. It says four things and nothing else:
   - `format_version` — `1`;
   - `installation_id` — the id of the Quasar installation on this container engine;
   - the **last verified recovery-actor image**, as a repository and a `sha256:` digest (never a
     tag, ADR 0001) — the image the seed re-creates the actor from;
   - `state` — `active`, or `uninstalled` once the operator's `uninstall` command or a console
     "remove host" has taken the machine's services away.

   A recovery actor rewrites the image only after it has **verified** a successor (the hand-over),
   so the seed never re-creates an actor from an image that was never shown to work. A seed that
   meets an unknown `format_version` does nothing and logs why; it never guesses.
2. **The two labels**, `io.quasar.installation=<installation id>` and
   `io.quasar.platform-service=recovery-actor`. The seed treats **any** container carrying both —
   running or stopped, under any name, including a hand-over's kept and successor containers — as
   "a recovery actor exists", and then does nothing. Other labels a recovery actor stamps (recipe
   revision, specification digest, attempt) are not part of this interface.
3. **The compiled actor profile** — the container the seed creates when no recovery actor exists:
   its name, restart policy, the container-engine socket at the path the seed itself was given,
   the machine-state volume, the command that starts the recovery actor, and the seed's own
   container identity, so the actor can report which seed the machine has. It is compiled into the
   seed path of the binary, never read from an image label or from a file an image supplies.

The seed's loop is correspondingly small: if `seed.json` says `uninstalled`, log and idle; if a
container with both labels exists, do nothing; otherwise create the actor from the profile, from
`seed.json`'s verified image or, on first install when there is no `seed.json`, from the seed's
own image. It never stops, replaces or removes anything, and never talks to the control plane.

## The contract-test obligation

A contract test runs the **current** seed code against machine-state fixtures written by **every
released recovery actor** — a `seed.json` and a set of labelled containers per release, appended
with each release — and asserts the seed makes the same decision it would have made when that actor
shipped. The converse holds too: every recovery actor must start correctly from the frozen profile
(the seed may re-create any later actor that way after the actor container was deleted) and then
bring its own container to its current shape through its own recipe (ADR 0008). A change that
fails either direction is a new seed interface, which needs its own ADR and owner sign-off.

## Considered options

- **The recovery actor itself as the manager-declared container** (rejected, #352 decision 3): a
  manager redeploy would then race Quasar replacing the actor one level down.
- **The manager's template also declaring the control plane and Postgres** (rejected): a redeploy
  or a GitOps update would recreate them from an older image — a duplicate role, a port conflict,
  and potentially a control plane older than the database (the ADR 0002 crash loop).
- **A separate seed binary and image** (rejected): a second container-engine client to maintain
  and certify, for no behavioural gain; the seed is a mode of the recovery-actor binary, dispatched
  before any actor initialisation.
- **A profile read from an image label** (rejected): the seed would then let an image decide what
  host access its own container gets, which is the authority boundary ADR 0008 keeps in reviewed
  code.

## Consequences

- The seed is built never to need an update; if one ever does, the console says so and the
  operator updates it in the manager. The recovery actor reports the seed's version, or its
  absence, on the agent's `register` (`protocol/agent-api.md` amendment 14, `seed_version`).
- Removing or redeploying the seed never removes or restarts Quasar. A missing seed only means a
  deleted recovery actor cannot be re-created on that machine.
- The seed pins nothing about the actor beyond `seed.json`; a manager that updates the seed image
  is harmless, because a seed never replaces an existing actor.
- The interface lives here rather than in `protocol/`: it is machine-local, and the recovery
  actor's sockets and journal, which it sits beside, are explicitly not frozen there.
