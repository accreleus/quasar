# Profiling the node-agent for async stalls (tokio-console)

D-4 (#394), prep work only — build recipe + capture/teardown procedure. This is the
counterpart to `docs/profiling-rust.md` (PROF-02, D-3): a CPU flamegraph shows a busy
thread; `tokio-console` shows a task that is *not* burning CPU but is holding a runtime
worker hostage while it polls for too long (the classic blocking-call-inside-async
defect). Given `tokio = { features = ["full"] }` and 85 lock/blocking sites in
`node-agent/src/`, this is a real prior.

**Scope note (CLAUDE.md / issue #394):** this instrumentation is diagnostic-only. It
must **not** land in `develop` unless a specific finding justifies carrying it. Apply
the patch on a disposable branch, capture, revert, done.

---

## 0. What the patch does

The patch — git-apply-able against `develop` — adds a new cargo feature `tokio-console` to `node-agent/Cargo.toml`, feature-gated so
it is inert when off (no dependency compiled, no behavior change):

```toml
console-subscriber = { version = "0.4", optional = true }

[features]
tokio-console = ["dep:console-subscriber", "tokio/tracing"]
```

`tokio/tracing` is feature-unification on the **already-declared** `tokio` dependency —
no second `[dependencies]` entry, no risk of two conflicting tokio requirements.
`tokio`'s `"full"` feature bundle does **not** include `"tracing"`; without it,
tokio's internal task/resource instrumentation is off and console-subscriber's client
connects to a server that reports zero tasks. `--cfg tokio_unstable` alone is not
enough — both the cfg and the feature are required.

`node-agent/src/main.rs` gets an `init_tracing()` split into two `#[cfg]` arms so a
second, independent `tracing_subscriber::fmt().init()` can't run alongside
console-subscriber's layer (that panics — "global default trace dispatcher already
set"). Feature on: compose `console_subscriber::spawn()` and the existing fmt layer
through `tracing_subscriber::registry()`. Feature off: byte-identical to the pre-patch
code path.

console-subscriber's gRPC server binds its default `127.0.0.1:6669` inside the agent
process — same loopback-only posture as the PROF-01 pprof listener
(`127.0.0.1:6060` in the control plane). Access section below.

---

## 1. Apply the patch and confirm both build states

```bash
git checkout -b prof/d4-console-scratch develop   # THROWAWAY — never push this branch

# The patch is not carried in the tree (it was a one-off diagnostic fixture, removed
# 2026-08-27 when deploy/ became the operator front door). Recover it from history:
git show 92dc4446:deploy/test-fixtures/d4-tokio-console.patch > /tmp/d4-tokio-console.patch

git apply /tmp/d4-tokio-console.patch
git apply --check /tmp/d4-tokio-console.patch   # sanity: already-applied errors are expected here
```

Verified 2026-07-31 on Tower (`quasar-agent-dev:latest`, `docker run --rm -v <scratch>:/src
-w /src/node-agent <image> cargo check ...`):

| build | command | result |
|---|---|---|
| feature **on** | `RUSTFLAGS="--cfg tokio_unstable -C force-frame-pointers=yes" cargo check --features cuda,tokio-console` | clean — `console-subscriber v0.4.1` resolves against `tokio v1.52.3` / `tracing v0.1.44` / `tracing-subscriber v0.3.23`, no version conflicts |
| feature **off** (patch applied, flag not passed) | `RUSTFLAGS="-C force-frame-pointers=yes" cargo check --features cuda` | clean — `console-subscriber` does not even compile (confirms the feature gate, not just "it happens to build") |

No dependency-resolution surprises: console-subscriber 0.4.x's own `tokio`/`tracing`
version bounds are already inside what the agent pins.

`cargo check`, not `cargo build` — this recipe only proves the instrumented build
compiles. Building the real binary (step 2) needs the container's linker/toolchain the
same as any other node-agent build (`scripts/dev/dev.sh build node-agent` normally); nothing
about the patch changes that.

---

## 2. Build the instrumented binary and run it under the dev stack

The dev-stack `quasar-node-agent` service execs the **workspace** build
(`deploy/docker-compose.yml`, `command:` → `exec
/workspace/node-agent/target/release/quasar-node-agent`) rather than the baked image
binary — that's the existing "no rebuild-the-image" override, reused here unchanged:

```bash
# on the box (Tower/hermes), from the repo root, on the throwaway branch:
docker run --rm -v "$PWD":/workspace -w /workspace/node-agent \
  -e RUSTFLAGS="--cfg tokio_unstable -C force-frame-pointers=yes" \
  quasar-agent-dev:latest cargo build --release --features cuda,tokio-console
  # drop `,cuda` on hermes (AMD/VA)

cd deploy
docker compose -f docker-compose.yml -f docker-compose.nvidia.yml \
  up -d --force-recreate quasar-node-agent   # Tower; drop the -vulkan overlay for hermes plain
```

No compose file changes needed — the existing `command:` always execs the workspace
binary, so building it with the feature on is the entire "deploy" step. Confirm the
console listener is actually up before driving a session:

```bash
docker logs --tail 20 deploy-quasar-node-agent-1 | grep -i console
# console-subscriber prints its own startup line, e.g.:
#   "-- Wait, there's more! --" listening on 127.0.0.1:6669 ... (may vary by version)
```

---

## 3. Drive it under live streaming load

Per the ticket: **a stalled task under idle conditions proves nothing.** Launch a real
session and let it reach `state=running` before capturing, on both Tower (NVENC) and
hermes (VA) — see `docs/profiling-rust.md` §3 for the same launch pattern
(`qses run --stack=... --app 'Steam' --profile 1080p60 --secs N`). Capture for the
duration of the session, not a snapshot.

---

## 4. Access the console client (loopback-in-container — no port publishing)

`docker exec <agent> tokio-console` does **not** work: `tokio-console` is a persistent
gRPC-streaming TUI client, not a one-shot binary, and it is not in any node-agent image
(the pprof-equivalent `docker exec … wget -qO- 127.0.0.1:6060/...` pattern only works
because that's a single stateless HTTP GET — `tokio-console` needs an interactive TTY
held open against a long-lived stream, which `docker exec` *can* give you, but the
binary itself is simply absent from the image and must not be added to a shipping
image for a diagnostic tool).

`ssh -L 6669:127.0.0.1:6669` from the Mac does not reach it either: that forwards to
127.0.0.1 **on the box**, but the listener's `127.0.0.1` is inside the agent
container's own network namespace — under `network_mode: host` the container shares
the box's netns, so this actually *would* resolve... except we deliberately do not
want a workflow that only works for `network_mode: host` services and silently breaks
for anything namespaced later. Treat the agent's loopback as private per the pprof
precedent (**no port publishing, ever** — CLAUDE.md/spec posture) and reach it by
**joining the agent's network namespace with a second container**:

```bash
docker run --rm -it \
  --network container:deploy-quasar-node-agent-1 \
  -v quasar-tokio-console-cargo:/root/.cargo/registry \
  quasar-agent-dev:latest \
  bash -c 'cargo install --locked tokio-console >/tmp/install.log 2>&1 || tail -50 /tmp/install.log; tokio-console http://127.0.0.1:6669'
```

Why this is the chosen approach over the alternatives:

- **`docker exec` into the agent container** — rejected: the client binary isn't
  there, and it must not be baked into a shipping image for a diagnostic-only tool
  (same principle as the profiling image being a separate, non-promoted variant in
  `docs/profiling-rust.md`).
- **Publish port 6669 in a compose override** — rejected: violates the standing
  posture set by PROF-01 for exactly this class of listener (`docker exec … wget` for
  pprof; "do not publish port 6060 in any compose file"). A gRPC debug listener with
  no auth reachable from the LAN is a bigger footgun than pprof's plain HTTP GET.
- **`docker run --network container:<agent>`** — chosen: joins the *network*
  namespace only (not filesystem/PID/IPC), so `127.0.0.1:6669` inside the new
  container **is** the agent's loopback, with zero host port exposure and zero
  changes to the agent image or compose files. `quasar-agent-dev:latest` already carries a
  full Rust toolchain and already has network egress (crates.io) in this environment,
  so `cargo install tokio-console` needs nothing new provisioned. The named volume
  caches the crates.io download/build across repeat sessions in one investigation.

`tokio-console` is a full-screen TUI (like `htop`); `-it` is required.

---

## 5. What to look for

- **Tasks view, `Total Dur` / `Busy` / `Idle` columns**: a task whose `Busy` time is
  a large fraction of its `Total` while the agent is otherwise idle-looking in `top`
  is the signature — CPU is not the story, wall-clock poll time is.
- **Poll Times histogram per task**: `tokio-console` flags a task whose poll exceeds
  the runtime's configured stall/slow-poll threshold (default ~50-100ms class of
  warning, shown inline). The acceptance criterion in #394 is explicitly **every task
  whose poll time exceeds the runtime's stall threshold** — list every one, not just
  the worst offender.
- **Resources view**: mutexes/semaphores with long **wait** times point at contention
  on a lock held across an `.await`-adjacent blocking call, not the lock primitive
  itself. Cross-reference the 85 lock/blocking sites in `node-agent/src/` (`grep -rn
  '\.lock()\|blocking_lock\|std::fs::\|reqwest::blocking\|ureq::' node-agent/src/`) —
  a long-poll task usually resolves to one of these calling something synchronous
  (disk I/O, a blocking HTTP client, a held `std::sync::Mutex`) directly inside an
  async fn instead of `spawn_blocking`.
- Every finding gets filed as its own issue with the poll-time evidence (a
  `tokio-console` screenshot/paste, task name, source location) attached, per the
  ticket's proposed-test note: the fix is normally `spawn_blocking` or an async
  equivalent, and the test is a task-poll-duration assertion or a latency assertion
  that fails today.
- A clean run (nothing over threshold under real load, on both hosts) is itself the
  deliverable if that's what's found — file nothing, write it up as "checked, clear."

---

## 6. Teardown / restore

The instrumented binary must not stay deployed:

```bash
cd deploy
docker run --rm -v "$PWD/..":/workspace -w /workspace/node-agent \
  -e RUSTFLAGS="-C force-frame-pointers=yes" \
  quasar-agent-dev:latest cargo build --release --features cuda   # rebuild WITHOUT tokio-console
docker compose -f docker-compose.yml -f docker-compose.nvidia.yml \
  up -d --force-recreate quasar-node-agent
```

Then, off the box:

```bash
git checkout develop
git branch -D prof/d4-console-scratch   # the throwaway branch, never pushed
```

Remove the `quasar-tokio-console-cargo` volume if this was a one-off investigation
(`docker volume rm quasar-tokio-console-cargo`); leave it if more D-4 sessions are
planned soon (rebuilding `tokio-console` from scratch each time is the only cost of
removing it early).

The patch itself is not carried in the tree — recover it from `92dc4446` (§1) and reapply
it fresh for the next investigation rather than keeping a long-lived branch around.
