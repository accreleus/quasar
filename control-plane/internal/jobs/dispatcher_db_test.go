// dispatcher_db_test.go — the tick, end to end, against Postgres.
// TEST_DATABASE_URL-gated; `make test-db` runs it.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedClock lets a test stand at a chosen instant, which is the only way to
// assert window snapping without waiting for 02:00.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newDispatcher(t *testing.T, pool *pgxpool.Pool, reg *Registry, defs ...Definition) (*Dispatcher, *Store) {
	t.Helper()
	store := NewStore(pool)
	if _, err := store.SyncDefinitions(context.Background(), defs, "UTC", 50); err != nil {
		t.Fatalf("sync: %v", err)
	}
	d := New(store, reg, DefaultConfig(), quietLog())
	return d, store
}

// TestEmptyRegistryTicksWithoutSideEffects is WP1's headline claim: merging the
// framework changes ZERO behaviour. With nothing registered the dispatcher
// materializes nothing, claims nothing and writes no row — every existing
// janitor keeps its own ticker, untouched.
func TestEmptyRegistryTicksWithoutSideEffects(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	reg := NewRegistry()
	d, _ := newDispatcher(t, pool, reg)

	for i := 0; i < 3; i++ {
		d.Tick(ctx)
	}
	d.Wait()

	var jobsN, runsN int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM job_runs)`).
		Scan(&jobsN, &runsN); err != nil {
		t.Fatal(err)
	}
	if jobsN != 0 || runsN != 0 {
		t.Fatalf("an empty registry produced %d jobs and %d runs", jobsN, runsN)
	}
}

func TestTickMaterializesClaimsExecutesAndRecords(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	var calls int
	var mu sync.Mutex
	def := intervalDef("artwork.sweep")
	def.Run = func(context.Context, RunContext) (Outcome, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return Succeeded(Summary{"apps_considered": 412, "artwork_resolved": 3}), nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)

	d.Tick(ctx)
	d.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("RunFunc called %d times, want 1", got)
	}
	last, found, err := store.LastTerminalRun(ctx, "artwork.sweep", "")
	if err != nil || !found {
		t.Fatalf("no run recorded: found=%v err=%v", found, err)
	}
	if last.State != StateSucceeded {
		t.Fatalf("state: %s", last.State)
	}
	if last.StartedAt == nil || last.FinishedAt == nil || last.DurationMS() == nil {
		t.Fatalf("timings missing: %+v", last)
	}
	if string(last.Summary) == "{}" {
		t.Fatal("summary was not recorded")
	}

	// The next tick must NOT run it again: the interval is measured from the end
	// of the previous pass, and 15 minutes have not elapsed.
	d.Tick(ctx)
	d.Wait()
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("RunFunc called %d times after a second tick, want 1", got)
	}
	open, found, err := store.OpenRun(ctx, "artwork.sweep", "")
	if err != nil || !found {
		t.Fatalf("no next run was materialized: found=%v err=%v", found, err)
	}
	if open.State != StatePending {
		t.Fatalf("next run state: %s", open.State)
	}
	if wait := open.ScheduledFor.Sub(*last.FinishedAt); wait < 14*time.Minute || wait > 16*time.Minute {
		t.Fatalf("next run is %s after the previous one finished, want ~15m", wait)
	}
}

// TestPanicInAJobIsRecordedAndTheDispatcherSurvives. Every current janitor
// swallows its errors and continues; that is preserved, but now it is RECORDED —
// and one bad sweep must never take the scheduler, and therefore every other
// job in the instance, down with it.
func TestPanicInAJobIsRecordedAndTheDispatcherSurvives(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	bad := intervalDef("bad.job")
	bad.Run = func(context.Context, RunContext) (Outcome, error) { panic("kaboom") }
	good := intervalDef("good.job")
	var ran bool
	var mu sync.Mutex
	good.Run = func(context.Context, RunContext) (Outcome, error) {
		mu.Lock()
		ran = true
		mu.Unlock()
		return Succeeded(nil), nil
	}
	reg := NewRegistry()
	for _, d := range []Definition{bad, good} {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	d, store := newDispatcher(t, pool, reg, bad, good)

	d.Tick(ctx)
	d.Wait()

	last, found, err := store.LastTerminalRun(ctx, "bad.job", "")
	if err != nil || !found {
		t.Fatalf("the panicking run left no record: found=%v err=%v", found, err)
	}
	if last.State != StateFailed {
		t.Fatalf("state: %s want failed", last.State)
	}
	if last.Error == "" || !strings.Contains(last.Error, "kaboom") {
		t.Fatalf("the panic message was not recorded: %q", last.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Fatal("a panicking job stopped its neighbour from running")
	}
}

// TestDeferralBackoffIsPersisted is the property the in-memory ladders this
// framework replaces could never offer: the refusal, its reason and its retry
// time are rows, so they survive a reconnect and a restart and an operator can
// see them.
func TestDeferralBackoffIsPersisted(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := intervalDef("template.warmup")
	def.Run = func(context.Context, RunContext) (Outcome, error) {
		return Deferred("host has 1 live session(s)"), nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)
	clock := &fixedClock{t: time.Now()}
	d.now = clock.now

	d.Tick(ctx)
	d.Wait()

	last, found, err := store.LastTerminalRun(ctx, "template.warmup", "")
	if err != nil || !found {
		t.Fatalf("no run recorded: %v", err)
	}
	if last.State != StateDeferred {
		t.Fatalf("state: %s want deferred", last.State)
	}
	if r := summaryReason(last.Summary); r != "host has 1 live session(s)" {
		t.Fatalf("reason not recorded: %q", r)
	}

	next, found, err := store.OpenRun(ctx, "template.warmup", "")
	if err != nil || !found {
		t.Fatalf("no retry was scheduled: found=%v err=%v", found, err)
	}
	if next.Attempt != 2 {
		t.Fatalf("attempt: %d want 2", next.Attempt)
	}
	if delay := next.ScheduledFor.Sub(clock.now()); delay < 25*time.Second || delay > 35*time.Second {
		t.Fatalf("retry in %s, want the 30s first rung", delay)
	}

	// A SECOND deferral doubles it, and the ladder is read back from the row —
	// which is exactly what surviving a restart means.
	clock.set(clock.now().Add(time.Minute))
	d.Tick(ctx)
	d.Wait()
	next, found, err = store.OpenRun(ctx, "template.warmup", "")
	if err != nil || !found {
		t.Fatalf("no second retry: %v", err)
	}
	if next.Attempt != 3 {
		t.Fatalf("attempt: %d want 3", next.Attempt)
	}
	if delay := next.ScheduledFor.Sub(clock.now()); delay < 55*time.Second || delay > 65*time.Second {
		t.Fatalf("second retry in %s, want the 60s rung", delay)
	}
}

// TestScheduledRunAfterASuccessResetsTheAttemptLadder.
func TestScheduledRunAfterASuccessResetsTheAttemptLadder(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	var refuse = true
	var mu sync.Mutex
	def := intervalDef("a.one")
	def.Run = func(context.Context, RunContext) (Outcome, error) {
		mu.Lock()
		defer mu.Unlock()
		if refuse {
			refuse = false
			return Deferred("busy"), nil
		}
		return Succeeded(nil), nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)
	clock := &fixedClock{t: time.Now()}
	d.now = clock.now

	d.Tick(ctx) // deferred, attempt 1 -> retry at +30s with attempt 2
	d.Wait()
	clock.set(clock.now().Add(time.Minute))
	d.Tick(ctx) // the retry succeeds
	d.Wait()

	last, _, err := store.LastTerminalRun(ctx, "a.one", "")
	if err != nil {
		t.Fatal(err)
	}
	if last.State != StateSucceeded || last.Attempt != 2 {
		t.Fatalf("retry run: state=%s attempt=%d", last.State, last.Attempt)
	}
	// One interval later the scheduled run is materialized and executed in the
	// same tick (materialize precedes execute, and it is due immediately). What
	// matters is that it carries attempt 1: a success resets the ladder.
	clock.set(clock.now().Add(20 * time.Minute))
	d.Tick(ctx)
	d.Wait()
	next, found, err := store.LastTerminalRun(ctx, "a.one", "")
	if err != nil || !found {
		t.Fatalf("no next run: found=%v err=%v", found, err)
	}
	if next.Attempt != 1 {
		t.Fatalf("a success did not reset the ladder: attempt=%d", next.Attempt)
	}
}

// TestWindowSnapsAScheduledRunForward — the A8 acceptance shape, driven from a
// fixed clock instead of waiting until 02:00.
func TestWindowSnapsAScheduledRunForward(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := intervalDef("nightly.job")
	def.Default = Schedule{Kind: KindInterval, IntervalSecs: 86400,
		WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "Europe/London"}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)
	london := mustLoc(t, "Europe/London")
	// 2026-08-12 14:00 London — the middle of the afternoon, well outside.
	clock := &fixedClock{t: time.Date(2026, 8, 12, 14, 0, 0, 0, london)}
	d.now = clock.now

	d.Tick(ctx)
	d.Wait()

	open, found, err := store.OpenRun(ctx, "nightly.job", "")
	if err != nil || !found {
		t.Fatalf("no run materialized: %v", err)
	}
	if open.State != StatePending {
		t.Fatalf("the run was claimed despite the window: %s", open.State)
	}
	want := time.Date(2026, 8, 13, 2, 0, 0, 0, london)
	if !open.ScheduledFor.Equal(want) {
		t.Fatalf("scheduled_for %s, want the next 02:00 London (%s)",
			open.ScheduledFor.In(london).Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestRunNowIgnoresTheWindowAndTheGateStillDecides — the design's central
// contract, and the A9/A10 acceptance pair.
func TestRunNowIgnoresTheWindowAndTheGateStillDecides(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := intervalDef("nightly.job")
	def.Default = Schedule{Kind: KindInterval, IntervalSecs: 86400,
		WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "Europe/London"}
	def.Run = func(context.Context, RunContext) (Outcome, error) {
		return Deferred("host has 1 live session(s)"), nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)
	london := mustLoc(t, "Europe/London")
	clock := &fixedClock{t: time.Date(2026, 8, 12, 14, 0, 0, 0, london)}
	d.now = clock.now

	run, err := d.RunNow(ctx, "nightly.job", "", "")
	if err != nil {
		t.Fatalf("run now: %v", err)
	}
	if !run.ScheduledFor.Equal(clock.now()) {
		t.Fatalf("a manual run was snapped into the window: %s", run.ScheduledFor)
	}

	d.Tick(ctx)
	d.Wait()

	last, found, err := store.LastTerminalRun(ctx, "nightly.job", "")
	if err != nil || !found {
		t.Fatalf("the manual run left no record: %v", err)
	}
	if last.State != StateDeferred {
		t.Fatalf("the gate was overridden: %s", last.State)
	}
	// The retry of a MANUAL run is not snapped into the window either: the
	// operator asked for this run now, and the framework does not overrule that.
	next, found, err := store.OpenRun(ctx, "nightly.job", "")
	if err != nil || !found {
		t.Fatalf("no retry scheduled: %v", err)
	}
	if delay := next.ScheduledFor.Sub(clock.now()); delay < 25*time.Second || delay > 35*time.Second {
		t.Fatalf("manual retry in %s, want 30s (not snapped to 02:00)", delay)
	}
}

// TestRunNowResolvesParamsForAJobThatNeedsThem closes the jobs+#488 acceptance
// defect: a manual run of an agent-plane job whose work is defined by its params
// (template.warmup) used to be materialized with an EMPTY blob, and the host
// failed it with `params incomplete` — an error about the framework rather than
// about the host. The resolver runs on the manual path so a hand-triggered run
// carries the same params an event-triggered one would.
func TestRunNowResolvesParamsForAJobThatNeedsThem(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostID := newHost(t, pool, "host-a-params")

	var sawHost string
	def := hostDef("template.warmup")
	def.Default = Schedule{Kind: KindEvent, Timezone: "UTC"}
	def.ResolveParams = func(_ context.Context, h string) (any, error) {
		sawHost = h
		return map[string]any{"image_id": "steam", "registry_ref": "ghcr.io/x@sha256:abc", "version": "1.2.3"}, nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)

	if _, err := d.RunNow(ctx, "template.warmup", hostID, ""); err != nil {
		t.Fatalf("run now: %v", err)
	}
	if sawHost != hostID {
		t.Errorf("resolver saw host %q, want %q", sawHost, hostID)
	}
	run, found, err := store.OpenRun(ctx, "template.warmup", hostID)
	if err != nil || !found {
		t.Fatalf("no run materialized: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(run.Params, &got); err != nil {
		t.Fatalf("params %q: %v", string(run.Params), err)
	}
	if got["image_id"] != "steam" || got["registry_ref"] != "ghcr.io/x@sha256:abc" || got["version"] != "1.2.3" {
		t.Errorf("stored params = %v, want the resolved image", got)
	}
}

// A resolver that cannot answer REFUSES the trigger, and the operator-readable
// cause survives the wrapping (the handler renders it verbatim).
func TestRunNowRefusesWhenParamsCannotBeResolved(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostID := newHost(t, pool, "host-a-noparams")

	def := hostDef("template.warmup")
	def.Default = Schedule{Kind: KindEvent, Timezone: "UTC"}
	def.ResolveParams = func(context.Context, string) (any, error) {
		return nil, errors.New("this host has no adopted image in the `ready` state, so there is nothing to warm up")
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)

	_, err := d.RunNow(ctx, "template.warmup", hostID, "")
	if !errors.Is(err, ErrParamsUnavailable) {
		t.Fatalf("err = %v, want ErrParamsUnavailable", err)
	}
	if !strings.Contains(err.Error(), "nothing to warm up") {
		t.Errorf("err = %q, want the resolver's reason to survive", err)
	}
	// AND no run row was left behind: a refused trigger must not queue work.
	if _, found, err := store.OpenRun(ctx, "template.warmup", hostID); err != nil || found {
		t.Errorf("a refused trigger materialized a run (found=%v err=%v)", found, err)
	}
}

// A pending EVENT run already carries the params the event built. A manual
// trigger pulls it forward and must NOT re-resolve — the resolver here would
// fail the test if it ran.
func TestRunNowKeepsAnExistingEventRunsParams(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostID := newHost(t, pool, "host-a-pullforward")

	def := hostDef("template.warmup")
	def.Default = Schedule{Kind: KindEvent, Timezone: "UTC"}
	def.ResolveParams = func(context.Context, string) (any, error) {
		return nil, errors.New("the resolver must not run when params already exist")
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)

	if _, err := d.Enqueue(ctx, "template.warmup", hostID,
		map[string]any{"image_id": "steam", "registry_ref": "ref", "version": "9"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := d.RunNow(ctx, "template.warmup", hostID, ""); err != nil {
		t.Fatalf("run now: %v", err)
	}
	run, found, err := store.OpenRun(ctx, "template.warmup", hostID)
	if err != nil || !found {
		t.Fatalf("no run: %v", err)
	}
	if run.Trigger != TriggerManual {
		t.Errorf("trigger = %s, want the run pulled forward as manual", run.Trigger)
	}
	var got map[string]any
	if err := json.Unmarshal(run.Params, &got); err != nil {
		t.Fatalf("params %q: %v", string(run.Params), err)
	}
	if got["version"] != "9" {
		t.Errorf("params = %v, want the event's params kept", got)
	}
}

func TestRunNowRefusesDisabledAndReportsAlreadyRunning(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	release := make(chan struct{})
	def := intervalDef("slow.job")
	def.Run = func(ctx context.Context, _ RunContext) (Outcome, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Succeeded(nil), nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, _ := newDispatcher(t, pool, reg, def)

	if _, err := d.RunNow(ctx, "slow.job", "", ""); err != nil {
		t.Fatalf("run now: %v", err)
	}
	d.Tick(ctx) // claims and starts it; the body blocks on release

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := d.RunNow(ctx, "slow.job", "", "")
		if errors.Is(err, ErrAlreadyRunning) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a second Run now while running returned %v, want ErrAlreadyRunning", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	d.Wait()

	// A disabled job never runs, not even manually: a kill switch a button can
	// bypass is not a kill switch.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET enabled = false WHERE id = 'slow.job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RunNow(ctx, "slow.job", "", ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("run now on a disabled job: %v", err)
	}
}

// TestEnqueueCoalescesEventsAndNeverFiresOnAClock covers the KindEvent path: a
// burst of events collapses into one run (the cap-1-channel behaviour the
// hand-rolled level-triggered workers already implement), and the dispatcher's
// clock never creates one.
func TestEnqueueCoalescesEventsAndNeverFiresOnAClock(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := Definition{
		ID: "provider.reconcile", Name: "Provider reconcile",
		Plane: PlaneControl, Scope: ScopeInstance, Managed: true,
		Default: Schedule{Kind: KindEvent, Timezone: "UTC"},
		Run: func(_ context.Context, rc RunContext) (Outcome, error) {
			return Succeeded(Summary{"params": string(rc.Params)}), nil
		},
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)

	// No clock ever creates a run for an event job.
	d.Tick(ctx)
	d.Wait()
	if _, found, err := store.OpenRun(ctx, "provider.reconcile", ""); err != nil || found {
		t.Fatalf("an event job was scheduled on a clock: found=%v err=%v", found, err)
	}

	first, err := d.Enqueue(ctx, "provider.reconcile", "", map[string]any{"image_id": "steam"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := d.Enqueue(ctx, "provider.reconcile", "", map[string]any{"image_id": "steam"})
	if err != nil {
		t.Fatalf("enqueue again: %v", err)
	}
	if second.ID != first.ID {
		t.Fatal("a burst of events did not coalesce into one run")
	}

	d.Tick(ctx)
	d.Wait()
	last, found, err := store.LastTerminalRun(ctx, "provider.reconcile", "")
	if err != nil || !found {
		t.Fatalf("the event run left no record: %v", err)
	}
	if last.State != StateSucceeded {
		t.Fatalf("state: %s", last.State)
	}
	if !strings.Contains(string(last.Summary), "steam") {
		t.Fatalf("params were not handed to the runner: %s", last.Summary)
	}
	// And the completed event run is NOT re-materialized by the clock.
	d.Tick(ctx)
	d.Wait()
	if _, found, err := store.OpenRun(ctx, "provider.reconcile", ""); err != nil || found {
		t.Fatalf("an event job was rescheduled after completing: found=%v err=%v", found, err)
	}
}

// TestHostScopedJobMaterializesOneRunPerHost.
func TestHostScopedJobMaterializesOneRunPerHost(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostA := newHost(t, pool, "host-a")
	hostB := newHost(t, pool, "host-b")

	def := hostDef("home.gc")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)

	d.Tick(ctx)
	d.Wait()

	for _, h := range []string{hostA, hostB} {
		run, found, err := store.OpenRun(ctx, "home.gc", h)
		if err != nil || !found {
			t.Fatalf("no run for host %s: found=%v err=%v", h, found, err)
		}
		// An agent-plane run must never be claimed by the control-plane tick.
		if run.State != StatePending {
			t.Fatalf("the control plane claimed an agent-plane run: %s", run.State)
		}
	}
}

// TestReapedRunIsRematerializedOnTheNextTick — the A11 acceptance shape.
func TestReapedRunIsRematerializedOnTheNextTick(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")

	def := hostDef("home.gc")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	if _, err := store.SyncDefinitions(ctx, []Definition{def}, "UTC", 50); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ClaimTimeout = 60 * time.Second
	d := New(store, reg, cfg, quietLog())

	d.Tick(ctx)
	claimed, err := store.ClaimDue(ctx, ClaimOptions{Plane: PlaneAgent, HostID: host, Now: time.Now()})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("agent claim: %v %+v", err, claimed)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET claimed_at = now() - interval '10 minutes' WHERE id = $1::uuid`,
		claimed[0].ID); err != nil {
		t.Fatal(err)
	}

	d.Tick(ctx)
	d.Wait()

	aborted, err := store.ListRuns(ctx, "home.gc", host, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawAborted bool
	for _, r := range aborted {
		if r.ID == claimed[0].ID && r.State == StateAborted {
			sawAborted = true
		}
	}
	if !sawAborted {
		t.Fatalf("the abandoned claim was not aborted: %+v", aborted)
	}
	if _, found, err := store.OpenRun(ctx, "home.gc", host); err != nil || !found {
		t.Fatalf("the reaped run was not re-materialized: found=%v err=%v", found, err)
	}
}

