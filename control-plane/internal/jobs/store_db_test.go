// store_db_test.go — the storage invariants. TEST_DATABASE_URL-gated like every
// other DB test in this repo (see internal/settings/settings_test.go's testDB
// for the identical pattern); `make test-db` provisions the database that makes
// them actually run.
//
// These are the tests that matter most in WP1, because the framework's
// correctness claims are all STORAGE claims: single-flight is a partial unique
// index, disjoint claiming is SKIP LOCKED, backoff is a column, and the
// ownership split between code and admin is which columns a sync writes. None of
// them can be verified without Postgres.
package jobs

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE jobs, hosts, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newHost(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name) VALUES ($1) RETURNING id::text`, name).Scan(&id)
	if err != nil {
		t.Fatalf("insert host: %v", err)
	}
	return id
}

// seed syncs one or more definitions and returns the store.
func seed(t *testing.T, pool *pgxpool.Pool, defs ...Definition) *Store {
	t.Helper()
	s := NewStore(pool)
	if _, err := s.SyncDefinitions(context.Background(), defs, "UTC", 50); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return s
}

func intervalDef(id string) Definition {
	d := okDef()
	d.ID = id
	return d
}

func hostDef(id string) Definition {
	return Definition{
		ID: id, Name: id, Plane: PlaneAgent, Scope: ScopeHost, Managed: true,
		Default: Schedule{Kind: KindInterval, IntervalSecs: 3600, Timezone: "UTC"},
	}
}

// --- registry sync ---------------------------------------------------------

// TestSyncOwnsIdentityAndNeverTheSchedule is THE ownership test. A boot that
// clobbered an admin's 02:00-06:00 window because a developer edited a
// Definition literal would make the whole surface untrustworthy.
func TestSyncOwnsIdentityAndNeverTheSchedule(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := NewStore(pool)

	def := intervalDef("artwork.sweep")
	res, err := s.SyncDefinitions(ctx, []Definition{def}, "UTC", 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Added != 1 || res.Updated != 0 || res.Removed != 0 {
		t.Fatalf("first sync: %+v want added=1", res)
	}

	// An admin edits every column they own.
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET enabled = false, interval_secs = 21600,
		    window_start = '02:00', window_end = '06:00', window_days = '{0,6}',
		    timezone = 'Europe/London', history_limit = 500
		WHERE id = 'artwork.sweep'`); err != nil {
		t.Fatalf("admin edit: %v", err)
	}

	// A later build renames the job and rewrites its default schedule.
	def.Name = "Cover artwork sweeper"
	def.Description = "Resolves cover and hero art."
	def.Default.IntervalSecs = 60
	def.Default.Timezone = "UTC"
	res, err = s.SyncDefinitions(ctx, []Definition{def}, "UTC", 50)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if res.Added != 0 || res.Updated != 1 {
		t.Fatalf("re-sync: %+v want updated=1", res)
	}

	j, err := s.Get(ctx, "artwork.sweep")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if j.Name != "Cover artwork sweeper" || j.Description != "Resolves cover and hero art." {
		t.Fatalf("identity was not reconciled: %+v", j)
	}
	if j.Enabled {
		t.Fatal("sync re-enabled a job the admin disabled")
	}
	if j.Schedule.IntervalSecs != 21600 {
		t.Fatalf("sync overwrote the admin's interval: %d", j.Schedule.IntervalSecs)
	}
	if j.Schedule.WindowStart == nil || j.Schedule.WindowStart.Hour != 2 ||
		j.Schedule.WindowEnd == nil || j.Schedule.WindowEnd.Hour != 6 {
		t.Fatalf("sync overwrote the admin's window: %+v", j.Schedule)
	}
	if len(j.Schedule.WindowDays) != 2 || j.Schedule.WindowDays[0] != 0 || j.Schedule.WindowDays[1] != 6 {
		t.Fatalf("sync overwrote the admin's window days: %v", j.Schedule.WindowDays)
	}
	if j.Schedule.Timezone != "Europe/London" {
		t.Fatalf("sync overwrote the admin's timezone: %s", j.Schedule.Timezone)
	}
	if j.HistoryLimit != 500 {
		t.Fatalf("sync lowered a history limit the admin raised: %d", j.HistoryLimit)
	}

	// An unchanged deploy must report an unchanged sync, or the boot line is noise.
	res, err = s.SyncDefinitions(ctx, []Definition{def}, "UTC", 50)
	if err != nil {
		t.Fatalf("idempotent sync: %v", err)
	}
	if res != (SyncResult{}) {
		t.Fatalf("idempotent re-sync reported %+v, want all zero", res)
	}
}

