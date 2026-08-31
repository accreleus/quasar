package library

// forcescan_db_test.go — the operator-facing "scan now" (POST
// /v1/admin/library/scan) and the on-enable nudge.
//
// THE DEFECT BOTH OF THESE CLOSE: discovery is paced by two independent
// six-hourly timers — the control-plane janitor and the node-agent's poll — and
// each is anchored to ITS OWN PROCESS BOOT rather than to the moment an operator
// enables the feature. The switch is read per pass, so no restart is needed; but
// the pass that read `false` has already scheduled its successor six hours out.
// Switching discovery on could therefore take most of a day to produce a single
// tile, which from the outside is indistinguishable from a broken feature.
//
// TEST_DATABASE_URL-gated like every other DB test here: without it these SKIP.
// Use `scripts/dev/dev.sh go-test-db`.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
	"github.com/accreleus/quasar/control-plane/internal/settings"
)

// --- harness -----------------------------------------------------------------

// newScanServer builds the library HTTP surface with a PASS-THROUGH admin
// middleware, so the behavioural tests below exercise the handler rather than
// the gate. The gate itself is asserted separately, with the real
// RequireAuth→RequireAdmin chain, in TestForceScanIsAdminOnly — hiding admin UI
// is never the access control (CLAUDE.md invariant #6) and neither is a
// pass-through stub.
//
// interval is a parameter because QUASAR_LIBRARY_SCAN_INTERVAL=0 is one of the
// three gates a force scan must still respect — it is wired in as a forced
// env override on the resolver, the same "env wins" rule production uses, so
// passing 0 here reproduces the real kill-switch path end to end.
func newScanServer(t *testing.T, f fixture, set *fakeSettings, interval time.Duration) *httptest.Server {
	t.Helper()
	resolver := NewResolver(set, true, interval, false, false)
	h := NewHandler(f.store, testStorageManager(f, set), set,
		NewAppDetails(false, quietLogger()), resolver, quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// forceScanResponse is the wire shape of POST /v1/admin/library/scan.
type forceScanResponse struct {
	Queued      int    `json:"queued"`
	Skipped     int    `json:"skipped"`
	Eligible    int    `json:"eligible"`
	InertReason string `json:"inert_reason"`
}

// postScan presses the button. body "" sends no JSON at all, which is the
// unscoped "scan everything now" an operator's button produces.
func postScan(t *testing.T, srv *httptest.Server, body string) (int, forceScanResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/admin/library/scan", strings.NewReader(body))
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer resp.Body.Close()
	var out forceScanResponse
	if resp.StatusCode == http.StatusOK {
		must(t, json.NewDecoder(resp.Body).Decode(&out))
	}
	return resp.StatusCode, out
}

// --- what force means --------------------------------------------------------

// TestForceScanIgnoresTheRecencyCheck IS THE TEST THAT DISTINGUISHES FORCE FROM
// THE JANITOR. Enqueue refuses a triple whose last scan succeeded inside the
// interval — that predicate is the whole pacing mechanism — and an operator
// pressing "scan now" is overriding exactly that, because they know something
// the recency rule cannot (they just installed a game, or just fixed a mount).
//
// If this test ever passes trivially because the janitor would have enqueued
// anyway, it is asserting nothing; hence the explicit "the janitor declines
// here" assertion in the middle.
func TestForceScanIgnoresTheRecencyCheck(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	set := &fakeSettings{enabled: true}

	// A scan that succeeded seconds ago.
	j := NewJanitor(f.store, set, newTestResolver(set), quietLogger())
	j.RunOnce(ctx)
	must(t, execT(ctx, pool, `UPDATE library_scans SET state='reported', reported_at=now()`))

	// The janitor declines: this is the pacing rule doing its job.
	j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 0 {
		t.Fatalf("janitor enqueued %d scans inside the interval, want 0 — this test is no longer testing force", n)
	}

	// The operator does not.
	code, res := postScan(t, newScanServer(t, f, set, 6*time.Hour), "")
	if code != http.StatusOK {
		t.Fatalf("force scan: status %d, want 200", code)
	}
	if res.Queued != 1 || res.Skipped != 0 {
		t.Fatalf("force scan queued=%d skipped=%d, want 1/0 (the recency check must not apply)", res.Queued, res.Skipped)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 1 {
		t.Fatalf("pending scans after a force = %d, want 1", n)
	}
}

// TestForceScanQueuesEveryEligibleTriple — unscoped, the population is the same
// one the janitor's enqueue step walks.
func TestForceScanQueuesEveryEligibleTriple(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	f.homeFor(t, f.other, "/homes/opaque-b")

	code, res := postScan(t, newScanServer(t, f, &fakeSettings{enabled: true}, 6*time.Hour), "")
	if code != http.StatusOK {
		t.Fatalf("force scan: status %d, want 200", code)
	}
	if res.Queued != 2 || res.Eligible != 2 {
		t.Fatalf("force scan queued=%d eligible=%d, want 2/2", res.Queued, res.Eligible)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 2 {
		t.Fatalf("pending scans = %d, want 2", n)
	}
}

// TestForceScanScopesByUser.
func TestForceScanScopesByUser(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	f.homeFor(t, f.other, "/homes/opaque-b")

	code, res := postScan(t, newScanServer(t, f, &fakeSettings{enabled: true}, 6*time.Hour),
		`{"user_id":"`+f.user+`"}`)
	if code != http.StatusOK {
		t.Fatalf("force scan: status %d, want 200", code)
	}
	if res.Queued != 1 || res.Eligible != 1 {
		t.Fatalf("user-scoped force queued=%d eligible=%d, want 1/1", res.Queued, res.Eligible)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE user_id=$1::uuid`, f.user); n != 1 {
		t.Errorf("scans for the scoped user = %d, want 1", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE user_id=$1::uuid`, f.other); n != 0 {
		t.Errorf("scans for the OTHER user = %d, want 0 — the scope must narrow", n)
	}
}

// TestForceScanScopesByApp. Two provider apps, one user, one host: the scope
// must pick out one of the two.
func TestForceScanScopesByApp(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	var second string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, library_provider, managed_home, profile_policy)
		VALUES ('Steam (second)', 'steam', true, 'inherit') RETURNING id::text`).Scan(&second))
	must(t, execT(ctx, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/opaque-second')`, f.user, second, f.host))

	code, res := postScan(t, newScanServer(t, f, &fakeSettings{enabled: true}, 6*time.Hour),
		`{"app_id":"`+second+`"}`)
	if code != http.StatusOK {
		t.Fatalf("force scan: status %d, want 200", code)
	}
	if res.Queued != 1 || res.Eligible != 1 {
		t.Fatalf("app-scoped force queued=%d eligible=%d, want 1/1", res.Queued, res.Eligible)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE app_id=$1::uuid`, second); n != 1 {
		t.Errorf("scans for the scoped app = %d, want 1", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE app_id=$1::uuid`, f.parent); n != 0 {
		t.Errorf("scans for the OTHER provider app = %d, want 0 — the scope must narrow", n)
	}
}

// TestForceScanDoublePressQueuesNoDuplicates — library_scans_open_uk permits one
// open scan per triple, so the second press is a normal outcome and must be
// reported as skipped, not as an error and not as a duplicate row.
func TestForceScanDoublePressQueuesNoDuplicates(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	srv := newScanServer(t, f, &fakeSettings{enabled: true}, 6*time.Hour)

	code, first := postScan(t, srv, "")
	if code != http.StatusOK || first.Queued != 1 {
		t.Fatalf("first press: status %d queued %d, want 200/1", code, first.Queued)
	}
	code, second := postScan(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("second press: status %d, want 200 (a double-click is not an error)", code)
	}
	if second.Queued != 0 || second.Skipped != 1 {
		t.Fatalf("second press queued=%d skipped=%d, want 0/1", second.Queued, second.Skipped)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 1 {
		t.Fatalf("scan rows after two presses = %d, want 1", n)
	}
}

// --- a force scan is not a bypass of the gates -------------------------------

// TestForceScanIsInertWithAStatedReason — the three gates a force scan still
// respects, and the reason it must give for each. A 200 reading "queued 0" with
// no reason is the silent-nothing this whole feature keeps having to design
// against; that is why every case asserts on inert_reason and not just the count.
func TestForceScanIsInertWithAStatedReason(t *testing.T) {
	cases := []struct {
		name     string
		set      *fakeSettings
		interval time.Duration
		wantIn   string
	}{
		{"switch off", &fakeSettings{enabled: false}, 6 * time.Hour, "switched off"},
		{"interval 0", &fakeSettings{enabled: true}, 0, "QUASAR_LIBRARY_SCAN_INTERVAL"},
		// #473 hard removal (2026-08-25): the docker-volume driver is gone, so a
		// legacy "volume" provider value (settings validation rejects writing it
		// going forward; this simulates a stale row/double bypassing that) no
		// longer gets its own reason — resolveDriver rejects it unconditionally,
		// which noHostHasStorageRoot treats the same as "no root", so it reports
		// the same reason a rootless instance gets.
		{"volume provider (never resolves, reported as no storage root)",
			&fakeSettings{enabled: true, provider: "volume"}, 6 * time.Hour, "storage root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			f := newFixture(t, pool)

			code, res := postScan(t, newScanServer(t, f, tc.set, tc.interval), "")
			if code != http.StatusOK {
				t.Fatalf("status %d, want 200", code)
			}
			if res.Queued != 0 {
				t.Errorf("queued %d while inert, want 0", res.Queued)
			}
			if !strings.Contains(res.InertReason, tc.wantIn) {
				t.Errorf("inert_reason = %q, want it to mention %q", res.InertReason, tc.wantIn)
			}
			if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 0 {
				t.Errorf("scan rows written while inert = %d, want 0", n)
			}
		})
	}
}

// TestForceScanSaysWhenTheScopeMatchedNothing — "everything was already queued"
// and "your scope matches no managed home" are both zero, and an operator has to
// be able to tell them apart: one means wait, the other means fix the scope.
func TestForceScanSaysWhenTheScopeMatchedNothing(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	code, res := postScan(t, newScanServer(t, f, &fakeSettings{enabled: true}, 6*time.Hour),
		`{"user_id":"`+f.other+`"}`) // f.other has no home
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if res.Queued != 0 || res.Eligible != 0 {
		t.Fatalf("queued=%d eligible=%d, want 0/0", res.Queued, res.Eligible)
	}
	if res.InertReason == "" {
		t.Fatal("a force scan that matched nothing returned no reason — this is the silent nothing the feature exists to avoid")
	}
}

// TestForceScanRejectsABadScope — a malformed uuid is a 400, not a 500, and a
// non-provider app gets the same "this app is not a library provider" 400 the
// other four /library/* admin routes give.
func TestForceScanRejectsABadScope(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	srv := newScanServer(t, f, &fakeSettings{enabled: true}, 6*time.Hour)

	if code, _ := postScan(t, srv, `{"app_id":"not-a-uuid"}`); code != http.StatusBadRequest {
		t.Errorf("malformed app_id: status %d, want 400", code)
	}
	if code, _ := postScan(t, srv, `{"user_id":"nope"}`); code != http.StatusBadRequest {
		t.Errorf("malformed user_id: status %d, want 400", code)
	}
	if code, _ := postScan(t, srv, `{`); code != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", code)
	}

	var plain string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, managed_home) VALUES ('Plain app', true)
		RETURNING id::text`).Scan(&plain))
	if code, _ := postScan(t, srv, `{"app_id":"`+plain+`"}`); code != http.StatusBadRequest {
		t.Errorf("non-provider app_id: status %d, want 400", code)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 0 {
		t.Errorf("scan rows written by rejected requests = %d, want 0", n)
	}
}

// TestForceScanIsAdminOnly — SERVER-ENFORCED, through the real
// RequireAuth→RequireAdmin chain wired at route registration. Invariant #6: the
// /admin route gating in the UI is UX, never the access control.
func TestForceScanIsAdminOnly(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	authHandler := auth.NewHandler(authSvc)

	fs := &fakeSettings{enabled: true}
	h := NewHandler(f.store, testStorageManager(f, fs), fs,
		NewAppDetails(false, quietLogger()), newTestResolver(fs), quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/library/scan", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated force scan: status %d, want 401", rr.Code)
	}

	_, err = authSvc.Register(ctx, "force-user@t.local", "forceuser", "password12345")
	must(t, err)
	tok, err := authSvc.Login(ctx, "force-user@t.local", "password12345", "test")
	must(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/library/scan", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin force scan: status %d, want 403", rr.Code)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 0 {
		t.Errorf("a rejected request queued %d scans, want 0", n)
	}
}

// --- a stuck claim must not outlive the button -------------------------------

// newAdminScanHarness wires the force-scan route behind the REAL
// RequireAuth→RequireAdmin chain and returns a post function that presses the
// button as a real admin, bearer token and all.
//
// The other behavioural tests here use a pass-through admin stub; this one does
// not, because the defect it covers is an OPERATOR-PATH defect — the claim that
// the button now clears a stuck scan is only worth anything if it is made through
// the same handler stack an operator's click actually traverses.
func newAdminScanHarness(t *testing.T, f fixture, set *fakeSettings, interval time.Duration) func(t *testing.T, body string) (int, forceScanResponse) {
	t.Helper()
	ctx := context.Background()

	authSvc, err := auth.NewService(f.pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	authHandler := auth.NewHandler(authSvc)
	_, err = authSvc.Register(ctx, "reap-admin@t.local", "reapadmin", "password12345")
	must(t, err)
	must(t, execT(ctx, f.pool, `UPDATE users SET role='admin' WHERE email='reap-admin@t.local'`))
	tok, err := authSvc.Login(ctx, "reap-admin@t.local", "password12345", "test")
	must(t, err)

	h := NewHandler(f.store, testStorageManager(f, set), set,
		NewAppDetails(false, quietLogger()), NewResolver(set, true, interval, false, false), quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return func(t *testing.T, body string) (int, forceScanResponse) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/admin/library/scan", strings.NewReader(body))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		defer resp.Body.Close()
		var out forceScanResponse
		if resp.StatusCode == http.StatusOK {
			must(t, json.NewDecoder(resp.Body).Decode(&out))
		}
		return resp.StatusCode, out
	}
}

// scanStateFor reports the state of the single scan on a triple.
func scanStateFor(t *testing.T, pool *pgxpool.Pool, user, app, host string) string {
	t.Helper()
	var state string
	must(t, pool.QueryRow(context.Background(), `
		SELECT state FROM library_scans
		 WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`,
		user, app, host).Scan(&state))
	return state
}

// TestForceScanClearsAnAbandonedClaim IS THE REGRESSION GATE for the stuck claim
// that took a home out of discovery for six hours.
//
// library_scans_open_uk is partial on ('pending','claimed'), so ONE scan stranded
// in 'claimed' blocks every subsequent enqueue for its (user, app, host) triple.
// A scan strands whenever the reconcile of its report fails — which is exactly
// what the ::uuid defect above caused. ReapClaimed was called ONLY from the
// janitor pass, and the janitor is on the six-hour timer, so the operator's one
// recovery lever was unreachable from the operator's one recovery button: four
// consecutive presses of "Scan now" reported queued=2 skipped=1 every time, and
// the skipped triple was the only home with any games in it.
//
// WHAT THE ASSERTION IS, AND WHY IT IS NOT "queued". Reaping moves the row
// 'claimed' → 'pending'; both states are inside the partial index, so the row
// still absorbs ForceEnqueue's ON CONFLICT DO NOTHING and the response still
// reads skipped=1. The counter is therefore NOT the discriminator and a test
// asserting queued=1 here would fail against the FIXED code. What the fix
// actually changes is the state machine: the triple goes from permanently wedged
// (nothing ever claims a 'claimed' row) to claimable by the next agent poll. So
// the gate is the scan's state and its claimability, which is the thing the
// operator actually needed.
func TestForceScanClearsAnAbandonedClaim(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	post := newAdminScanHarness(t, f, &fakeSettings{enabled: true}, 6*time.Hour)

	// A scan whose agent died — or whose report failed to reconcile — long enough
	// ago that no agent could still be walking it.
	must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now() - interval '31 minutes')`,
		f.user, f.parent, f.host))

	// Precondition: this triple is wedged. Nothing can claim it.
	stuck, err := f.store.ClaimPending(ctx, f.host)
	must(t, err)
	if len(stuck) != 0 {
		t.Fatalf("claimed %d scans before the force press, want 0 — the triple is supposed to be wedged", len(stuck))
	}

	code, res := post(t, "")
	if code != http.StatusOK {
		t.Fatalf("force scan: status %d, want 200", code)
	}
	if res.Eligible != 1 {
		t.Fatalf("force scan eligible=%d, want 1", res.Eligible)
	}

	// THE GATE: the stranded claim was returned to 'pending' by the button.
	if state := scanStateFor(t, pool, f.user, f.parent, f.host); state != "pending" {
		t.Fatalf("scan state after a force press = %q, want \"pending\" — a claim stranded past "+
			"ClaimTTL must be reaped by the button, not left for the six-hourly janitor. "+
			"library_scans_open_uk is partial on ('pending','claimed'), so while it sits in "+
			"'claimed' this triple can never be enqueued OR claimed again", state)
	}

	// ...and the triple is genuinely unblocked: an agent's next poll picks it up.
	again, err := f.store.ClaimPending(ctx, f.host)
	must(t, err)
	if len(again) != 1 {
		t.Fatalf("an agent claimed %d scans after the force press, want 1 — reaping to 'pending' "+
			"is only a fix if the scan actually becomes claimable", len(again))
	}

	// Exactly one row: the reap recycles the stranded scan rather than adding a
	// second one alongside it.
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 1 {
		t.Errorf("scan rows = %d, want 1 (the stranded row is recycled, not duplicated)", n)
	}
}

