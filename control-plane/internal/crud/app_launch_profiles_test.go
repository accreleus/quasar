package crud

// UI-P5 admin write-path tests for an app's launchable-launch-profile
// allow-list. Require Postgres (TEST_DATABASE_URL); all tests share one DB and
// truncate in setup (-p 1 is mandatory).
//
// The LAUNCH-side enforcement lives in internal/session (see
// app_launch_profiles_db_test.go, and specifically
// TestPostSessionsRejectsProfileOutsideAppAllowList, which is the test that
// proves the allow-list is not UI-gated). This file covers the write shape: what
// can be stored, what cannot, and who may store it.

import (
	"context"
	"net/http"
	"testing"
)

// asStrings converts a decoded JSON array to []string for comparison.
func asStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("launchable_profile_ids: got %T (%v), want an array", v, v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("launchable_profile_ids entry: got %T, want string", e)
		}
		out = append(out, s)
	}
	return out
}

func TestAppLaunchableProfilesWriteRoundTrip(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@p5.test", "p5admin")

	// CREATE without the field: the app is unrestricted, which is exactly the
	// pre-UI-P5 behaviour, and the read shape says so with [] rather than null.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "No allow-list"}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	if ids := asStrings(t, app["launchable_profile_ids"]); len(ids) != 0 {
		t.Fatalf("a fresh app must be unrestricted: got %v", ids)
	}
	appID := app["id"].(string)

	// PATCH sets a list.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p60", "1080p60"},
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch allow-list: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app = body["app"].(map[string]any)
	if ids := asStrings(t, app["launchable_profile_ids"]); len(ids) != 2 {
		t.Fatalf("after patch: got %v, want 2 entries", ids)
	}

	// A patch that does NOT mention the field leaves it alone.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"description": "unrelated"}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unrelated patch: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if ids := asStrings(t, body["app"].(map[string]any)["launchable_profile_ids"]); len(ids) != 2 {
		t.Fatalf("an unrelated patch must not clear the allow-list: got %v", ids)
	}

	// An explicit [] clears it.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{},
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear allow-list: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if ids := asStrings(t, body["app"].(map[string]any)["launchable_profile_ids"]); len(ids) != 0 {
		t.Fatalf("after []: got %v, want empty", ids)
	}

	// CREATE with a list, so create and patch agree.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                   "Restricted at birth",
		"launchable_profile_ids": []string{"720p60"},
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with allow-list: want 201, got %d (%v)", resp.StatusCode, body)
	}
	if ids := asStrings(t, body["app"].(map[string]any)["launchable_profile_ids"]); len(ids) != 1 || ids[0] != "720p60" {
		t.Fatalf("create with allow-list: got %v, want [720p60]", ids)
	}
}

func TestAppLaunchableProfilesValidation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@p5val.test", "p5valadmin")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "Validation"}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	// An id that names no launch profile.
	resp, _ = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p60", "nope"},
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown launch profile id: got %d, want 400", resp.StatusCode)
	}

	// A DEBUG launch profile is not user-facing: an allow-list entry a user could
	// never be offered is as wrong as a default they could never be given.
	resp, _ = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p30"},
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("debug launch profile id: got %d, want 400", resp.StatusCode)
	}

	// A RUNG id (stream profile) is not a launch profile. This is the UI-P4 trap:
	// the two id spaces overlap in shape and only differ in table.
	resp, _ = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p60-h264"},
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rung id in the allow-list: got %d, want 400", resp.StatusCode)
	}

	// Explicit null is rejected rather than reinterpreted as "clear" — [] already
	// says that, and silently acting on a value the caller meant something by is
	// the class of bug 7b6ddcc fixed for the runtime-preset list fields.
	resp, _ = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"launchable_profile_ids": nil}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("explicit null: got %d, want 400", resp.StatusCode)
	}

	// Duplicates are deduped, not rejected — the join table's primary key would
	// refuse them and a 400 for "you listed 720p60 twice" helps nobody.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p60", "720p60"},
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate ids: got %d, want 200 (%v)", resp.StatusCode, body)
	}
	if ids := asStrings(t, body["app"].(map[string]any)["launchable_profile_ids"]); len(ids) != 1 {
		t.Errorf("duplicate ids: got %v, want one entry", ids)
	}
}

// TestAppLaunchableProfilesForceMirror: `force` pins the app's launch profile
// outright, so an allow-list can never apply to it. The server MIRRORS the admin
// UI hiding the control — it refuses to store one, and it CLEARS any existing
// one when an app is switched to `force`, so nothing can silently reactivate the
// day the operator switches back to `prefer`.
func TestAppLaunchableProfilesForceMirror(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@p5force.test", "p5forceadmin")

	// A create that asks for both at once is refused.
	resp, _ := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                   "Forced",
		"profile_policy":         "force",
		"default_profile_id":     "1080p60",
		"launchable_profile_ids": []string{"720p60"},
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create force + allow-list: got %d, want 400", resp.StatusCode)
	}

	// Set up a restricted `prefer` app, then switch it to `force` WITHOUT
	// mentioning the allow-list. The stored rows must be cleared anyway.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                   "Switcher",
		"profile_policy":         "prefer",
		"default_profile_id":     "1080p60",
		"launchable_profile_ids": []string{"720p60"},
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create prefer + allow-list: got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"profile_policy": "force"}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch to force: got %d (%v)", resp.StatusCode, body)
	}
	if ids := asStrings(t, body["app"].(map[string]any)["launchable_profile_ids"]); len(ids) != 0 {
		t.Fatalf("switching to force must clear the allow-list: got %v", ids)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM app_launch_profiles WHERE app_id::text = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("rows survived the switch to force: got %d, want 0", n)
	}

	// And a later patch that tries to set one while the STORED policy is force is
	// refused, even though the patch says nothing about the policy.
	resp, _ = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p60"},
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("allow-list against a stored force policy: got %d, want 400", resp.StatusCode)
	}
}

// TestAppLaunchableProfilesAdminGated: the allow-list rides the existing
// POST /v1/apps and PATCH /v1/apps/{id} routes, both of which are
// RequireAuth → RequireAdmin. UI-P5 adds no new admin route, so this asserts the
// field cannot be written through a non-admin bearer on the routes it DOES use —
// the gate is the middleware, and the 403 precedes any resource lookup.
func TestAppLaunchableProfilesAdminGated(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@p5gate.test", "p5gateadmin")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "Gated"}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	if _, err := authSvc.Register(ctx, "plain@p5gate.test", "p5plain", "unrelated-pw-16"); err != nil {
		t.Fatalf("register plain user: %v", err)
	}
	tok, err := authSvc.Login(ctx, "plain@p5gate.test", "unrelated-pw-16", "")
	if err != nil {
		t.Fatalf("login plain user: %v", err)
	}

	resp, _ = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"launchable_profile_ids": []string{"720p60"},
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin PATCH: got %d, want 403", resp.StatusCode)
	}
	resp, _ = post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                   "Sneaky",
		"launchable_profile_ids": []string{"720p60"},
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin POST: got %d, want 403", resp.StatusCode)
	}

	// The write never happened.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM app_launch_profiles WHERE app_id::text = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("a non-admin write reached the table: got %d rows, want 0", n)
	}
}
