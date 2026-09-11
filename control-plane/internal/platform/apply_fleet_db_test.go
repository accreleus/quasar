// The fleet half against a real Postgres: migration 0075's active-run index IS
// the run_active refusal, so it is exercised rather than asserted about, and
// the endpoints run behind the REAL RequireAuth→RequireAdmin chain.
package platform

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

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// parkedDrivers leave every attempt exactly where the sequencer put it, so a
// test can look at a run mid-flight.
type parkedDrivers struct{}

func (parkedDrivers) Start(Attempt)                  {}
func (parkedDrivers) UpdaterPresent() bool           { return true }
func (parkedDrivers) Apply(context.Context, Attempt) {}
func (parkedDrivers) Adopt(context.Context, Attempt, string) bool {
	return false
}

// succeedingDrivers resolve each attempt against the real store, which is what
// lets a DB test watch a whole run reach `succeeded`.
// The store is set after the harness builds it, which is why this is a pointer.
type succeedingDrivers struct{ store *Store }

func (d *succeedingDrivers) Start(a Attempt) {
	_, _ = d.store.SucceedAttempt(context.Background(), a.ID)
}
func (d *succeedingDrivers) UpdaterPresent() bool { return true }
func (d *succeedingDrivers) Apply(ctx context.Context, a Attempt) {
	_, _ = d.store.SucceedAttempt(ctx, a.ID)
}
func (d *succeedingDrivers) Adopt(ctx context.Context, a Attempt, _ string) bool {
	_, _ = d.store.SucceedAttempt(ctx, a.ID)
	return true
}

type fleetHarness struct {
	pool    *pgxpool.Pool
	store   *Store
	fleet   *FleetRunner
	base    string
	admin   string
	user    string
	release Release
	hostID  string
	// What this control plane reports it is on: commitA is "behind the
	// release", commitB is "already booted on it".
	cpCommit string
	// How this control plane got its own image; a source-built one is never
	// offered a registry image.
	cpInstallMode string
	cpBuiltAt     *string
	channel       string
}

