# RH-02 #261: provenance and freshness on the readiness card — acceptance

| | |
|---|---|
| Ticket | #261, specification #252, protocol amendment 11 (#260), ADR 0005 |
| Source | `feature/rh-02-gate`: pin `c944a6d`, slice `97fd0d9`, card colour fix `820373d`. Branched from `initiative/resilient-host-architecture` at `21626c5`, after #268 (`b116637`). |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build, no shared stack touched. |
| Protocol pin | `128488d`: amendment 11 without the two override routes, which land with #263. `web/src/api/schema.d.ts` regenerated from it. |
| Images | `quasar-node-agent` and `quasar-control-plane` from `97fd0d9`, and `quasar-control-plane` again from `820373d`, through `deploy/build-images.sh … --no-prune --no-latest` (agent contract 148 passed, 0 failed, 2 GPU-gated skips; control plane 23 passed, 0 failed). Local to the development host and the lab's registry. |

## What changed

The agent reports three optional fields on each readiness check, and an inconclusive host
probe is `unknown` rather than a warning. The control plane is unchanged: it already stores
and serves the report verbatim. The console card shows the new fields. Nothing blocks a
launch yet; that is #262.

- `source` and `blocks` are set where a check is made, not looked up by id afterwards. The
  local constructors make proxy checks: source `local`, never `blocks`. A check that rests
  on evidence adds its own.
- `observed_at` is the refresh time for a locally recomputed check and the observation's own
  time for a retained one. An inconclusive probe leaves the last definitive result and its
  original `observed_at` in place, so it can neither set nor clear a block; `unknown` is
  reported only while no definitive result exists for that id.
- `ProbeTarget::blocks` returns nothing for a GPU-scoped probe with no index rather than
  panicking. No caller builds one today.
- Diagnostic mode still reports only the checks it observed; its two checks gained fields.

## Tests, written first

| Boundary | Tests |
|---|---|
| Agent checks (fake root), `readiness/tests/provenance.rs` | No proxy check carries `blocks`, over four fixture shapes and every id `probe` emits. The three local evidence checks carry their scope in every status. Every local check names `local` or `runtime`. Host-probe results carry `host_probe` and their scope on pass, fail and indeterminate. Indeterminate is `unknown`. An inconclusive probe keeps a held fail, and a held pass, with the original time. `unknown` only until a definitive result arrives. Refresh time against retained time, also across a failed refresh. The startup-cleanup safety check is agent-enforced. The wire shape: a check with no provenance serialises exactly as before, and `gpu_index` appears only for the gpu scope. |
| Control plane, DB-backed | `TestAmendment11ReadinessSurvivesTheWriteSeam` (agentws) and `TestAmendment11ReadinessServedVerbatimByTheHostReadPath` (crud, single read and list): the new fields, an unrecognised `source` and `blocks.scope`, and an unknown extra key survive; a legacy check gains no keys; `gpu_index` stays absent on a host-scoped block. Two tests, because crud imports agentws transitively and one binary cannot hold both seams. Each assertion was shown to fail under a deliberate mutation. No production Go changed. |
| Console | 14 card tests (the observed-at line, five source labels including an unrecognised one, a malformed time, the two marker texts and four titles, an unrecognised scope, `unknown` as Indeterminate and outside "not applicable", no "Needs attention" for `unknown`) and 3 group tests (`nvidia_driver_mount` under NVIDIA driver, `host_container_mounts` under Container runtime, `unknown` sorting between warnings and passes). Both ids joined the pin list. |

## Gates at `97fd0d9`

Run serially on the development container, nothing else running.

| Gate | Result |
|---|---|
| `make verify` | 432 pass, 0 warn, 0 fail |
| `make test-rust` (fmt, clippy `-D warnings`, tests) | 1715 passed, 0 failed, 9 ignored, on the second run; see below |
| `make test-go` | pass, including the OpenAPI route drift test at pin `128488d` |
| `make test-db` (fresh ephemeral Postgres, `-p 1`) | pass on the second run; see below |
| `make test-web` | pass: schema drift, typecheck, 236 test files, production build |
| leak scan (tree, operator patterns) | clean |

Two first-run failures, neither in this slice's code, both filed rather than rerun away:

