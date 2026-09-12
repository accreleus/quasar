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
