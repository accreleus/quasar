// interval_source_db_test.go — a job whose period comes from an IntervalSource (the
// library.discovery scan interval, #536). TEST_DATABASE_URL-gated; `make test-db` runs it.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sourcedJob is library.discovery as app.go registers it, with the source a test moves.
type sourcedJob struct {
	def    Definition
	d      *Dispatcher
	h      *Handler
	store  *Store
	url    string
	period atomic.Int64 // nanoseconds
	byEnv  atomic.Bool
	fail   atomic.Bool
	execs  atomic.Int32
}

func newSourcedJob(t *testing.T, period time.Duration) *sourcedJob {
	t.Helper()
	pool := testDB(t)
	s := &sourcedJob{}
	s.period.Store(int64(period))
	def := intervalDef("library.discovery")
	def.EnvOverride = "QUASAR_LIBRARY_SCAN_INTERVAL"
	def.Default.IntervalSecs = 21600
	def.IntervalOwner = "instance_settings.library_discovery_interval_minutes"
	def.IntervalOwnerHint = "Settings, Libraries, Scan interval"
	def.IntervalSource = func(context.Context) (time.Duration, bool, error) {
		if s.fail.Load() {
			return 0, false, errors.New("settings unreadable")
		}
		return time.Duration(s.period.Load()), s.byEnv.Load(), nil
	}
	def.Run = func(context.Context, RunContext) (Outcome, error) {
		s.execs.Add(1)
		return Succeeded(nil), nil
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	h, store, d, srv := newJobsServer(t, pool, reg, def)
	s.def, s.d, s.h, s.store, s.url = def, d, h, store, srv.URL+"/v1/admin/jobs/library.discovery"
	return s
}

// firstPassAndSuccessor runs the first pass, then lets the next tick materialize its successor.
func (s *sourcedJob) firstPassAndSuccessor(t *testing.T) (finished time.Time, next Run) {
	t.Helper()
	ctx := context.Background()
	s.d.Tick(ctx)
	s.d.Wait()
	s.d.Tick(ctx)
	s.d.Wait()
	last, found, err := s.store.LastTerminalRun(ctx, "library.discovery", "")
	if err != nil || !found {
		t.Fatalf("first pass: found=%v err=%v", found, err)
	}
	return *last.FinishedAt, s.next(t)
}

func (s *sourcedJob) next(t *testing.T) Run {
	t.Helper()
	r, found, err := s.store.OpenRun(context.Background(), "library.discovery", "")
	if err != nil || !found {
		t.Fatalf("no pending run: found=%v err=%v", found, err)
	}
	return r
}

func (s *sourcedJob) tick(t *testing.T) {
	t.Helper()
	s.d.Tick(context.Background())
	s.d.Wait()
}

func (s *sourcedJob) reported(t *testing.T) jobJSON {
	t.Helper()
	status, body := doReq(t, http.MethodGet, s.url, "")
	if status != http.StatusOK {
		t.Fatalf("GET job: %d %s", status, body)
	}
	var j jobJSON
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func near(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want-time.Minute || got > want+time.Minute {
		t.Fatalf("%s = %s, want about %s", what, got, want)
	}
}

func TestSourcedIntervalSchedulesAndIsReported(t *testing.T) {
	s := newSourcedJob(t, 15*time.Minute)
	finished, next := s.firstPassAndSuccessor(t)
	near(t, "next run after the first pass", next.ScheduledFor.Sub(finished), 15*time.Minute)

	j := s.reported(t)
	if j.Schedule.IntervalSecs == nil || *j.Schedule.IntervalSecs != 900 || !j.Schedule.Locked ||
		j.Schedule.LockedBy == nil || *j.Schedule.LockedBy != "instance_settings.library_discovery_interval_minutes" {
		t.Fatalf("reported schedule = %+v, want interval_secs 900 locked by the setting (the stored column is 21600)", j.Schedule)
	}
	if j.NextRunAt == nil || !j.NextRunAt.Equal(next.ScheduledFor) {
		t.Fatalf("next_run_at = %v, want %v", j.NextRunAt, next.ScheduledFor)
	}
}

func TestLoweringTheSourceBringsThePendingRunForward(t *testing.T) {
	s := newSourcedJob(t, 6*time.Hour)
	finished, next := s.firstPassAndSuccessor(t)
	near(t, "next run at 6h", next.ScheduledFor.Sub(finished), 6*time.Hour)

	s.period.Store(int64(15 * time.Minute))
	s.tick(t)
	moved := s.next(t)
	if moved.ID != next.ID {
		t.Fatalf("lowering made a second run: %s vs %s", moved.ID, next.ID)
	}
	near(t, "next run after lowering to 15m", moved.ScheduledFor.Sub(finished), 15*time.Minute)

	// The last pass ended an hour ago: a 15 minute period owes a pass now.
	if _, err := s.store.pool.Exec(context.Background(),
		`UPDATE job_runs SET finished_at = finished_at - interval '1 hour'
		  WHERE job_id = 'library.discovery' AND state <> 'pending'`); err != nil {
		t.Fatal(err)
	}
	s.tick(t)
	if got := s.execs.Load(); got != 2 {
		t.Fatalf("passes run = %d, want 2: an overdue 15 minute period runs without waiting out the old 6h", got)
	}
}

func TestRaisingTheSourcePushesThePendingRunBackWithoutABurst(t *testing.T) {
	s := newSourcedJob(t, 15*time.Minute)
	finished, next := s.firstPassAndSuccessor(t)

	s.period.Store(int64(24 * time.Hour))
	s.tick(t)
	moved := s.next(t)
	if moved.ID != next.ID {
		t.Fatalf("raising made a second run: %s vs %s", moved.ID, next.ID)
	}
	near(t, "next run after raising to 24h", moved.ScheduledFor.Sub(finished), 24*time.Hour)
	if got := s.execs.Load(); got != 1 {
		t.Fatalf("passes run = %d, want 1", got)
	}
}

func TestOnlyAScheduledFirstAttemptIsRealigned(t *testing.T) {
	s := newSourcedJob(t, 6*time.Hour)
	_, next := s.firstPassAndSuccessor(t)
	if _, err := s.store.pool.Exec(context.Background(),
		`UPDATE job_runs SET trigger = 'manual' WHERE id = $1::uuid`, next.ID); err != nil {
		t.Fatal(err)
	}
	before := s.next(t).ScheduledFor
	s.period.Store(int64(15 * time.Minute))
	s.tick(t)
	if got := s.next(t).ScheduledFor; !got.Equal(before) {
		t.Fatalf("a manual run was re-dated: %v -> %v", before, got)
	}
}

func TestSourcedEnvOverrideLocksAndZeroSchedulesNothing(t *testing.T) {
	s := newSourcedJob(t, 2*time.Hour)
	s.byEnv.Store(true)
	finished, next := s.firstPassAndSuccessor(t)
	near(t, "env interval", next.ScheduledFor.Sub(finished), 2*time.Hour)

	j := s.reported(t)
	if !j.Schedule.Locked || j.Schedule.LockedBy == nil || *j.Schedule.LockedBy != "QUASAR_LIBRARY_SCAN_INTERVAL" {
		t.Fatalf("schedule = %+v, want locked by the env var", j.Schedule)
	}
	status, body := doReq(t, http.MethodPatch, s.url, `{"interval_secs": 900}`)
	if status != http.StatusConflict || decodeErr(t, body).Error.Code != "schedule_locked" {
		t.Fatalf("PATCH under the env override: %d %s, want 409 schedule_locked", status, body)
	}

	// The kill switch: a fresh install under QUASAR_LIBRARY_SCAN_INTERVAL=0 gets no run.
	z := newSourcedJob(t, 0)
	z.byEnv.Store(true)
	z.tick(t)
	z.tick(t)
	if _, found, err := z.store.OpenRun(context.Background(), "library.discovery", ""); err != nil || found || z.execs.Load() != 0 {
		t.Fatalf("kill switch scheduled work: found=%v err=%v execs=%d", found, err, z.execs.Load())
	}
}

func TestPatchOfASourcedIntervalIsRefusedUnlessItIsTheValueReported(t *testing.T) {
	s := newSourcedJob(t, 15*time.Minute)
	s.tick(t)

	status, body := doReq(t, http.MethodPatch, s.url, `{"interval_secs": 3600}`)
	if status != http.StatusConflict || decodeErr(t, body).Error.Code != "schedule_locked" {
		t.Fatalf("PATCH of a different interval: %d %s, want 409 schedule_locked", status, body)
	}
	got := decodeErr(t, body).Error.Message
	if !strings.Contains(got, "instance_settings.library_discovery_interval_minutes") || strings.Contains(got, "environment") {
		t.Fatalf("message %q must name the setting, not the environment", got)
	}

	// Shape comes before ownership, as for any job: a different value below 60 is 422.
	status, body = doReq(t, http.MethodPatch, s.url, `{"interval_secs": 5}`)
	if status != http.StatusUnprocessableEntity || decodeErr(t, body).Error.Code != "validation_failed" {
		t.Fatalf("PATCH of a different value below the floor: %d %s, want 422 validation_failed", status, body)
	}

	// Under the env override the advice is that Settings takes over once it is unset.
	s.byEnv.Store(true)
	status, body = doReq(t, http.MethodPatch, s.url, `{"interval_secs": 3600}`)
	if got := decodeErr(t, body).Error.Message; status != http.StatusConflict ||
		!strings.Contains(got, "Settings, Libraries, Scan interval") || strings.Contains(got, "admin-owned") {
		t.Fatalf("env-locked PATCH: %d %q, want 409 pointing at Settings once unset", status, got)
	}
	if status, body = doReq(t, http.MethodPatch, s.url, `{"interval_secs": 5}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("env-locked PATCH below the floor: %d %s, want 422", status, body)
	}
	s.byEnv.Store(false)

	// The Jobs tab sends the interval back unchanged with a window edit.
	patch := `{"interval_secs": 900, "window_start": "01:00", "window_end": "05:00"}`
	status, body = doReq(t, http.MethodPatch, s.url, patch)
	if status != http.StatusOK {
		t.Fatalf("PATCH with the interval reported: %d %s, want 200", status, body)
	}
	// ...and so under the environment override, kill switch included.
	s.byEnv.Store(true)
	status, body = doReq(t, http.MethodPatch, s.url, patch)
	if status != http.StatusOK {
		t.Fatalf("PATCH with the interval reported under the env override: %d %s, want 200", status, body)
	}
	// A positive reported period below the floor is resent unchanged too.
	s.period.Store(int64(30 * time.Second))
	status, body = doReq(t, http.MethodPatch, s.url, `{"interval_secs": 30}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH resending a reported 30 s: %d %s, want 200", status, body)
	}
	s.period.Store(0)
	status, body = doReq(t, http.MethodPatch, s.url, `{"interval_secs": 0}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH resending the kill switch's 0: %d %s, want 200", status, body)
	}
	var stored int32
	if err := s.store.pool.QueryRow(context.Background(),
		`SELECT interval_secs FROM jobs WHERE id = 'library.discovery'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 21600 {
		t.Fatalf("stored interval_secs = %d, want the seed 21600 untouched", stored)
	}
}

func TestAnUnreadableSourceFailsTheJobsReadAndSkipsScheduling(t *testing.T) {
	s := newSourcedJob(t, 15*time.Minute)
	s.fail.Store(true)
	if status, body := doReq(t, http.MethodGet, s.url, ""); status != http.StatusInternalServerError {
		t.Fatalf("GET with an unreadable source: %d %s, want 500", status, body)
	}
	if status, _ := doReq(t, http.MethodGet, strings.TrimSuffix(s.url, "/library.discovery"), ""); status != http.StatusInternalServerError {
		t.Fatalf("list with an unreadable source: %d, want 500", status)
	}
	s.tick(t)
	if _, found, err := s.store.OpenRun(context.Background(), "library.discovery", ""); err != nil || found {
		t.Fatalf("scheduled with an unreadable source: found=%v err=%v", found, err)
	}
}

func TestAPendingRunWithNoFinishedRunKeepsItsDate(t *testing.T) {
	s := newSourcedJob(t, 15*time.Minute)
	_, next := s.firstPassAndSuccessor(t)
	if _, err := s.store.pool.Exec(context.Background(),
		`UPDATE job_runs SET scheduled_for = now() + interval '60 days' WHERE id = $1::uuid`, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.pool.Exec(context.Background(),
		`DELETE FROM job_runs WHERE job_id = 'library.discovery' AND state <> 'pending'`); err != nil {
		t.Fatal(err)
	}
	before := s.next(t).ScheduledFor
	s.tick(t)
	if got := s.next(t).ScheduledFor; !got.Equal(before) || s.execs.Load() != 1 {
		t.Fatalf("scheduled_for %v -> %v, execs %d: a pending run with no finished run to measure from moved", before, got, s.execs.Load())
	}
}
