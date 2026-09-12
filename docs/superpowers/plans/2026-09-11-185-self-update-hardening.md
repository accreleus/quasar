# Self-update hardening (#185) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> Grilled with the operator 2026-09-11 (rounds 1–3, all recommendations accepted); the decisions below are the record. The term **Preflight** is in `CONTEXT.md`.

**Goal:** An install that cannot take a release is told so on the release card before any apply starts; a host whose updated agent will not come healthy is put back on its previous digests without an operator; a run that skipped a host cannot be read as a clean success.

**Architecture:** One contract amendment (#186, amendment 9 on `quasar-protocol`) carries every wire change the four hardening pieces share, so it is signed off once. Preflight (#187) is a *pure* extension of `PlanRelease`: every collector is a read the view already performs or a read of the host's own readiness report, and the decision feeds eligibility as a new reason, so the fleet run, the per-host apply, the unattended path and the console all refuse a blocked target through the machinery they already have. Automatic revert (#188) is done by the **updater**, not the control plane: when a node-agent apply fails its health wait the agent that would carry a control-plane-issued revert is the one that is down, and the updater already holds `.env.prev` and the previous digests — it restores, records `restored`, and the restored agent's reconnect replays the result (#193) so the control plane can write the `auto_revert` history row. Conformance (#189) is three new agent readiness checks plus the three-way socket diagnosis on the control-plane side; both use the amendment's check-id vocabulary so preflight and readiness say the same words. Partial outcome (#190) is a new terminal run state decided by a pure function of the run's skips, with skips persisted, and "Retry skipped hosts" is a plain fleet apply linked by `retry_of`.

**Tech Stack:** Go 1.24 (control plane + updater, pgx, `make test-go` / `make test-db`), Rust 2021 (node-agent, `make test-rust`), TypeScript/React/Vitest (web, `make test-web`), openapi-typescript for `web/src/api/schema.d.ts`.

---

## Decisions that differ from the issue text, and why

Read these before executing; each one is a place where the issues' suggested fix does not survive contact with the code.

1. **#187's per-host `health_addr_bindable` is not tested by the updater.** The updater is not host-networked, and a bind test from a host-networked updater could not tell the agent's own listener from a squatter without also asking `/health` who answers. The agent already answers `/health` with `node` and `pid` (#152). So the check is an **agent readiness check** (#189): GET the configured `QUASAR_HEALTH_ADDR`, compare `node`+`pid` to self. Preflight reads it from `hosts.readiness`, refreshed every 15 s by the agent. No compose change, no agent-api change.
2. **#187's `image_resolvable` is checked from the control plane against the registry, once per release, not by each host's updater.** A per-host `docker manifest inspect` needs a new agent-api message to reach the updater through the agent. The registry check says "the digests exist where every host will pull from"; a host that cannot reach the registry still fails `pull_failed` at its step, exactly as today. The contract text says so.
3. **`preflight_blocked` is an `EligibilityReason`, not a parallel gate.** Inserted after `control_plane_not_first` and before `attempt_in_flight` (a stack shape is a durable fact; the two transient reasons stay last). Everything downstream falls out: the fleet run skips a blocked host (→ partial, #190), the per-host apply answers `409 host_not_eligible reason=preflight_blocked`, `PlanAutoApply` refuses a blocked control plane and skips a blocked host, the console's existing `controlPlaneBlocker` disables Update when the control plane is blocked. `unknown` never blocks: a fleet of pre-amendment agents must keep updating.
4. **#188's revert is performed by the updater.** `control-plane/internal/updater/exec.go` says "A node-agent apply is NEVER auto-restored" because a silent revert hides the failure. It is not silent here: the result carries `restored: true` and the failed container's last log lines, the wire gains `restored` (additive on `release_state`), the control plane records a `kind: auto_revert` attempt row beside the failed one, and the run stops `failed`. ADR 0004 records the reversal.
5. **#190's "Retry skipped hosts" needs no `host_ids` filter.** A plain fleet apply of the same release already touches only the hosts that are behind: the control plane and the updated hosts read `up_to_date` and are skipped. The retry carries `retry_of` for the link. The partial rule is therefore "at least one skip whose reason is not `up_to_date`" — `install_mode_source` and `updater_absent` hosts count, because the fleet IS on mixed versions and the issue's complaint is exactly that nothing shouts.
6. **Skips are persisted** (`platform_apply_runs.skipped JSONB`). A `succeeded_partial` run whose skip list evaporated on a control-plane crash would be a state with no explanation.
7. **The operator's Revert skips `auto_revert` rows** (grilling Q18). Both `LastSucceededAttempt` (server) and `revertStates` (console) derive the Revert target from the latest succeeded attempt's `previous_digests`; an accurate `auto_revert` row would make them offer the release that just failed. Both exclude the kind, pinned together by a test.
8. **`pull_failed` restores `.env` only** (grilling Q24): the old container is still running, so no recreate; the file just stops naming an image that never arrived.
9. **Live gates after the cut** (grilling Q20/Q25): the squatted-port scenario on gpu-test proves the double failure (a post-#152 restored agent fails the same bind); a request posted to the updater socket with a non-agent digest proves the clean restore and the `restored` replay; aux-infra's agent is stopped briefly for the partial-outcome gate and brought back for the retry.
10. **Issue closing** (grilling Q26): #186–#190 and #184 close on the develop merge; #185 after the post-cut gates; #173 after its item 4.
11. **An agent `skip` reads as a preflight `pass`** ("not applicable"), not `unknown` (in-session review, 2026-09-12): a disabled health endpoint has nothing to bind and a stack with no updater service is already `updater_absent`; neither should keep a target's preflight `unknown` for ever.
12. **A restore that itself fails leaves no `auto_revert` row** (in-session review): `restored` is only ever `true` for a restore that came up, so a failed one is unrepresentable on the wire; both failures are in the failed apply's output and `previous` is the recipe.
13. **Migration number is 0083.** PR #182 (#176, held for its own schema.md amendment) also claims 0083. Whichever lands second renumbers to 0084; golang-migrate only moves forward, so a stack that applied 0084 first would never apply 0083. Flag this at landing time.

## What the amendment (#186) contains

Filed as **amendment 9** on `quasar-protocol` (branch `amend/self-update-hardening`, pushed; merged to `main` only on the operator's sign-off; the superproject pins the branch commit, which is the same sha after a fast-forward merge).

| Surface | Change | Additive? |
|---|---|---|
| `PlatformReleaseTarget` | `preflight: PlatformPreflight` — `{ state: ok\|blocked\|unknown, checked_at: date-time\|null, checks: [{ id: PreflightCheckId, status: pass\|fail\|unknown, detail: string }] }`. Evaluated for every target whether or not a release is listed (the stack-shape checks are useful on their own; `image_resolvable` is `unknown` with no release). | yes (new required field on a server implementing this amendment; absent on an older one) |
| `PreflightCheckId` | closed: `updater_socket`, `updater_stack_dir`, `updater_overlays`, `image_resolvable`, `agent_connected`, `health_addr_bindable`. The same ids are the agent's readiness check ids for the three it can answer about itself. | new schema |
| `EligibilityReason` | `preflight_blocked` inserted after `control_plane_not_first` | inserted, see decision 3 |
| `POST /v1/admin/platform/apply` | `409 preflight_blocked` when the control-plane target is blocked; body `retry_of` (uuid, optional) | yes |
| `POST /v1/admin/platform/hosts/{id}/apply` | `409 host_not_eligible` may carry `reason: preflight_blocked` (no new code) | prose only |
| `ApplyRunState` | `succeeded_partial` appended | yes |
| `PlatformApplyRun` | `retry_of: uuid\|null` | yes |
| `PlatformApplyAttempt.kind` | `auto_revert` appended | yes |
| `agent-api.md release_state` | `restored: boolean` (optional, additive): the updater restored the previous digests itself after this failure | yes |
| `schema.md` | migration 0083: `platform_apply_runs` state CHECK gains `succeeded_partial`, `retry_of UUID NULL REFERENCES platform_apply_runs ON DELETE SET NULL`, `skipped JSONB NOT NULL DEFAULT '[]'`; `platform_apply_attempts` kind CHECK gains `auto_revert` | yes |
| `control-api.md` | the section text for all of the above; the "no partial state" sentences are corrected; the "never auto-restored for a host" sentences are corrected | — |

## File map

**Protocol (submodule `protocol/`, repo `quasar-protocol`)**
- `openapi.yaml` — shapes above.
- `control-api.md` — new §"Self-update hardening (amendment 9, #185)" after amendment 8, plus corrections in amendment 2's text.
- `agent-api.md` — `release_state.restored`; the `never_started` row's "never auto-restored" sentence.
- `schema.md` — the two tables + the 0083 line in the migration list.

**Control plane (Go)**
- `control-plane/migrations/0083_self_update_hardening.{up,down}.sql` — new.
- `control-plane/internal/platform/preflight.go` — new: `PreflightCheck`, `Preflight`, `PreflightFacts`, `PlanPreflight` (pure), the id constants.
- `control-plane/internal/platform/preflight_test.go` — new.
- `control-plane/internal/platform/preflight_collect.go` — new: `SelfPreflightFacts` (socket three-way + `/v1/self`), `ImageResolver` (registry HEAD with TTL cache), `hostFactsFromReadiness`.
- `control-plane/internal/platform/release.go` — `ReasonPreflightBlocked`, `Target.Preflight`, `HostIdentity.Readiness`/`ReadinessReportedAt` (unserialized).
- `control-plane/internal/platform/plan.go` — `PlanInputs` gains the facts; `targets` attaches preflight; `hostReason`/`controlPlaneReason` gain the reason.
- `control-plane/internal/platform/handler.go` — `Deps` gains `ControlPlanePreflight`, `ImageResolvable`; `releaseView` gathers them.
- `control-plane/internal/platform/apply_self.go` — `UpdaterSelf` gains the fields preflight reads; `UpdaterClient.SocketState()`; `SelfApplier.PreflightFacts()`.
- `control-plane/internal/platform/apply.go` — `KindAutoRevert`, `RunSucceededPartial`, `ApplyRun.RetryOf`, `ReleaseStateReport.Restored`.
- `control-plane/internal/platform/apply_fleet.go` — `finishHosts` decides via `RunOutcome` (pure); skips persisted through the store.
- `control-plane/internal/platform/apply_fleet_store.go` — `retry_of`, `skipped` columns; `RecordSkip`; `CreateRun` takes `retry_of`.
- `control-plane/internal/platform/apply_fleet_handler.go` — `preflight_blocked` refusal; `retry_of` validation.
- `control-plane/internal/platform/apply_runner.go` — `HandleReleaseState` writes the `auto_revert` row on `restored`.
- `control-plane/internal/platform/apply_store.go` — `CreateAutoRevertAttempt` (terminal on insert).
- `control-plane/internal/platform/auto_apply.go` — no logic change; test that a partial run does not suppress and that a behind host is picked up.
- `control-plane/internal/updater/exec.go` — restore on a failed node-agent health wait; failed-container log tail in the output.
- `control-plane/internal/updater/server.go` — `SelfResponse` gains `stack_dir_visible`, `service_config_files`.
- `control-plane/internal/agentws/messages.go` — `ReleaseStateMsg.Restored`.
- `control-plane/cmd/quasar-control/app.go` — wiring.
- `control-plane/internal/crud/store.go` — `Hosts` projection carries readiness.

**Node agent (Rust)**
- `node-agent/src/readiness.rs` — `ProbeEnv.updater`, `ProbeEnv.health`, three checks.
- `node-agent/src/readiness/platform_update.rs` — new: the collectors (`UpdaterSelfView`, `HealthOwner`) and the three check functions with tests.
- `node-agent/src/messages.rs` — `ReleaseState.restored`.
- `node-agent/src/release/mod.rs` — `UpdaterResult.restored` relayed.

**Web (TypeScript)**
- `web/src/api/schema.d.ts` — regenerated.
- `web/src/pages/admin/fleet/releasesCopy.ts` — texts for the new identifiers.
- `web/src/pages/admin/fleet/preflight.ts` — new: `blockingCheck(target)`, `preflightSummary`.
- `web/src/pages/admin/fleet/ReleasesTab.tsx` — preflight detail under a blocked target; holdout text.
- `web/src/pages/admin/fleet/FleetApply.tsx` — modal names hosts that will be skipped; partial banner; Retry button; retry link.
- `web/src/pages/admin/fleet/ApplyControls.tsx` — `auto_revert` label.
- `web/src/lib/readiness/groups.ts` — `platform_update` group.
- `web/src/api/admin.ts` — `retry_of` on the apply body.

**Docs**
- `docs/adr/0004-automatic-restore-of-a-failed-agent-apply.md` — new.
- `docs/upgrading.md` — shrink the three recipes.
- `docs/configuration.md` — nothing new (no knob added); `CONTEXT.md` gains "Preflight".
- `CHANGELOG.md` `## Unreleased`.

---

## Phase A — the amendment (#186)

### Task A1: Draft amendment 9 on a `quasar-protocol` branch

**Files:** `protocol/openapi.yaml`, `protocol/control-api.md`, `protocol/agent-api.md`, `protocol/schema.md`

- [ ] In `protocol/`: `git checkout -b amend/self-update-hardening main`.
- [ ] `openapi.yaml`: add `PreflightCheckId` (enum of the six ids), `PlatformPreflightCheck` (`id`, `status: pass|fail|unknown` — NOT an enum type: a consumer passes an unrecognised value through, same rule as `ReadinessCheck.status`; `detail` string, operator prose never parsed), `PlatformPreflight` (`state: ok|blocked|unknown`, `checked_at`, `checks`). Add `preflight` to `PlatformReleaseTarget.required` and properties. Insert `preflight_blocked` in `EligibilityReason` after `control_plane_not_first`, and append a paragraph to its description. Append `succeeded_partial` to `ApplyRunState` and rewrite its description (there IS a partial state now, and it is not a failure). Add `retry_of` to `PlatformApplyRun` (required, nullable) and to `PlatformApplyRequest` (optional). Add `auto_revert` to the attempt `kind` enum with a sentence. On `POST /v1/admin/platform/apply`'s 409 description add `preflight_blocked`. On the per-host apply's 409 add `preflight_blocked` to the reason list.
- [ ] `agent-api.md` §`release_state`: add `restored` *(boolean, optional, additive — amendment 9)*; correct the `never_started` row and the `unhealthy`/`recreate_failed` rows: a node-agent apply that fails its health wait IS restored by the updater and says so with `restored: true`.
- [ ] `control-api.md`: new section "## Self-update hardening (amendment 9, #185, additive, admin-gated)" with: the preflight object and each check's meaning + the fix each detail names; the reason's position and the "unknown never blocks" rule; `succeeded_partial` and the partial rule (decision 5); `retry_of`; `auto_revert` and what the row means; the three sentences in amendment 2 that are now wrong, each quoted and corrected ("There is deliberately no `partial` state", "There is no automatic restore for a host", "A node-agent apply is never auto-restored").
- [ ] `schema.md`: `platform_apply_runs` rows for `state` (CHECK widened), `retry_of`, `skipped`; `platform_apply_attempts.kind` CHECK widened; the 0083 line in the migration list with the ROLLBACK note (down narrows the CHECKs — it must first fail any row in the new states/kinds, so it rewrites `succeeded_partial`→`succeeded` and `auto_revert`→`revert` before narrowing, and drops the two columns).
- [ ] Commit on the protocol branch: `feat(apply): preflight, automatic agent restore, partial fleet outcome (amendment 9, #185)`. Push: `git push -u origin amend/self-update-hardening`.
- [ ] Open the sign-off PR on `accreleus/quasar-protocol` (base `main`), body = the table above. Do NOT merge.
- [ ] In the superproject: `git add protocol` (the gitlink now points at the branch commit — the commit is on the remote, so a fresh clone can fetch it).

### Task A2: Regenerate `schema.d.ts`

- [ ] `make test-web` regenerates and diffs; run `cd web && npm run gen:api` in the devtools container (`docker compose -f scripts/verify/docker-compose.devtools.yml run --rm --no-deps -w /workspace/web devtools npm run gen:api`) and commit `web/src/api/schema.d.ts`.
- [ ] `make test-go` will now FAIL `TestOpenAPIDrift` only if a route was added — none is. Expected: green.

---

## Phase B — migration 0083 and the Go vocabulary

### Task B1: Migration

**Files:** `control-plane/migrations/0083_self_update_hardening.up.sql`, `.down.sql`

```sql
-- 0083 up (amendment 9, #185): a fleet run can end partial, a run can be a retry
-- of another, skips are persisted, and an updater-performed restore is a kind.
ALTER TABLE platform_apply_runs DROP CONSTRAINT platform_apply_runs_state_check;
ALTER TABLE platform_apply_runs ADD CONSTRAINT platform_apply_runs_state_check
    CHECK (state IN ('pending','running','succeeded','succeeded_partial','failed','cancelled'));
ALTER TABLE platform_apply_runs
    ADD COLUMN retry_of UUID NULL REFERENCES platform_apply_runs(id) ON DELETE SET NULL,
    ADD COLUMN skipped  JSONB NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(skipped) = 'array' AND octet_length(skipped::text) <= 16384);
ALTER TABLE platform_apply_attempts DROP CONSTRAINT platform_apply_attempts_kind_check;
ALTER TABLE platform_apply_attempts ADD CONSTRAINT platform_apply_attempts_kind_check
    CHECK (kind IN ('apply','revert','auto_revert'));
```

- [ ] Check the actual constraint names first: `\d platform_apply_runs` in a `make test-db` Postgres, or grep 0075 for named constraints. If 0075 used unnamed inline CHECKs, Postgres named them `<table>_<column>_check`. Verify with a DB test, not by assumption.
- [ ] Down: rewrite the new values to their nearest old ones, drop the columns, narrow the CHECKs.
- [ ] Extend the DB test that pins `TerminalRunState` to SQL (`TestTerminalRunSplitMatchesSQL`) so it inserts a `succeeded_partial` row and an `auto_revert` attempt.

### Task B2: Vocabulary

**Files:** `apply.go`, `release.go`

- [ ] `apply.go`: `KindAutoRevert = "auto_revert"`; `RunSucceededPartial = "succeeded_partial"`; `TerminalRunState` includes it; `ApplyRun.RetryOf *string json:"retry_of"`; `ReleaseStateReport.Restored bool`.
- [ ] `apply_fleet_store.go`: `terminalRunStatesSQL` gains it; `runColumns` + `scanRun` read `retry_of` and `skipped`; `CreateRun(ctx, releaseID, force, actor, retryOf *string)`; `RecordSkip(ctx, runID, skip RunSkip)` appends with `skipped = skipped || $2::jsonb`; `fillRun` reads skips from the row (drop the in-memory `Skips` map — the store is the record now; keep `FleetRunner.Skips` as a read-through for the tests that use it, or delete and fix the tests).
- [ ] `release.go`: `ReasonPreflightBlocked = "preflight_blocked"`; `Target.Preflight Preflight json:"preflight"`; `HostIdentity.Readiness json.RawMessage json:"-"`, `HostIdentity.ReadinessReportedAt *time.Time json:"-"`.
- [ ] `crud/store.go` `Hosts` projection: select `readiness, readiness_reported_at` into the two new fields.
- [ ] `make test-go` green; `make test-db` green.

---

## Phase C — preflight (#187), pure first

### Task C1: `preflight.go` — the pure decision

```go
package platform

// Preflight check ids — the closed PreflightCheckId vocabulary (amendment 9).
// The three the agent can answer about itself are ALSO its readiness check ids
// (node-agent/src/readiness/platform_update.rs), so preflight and readiness
// never disagree about a name.
const (
	CheckUpdaterSocket      = "updater_socket"
	CheckUpdaterStackDir    = "updater_stack_dir"
	CheckUpdaterOverlays    = "updater_overlays"
	CheckImageResolvable    = "image_resolvable"
	CheckAgentConnected     = "agent_connected"
	CheckHealthAddrBindable = "health_addr_bindable"
)

const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckUnknown = "unknown"

	PreflightOK      = "ok"
	PreflightBlocked = "blocked"
	PreflightUnknown = "unknown"
)

type PreflightCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Preflight struct {
	State     string           `json:"state"`
	CheckedAt *string          `json:"checked_at"`
	Checks    []PreflightCheck `json:"checks"`
}

// PreflightFacts is what the collectors found for ONE target. Every field is a
// tri-state: nil pointer = nobody could look.
type PreflightFacts struct {
	CheckedAt *time.Time
	// Socket is the three-way #184 diagnosis. Control plane only.
	Socket *SocketState
	// Self is the updater's self-report, nil when it did not answer.
	Self *UpdaterSelfFacts
	// Readiness is the host's own report of the three agent checks, keyed by
	// id; a missing id is unknown (an agent predating the checks).
	Readiness map[string]ReadinessFact
	AgentConnected *bool
	// Image is instance-wide (decision 2) and copied onto every target.
	Image *ImageFact
}

type SocketState struct{ DirExists, SocketExists bool }
type UpdaterSelfFacts struct {
	Err          string   // non-empty: the socket exists but /v1/self failed
	StackDir     string
	ConfigFiles  []string
	// Per service, the compose file set that service's running container was
	// started with, from its labels. Nil when the updater could not inspect it.
	ServiceConfigFiles map[string][]string
}
type ReadinessFact struct{ Status, Summary, Remediation string }
type ImageFact struct{ Err string } // "" = every component resolved

// PlanPreflight is pure. Order is the order the card shows and the order a
// short-circuit would take; every check is still evaluated so the card can
// name every fix at once.
func PlanPreflight(kind string, f PreflightFacts) Preflight
```

Rules, each a table-test row:
- control plane: `updater_socket`: no dir → fail "the updater's socket volume is not mounted in this container; recreate the control plane (`docker compose up -d --force-recreate --no-deps quasar-control-plane`)"; dir but no socket → fail "the updater is not running (the volume is mounted, the socket is absent); `docker compose up -d quasar-updater`"; socket but Self.Err → fail "the updater did not answer: <err>"; else pass "updater <version> answered".
- control plane: `updater_stack_dir`: from Self; unknown when Self nil; pass names the dir; fail when StackDir=="" (an updater that discovered nothing — cannot happen past its own fail-closed boot, but stated).
- control plane: `updater_overlays`: unknown when ServiceConfigFiles nil; fail when any service's list ≠ updater's `ConfigFiles` (detail names the service and both lists: "quasar-control-plane was started with [a.yml] but the updater with [a.yml, b.yml]; re-run compose with the same -f set for both, or recreate the updater"); pass otherwise.
- host: `agent_connected` from the pointer (unknown when nil); the three readiness ids: `pass`→pass, `fail`→fail(summary + remediation), `warn`→pass with the summary, anything else/missing→unknown.
- both: `image_resolvable`: nil→unknown "no release listed" / fail(Err) / pass.
- `state`: any fail → blocked; else any unknown → unknown; else ok. `checked_at` = f.CheckedAt.

- [ ] Write `preflight_test.go` table tests FIRST (one per rule above + the state fold), run, see them fail to compile, implement, run green.
- [ ] Commit `feat(platform): the preflight decision as a pure function (#187)`.

### Task C2: Feed eligibility

**Files:** `plan.go`, `plan_test.go`

- [ ] `PlanInputs` gains `ControlPlanePreflight PreflightFacts` and `Image *ImageFact`. `targets()` builds `PlanPreflight(kind, facts)` per target (host facts derived from `HostIdentity`: `AgentConnected`, `Readiness` parsed into the map by id, `ReadinessReportedAt`; the image fact copied on). Attach to `Target.Preflight`.
- [ ] `controlPlaneReason` and `hostReason` take the preflight and return `ReasonPreflightBlocked` when `State == PreflightBlocked`, placed after `control_plane_not_first` and before `attempt_in_flight`.
- [ ] Tests: a blocked host reads `preflight_blocked`; a blocked host with an open attempt reads `preflight_blocked` (the precedence); an `unknown` host is eligible; the control plane blocked → `preflight_blocked` and every host `control_plane_not_first`.
- [ ] Commit.

### Task C3: Collectors

**Files:** `preflight_collect.go`, `apply_self.go`, `updater/server.go`, `handler.go`, `app.go`

- [ ] `updater/server.go` `SelfResponse` gains `service_config_files: map[string][]string` (each componentTarget's service → its container's `com.docker.compose.project.config_files`, via one `docker inspect` per service; a service with no container maps to nil). Cache the whole self-report for 15 s: the agent asks every 15 s and the control plane every 30 s.
- [ ] `apply_self.go`: `UpdaterSelf` decodes `version`, `working_dir`, `config_files`, `service_config_files`. `UpdaterClient.SocketState() SocketState` (two stats). `SelfApplier.PreflightFacts(ctx) PreflightFacts` reuses the InstallMode TTL cache (extend the cached value to hold the whole self-report + error).
- [ ] `ImageResolver` in `preflight_collect.go`: `Check(ctx, release) ImageFact` — for a manifest release, `InspectConfig(image@digest)` per component via `images.ImageInspector`; for an edge release, the `EdgeApplyResolver` resolves the tag. TTL 10 min keyed by release id; `Invalidate()` called by the detect job and by `handleFleetApply`/`handleHostApply` before their view read.
- [ ] `handler.go` `Deps`: `ControlPlanePreflight func(ctx) PreflightFacts`, `ImageResolvable func(ctx, Release) *ImageFact`. `releaseView` calls both (image only for `available[0]`, computed after `offerable` — so call `PlanRelease` in two steps or pass a closure; simplest: compute `offerable` first inside `releaseView` by calling `PlanRelease` once without image, then again with; avoid — instead add `PlanInputs.ImageFor func(Release) *ImageFact` evaluated inside `targets` for `newest`; a func in PlanInputs keeps `PlanRelease` pure enough because the closure is a cache read the test supplies).
- [ ] `app.go`: wire; `platformDeps` gains the two.
- [ ] DB test: `handler_db_test.go` renders a view with a stubbed updater socket dir absent → the control-plane target's preflight has `updater_socket` failing with "recreate the control plane" in the detail.
- [ ] Commit `feat(platform): preflight every target on the release view (#187)`.

### Task C4: Refusals

**Files:** `apply_fleet_handler.go`, `apply_handler.go`, `auto_apply_test.go`

- [ ] `handleFleetApply`: before the durable-reason refusal, if the control-plane target's reason is `preflight_blocked` → `409 preflight_blocked` with the blocking check named. Add `CodePreflightBlocked = "preflight_blocked"`. Read `retry_of` (uuid or absent; `404` if not a run) and pass to `CreateRun`. Call `ImageResolver.Invalidate()` first so the view is fresh.
- [ ] Per-host apply: nothing new — `host_not_eligible` already carries the reason.
- [ ] `auto_apply_test.go`: a blocked control plane → `not_eligible`; a blocked host → the run is created (the host is skipped at its turn — assert on `fleetTargetReason`).
- [ ] Commit.

---

## Phase D — readiness conformance (#189)

### Task D1: Agent checks

**Files:** `node-agent/src/readiness/platform_update.rs` (new), `readiness.rs`

- [ ] `platform_update.rs`:
  - `pub struct UpdaterView { pub socket_exists: bool, pub self_report: Option<Result<UpdaterSelf, String>> }` where `UpdaterSelf { version, working_dir, config_files, service_config_files: BTreeMap<String, Option<Vec<String>>> }`.
  - `pub struct HealthOwner { pub addr: Option<String>, pub answer: Option<Result<HealthIdentity, String>> }`, `HealthIdentity { node: String, pid: u32 }`.
  - `pub fn collect_updater(socket: &Path) -> UpdaterView` (GET `/v1/self` via `release::unix_http`, 3 s), `pub fn collect_health(addr: Option<String>) -> HealthOwner` (plain TCP GET `/health`, 2 s, parse `node`/`pid`).
  - `pub fn check_updater_socket(v: &UpdaterView) -> ReadinessCheck`: no socket → **skip** when compose reported no updater service (`buildinfo::updater_present == Some(false)`; pass that in) else fail "the updater service is in this stack but its socket is absent — the agent container predates the volume; recreate it"; socket + Err → fail; ok → pass "updater <version>".
  - `check_updater_stack_dir`: unknown → skip; `working_dir` empty or `config_files` empty → fail; also compare `service_config_files["quasar-node-agent"]` to `config_files` → fail naming both lists (this IS `updater_overlays`; emit both ids — `updater_stack_dir` for the dir, `updater_overlays` for the file-set match).
  - `check_health_addr_bindable(h: &HealthOwner, me_node, me_pid)`: addr None → skip "health endpoint disabled"; answer Err → fail "nothing answers <addr>; the next agent start will try to bind it — `ss -ltnp | grep <port>`"; identity ≠ me → fail "<addr> is answered by node <n> pid <p>, not this agent; free the port or set QUASAR_HEALTH_ADDR (…)"; else pass.
  - Unit tests for every branch with constructed views (no I/O).
- [ ] `readiness.rs`: `ProbeEnv` gains `updater: UpdaterView`, `updater_present: Option<bool>`, `health: HealthOwner`, `self_node: String`, `self_pid: u32`; `live()` collects them; `probe()` appends the four checks; the existing "pure w.r.t. env" doc still holds.
- [ ] `agent.rs` already re-probes every 15 s. Nothing to do.
- [ ] `cargo fmt`, `cargo clippy -- -D warnings`, `make test-rust` green. Commit `feat(node-agent): readiness checks for the update path (#189)`.

### Task D2: Console grouping

- [ ] `web/src/lib/readiness/groups.ts`: group `platform_update` "Updates" with the four ids; `groups.test.ts` pins them.
- [ ] Commit.

### Task D3: Close #184 through the control-plane socket check (done in C1/C3). Note it in the CHANGELOG line.

---

## Phase E — automatic restore (#188)

### Task E1: Updater restore for the node agent

**Files:** `updater/exec.go`, `updater/exec_test.go`, `updater/result.go`

- [ ] `result.go`: `Result.Restored` already exists (`restored`). Add nothing; document it now applies to both components.
- [ ] `exec.go` `Apply`: after `verify` returns a reason, `if restoreWorthy(reason, req.Components)`: run `restore`; set `res.Restored`; append the sentence. `restoreWorthy` is pure: `never_started` for either component (already), `unhealthy` and `recreate_failed` for a node-agent-only request. Never for the control plane's `unhealthy`/`recreate_failed` (it may have migrated — ADR 0002 unchanged).
- [ ] Before restoring, capture `docker logs --tail 40 <container>` of every failed service's container into the body (a new `Docker.Run` call; the container id is in `composePS.ID`). This is where `health-bind-failed` lands so the card can say why.
- [ ] `exec_test.go` with the fake docker: unhealthy node-agent → restore commands issued, `restored: true`, output contains the log tail and the restore sentence; unhealthy control plane → no restore; pull_failed → no restore.
- [ ] Commit `feat(updater): restore the previous agent when the new one fails its health wait (#188)`.

### Task E2: Relay `restored`

- [ ] `node-agent/src/messages.rs` `ReleaseState.restored: bool` (serde default false, skip when false — additive on the wire); `release/mod.rs` `UpdaterResult.restored` → `into_msg`. Test in `release/tests.rs`.
- [ ] `agentws/messages.go` `ReleaseStateMsg.Restored bool json:"restored"`; `app.go` adapter copies it.
- [ ] Commit.

### Task E3: The `auto_revert` row

**Files:** `apply_runner.go`, `apply_store.go`, `apply_runner_test.go`

- [ ] `apply_store.go`: `CreateAutoRevertAttempt(ctx, in NewHostAttempt, output string) (Attempt, error)` inserts `kind='auto_revert'`, `state='succeeded'`, `started_at=finished_at=now()`, `requested_digests`=the previous digests (as ComponentDigest with the image from the failed attempt), `previous_digests`=the failed attempt's requested set. Must not collide with the open-target index: insert AFTER the failed row is terminal.
- [ ] `HandleReleaseState` `AttemptFailed` branch: after `FailAttempt`, if `rep.Restored && a.Target == TargetHost && a.Kind == KindApply`: build and insert the auto_revert row; log `token=apply-auto-reverted`. The previous digests come from `rep.Previous` (non-nil digests only; if none, skip the row and log why).
- [ ] Also the fleet run: `resumeHost` logs "failed (reverted)" when the host's latest attempt is an `auto_revert`; the run still finishes `failed` (default stop rule, unchanged).
- [ ] Test with the fake store: a failed report with `Restored` produces the row with swapped digest sets; without `Restored` no row.
- [ ] Commit.

### Task E4: ADR 0004

- [ ] `docs/adr/0004-automatic-restore-of-a-failed-agent-apply.md`: context (#152 field incident, the updater is the only actor with access when the agent is down), decision, consequences (the `restored` field, the `auto_revert` row, control plane unchanged), the exec.go comment updated to point here.

---

## Phase F — partial outcome and retry (#190)

### Task F1: `RunOutcome` pure + persistence

**Files:** `apply_fleet.go`, `apply_fleet_test.go`, `apply_fleet_db_test.go`

```go
// RunOutcome is the terminal state a run that reached the end of its host
// list deserves: succeeded_partial when it passed over a host that was behind
// the release and could not take it — any skip except up_to_date.
func RunOutcome(skips []RunSkip) string
```

- [ ] `drive`'s final `f.finish(runID, RunSucceeded, "")` → `f.finish(runID, RunOutcome(skips), "")` where skips are read from the store row.
- [ ] `recordSkip` → `store.RecordSkip` (persisted). Delete the in-memory map and `Skips()`; `fillRun` reads the row.
- [ ] Tests: two hosts, one offline → `succeeded_partial` with the skip persisted; every host up_to_date → `succeeded`; DB test that a partial run round-trips through `scanRun`.
- [ ] Commit.

### Task F2: Suppression and the next tick

- [ ] `UnattendedFailedReleaseIDs` matches `state = 'failed'` only — `succeeded_partial` does not suppress. Add the DB test.
- [ ] `auto_apply_test.go`: view with one host behind and everything else up_to_date → `Apply: true` (this is the "retry on the next tick" guarantee).
- [ ] Commit.

### Task F3: `retry_of`

- [ ] Handler reads `retry_of`; stored; served. DB test: create run A, create run B with `retry_of=A` → B serves it; delete A → B's is null.
- [ ] Commit.

---

## Phase G — console

### Task G1: Copy and preflight helpers

- [ ] `releasesCopy.ts`: `preflight_blocked: "A preflight check failed on this target — see the check below."`; `succeeded_partial: "Applied, but at least one host was skipped and is still on the old release."`; `PREFLIGHT_CHECK_TEXT` for the six ids; `attemptKindText` (`apply` → Apply, `revert` → Revert, `auto_revert` → "Reverted automatically").
- [ ] `preflight.ts`: `blockingChecks(t: PlatformReleaseTarget): PlatformPreflightCheck[]` (status fail), `unknownChecks`. Tests.
- [ ] Commit.

### Task G2: Targets card

- [ ] `ReleasesTab.tsx` `TargetsCard`: under the per-host table, for each target with failing checks render a `.note` with the check name and its `detail` (the fix); the holdout line shows the first failing check's text instead of the generic reason. The `Fact label="Control plane"` chip becomes "Blocked" (danger) when `preflight_blocked`. `TargetChip`: `preflight_blocked` → `<Chip variant="danger">Blocked</Chip>`.
- [ ] `ReleasesTab.test.tsx`: a view with the control-plane target blocked on `updater_socket` renders the detail text and the Update button disabled with the reason in its title.
- [ ] Commit.

### Task G3: Fleet modal, partial banner, retry

- [ ] `FleetApplyModal`: list hosts that are not eligible for a reason other than `up_to_date`, as "Will be skipped: <name> — <reason text>", so consent names the partial outcome up front.
- [ ] `FleetRunPanel`: `RUN_STATE_CHIP.succeeded_partial = "warning"`; when partial, the banner line is built by `partialSummary(run, targets)`: "Applied to the control plane and 2 of 3 hosts — 1 skipped: <name> (offline)". The skipped list is already inline; keep it, drop the "Not updated" heading in favour of the sentence.
- [ ] "Retry skipped hosts" button on a terminal partial run (and only when `hasUpdate` still reports a step for some host): calls `applyPlatformReleaseToFleet(token, { release_id, force: false, retry_of: run.id })`. Hidden while a run is active.
- [ ] Run list: a run with `retry_of` renders "retry of <short id>" as a muted suffix; the original run renders "retried" when a later run in the list carries its id.
- [ ] Tests in `FleetApply.test.tsx`: partial banner sentence; Retry posts `retry_of`; a failed run reads unchanged.
- [ ] `ApplyControls.tsx` history row uses `attemptKindText`.
- [ ] `make test-web` green. Commit.

### Task G4: Visual verdict

- [ ] No v3 mock covers the Releases tab (ReleasesTab.tsx header says so); the change composes existing chips/notes/tables only. Screenshot the Targets card blocked state and the partial banner with the Playwright tools against a local `make up` stack seeded with a fake view if practical; otherwise record that the surface has no mock and nothing new was styled.

---

## Phase H — docs, changelog, gates, landing

- [ ] `docs/upgrading.md`: "Adding it to an existing install" step 3 keeps the recreate (it is still the fix) but loses the paragraph telling the operator to diagnose it by hand — the card names it now; the "before applying, check 9091" paragraph goes, replaced by one sentence pointing at the host's readiness card; the "there is no automatic rollback for a host" sentences in "Applying from the console" and "Update Quasar from the console" are rewritten; step 5 gains the partial outcome and Retry.
- [ ] `CONTEXT.md` "Platform releases": **Preflight** — the per-target set of stack-shape checks evaluated on the release view before an apply is offered; distinct from eligibility (may this target take it) and from readiness (can this host run sessions).
- [ ] `CHANGELOG.md` `## Unreleased`: Added — preflight (#187/#184/#186), automatic agent restore (#188), partial outcome + retry (#190), readiness update checks (#189); note migration 0083.
- [ ] Gates: `make test-go`, `make test-db`, `make test-rust`, `make test-web`, `scripts/dev/leak-scan.sh`. Run serially (devtools volume).
- [ ] Review: `mcp__alice-review` on the branch; resolve; re-run gates.
- [ ] Push `feat/185-self-update-hardening` to origin. Landing waits on the amendment sign-off: on sign-off, fast-forward `amend/self-update-hardening` into `quasar-protocol` `main`, verify the superproject pin sha is unchanged, merge to `develop`, push, and comment on #185–#190 with the merge sha. Renumber 0083 if PR #182 landed first.
- [x] Live gate, run 2026-09-12 on the operator's appliance stack (gpu-test was off) with the
  branch published by an Images dispatch and the stack's updater pinned to the branch build
  (the updater is not part of a release). Record:
  - **Apply through the previous control plane:** fleet run `succeeded` in 40 s, schema 82 → 83,
    every preflight check `pass` on both targets afterwards with its detail text.
  - **(a) squatted health port, restore also fails:** host reverted to its previous agent, a
    squatter took 127.0.0.1:9091 the moment the recreate freed it; the attempt ended
    `failed / recreate_failed` in 34 s, its output carries the new container's
    `health-bind-failed` lines, the restore's own failure and "apply the digests in `previous`
    by hand"; the host came back on the previous digest once the port was freed and replayed
    the result. No `auto_revert` row (decision 12).
  - **(a') squatted port released as the restore begins:** the updater reported
    `restored: true` in 23 s and the previous agent came up. **Defect found:** the restored
    agent connected while the updater was still verifying the restore, re-emitted `verifying`
    once (#193) and never relayed the end — the attempt sat `verifying`. Fixed in this branch
    (the agent adopts a non-terminal result found on connect); until a host runs that agent,
    restarting the restored agent after the updater finishes replays the terminal result.
    The `auto_revert` row cannot appear while the *restored* agent predates amendment 9 — it
    is the one that would say `restored`.
  - **(b) blocked control plane:** stopping the updater flipped the control-plane target to
    `preflight: blocked` on `updater_socket` within one refresh, and the fleet apply was
    refused `409 preflight_blocked` with the check's remedy in the message.
  - **Retry linkage:** a plain fleet apply with `retry_of` succeeded in 5 s and the run carries
    the link. (c) partial run + retry, and the adopted-restore path, are exercised against the
    fix build — see the session record on #185.
