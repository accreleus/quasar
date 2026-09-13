# Sessions survive a control-plane restart (#128) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A control-plane restart (~60–90 s) no longer ends every running session on every host.

**Architecture:** Three actors each end the session independently today, and fixing fewer than all three changes nothing at the `<video>` element. Phase 1 (Go, control plane) stops reaping `running` rows on reconnect and reconciles against `heartbeat.running_sessions` instead, with a boot-aware sweep as the backstop for a host that never returns. Phase 2 (Rust, agent) hoists session state above the reconnect loop and holds sessions for a bounded grace window instead of stopping them in `Drop`. Phase 3 (TypeScript, web) retries the signalling-token mint with backoff instead of giving up after one failure. Each phase lands and is testable on its own; phases 1 and 2 are ordered — the agent hoist is unsafe without the reverse-direction stop from phase 1.

**No protocol amendment.** `heartbeat.running_sessions` is already specified (`protocol/agent-api.md:453`), already populated by the agent (`agent.rs:1254`), and already decoded by the control plane (`agentws/messages.go:184`) — it is only debug-logged and never reaches the coordinator. Zero wire shapes change.

**Tech Stack:** Go 1.25 (control plane, pgx, `make test-db`), Rust 2021 (node-agent, `make test-rust`), TypeScript/React/Vitest (web, `make test-web`).

**Review corrections applied (2026-09-08).** The first draft was reviewed and had
five defects that would have stopped it being executed as written. They are fixed
inline below; recorded here because each is a trap for anyone re-deriving this:

1. `Host.LastHeartbeatAt` does not exist — the field is `LastHeartbeat *time.Time`
   and it is NULL for a host that has never heartbeated (every test fixture).
2. `c.agents` (`AgentConnectivity`) is nil unless `WithAgentConnectivity` is
   passed, so an unguarded `IsConnected` panics in tests — and an unwired
   backstop must never false-reap in production either.
3. `HostsWithRunningSessions` was referenced but never defined.
4. The test helpers the plan invented (`mustSession`, `mustState`,
   `newCoordinator`, `mustSetHeartbeat`) do not exist. The real ones are
   `testDB`, `seed`, `newTestCoordinator`, `newFakeDispatcher`, `waitFor`,
   `must`, `store.Get`.
5. **The reverse direction was promised in the architecture and then missing from
   the code.** It has to be in phase 1. Once phase 2 lands, `AgentReconnected`
   reaps a `starting` row (the agent already holds it — it inserts at
   `session_start` ack, before reporting running) while the hoisted agent keeps
   the pipeline alive forever. The runner's idle reaper bounds that for ordinary
   sessions but is DISABLED for console sessions, so it is an unbounded orphaned
   container on any console host.

---

## Contract note (do first, no code)

`protocol/agent-api.md` contradicts itself: `:187` says "disconnect = `offline` and its sessions are reaped", while `:1700` §Reconnection specifies reconciliation against `heartbeat.running_sessions`. This plan implements `:1700`. Land a no-wire-change clarification note in `quasar-protocol` saying `:187`'s "reaped" means "after the heartbeat-miss grace", so the next reader does not resolve the contradiction the other way. Flag to the operator; do not silently reinterpret.

---

## Phase 1 — Control plane: reconcile instead of reap

### Task 1: A store method that reaps everything except `running`

