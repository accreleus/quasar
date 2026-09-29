# Agent-socket console fixtures

The console-mode shapes the recovery actor serves on the node agent's socket only
(RH-07 #395): `GET /v1/console` and `POST /v1/console`. Each file is
`{"shape", "about", "body"}`, where `shape` is one of `console_request`,
`console_status`, `rejection`.

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
  `restored`, when it started and finished), or `null`
  when the machine's last reconfigure was not a console change.
- `supported: false` comes with `why`; `why` is `null` otherwise.
- `POST /v1/console` answers `202` with a `console_status` once a replacement is
  admitted, `200` with one when nothing needed re-creating, `409` with a `busy`
  `rejection`, `400` with any other. A `console_request` carrying a field the actor does
  not know is refused.
- The control socket answers `404` on both routes.

**Not a frozen interface.** Nothing here is part of `protocol/`.
