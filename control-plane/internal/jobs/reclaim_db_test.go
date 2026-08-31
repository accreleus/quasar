// reclaim_db_test.go — #492: an agent-plane run orphaned by an agent restart
// must not wedge its job on that host forever.
//
// The failure this closes was found live: a `template.warmup` run left `running`
// when the node-agent container was recreated mid-run, after which every
// POST /v1/admin/jobs/template.warmup/run answered 409 job_already_running,
// because job_runs_open_per_target correctly refuses a second open run.
//
// TEST_DATABASE_URL-gated; `make test-db` runs it.
package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// eventHostDef is `template.warmup`'s shape: agent plane, host scope, EVENT
// schedule. The event kind matters — an interval job at least re-materializes
// after the reaper, while an event job's only route back is an admin trigger,
// which is exactly the 409 an operator hit.
func eventHostDef(id string) Definition {
	return Definition{
		ID: id, Name: id, Plane: PlaneAgent, Scope: ScopeHost, Managed: true,
		Default: Schedule{Kind: KindEvent, Timezone: "UTC"},
	}
}

// claimForHost puts one pending run of def into `running` the way the agent pull
// channel does (GET /v1/agent/jobs/pending -> Store.ClaimDue).
func claimForHost(t *testing.T, store *Store, jobID, hostID string) Run {
	t.Helper()
	claimed, err := store.ClaimDue(context.Background(), ClaimOptions{
		Plane: PlaneAgent, HostID: hostID, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("agent claim: %v", err)
	}
	for _, r := range claimed {
		if r.JobID == jobID {
			return r
		}
	}
	t.Fatalf("no run of %s was claimed for host %s (claimed %+v)", jobID, hostID, claimed)
	return Run{}
}

// TestOrphanedAgentRunBlocksRunNowUntilAgentReRegisters is the whole of #492 in
// one pass: the wedge, then the eager reclaim that clears it.
func TestOrphanedAgentRunBlocksRunNowUntilAgentReRegisters(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")

	def := eventHostDef("template.warmup")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := seed(t, pool, def)
	d := New(store, reg, DefaultConfig(), quietLog())

	// The event trigger materialized a run and the host claimed it...
	if _, err := d.Enqueue(ctx, def.ID, host, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	orphan := claimForHost(t, store, def.ID, host)

	// ...and then the agent container was recreated. Nothing will ever report
	// this run, and the single-flight index turns that into a permanent 409.
	if _, err := d.RunNow(ctx, def.ID, host, ""); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("RunNow on an orphaned run = %v, want ErrAlreadyRunning (the 409)", err)
	}

	// The agent re-registers. AgentReconnected calls exactly this, with its own
	// reason literal (session.jobReclaimReason) — spelled out here rather than
	// referenced so the stored text is asserted, not tautologically compared.
	n, err := d.ReclaimHostRuns(ctx, host, "agent restarted")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d runs, want 1", n)
	}

	closed, err := store.GetRun(ctx, orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != StateAborted {
		t.Fatalf("orphan state = %q, want %q", closed.State, StateAborted)
	}
	if closed.FinishedAt == nil {
		t.Fatal("a closed run must carry finished_at")
	}
	if closed.Error != "agent restarted" {
		t.Fatalf("orphan error = %q, want %q", closed.Error, "agent restarted")
	}

	// And the job is runnable again — the point of the whole exercise.
	fresh, err := d.RunNow(ctx, def.ID, host, "")
	if err != nil {
		t.Fatalf("RunNow after reclaim: %v", err)
	}
	if fresh.State != StatePending {
		t.Fatalf("fresh run state = %q, want pending", fresh.State)
	}
	if fresh.ID == orphan.ID {
		t.Fatal("RunNow returned the orphaned run, not a fresh one")
	}
}

// TestReclaimTouchesOnlyThisHostsAgentRuns guards the three rows the reclaim
// must NOT take: another host's run, a pending run (its work has not started and
// the reconnecting agent will claim it), and a CONTROL-plane run for this host —
// which executes in the control-plane process and is entirely unaffected by an
// agent restarting.
func TestReclaimTouchesOnlyThisHostsAgentRuns(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostA := newHost(t, pool, "host-a")
	hostB := newHost(t, pool, "host-b")

	agentJob := hostDef("home.gc")
	otherJob := eventHostDef("template.warmup")
	controlJob := Definition{
		ID: "console.selfheal", Name: "console.selfheal",
		Plane: PlaneControl, Scope: ScopeHost, Managed: true,
		Default: Schedule{Kind: KindEvent, Timezone: "UTC"},
		// Never invoked: the test claims this run itself and then asserts the
		// reclaim left it alone. A managed control-plane job must carry a Run func
		// to register at all.
		Run: func(context.Context, RunContext) (Outcome, error) { return Succeeded(nil), nil },
	}
	reg := NewRegistry()
	for _, def := range []Definition{agentJob, otherJob, controlJob} {
		if err := reg.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	store := seed(t, pool, agentJob, otherJob, controlJob)
	d := New(store, reg, DefaultConfig(), quietLog())

	// hostA: one claimed agent run (must be reclaimed) and one pending one.
	if _, err := d.Enqueue(ctx, agentJob.ID, hostA, nil); err != nil {
		t.Fatal(err)
	}
	hostAClaimed := claimForHost(t, store, agentJob.ID, hostA)
	hostAPending, err := d.Enqueue(ctx, otherJob.ID, hostA, nil)
	if err != nil {
		t.Fatal(err)
	}
	// hostA: a control-plane run, claimed by the control plane itself.
	if _, err := d.Enqueue(ctx, controlJob.ID, hostA, nil); err != nil {
		t.Fatal(err)
	}
	controlClaimed, err := store.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: time.Now()})
	if err != nil || len(controlClaimed) != 1 {
		t.Fatalf("control claim: %v %+v", err, controlClaimed)
	}
	// hostB: a claimed agent run of the same job.
	if _, err := d.Enqueue(ctx, agentJob.ID, hostB, nil); err != nil {
		t.Fatal(err)
	}
	hostBClaimed := claimForHost(t, store, agentJob.ID, hostB)

	n, err := d.ReclaimHostRuns(ctx, hostA, DefaultReclaimReason)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d runs, want 1 (only hostA's claimed agent run)", n)
	}

	for _, want := range []struct {
		what  string
		id    string
		state State
	}{
		{"hostA's claimed agent run", hostAClaimed.ID, StateAborted},
		{"hostA's pending agent run", hostAPending.ID, StatePending},
		{"hostA's claimed CONTROL run", controlClaimed[0].ID, StateRunning},
		{"hostB's claimed agent run", hostBClaimed.ID, StateRunning},
	} {
		got, err := store.GetRun(ctx, want.id)
		if err != nil {
			t.Fatalf("%s: %v", want.what, err)
		}
		if got.State != want.state {
			t.Errorf("%s: state = %q, want %q", want.what, got.State, want.state)
		}
	}
}