// newFleetHarness wires the four fleet endpoints behind the real admin chain.
// The control-plane identity is synthesized: a test binary carries no build
// stamps, so every target would otherwise read control_plane_not_first.
func newFleetHarness(t *testing.T, cpCommit string, drivers interface {
	hostDriver
	selfDriver
}) *fleetHarness {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	h := &fleetHarness{pool: pool, store: store, cpCommit: cpCommit, cpInstallMode: InstallRegistry, channel: ChannelStable}
	h.release = seedRelease(t, store, commitB, buildinfo.Get().SchemaVersion)
	h.hostID = seedHost(t, pool, "gpu-fleet-01", commitA, "online")

	view := func(ctx context.Context) (View, error) {
		hosts, err := store.Hosts(ctx)
		if err != nil {
			return View{}, err
		}
		releases, err := store.Releases(ctx, h.channel)
		if err != nil {
			return View{}, err
		}
		open, err := store.OpenAttempts(ctx)
		if err != nil {
			return View{}, err
		}
		run, err := store.ActiveRun(ctx)
		if err != nil {
			return View{}, err
		}
		identity := cp(h.cpCommit, buildinfo.Get().SchemaVersion)
		identity.BuiltAt = h.cpBuiltAt
		return PlanRelease(PlanInputs{
			Channel:                 h.channel,
			ControlPlane:            identity,
			Hosts:                   hosts,
			Releases:                releases,
			OpenAttempts:            open,
			ActiveRun:               run,
			UpdaterPresent:          true,
			ControlPlaneInstallMode: nilIfEmpty(h.cpInstallMode),
		}), nil
	}

	// Real cordons: they move hosts.status, which is what the restore is read
	// back from.
	cordons := FleetCordons{
		Cordon: func(ctx context.Context, hostID string) error {
			_, err := pool.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id = $1::uuid`, hostID)
			return err
		},
		Uncordon: func(ctx context.Context, hostID string) error {
			_, err := pool.Exec(ctx, `UPDATE hosts SET status='online' WHERE id = $1::uuid`, hostID)
			return err
		},
		// Stands in for coordinator.DrainHost(force=true). The real one dispatches
		// session_stop and lets the agent report back; here the rows just end,
		// which is all this package needs to observe.
		Drain: func(ctx context.Context, hostID string) error {
			if _, err := pool.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id = $1::uuid`, hostID); err != nil {
				return err
			}
			_, err := pool.Exec(ctx, `
				UPDATE sessions SET state='stopped', ended_at=now()
				WHERE host_id = $1::uuid AND state NOT IN ('stopped','failed')`, hostID)
			return err
		},
	}
	h.fleet = NewFleetRunner(store, drivers, drivers, ManifestOrEdge{}, cordons, view, testLogger())
	h.fleet.AdoptSettle = time.Millisecond
	h.fleet.PollWait = 5 * time.Millisecond
	// Long enough that a test can observe the wait, short enough that a test
	// which never settles is not a 45 s stall.
	h.fleet.InFlightSettle = 3 * time.Second
	t.Cleanup(h.fleet.Close)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	authHandler := auth.NewHandler(authSvc)
	for _, u := range []struct{ email, name string }{
		{"fleet-admin@t.local", "fleetadmin"},
		{"fleet-user@t.local", "fleetuser"},
	} {
		if _, err := authSvc.Register(ctx, u.email, u.name, "password12345"); err != nil {
			t.Fatalf("register %s: %v", u.email, err)
		}
	}
	mustExec(t, pool, `UPDATE users SET role='admin' WHERE email='fleet-admin@t.local'`)
	adminTok, err := authSvc.Login(ctx, "fleet-admin@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "fleet-user@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}
	h.admin, h.user = adminTok.Plaintext, userTok.Plaintext

	mux := http.NewServeMux()
	NewApplyHandler(store, testRunner(store, ApplyDeps{
		Cordon:   func(context.Context, string) error { return nil },
		Uncordon: func(context.Context, string) error { return nil },
		Send:     func(context.Context, string, ApplyCommand) (Ack, error) { return Ack{OK: true}, nil },
	}), view, audit.NewStore(pool), testLogger()).
		WithFleet(h.fleet).
		Register(mux, func(next http.Handler) http.Handler {
			return authHandler.RequireAuth(authHandler.RequireAdmin(next))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.base = srv.URL
	return h
}

func (h *fleetHarness) do(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func decodeRun(t *testing.T, body []byte) ApplyRun {
	t.Helper()
	var env RunEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode run %s: %v", body, err)
	}
	return env.Run
}

func TestFleetApplyIsAdminOnlyAndRefusesASecondRun(t *testing.T) {
	h := newFleetHarness(t, commitA, parkedDrivers{})
	body := FleetApplyRequest{ReleaseID: h.release.ID}

	if code, _ := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.user, body); code != http.StatusForbidden {
		t.Fatalf("a non-admin got %d, want 403", code)
	}
	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	run := decodeRun(t, raw)
	if run.State != RunPending && run.State != RunRunning {
		t.Fatalf("run state = %q, want pending or running", run.State)
	}

	// The database's active-run index, not a code check.
	waitFor(t, "the run to reach its control-plane target", func() bool {
		r, err := h.store.Run(context.Background(), run.ID)
		return err == nil && r.State == RunRunning
	})
	code, raw = h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin, body)
	if code != http.StatusConflict {
		t.Fatalf("a second fleet apply = %d %s, want 409", code, raw)
	}
	if got := errCode(t, raw); got != CodeRunActive && got != CodeAttemptInFlight {
		t.Fatalf("refusal code = %q, want run_active or attempt_in_flight", got)
	}
}

