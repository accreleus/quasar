---
name: quasar-host
description: Use for any operation that must run on a Quasar host rather than this workstation — build/test loops (cargo, go, web) in the dev container, stack lifecycle (ps/up/down/logs/health/restart), full redeploys, drift checks between hosts, raw shell access, file staging, and GPU/encoder checks. Triggers — "run the tests", "does it compile", "go-test-db", "cargo build/clippy", "bring the stack up", "restart the control-plane", "tail the agent logs", "did the node-agent register", "redeploy", "is it green", "check the GPU", "nvidia-smi", "run this on the box", "shell into the host". Hosts are addressed by ROLE (gpu-test, aux-infra, deploy-only) from _shared/hosts.json. Pairs with quasar-session (live sessions), quasar-netem (impairment), quasar-image (image build/audit), quasar-ticket (per-ticket discipline).
arguments:
  - name: verb
    description: "qhost verb (hosts, info, notes, test, gofmt, web-check, sync-branch, ps, up, down, logs, health, registered, control, agent, web-deploy, redeploy, drift, multiup, multidown, multips, multiagent, sh, docker, cp, runsh, pyrun, gst, gpu, reauth)"
    required: true
  - name: "--host"
    description: "role or host name to act on (default: the gpu-test role)"
    required: false
  - name: args
    description: "arguments to the verb (dev.sh target, service name, git ref, command string, local file path)"
    required: false
---

# Operating a Quasar host

One skill, one script, any host:

```
.claude/skills/quasar-host/scripts/qhost [--host ROLE|NAME] <verb> [args]
```

`qhost --help` prints the full verb table. **Mechanics live in the script; this file
is for judgment** — which host, which verb, when to escalate, what not to do.

## Hosts are roles, not names
The operator's `_shared/hosts.json` binds roles to hosts and holds every
host-specific fact (address, user, key, working directory, GPU vendor, compose
files, platform quirks). **Nothing host-specific belongs in this skill or its
scripts.** Adding or replacing a box is a JSON edit; `_shared/hosts.example.json`
documents the schema.

| role | meaning | default? |
|---|---|---|
| `gpu-test` | primary GPU / streaming test host — where quality is judged | **yes**, every verb targets it unless `--host` says otherwise |
| `aux-infra` | sanctioned infrastructure: impairment shaping, headless browser peer, container build runner | only when a verb is about infrastructure |
| `deploy-only` | production / staging target | never implicitly |

Override the default role for a session with `QUASAR_DEFAULT_ROLE=<role>`.

`qhost hosts` lists what is configured. `qhost info` / `qhost notes` print a host's
resolved facts and the **operator notes** — read `notes` before doing anything
unusual on a box: that is where platform quirks live (volatile `~/.ssh` on some
appliances, working-directory rules, disk-space traps, driver requirements). Do not
assume a note from one host applies to another.

## Safety rules the script enforces — respect them, don't route around them
- A verb that **mutates** a `deploy-only` host requires an explicit `--host`; a
  **destructive** one (`down`, `multidown`) is refused there outright. If you find
  yourself wanting to bypass this, stop and ask the operator.
- Never target a role the operator has marked infrastructure-only for work that
  belongs on `gpu-test`. Check the host's `notes` — an "infrastructure only" note is
  a standing directive, not a preference.
- `sh` / `docker` / `runsh` are raw. Some hosts forbid scratch files outside their
  configured `dir` (see `notes`) — stage into `dir`, never `/tmp`, on those.

## Which verb
**Locally, prefer `make <target>`** (verify/test/lint/status against your own
checkout) — this skill covers the **remote host path**: the workstation has no
Rust, Go, GStreamer, Wayland, or Docker daemon, so anything that must actually
compile or run Quasar goes through a host.

