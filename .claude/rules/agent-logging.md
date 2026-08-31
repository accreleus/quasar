---
paths:
  - "node-agent/**"
---

# Node-agent logging: session spans and the WARN/ERROR token

Two rules. Both are enforced — the first by construction, the second by
`node-agent/tests/log_convention.rs`, which walks `node-agent/src/**/*.rs` at
test time and fails the build on a violation.

## 1. Session lines carry `session=<id>`

The session runner thread enters `session{id=<session_id> host=<node_name>}`
once at the top of `session::runner::run_blocking`
(`crate::logging::session_span`). Every line emitted on that thread carries both
fields with no work at the call site — do **not** re-add a `session_id = %…`
field to code that runs under it.

Threads spawned from the runner re-enter a clone:

```rust
let log_span = tracing::Span::current();
std::thread::spawn(move || {
    let _log_span = log_span.enter();
    …
});
```

Two places are outside the span on purpose and keep an explicit
`session_id = %session_id` field:

- **GStreamer streaming threads.** Pad probes and signal callbacks run on
  threads GStreamer created; a thread-local span cannot reach them.
- **The agent's control loop** (`agent.rs`). It handles every session, so it has
  no single one to name.

Spans are thread-local, so never hold a span guard across an `.await` — in an
async task use `tracing::Instrument` instead.

## 2. Every WARN and ERROR carries `token = "<area>-<condition>"`

```rust
warn!(token = "encoder-stall", "no encoder output for {since_ms} ms (reason={reason})");
error!(target: T, token = "drvvol-provision-failed", error = %msg, "…");
```

- `token` is the **first** field (after `target:` when one is present).
- kebab-case, `<area>-<condition>`: `abr-rung-retired`, `gc-volume-rm-failed`,
  `xid-visibility-unavailable`. Name the condition, not the sentence — the prose
  is expected to be rewritten, the token is not.
- One token names one condition. Two sites may share a token only when they mean
  literally the same thing (the same failure reached from two match arms, or a
  fact discovered both by the session runner and by the standalone session
  server); list it in `SHARED_TOKENS` in the test.
- There is no exemption list worth adding to. `ALLOWED_UNTOKENED` exists so a
  genuine exception has a home, and a third test asserts it is empty — an
  exemption is a hole in the grep surface.

INFO and DEBUG do not need a token. Give one anyway to a line an operator is
likely to be told to grep for.

## Levels

| Level | Means |
|---|---|
| `error!` | The session or the host **failed, or will fail**. Something a person has to deal with. |
| `warn!` | Degraded but continuing, **and an operator should care**: a knob ignored, a capability missing, an image left on disk, a fallback taken. |
| `info!` | Lifecycle and resolved configuration — what happened, in the normal course of things. |
| `debug!` | Chatter: routine skips, per-pass deferrals, probe misses that the caller already handles by design. |

The test enforces the token, not the level. The level is a judgement, and the
one to keep asking is: *would an operator do something about this line?* If the
answer is no because the code already handles it — a property that differs
across encoder generations, a GC pass deferring a volume that is in use — it is
`debug!`, and demoting it makes the remaining warnings mean something again.

## Grepping

```
make session-logs SID=<session-id> HOST=<role>                        # one session
make session-logs SID=<session-id> HOST=<role> GREP=token=encoder-stall
```

`SID` matches all four shapes the id appears in (the text-format span prefix,
the json span object, an explicit `session_id=` field, and prose); `GREP` is a
free-text extended regex applied on top.

Fleet-wide, without a session in hand:

```
docker logs node-agent 2>&1 | grep -o 'token="[a-z0-9-]*"' | sort | uniq -c | sort -rn
```

## `QUASAR_LOG_FORMAT`

`text` (default) is the human format; a session's lines are prefixed
`session{id=… host=…}:`. `json` emits one object per line, event fields
flattened to the top level and open spans under `spans` — for a shipper, not for
reading. An unrecognised value warns
(`token="log-format-unrecognised"`) and falls back to text. Both go to stderr;
stdout belongs to machine-readable subcommands (`probe-encoder --json`).
Documented in `docs/configuration.md`.