// The live #117 failure: a source-built control plane was offered the registry
// image, which starts as a different uid and cannot write its own state. Nothing
// moves before the control plane, so the run must not start at all.
func TestFleetApplyRefusesASourceBuiltControlPlane(t *testing.T) {
	for _, tc := range []struct{ name, mode string }{
		{"source-built", InstallSource},
		{"install mode unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFleetHarness(t, commitA, parkedDrivers{})
			h.cpInstallMode = tc.mode

			code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
				FleetApplyRequest{ReleaseID: h.release.ID})
			if code != http.StatusConflict || errCode(t, raw) != CodeReleaseNotOffered {
				t.Fatalf("POST apply = %d %s, want 409 release_not_offered", code, raw)
			}
			runs, err := h.store.ListRuns(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Fatalf("runs = %+v, want none created", runs)
			}
		})
	}
}

func TestFleetRunsHistoryAndCancel(t *testing.T) {
	h := newFleetHarness(t, commitA, parkedDrivers{})
	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
		FleetApplyRequest{ReleaseID: h.release.ID, Force: true})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s", code, raw)
	}
	run := decodeRun(t, raw)
	waitFor(t, "the control-plane attempt to be created", func() bool {
		as, err := h.store.RunAttempts(context.Background(), run.ID)
		return err == nil && len(as) == 1
	})

	code, raw = h.do(t, http.MethodGet, "/v1/admin/platform/apply/runs", h.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("GET runs = %d %s", code, raw)
	}
	var list RunsResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].ID != run.ID {
		t.Fatalf("runs = %+v, want the one just created", list.Runs)
	}
	if !list.Runs[0].Force {
		t.Fatal("force was not recorded on the run")
	}
	if len(list.Runs[0].Attempts) != 1 || list.Runs[0].Attempts[0].Target != TargetControlPlane {
		t.Fatalf("attempts = %+v, want the control plane's, first", list.Runs[0].Attempts)
	}

	code, raw = h.do(t, http.MethodGet, "/v1/admin/platform/apply/runs/"+run.ID, h.admin, nil)
	if code != http.StatusOK || decodeRun(t, raw).ID != run.ID {
		t.Fatalf("GET run = %d %s", code, raw)
	}

	// A cancel sets the persisted flag and resolves the attempt it caught
	// before it was sent — nothing had been handed to the updater.
	code, raw = h.do(t, http.MethodPost, "/v1/admin/platform/apply/runs/"+run.ID+"/cancel", h.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %s", code, raw)
	}
	cancelled := decodeRun(t, raw)
	if !cancelled.CancelRequested || cancelled.CancelRequestedAt == nil {
		t.Fatalf("run = %+v, want the cancel flag persisted", cancelled)
	}
	if len(cancelled.Attempts) != 1 || cancelled.Attempts[0].State != AttemptCancelled {
		t.Fatalf("attempts = %+v, want the unsent one cancelled", cancelled.Attempts)
	}
	// Idempotent while the run is still active; once the run has stopped there
	// is nothing left to stop, which is its own refusal. The run is winding down
	// concurrently, so either answer is right — what is wrong is anything else.
	code, raw = h.do(t, http.MethodPost, "/v1/admin/platform/apply/runs/"+run.ID+"/cancel", h.admin, nil)
	switch {
	case code == http.StatusOK:
	case code == http.StatusConflict && errCode(t, raw) == CodeRunNotActive:
	default:
		t.Fatalf("second cancel = %d %s, want a 200 no-op or 409 run_not_active", code, raw)
	}

	if code, _ := h.do(t, http.MethodGet, "/v1/admin/platform/apply/runs/"+testRunID, h.admin, nil); code != http.StatusNotFound {
		t.Fatalf("GET an unknown run = %d, want 404", code)
	}
}