// TestForceScanDoesNotReapALiveClaim is the guard the reap above depends on.
//
// A scan claimed INSIDE ClaimTTL belongs to an agent that is plausibly still
// walking the home right now. Returning it to 'pending' would hand the same scan
// to a second agent, and the loser's report would then hit a scan that is no
// longer 'claimed'. The 30-minute window is the entire reason it is safe to reap
// from an operator-triggered path at all, so it is asserted here rather than left
// implicit — a reap that ignored the TTL would still make the test above pass.
func TestForceScanDoesNotReapALiveClaim(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	post := newAdminScanHarness(t, f, &fakeSettings{enabled: true}, 6*time.Hour)

	must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now() - interval '5 minutes')`,
		f.user, f.parent, f.host))

	code, res := post(t, "")
	if code != http.StatusOK {
		t.Fatalf("force scan: status %d, want 200", code)
	}
	if res.Queued != 0 || res.Skipped != 1 {
		t.Errorf("force scan queued=%d skipped=%d, want 0/1 — a live claim still holds the triple",
			res.Queued, res.Skipped)
	}
	if state := scanStateFor(t, pool, f.user, f.parent, f.host); state != "claimed" {
		t.Fatalf("scan state after a force press = %q, want \"claimed\" — a scan claimed inside "+
			"ClaimTTL may still have an agent walking it, and reaping it would run the same scan twice", state)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 0 {
		t.Errorf("pending scans = %d, want 0 — a live claim must not be recycled", n)
	}
}

// --- the on-enable nudge -----------------------------------------------------
//
// Since the jobs-framework adoption (§8.2) the janitor no longer owns a
// timer or a nudge channel: cmd/quasar-control/app.go wires
// settingsHandler.OnLibraryDiscoveryEnabled to a non-blocking
// jobsDispatcher.Enqueue("library.discovery", ...) call instead of
// Janitor.Nudge, and Materialize's single-open-run invariant does the
// coalescing a capacity-1 channel used to do by hand.

// nudgeHarness wires the REAL settings handler to a REAL jobs.Dispatcher over
// a library.discovery Definition whose RunFunc is the REAL janitor's RunOnce —
// the same shape cmd/quasar-control/app.go wires — behind the real admin gate,
// and hands back a function that performs an admin PATCH /v1/admin/settings.
type nudgeHarness struct {
	dispatcher *jobs.Dispatcher
	patch      func(t *testing.T, body string) int
}

func newNudgeHarness(t *testing.T, f fixture, interval time.Duration) nudgeHarness {
	t.Helper()
	ctx := context.Background()
	pool := f.pool

	setStore := settings.NewStore(pool)
	must(t, setStore.Seed(ctx, settings.RegistrationClosed))

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	authHandler := auth.NewHandler(authSvc)
	_, err = authSvc.Register(ctx, "nudge-admin@t.local", "nudgeadmin", "password12345")
	must(t, err)
	must(t, execT(ctx, pool, `UPDATE users SET role='admin' WHERE email='nudge-admin@t.local'`))
	tok, err := authSvc.Login(ctx, "nudge-admin@t.local", "password12345", "test")
	must(t, err)

	// interval is forced in as an env override, same as newScanServer, so a
	// caller passing 0 reproduces the real QUASAR_LIBRARY_SCAN_INTERVAL=0
	// kill-switch path (RunOnce reports it as a Skipped outcome, not a launch
	// refusal — there is no goroutine left to refuse to launch).
	j := NewJanitor(f.store, setStore, NewResolver(setStore, true, interval, false, false), quietLogger())

	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.Definition{
		ID:      "library.discovery",
		Name:    "Library discovery",
		Plane:   jobs.PlaneControl,
		Scope:   jobs.ScopeInstance,
		Managed: true,
		Default: jobs.Schedule{Kind: jobs.KindInterval, IntervalSecs: 21600},
		Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
			res, skip := j.RunOnce(ctx)
			if skip != "" {
				return jobs.Skipped(skip), nil
			}
			return jobs.Succeeded(jobs.Summary{
				"enqueued":           res.Enqueued,
				"returned_abandoned": res.ReturnedAbandoned,
				"expired":            res.Expired,
				"pruned":             res.Pruned,
			}), nil
		},
	})
	jobStore := jobs.NewStore(pool)
	if _, err := reg.Sync(ctx, jobStore, "UTC", 50, quietLogger()); err != nil {
		must(t, err)
	}
	disp := jobs.New(jobStore, reg, jobs.DefaultConfig(), quietLogger())

	sh := settings.NewHandler(setStore)
	// The real app.go wiring: a non-blocking goroutine over Dispatcher.Enqueue.
	sh.OnLibraryDiscoveryEnabled = func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = disp.Enqueue(ctx, "library.discovery", "", nil)
		}()
	}

	mux := http.NewServeMux()
	sh.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	patch := func(t *testing.T, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/admin/settings", strings.NewReader(body))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	return nudgeHarness{dispatcher: disp, patch: patch}
}

// pendingLibraryDiscoveryRuns counts open (pending or running) job_runs rows
// for library.discovery — the durable stand-in for the old in-memory nudge
// channel's buffer.
func pendingLibraryDiscoveryRuns(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return countT(t, pool, `SELECT count(*) FROM job_runs WHERE job_id='library.discovery' AND state IN ('pending','running')`)
}

// TestEnableNudgesOnlyOnTheFalseToTrueTransition.
//
// The dispatcher's tick is deliberately never driven here, so nothing claims
// the materialized row and the assertion is on the row itself — deterministic,
// with no sleeps. That a nudge actually produces a pass is the next test's job.
func TestEnableNudgesOnlyOnTheFalseToTrueTransition(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	h := newNudgeHarness(t, f, 6*time.Hour)

	// false → false: nothing to run.
	if code := h.patch(t, `{"library_discovery_enabled":false}`); code != http.StatusOK {
		t.Fatalf("patch false: status %d, want 200", code)
	}
	if !waitForCount(t, func() int { return pendingLibraryDiscoveryRuns(t, pool) }, 0, time.Second) {
		t.Fatalf("false→false nudged, want 0 pending runs")
	}

	// A PATCH that does not mention the field at all must not nudge either.
	if code := h.patch(t, `{"registration_mode":"invite_only"}`); code != http.StatusOK {
		t.Fatalf("patch registration_mode: status %d, want 200", code)
	}
	if !waitForCount(t, func() int { return pendingLibraryDiscoveryRuns(t, pool) }, 0, time.Second) {
		t.Fatalf("an unrelated PATCH nudged, want 0 pending runs")
	}

	// false → true: THIS is the transition that earns a pass.
	if code := h.patch(t, `{"library_discovery_enabled":true}`); code != http.StatusOK {
		t.Fatalf("patch true: status %d, want 200", code)
	}
	if !waitForCount(t, func() int { return pendingLibraryDiscoveryRuns(t, pool) }, 1, 2*time.Second) {
		t.Fatalf("false→true did not materialize exactly 1 pending run")
	}

	// true → true: a re-save of a form that already had it on must not re-walk
	// every home — and Materialize's single-open-run invariant means a second
	// Enqueue while the first is still pending does not insert a second row.
	if code := h.patch(t, `{"library_discovery_enabled":true}`); code != http.StatusOK {
		t.Fatalf("patch true again: status %d, want 200", code)
	}
	time.Sleep(200 * time.Millisecond) // let OnLibraryDiscoveryEnabled's goroutine run, if it fired
	if n := pendingLibraryDiscoveryRuns(t, pool); n != 1 {
		t.Fatalf("true→true pending runs = %d, want 1 (still just the false→true row)", n)
	}
}

// TestNudgeRunsAPassPromptly — end to end: driving the dispatcher's tick (what
// the real 10s ticker in cmd/quasar-control/app.go does on its own) claims and
// executes the materialized run within moments of an admin flipping the
// switch, producing scan rows.
func TestNudgeRunsAPassPromptly(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	h := newNudgeHarness(t, f, 6*time.Hour)

	if code := h.patch(t, `{"library_discovery_enabled":true}`); code != http.StatusOK {
		t.Fatalf("patch true: status %d, want 200", code)
	}
	if !waitForScan(t, pool, h.dispatcher, 5*time.Second) {
		t.Fatal("no scan was enqueued after enabling discovery — the nudge did not reach the dispatcher")
	}
}

// TestNudgeCannotBlockTheSettingsHandler — OnLibraryDiscoveryEnabled runs its
// Dispatcher.Enqueue call on its OWN goroutine (see newNudgeHarness), so a
// PATCH must return promptly regardless of how long that DB call takes.
func TestNudgeCannotBlockTheSettingsHandler(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	h := newNudgeHarness(t, f, 0)

	done := make(chan int, 1)
	go func() { done <- h.patch(t, `{"library_discovery_enabled":true}`) }()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("patch: status %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PATCH /v1/admin/settings blocked on the nudge — the send is not non-blocking")
	}
	_ = pool
}

// waitForCount polls fn until it returns want or the deadline passes.
func waitForCount(t *testing.T, fn func() int, want int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if fn() == want {
			return true
		}
		if time.Now().After(deadline) {
			return fn() == want
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForScan drives the dispatcher's tick (materialize/claim/execute) until a
// scan row appears or the deadline passes. A poll rather than a hook because
// the thing under test is a real goroutine reacting to a real HTTP request.
func waitForScan(t *testing.T, pool *pgxpool.Pool, disp *jobs.Dispatcher, within time.Duration) bool {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		disp.Tick(ctx)
		if countT(t, pool, `SELECT count(*) FROM library_scans`) > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
