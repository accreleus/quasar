package session

// app_launch_profiles_db_test.go — UI-P5: the per-app launchable-launch-profile
// allow-list.
//
// The load-bearing test in this file is
// TestPostSessionsRejectsProfileOutsideAppAllowList: it drives the REAL
// POST /v1/sessions HTTP handler with a disallowed profile_id, bypassing every
// piece of UI. That is the test that proves the allow-list is not UI-gated, and
// it is written deliberately rather than as a side effect of a coordinator test
// — a coordinator-level assertion would still leave open the possibility that
// the handler never reaches the check.
//
// DB-gated like the rest of this package (go-test-db / TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- helpers -----------------------------------------------------------------

// allowLaunchProfiles sets an app's stored allow-list directly, which is what
// the admin write path (internal/crud) produces. Writing it here keeps these
// tests focused on the LAUNCH-side rule rather than on the admin surface.
func allowLaunchProfiles(t *testing.T, pool *pgxpool.Pool, appID string, ids ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM app_launch_profiles WHERE app_id::text = $1`, appID); err != nil {
		t.Fatalf("clear allow-list: %v", err)
	}
	for _, id := range ids {
		if _, err := pool.Exec(ctx,
			`INSERT INTO app_launch_profiles (app_id, launch_profile_id) VALUES ($1::uuid, $2)`,
			appID, id); err != nil {
			t.Fatalf("insert allow-list row %q: %v", id, err)
		}
	}
}

func setAppProfilePolicy(t *testing.T, pool *pgxpool.Pool, appID, policy string, defaultProfileID *string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE apps SET profile_policy = $2, default_profile_id = $3 WHERE id::text = $1`,
		appID, policy, defaultProfileID); err != nil {
		t.Fatalf("set profile policy: %v", err)
	}
}

func strPtr(s string) *string { return &s }

// --- THE test: server-side enforcement at POST /v1/sessions ------------------

