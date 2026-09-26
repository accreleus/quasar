---
status: accepted
date: 2026-09-11
---
# A node-agent apply whose new container fails its health wait is restored by the updater

The self-update design (#104) allowed exactly one automatic restore: a control-plane
container that **never started**, because with `State.StartedAt` zero no migration can
have run and there is no console left to press Revert in. A node-agent apply was
deliberately never restored — "a host that silently reverts hides the failure" — and its
remedy was the operator's `POST /v1/admin/platform/hosts/{id}/revert`.

The first external apply through the console (#185's incident trail, #152 in the field)
showed the gap: the updated agent refused to start because another process held its
health port, the run failed, and the host stayed down until the reporter found the port.
The operator's revert could not help, because a revert is a `release_apply` carried by
the host's agent — the process that would not start. The only actor with both the
information (`.env.prev`, the previous digests) and the access (the docker socket) is the
updater, which already performs this exact restore for a never-started control plane.

## Decision

When a **node-agent** request fails its health wait — `never_started`,
`recreate_failed` or `unhealthy` — the updater restores `.env.prev`, brings the previous
digest back up, captures the failed container's last log lines into the result's output
(which is where `health-bind-failed` is written), and reports the failure with
`restored: true` (`agent-api.md` `release_state`, amendment 9). One restore per attempt,
no retry; if the restore also fails, both failures are in the output and the host is
where a failed recreate always left it, with `previous` as the manual recipe.

The failure is not hidden. The restored agent's reconnect replays the result (#193), and
the control plane records the apply `failed` **and** inserts a `kind: auto_revert`
attempt beside it, already terminal, with the digest sets swapped — so history shows
both steps, and the fleet run still stops at that host. Both revert-target derivations
(server and console) skip `auto_revert` rows, so "Revert" never offers the release that
just failed.

`pull_failed` restores `.env` only: nothing was recreated, so the old container is still
running and the file merely stops naming an image that never arrived.

## Considered options

- **Updater-performed restore (chosen).** The updater has `.env.prev`, the previous
  digests, the docker socket, and already does this for the control plane. Reversing the
  "never for a host" rule costs one `restored` field and one history row.
- **Control-plane-issued revert on a failed health wait (#188's original text).**
  Unreachable in the case that motivated it: the revert travels over the agent socket of
  the agent that is down.
- **Leave it to the operator, improve the docs.** What existed before; the host stays
  down for as long as it takes a person to read the logs.

## Consequences

- **The control-plane rule is unchanged.** A control plane that *started* may have
  migrated the database and is never restored automatically (ADR 0002).
- The agent-api `release_state` gains an optional `restored`; an older control plane
  ignores it and sees the failure as before, minus the history row.
- A fleet run behaves as before on a reverted host: it stops there. A second host failing
  the same way is likely the same cause, and continuing is the thing the stop rule
  exists to prevent.
- The restored agent normally connects to the control plane before the updater has
  finished verifying the restore, so the result it finds on connect is still
  `verifying`. It therefore **adopts** that apply — watches the result to its terminal
  state and relays it — rather than re-emitting the state once (the pre-amendment
  replay, #193, which left the attempt `verifying` for ever on the first live gate).
  An agent older than this amendment does re-emit only once; restarting it after the
  updater has finished replays the terminal result.
- The updater's result output now includes the failed container's log tail, bounded
  inside the 8 KiB the wire carries. It never includes environment values.

## Amendment (2026-09-25, RH06, #353): the recovery actor, and a control plane that never passed a health check

On an owned machine (RH06, #352) the **recovery actor** replaces platform services instead of
the updater, and it keeps the old container **stopped and kept**, with its restart policy
disabled, until the new one is verified. Restoring is therefore restarting the kept container —
no `.env.prev` and no pull. The decision above is extended, and nothing in it is withdrawn:

- **The node agent**: unchanged in substance. A failed verification (`recreate_failed`,
  `never_started`, `unhealthy`) is restored automatically, reported `restored: true`, and recorded
  as a `kind: auto_revert` attempt beside the failed one.
- **The recovery actor itself** is restored the same way. A successor that never verifies is
  removed and the previous actor re-enabled and started — **failed, restored** — so a machine
  never loses its recovery authority to a bad release.
- **The control plane** is restored automatically when the new container **never passed a health
  check and the release does not migrate the database**. That widens the original rule, which
  covered only a container that never *started*: on an unchanged schema the previous control plane
  runs against exactly the database the failed one saw, so restarting it loses nothing, and the
  case it rescues — a broken image leaving a household install with no console — is the one
  that left an install dead before. **A migrating control plane is never restored automatically**,
  however early it failed: its migration may have run. On a Quasar-owned database that attempt
  ends `failed`, names the pre-update dump the recovery actor took, and prints one `restore`
  command that loads the dump into a stopped database and starts the control plane the dump
  belongs to. On an operator-supplied (external) database there is no dump: the attempt names
  none, Quasar never restores that database, and the way back is the operator's own backup,
  confirmed before the update. ADR 0002 holds: an older control plane never runs against a newer
  schema.
- **"Passed a health check" is defined per recipe**: the container's own healthcheck reported
  healthy at least once (for the control plane, its `/health`, which also reaches the database),
  and for the node agent additionally its `register` carrying the expected commit.
- **One service per failure.** When one attempt replaces several components in order (the recovery
  actor first, ADR 0008), a failure restores the component that failed; components already
  verified stay on their new digests, and the `auto_revert` row names only the restored component.
- **An interrupted attempt is `failed` with reason `interrupted`: nothing changed, and it is not
  restored.** That applies to an attempt interrupted before the old container was taken out of
  service; one interrupted after that continues to verification on the next start and is restored
  under the rules above if verification fails. Nothing is ever retried on its own.
  *(Clarification, #362.)* In a request naming several components, `interrupted` describes the
  component being replaced when the restart came: that component was not changed and later ones
  were never touched, while components earlier in the list that were already replaced and
  verified stay on their new digests (the bullet above), and `output` names them. For the
  recovery actor's own component, its old container counts as taken out of service once the
  running actor has released the machine's lease to its successor. This clarification is
  recorded in `protocol/agent-api.md` §`release_state` and `protocol/control-api.md` §"Failure
  reasons"; it was made under the RH06-01 process with the coordinator's authorisation and is
  flagged for the owner.
- A `registry` machine keeps the updater's behaviour described above until the Go updater retires
  with RH06-15 (#367).

Considered and rejected for the control plane: restoring automatically after a migration (a
schema moved forward cannot be undone by restarting an older binary, and D7's automatic
database restore was cut by the owner's R1 review, both in `docs/rh06/2026-09-24-decisions.md`),
and never restoring it automatically at all (the pre-RH06 gap this amendment closes). The wire and
storage consequences are `protocol/agent-api.md` and `protocol/control-api.md` amendment 14.

This amendment was approved through the #353 contract amendment: an Opus contract review
returned APPROVED on round 4, and the owner's standing approval on #352, widened on #353, is the
sign-off.
