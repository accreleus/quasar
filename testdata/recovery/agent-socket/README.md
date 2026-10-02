# Agent-socket console fixtures

The console-mode shapes the recovery actor serves on the node agent's socket only
(RH-07 #395, #407): `GET /v1/console`, `POST /v1/console` and
`POST /v1/console/preflight`. Each file is `{"shape", "about", "body"}`, where `shape` is
one of `console_request`, `console_status`, `console_preflight`, `rejection`.

- Rust: `quasar_recovery::console` (`rejection` is `quasar_recovery::socket::Rejection`),
  tested by `node-agent/crates/quasar-recovery/tests/socket_fixtures.rs` (`make test-rust`).

They live apart from `../socket/` because the control plane never speaks them: its
fixture test fails on a shape it does not know.

Each body is decoded into its type and re-encoded; the result must be the same JSON
value (compared as parsed values, so a missing, extra or renamed field, or a different
omit-versus-`null` choice, fails).

Rules the fixtures pin:

- Every `console_status` field is always present, with `null` where a value is absent.
- `enabled` is console mode as the verified agent runs it: a change is not in force
  until it settles. `in_flight` names the console change being applied until then;
  `in_flight_target` and `in_flight_started_at` are set exactly when it is.
- `last` is how the last console change settled (its `request_id`, `applied` or
  `put_back`, the attempt's failure `reason`, whether the previous agent was
  `restored`, when it started and finished, and `detail`), or `null`
  when the machine's last reconfigure was not a console change. The recovery actor's
  own re-creation of the agent for changed console devices (i2c nodes renumbered by a
  reboot, #407) is not a console change.
- `last.detail` is the console agent's own preflight text when a failed preflight is
  why the attempt failed (`reason` is then `unhealthy`), and `null` otherwise.
- `supported: false` comes with `why`; `why` is `null` otherwise.
- `POST /v1/console` answers `202` with a `console_status` once a replacement is
  admitted, `200` with one when nothing needed re-creating, `409` with a `busy`
  `rejection`, `400` with any other. A `console_request` carrying a field the actor does
  not know is refused.
- `POST /v1/console/preflight` (a `console_preflight`) is how an agent created with the
  console additions reports, when it starts and before it reports healthy, whether it
  can take the display. `ok: false` needs a one-line `detail` (at most 1024 bytes) an
  operator reads: what holds the display, or what host preparation lacks. `detail` is
  `null` or a note when `ok` is true. The actor answers `200` with the report as kept,
  `400` with an `invalid` `rejection`, or `409` with a `busy` one when it could not
  record it. While an attempt that turns console mode on is
  being verified, a report with `ok: false` fails it at once.
- The control socket answers `404` on every route here.

**Not a frozen interface.** Nothing here is part of `protocol/`.