// TestPostSessionsRejectsProfileOutsideAppAllowList is the Gate-5 test.
//
// It calls POST /v1/sessions DIRECTLY over HTTP — no UI, no client-side
// filtering, no GET /v1/me/profiles first — with a profile_id that is a
// perfectly valid, user-visible, device-eligible launch profile and is simply
// NOT in the app's allow-list. The launch must be refused, and no session row
// may persist.
//
// 409 profile_not_launchable_for_app is the asserted status+code, consistent
// with this endpoint's neighbouring policy refusals (`profile_ineligible` when
// the DEVICE refuses a valid profile, `conflict` when the app's `force` policy
// refuses an override). See handler.go for why not 400, why not 403, and why it
// carries its own code rather than the generic `conflict`.
func TestPostSessionsRejectsProfileOutsideAppAllowList(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _ := newStopServer(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	user, err := authSvc.Register(ctx, "p5user@test.local", "p5user", "quasar-fixture-pw-07")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "p5user@test.local", "quasar-fixture-pw-07")

	// The app offers ONLY 720p60. 1080p60 exists, is user-visible, and is
	// eligible for a probe-less caller — the only thing wrong with it is that
	// this app does not offer it.
	allowLaunchProfiles(t, pool, s.appID, "720p60")

	// Two request shapes, both from a PLAIN non-admin bearer, both naming the same
	// disallowed profile:
	//
	//   1. the bare launch; and
	//   2. THE SAME LAUNCH PLUS AN EXPLICIT `stream` OVERRIDE. `stream` carries no
	//      role gate on this endpoint — every authenticated user may send it — so
	//      if the allow-list check sits inside the "no override" guard, adding one
	//      harmless field defeats the entire feature and the disallowed chain is
	//      persisted and dispatched (an override also short-circuits the
	//      visibility/eligibility resolution, so the whole chain rides through).
	//      This case is the regression pin for that bypass.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{
			name: "bare",
			body: map[string]any{"app_id": s.appID, "profile_id": "1080p60"},
		},
		{
			name: "with a stream override",
			body: map[string]any{
				"app_id":     s.appID,
				"profile_id": "1080p60",
				"stream":     map[string]any{"fps": 60},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions", tok, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("POST /v1/sessions with a disallowed profile: got %d, want 409", resp.StatusCode)
			}
			var errBody struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if errBody.Error.Code != "profile_not_launchable_for_app" {
				t.Errorf("error code: got %q, want profile_not_launchable_for_app", errBody.Error.Code)
			}

			// No session row may persist: the refusal happens before
			// ScheduleAndCreate, so there is nothing to roll back and nothing to
			// leak a reservation.
			var n int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id::text = $1`, user.ID).Scan(&n); err != nil {
				t.Fatalf("count sessions: %v", err)
			}
			if n != 0 {
				t.Errorf("sessions persisted after a refused launch: got %d, want 0", n)
			}
		})
	}

	// Control: the SAME request shape with an allowed profile succeeds through
	// the same handler, so the 409 above is the allow-list and not a broken
	// fixture.
	resp2 := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions", tok, map[string]any{
		"app_id":     s.appID,
		"profile_id": "720p60",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/sessions with an allowed profile: got %d, want 201", resp2.StatusCode)
	}
	_ = store
}

// TestLaunchAllowListAdminBypass pins the documented escape hatch: an ADMIN
// caller is not subject to the allow-list, exactly as they are not subject to
// the eligibility gate or the override policy. That is a server-verified role
// (RequireAuth → the users.role column), never a client assertion, so it does
// not weaken the rule above — but it is behaviour worth pinning so a future
// change cannot flip it silently in either direction.
func TestLaunchAllowListAdminBypass(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	allowLaunchProfiles(t, pool, s.appID, "720p60")

	fps := int32(60)
	if _, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{
		AppID: s.appID, ProfileID: "1080p60",
	}); !errors.Is(err, ErrProfileNotLaunchableForApp) {
		t.Fatalf("non-admin: got %v, want ErrProfileNotLaunchableForApp", err)
	}
	// A non-admin WITH a stream override is refused identically. The override
	// hatch beats the eligibility gate, never the allow-list — `stream` is
	// available to every authenticated caller, so honouring it here would make the
	// allow-list opt-out-able by the party it constrains.
	if _, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{
		AppID: s.appID, ProfileID: "1080p60", Override: StreamOverride{FPS: &fps},
	}); !errors.Is(err, ErrProfileNotLaunchableForApp) {
		t.Fatalf("non-admin + stream override: got %v, want ErrProfileNotLaunchableForApp", err)
	}
	res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{
		AppID: s.appID, ProfileID: "1080p60", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("admin: got %v, want a successful launch", err)
	}
	stopSessionRow(t, pool, res.Session.ID)
	// Admin + override is the same bypass, still allowed.
	if _, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{
		AppID: s.appID, ProfileID: "1080p60", IsAdmin: true, Override: StreamOverride{FPS: &fps},
	}); err != nil {
		t.Fatalf("admin + stream override: got %v, want a successful launch", err)
	}
}

// --- the no-regression case --------------------------------------------------

// TestEmptyAllowListBehavesExactlyAsBefore is the case that decides whether this
// feature ships inert. Every pre-UI-P5 app has no rows in app_launch_profiles,
// so an empty allow-list must mean "any launch profile the device is eligible
// for" on BOTH paths: the explicit profile_id and the implicit
// no-profile_id resolution.
func TestEmptyAllowListBehavesExactlyAsBefore(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	// No allow-list rows at all — the shipped state of every existing app.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM app_launch_profiles WHERE app_id::text = $1`, s.appID).Scan(&n); err != nil {
		t.Fatalf("count allow-list: %v", err)
	}
	if n != 0 {
		t.Fatalf("fixture: app already has %d allow-list rows", n)
	}

	// Explicit: any user-facing profile launches.
	for _, id := range []string{"720p60", "1080p60", "1440p60"} {
		res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, ProfileID: id})
		if err != nil {
			t.Fatalf("launch %s with an empty allow-list: %v", id, err)
		}
		if res.Session.ProfileID == nil || *res.Session.ProfileID != id {
			t.Errorf("launch %s: profile_id got %v", id, res.Session.ProfileID)
		}
		stopSessionRow(t, pool, res.Session.ID)
	}

	// Implicit: no profile_id still resolves through the unfiltered catalogue.
	res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID})
	if err != nil {
		t.Fatalf("implicit launch with an empty allow-list: %v", err)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "1080p60" {
		t.Errorf("implicit launch: profile_id got %v, want 1080p60 (the probe-less recommendation)", res.Session.ProfileID)
	}
}