func TestSyncRemovesUnregisteredJobsAndTheirHistory(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"), intervalDef("b.two"))

	run, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "b.two", Trigger: TriggerSchedule, ScheduledFor: time.Now(),
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	res, err := s.SyncDefinitions(ctx, []Definition{intervalDef("a.one")}, "UTC", 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("sync: %+v want removed=1", res)
	}
	if _, err := s.Get(ctx, "b.two"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("de-registered job survived: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE id = $1::uuid`, run.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("history of a de-registered job was not cascaded away")
	}
}

func TestSyncSeedsHistoryLimitAndTimezoneFromTheEnvironmentDefaults(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	// A Definition that names no zone inherits QUASAR_JOBS_TIMEZONE.
	def := intervalDef("a.one")
	def.Default.Timezone = ""
	if _, err := s.SyncDefinitions(ctx, []Definition{def}, "Europe/London", 200); err != nil {
		t.Fatalf("sync: %v", err)
	}
	j, err := s.Get(ctx, "a.one")
	if err != nil {
		t.Fatal(err)
	}
	if j.Schedule.Timezone != "Europe/London" {
		t.Fatalf("timezone seed: got %s", j.Schedule.Timezone)
	}
	if j.HistoryLimit != 200 {
		t.Fatalf("history limit seed: got %d", j.HistoryLimit)
	}
}

// --- single flight ---------------------------------------------------------

// TestPartialUniqueIndexForbidsASecondOpenRun goes AROUND Materialize on
// purpose. The claim is that single-flight is a storage invariant rather than a
// convention, and a test that only exercised the Go path would prove nothing
// about a second dispatcher instance.
func TestPartialUniqueIndexForbidsASecondOpenRun(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	seed(t, pool, intervalDef("a.one"))

	ins := func(state string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO job_runs (job_id, state, trigger, scheduled_for)
			VALUES ('a.one', $1, 'schedule', now())`, state)
		return err
	}
	if err := ins("pending"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	for _, state := range []string{"pending", "running"} {
		err := ins(state)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("a second open (%s) run was accepted: %v", state, err)
		}
	}
	// Terminal rows are unconstrained — history is allowed to accumulate.
	for _, state := range []string{"succeeded", "failed", "deferred", "skipped", "aborted"} {
		if err := ins(state); err != nil {
			t.Fatalf("terminal insert (%s): %v", state, err)
		}
	}
}

func TestPartialUniqueIndexIsPerTarget(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, hostDef("home.gc"))
	a := newHost(t, pool, "host-a")
	b := newHost(t, pool, "host-b")

	for _, h := range []string{a, b} {
		if _, m, err := s.Materialize(ctx, MaterializeParams{
			JobID: "home.gc", HostID: h, Trigger: TriggerSchedule, ScheduledFor: time.Now(),
		}); err != nil || m != RunCreated {
			t.Fatalf("materialize for %s: m=%v err=%v", h, m, err)
		}
	}
}

func TestMaterializeReturnsTheOpenRunRatherThanDuplicating(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"))

	first, m, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: time.Now(),
	})
	if err != nil || m != RunCreated {
		t.Fatalf("first: m=%v err=%v", m, err)
	}
	second, m, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: time.Now(),
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if m != RunCoalesced || second.ID != first.ID {
		t.Fatalf("a second run row was created: %s vs %s (m=%v)", second.ID, first.ID, m)
	}
}

// TestManualTriggerPullsAPendingRunForward — "Run now" on a job that is already
// queued means NOW, not TWICE.
func TestManualTriggerPullsAPendingRunForward(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"))

	future := time.Now().Add(6 * time.Hour)
	queued, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: future,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pulled, m, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerManual, ScheduledFor: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m != RunPulledForward || pulled.ID != queued.ID {
		t.Fatalf("manual trigger created a second row: m=%v %+v", m, pulled)
	}
	if pulled.Trigger != TriggerManual {
		t.Fatalf("trigger not re-stamped: %s", pulled.Trigger)
	}
	if pulled.ScheduledFor.After(now.Add(time.Second)) {
		t.Fatalf("not pulled forward: %s", pulled.ScheduledFor)
	}
}

func TestMaterializeEnforcesScopeAndGates(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")
	unmanaged := Definition{ID: "c.unmanaged", Name: "c", Plane: PlaneControl,
		Scope: ScopeInstance, Managed: false, Default: Schedule{Kind: KindEvent, Timezone: "UTC"}}
	s := seed(t, pool, intervalDef("a.one"), hostDef("h.one"), unmanaged)

	base := MaterializeParams{Trigger: TriggerManual, ScheduledFor: time.Now()}

	p := base
	p.JobID, p.HostID = "h.one", ""
	if _, _, err := s.Materialize(ctx, p); !errors.Is(err, ErrHostRequired) {
		t.Fatalf("host-scoped job with no host: %v", err)
	}
	p = base
	p.JobID, p.HostID = "a.one", host
	if _, _, err := s.Materialize(ctx, p); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("instance-scoped job with a host: %v", err)
	}
	p = base
	p.JobID = "c.unmanaged"
	if _, _, err := s.Materialize(ctx, p); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("unmanaged job: %v", err)
	}
	p = base
	p.JobID = "nope.missing"
	if _, _, err := s.Materialize(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}

	// A disabled job never runs, not even manually.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET enabled = false WHERE id = 'a.one'`); err != nil {
		t.Fatal(err)
	}
	p = base
	p.JobID = "a.one"
	if _, _, err := s.Materialize(ctx, p); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled job: %v", err)
	}
}

