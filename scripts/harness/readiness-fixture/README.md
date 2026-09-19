# readiness-fixture

**Test-only.** This is a separate Go module (`quasar-readiness-fixture`) used by
`scripts/harness/run-readiness-faults.sh` (RH-02, #264). It is never built into
any deploy image: `control-plane/`, `node-agent/` and `web/` never import it,
no `deploy/` Dockerfile can `COPY`/`ADD` it in, and `scripts/verify/` asserts as
much on every `make verify` (see `docs/superpowers/plans/2026-09-19-rh02-264-harness-matrix.md`
"Why the synthetic check cannot reach production"). It runs in a `golang`
container the same way `scripts/harness/apitest/` does.

## `readiness-fixture relay`

```
readiness-fixture relay --listen ADDR --upstream WS_URL --control ADDR
```

- `--listen` (default `:8500`) — where the real node agent dials this relay,
  same path (`/agent/ws`) and framing the control plane would have accepted.
- `--upstream` (required) — the control plane's real `ws://.../agent/ws`.
- `--control` (default `127.0.0.1:8501`) — loopback HTTP control port.

Forwards every WS frame verbatim in both directions (text and binary, order
preserved, close propagated either way, repeated agent reconnects work). The
only mutation: an upstream (agent → control) **text** frame that is JSON with
`"type":"capacity"` and carries a `readiness` array has that array rewritten
per the current rule — every other byte of meaning, including unknown fields
on each check and every other top-level field, passes through untouched. A
capacity message with no `readiness` field, or any other message type, is
never touched, in any rule mode.

Rule (`GET`/`PUT /rule`, JSON):

```json
{"mode":"off"}
{"mode":"inject","check":{"id":"harness_synthetic_gate","status":"fail","summary":"...","remediation":"...","source":"host_probe","blocks":{"scope":"host","enforced_by":"control_plane"}}}
```

`inject` mode: removes any existing check whose `id` starts with
`harness_synthetic_`, then appends the rule's check with `observed_at` forced
to now (RFC3339 UTC) — the check `id` **must** start with `harness_synthetic_`
or `PUT /rule` returns `400`. A rule change takes effect on the agent's next
capacity report that carries `readiness`; the relay never synthesizes a
message on its own.

Control HTTP:
- `GET /healthz` → `200`
- `GET /rule` → the current rule
- `PUT /rule` → set the rule (`400` on an invalid one)
- `GET /stats` → `{"agent_connections":N,"capacity_seen":N,"capacity_rewritten":N,"last_capacity_at":"<RFC3339 or null>"}`

## `readiness-fixture host`

```
readiness-fixture host --control-plane WS_URL --node-name NAME --enrollment-token TOKEN \
  [--slots N=1] [--vram-mb N=8192] [--readiness-file PATH] [--report-interval 10s]
```

A scripted node agent standing in for a second host (protocol/agent-api.md
Transport/Auth/register/capacity/heartbeat/session_*). Connects, `register`s
with the enrollment token, prints exactly one line to stdout on success —
`registered host_id=<id>` — then reports one GPU (`vendor: "amd"`, the given
`encode_slots_total`/`vram_mb_total`, `codecs: ["h264"]`) with `readiness`
loaded from `--readiness-file` (a JSON array; re-read on every report, so the
harness can flip it mid-run; defaults to one passing check when omitted).
Re-sends capacity every `--report-interval` and heartbeats on the interval the
control plane assigns in `registered`, with `running_sessions` kept accurate.
Acks `session_assign`/`session_start`/`session_stop` and reports
`starting → running` / `stopping → stopped`; ignores any other downstream
message type without crashing. SIGTERM/SIGINT close cleanly, exit `0`. If the
control plane refuses enrollment because `node_name` is already live, the
process exits non-zero with a message naming that.

Persists nothing across runs.
