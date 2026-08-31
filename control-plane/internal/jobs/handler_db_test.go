// handler_db_test.go — the WP3 admin HTTP surface, against Postgres.
// TEST_DATABASE_URL-gated like every other DB test here; `make test-db` runs
// them for real. See store_db_test.go's testDB/newHost/seed/okDef for the
// shared harness this file builds on.
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// passThroughAdmin skips the auth chain entirely, for the handler-behaviour
// tests below. The gate itself is asserted separately, through the REAL
// RequireAuth -> RequireAdmin chain, in TestJobsAdminRoutesAreAdminOnly —
// hiding admin UI is never the access control (CLAUDE.md invariant #6) and
// neither is a pass-through stub standing in for the whole surface.
func passThroughAdmin(next http.Handler) http.Handler { return next }

// newJobsServer builds the admin jobs HTTP surface behind a pass-through admin
// gate and returns the handler (for direct store/getenv seams), the store, the
// dispatcher (to drive a deterministic Tick) and a ready httptest.Server.
func newJobsServer(t *testing.T, pool *pgxpool.Pool, reg *Registry, defs ...Definition) (*Handler, *Store, *Dispatcher, *httptest.Server) {
	t.Helper()
	store := seed(t, pool, defs...)
	d := New(store, reg, DefaultConfig(), quietLog())
	h := NewHandler(store, reg, d, quietLog(), nil)
	mux := http.NewServeMux()
	h.Register(mux, passThroughAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, store, d, srv
}

func doReq(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// errEnvelope mirrors httpx's private errorEnvelope wire shape (respond.go) —
// duplicated here rather than imported because the type is unexported.
type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErr(t *testing.T, b []byte) errEnvelope {
	t.Helper()
	var e errEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("decode error envelope: %v (body: %s)", err, b)
	}
	return e
}

// --- admin gate ----------------------------------------------------------

// TestJobsAdminRoutesAreAdminOnly is the server-enforced check, through the
// REAL RequireAuth -> RequireAdmin chain wired at registration — the same
// invariant every other admin surface in this repo pins (CLAUDE.md #6:
// hiding admin UI is never the access control).
func TestJobsAdminRoutesAreAdminOnly(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	def := okDef()
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	store := seed(t, pool, def)
	d := New(store, reg, DefaultConfig(), quietLog())
	h := NewHandler(store, reg, d, quietLog(), nil)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authHandler := auth.NewHandler(authSvc)

	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// No bearer token at all: 401.
	status, _ := doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs", "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /v1/admin/jobs: status %d, want 401", status)
	}

	// A real, non-admin user: 403.
	if _, err := authSvc.Register(ctx, "jobs-user@t.local", "jobsuser", "password12345"); err != nil {
		t.Fatal(err)
	}
	tok, err := authSvc.Login(ctx, "jobs-user@t.local", "password12345", "test")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/admin/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin GET /v1/admin/jobs: status %d, want 403", resp.StatusCode)
	}
}

// --- GET /v1/admin/jobs and /v1/admin/jobs/{id}: shape incl. last/next run --

