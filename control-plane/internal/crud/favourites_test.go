package crud

// Integration tests for UI-P1: apps.kind + user_app_favourites. Require Postgres:
// run via scripts/dev/dev.sh go-test-db (sets TEST_DATABASE_URL). Uses the same testDB /
// newTestServer / post / patch / getReq helpers as handler_test.go (same package).

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// --- kind: create/patch default-fallthrough + validation --------------------

// TestCreateAppKindDefaultsToGame is the cb97bfb-trap regression test made explicit
// for `kind`: an app created with NO kind field must land as 'game' (the schema
// DEFAULT), never a Go zero value ("").
func TestCreateAppKindDefaultsToGame(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "kind-default-app"}, tok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	if app["kind"] != "game" {
		t.Fatalf("kind: want schema default %q, got %v", "game", app["kind"])
	}
}

// TestCreateAppKindDesktopPersists: an app created with kind:"desktop" persists as
// "desktop" — the write path is not silently coercing to the default.
func TestCreateAppKindDesktopPersists(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "desktop-app", "kind": "desktop",
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	if app["kind"] != "desktop" {
		t.Fatalf("kind: want %q, got %v", "desktop", app["kind"])
	}
	appID := app["id"].(string)

	// Re-fetch to confirm it's the persisted value, not just the create response.
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: want 200, got %d", resp.StatusCode)
	}
	if body["app"].(map[string]any)["kind"] != "desktop" {
		t.Fatalf("persisted kind: want %q, got %v", "desktop", body["app"].(map[string]any)["kind"])
	}
}

// TestCreateAppKindValidation: kind:"" and kind:"nonsense" are both 400
// validation_failed on create — an explicit "" is never "use the default" (the
// distinguishing behaviour from profile_policy, where "" is accepted).
func TestCreateAppKindValidation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "empty-kind-app", "kind": "",
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf(`kind:"" : want 400, got %d (%v)`, resp.StatusCode, body)
	}

	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "nonsense-kind-app", "kind": "nonsense",
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf(`kind:"nonsense" : want 400, got %d (%v)`, resp.StatusCode, body)
	}
}

// TestUpdateAppKindPreservedWhenAbsent: a PATCH with no `kind` field leaves the
// stored value untouched (nil pointer = "unchanged", the same idiom as profile_policy).
func TestUpdateAppKindPreservedWhenAbsent(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "patch-kind-app", "kind": "desktop",
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	// Patch something unrelated; kind is absent from the body.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"description": "still a desktop app",
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if body["app"].(map[string]any)["kind"] != "desktop" {
		t.Fatalf("kind after unrelated patch: want preserved %q, got %v", "desktop", body["app"].(map[string]any)["kind"])
	}

	// Explicit kind:"" on PATCH must also be rejected, not treated as "leave alone".
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"kind": ""}, tok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf(`patch kind:"" : want 400, got %d (%v)`, resp.StatusCode, body)
	}
}

// --- favourites: idempotency, isolation, cascades, visibility ---------------

// TestListAppsRequiresAuth: UI-P1 breaking change — GET /v1/apps with no bearer is
// 401, not the full catalogue.
func TestListAppsRequiresAuth(t *testing.T) {
	pool := testDB(t)
	srv, _ := newTestServer(t, pool)

	resp, _ := getReq(t, srv.URL+"/v1/apps", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /v1/apps: want 401, got %d", resp.StatusCode)
	}
}