// --- the implicit default is always included ---------------------------------

// TestAppDefaultAlwaysLaunchableEvenWhenNotListed: the app's own default
// (profile_policy = prefer) is implicitly in the allow-list and cannot be
// removed, so it launches even when the stored list does not name it. This is
// what makes the admin UI's disabled "Default" checkbox true rather than
// decorative.
func TestAppDefaultAlwaysLaunchableEvenWhenNotListed(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	setAppProfilePolicy(t, pool, s.appID, "prefer", strPtr("1440p60"))
	// The stored list deliberately OMITS the default.
	allowLaunchProfiles(t, pool, s.appID, "720p60")

	res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, ProfileID: "1440p60"})
	if err != nil {
		t.Fatalf("launch the app default: got %v, want success", err)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "1440p60" {
		t.Errorf("profile_id: got %v, want 1440p60", res.Session.ProfileID)
	}
	stopSessionRow(t, pool, res.Session.ID)

	// And a third profile that is neither the default nor listed is still refused
	// — the implicit inclusion is one profile, not an amnesty.
	if _, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, ProfileID: "1080p60"}); !errors.Is(err, ErrProfileNotLaunchableForApp) {
		t.Fatalf("unlisted non-default: got %v, want ErrProfileNotLaunchableForApp", err)
	}
}

// TestInheritAppDoesNotImplicitlyIncludeStaleDefault: under `inherit` the
// account/global default decides, so a leftover apps.default_profile_id is NOT
// this app's default and must not widen the allow-list by one.
func TestInheritAppDoesNotImplicitlyIncludeStaleDefault(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	setAppProfilePolicy(t, pool, s.appID, "inherit", strPtr("1440p60"))
	allowLaunchProfiles(t, pool, s.appID, "720p60")

	if _, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, ProfileID: "1440p60"}); !errors.Is(err, ErrProfileNotLaunchableForApp) {
		t.Fatalf("stale default under inherit: got %v, want ErrProfileNotLaunchableForApp", err)
	}
}

// --- force ignores the list --------------------------------------------------

// TestForceAppIgnoresAllowList: `force` pins the app's launch profile outright,
// so no allow-list can apply. Mirrored server-side — a row that somehow exists
// (hand-edited, or written before a policy change) must not take effect.
func TestForceAppIgnoresAllowList(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	setAppProfilePolicy(t, pool, s.appID, "force", strPtr("1080p60"))
	allowLaunchProfiles(t, pool, s.appID, "720p60")

	app, err := store.GetLaunchApp(ctx, s.appID)
	if err != nil {
		t.Fatalf("get launch app: %v", err)
	}
	restriction, err := store.AppProfileRestrictionFor(ctx, app)
	if err != nil {
		t.Fatalf("restriction: %v", err)
	}
	if restriction.Restricted {
		t.Fatal("a force app must be unrestricted: an allow-list can never apply to it")
	}
	if !restriction.Permits("1440p60") {
		t.Error("a force app's restriction must permit everything")
	}
}

// TestForceAppAllowsExplicitRequestMatchingItsOwnDefault: naming the exact
// profile_id a `force` app already pins is not an override attempt — it
// resolves to the identical chain the implicit (no-profile_id) path would
// reach via ResolveDefaultProfile. Refusing it with ErrProfileOverrideDisabled
// was a real gap: a non-admin caller that happens to name its own forced
// default 409s for asking for exactly what it would get anyway. See
// docs/reports/2026-08-19-overnight-2/README.md harness note #3 (the bench
// identity hit this launching the pinned `--profile 1080p60` against
// Quasar Benchapp, which was forced to 1080p60).
func TestForceAppAllowsExplicitRequestMatchingItsOwnDefault(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	setAppProfilePolicy(t, pool, s.appID, "force", strPtr("1080p60"))

	res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("explicit profile_id matching the force pin: got %v, want success", err)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "1080p60" {
		t.Fatalf("resolved profile_id = %v, want 1080p60", res.Session.ProfileID)
	}
}

