# Developer tooling catalogue

The complete inventory of Quasar's developer-facing tooling: the `make` interface, the
canonical scripts it delegates to, the acceptance/diagnostic harnesses, and the repo
skills. `make help` is always the live source of truth for targets; `AGENTS.md` holds the
operating contract (verification levels, isolation, authorization rules); this page is the
reference map of what exists and when to reach for it.

## Layering

```
make <target>                  ← discoverable façade (root Makefile, zero logic)
  └── scripts/dx/*.sh          ← testable orchestration (RESULT lines, guards, isolation)
        └── canonical scripts  ← scripts/verify.sh · scripts/dev/dev.sh · deploy/build-images.sh
                                 deploy/redeploy.sh · scripts/harness/run-*.sh
skills (.claude/skills/)       ← judgment-heavy remote workflows, hosts addressed by ROLE
```

Conventions every dx script follows: `PASS/FAIL/WARN <check> — <hint>` lines, exactly one
final `RESULT status=ok|degraded|failed target=<t> <k=v…>` machine line, exit codes
0 (ok/degraded) / 1 (failure) / 2 (usage or guard violation), no secret values in output.
Two deliberate deviations, both documented at their call sites: `session_soak.sh` exits 3
for "this encoder cannot live-resize", and the `make session-*` verbs use
0 ok / 1 degraded / 2 tool error / 3 usage so a caller can tell "the stream is bad" apart
from "I could not read the stream".
Every worktree gets an isolated `QUASAR_INSTANCE` (compose project, 10-port block,
ephemeral test database) so parallel checkouts and agents never collide.

## Make targets

| Target | What it does | Notes |
|---|---|---|
| `make help` | Lists all targets (default target) | Greppable; shows knobs + this worktree's instance/ports |
| `make init` | First-time setup: protocol submodule, devtools image, doctor | Idempotent; **never overwrites `.env`** |
| `make doctor` | Environment check: docker, go, node, submodule, disk, remote reachability | Remote check is ADVISORY → `degraded`, not failure |
| `make config-check` | Parses every compose file set; diffs `.env` keys vs `docs/configuration.md` | Key diff is advisory WARN |
| `make verify` | fmt + lint + build across components + shellcheck + DX self-tests | No DB, no network; the baseline for ANY change |
| `make test` | All unit suites | = test-go + test-rust + test-web |
| `make test-go` | Go build/vet/test | DB integration tests SKIP without a database — green here ≠ DB-tested |
| `make test-rust` | cargo fmt/clippy/test in the `quasar-agent-dev` container | Host has no GStreamer toolchain |
| `make test-web` | Web typecheck/test/build | |
| `make test-db` | Go integration tests vs a FRESH ephemeral Postgres | Per-instance port + name; `-p 1` enforced; container always reaped |
| `make docs-metrics-sync` | Copy `docs/session-trace/metrics.json` (the metric manifest) to its Go embed and web bundle copies | Both copies are byte-equality tested; a stale copy fails `go test ./...` |
| `make docs-trace` | Sync the manifest, then regenerate the `trace-format.md` §2 metric table from it | Edit the manifest, never the table; `make verify` fails if the table is stale |
| `make preflight` | doctor + config-check + verify | The pre-merge-to-develop gate |
| `make up` / `down` / `restart` | Local agentless stack (postgres + control-plane + web) | No node-agent locally (needs a Linux GPU host); volumes survive `down` |
| `make rebuild` | Local: compose build. Remote (`HOST=<role-or-host>`): delegates to `build-images.sh` + `redeploy.sh` | Remote requires typing `HOST=<role-or-host>`, e.g. `HOST=gpu-test` |
| `make status` / `health` | Container state (healthy/degraded/stopped/failed) / endpoint probes | Read-only; safe with a remote `HOST=<role-or-host>` |
| `make logs S=<svc> N=<n>` | Bounded log tail (default 200) | `S=` takes the compose service name on both hosts |
| `make logs-follow S=<svc>` | Streaming logs | The ONLY streaming target — deliberate use only |
| `make dev-web` / `dev-cp` | Native fast loops: vite dev / `go run` vs ephemeral PG | |
| `make diagnose` | One page: git state, instance, stack, health, versions, recent errors | |
| `make diagnose-bundle` | Sanitized tar under `.diagnostics/` (gitignored, 0700) | Redacted (secrets/URL creds/PEM/JWT/bearer); MANIFEST lists inclusions AND exclusions |
| `make session-list` | Every session on a stack, running first — `HOST=` | Read-only |
| `make session-verdict` | The session's **Verdict** — state, reason, evidence tier, clock, and the falsifier table (name/estimator/value/condition/n/holds) — `SID=<id>\|latest HOST=` | Prefers `GET .../verdict`, falls back to the diagnostic bundle on a 404 (older control plane). exit 1 when degraded; an unrecognised verdict is exit 0 + a note. `JSON=1` returns the Verdict verbatim |
| `make session-metrics` | Recent telemetry as a table — `SID= HOST= [SINCE= N=]` | Read-only |
| `make session-trace` | Trace window + events — `SID= HOST= [WINDOW=]` | Read-only |
| `make session-bundle` | Raw diagnostic-bundle JSON to a file — `SID= HOST= [OUT=]` | Written 0600 under `.diagnostics/` by default |
| `make session-capture` | ONE bounded observation of a live session — the encode graph, the encoder's live properties, or a dense burst of encode timings — decoded to a file — `SID= HOST= KIND=pipeline_dot\|encoder_props\|burst_stats\|all [OUT= WINDOWS= WINDOW_MS=]` | Arms, then polls (a 404 while in flight is the poll signal). Written 0600 under `.diagnostics/`; a `.dot` also renders `.svg` when graphviz is installed. Single-flight per session — `KIND=all` runs sequentially. exit 2 names the next command; **501 means the host's agent predates captures → `make rebuild`** |
| `make session-logs` | `quasar-sess-<sid>` / `quasar-pulse-<sid>` / node-agent logs — `SID= HOST=` | Over ssh; on devbox prefer the dozzle MCP |
| `make session-diagnose` | Full smoothness analysis (the former `qdiag`) — `SID= HOST=` | |
| `make admin-token` | Print an admin bearer for a stack — `HOST= [ARGS='--fresh']` | THE one ladder; stdout is the token, stderr the diagnostics |
| `make clean` | Build artifacts only | |
| `make reset CONFIRM=reset` | Remove THIS instance's local containers | Volumes preserved; `CONFIRM=reset-data` also removes them; **never remote** |

