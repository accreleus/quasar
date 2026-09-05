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

	h := &fleetHarness{pool: pool, store: store, cpCommit: cpCommit, cpInstallMode: InstallRegistry}
	h.release = seedRelease(t, store, commitB, buildinfo.Get().SchemaVersion)
	h.hostID = seedHost(t, pool, "gpu-fleet-01", commitA, "online")

	view := func(ctx context.Context) (View, error) {
		hosts, err := store.Hosts(ctx)
		if err != nil {
			return View{}, err
		}
		releases, err := store.Releases(ctx, ChannelStable)
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
		return PlanRelease(PlanInputs{
			Channel:                 ChannelStable,
			ControlPlane:            cp(h.cpCommit, buildinfo.Get().SchemaVersion),
			Hosts:                   hosts,
			Releases:                releases,
			OpenAttempts:            open,
			ActiveRun:               run,
			UpdaterPresent:          true,
			ControlPlaneInstallMode: nilIfEmpty(h.cpInstallMode),
		}), nil
	}

	noCordons := FleetCordons{
		Cordon:   func(context.Context, string) error { return nil },
		Uncordon: func(context.Context, string) error { return nil },
	}
	h.fleet = NewFleetRunner(store, drivers, drivers, ManifestOrEdge{}, noCordons, view, testLogger())
	h.fleet.PollWait = 5 * time.Millisecond
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
	// is nothing left to stop, which is its own refusal.
	current, err := h.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, raw = h.do(t, http.MethodPost, "/v1/admin/platform/apply/runs/"+run.ID+"/cancel", h.admin, nil)
	if TerminalRunState(current.State) {
		if code != http.StatusConflict || errCode(t, raw) != CodeRunNotActive {
			t.Fatalf("cancel of a finished run = %d %s, want 409 run_not_active", code, raw)
		}
	} else if code != http.StatusOK {
		t.Fatalf("second cancel = %d %s, want a 200 no-op", code, raw)
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
	run, err := h.store.CreateRun(ctx, h.release.ID, false, nil)
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