| you want to… | verb |
|---|---|
| compile / test / lint a change | `qhost test <scripts/dev/dev.sh target…>` — pushes the branch, syncs the host, runs it in the container |
| Go formatting (not covered by go-check) | `qhost gofmt` |
| web typecheck + build + unit tests | `qhost web-check` |
| see stack state / logs / health | `qhost ps` · `qhost logs [service]` · `qhost health` · `qhost registered` |
| apply a merged Go change | `make redeploy-cp HOST=<role-or-host>` — `redeploy.sh <profile> <ref> control`: rebuilds only the Go image (~1 min), force-recreates it, waits for healthy (proves migrations ran), verifies. `qhost control` is the older in-place variant: it rebuilds the image but does **not** sync the ref, force-recreate, or verify |
| apply a node-agent change | `qhost agent` (rebuild + **force-recreate**) |
| apply a web change | `qhost web-deploy` |
| put a whole host on a ref | `qhost redeploy [ref]` — the canonical path; prefer it over hand-rolled piecemeal restarts |
| confirm two hosts match before a cross-host test | `qhost drift <A> <B>` |
| run something ad hoc / stage a script / probe the GPU | `qhost sh` · `qhost runsh` · `qhost pyrun` · `qhost gst` · `qhost gpu` |

## Rules that decide "done"
- **`go-check` does not run the DB tests** — they `t.Skip()` with no database
  wired, so a green `go-check` means they were *skipped*. A control-plane ticket
  touching the DB is done only on a green **`qhost test go-test-db`**.
- **`go-check` / `go-test-db` do not run `gofmt`.** Check formatting separately
  (`qhost gofmt`) and fix what your branch touched.
- **Rust done** = `qhost test check node-agent` clean (`cargo fmt --check` +
  `clippy -D warnings`), plus the ticket's tests/acceptance.
- The host tests the **pushed branch HEAD**, not your worktree — `qhost test` warns
  on uncommitted tracked changes. Commit first.

## Traps that have actually bitten
- **Never roll a stack back below an applied DB migration.** Deploying a
  DB-touching branch and then checking out an older ref crash-loops the
  control-plane (`fatal: migrations: no migration found for version N`) — the older
  binary embeds no down-file for that version. Recovery: redeploy the branch that
  embeds the migration. An unmerged DB-touching branch therefore keeps its stack on
  that branch until merge.
- **A plain `up -d` does not restart a container whose compose config is
  unchanged** — a rebuilt binary keeps running the old one in memory. That is why
  `agent` / `multiagent` force-recreate.
- **`web/dist` is a bind mount.** Rebuilding it from scratch swaps the directory
  inode and the running container serves 404 for `/` until restarted; browsers also
  cache `index.html` pointing at the old hashed bundle, which renders a blank page
  with no console error. `qhost web-deploy` handles both — then hard-refresh.
- **"No host available" on launch** has two causes: the node-agent never registered
  (`qhost logs` — healthy is `sent register` → `enrolled/reconnected as host <uuid>`
  → `capacity report sent`), or a stale session is holding every encode slot. Stop
  sessions you start; they count against capacity.
- **`ENROLLMENT_TOKEN` must match** between control-plane and node-agent (both read
  `deploy/.env`). A register rejection in the agent log means a token mismatch.
- **Encoder env traps:** VA needs `MESA_LOADER_DRIVER_OVERRIDE` /
  `LIBGL_ALWAYS_SOFTWARE` **unset** (blank is not unset — softpipe hides the
  encoder). Input needs `device_cgroup_rules: ['c 13:* rmw']` in the compose file or
  uinput devices open silently-fail and input never reaches the compositor.
- **Runtime image ≠ build image.** The node-agent binary is *baked* into the runtime
  image; an in-container `cargo build` or a compose `command:` override is a
  regression. For ad-hoc `cargo`, use the dev image. Which tag a host uses is
  `runtime_image` in `hosts.json`.
- **A host's git auth may be reboot-volatile** (see its `notes`). `qhost reauth` is
  the idempotent repair.

## Escalate rather than improvise
Ambiguous ticket, a frozen `protocol/` contract, the latency path, or
security/concurrency → Opus. A build or deploy that fails twice the same way →
stop and report the real output; do not test around a known bug.

Self-test: `scripts/validate [--live ROLE|HOST]` (offline checks run against a
synthetic fixture fleet — no real host is contacted).