// TestReportLosingToReclaimIsLoud covers the residual race the agent-side abort
// flag narrows but cannot close: a blocking pass that was already in flight
// finishes, its report arrives after the reclaim, and Store.Report discards it
// because the row is no longer `running`.
//
// Silently discarding a real verdict is the thing to prevent. A run that
// SUCCEEDED being recorded `aborted` is a lie in the operator's history, and a
// dropped `deferred` costs the follow-up retry row too — scheduleDeferral only
// fires on an applied report, so the persisted backoff ladder never advances.
func TestReportLosingToReclaimIsLoud(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")

	def := eventHostDef("template.warmup")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := seed(t, pool, def)
	var logs bytes.Buffer
	d := New(store, reg, DefaultConfig(),
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if _, err := d.Enqueue(ctx, def.ID, host, nil); err != nil {
		t.Fatal(err)
	}
	orphan := claimForHost(t, store, def.ID, host)
	if _, err := d.ReclaimHostRuns(ctx, host, "agent restarted"); err != nil {
		t.Fatal(err)
	}

	// The late report. It must not error — the agent has nothing useful to do
	// with a failure here, exactly as the idempotent-retry case.
	run, err := d.Report(ctx, orphan.ID, StateDeferred, Summary{"reason": "host has 1 live session(s)"}, "")
	if err != nil {
		t.Fatalf("late report: %v", err)
	}
	if run.State != StateAborted {
		t.Fatalf("the late report overwrote the reclaim: state = %q", run.State)
	}

	got := logs.String()
	for _, want := range []string{
		"job: report lost to reclaim",
		"reported_state=deferred",
		"deferral_dropped=true",
		orphan.ID,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the discarded report was not logged with %q:\n%s", want, got)
		}
	}

	// And the dropped deferral really did cost the follow-up row — which is what
	// the log line has to make findable.
	if _, found, err := store.OpenRun(ctx, def.ID, host); err != nil || found {
		t.Fatalf("a discarded deferral must not schedule a retry: found=%v err=%v", found, err)
	}
}