// TestDisabledDispatcherStartsNothing — QUASAR_JOBS=0.
func TestDisabledDispatcherStartsNothing(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := intervalDef("a.one")
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	if _, err := store.SyncDefinitions(ctx, []Definition{def}, "UTC", 50); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Enabled = false
	d := New(store, reg, cfg, quietLog())
	d.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	d.Wait()

	if _, found, err := store.OpenRun(ctx, "a.one", ""); err != nil || found {
		t.Fatalf("QUASAR_JOBS=0 still scheduled a run: found=%v err=%v", found, err)
	}
}

// TestEnvOverrideWinsOverTheAdminInterval — the schedule_locked precedence, at
// the scheduling layer rather than the API layer.
func TestEnvOverrideWinsOverTheAdminInterval(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := intervalDef("library.discovery")
	def.EnvOverride = "QUASAR_LIBRARY_SCAN_INTERVAL"
	def.Run = func(context.Context, RunContext) (Outcome, error) { return Succeeded(nil), nil }
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)
	d.getenv = func(k string) string {
		if k == "QUASAR_LIBRARY_SCAN_INTERVAL" {
			return "2h"
		}
		return ""
	}
	// The admin asked for six hours; the environment says two, and the
	// environment is authoritative.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET interval_secs = 21600 WHERE id = 'library.discovery'`); err != nil {
		t.Fatal(err)
	}

	d.Tick(ctx)
	d.Wait()
	last, found, err := store.LastTerminalRun(ctx, "library.discovery", "")
	if err != nil || !found {
		t.Fatalf("first run: %v", err)
	}
	// The next run is materialized by the FOLLOWING tick (materialize precedes
	// execute within a tick, so the run that just finished had no successor yet).
	d.Tick(ctx)
	d.Wait()
	next, found, err := store.OpenRun(ctx, "library.discovery", "")
	if err != nil || !found {
		t.Fatalf("no next run: %v", err)
	}
	if wait := next.ScheduledFor.Sub(*last.FinishedAt); wait < 115*time.Minute || wait > 125*time.Minute {
		t.Fatalf("next run in %s, want the environment's 2h, not the admin's 6h", wait)
	}
}

// TestEnvKillSwitchSchedulesNothing — QUASAR_LIBRARY_SCAN_INTERVAL=0's
// documented "forces the feature dark regardless of the database" semantics.
func TestEnvKillSwitchSchedulesNothing(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := intervalDef("library.discovery")
	def.EnvOverride = "QUASAR_LIBRARY_SCAN_INTERVAL"
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	d, store := newDispatcher(t, pool, reg, def)
	d.getenv = func(k string) string {
		if k == "QUASAR_LIBRARY_SCAN_INTERVAL" {
			return "0s"
		}
		return ""
	}
	d.Tick(ctx)
	d.Wait()
	if _, found, err := store.OpenRun(ctx, "library.discovery", ""); err != nil || found {
		t.Fatalf("the env kill switch still scheduled a run: found=%v err=%v", found, err)
	}
}
