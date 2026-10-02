# Control-socket fixtures

The request and status shapes of the recovery actor's control socket
(`docs/rh06/2026-09-24-architecture.md` §5.2), pinned so the control plane (Go) and the
actor (Rust) cannot drift (#356). Each file is `{"shape", "about", "body"}`, where
`shape` is one of `request`, `accepted`, `rejection`, `result`, `status`.

- Go: `control-plane/internal/actorsocket` (`make test-go`).
- Rust: `quasar_recovery::socket`, tested by
  `node-agent/crates/quasar-recovery/tests/socket_fixtures.rs` (`make test-rust`).

Both sides decode every body into their type and re-encode it; the result must be the
same JSON value as the fixture (compared as parsed values, so key order and
whitespace do not matter, but a missing, extra or renamed field, or a different
omit-versus-`null` choice, does). Both also check that the fixtures cover every shape,
every request kind, every state, every reason, and a failure both restored and not.
An unknown shape fails both.

Rules the fixtures pin:

- `request.wait_timeout_s` and `rejection.request_id` are omitted when zero/empty;
  every other field is always present, with `null` where a value is absent.
- `status.result` is the attempt `GET /v1/status?request_id=<id>` names (`null` when the
  machine has no journal for it, or it was submitted on the other socket), or the most
  recent attempt submitted on the asking socket when no id is given; the
  agent's relay reads the second on connect to replay or adopt it. `POST /v1/submit`
  answers `202` with an `accepted`, `409` with a `busy` `rejection`, `400` with any other.
- `result` keeps `release_state`'s spellings (as the retired Go updater's result file
  did, without its `commands`), so the agent's
  `release_state` relay stays a re-frame. `reason` is set exactly when `state` is
  `failed`; `finished_at` exactly when the state is terminal.
- An interrupted attempt is `state: failed`, `reason: interrupted`, `restored: false`.
- The actor refuses a request carrying a field it does not know; readers of what the
  actor writes ignore unknown fields, because the actor moves first and may be one
  release ahead of the control plane or the agent. A reason a reader does not know is
  kept and rendered verbatim.

**This socket is not a frozen interface.** The reason vocabulary is agent-api.md
`release_state`'s closed set as the actor emits it, followed by the RH-06 identifiers
`recipe_unsupported`, `owner_conflict`, `backup_failed`, `backup_unconfirmed` and
`interrupted`, spelled as in the RH06-01 draft amendment (#353). Those five, and the
request kinds and status fields beyond today's updater, follow that draft pending its
sign-off; the actor slices may extend the shapes, changing both types and these
fixtures together. Nothing here is part of `protocol/`.