// TestIdempotentReportRetryStaysQuiet is the other half: an agent retrying a
// report it already landed is ordinary, and must not produce the warning above —
// or the signal would be worthless.
func TestIdempotentReportRetryStaysQuiet(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")

	def := eventHostDef("template.warmup")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := seed(t, pool, def)
	var logs bytes.Buffer
	d := New(store, reg, DefaultConfig(),
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if _, err := d.Enqueue(ctx, def.ID, host, nil); err != nil {
		t.Fatal(err)
	}
	run := claimForHost(t, store, def.ID, host)
	if _, err := d.Report(ctx, run.ID, StateSucceeded, Summary{"built": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Report(ctx, run.ID, StateSucceeded, Summary{"built": 1}, ""); err != nil {
		t.Fatalf("retry of a landed report: %v", err)
	}

	if strings.Contains(logs.String(), "report lost to reclaim") {
		t.Fatalf("an idempotent retry was reported as a lost race:\n%s", logs.String())
	}
}

// TestReclaimRequiresAHost: a blank host id would otherwise match every
// instance-scoped run's NULL host_id under a careless cast.
func TestReclaimRequiresAHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	if _, err := store.ReclaimHostRuns(context.Background(), "  ", ""); !errors.Is(err, ErrHostRequired) {
		t.Fatalf("reclaim with no host = %v, want ErrHostRequired", err)
	}
}

// TestReaperClosesAnOverTimeoutAgentRun is fix candidate 1's verification: the
// claim-timeout reaper is wired into the tick and DOES cover an agent-plane,
// event-scheduled run claimed over the agent pull channel — i.e. it bounds the
// wedge even for a host that never comes back. The eager reclaim above shortens
// that bound to zero; it does not replace it.
func TestReaperClosesAnOverTimeoutAgentRun(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")

	def := eventHostDef("template.warmup")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := seed(t, pool, def)
	cfg := DefaultConfig()
	cfg.ClaimTimeout = 60 * time.Second
	d := New(store, reg, cfg, quietLog())

	if _, err := d.Enqueue(ctx, def.ID, host, nil); err != nil {
		t.Fatal(err)
	}
	orphan := claimForHost(t, store, def.ID, host)
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET claimed_at = now() - interval '10 minutes' WHERE id = $1::uuid`,
		orphan.ID); err != nil {
		t.Fatal(err)
	}

	d.Tick(ctx)
	d.Wait()

	closed, err := store.GetRun(ctx, orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != StateAborted {
		t.Fatalf("the reaper left an over-timeout agent run %q", closed.State)
	}
	// An event-kind job is not re-materialized (nothing happened to trigger it),
	// so the single-flight slot must now be FREE rather than re-taken.
	if _, err := d.RunNow(ctx, def.ID, host, ""); err != nil {
		t.Fatalf("RunNow after the reap: %v", err)
	}
}
