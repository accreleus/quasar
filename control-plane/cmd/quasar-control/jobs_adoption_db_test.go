// jobs_adoption_db_test.go — WP2 (docs/design/plans/2026-08-12-jobs-framework-and-viewer.md
// §8): a real NewServices boot against a real Postgres, verifying every
// adopted job and every unmanaged row lands in the `jobs` table (registration
// is only exercised at RUNTIME — a duplicate id or an invalid Definition
// panics inside NewServices, which nothing at build/vet time can catch), and
// that the dispatcher actually executes an adopted job with no ticker of its
// own left to race it.
//
// TEST_DATABASE_URL-gated like every other DB test in this repo; `make
// test-db` provisions the database that makes it actually run.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/config"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func jobsTestDB(t *testing.T) *pgxpool.Pool {
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
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Every DB test in this repo shares one database (make test-db) and
	// truncates in setup — see internal/settings/settings_test.go's testDB for
	// the identical pattern.
	tables := []string{
		"job_runs", "jobs", "library_scans", "user_homes", "auth_tokens",
		"sessions", "user_devices", "invites", "users", "hosts", "instance_settings",
	}
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+tbl+" CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	return pool
}

// jobsTestConfig builds a minimal-but-real config.Config via config.Load(),
// pointed at throwaway paths, so NewServices exercises the SAME parsing and
// defaulting path production does rather than a hand-built struct that could
// drift from what config.Load actually produces.
func jobsTestConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://unused/unused") // required by Load, never dialed (pool is passed separately)
	t.Setenv("ENROLLMENT_TOKEN", "test-enrollment-token")
	t.Setenv("QUASAR_ARTWORK_DIR", t.TempDir())
	t.Setenv("QUASAR_SETUP_TOKEN_PATH", t.TempDir()+"/setup-token")
	t.Setenv("QUASAR_JOBS_TICK_SECS", "1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// wantedJobIDs is the WP2 adoption list (§8.2) plus the §8.6 unmanaged rows —
// the full inventory the Jobs page (WP7) is meant to show. Any change to
// cmd/quasar-control/app.go's registrations that adds, removes or renames an
// id should be a deliberate edit to this list too.
var wantedManagedJobIDs = []string{
	"artwork.sweep",
	"artwork.prune_orphans",
	"library.discovery",
	"auth.token_janitor",
	"storage.home_janitor",
	"devauth.reaper",
	"telemetry.retain",
}

// wantedAgentJobIDs is the WP5/WP6 adoption list: managed jobs whose SCHEDULE
// and run record live here but whose WORK happens on a host, claimed over the
// /v1/agent/jobs/* pull channel. They carry no Run func by construction (the
// registry refuses one for a PlaneAgent Definition), and they must be host
// scoped — an agent-plane job with instance scope would have no host to run on.
var wantedAgentJobIDs = []string{
	"template.warmup",
	"home.gc",
}

var wantedUnmanagedJobIDs = []string{
	"images.provider_reconcile",
	"images.ensure_retry",
	"images.registration_reconcile",
	"session.start_watchdog",
	"console.selfheal",
	"session.cert_bench",
	"library.scanner",
	"console.hotplug_watcher",
	"images.workers",
}

func discardSlogLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestJobsRegistrySyncAdoptsExactlyWP2sList is the headline registration
// assertion: NewServices must not panic (a duplicate id or a bad Definition
// panics via jobRegistry.MustRegister) and the `jobs` table must end up with
// exactly the WP2 managed jobs (each schedule_kind/plane/scope/managed as
// designed) plus the §8.6 unmanaged rows — nothing missing, nothing extra.
func TestJobsRegistrySyncAdoptsExactlyWP2sList(t *testing.T) {
	pool := jobsTestDB(t)
	cfg := jobsTestConfig(t)
	log := discardSlogLogger()

	svc, err := NewServices(cfg, pool, log, nil)
	if err != nil {
		t.Fatalf("NewServices: %v", err)
	}
	defer svc.Stop()

	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT id, plane, scope, managed, schedule_kind FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()

	type row struct {
		id, plane, scope, kind string
		managed                bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.plane, &r.scope, &r.managed, &r.kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	byID := make(map[string]row, len(got))
	for _, r := range got {
		byID[r.id] = r
	}

	for _, id := range wantedManagedJobIDs {
		r, ok := byID[id]
		if !ok {
			t.Errorf("managed job %q is missing from the jobs table", id)
			continue
		}
		if !r.managed {
			t.Errorf("job %q: managed = false, want true", id)
		}
		if r.plane != "control" {
			t.Errorf("job %q: plane = %q, want control (WP2 adopts control-plane jobs only)", id, r.plane)
		}
	}
	for _, id := range wantedAgentJobIDs {
		r, ok := byID[id]
		if !ok {
			t.Errorf("agent-plane job %q is missing from the jobs table", id)
			continue
		}
		if !r.managed {
			t.Errorf("job %q: managed = false, want true", id)
		}
		if r.plane != "agent" {
			t.Errorf("job %q: plane = %q, want agent", id, r.plane)
		}
		if r.scope != "host" {
			t.Errorf("job %q: scope = %q, want host (an agent-plane job runs ON a host)", id, r.scope)
		}
	}
	for _, id := range wantedUnmanagedJobIDs {
		r, ok := byID[id]
		if !ok {
			t.Errorf("unmanaged job %q is missing from the jobs table", id)
			continue
		}
		if r.managed {
			t.Errorf("job %q: managed = true, want false (it is not adopted)", id)
		}
	}

	wantTotal := len(wantedManagedJobIDs) + len(wantedAgentJobIDs) + len(wantedUnmanagedJobIDs)
	if len(got) != wantTotal {
		t.Errorf("jobs table has %d rows, want %d (%v)", len(got), wantTotal, byID)
	}

	// artwork.sweep and library.discovery are the two interval jobs whose
	// Default the code computes (a literal for artwork, a settings-store read
	// for library) — assert the schedule_kind landed as designed rather than
	// silently defaulting to something else.
	if r, ok := byID["artwork.sweep"]; ok && r.kind != "interval" {
		t.Errorf("artwork.sweep schedule_kind = %q, want interval", r.kind)
	}
	if r, ok := byID["artwork.prune_orphans"]; ok && r.kind != "manual" {
		t.Errorf("artwork.prune_orphans schedule_kind = %q, want manual", r.kind)
	}
	if r, ok := byID["library.discovery"]; ok && r.kind != "interval" {
		t.Errorf("library.discovery schedule_kind = %q, want interval", r.kind)
	}
	// WP5/WP6. template.warmup is EVENT-triggered (an image reaching `ready` on a
	// host, enqueued from images.Ensurer.AgentImageState) — a clock schedule would
	// either re-run it pointlessly or delay it. home.gc reproduces the agent
	// ticker it replaced exactly: every 6 h.
	if r, ok := byID["template.warmup"]; ok && r.kind != "event" {
		t.Errorf("template.warmup schedule_kind = %q, want event", r.kind)
	}
	if r, ok := byID["home.gc"]; ok && r.kind != "interval" {
		t.Errorf("home.gc schedule_kind = %q, want interval", r.kind)
	}
	var homeGCInterval int32
	if err := pool.QueryRow(ctx, `SELECT interval_secs FROM jobs WHERE id = 'home.gc'`).Scan(&homeGCInterval); err != nil {
		t.Fatalf("read home.gc interval: %v", err)
	}
	if homeGCInterval != 6*3600 {
		t.Errorf("home.gc interval_secs = %d, want 21600 (the agent ticker it replaced)", homeGCInterval)
	}
}

// TestJobsDispatcherRunsAnAdoptedJobWithNoLegacyTicker is the "legacy ticker
// gone, framework is the trigger" acceptance check for WP2 (task constraints):
// devauth.reaper's interval is 60s (devauth.ReapInterval), well below the
// test's patience, so driving a few dispatcher ticks — the ONLY mechanism
// left that can start this job, since devauth.StartReaper no longer exists —
// must produce a terminal run row with a real summary.
func TestJobsDispatcherRunsAnAdoptedJobWithNoLegacyTicker(t *testing.T) {
	pool := jobsTestDB(t)
	cfg := jobsTestConfig(t)
	log := discardSlogLogger()

	svc, err := NewServices(cfg, pool, log, nil)
	if err != nil {
		t.Fatalf("NewServices: %v", err)
	}
	defer svc.Stop()

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var state, jobID string
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `
			SELECT job_id, state FROM job_runs
			WHERE job_id = 'devauth.reaper' AND state NOT IN ('pending','running')
			ORDER BY created_at DESC LIMIT 1`).Scan(&jobID, &state)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if jobID != "devauth.reaper" {
		t.Fatal("devauth.reaper never produced a terminal run — the dispatcher tick is not driving it")
	}
	if state != "succeeded" {
		t.Fatalf("devauth.reaper run state = %q, want succeeded (an empty-table reap is a no-op success)", state)
	}

	// No SECOND ticker exists to double-run it: with the legacy 60s
	// devauth.StartReaper ticker gone, the dispatcher (QUASAR_JOBS_TICK_SECS=1
	// here) is the only thing that can ever create a job_runs row for this id —
	// so a run existing at all, with no code path left to have created it other
	// than the dispatcher, IS the "ticker removed" proof.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE job_id = 'devauth.reaper'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Fatal("no devauth.reaper run rows at all")
	}
}
