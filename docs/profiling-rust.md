# Profiling the node-agent (Rust)

PROF-02 (#389). The reusable half of this ticket is the recipe below, not the Cargo
stanza. D-3 (#393, hot-path flamegraph) and D-4 (#394, async stalls) consume it.

---

## Why a separate build exists

`[profile.release]` sets no `debug` key, so the shipped agent carries no DWARF. Symbols
survive (`strip` is not set), so `perf` and `samply` already resolve function **names**
off the shipped binary. What they cannot do is give line numbers — and, the part that
actually misleads, **an inlined frame is attributed to whatever it was inlined into.**
With `codegen-units = 1` plus thin LTO the inlining is aggressive.

That is not a theoretical concern. Measured on a live 1080p60 Tower session
(2026-07-31, 25 s, 999 Hz): of the 106 sampled addresses inside the agent, **89 were
inlined frames**, one of them collapsing **18 source frames into a single machine
address**. A flamegraph off the release binary attributes that address to the outermost
inlinee — `std::sys::alloc::unix::…::alloc` — when the frame that actually called it is
`session/pipeline/webrtc.rs:715`. Same addresses resolved against the release binary:
**0** map to a node-agent source file and 49 return `??`.

So: profile the `profiling` build, or do not trust the flamegraph.

```toml
# node-agent/Cargo.toml
[profile.profiling]
inherits = "release"   # same opt-level / LTO / codegen-units — the shipped SHAPE
debug = 1              # line tables only; changes no codegen
strip = false
```

`[profile.release]` is deliberately untouched: line tables take the agent binary from
6.9 MB to 33 MB, and `deploy/image-contract.json` asserts image properties. The contract
is never relaxed to make a build green (CLAUDE.md), so the profiling build is a
**separate, non-promoted image variant** instead.

---

## 1. Build the image

```bash
deploy/build-images.sh profiling
```

Never a hand-typed `docker build`. The script prints the dated tag and warns, correctly,
that it promoted nothing:

```
BUILT  profiling  quasar-profiling:20260730-2330  9699MB  413s
WARNING: role 'profiling' is a non-shipping diagnostic image: no contract check, no :latest, no push.
```

There is **no `quasar-profiling:latest`, ever** — the image is not a shipped artifact
and must not be reachable by a tag that looks like one. `--push` with this role in the
list is refused outright rather than silently skipped.

The build asserts its own point: it fails unless the profiling binary contains
`.debug_line` **and** the release binary inherited from the `build` stage does not.

## 2. Point the stack at it

```bash
cd deploy
export QUASAR_PROFILING_IMAGE=quasar-profiling:20260730-2330

# Tower (NVENC / Vulkan)
docker compose -f docker-compose.yml -f docker-compose.nvidia.yml \
               -f docker-compose.profiling.yml up -d --force-recreate quasar-node-agent

# hermes (AMD / VA)
docker compose -f docker-compose.yml -f docker-compose.profiling.yml \
               up -d --force-recreate quasar-node-agent
```

`docker-compose.profiling.yml` adds the two things `perf_event_open` needs, and the
important one is **not** the capability:

- `security_opt: [seccomp:unconfined]` — Docker's **default seccomp profile denies
  `perf_event_open` outright**, at the syscall filter, before any capability check runs.
  Tower's agent already holds `CAP_SYS_ADMIN` and every profiler in it still fails with
  `EPERM` without this. Capabilities cannot buy their way past a seccomp deny.
- `cap_add: [PERFMON]` — satisfies the kernel's `perf_event_paranoid` gate, because
  `perfmon_capable()` short-circuits it. With PERFMON the host sysctl stops mattering:
  Tower sits at `perf_event_paranoid = 2` and needed no change.

Confirm the swap took, and that the image really is a drop-in agent:

```bash
docker inspect deploy-quasar-node-agent-1 \
  --format 'image={{.Config.Image}} caps={{.HostConfig.CapAdd}} secopt={{.HostConfig.SecurityOpt}}'
docker logs --tail 5 deploy-quasar-node-agent-1     # expect "capacity report sent"
```

## 3. Capture during a live session

Start the session first and let it reach `running` — a capture taken against an idle
agent is a profile of nothing, and it looks exactly like a successful one:

```bash
.claude/skills/quasar-session/scripts/qses run --stack=tower --app 'Steam' \
    --profile 1080p60 --secs 200 &
# poll until state=running before capturing
```

Then, inside the agent container:

```bash
PID=$(pgrep -f '^/usr/local/bin/quasar-node-agent$' | head -1)
timeout -s INT 25 samply record -p "$PID" -r 999 \
    --save-only --unstable-presymbolicate -o /tmp/cap.json.gz
```

Three flags with non-obvious behaviour, each of which cost a wasted capture:

| Flag | Why |
|---|---|
| `timeout -s INT N` | **`samply --duration/-d` is ignored when attaching with `-p`.** It prints "Recording … until Ctrl+C" and records forever. `timeout -s INT` is what actually bounds an attach capture. |
| `--save-only` | Otherwise samply starts a web server and tries to open a browser, which is not what a headless box wants. |
| `--unstable-presymbolicate` | Without it the saved profile stores **raw addresses only** — symbolication is deferred to load time and needs the binary present. The sidecar makes the capture readable off-host. Note the sidecar is named `cap.json.syms.json` (samply strips only `.gz`), which is easy to miss when checking the capture worked. |

Copy both files out:

```bash
docker cp deploy-quasar-node-agent-1:/tmp/cap.json.gz .
docker cp deploy-quasar-node-agent-1:/tmp/cap.json.syms.json .
```

## 4. Read it

Open <https://profiler.firefox.com> and load `cap.json.gz` (it is a local file load — the
profile is not uploaded anywhere), or serve it from the box with `samply load cap.json.gz`.

## 5. Verify the capture is worth trusting

Do this **before** drawing any conclusion from a flamegraph. It takes a minute and it is
the difference between a finding and a guess.

Pull the sampled agent addresses out of the sidecar and resolve them against both
binaries, in the container:

```bash
addr2line -f -i -C -e /usr/local/bin/quasar-node-agent -a @/tmp/agent-rvas.txt  # profiling
addr2line -f -i -C -e /opt/agent/quasar-node-agent     -a @/tmp/agent-rvas.txt  # release
```

Healthy result (the 2026-07-31 Tower run):

| | profiling binary | release binary |
|---|---|---|
| sampled addresses resolving to a `node-agent/src` file | 57 / 106 | **0** |
| sampled addresses with no line info (`??`) | **0** | 49 |
| addresses that are an inlined frame (chain depth > 1) | 89 / 106 | not visible at all |

If the profiling column looks like the release column, the profile stanza did not apply
— check that the container is actually running the profiling image and not a bind-mounted
release binary.

## 6. Put the host back

The profiling image is unvalidated by `image-contract.json` by design. **Do not leave a
host on it.**

```bash
cd deploy
docker compose -f docker-compose.yml -f docker-compose.nvidia.yml \
               up -d --force-recreate quasar-node-agent
```

---

## Fallback: a static SVG for a ticket attachment

`cargo-flamegraph` when a flamegraph needs to be pasted into an issue rather than
explored. Same `perf_event_open` prerequisites; `perf` is in the image.

## Things that will waste your afternoon

- **A capture with no session running.** The agent is nearly idle between sessions, the
  capture still succeeds, and the flamegraph is all runtime bookkeeping. Poll for
  `state=running` first.
- **`home_in_use` (HTTP 409) on launch.** A previous session still holds the app's
  storage. Stop it (`qses stop --stack=tower <SID>`) before relaunching.
- **Bench apps may not be enabled.** On Tower the reliable launch is `--app 'Steam'`;
  `GOW XFCE Desktop` exited with code 1 during the PROF-02 run. Check the catalog rather
  than assuming a bench tile exists.
- **An un-spawned future is invisible to tokio-console, not slow in it (#532).** Tokio
  emits a `runtime.spawn` tracing span only for **spawned** tasks — the
  `#[tokio::main]` main future gets none. The node-agent's control loop
  (`agent::run` → `connect_and_run`: heartbeats, capacity, readiness, signalling
  relay, session lifecycle) used to run as that main future, so the first D-4 run
  (#394) reported three over-threshold sites — all of them `kind=blocking`, correct
  by construction — and read as a clean bill of health while the agent was burning
  a full core (#530) and stalling 1311 ms per poll (#531). `main.rs` now
  `tokio::spawn`s the control loop and awaits its `JoinHandle` (`spawn_agent`), so
  it is a first-class task in every future capture. If you point a poll-time
  instrument at some *other* long-lived future and it reports nothing, check
  whether that future is spawned before believing the result.
- **Disk.** The profiling image is ~9.7 GB and `FROM dev`, and any `node-agent/` change
  invalidates the `build`→`dev` chain, so a rebuild is not incremental. `build-images.sh`
  refuses to start below 20 GB free on the docker root, which is the correct behaviour —
  free space rather than lowering the floor.

---

# Heap attribution for a per-session leak (#419)

Everything above profiles **CPU time**. This section is the memory analogue, written for
issue #419: the agent grows ~1–4 MB of RSS per session cycle on every codec (h264 +4.2,
h265 +3.4, av1 +1.2 MB/cycle over 340+ cycles, no sign of a plateau) with **zero**
correlated growth in fds, threads, live `GstObject`s or VRAM.

That last clause is the whole problem. The GstObject leaks tracer — the tool this repo
reaches for first (`.claude/rules/gstreamer-gotchas.md`) — is **blind** here by
construction: it counts `GstObject`s, and none are leaking.

One caveat before concluding "not GStreamer": `leaks(filters=GstObject)` tracks
**GstObjects only**. `GstCaps`, `GstStructure`, `GstBuffer`, `GstMemory` and `GstEvent`
are `GstMiniObject`s and are **not** in that filter's population, so a per-frame caps or
buffer leak produces exactly the observed evidence — RSS up, "gst objects" flat. Before
spending a night on heaptrack, re-run the tracer **unfiltered** (`GST_TRACERS='leaks'`)
and diff the `object-alive` census across cycles. Mini-object leaks have precedent in
this tree (the gwd CUDA-pool patch also fixed an over-unref of borrowed caps).

What is left after that is plain heap,
and plain heap has two shapes that look identical from outside the process:

1. **Allocator retention.** The memory is `free()`d, the program cannot reach it, but
   glibc never returns it to the kernel so RSS only ratchets up. The agent runs 50+
   threads; glibc gives a contending thread its own arena (up to 8 × ncores), recycles
   arenas when threads exit but never unmaps them, and each keeps its own high-water mark.
2. **Real retention.** Something is still reachable — a Rust allocation, or a C allocation
   owned by a non-`GstObject`, that outlives the session.

**These need different tools, and the expensive tool only answers question 2.** Establish
which shape you have before reaching for a heap profiler; running heaptrack for 8 hours to
discover the memory was reclaimable free heap is a wasted night.

## Step 0 — free: read it off the agent log

`node-agent/src/memstat.rs` samples `/proc/self/statm` at every session start and every
session teardown and logs under target `quasar.mem`. No tooling, no overhead, always on:

```bash
docker logs deploy-quasar-node-agent-1 2>&1 | grep quasar.mem
# session <id>: start rss_kib=…
# session <id>: stop  rss_kib=…
```

The per-cycle retention number is the delta between **successive `start` samples** (a
`start`→`stop` delta inside one cycle also contains the session's own live footprint).
The startup line records the allocator environment, so a capture always states its arm.

## Step 1 — cheap and decisive: is it reclaimable?

Two independent probes; run both, they should agree.

**(a) The `malloc_trim` A/B.** `QUASAR_MALLOC_TRIM=1` makes the agent call
`malloc_trim(0)` at each session teardown, which walks every arena and hands free pages
back to the kernel. It logs RSS either side of the call.

```bash
cd deploy
QUASAR_MALLOC_TRIM=1 docker compose -f docker-compose.yml -f docker-compose.nvidia.yml \
    up -d --force-recreate quasar-node-agent
```

**(b) `malloc_info`, no rebuild, no restart.** glibc's own per-arena accounting, dumped
out of the live process:

```bash
docker exec deploy-quasar-node-agent-1 sh -c '
  PID=$(pgrep -f "^/usr/local/bin/quasar-node-agent$" | head -1)
  gdb -p "$PID" -batch -ex "call (int)malloc_info(0, fopen(\"/tmp/mi.xml\",\"w\"))" \
                       -ex "call (int)fflush(0)"'
docker cp deploy-quasar-node-agent-1:/tmp/mi.xml .
```

Sample it early and late in the soak and compare the `<system type="current">` total
against the summed `<total type="rest">` (free) across arenas. Needs `gdb` and
`CAP_SYS_PTRACE` (`docker-compose.profiling.yml` already runs `seccomp:unconfined`; add
`cap_add: [SYS_PTRACE]` for the capture if the attach is refused).

Read the result:

| Observation | Conclusion | Next step |
|---|---|---|
| Trim flattens the per-cycle slope; `malloc_info` free bytes track the RSS growth | **Allocator retention** — the memory is free, glibc is hoarding it | Stop. Fix is a knob: `MALLOC_ARENA_MAX` / `MALLOC_TRIM_THRESHOLD_`, or the per-teardown trim. No heap profiler needed. |
| Trim releases little; free bytes stay flat while RSS climbs | **Real retention** | Step 2. |
| Trim releases some but the slope persists | Both, layered | Knob the reclaimable part, then Step 2 for the residual. |

The glibc arm is a compose passthrough on both stacks (`MALLOC_ARENA_MAX`,
`MALLOC_TRIM_THRESHOLD_`, `MALLOC_MMAP_THRESHOLD_` in `docker-compose.yml` and
`docker-compose.nvidia.yml`), all unset by default — **no image rebuild to A/B it.** The
sharpest single arm is `MALLOC_ARENA_MAX=1`: it collapses the per-thread arenas to one,
so if the growth is arena high-water marks it disappears outright. It costs allocator
contention across 50+ threads, so it is a diagnostic, not a proposed default.

## Step 2 — allocation-site attribution: heaptrack, bounded window

**Recommendation: `heaptrack`, from process start, over ~4 cycles — not 8 hours.**

Why heaptrack over the alternatives, for this specific process:

- **heaptrack** — `LD_PRELOAD`s over `malloc`/`free`, so it sees **C and Rust equally**.
  That matters more than anything else here: the agent is mostly GStreamer/GLib, and the
  suspect allocation may never touch Rust's `GlobalAlloc`. Debian package (`heaptrack`,
  `heaptrack-gui` on a workstation), and it consumes exactly what `[profile.profiling]`
  already produces — line tables plus `-C force-frame-pointers=yes` for cheap, correct
  unwinding. Output is a compressed trace; `heaptrack_print --leaks` gives allocations
  still live at exit, grouped by backtrace, which is precisely the #419 question.
- **jemalloc + `MALLOC_CONF=prof:true`** — much lighter (sampled, ~1–2%) and it would be
  the obvious pick for an 8 h run, but two problems: Debian's `libjemalloc2` is **not
  reliably built with `--enable-prof`** (verify with
  `MALLOC_CONF=prof:true LD_PRELOAD=…/libjemalloc.so.2 /bin/true` — a build without prof
  aborts with "Invalid conf pair"), and swapping the allocator **changes the thing under
  test**: jemalloc has completely different arena and page-return behaviour, so it can
  make a glibc fragmentation problem vanish and teach you nothing. Fine as a
  confirmation ("does the growth follow the allocator?"), wrong as the primary tool.
- **valgrind massif / memcheck** — 20–50× slowdown. A 60 fps pipeline will not run; the
  workload stops being the workload. Do not.
- **bytehound** — comparable to heaptrack, no distro package, less maintained. No reason
  to prefer it here.

### Recipe (proposed — not yet executed)

Heaptrack must wrap the process from `main`, and attaching to a running process needs a
gdb injection that is fragile in a container. Override the compose `command:` instead so
the agent starts under it, and give the run a **bounded** session count:

```bash
cd deploy
export QUASAR_PROFILING_IMAGE=quasar-profiling:<dated-tag>

cat > /tmp/heaptrack.override.yml <<'YAML'
services:
  quasar-node-agent:
    entrypoint: ["/bin/bash", "-lc"]
    command:
      - |
        exec heaptrack -o /tmp/agent.heaptrack /usr/local/bin/quasar-node-agent
    environment:
      # Frame pointers are already on in the profiling build; this keeps glibc from
      # hiding the arena behaviour behind a moving trim threshold during the capture.
      MALLOC_TRIM_THRESHOLD_: "131072"
YAML

docker compose -f docker-compose.yml -f docker-compose.nvidia.yml \
               -f docker-compose.profiling.yml -f /tmp/heaptrack.override.yml \
               up -d --force-recreate quasar-node-agent
```

Then drive **4 full launch → stream → stop cycles** with the normal harness, stop the
container cleanly (`docker stop`, i.e. SIGTERM — heaptrack finalises its trace on exit;
`docker kill` loses it), and pull the trace:

```bash
docker cp deploy-quasar-node-agent-1:/tmp/agent.heaptrack.zst .
heaptrack_print --leaks --print-leaks agent.heaptrack.zst | head -80
```

Read the **leaks** view (allocations live at exit), not the peak view — a per-session leak
across 4 cycles shows up as a backtrace whose live allocation count is ~4× the per-cycle
count, which is the fingerprint to look for. Cross-check against the `quasar.mem` RSS
series from the same run: a candidate backtrace has to account for MB, not KB.

Costs and caveats, so nobody is surprised:

- Every `malloc` gets a backtrace. Expect materially higher CPU and a trace on the order
  of hundreds of MB to a few GB for 4 cycles. **Watch disk** — the profiling image is
  already ~9.7 GB and `build-images.sh` wants 20 GB free.
- The session may miss its frame deadline under instrumentation. That is acceptable for a
  leak hunt (the allocation *pattern* is what is being measured) but it means smoothness
  numbers from a heaptrack run are meaningless. Do not mix the two goals in one run.
- Same rule as the CPU capture: **do not leave a host on the profiling image**, and
  certainly not on the heaptrack override. Put the stack back on its normal chain when
  done (section 6 above).

### What the 8 h soak should carry instead

An 8 h/20-cycle soak wants the cheap instrumentation, not heaptrack:

- `quasar.mem` RSS series (free, always on) — the per-cycle slope, per codec.
- `QUASAR_MALLOC_TRIM=1` on one arm — the reclaimable/unreclaimable split.
- `MALLOC_ARENA_MAX=1` on another arm — arena high-water marks in or out.
- `/proc/PID/smaps_rollup` sampled per cycle alongside RSS, so an `Rss` rise can be
  attributed to `[heap]`/anon vs file-backed mappings.

heaptrack is then aimed at whatever the soak has already narrowed to, in a short bounded
run the next day.