// --- claiming --------------------------------------------------------------

// TestConcurrentClaimsAreDisjoint is the multi-instance-safety test: two
// dispatchers against one database must partition the due work, never duplicate
// it and never deadlock on it.
func TestConcurrentClaimsAreDisjoint(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	const jobCount = 12
	defs := make([]Definition, 0, jobCount)
	for i := 0; i < jobCount; i++ {
		defs = append(defs, intervalDef(string(rune('a'+i))+".job"))
	}
	s := seed(t, pool, defs...)
	for _, d := range defs {
		if _, _, err := s.Materialize(ctx, MaterializeParams{
			JobID: d.ID, Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatalf("materialize %s: %v", d.ID, err)
		}
	}

	const dispatchers = 4
	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for i := 0; i < dispatchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pass := 0; pass < 4; pass++ {
				runs, err := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: time.Now(), Limit: 5})
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				for _, r := range runs {
					seen[r.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != jobCount {
		t.Fatalf("claimed %d distinct runs, want %d", len(seen), jobCount)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("run %s was claimed %d times — SKIP LOCKED did not partition the work", id, n)
		}
	}
}

func TestClaimRespectsPlaneHostEnabledAndDueTime(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostA := newHost(t, pool, "host-a")
	hostB := newHost(t, pool, "host-b")
	s := seed(t, pool, intervalDef("c.one"), intervalDef("c.two"), hostDef("h.one"))

	now := time.Now()
	mk := func(job, host string, at time.Time) {
		t.Helper()
		if _, _, err := s.Materialize(ctx, MaterializeParams{
			JobID: job, HostID: host, Trigger: TriggerSchedule, ScheduledFor: at,
		}); err != nil {
			t.Fatalf("materialize %s: %v", job, err)
		}
	}
	mk("c.one", "", now.Add(-time.Minute)) // due, control plane
	mk("c.two", "", now.Add(time.Hour))    // not due yet
	mk("h.one", hostA, now.Add(-time.Minute))
	mk("h.one", hostB, now.Add(-time.Minute))

	// A control-plane dispatcher must never claim work only a host can execute.
	got, err := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].JobID != "c.one" {
		t.Fatalf("control claim: %+v", got)
	}

	// An agent claims only its own host's runs.
	got, err = s.ClaimDue(ctx, ClaimOptions{Plane: PlaneAgent, HostID: hostA, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].HostID != hostA {
		t.Fatalf("hostA claim: %+v", got)
	}

	// A disabled job's pending row is not claimed.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET enabled = false WHERE id = 'h.one'`); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimDue(ctx, ClaimOptions{Plane: PlaneAgent, HostID: hostB, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("claimed a disabled job's run: %+v", got)
	}

	if _, err := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneAgent, Now: now}); !errors.Is(err, ErrHostRequired) {
		t.Fatal("an agent claim with no host must be refused")
	}
}