// TestFavouriteIdempotent: a repeat PUT is another 204 and does not create a second
// row or re-stamp created_at (INSERT ... ON CONFLICT DO NOTHING).
func TestFavouriteIdempotent(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userAcct, err := authSvc.Register(ctx, "fav-user@test.local", "favuser", "quasar-fixture-pw-05")
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "fav-user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "fav-app"}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	favURL := srv.URL + "/v1/me/favourites/" + appID
	resp1, _ := putReq(t, favURL, userTok.Plaintext)
	if resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("first PUT favourite: want 204, got %d", resp1.StatusCode)
	}

	var createdAt1 time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM user_app_favourites WHERE user_id::text = $1 AND app_id::text = $2`,
		userAcct.ID, appID).Scan(&createdAt1); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	resp2, _ := putReq(t, favURL, userTok.Plaintext)
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second PUT favourite: want 204, got %d", resp2.StatusCode)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_app_favourites WHERE user_id::text = $1 AND app_id::text = $2`,
		userAcct.ID, appID).Scan(&count); err != nil {
		t.Fatalf("count favourites: %v", err)
	}
	if count != 1 {
		t.Fatalf("favourite rows after double PUT: want 1, got %d", count)
	}

	var createdAt2 time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM user_app_favourites WHERE user_id::text = $1 AND app_id::text = $2`,
		userAcct.ID, appID).Scan(&createdAt2); err != nil {
		t.Fatalf("read created_at again: %v", err)
	}
	if !createdAt1.Equal(createdAt2) {
		t.Fatalf("created_at was re-stamped on repeat PUT: %v -> %v", createdAt1, createdAt2)
	}

	// GET /v1/apps must now reflect favourite:true for this user.
	resp, body = getReq(t, srv.URL+"/v1/apps", userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps: want 200, got %d", resp.StatusCode)
	}
	items := body["items"].([]any)
	found := false
	for _, it := range items {
		item := it.(map[string]any)
		if item["id"] == appID {
			found = true
			if item["favourite"] != true {
				t.Fatalf("favourite: want true, got %v", item["favourite"])
			}
		}
	}
	if !found {
		t.Fatalf("app not found in list: %v", body)
	}
}

// TestUnfavouriteIdempotentNeverNotFound: DELETE on an app that was never favourited
// (or does not exist) is still 204 — the endpoint deliberately never 404s.
func TestUnfavouriteIdempotentNeverNotFound(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "unfav-user@test.local", "unfavuser", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "unfav-user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "never-favourited-app"}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	// Never favourited — still 204.
	resp = deleteReq(t, srv.URL+"/v1/me/favourites/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unfavourite never-favourited: want 204, got %d", resp.StatusCode)
	}

	// Repeat delete — still 204, not 404.
	resp = deleteReq(t, srv.URL+"/v1/me/favourites/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("repeat unfavourite: want 204, got %d", resp.StatusCode)
	}

	// A well-formed but nonexistent app id — still 204.
	resp = deleteReq(t, srv.URL+"/v1/me/favourites/00000000-0000-0000-0000-000000000000", userTok.Plaintext)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unfavourite nonexistent app: want 204, got %d", resp.StatusCode)
	}

	// A malformed UUID is 400, the one case that is NOT 204.
	resp = deleteReq(t, srv.URL+"/v1/me/favourites/not-a-uuid", userTok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unfavourite malformed uuid: want 400, got %d", resp.StatusCode)
	}
}

// TestFavouritePerUserIsolation is the important one: user A favourites an app; user
// B's GET /v1/apps must show favourite:false for that same app. Favouriting is
// never a stored property of the app.
func TestFavouritePerUserIsolation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "user-a@test.local", "usera", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user a: %v", err)
	}
	userATok, err := authSvc.Login(ctx, "user-a@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user a: %v", err)
	}
	if _, err := authSvc.Register(ctx, "user-b@test.local", "userb", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user b: %v", err)
	}
	userBTok, err := authSvc.Login(ctx, "user-b@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user b: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "isolation-app"}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	resp1, _ := putReq(t, srv.URL+"/v1/me/favourites/"+appID, userATok.Plaintext)
	if resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("user A favourite: want 204, got %d", resp1.StatusCode)
	}

	// User A sees favourite:true.
	resp, body = getReq(t, srv.URL+"/v1/apps", userATok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps as user A: want 200, got %d", resp.StatusCode)
	}
	if !findFavourite(t, body, appID) {
		t.Fatalf("user A: want favourite=true, got false: %v", body)
	}

	// User B must see favourite:false for the SAME app.
	resp, body = getReq(t, srv.URL+"/v1/apps", userBTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps as user B: want 200, got %d", resp.StatusCode)
	}
	if findFavourite(t, body, appID) {
		t.Fatalf("user B: want favourite=false (cross-user leak), got true: %v", body)
	}
}

func findFavourite(t *testing.T, body map[string]any, appID string) bool {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("no items in body: %v", body)
	}
	for _, it := range items {
		item := it.(map[string]any)
		if item["id"] == appID {
			return item["favourite"] == true
		}
	}
	t.Fatalf("app %s not found in list: %v", appID, body)
	return false
}

// TestFavouriteCascades: deleting an app cascades away its favourite rows, and
// deleting a user cascades away theirs too (schema.md user_app_favourites, both FKs
// ON DELETE CASCADE).
func TestFavouriteCascades(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	cascadeUser, err := authSvc.Register(ctx, "cascade-user@test.local", "cascadeuser", "quasar-fixture-pw-05")
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "cascade-user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	// App-delete cascade.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "cascade-app-1"}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app 1: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app1ID := body["app"].(map[string]any)["id"].(string)
	if resp1, _ := putReq(t, srv.URL+"/v1/me/favourites/"+app1ID, userTok.Plaintext); resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("favourite app 1: want 204, got %d", resp1.StatusCode)
	}
	if resp := deleteReq(t, srv.URL+"/v1/apps/"+app1ID, adminTok.Plaintext); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete app 1: want 204, got %d", resp.StatusCode)
	}
	var rowsAfterAppDelete int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_app_favourites WHERE app_id::text = $1`, app1ID).Scan(&rowsAfterAppDelete); err != nil {
		t.Fatalf("count favourites after app delete: %v", err)
	}
	if rowsAfterAppDelete != 0 {
		t.Fatalf("favourite rows after app delete: want 0, got %d", rowsAfterAppDelete)
	}

	// User-delete cascade (no DELETE /v1/users endpoint exists; delete directly).
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "cascade-app-2"}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app 2: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app2ID := body["app"].(map[string]any)["id"].(string)
	if resp1, _ := putReq(t, srv.URL+"/v1/me/favourites/"+app2ID, userTok.Plaintext); resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("favourite app 2: want 204, got %d", resp1.StatusCode)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, cascadeUser.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var rowsAfterUserDelete int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_app_favourites WHERE user_id::text = $1`, cascadeUser.ID).Scan(&rowsAfterUserDelete); err != nil {
		t.Fatalf("count favourites after user delete: %v", err)
	}
	if rowsAfterUserDelete != 0 {
		t.Fatalf("favourite rows after user delete: want 0, got %d", rowsAfterUserDelete)
	}
}

// TestFavouriteDisabledAppNotFoundForNonAdmin: PUT on a disabled app resolves 404
// under the SAME visibility rule as GET /v1/apps/{id} for a non-admin caller.
func TestFavouriteDisabledAppNotFoundForNonAdmin(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "disabled-app-user@test.local", "disableduser", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "disabled-app-user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "to-be-disabled-app"}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"enabled": false}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable app: want 200, got %d (%v)", resp.StatusCode, body)
	}

	resp, _ = putReq(t, srv.URL+"/v1/me/favourites/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("favourite disabled app as non-admin: want 404, got %d", resp.StatusCode)
	}

	// Malformed UUID is 400, not 404.
	resp, _ = putReq(t, srv.URL+"/v1/me/favourites/not-a-uuid", userTok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("favourite malformed uuid: want 400, got %d", resp.StatusCode)
	}
}

// --- small HTTP helpers (PUT/DELETE with no body; post/patch/getReq already
// exist in handler_test.go) -------------------------------------------------

func putReq(t *testing.T, url, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	return resp, nil
}

func deleteReq(t *testing.T, url, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	return resp
}