// TestForceAppStillRejectsExplicitDifferentProfile: the carve-out above is
// narrow — a profile_id that differs from the `force` pin is a genuine
// override attempt and must still 409, exactly as before.
func TestForceAppStillRejectsExplicitDifferentProfile(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	setAppProfilePolicy(t, pool, s.appID, "force", strPtr("1080p60"))

	_, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, ProfileID: "1440p60"})
	if !errors.Is(err, ErrProfileOverrideDisabled) {
		t.Fatalf("explicit profile_id differing from the force pin: got %v, want ErrProfileOverrideDisabled", err)
	}
}

// --- the implicit path cannot route around the list --------------------------

// TestAllowListConstrainsImplicitResolution: a no-profile_id launch resolves
// through the user preference / global default / recommendation chain, none of
// which knows about the app. A disallowed preference must be SKIPPED rather than
// used, or the implicit path would silently grant what the explicit path is
// rejected for.
func TestAllowListConstrainsImplicitResolution(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	s := seed(t, pool, 4)
	allowLaunchProfiles(t, pool, s.appID, "720p60")
	if _, err := store.UpdateUserProfilePreferences(ctx, s.userID, strPtr("1440p60")); err != nil {
		t.Fatalf("set user preference: %v", err)
	}

	res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID})
	if err != nil {
		t.Fatalf("implicit launch: %v", err)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "720p60" {
		t.Fatalf("implicit launch with a disallowed user preference: profile_id got %v, want 720p60 "+
			"(the preference must be skipped, not honoured)", res.Session.ProfileID)
	}
}

// --- cascade -----------------------------------------------------------------

// TestAllowListCascadesOnLaunchProfileDelete pins migration 0037's chosen
// cascade: deleting a launch profile removes it from every app's allow-list
// rather than refusing the delete. The three references that DO refuse
// (apps.default_profile_id, the global default, a user preference) are
// unaffected by this — see the migration header for why an allow-list entry is
// not one of them.
func TestAllowListCascadesOnLaunchProfileDelete(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	allowLaunchProfiles(t, pool, s.appID, "720p60", "1080p60")

	if err := store.DeleteLaunchProfile(ctx, "720p60"); err != nil {
		t.Fatalf("delete launch profile: %v", err)
	}

	ids, err := store.appLaunchProfileIDs(ctx, s.appID)
	if err != nil {
		t.Fatalf("read allow-list: %v", err)
	}
	if len(ids) != 1 || ids[0] != "1080p60" {
		t.Fatalf("after cascade: got %v, want [1080p60]", ids)
	}

	// Deleting the last one empties the list, which means unrestricted again.
	// That widening is the documented cost of the cascade (migration 0037), and
	// it is asserted here so it stays a decision rather than a surprise.
	if err := store.DeleteLaunchProfile(ctx, "1080p60"); err != nil {
		t.Fatalf("delete second launch profile: %v", err)
	}
	app, err := store.GetLaunchApp(ctx, s.appID)
	if err != nil {
		t.Fatalf("get launch app: %v", err)
	}
	restriction, err := store.AppProfileRestrictionFor(ctx, app)
	if err != nil {
		t.Fatalf("restriction: %v", err)
	}
	if restriction.Restricted {
		t.Error("an allow-list emptied by cascade must read as unrestricted")
	}
}

// TestAppDeleteCascadesAllowList: the other side of the join. Deleting an app
// takes its allow-list with it — the rows have no meaning without the app.
func TestAppDeleteCascadesAllowList(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	s := seed(t, pool, 4)
	allowLaunchProfiles(t, pool, s.appID, "720p60")
	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id::text = $1`, s.appID); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM app_launch_profiles WHERE app_id::text = $1`, s.appID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("allow-list rows survived the app delete: got %d, want 0", n)
	}
}

// --- GET /v1/me/profiles?app_id= ---------------------------------------------