// --- reporting -------------------------------------------------------------

func TestReportIsIdempotentForATerminalRun(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"))
	if _, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: time.Now()})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	run := claimed[0]

	got, applied, err := s.Report(ctx, run.ID, StateSucceeded, Summary{"apps_considered": 412}, "")
	if err != nil || !applied {
		t.Fatalf("first report: applied=%v err=%v", applied, err)
	}
	if got.State != StateSucceeded || got.FinishedAt == nil || got.DurationMS() == nil {
		t.Fatalf("report did not close the run: %+v", got)
	}

	// An agent retrying after a network blip must be safe: a no-op, not an error
	// that turns a successful run into a permanent failure in the operator's face.
	got, applied, err = s.Report(ctx, run.ID, StateFailed, nil, "boom")
	if err != nil {
		t.Fatalf("retry report: %v", err)
	}
	if applied {
		t.Fatal("a report for an already-terminal run was applied")
	}
	if got.State != StateSucceeded {
		t.Fatalf("a retry rewrote a terminal outcome: %s", got.State)
	}
}

func TestReportRefusesAnOversizedSummary(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"))
	if _, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, _ := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: time.Now()})
	big := make([]byte, 5000)
	for i := range big {
		big[i] = 'x'
	}
	if _, _, err := s.Report(ctx, claimed[0].ID, StateSucceeded, Summary{"blob": string(big)}, ""); err == nil {
		t.Fatal("an oversized summary must fail the report rather than violate the CHECK")
	}
}

// --- reaping ---------------------------------------------------------------

// TestReapAbandonedFreesTheSingleFlightSlot. Without the reaper an agent that
// died mid-run leaves a `running` row forever, and the single-flight index then
// blocks that job on that host for good: the failure mode is not "one lost run",
// it is "this job never runs again here".
func TestReapAbandonedFreesTheSingleFlightSlot(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	host := newHost(t, pool, "host-a")
	s := seed(t, pool, hostDef("home.gc"))

	if _, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "home.gc", HostID: host, Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneAgent, HostID: host, Now: time.Now()})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	// Backdate the claim to simulate an agent that died holding it.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET claimed_at = now() - interval '2 hours' WHERE id = $1::uuid`, claimed[0].ID); err != nil {
		t.Fatal(err)
	}

	if reaped, err := s.ReapAbandoned(ctx, 5*time.Minute); err != nil || len(reaped) != 1 {
		t.Fatalf("reap: %v %+v", err, reaped)
	}
	if _, open, err := s.OpenRun(ctx, "home.gc", host); err != nil || open {
		t.Fatalf("the slot is still held after reaping: open=%v err=%v", open, err)
	}
	// A young claim is left alone.
	if _, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "home.gc", HostID: host, Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneAgent, HostID: host, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if reaped, err := s.ReapAbandoned(ctx, time.Hour); err != nil || len(reaped) != 0 {
		t.Fatalf("a fresh claim was reaped: %v %+v", err, reaped)
	}
}