## Canonical scripts

| Script | Role |
|---|---|
| `scripts/dx/*.sh` | All make-target logic; `scripts/dx/tests/run.sh` is its deterministic self-suite (guards, redaction goldens, isolation, degraded paths, exit codes) |
| `scripts/dx/admin_token.sh` | **THE admin-bearer ladder** for the whole repo: `$QUASAR_ADMIN_TOKEN` → cached token → mint on the host. Every other script delegates; none carries a second copy |
| `scripts/dx/session.sh` | The live-session reader behind `make session-*`. Fetches; never mints |
| `scripts/dx/session_diagnose/` | The analysis package (bundle analysis, HTML report, IR/FEC experiment runners). Fed a bundle FILE; owns no credentials and no verdict vocabulary |
| `scripts/verify.sh` | Containerized verify runner (`quick`/`web`/`control`/`agent`/`db`/`full`) on the devtools image |
| `scripts/dev/dev.sh` | Container build/test wrapper (`image`/`build`/`test`/`check`/`cargo`/`go`/`go-check`/`go-test-db`/`run <name>`/`shell`) |
| `deploy/build-images.sh` | **The only supported image build path** — forces explicit `--target`, rejects undeclared build-args, validates against `deploy/image-contract.json` before promoting `:latest`. Never relax an assertion to make a build green. `--deploy` additionally recreates this host's node-agent and proves the deployment (below) |
| `deploy/validate-image.sh` | Assert any image against the contract. `--pull --require-digest-ref` validates an artifact that lives in a registry, addressed by digest — that is how `.github/workflows/images.yml` gates a GHCR publish before promoting any tag (`deploy/test-validate-image-refs.sh` covers the reference handling) |
| `deploy/redeploy.sh` | Canonical full-stack redeploy for one environment (web + control-plane + agent from one ref, post-deploy drift-verified) |
| `deploy/db-backup-restore-drill.sh` | Postgres backup/restore drill against disposable containers |
| `deploy/seed-tls-hosts.sh` | Idempotent TLS-host seed (operator path; run automatically by `redeploy.sh`) |
| `scripts/dev/seed-*.sh` | Idempotent catalog seeds for development (benchmark apps, diagnostics app) |
| `scripts/release/release-preflight.sh` · `generate-release-sbom.sh` · `scan-release-image.sh` | Release-evidence gates — deliberately separate artifacts, shared `release-supply-chain-lib.sh` |
| `scripts/release/changelog-section.sh` | Prints one version's `CHANGELOG.md` section — the release notes. The tag-push workflow refuses a tag whose section is missing or empty, before any build |
| `scripts/release/generate-platform-release-manifest.sh` · `validate-platform-release-manifest.sh` | Write and check `platform-release-manifest.json`, the GitHub Release asset naming each component image by digest ([schema](../scripts/release/platform-release-manifest.md)). Not `scripts/release/release-manifest.json`, which is the preflight's committed INPUTS file |
| `scripts/harness/lib/harness.sh` (+ `harness-selftest.sh`) | PASS/FAIL/SKIP/report core every `run-*.sh` harness builds on |