// TestMeProfilesFilteredByAppID: the read returns the ALREADY-FILTERED list, so
// a client never has to intersect anything. Without the parameter the response
// is the full catalogue, byte-for-byte the pre-UI-P5 behaviour.
func TestMeProfilesFilteredByAppID(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _, _ := newStopServer(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	if _, err := authSvc.Register(ctx, "p5read@test.local", "p5read", "quasar-fixture-pw-06"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "p5read@test.local", "quasar-fixture-pw-06")
	allowLaunchProfiles(t, pool, s.appID, "720p60")

	unfiltered := getProfileIDs(t, srv.URL+"/v1/me/profiles", tok)
	if len(unfiltered) < 2 {
		t.Fatalf("unfiltered catalogue too small to be a meaningful test: %v", unfiltered)
	}

	filtered := getProfileIDs(t, srv.URL+"/v1/me/profiles?app_id="+s.appID, tok)
	if len(filtered) != 1 || filtered[0] != "720p60" {
		t.Fatalf("filtered by app_id: got %v, want [720p60]", filtered)
	}

	// An app that does not resolve is 404, under the same visibility rule as
	// GET /v1/apps/{id} — never a silent fall-back to the full catalogue.
	resp := doJSON(t, http.MethodGet,
		srv.URL+"/v1/me/profiles?app_id=00000000-0000-0000-0000-000000000000", tok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown app_id: got %d, want 404", resp.StatusCode)
	}
}

func getProfileIDs(t *testing.T, url, tok string) []string {
	t.Helper()
	resp := doJSON(t, http.MethodGet, url, tok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", url, resp.StatusCode)
	}
	var body struct {
		Profiles []struct {
			ID string `json:"id"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode profiles: %v", err)
	}
	out := make([]string, 0, len(body.Profiles))
	for _, p := range body.Profiles {
		out = append(out, p.ID)
	}
	return out
}

// stopSessionRow terminates a session row directly so the per-user quota and the
// single-writer home lock do not refuse the next launch in a table-driven test.
func stopSessionRow(t *testing.T, pool *pgxpool.Pool, sessionID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id::text = $1`, sessionID); err != nil {
		t.Fatalf("stop session row: %v", err)
	}
}

// --- pure unit: the three folding rules --------------------------------------

// TestNewAppProfileRestrictionRules covers the policy/default folding without a
// database, so the rules are readable in one place.
func TestNewAppProfileRestrictionRules(t *testing.T) {
	cases := []struct {
		name       string
		policy     string
		defaultID  *string
		stored     []string
		restricted bool
		permits    map[string]bool
	}{
		{
			name: "no rows is unrestricted", policy: "inherit", stored: nil,
			restricted: false, permits: map[string]bool{"anything": true},
		},
		{
			name: "force is unrestricted even with rows", policy: "force",
			defaultID: strPtr("1080p60"), stored: []string{"720p60"},
			restricted: false, permits: map[string]bool{"1440p60": true},
		},
		{
			name: "prefer folds in the app default", policy: "prefer",
			defaultID: strPtr("1440p60"), stored: []string{"720p60"},
			restricted: true,
			permits:    map[string]bool{"720p60": true, "1440p60": true, "1080p60": false},
		},
		{
			name: "inherit does not fold in a stale default", policy: "inherit",
			defaultID: strPtr("1440p60"), stored: []string{"720p60"},
			restricted: true,
			permits:    map[string]bool{"720p60": true, "1440p60": false},
		},
		{
			name: "an empty default string is not folded in", policy: "prefer",
			defaultID: strPtr(""), stored: []string{"720p60"},
			restricted: true,
			permits:    map[string]bool{"720p60": true, "": false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newAppProfileRestriction(tc.policy, tc.defaultID, tc.stored)
			if r.Restricted != tc.restricted {
				t.Fatalf("Restricted: got %v, want %v", r.Restricted, tc.restricted)
			}
			for id, want := range tc.permits {
				if got := r.Permits(id); got != want {
					t.Errorf("Permits(%q): got %v, want %v", id, got, want)
				}
			}
		})
	}
}