**Files:**
- Modify: `control-plane/internal/session/store.go` (beside `ReapHost`, ~line 735)
- Test: `control-plane/internal/session/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestReapHostExceptRunning: a reconnecting agent's in-flight rows are stale
// (their driving goroutine died with the old process) but a `running` row is
// the agent's own report and must survive for the heartbeat to reconcile.
func TestReapHostExceptRunning(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	running := mustSession(t, store, s, StateRunning)
	starting := mustSession(t, store, s, StateStarting)

	n, err := store.ReapHostExceptRunning(ctx, s.hostID, "agent reconnected")
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d rows, want 1 (the starting row only)", n)
	}
	if got := mustState(t, store, running); got != StateRunning {
		t.Errorf("running session = %s, want it preserved as running", got)
	}
	if got := mustState(t, store, starting); got != StateFailed {
		t.Errorf("starting session = %s, want failed", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `make test-db 2>&1 | grep -E "ReapHostExceptRunning|FAIL"`
Expected: compile failure, `store.ReapHostExceptRunning undefined`.

- [ ] **Step 3: Implement**

```go
// ReapHostExceptRunning fails a host's non-terminal sessions EXCEPT `running`
// ones (#128). A `running` row is the agent's own report (state.go: a row only
// reaches running after the agent said so), so on reconnect it is reconciled
// against heartbeat.running_sessions rather than assumed dead. Everything else
// non-terminal was mid-flight in a goroutine that died with the old connection
// and has no owner to finish it.
func (s *Store) ReapHostExceptRunning(ctx context.Context, hostID, reason string) (int64, error) {
	if !isValidUUID(hostID) {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET
		    state         = 'failed',
		    state_detail  = 'host_lost',
		    error_message = $2,
		    ended_at      = now()
		WHERE host_id = $1::uuid
		  AND state NOT IN ('stopped','failed','running')
	`, hostID, reason)
	if err != nil {
		return 0, fmt.Errorf("reap host sessions except running: %w", err)
	}
	return tag.RowsAffected(), nil
}
```

- [ ] **Step 4: Run and watch it pass**

Run: `make test-db 2>&1 | tail -5` → `RESULT status=ok`.

- [ ] **Step 5: Commit**

```bash
git add control-plane/internal/session/store.go control-plane/internal/session/lifecycle_test.go
git commit -m "feat(control-plane): reap a host's non-running sessions only (#128)"
```

### Task 2: `AgentHeartbeat` on the Events interface

**Files:**
- Modify: `control-plane/internal/agentws/events.go`
- Modify: `control-plane/internal/agentws/handler.go` (heartbeat case, ~line 456)

- [ ] **Step 1: Extend the interface and the no-op**

```go
	// AgentHeartbeat carries the agent's authoritative list of the sessions it is
	// actually running (agent-api.md §Reconnection). The coordinator fails
	// `running` rows the agent does not list and stops sessions the agent runs
	// that the control plane has lost. Fire-and-forget, like AgentMetrics.
	AgentHeartbeat(ctx context.Context, hostID string, running []string)
```

```go
func (noopEvents) AgentHeartbeat(context.Context, string, []string) {}
```

- [ ] **Step 2: Call it from the heartbeat case**

In `handler.go`, immediately after the existing `updateHeartbeat` block and before the vram enqueue:

```go
			// #128: the agent's own list is ground truth for this host. Same
			// connection-lifetime ctx + deadline as the heartbeat write above, so a
			// stalled store drops the connection rather than parking the read loop.
			rcCtx, rcCancel := context.WithTimeout(bg, agentDBCallTimeout)
			h.events.AgentHeartbeat(rcCtx, hostID, hb.RunningSessions)
			rcCancel()
```

- [ ] **Step 3: Build**

Run: `make test-go 2>&1 | tail -5`
Expected: PASS (the coordinator does not implement it yet — add the method in Task 3 in the same commit if the build breaks; `session.Coordinator` must satisfy `agentws.Events`).

- [ ] **Step 4: Commit** (with Task 3, so the tree always builds)

### Task 3: Coordinator reconciles on heartbeat

**Files:**
- Modify: `control-plane/internal/session/coordinator.go`
- Test: `control-plane/internal/session/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// TestAgentHeartbeatFailsUnlistedRunningSessions (#128): the agent's list is
// ground truth. A running row it does not name is gone.
func TestAgentHeartbeatFailsUnlistedRunningSessions(t *testing.T) {
	pool := testDB(t)
	coord, s := newCoordinator(t, pool)
	ctx := context.Background()
	kept := mustSession(t, coord.store, s, StateRunning)
	gone := mustSession(t, coord.store, s, StateRunning)

	coord.AgentHeartbeat(ctx, s.hostID, []string{kept})

	if got := mustState(t, coord.store, kept); got != StateRunning {
		t.Errorf("listed session = %s, want running", got)
	}
	if got := mustState(t, coord.store, gone); got != StateFailed {
		t.Errorf("unlisted session = %s, want failed", got)
	}
}

// TestAgentHeartbeatPreservesAcrossReconnect (#128): the reported case. A
// session running before the control plane restarted is still running after the
// agent reconnects and names it.
func TestAgentHeartbeatPreservesAcrossReconnect(t *testing.T) {
	pool := testDB(t)
	coord, s := newCoordinator(t, pool)
	ctx := context.Background()
	sid := mustSession(t, coord.store, s, StateRunning)

	coord.AgentReconnected(ctx, s.hostID)
	if got := mustState(t, coord.store, sid); got != StateRunning {
		t.Fatalf("after reconnect = %s, want running (reconnect must not reap it)", got)
	}
	coord.AgentHeartbeat(ctx, s.hostID, []string{sid})
	if got := mustState(t, coord.store, sid); got != StateRunning {
		t.Errorf("after heartbeat = %s, want running", got)
	}
}
```

- [ ] **Step 2: Run and watch both fail**

Run: `make test-db 2>&1 | grep -E "AgentHeartbeat|FAIL"`
Expected: `coord.AgentHeartbeat undefined`, and once defined, `TestAgentHeartbeatPreservesAcrossReconnect` fails because `AgentReconnected` still reaps.

- [ ] **Step 3: Implement**

```go
// AgentHeartbeat reconciles this host against the agent's own list of running
// sessions (#128, agent-api.md §Reconnection). It is the reverse-direction half
// of the grace window: the agent may keep sessions alive across a control-plane
// restart, so the control plane must learn which survived rather than assume
// none did.
//
// Only `running` rows are judged. A row reaches running only after the agent
// reported it (state.go), so a running row the agent no longer names is
// genuinely gone. Rows in flight (assigned/starting) are owned by a launch
// goroutine and are NOT this function's business.
func (c *Coordinator) AgentHeartbeat(ctx context.Context, hostID string, running []string) {
	live := make(map[string]struct{}, len(running))
	for _, id := range running {
		live[id] = struct{}{}
	}
	rows, err := c.store.RunningSessionIDsOnHost(ctx, hostID)
	if err != nil {
		c.log.Warn("heartbeat reconcile: list running sessions failed", "host_id", hostID, "err", err)
		return
	}
	for _, sid := range rows {
		if _, ok := live[sid]; ok {
			continue
		}
		detail := "host_lost"
		c.failSessionWithDetail(sid, "agent no longer running this session", &detail)
	}
}
```

Add the store read beside `NonTerminalSessionIDsOnHost`:

```go
// RunningSessionIDsOnHost lists only `running` rows, the set the agent's
// heartbeat is authoritative over (#128).
func (s *Store) RunningSessionIDsOnHost(ctx context.Context, hostID string) ([]string, error) {
	if !isValidUUID(hostID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text FROM sessions WHERE host_id = $1::uuid AND state = 'running'`, hostID)
	if err != nil {
		return nil, fmt.Errorf("list running sessions on host: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list running sessions on host: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Switch `AgentReconnected` to the non-running reap**

In `host_lifecycle.go`, replace `c.store.ReapHost(ctx, hostID, "agent reconnected; prior sessions not recovered")` with:

```go
	n, err := c.store.ReapHostExceptRunning(ctx, hostID, "agent reconnected; in-flight launch not recovered")
```

and replace the stale doc comment above `AgentReconnected` with:

```go
// AgentReconnected reconciles a host whose agent connected fresh. It fails the
// rows whose driving goroutine died with the old connection (assigned,
// starting, stopping) and DELIBERATELY LEAVES `running` rows alone: since #128
// the agent may have held those sessions across the outage, and its first
// heartbeat (AgentHeartbeat, ~5 s later) is what decides which survived. Reaping
// them here would defeat the whole grace window.
```

Also change `NonTerminalSessionIDsOnHost` usage in that function to only forget the ids it actually reaped, so a surviving session keeps its in-memory health/display/swapper state. Add:

```go
// ReapedNonRunningSessionIDsOnHost lists the ids ReapHostExceptRunning would
// fail, captured before the bulk UPDATE so their in-memory state can be dropped.
func (s *Store) ReapedNonRunningSessionIDsOnHost(ctx context.Context, hostID string) ([]string, error) {
	if !isValidUUID(hostID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text FROM sessions WHERE host_id = $1::uuid AND state NOT IN ('stopped','failed','running')`, hostID)
	if err != nil {
		return nil, fmt.Errorf("list reapable sessions on host: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list reapable sessions on host: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Run the full DB suite**

Run: `make test-db 2>&1 | tail -5` → `RESULT status=ok`.
Expect `TestCoordinatorHostDisconnected` and friends to need updating in Task 4.

- [ ] **Step 6: Commit**

```bash
git add control-plane/internal/{agentws,session}
git commit -m "feat(control-plane): reconcile sessions from the agent heartbeat (#128)"
```

### Task 4: `HostDisconnected` stops reaping; the sweep becomes the backstop

**Files:**
- Modify: `control-plane/internal/session/host_lifecycle.go`
- Modify: `control-plane/internal/session/lifecycle_test.go:535` (`TestCoordinatorHostDisconnected`)

- [ ] **Step 1: Rewrite the existing test to the new contract**

```go
// TestCoordinatorHostDisconnected (#128): a disconnect no longer reaps a running
// session. The agent may be holding it across a brief outage; the stale-host
// sweep is what reaps if the host never returns.
func TestCoordinatorHostDisconnected(t *testing.T) {
	pool := testDB(t)
	coord, s := newCoordinator(t, pool)
	ctx := context.Background()
	sid := mustSession(t, coord.store, s, StateRunning)

	coord.HostDisconnected(ctx, s.hostID)

	if got := mustState(t, coord.store, sid); got != StateRunning {
		t.Errorf("after disconnect = %s, want running (held for the grace window)", got)
	}
}
```

- [ ] **Step 2: Run and watch it fail** — `make test-db`; the row is `failed`.

- [ ] **Step 3: Implement**: in `HostDisconnected`, replace the `ReapHost` call with `ReapHostExceptRunning` using reason `"host agent connection lost"`, and update the doc comment to say the running rows are held for the sweep.

- [ ] **Step 4: Run** — `make test-db` → ok.

- [ ] **Step 5: Commit**

### Task 5: The boot-aware stale-host sweep

**Files:**
- Create: `control-plane/internal/session/stale_sweep.go`
- Test: `control-plane/internal/session/stale_sweep_db_test.go`
- Modify: `control-plane/internal/config/config.go` (add `SessionGraceSecs`)
- Modify: `control-plane/cmd/quasar-control/app.go` (start the ticker)

- [ ] **Step 1: Write the failing test**

```go
// TestStaleSweepIgnoresRecentBoot (#128): the sweep must measure from
// max(last_heartbeat_at, control-plane boot) or it reaps every session at
// startup, before any agent has had a chance to reconnect.
func TestStaleSweepIgnoresRecentBoot(t *testing.T) {
	pool := testDB(t)
	coord, s := newCoordinator(t, pool)
	ctx := context.Background()
	sid := mustSession(t, coord.store, s, StateRunning)
	mustSetHeartbeat(t, pool, s.hostID, time.Now().Add(-10*time.Minute))

	// Booted one second ago: nothing has had time to reconnect.
	coord.sweepStaleHosts(ctx, time.Now().Add(-time.Second), 2*time.Minute)

	if got := mustState(t, coord.store, sid); got != StateRunning {
		t.Errorf("swept at boot = %s, want running (grace runs from boot)", got)
	}

	// Booted an hour ago: the grace has genuinely elapsed.
	coord.sweepStaleHosts(ctx, time.Now().Add(-time.Hour), 2*time.Minute)

	if got := mustState(t, coord.store, sid); got != StateFailed {
		t.Errorf("swept after grace = %s, want failed", got)
	}
}
```

- [ ] **Step 2: Run and watch it fail** — `coord.sweepStaleHosts undefined`.

- [ ] **Step 3: Implement**

```go
// sweepStaleHosts fails running sessions on hosts whose agent is neither
// connected nor recently heard from (#128).
//
// The deadline is measured from max(last_heartbeat_at, bootedAt). Without the
// boot term a control plane that restarts after a long quiet period reaps every
// session the instant it starts, before any agent's ~2 s reconnect can land —
// which is the very failure this issue is about, reintroduced from the other
// side.
func (c *Coordinator) sweepStaleHosts(ctx context.Context, bootedAt time.Time, grace time.Duration) {
	hosts, err := c.store.HostsWithRunningSessions(ctx)
	if err != nil {
		c.log.Warn("stale sweep: host list failed", "err", err)
		return
	}
	for _, h := range hosts {
		if c.agents.IsConnected(h.ID) {
			continue
		}
		last := h.LastHeartbeatAt
		if last.Before(bootedAt) {
			last = bootedAt
		}
		if time.Since(last) < grace {
			continue
		}
		ids, err := c.store.RunningSessionIDsOnHost(ctx, h.ID)
		if err != nil {
			c.log.Warn("stale sweep: list failed", "host_id", h.ID, "err", err)
			continue
		}
		for _, sid := range ids {
			detail := "host_lost"
			c.failSessionWithDetail(sid, "host agent did not return within the grace window", &detail)
		}
	}
}
```

- [ ] **Step 4: Run** — `make test-db` → ok.
- [ ] **Step 5: Wire the ticker** in `app.go`, interval `grace/4`, and add `QUASAR_SESSION_GRACE_SECS` (default 120) to config + `docs/configuration.md`.
- [ ] **Step 6: Commit**

### Task 6: Phase 1 verification

- [ ] `make test-db` → `RESULT status=ok`
- [ ] `make test-go`, `make verify`
- [ ] Fable review of the phase-1 diff
- [ ] Deploy to Hermes and confirm no behaviour change with today's agent: a session ends ~5 s after reconnect rather than at reconnect, and nothing is orphaned.

---

## Phase 2 — Agent: hold sessions across a disconnect

### Task 7: Hoist session state above the reconnect loop

**Files:**
- Modify: `node-agent/src/agent.rs` (`run` ~line 250, `connect_and_run` ~line 855, `SessionManager::new` ~line 1066, `struct SessionManager` ~line 1672, `impl Drop` ~line 2803)

The seam: `RunningHandle` holds only std-mpsc senders and a stop flag, so it is connection-agnostic. What must move up with `running`:

- `running: HashMap<String, RunningHandle>`
- the event channels `evt_tx`/`evt_rx` and `diagnostic_tx` — runner threads capture CLONES at spawn (`:2187`), so hoisting the map without the channels sends a survivor's terminal events into a dead channel and the slot is later reaped with a wrong "runner thread ended without reporting" state
- `live_refs` (`:1063`) — otherwise the next connection's GC job sees an empty set and can reap a home a live session is mounting

What must NOT survive: `pending` (clear on disconnect; the control plane re-drives assigns), `source_policy` (its ConnectionGuard invalidates by design), `warmup_*`, `readiness`, `host_codec_report`, `gpu_inventory`.

- [ ] **Step 1:** Introduce `struct SessionState { running, evt_tx, evt_rx, diagnostic_tx, live_refs }` owned by `run()`, passed as `&mut` into `connect_and_run`.
- [ ] **Step 2:** `SessionManager::new` takes `&mut SessionState`. Update all 9 call sites (`:3642` helper, `:3807`, `:3838`, `:3897`, `:3975`, `:4444`, `:4495`).
- [ ] **Step 3:** `make test-rust` — expect green with no behaviour change yet (`Drop` still stops everything).
- [ ] **Step 4:** Commit.

### Task 8: Replace `Drop` with an explicit bounded grace

- [ ] **Step 1: Write the failing test** for a pure decision function:

```rust
#[test]
fn sessions_are_held_until_the_grace_window_expires() {
    let start = Instant::now();
    assert_eq!(disconnect_action(start, start + Duration::from_secs(30), Duration::from_secs(90)), DisconnectAction::Hold);
    assert_eq!(disconnect_action(start, start + Duration::from_secs(91), Duration::from_secs(90)), DisconnectAction::StopAll);
}
```

- [ ] **Step 2:** Run — fails, `disconnect_action` undefined.
- [ ] **Step 3:** Implement `disconnect_action`, delete `impl Drop for SessionManager`, and in `run()`'s `Err` arm record `disconnected_at` and call `stop_all` only once the window expires. Clear `pending`. Knob `QUASAR_SESSION_GRACE_SECS`, default 90 (covers the measured ~70 s recreate).
- [ ] **Step 4:** Run `make test-rust` → green.
- [ ] **Step 5:** Drain `evt_rx` while disconnected so a runner's `blocking_send` on the 256-capacity channel cannot park its thread.
- [ ] **Step 6:** Commit.

### Task 9: Phase 2 verification
- [ ] `make test-rust` (fmt, clippy -D warnings, full suite)
- [ ] Fable review
- [ ] Deploy to Hermes; restart the control plane with a session running and confirm the agent holds it.

---

## Phase 3 — Web: retry the token mint

### Task 10: Bounded retry with backoff

**Files:**
- Modify: `web/src/pages/app/sessionRuntime.ts:466-505` (`handleRecovery` / `mintInFlight` — the mint-once path)
- Modify: `web/src/webrtc/session.ts:189-200` (terminal classification)
- Test: `web/src/pages/app/sessionRuntime.test.ts`

- [ ] **Step 1: Write the failing test** — a mint that fails twice then succeeds must recover the session, not end it.
- [ ] **Step 2:** Run — fails (one attempt only).
- [ ] **Step 3:** Implement bounded retry (e.g. 6 attempts, exponential 1→16 s, ~90 s total to match the agent grace), keeping the peer connection alive across attempts. `RecoveryController` already preserves the PC/DC across ICE restarts; it is the `terminal` → mint-once path that gives up.
- [ ] **Step 4:** Also back off on repeated 4500 `ErrAgentNotConnected`, the reverse ordering where the browser re-attaches before the agent re-registers.
- [ ] **Step 5:** `make test-web` → green. Commit.

### Task 11: Phase 3 verification
- [ ] `make test-web`
- [ ] Fable review

---

## Phase 4 — Live gate (this is also #119's blocking criterion)

- [ ] Launch a session on Hermes.
- [ ] `docker compose up -d --force-recreate --no-deps quasar-control-plane` (~60–90 s).
- [ ] Confirm: the `<video>` keeps decoding throughout, the session row stays `running`, and apply history is complete.
- [ ] Confirm the agent reconnect after a window EXPIRY still cleans up (no orphaned containers).
- [ ] Only then correct `deploy/redeploy.sh`'s header back to claiming survival.
- [ ] Record the evidence report under `docs/reports/`.