### Where things live

`deploy/` holds only what an operator installing Quasar needs (compose files, Dockerfiles,
the image contract and pins, the build/redeploy/validate entrypoints, `patches/`). Everything
else moved out on 2026-08-27:

| Directory | Holds |
|---|---|
| `deploy/overlays/` | Situational compose overlays, none of them part of a normal install: `dev` (the build-from-source shape — `redeploy.sh` applies it), `local`, `multiagent`, `cores`, `profiling`, `adopt-volumes`, and `console` (the one operator-facing member — local display). [`deploy/overlays/README.md`](../deploy/overlays/README.md) has the table |
| `scripts/dev/` | `dev.sh`, the compose-overlay test, the volume migrator, the local-audio validator, the dev seeders and the diagnostics-app image |
| `scripts/verify/` | The verify stage scripts plus the devtools image they run on |
| `scripts/harness/` | Acceptance harnesses (`run-*.sh`), `lib/`, `checks/`, `fixtures/`, the `apitest` Go module, and `peer-driver.mjs` (the headless WebRTC peer driver, formerly `p4-troubleshoot.mjs`) |
| `scripts/release/` | Release preflight, supply-chain lib + manifest, SBOM, image scan, the Vulkan encoder runtime probe, and their offline contract tests |

### Deploying and proving the agent on a host

`deploy/build-images.sh runtime --deploy`, run on the host itself (`qhost --host <role> sh`,
or a plain shell on the box), builds the agent image and then recreates and verifies the
deployment. The `--deploy` checks are, in order: the running container's image ID equals the
fresh build (the 2026-07-14 wrong-tag trap); the agent process resolves to the image's
**baked** binary at `/usr/local/bin/quasar-node-agent` and not a workspace-compiled one (a
re-introduced compose `command:` override); and `QUASAR_PULSE_IMAGE` names an image that
actually has a `pulseaudio` binary (the 2026-07-26 silent-audio outage). It absorbed the
former `deploy/build-agent-host.sh`, which was already a thin wrapper over this script.

The companion check is `scripts/dev/validate-local-audio.sh [--capture N]` — PASS/FAIL/SKIP
checks for the local-audio (console-mode) PulseAudio sidecar: socket/cookie permissions,
non-root auth, sink/source defaults, log scan, optional amplitude capture, node-agent ALSA FD.

### The harness library

`scripts/harness/lib/harness.sh` is the shared pass/fail/skip/report core for
`scripts/harness/run-*.sh` (extracted from the pass/fail/counter/JSON-report block every
harness used to reimplement under a different variable name — `TOTAL_PASS`, `ST_PASS`,
`PASS_COUNT`). Source it, call `harness_init <name>`, then `pass`/`fail`/`skip`/`harness_note`
your way through a script and finish with `harness_report` (prints `PASS: n FAIL: n SKIP: n`,
writes `deploy/results/<name>-<ts>.json`, exits 1 on any FAIL). The file's header comment has
the full API; `scripts/harness/lib/harness-selftest.sh` is a runnable, stack-free proof it
works (`bash scripts/harness/lib/harness-selftest.sh`).

Two harnesses built on it, for issue #383 (admission on encode slots + live free-VRAM veto):

- `scripts/harness/checks/vram-telemetry.sh [--stack=<name>] [--staleness=SECS]` — standalone,
  idempotent, read-only: is live VRAM telemetry flowing right now? For every GPU on every
  online host it asserts `vram_sampled_at` is fresh and `vram_mb_free` is plausible, and skips
  (not fails) a GPU whose control plane predates #383. Sourceable —
  `scripts/harness/run-admission.sh` calls its `vram_telemetry_check` function directly as its
  own precondition step.
- `scripts/harness/run-admission.sh [--stack=<name>]` — the end-to-end admission harness:
  exhausts encode slots and expects `503 capacity_exhausted` with no session row persisted,
  confirms releasing a session re-admits, drives the live-VRAM veto by direct DB mutation of
  `gpus.vram_mb_free` (snapshotted and restored by its cleanup trap — never a control-plane
  restart, which would drop every agent websocket mid-run), proves a stale sample fails open,
  and confirms an app with no declared VRAM field launches fine. Runs **on the host** (not via
  `dev.sh run`, which mounts only the repo with no docker socket) since it needs `docker exec`
  against the postgres/control-plane containers. Refuses to run against a stack with foreign
  active sessions unless `ADMISSION_ALLOW_SHARED=1`.