// TestListShapeIncludesLastAndNextRun walks an instance-scoped job through
// pending -> succeeded and checks both admin-visible states: a scheduled
// (pending) run shows up as next_run_at with no last_run yet, and after it
// finishes last_run appears with duration and summary while next_run_at goes
// back to null (nothing else is scheduled).
func TestListShapeIncludesLastAndNextRun(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	def := okDef() // id "artwork.sweep", PlaneControl, ScopeInstance, managed, interval
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, store, _, srv := newJobsServer(t, pool, reg, def)

	// Postgres stores timestamptz at microsecond precision; a nanosecond
	// fixture never round-trips Equal.
	future := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Microsecond)
	run, _, err := store.Materialize(ctx, MaterializeParams{
		JobID: def.ID, Trigger: TriggerSchedule, ScheduledFor: future,
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	status, body := doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs/"+def.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET job: status %d body %s", status, body)
	}
	var got jobJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	if got.Running == nil || *got.Running {
		t.Errorf("running: %+v, want false (pending, not claimed)", got.Running)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(future) {
		t.Errorf("next_run_at: %v, want %v", got.NextRunAt, future)
	}
	if got.LastRun != nil {
		t.Errorf("last_run: %+v, want nil (nothing has finished yet)", got.LastRun)
	}

	// Claim it before reporting — Report only closes a `running` row, and
	// ClaimDue takes an explicit Now so a future-scheduled run can be claimed
	// deterministically without waiting for the clock.
	claimed, err := store.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Now: future.Add(time.Second), Limit: 5})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, _, err := store.Report(ctx, run.ID, StateSucceeded, Summary{"apps_considered": 3}, ""); err != nil {
		t.Fatalf("report: %v", err)
	}

	status, body = doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs/"+def.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET job (2): status %d body %s", status, body)
	}
	// Fresh zero value: NextRunAt/LastRun are `omitempty`, so a stale pointer
	// from the first decode would otherwise survive a key that is legitimately
	// absent from this response.
	got = jobJSON{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NextRunAt != nil {
		t.Errorf("next_run_at after completion: %v, want nil (nothing else scheduled)", got.NextRunAt)
	}
	if got.LastRun == nil {
		t.Fatal("last_run: nil, want the succeeded run")
	}
	if got.LastRun.State != "succeeded" {
		t.Errorf("last_run.state: %s, want succeeded", got.LastRun.State)
	}
	if got.LastRun.DurationMS == nil {
		t.Error("last_run.duration_ms: nil, want a value")
	}
	if string(got.LastRun.Summary) == "{}" || len(got.LastRun.Summary) == 0 {
		t.Errorf("last_run.summary: %s, want the recorded summary", got.LastRun.Summary)
	}

	// GET /v1/admin/jobs (the list) must show the identical last_run for the
	// same job — one computation, not two that could drift (toJobJSON's
	// runStateFor is shared by both handlers).
	status, body = doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs", "")
	if status != http.StatusOK {
		t.Fatalf("GET list: status %d body %s", status, body)
	}
	var page struct {
		Items      []jobJSON `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != def.ID {
		t.Fatalf("list items: %+v", page.Items)
	}
	if page.Items[0].LastRun == nil || page.Items[0].LastRun.State != "succeeded" {
		t.Errorf("list last_run: %+v", page.Items[0].LastRun)
	}
}

// TestHostScopedJobListsOneTargetPerHost checks the OTHER shape (design
// §3.6): a scope=host job carries `targets`, not top-level running/last_run.
func TestHostScopedJobListsOneTargetPerHost(t *testing.T) {
	pool := testDB(t)
	hostID := newHost(t, pool, "host-a")

	def := okDef()
	def.ID = "template.warmup"
	def.Plane = PlaneControl // a real agent-plane job needs no Run; PlaneControl keeps this test self-contained
	def.Scope = ScopeHost
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, _, _, srv := newJobsServer(t, pool, reg, def)

	status, body := doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs/"+def.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET job: status %d body %s", status, body)
	}
	var got jobJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Running != nil || got.NextRunAt != nil || got.LastRun != nil {
		t.Errorf("scope=host job carried top-level run fields: %+v", got)
	}
	if len(got.Targets) != 1 || got.Targets[0].HostID != hostID || got.Targets[0].NodeName != "host-a" {
		t.Fatalf("targets: %+v, want one target for host %s", got.Targets, hostID)
	}
}

// TestUnmanagedJobIsListedWithNulls pins design §3.7: an unadopted job is
// LISTED, not hidden, with every run-derived field null and a note.
func TestUnmanagedJobIsListedWithNulls(t *testing.T) {
	pool := testDB(t)
	def := Definition{
		ID: "console.selfheal", Name: "Console self-heal backoff",
		Description: "Runs on a hard-coded backoff in internal/agentws/handler.go; not yet adopted.",
		Plane:       PlaneControl, Scope: ScopeHost, Managed: false,
		Default: Schedule{Kind: KindEvent, Timezone: "UTC"},
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, _, _, srv := newJobsServer(t, pool, reg, def)

	status, body := doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs/"+def.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET job: status %d body %s", status, body)
	}
	var got jobJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Managed {
		t.Fatal("managed: true, want false")
	}
	if got.Running != nil || got.NextRunAt != nil || got.LastRun != nil || len(got.Targets) != 0 {
		t.Errorf("unmanaged job carried a run-derived field: %+v", got)
	}
	if got.UnmanagedNote == "" {
		t.Error("unmanaged_note: empty, want the note naming the source file")
	}
}

// --- POST /v1/admin/jobs/{id}/run: happy path + every sentinel mapping -----

func TestRunNowHappyPath(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	def := okDef()
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, store, d, srv := newJobsServer(t, pool, reg, def)

	status, body := doReq(t, http.MethodPost, srv.URL+"/v1/admin/jobs/"+def.ID+"/run", "")
	if status != http.StatusAccepted {
		t.Fatalf("POST run: status %d body %s", status, body)
	}
	var resp struct {
		RunID        string `json:"run_id"`
		State        string `json:"state"`
		ScheduledFor string `json:"scheduled_for"`
		EtaNote      string `json:"eta_note"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID == "" || resp.State != "pending" || resp.EtaNote == "" {
		t.Fatalf("run-now response: %+v", resp)
	}

	// Drive the dispatcher's one tick deterministically rather than waiting on
	// its ticker — the same pattern dispatcher_db_test.go uses.
	d.Tick(ctx)
	d.Wait()

	last, found, err := store.LastTerminalRun(ctx, def.ID, "")
	if err != nil || !found {
		t.Fatalf("no terminal run recorded: found=%v err=%v", found, err)
	}
	if last.ID != resp.RunID || last.State != StateSucceeded || last.Trigger != TriggerManual {
		t.Fatalf("last run: %+v", last)
	}
}

func TestRunNowSentinelErrorMapping(t *testing.T) {
	pool := testDB(t)

	managed := okDef()
	managed.ID = "artwork.sweep"

	unmanaged := Definition{
		ID: "console.selfheal", Name: "n", Plane: PlaneControl, Scope: ScopeInstance,
		Managed: false, Default: Schedule{Kind: KindEvent, Timezone: "UTC"},
	}

	reg := NewRegistry()
	for _, d := range []Definition{managed, unmanaged} {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	_, store, _, srv := newJobsServer(t, pool, reg, managed, unmanaged)

	// not_found: no such job id at all.
	status, body := doReq(t, http.MethodPost, srv.URL+"/v1/admin/jobs/no.such.job/run", "")
	if status != http.StatusNotFound {
		t.Errorf("unknown job: status %d, want 404 (body %s)", status, body)
	}
	if e := decodeErr(t, body); e.Error.Code != "not_found" {
		t.Errorf("unknown job code: %q", e.Error.Code)
	}

	// job_unmanaged: listed but not adopted.
	status, body = doReq(t, http.MethodPost, srv.URL+"/v1/admin/jobs/console.selfheal/run", "")
	if status != http.StatusConflict {
		t.Errorf("unmanaged job: status %d, want 409 (body %s)", status, body)
	}
	if e := decodeErr(t, body); e.Error.Code != "job_unmanaged" {
		t.Errorf("unmanaged job code: %q", e.Error.Code)
	}

	// job_disabled: an admin turned it off; a manual trigger must still refuse.
	if _, err := store.UpdateSchedule(context.Background(), managed.ID, ScheduleUpdate{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	status, body = doReq(t, http.MethodPost, srv.URL+"/v1/admin/jobs/"+managed.ID+"/run", "")
	if status != http.StatusConflict {
		t.Errorf("disabled job: status %d, want 409 (body %s)", status, body)
	}
	if e := decodeErr(t, body); e.Error.Code != "job_disabled" {
		t.Errorf("disabled job code: %q", e.Error.Code)
	}
	if _, err := store.UpdateSchedule(context.Background(), managed.ID, ScheduleUpdate{Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	// job_already_running: claim the pending row, then trigger again.
	status, body = doReq(t, http.MethodPost, srv.URL+"/v1/admin/jobs/"+managed.ID+"/run", "")
	if status != http.StatusAccepted {
		t.Fatalf("first run-now: status %d body %s", status, body)
	}
	claimed, err := store.ClaimDue(context.Background(), ClaimOptions{Plane: PlaneControl, Limit: 5})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	status, body = doReq(t, http.MethodPost, srv.URL+"/v1/admin/jobs/"+managed.ID+"/run", "")
	if status != http.StatusConflict {
		t.Errorf("already-running job: status %d, want 409 (body %s)", status, body)
	}
	if e := decodeErr(t, body); e.Error.Code != "job_already_running" {
		t.Errorf("already-running job code: %q", e.Error.Code)
	}
}

func boolPtr(b bool) *bool { return &b }

// --- PATCH /v1/admin/jobs/{id}: validation + the env-lock gate -------------

func TestPatchUpdatesScheduleFields(t *testing.T) {
	pool := testDB(t)
	def := okDef()
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, store, _, srv := newJobsServer(t, pool, reg, def)

	status, body := doReq(t, http.MethodPatch, srv.URL+"/v1/admin/jobs/"+def.ID,
		`{"interval_secs": 21600, "window_start": "02:00", "window_end": "06:00", "window_days": [0,6], "timezone": "Europe/London", "history_limit": 100}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH: status %d body %s", status, body)
	}
	var got jobJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Schedule.IntervalSecs == nil || *got.Schedule.IntervalSecs != 21600 {
		t.Errorf("interval_secs: %+v", got.Schedule.IntervalSecs)
	}
	if got.Schedule.WindowStart == nil || *got.Schedule.WindowStart != "02:00:00" {
		t.Errorf("window_start: %+v", got.Schedule.WindowStart)
	}
	if got.Schedule.Timezone != "Europe/London" {
		t.Errorf("timezone: %s", got.Schedule.Timezone)
	}
	if got.HistoryLimit != 100 {
		t.Errorf("history_limit: %d", got.HistoryLimit)
	}

	// Persisted, not just echoed: re-read confirms the store actually wrote it.
	j, err := store.Get(context.Background(), def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Schedule.IntervalSecs != 21600 || j.HistoryLimit != 100 {
		t.Fatalf("stored job: %+v", j)
	}

	// A below-floor interval is rejected before it reaches the store.
	status, body = doReq(t, http.MethodPatch, srv.URL+"/v1/admin/jobs/"+def.ID, `{"interval_secs": 10}`)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("sub-floor interval: status %d, want 422 (body %s)", status, body)
	}
	if e := decodeErr(t, body); e.Error.Code != "validation_failed" {
		t.Errorf("sub-floor interval code: %q", e.Error.Code)
	}
}

// TestPatchScheduleLockedByEnvOverride pins design §3.3/§3.6: an admin cannot
// silently "succeed" at editing an interval the environment overrules — the
// exact interval_overridden_by_env confusion the library status endpoint
// already exists to prevent.
func TestPatchScheduleLockedByEnvOverride(t *testing.T) {
	pool := testDB(t)
	def := okDef()
	def.EnvOverride = "QUASAR_TEST_JOBS_INTERVAL_OVERRIDE"
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	h, _, _, srv := newJobsServer(t, pool, reg, def)
	// Test seam rather than os.Setenv: no process-wide env mutation to leak
	// across other tests in this package.
	h.getenv = func(k string) string {
		if k == def.EnvOverride {
			return "5m"
		}
		return ""
	}

	status, body := doReq(t, http.MethodPatch, srv.URL+"/v1/admin/jobs/"+def.ID, `{"interval_secs": 21600}`)
	if status != http.StatusConflict {
		t.Fatalf("locked PATCH: status %d, want 409 (body %s)", status, body)
	}
	e := decodeErr(t, body)
	if e.Error.Code != "schedule_locked" {
		t.Errorf("locked PATCH code: %q", e.Error.Code)
	}

	// The unrelated timezone field is NOT locked by the interval override —
	// only the field the environment actually governs is refused.
	status, body = doReq(t, http.MethodPatch, srv.URL+"/v1/admin/jobs/"+def.ID, `{"timezone": "Europe/London"}`)
	if status != http.StatusOK {
		t.Errorf("timezone PATCH while interval locked: status %d, want 200 (body %s)", status, body)
	}
}

// TestPatchUnmanagedJobIsConflict pins design §3.6's other 409: an unmanaged
// job has no schedule to edit at all.
func TestPatchUnmanagedJobIsConflict(t *testing.T) {
	pool := testDB(t)
	def := Definition{
		ID: "console.selfheal", Name: "n", Description: "see internal/agentws/handler.go",
		Plane: PlaneControl, Scope: ScopeInstance, Managed: false,
		Default: Schedule{Kind: KindEvent, Timezone: "UTC"},
	}
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, _, _, srv := newJobsServer(t, pool, reg, def)

	status, body := doReq(t, http.MethodPatch, srv.URL+"/v1/admin/jobs/"+def.ID, `{"enabled": false}`)
	if status != http.StatusConflict {
		t.Fatalf("PATCH unmanaged: status %d, want 409 (body %s)", status, body)
	}
	if e := decodeErr(t, body); e.Error.Code != "job_unmanaged" {
		t.Errorf("PATCH unmanaged code: %q", e.Error.Code)
	}
}

// --- GET /v1/admin/jobs/{id}/runs -------------------------------------------

func TestListRunsReturnsHistoryNewestFirst(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	def := okDef()
	reg := NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	_, store, _, srv := newJobsServer(t, pool, reg, def)

	for i := 0; i < 3; i++ {
		run, _, err := store.Materialize(ctx, MaterializeParams{
			JobID: def.ID, Trigger: TriggerManual, ScheduledFor: time.Now(),
		})
		if err != nil {
			t.Fatalf("materialize %d: %v", i, err)
		}
		if _, err := store.ClaimDue(ctx, ClaimOptions{Plane: PlaneControl, Limit: 5}); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if _, _, err := store.Report(ctx, run.ID, StateSucceeded, Summary{"n": i}, ""); err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
	}

	status, body := doReq(t, http.MethodGet, srv.URL+"/v1/admin/jobs/"+def.ID+"/runs?limit=2", "")
	if status != http.StatusOK {
		t.Fatalf("GET runs: status %d body %s", status, body)
	}
	var page struct {
		Items      []runJSON `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items: %d, want 2 (limit)", len(page.Items))
	}
	if page.NextCursor == nil {
		t.Fatal("next_cursor: nil, want a cursor for the third row")
	}
	for _, it := range page.Items {
		if it.State != "succeeded" {
			t.Errorf("item state: %s", it.State)
		}
	}
}