// The whole point of persisting the run: the control plane that boots on the
// new image resumes at the next target.
func TestFleetRunIsAdoptedAfterARestart(t *testing.T) {
	// The identity of a control plane that has already booted on the release.
	drivers := &succeedingDrivers{}
	h := newFleetHarness(t, commitB, drivers)
	drivers.store = h.store
	ctx := context.Background()

	// The state a restart leaves behind: a run mid-flight with the
	// control-plane attempt already resolved and no host reached.
	run, err := h.store.CreateRun(ctx, h.release.ID, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetRunTarget(ctx, run.ID, TargetControlPlane, nil); err != nil {
		t.Fatal(err)
	}
	a, err := h.store.CreateControlPlaneAttempt(ctx, NewControlPlaneAttempt{
		RunID: &run.ID, ReleaseID: &h.release.ID,
		Requested: []ComponentDigest{{Name: ComponentControlPlane, Image: "x", Digest: "sha256:" + hex64}},
		Previous:  []PreviousDigest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SucceedAttempt(ctx, a.ID); err != nil {
		t.Fatal(err)
	}

	// A NEW sequencer over the same database, as the next boot builds.
	h.fleet.Adopt(ctx)

	waitFor(t, "the adopted run to finish", func() bool {
		r, err := h.store.Run(ctx, run.ID)
		return err == nil && TerminalRunState(r.State)
	})
	final, err := h.store.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run state = %q (%v), want succeeded", final.State, final.Error)
	}
	attempts, err := h.store.RunAttempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[1].Target != TargetHost {
		t.Fatalf("attempts = %+v, want the control plane then the host", attempts)
	}
	if attempts[1].RunID == nil || *attempts[1].RunID != run.ID {
		t.Fatal("the host attempt does not name its run")
	}
}

// The live #117 failure: a run's control-plane step restarts this process, so
// the pre-run scheduling state cannot live in its memory. A failed run left
// both hosts `draining` with nothing left that knew to lift it.
func TestFleetRestoresTheCordonAcrossARestart(t *testing.T) {
	drivers := &succeedingDrivers{}
	h := newFleetHarness(t, commitB, drivers)
	drivers.store = h.store
	ctx := context.Background()

	// A second host, so the record has both shapes in it.
	other := seedHost(t, h.pool, "gpu-fleet-02", commitA, "online")

	run, err := h.store.CreateRun(ctx, h.release.ID, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// What the previous process wrote before it cordoned and replaced itself.
	if err := h.store.SetCordonedHosts(ctx, run.ID, []HostCordon{
		{HostID: h.hostID, WasCordoned: false},
		{HostID: other, WasCordoned: true},
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, h.pool, `UPDATE hosts SET status='draining'`)
	if err := h.store.SetRunTarget(ctx, run.ID, TargetControlPlane, nil); err != nil {
		t.Fatal(err)
	}
	a, err := h.store.CreateControlPlaneAttempt(ctx, NewControlPlaneAttempt{
		RunID: &run.ID, ReleaseID: &h.release.ID,
		Requested: []ComponentDigest{{Name: ComponentControlPlane, Image: "x", Digest: "sha256:" + hex64}},
		Previous:  []PreviousDigest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SucceedAttempt(ctx, a.ID); err != nil {
		t.Fatal(err)
	}

	// A NEW sequencer over the same database, as the next boot builds.
	h.fleet.Adopt(ctx)
	waitFor(t, "the adopted run to finish", func() bool {
		r, err := h.store.Run(ctx, run.ID)
		return err == nil && TerminalRunState(r.State)
	})

	// The run's own cordon is lifted whatever the outcome; the operator's stays.
	// The restore runs after the terminal write, so it is waited for rather than
	// read straight after.
	waitFor(t, "the run's cordon to be lifted", func() bool {
		status, err := h.store.HostStatus(ctx, h.hostID)
		return err == nil && status == "online"
	})
	if status, err := h.store.HostStatus(ctx, other); err != nil || status != "draining" {
		t.Fatalf("the operator's cordon = %q (%v), want it left in place", status, err)
	}
}

// The #140 incident: a v0.2.0 control plane started the run, cordoned the fleet
// and recreated itself before migration 0076 existed, so the adopting process
// finds every host draining and NO record. Inferring the operator's intent from
// those statuses left the whole fleet out of scheduling at finish.
func TestFleetAdoptedWithNoCordonRecordLeavesTheFleetOnline(t *testing.T) {
	drivers := &succeedingDrivers{}
	h := newFleetHarness(t, commitB, drivers)
	drivers.store = h.store
	ctx := context.Background()

	other := seedHost(t, h.pool, "gpu-fleet-02", commitA, "online")

	run, err := h.store.CreateRun(ctx, h.release.ID, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The old process cordoned the fleet and had no column to record it in.
	mustExec(t, h.pool, `UPDATE hosts SET status='draining'`)
	mustExec(t, h.pool, `UPDATE platform_apply_runs SET cordoned_hosts = '[]'::jsonb`)
	if err := h.store.SetRunTarget(ctx, run.ID, TargetControlPlane, nil); err != nil {
		t.Fatal(err)
	}
	// Left open: the recreate is what ended that process.
	if _, err := h.store.CreateControlPlaneAttempt(ctx, NewControlPlaneAttempt{
		RunID: &run.ID, ReleaseID: &h.release.ID,
		Requested: []ComponentDigest{{Name: ComponentControlPlane, Image: "x", Digest: "sha256:" + hex64}},
		Previous:  []PreviousDigest{},
	}); err != nil {
		t.Fatal(err)
	}

	h.fleet.Adopt(ctx)
	waitFor(t, "the adopted run to finish", func() bool {
		r, err := h.store.Run(ctx, run.ID)
		return err == nil && TerminalRunState(r.State)
	})
	waitFor(t, "the fleet to be back in scheduling", func() bool {
		one, err1 := h.store.HostStatus(ctx, h.hostID)
		two, err2 := h.store.HostStatus(ctx, other)
		return err1 == nil && err2 == nil && one == "online" && two == "online"
	})
}

func TestFleetApplyRejectsOlderEdgeBeforeCreatingRun(t *testing.T) {
	h := newFleetHarness(t, commitA, parkedDrivers{})
	h.channel = ChannelEdge
	h.cpBuiltAt = str(h.release.BuiltAt.Add(time.Hour).Format(time.RFC3339))
	mustExec(t, h.pool, `UPDATE platform_releases SET channel = 'edge', version = NULL, manifest = NULL WHERE id = $1`, h.release.ID)
	for _, force := range []bool{false, true} {
		code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
			FleetApplyRequest{ReleaseID: h.release.ID, Force: force})
		if code != http.StatusConflict || errCode(t, raw) != CodeReleaseNotOffered {
			t.Fatalf("force=%v: POST older edge = %d %s, want 409 release_not_offered", force, code, raw)
		}
	}
	runs, err := h.store.ListRuns(context.Background(), 10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs = %+v, err = %v; want no run created", runs, err)
	}
}

// sessionStates is every session row's state, so a test can say what a fleet
// run did to the sessions that were live while it ran.
func sessionStates(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT state FROM sessions ORDER BY created_at`)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan session: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// #153's acceptance line: a fleet apply whose release runs no migration leaves
// running sessions running, and reports no `sessions_remaining` it is not
// waiting on.
func TestFleetHoldsRunningSessionsWhenTheReleaseRunsNoMigration(t *testing.T) {
	drivers := &succeedingDrivers{}
	// commitA is BEHIND the release, so the run really takes the control-plane
	// step; the harness seeds that release at this binary's own schema version,
	// which is the "runs no migration" case.
	h := newFleetHarness(t, commitA, drivers)
	drivers.store = h.store
	seedSession(t, h.pool, h.hostID)

	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
		FleetApplyRequest{ReleaseID: h.release.ID})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	run := decodeRun(t, raw)

	// Nothing ever ends that session, so terminating at all is the assertion.
	waitFor(t, "the run to finish without the fleet emptying", func() bool {
		r, err := h.store.Run(context.Background(), run.ID)
		return err == nil && TerminalRunState(r.State)
	})
	final, err := h.store.Run(context.Background(), run.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("run state = %q (%v), want succeeded", final.State, err)
	}
	if got := sessionStates(t, h.pool); len(got) != 1 || got[0] != "running" {
		t.Fatalf("session states = %v, want the one that was running to still be running", got)
	}
	as, err := h.store.RunAttempts(context.Background(), run.ID)
	if err != nil || len(as) == 0 || as[0].Target != TargetControlPlane {
		t.Fatalf("attempts = %+v (%v), want the control plane's first", as, err)
	}
	if as[0].State != AttemptSucceeded {
		t.Fatalf("control-plane attempt state = %q, want succeeded", as[0].State)
	}
	if as[0].SessionsRemaining != nil {
		t.Fatalf("sessions_remaining = %d, want null: the step waited on nothing and ended nothing",
			*as[0].SessionsRemaining)
	}
}

// The other half: a release carrying a migration still drains the whole fleet
// first, and waits rather than stopping anything.
func TestFleetStillDrainsWhenTheReleaseCarriesAMigration(t *testing.T) {
	h := newFleetHarness(t, commitA, parkedDrivers{})
	migrating := seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion+1)
	seedSession(t, h.pool, h.hostID)

	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
		FleetApplyRequest{ReleaseID: migrating.ID})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	run := decodeRun(t, raw)

	waitFor(t, "the control-plane attempt to wait on the whole fleet", func() bool {
		as, err := h.store.RunAttempts(context.Background(), run.ID)
		return err == nil && len(as) == 1 && as[0].State == AttemptWaitingSessions &&
			as[0].SessionsRemaining != nil && *as[0].SessionsRemaining == 1
	})
	if got := sessionStates(t, h.pool); len(got) != 1 || got[0] != "running" {
		t.Fatalf("session states = %v, want the drain to WAIT rather than stop anything", got)
	}
}

// force on a migrating release must END the sessions, not merely skip the wait:
// since #128 the recreate no longer ends them, so nothing else would (#153).
func TestFleetForceEndsSessionsBeforeAMigratingControlPlaneStep(t *testing.T) {
	drivers := &succeedingDrivers{}
	h := newFleetHarness(t, commitA, drivers)
	drivers.store = h.store
	migrating := seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion+1)
	seedSession(t, h.pool, h.hostID)

	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
		FleetApplyRequest{ReleaseID: migrating.ID, Force: true})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	run := decodeRun(t, raw)

	waitFor(t, "the forced run to finish", func() bool {
		r, err := h.store.Run(context.Background(), run.ID)
		return err == nil && TerminalRunState(r.State)
	})
	final, err := h.store.Run(context.Background(), run.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("run state = %q (%v), want succeeded", final.State, err)
	}
	// The session the operator agreed to lose is actually gone BEFORE the
	// migration ran, which is the property the old fleet-wide drain provided.
	if got := sessionStates(t, h.pool); len(got) != 1 || got[0] == "running" {
		t.Fatalf("session states = %v, want the forced drain to have ended it", got)
	}
}

// A non-migrating step no longer drains, but an in-flight launch is still
// reaped by the reconnecting agent — so the step waits for it to settle (#153).
func TestFleetWaitsForAnInFlightLaunchOnANonMigratingStep(t *testing.T) {
	drivers := &succeedingDrivers{}
	// commitA is behind the release, and the harness seeds the release at this
	// binary's own schema version: the release runs no migration.
	h := newFleetHarness(t, commitA, drivers)
	drivers.store = h.store
	seedSession(t, h.pool, h.hostID)
	// Put it mid-launch: `starting` is what session.Store.ReapHostExceptRunning
	// fails on the new binary's first agent reconnect.
	mustExec(t, h.pool, `UPDATE sessions SET state='starting'`)

	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
		FleetApplyRequest{ReleaseID: h.release.ID})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	run := decodeRun(t, raw)

	// The fleet is cordoned, and nothing has been sent while the launch is in
	// flight — but the attempt is NOT in waiting_sessions: nothing is being lost
	// and no operator consented to anything.
	waitFor(t, "the fleet to be cordoned", func() bool {
		var n int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM hosts WHERE status='draining'`).Scan(&n); err != nil {
			return false
		}
		return n > 0
	})
	as, err := h.store.RunAttempts(context.Background(), run.ID)
	if err != nil || len(as) != 1 || as[0].Target != TargetControlPlane {
		t.Fatalf("attempts = %+v (%v), want just the control plane's", as, err)
	}
	if as[0].State == AttemptWaitingSessions {
		t.Fatal("a non-migrating step must not enter waiting_sessions: nothing is being lost")
	}
	if as[0].SessionsRemaining != nil {
		t.Fatalf("sessions_remaining = %d, want null", *as[0].SessionsRemaining)
	}

	// The launch lands. It is `running` now, so it survives the recreate.
	mustExec(t, h.pool, `UPDATE sessions SET state='running'`)
	waitFor(t, "the run to finish once the launch settled", func() bool {
		r, err := h.store.Run(context.Background(), run.ID)
		return err == nil && TerminalRunState(r.State)
	})
	final, err := h.store.Run(context.Background(), run.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("run state = %q (%v), want succeeded", final.State, err)
	}
	if got := sessionStates(t, h.pool); len(got) != 1 || got[0] != "running" {
		t.Fatalf("session states = %v, want the launch to have survived", got)
	}
}

// Migration 0083: the persisted skip list, the partial state and retry_of all
// round-trip through the row, and a skip is recorded once per host however
// many times a re-adopted run re-walks its list.
func TestRunSkipsPartialStateAndRetryOfRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})

	first, err := h.store.CreateRun(ctx, h.release.ID, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	skip := RunSkip{HostID: h.hostID, NodeName: "gpu-01", Reason: ReasonPreflightBlocked}
	for i := 0; i < 2; i++ {
		if err := h.store.RecordSkip(ctx, first.ID, skip); err != nil {
			t.Fatalf("record skip: %v", err)
		}
	}
	if err := h.store.FinishRun(ctx, first.ID, RunSucceededPartial, ""); err != nil {
		t.Fatalf("finish partial: %v", err)
	}
	got, err := h.store.Run(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RunSucceededPartial || got.FinishedAt == nil {
		t.Fatalf("run = %+v, want terminal succeeded_partial", got)
	}
	if len(got.Skipped) != 1 || got.Skipped[0] != skip {
		t.Fatalf("skipped = %+v, want exactly one entry for the host", got.Skipped)
	}
	if got.RetryOf != nil {
		t.Fatalf("retry_of = %v on a plain run, want null", *got.RetryOf)
	}
	// A terminal partial run no longer owns the fleet: a retry can start.
	if active, err := h.store.ActiveRun(ctx); err != nil || active != nil {
		t.Fatalf("active run after a partial finish = %v (err %v), want none", active, err)
	}

	retry, err := h.store.CreateRun(ctx, h.release.ID, false, nil, &first.ID)
	if err != nil {
		t.Fatalf("create retry: %v", err)
	}
	if retry.RetryOf == nil || *retry.RetryOf != first.ID {
		t.Fatalf("retry_of = %v, want %s", retry.RetryOf, first.ID)
	}
	if err := h.store.FinishRun(ctx, retry.ID, RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	// Deleting the original leaves the retry standing, unlinked.
	mustExec(t, h.pool, `DELETE FROM platform_apply_runs WHERE id = $1::uuid`, first.ID)
	again, err := h.store.Run(ctx, retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.RetryOf != nil {
		t.Fatalf("retry_of after the original was deleted = %v, want null", *again.RetryOf)
	}

	// And the attempt kind CHECK admits auto_revert.
	a, err := h.store.CreateAutoRevertAttempt(ctx, NewAutoRevert{
		Failed:    Attempt{HostID: &h.hostID, RunID: &retry.ID},
		Requested: []ComponentDigest{{Name: ComponentNodeAgent, Image: "x", Digest: "sha256:" + hex64}},
		Previous:  []PreviousDigest{{Name: ComponentNodeAgent}},
		Succeeded: false, Output: "restore failed too",
	})
	if err != nil {
		t.Fatalf("auto_revert row: %v", err)
	}
	if a.Kind != KindAutoRevert || a.State != AttemptFailed || a.Reason == nil {
		t.Fatalf("row = %+v, want a failed auto_revert with a reason", a)
	}
}