- One Rust test from #256, the diagnostic-registration resume test, timed out once in the full
  suite. It then passed 30 of 30 alone and in 7 of 7 further full-suite runs. Filed as #269
  with the output and load averages. The two lock-lease tests of #197 did not fail.
- `make test-db` failed in its first package with a connection reset: the harness trusts
  `pg_isready` while the Postgres image is still on its init-time server. Filed as #270.

After `820373d` the card tests, the launch-wording tests and the typecheck were rerun and pass.

## Live evidence: AMD test host, 2026-09-19

Shared-host version preflight: zero containers and zero compose projects on the host, read
again immediately before the first mutation. A disposable stack under its own compose
project (Postgres, control plane, agent), schema 84, removed afterwards with its volumes and
its home and template roots. No session was created. Nothing to roll back to.

The stored report, read through `GET /v1/hosts`, 32 checks, agent `source_commit = 97fd0d9`:

| Checks | `source` | `observed_at` | `blocks` |
|---|---|---|---|
| `runtime_endpoint` | `runtime` | the refresh, 04:42:14Z | `host`, enforced by `agent` |
| `runtime_api_version`, `runtime_capabilities`, `runtime_cdi` | `runtime` | the refresh | none |
| `homes_root_writable`, `homes_free_space` | `local` | the refresh | `homes`, `control_plane` |
| the other 22 local checks, passing and skipped | `local` | the refresh | none |
| `input_probe`, `audio_probe` | `host_probe` | 04:41:14Z, a minute before the report | `host`, `control_plane` |
| `media_probe_gpu1`, `application_gpu_probe_gpu1` | `host_probe` | 04:41:16Z | `gpu`, index 1, `control_plane` |

Seven checks carry `blocks` and they are the seven the contract allows. The retained probe
results keep their own observation time while the local checks move with each refresh.
`startup_cleanup` is absent, as it should be outside diagnostic mode.

## Visual check

Headless Chromium against the disposable stack's own console, host detail page, 1440 px wide:
`rh02-261/readiness-card-amd.png`.

- The observed-at line is the card's small print: 11 px, which is `--t-xs`, in
  `oklch(0.6605 0.0265 259.81)`, which is `--text-3` in `console-v3.css`. The first build
  rendered it in `--text-2`, because `.host-setting-copy p` out-specifies `.muted`; `820373d`
  sets the token inline, and the measurement above is from the rebuilt image.
- The marker is the existing `chip chip-neutral chip-sm`, reading "Can block launches" on
  exactly the seven evidence checks, with the scope in its title. The failing form, "Blocks
  launches" in `chip-danger`, and the Indeterminate glyph are covered by component tests here
  and are shown live by #262's fault injection.
- Groups rendered: Container runtime (5, `host_container_mounts` last), GPU & display,
  Input & sandbox, Audio, Storage, Network, Updates. No "Other" group.
- No new class, colour, icon or stylesheet rule. No readiness mock exists in
  `design_handoff_v3`; the owner approved extending the card in its current idioms.

## Wire and compatibility

- A check with none of the three fields serialises with exactly `id`, `status`, `summary`,
  `remediation`, so an older control plane sees nothing new, and the card renders an older
  agent's report with no new element (pinned by a component test).
- The native client needs no change: it reads no host body, and `error.code` is an open string.
- Release preflight reads `hosts.readiness` as before and already maps an unrecognised status
  to its own `unknown`; no file under `internal/platform` changed.

## NVIDIA, Intel

NVIDIA: not run for this slice. The fields are vendor-neutral and set by the same code on every
host; the NVIDIA test host is exercised by #262's evidence. Intel: not validated, nothing claimed.

## Models

Failing tests for the agent, the verdict of every diff, the gates, the pin, the evidence and
this record: Fable 5.1. Agent implementation and the two Go tests: Sonnet 5. Card tests, group
and pin lists, changelog: Haiku 4.5. Card component: Sonnet 5. Review edits by Fable: the
panic-free `ProbeTarget::blocks`, comments cut to the repo's bar, realistic check ids in the Go
fixtures, exact marker-text assertions, and the small-print colour.

## Not verified

- The failing and indeterminate renderings on a live stack (deferred to #262's fault injection).
- The setup wizard and the Fleet hosts tab were not screenshotted; they render the same shared
  component, and their test files pass.