## Acceptance / diagnostic harnesses (`scripts/harness/run-*.sh`, via `scripts/dev/dev.sh run <name>`)

This corpus is deliberately curated — one-off investigation harnesses are culled once their
issue closes (precedent: 2026-07-17; recover any from git history). Current set:

| Harness | Proves |
|---|---|
| `scripts/harness/run-admission.sh` | Encode-slot + live-VRAM admission (503 path) |
| `scripts/harness/run-apitest.sh` | OpenAPI conformance vs `protocol/openapi.yaml` |
| `scripts/harness/run-codec-validate.sh` | Per-codec encode/decode validation (h264/h265/av1) |
| `scripts/harness/run-nvenc-fallback-smoke.sh` | Vulkan → vendor-encoder per-session fallback |
| `scripts/harness/run-st-trace.sh` | Session tracer end-to-end (Observability v2) |
| `scripts/harness/run-spt06-certify.sh` | Encoder certification bench (rung × bitrate, real peer) |
| `scripts/harness/run-soak-profile.sh` | Leak detection: session-cycle soak → CSV → verdicts (LEAK/SUSPECT/FLAT…); `scripts/harness/lib/soak_report.py` |
| `scripts/harness/checks/vram-telemetry.sh` | Live VRAM telemetry is flowing (read-only; sourceable) |
| `scripts/harness/peer-driver.mjs` | The headless WebRTC peer the shell harnesses drive |
| `scripts/dev/validate-local-audio.sh` | Console-mode Pulse sidecar audio |
| `scripts/release/probe-vulkan-encoder-runtime.sh` | Vulkan encoder actually registered on this GPU |
| `scripts/release/test-*.sh` | Offline contract tests for the release scripts (mock docker, fixtures). `test-release-preflight.sh`, `test-release-supply-chain.sh`, `test-probe-vulkan-encoder-runtime.sh`, `test-changelog-section.sh`, `test-platform-release-manifest.sh`. Run them by hand — no verify stage does |
| `deploy/db-backup-restore-drill.sh` | Postgres backup/restore drill (disposable containers) |

The phase-era harnesses (`run-p1-10-demo`, `run-p3-multihost`, `run-p5-home`,
`run-p4-troubleshoot`, `run-as06-abr-netem`) and the overlap repro were culled 2026-08-27
along with their phases; recover any from git history.

## Skills (`.claude/skills/` — judgment-heavy workflows)

Hosts are addressed by **role** — `gpu-test`, `aux-infra`, `deploy-only` — resolved from
`_shared/hosts.json` (schema + placeholders: `_shared/hosts.example.json`). Skill prose and
script defaults contain no deployment-specific hostnames or hardware; box facts live in each
host's `notes[]`. The `aux-infra` role is the sanctioned infrastructure host (network
impairment, headless browser peer) even where it is not a test target.

| Skill | Use for | Pairs with |
|---|---|---|
| `quasar-host` | Anything that must run ON a fleet host: container build/test loops, stack ps/up/down/logs/health, redeploy, drift between hosts, shell/file staging, GPU checks | everything below |
| `quasar-session` | Launch/observe/tear down a real WebRTC session; headless browser peer (on aux-infra); decode verification; metric series | quasar-netem, quasar-host |
| `quasar-netem` | Network impairment: shape the impairment host's loopback/ingress or the streaming host's egress (`sender`/`ingress` verbs) | quasar-session |
| `quasar-diagnose` | Multi-run smoothness work: soaks, run comparison, the ABR/IR/FEC experiment matrices, the HTML report. Reading ONE live session is `make session-*`, not this skill | quasar-session, quasar-netem |
| `quasar-image` | Fleet image audit/validate/build/re-pin/deploy; wraps the build-images contract flow | quasar-host |
| `quasar-ticket` | Per-ticket discipline vs frozen `protocol/` contracts: TDD, container builds, acceptance line, develop-branch flow (no PR) | — |
| `ship-milestone` | Drive a GitHub milestone end-to-end (issue DAG, tiered subagents, done-gates, report) | quasar-ticket |

Rule of thumb: **mechanical + local → `make`; judgment + remote → a skill.** When both could
work, prefer `make` — its output contract and guards are cheaper to reason about.