// --- retention -------------------------------------------------------------

func TestPruneAppliesTheRowCapPerTargetAndTheAgeCap(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostA := newHost(t, pool, "host-a")
	hostB := newHost(t, pool, "host-b")
	s := seed(t, pool, hostDef("home.gc"), intervalDef("a.one"))

	if _, err := pool.Exec(ctx, `UPDATE jobs SET history_limit = 5`); err != nil {
		t.Fatal(err)
	}
	// 10 terminal runs per (job, target).
	for i := 0; i < 10; i++ {
		for _, h := range []string{hostA, hostB} {
			if _, err := pool.Exec(ctx, `
				INSERT INTO job_runs (job_id, host_id, state, trigger, scheduled_for, created_at)
				VALUES ('home.gc', $1::uuid, 'succeeded', 'schedule', now(), now() - make_interval(mins => $2))`,
				h, i); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO job_runs (job_id, state, trigger, scheduled_for, created_at)
			VALUES ('a.one', 'succeeded', 'schedule', now(), now() - make_interval(mins => $1))`, i); err != nil {
			t.Fatal(err)
		}
	}
	// One open run per (job, target) that must survive: pruning a pending row
	// would delete the next run.
	if _, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PruneRuns(ctx, 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	count := func(job, host string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM job_runs
			WHERE job_id = $1 AND ($2 = '' OR host_id = $2::uuid)`, job, host).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count("home.gc", hostA); got != 5 {
		t.Fatalf("hostA history: %d want 5 (the cap is per target, not per job)", got)
	}
	if got := count("home.gc", hostB); got != 5 {
		t.Fatalf("hostB history: %d want 5", got)
	}
	if got := count("a.one", ""); got != 6 {
		t.Fatalf("instance history: %d want 5 terminal + 1 pending", got)
	}

	// The age cap applies on top of the row cap.
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET created_at = now() - interval '40 days'
		WHERE job_id = 'a.one' AND state = 'succeeded'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneRuns(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if got := count("a.one", ""); got != 2 {
		t.Fatalf("after the age cap: %d want 2 (the pending row and the anchor)", got)
	}
}

// The newest terminal run of each (job, target) survives the age cap, whatever
// its age or outcome: it anchors the interval (LastTerminalRun).
func TestPruneRunsKeepsTheNewestTerminalRunPerTarget(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	hostA := newHost(t, pool, "host-a")
	hostB := newHost(t, pool, "host-b")
	s := seed(t, pool, hostDef("home.gc"))

	// Three terminal runs per host, all older than the age cap. The newest by
	// finished_at is the middle row by created_at, so the keeper is not simply
	// the newest-created row. Host B's keeper failed: a failed run anchors too.
	for _, h := range []struct{ id, state string }{{hostA, "succeeded"}, {hostB, "failed"}} {
		for _, days := range []struct{ created, finished int }{{90, 89}, {80, 41}, {70, 60}} {
			state := "succeeded"
			if days.finished == 41 {
				state = h.state
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO job_runs (job_id, host_id, state, trigger, scheduled_for, created_at, finished_at)
				VALUES ('home.gc', $1::uuid, $2, 'schedule', now(),
				        now() - make_interval(days => $3), now() - make_interval(days => $4))`,
				h.id, state, days.created, days.finished); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, err := s.PruneRuns(ctx, 30); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{hostA, hostB} {
		last, found, err := s.LastTerminalRun(ctx, "home.gc", h)
		if err != nil || !found {
			t.Fatalf("host %s lost its anchor: found=%v err=%v", h, found, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE host_id = $1::uuid`, h).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 || last.FinishedAt == nil || time.Since(*last.FinishedAt) < 40*24*time.Hour || time.Since(*last.FinishedAt) > 42*24*time.Hour {
			t.Fatalf("host %s: %d rows left, anchor finished %v; want only the run finished 41 days ago", h, n, last.FinishedAt)
		}
	}
}

// The row PruneRuns keeps is the row LastTerminalRun returned before it ran.
// pruneRow: created and finished are days ago; finished -1 is NULL.
type pruneRow struct {
	created  int
	finished int
	state    string
}

func TestPruneRunsKeepsExactlyTheRowLastTerminalRunReturns(t *testing.T) {
	cases := []struct {
		name       string
		limit      int
		rows       []pruneRow
		wantRows   int // terminal rows left in the partition
		wantAnchor int // finished days ago of the survivor LastTerminalRun must return; -1 = by created
	}{
		{"newest finished is not newest created", 50,
			[]pruneRow{{90, 89, "succeeded"}, {80, 41, "failed"}, {70, 60, "succeeded"}}, 1, 41},
		{"a newer terminal row has no finished_at", 50,
			[]pruneRow{{50, 45, "succeeded"}, {40, -1, "aborted"}}, 1, 45},
		{"no row has finished_at", 50,
			[]pruneRow{{50, -1, "aborted"}, {40, -1, "skipped"}}, 1, -1},
		{"deferred skipped aborted all anchor", 50,
			[]pruneRow{{60, 59, "skipped"}, {58, 57, "deferred"}, {56, 55, "aborted"}}, 1, 55},
		{"row cap of one and anchor is not the newest created", 1,
			[]pruneRow{{20, 5, "succeeded"}, {10, 8, "succeeded"}}, 2, 5},
		{"row cap only, all recent", 2,
			[]pruneRow{{5, 4, "succeeded"}, {4, 3, "succeeded"}, {3, 2, "failed"}, {2, 1, "succeeded"}}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			ctx := context.Background()
			host := newHost(t, pool, "host-a")
			s := seed(t, pool, hostDef("home.gc"), intervalDef("a.one"))
			if _, err := pool.Exec(ctx, `UPDATE jobs SET history_limit = $1`, tc.limit); err != nil {
				t.Fatal(err)
			}
			// The same rows in a per-host partition and in the fleet partition.
			for _, target := range []struct{ job, host string }{{"home.gc", host}, {"a.one", ""}} {
				for _, r := range tc.rows {
					if _, err := pool.Exec(ctx, `
						INSERT INTO job_runs (job_id, host_id, state, trigger, scheduled_for, created_at, finished_at)
						VALUES ($1, NULLIF($2, '')::uuid, $3, 'schedule', now(),
						        now() - make_interval(days => $4::int),
						        CASE WHEN $5::int < 0 THEN NULL ELSE now() - make_interval(days => $5::int) END)`,
						target.job, target.host, r.state, r.created, r.finished); err != nil {
						t.Fatal(err)
					}
				}
				// An open run is never the anchor and never pruned.
				if _, err := pool.Exec(ctx, `
					INSERT INTO job_runs (job_id, host_id, state, trigger, scheduled_for, created_at)
					VALUES ($1, NULLIF($2, '')::uuid, 'pending', 'schedule', now() + interval '1 day', now() - interval '100 days')`,
					target.job, target.host); err != nil {
					t.Fatal(err)
				}
			}
			for _, target := range []struct{ job, host string }{{"home.gc", host}, {"a.one", ""}} {
				before, found, err := s.LastTerminalRun(ctx, target.job, target.host)
				if err != nil || !found {
					t.Fatalf("%s: no anchor before the prune: %v", target.job, err)
				}
				if tc.wantAnchor >= 0 {
					if before.FinishedAt == nil {
						t.Fatalf("%s: LastTerminalRun picked a row with no finished_at", target.job)
					}
					age := time.Since(*before.FinishedAt)
					want := time.Duration(tc.wantAnchor) * 24 * time.Hour
					if age < want-time.Hour || age > want+time.Hour {
						t.Fatalf("%s: LastTerminalRun picked finished %s ago, want %s", target.job, age, want)
					}
				}
			}
			type key struct{ job, host string }
			befores := map[key]Run{}
			for _, target := range []key{{"home.gc", host}, {"a.one", ""}} {
				b, _, _ := s.LastTerminalRun(ctx, target.job, target.host)
				befores[target] = b
			}
			if _, err := s.PruneRuns(ctx, 30); err != nil {
				t.Fatal(err)
			}
			// A second prune must be a no-op: the survivor set is stable.
			if n, err := s.PruneRuns(ctx, 30); err != nil || n != 0 {
				t.Fatalf("second prune removed %d rows (err %v), want 0", n, err)
			}
			for target, b := range befores {
				after, found, err := s.LastTerminalRun(ctx, target.job, target.host)
				if err != nil || !found {
					t.Fatalf("%s: the anchor was pruned: found=%v err=%v", target.job, found, err)
				}
				if after.ID != b.ID {
					t.Fatalf("%s: prune kept %s, the dispatcher was measuring from %s", target.job, after.ID, b.ID)
				}
				var terminal, open int
				if err := pool.QueryRow(ctx, `
					SELECT count(*) FILTER (WHERE state NOT IN ('pending','running')),
					       count(*) FILTER (WHERE state IN ('pending','running'))
					FROM job_runs WHERE job_id = $1`, target.job).Scan(&terminal, &open); err != nil {
					t.Fatal(err)
				}
				if terminal != tc.wantRows || open != 1 {
					t.Fatalf("%s: %d terminal + %d open rows left, want %d + 1", target.job, terminal, open, tc.wantRows)
				}
			}
		})
	}
}

// Rows tied on finished_at and created_at: LastTerminalRun has no id
// tie-break, PruneRuns has. Whichever survives, the schedule base is equal.
func TestPruneRunsTieKeepsAnEqualAnchor(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"))
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_runs (job_id, state, trigger, scheduled_for, created_at, finished_at)
		SELECT 'a.one', st, 'schedule', now(), now() - interval '50 days', now() - interval '45 days'
		FROM unnest(ARRAY['succeeded','failed','aborted']) AS st`); err != nil {
		t.Fatal(err)
	}
	before, _, err := s.LastTerminalRun(ctx, "a.one", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneRuns(ctx, 30); err != nil {
		t.Fatal(err)
	}
	after, found, err := s.LastTerminalRun(ctx, "a.one", "")
	if err != nil || !found {
		t.Fatalf("the anchor was pruned: found=%v err=%v", found, err)
	}
	if !after.FinishedAt.Equal(*before.FinishedAt) || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("the tie changed the schedule base: %v -> %v", before.FinishedAt, after.FinishedAt)
	}
}

func TestListRunsAndLastTerminalRun(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := seed(t, pool, intervalDef("a.one"))
	if _, _, err := s.Materialize(ctx, MaterializeParams{
		JobID: "a.one", Trigger: TriggerSchedule, ScheduledFor: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LastTerminalRun(ctx, "a.one", ""); err != nil || found {
		t.Fatalf("a pending run must not count as the last terminal run: found=%v err=%v", found, err)
	}
	claimed, _ := s.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: time.Now()})
	if _, _, err := s.Report(ctx, claimed[0].ID, StateSkipped, Summary{"reason": "no provider"}, ""); err != nil {
		t.Fatal(err)
	}
	last, found, err := s.LastTerminalRun(ctx, "a.one", "")
	if err != nil || !found || last.State != StateSkipped {
		t.Fatalf("last terminal run: %+v found=%v err=%v", last, found, err)
	}
	runs, err := s.ListRuns(ctx, "a.one", "", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("list runs: %v %+v", err, runs)
	}
}
