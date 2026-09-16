# RH-01 #251 — reconnect/stop reconciliation

Status: **PASS — #251 acceptance complete**. Reviewed and deployed source: `64083f2558001d50cb43eaf604d6470c7a8c084d`.

[Sanitized observations, deployed identities and cleanup evidence](2026-09-17-rh01-251/evidence.json) · [Next #240 prompt](2026-09-17-rh01-240-handoff.md).

## Scope and decision

Builds on #239 and the approved #228/#229 lifecycle contracts. The change stays in the control plane; the runtime, agent image and frozen protocol remain unchanged.

An explicit heartbeat from the currently registered agent connection reconciles absent sessions which have reached `running`. A missing list is not a report of absence. A listed session undergoing teardown remains `stopping`, retains its reservation and blocks another writer to its managed home.

For an absent session, the database transition is conditional on the same host and an eligible current state:

- `running` becomes `failed`, with `host_lost` detail.
- `stopping` with a recorded `started_at` becomes `stopped`, with `host_lost` detail and no application-failure report. This preserves intentional-stop semantics.
- A pre-running stop remains on the existing acknowledgment, lifecycle callback, reconnect and stale-host paths. The heartbeat describes the agent's live map, not its pending assignments.
- A concurrent terminal transition wins without its evidence being overwritten or terminal bookkeeping being repeated.

Connection replacement and lifecycle dispatch share a per-host gate. Checking connection identity before a callback is insufficient: replacement could otherwise occur between that check and the database mutation. The gate must not hold the registry's connection-map mutex during callbacks, which can dispatch commands back through the registry.

This uses the existing agent lifecycle evidence rather than a new wire field, persisted connection generation or arbitrary teardown timeout. Slow teardown stays live in the agent's session map. An absent previously running session can no longer finish through that connection's ordinary session callback.

### Cleanup and managed homes

**Heartbeat absence is not proof of container deletion.** The agent can finish a session while a failed container removal remains durably pending. The control plane closes lifecycle ownership; it does not delete a home, clear an agent journal, or claim a successful engine mutation.

Safety continues to depend on the unchanged runtime gates accepted in #239: pending application operations retain writable-source identities; a later application launch must retire conflicting pending operations before acquiring that home; unresolved retirement fails closed. Process startup must complete durable application retirement before registration. Periodic maintenance retries cleanup without depending on a WebSocket connection.

The baseline also exposed a related managed-home guard gap: `liveHomeSessionSQL` omitted `stopping`. The shared launch/swap predicate now includes it. This prevents admission while legitimate teardown is still in progress, including stops before `running`; terminal reconciliation releases this database exclusion, while the agent's durable runtime gate remains authoritative for actual writable-source reuse.

## Reproduction and tests

The initial database-backed coordinator regression failed before production changes: after reconnect, user stop and an explicit empty heartbeat, the row remained `stopping`. The final expected outcome is `stopped` with `host_lost` detail, preserving the existing intentional-stop contract.

Live baselines independently reproduced the same sequence on both targets:

| Baseline | State after repeated heartbeats | Application cleanup | Managed-home marker | Recovery |
| --- | --- | --- | --- | --- |
| Isolated AMD | `stopping`, `stop requested`, after 20.8 s | Journal `Completed`; exact owned containers absent | Preserved | Required another agent reconnect |
| NVIDIA `gpu-test` | `stopping`, `stop requested`, after 20.6 s | Journal `Completed`; exact owned containers absent | Preserved | Required another agent reconnect |

Each baseline subsequently relaunched the same managed home and stopped normally. The initial AMD home-guard probe also demonstrated that the old predicate admitted a second same-home launch while the first row was `stopping`; that disposable session was stopped and recovered before the clean baseline rerun.

Focused tests pass with Go's race detector. Coverage includes the exact reconnect/stop race, missing/null versus empty lists, listed slow teardown without duplicate stops, retained reservation/home exclusion, intentional-stop reporting without a failure audit, duplicate reconciliation without repeated forget hooks or changed terminal evidence, wrong-host events, cancelled observation, a real competing terminal transaction, and shared homes across launch/swap and derived app families.

The full database run caught a test-isolation defect: audit assertions included unrelated activity from earlier packages. The corrected assertions select the exact session target, retaining the no-failure and exactly-once requirements. Independent specification re-review passed.

Connection-gate tests pass for 20 race-enabled runs. They cover a replacement queued behind a live callback, current-pointer rejection after displacement, same-host command dispatch without deadlock, progress on another host, and a third registration queued behind disconnect bookkeeping.

A second deterministic RED test exposed a review defect: an autocommit update could commit terminal state before a returned row failed to decode. A PostgreSQL `infinity` timestamp, which the Go session type cannot represent, reproduces this without SDK mocks. The corrected operation reads all returned session data inside an explicit transaction and commits only after successful decoding; the same test now leaves the row `running` and retryable on decode failure.

| Check | Result |
| --- | --- |
| `make verify` | PASS — 428 checks, no warnings/failures |
| Focused coordinator/store tests, `-race` | PASS — including decode-failure rollback |
| Connection registry tests, `-race -count=20` | PASS |
| `make test-go` | PASS — final run after test-isolation correction |
| `make test-db` | PASS — full fresh-database suite, no cache; session package 92.563 s |

## Deployed candidates and live acceptance

Both agents remain on the accepted #239 source `fc5505806d4f5f37d3af455b4ebf14e7692b18fb`, image `quasar-node-agent:20260916-1638-rh01-239e`, ID `sha256:2d6455dd0d37ae553ccd5ca65f0e520884d20263b9f5ca4f50cfc4012bc7012b`.

Baseline control-plane identities are recorded separately from agent identities: AMD image `sha256:fa08894a9796597b62c5de5e9626cf8c6768132d1656886c929d527b7c27e9ac` reports no source stamp; NVIDIA image `sha256:204b96bf4e7c00b8a091fad617cd00cccd010ca166f6a3d295b18541cf76f68e` reports source `81d5f078e4847162912834787929c618944b2c0d`. These are observed baseline identities, not claims that both control planes were rebuilt from the #239 agent commit. Both candidate builds report the reviewed source explicitly.

The NVIDIA baseline database was at schema 80; AMD was already at 84. The authorized control-plane deployment also applied the branch's existing migrations 81–84 on NVIDIA. A read-only preflight found zero application rows affected by migration 82, and a private database backup was captured. No migration or updater implementation is added by #251. After this upgrade, a control-plane binary embedding only schema 80 is not a valid rollback target; retain a schema-84-compatible control plane when returning to a known-good agent image.

Both candidate control planes report source `64083f2558001d50cb43eaf604d6470c7a8c084d` and schema **84**:

| Target | Control-plane image ID | Build timestamp (UTC) |
| --- | --- | --- |
| Isolated AMD | `sha256:6c4708a794c3d468a6713f7979f9bc9a17e3fbb9a07a4e84e6223b5e808b542e` | 2026-09-16 22:37:05 |
| NVIDIA `gpu-test` | `sha256:20d863f520febf6caa5daaff7be9d8f0163be1b6298ae6baecc52fa0727564ed` | 2026-09-16 22:37:10 |

NVIDIA used `make redeploy-cp HOST=gpu-test REF=64083f2558001d50cb43eaf604d6470c7a8c084d`. AMD used the existing isolated stack wrapper with the canonical `Dockerfile.control` build and control-plane-only recreate, retaining its agent/LAN compose overlays. Both targets were idle; deployed source/schema, mount definitions and endpoints were verified. Agent container IDs, images and start times were unchanged by deployment. The live fault test subsequently restarted each same agent container narrowly, as authorized. No node-agent image build or Actions image build ran.

| Candidate check | AMD | NVIDIA |
| --- | --- | --- |
| Reconnect → stop → first heartbeat | `stopped/host_lost` in **4.888 s** | `stopped/host_lost` in **4.875 s** |
| Further reconnect required | No; one registration observed | No; one registration observed |
| Intentional-stop failure fields | None | None |
| Independent application cleanup | Journal `Completed`; exact containers absent | Journal `Completed`; exact containers absent |
| Same-home relaunch | Running; original marker preserved | Running; original marker preserved |
| Normal stop and complete teardown | **10.961 s** | **10.660 s** |
| Home admission during normal teardown | Rejected; row remained `stopping` across heartbeats | Rejected; row remained `stopping` across heartbeats |

The live test separates three observations: terminal control-plane state without another reconnect; completed application journal and absence of the exact owned containers; successful same-home relaunch with the original marker preserved.

## Review, cleanup and limitations

Fixed-diff reviews were performed without concurrent edits to the reviewed files. The primary agent reviewed standards/correctness; `gpt-5.6-sol` independently reviewed the specification. The `gpt-5.6-terra` implementation worker received and corrected defects before re-review; the primary agent expanded the coordinator/store test coverage.

| Review axis | Findings resolved | Final code review |
| --- | --- | --- |
| Standards/correctness | Post-update reads could skip terminal hooks; missing race/duplicate/home coverage; misleading comments and test name | PASS |
| Specification | Independently found the post-commit read gap, then required scan/iteration errors to roll back before commit | PASS |

The final reviewed code diff against `24dbc106f81c6c9d2b4ff2542010542656c7fa4d` has SHA-256 `7ace586bc90fe846ebf041554b5b6bf3ef0ad146204dd9e9fe538f2831f93aa6`. The transaction correction does not claim a new durable outbox guarantee for an indeterminate database commit reply.

The operator requested a durable shared-host version preflight after the baseline mismatch. `AGENTS.md` now requires observed control-plane source/image/schema, agent source/image, stack identity, session ownership and a compatible rollback target before testing or deployment, with a recheck immediately before mutation. Independent review passed. The #240 handoff references this rule.

Final cleanup passed on both targets. All disposable app records and the three recorded disposable home paths were removed; no owned application/audio containers remain. Both stacks are idle and online. The two unrelated AMD apps and five unrelated NVIDIA apps, existing settings/host overrides, agent container/image/mounts and host identity are preserved. All application journals are `Completed`. No temporary settings remain. Private backups and raw evidence remain local; only sanitized observations are attached.

The existing #239 boot-retirement limitation for session runtime directories is unchanged. Any disposable paths left by the interrupted test are identified from the owned session's mounts and removed narrowly after container cleanup. No host-wide faults, engine restart, driver changes or broad pruning are part of this acceptance.

Live #251 acceptance covers lifecycle reconciliation, teardown and managed-home reuse. It does not repeat the #239 encoder/audio/input certification; those paths and agent images are unchanged.

Intel hardware is unavailable. This control-plane change does not alter Intel runtime paths or claim additional hardware certification. #240 remains separate and is not started by this work.
