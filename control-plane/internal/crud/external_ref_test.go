package crud

// Steam library discovery, PHASE 1: apps.external_source / apps.external_id
// (migration 0042) on the app read and admin write paths.
//
// Requires Postgres (TEST_DATABASE_URL); shares the DB with every other crud
// test (-p 1 mandatory).

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The write path round-trips both fields, and they are visible on the read path
// to admin and non-admin callers alike (they are identity, not operator config).
func TestExternalRefRoundTripsThroughCreateAndPatch(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@extref.test", "extrefadmin")
	if _, err := authSvc.Register(ctx, "user@extref.test", "extrefuser", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@extref.test", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":            "Portal 2",
		"external_source": "steam",
		"external_id":     "620",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	appID := app["id"].(string)
	if app["external_source"] != "steam" || app["external_id"] != "620" {
		t.Fatalf("create response: %v", app)
	}

	// The user-facing library read carries them too.
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user get: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app = body["app"].(map[string]any)
	if app["external_source"] != "steam" || app["external_id"] != "620" {
		t.Fatalf("user get response: %v", app)
	}

	// Re-tagging to a different appid.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"external_id": "400"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app = body["app"].(map[string]any)
	if app["external_id"] != "400" || app["external_source"] != "steam" {
		t.Fatalf("patch response: %v", app)
	}

	// An explicit "" is a deliberate CLEAR, distinct from omission.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"external_source": "",
		"external_id":     "",
	}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app = body["app"].(map[string]any)
	if app["external_source"] != "" || app["external_id"] != "" {
		t.Fatalf("clear response: %v", app)
	}

	// An app created without them defaults to "" rather than null.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "Untagged"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create untagged: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app = body["app"].(map[string]any)
	if app["external_source"] != "" || app["external_id"] != "" {
		t.Fatalf("untagged create response: %v", app)
	}
}

// THE cb97bfb FALLTHROUGH. An absent field in a PATCH body must leave the stored
// value alone. If these were plain strings instead of pointers, any unrelated
// PATCH would silently un-tag the app's Steam appid and send its artwork back to
// the fuzzy matcher — the same shape as the incident where omitted create fields
// zeroed encode slots and bypassed admission.
func TestExternalRefUnchangedByAPatchThatOmitsThem(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@extref-omit.test", "extrefomit")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":            "Portal 2",
		"external_source": "steam",
		"external_id":     "620",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"description": "an unrelated edit",
	}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	if app["external_source"] != "steam" || app["external_id"] != "620" {
		t.Fatalf("an omitted field must not be overwritten with \"\": %v", app)
	}

	// And in the database, not just in the response.
	var source, id string
	if err := pool.QueryRow(ctx,
		`SELECT external_source, external_id FROM apps WHERE id::text = $1`, appID).
		Scan(&source, &id); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if source != "steam" || id != "620" {
		t.Fatalf("stored values were clobbered: source=%q id=%q", source, id)
	}
}

// The HANDLER is the real gate (the CHECK is the backstop), so a bad value is a
// 400 validation_failed and never a 500 from a constraint violation.
func TestExternalRefValidationRejectsBadValues(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@extref-bad.test", "extrefbad")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "Anchor"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	for _, bad := range []map[string]any{
		{"external_source": "epic"},
		{"external_source": "Steam"}, // case matters; the CHECK is exact
		{"external_id": "0"},
		{"external_id": "007"},                // leading zero
		{"external_id": "99999999999"},        // 11 digits, out of grammar
		{"external_id": "620 -applaunch 480"}, // the argument-injection shape (§10)
		{"external_id": "1; rm -rf /"},
		{"external_id": "-620"},
		{"external_id": " 620"},
		{"external_id": "62 0"},
	} {
		bad["name"] = "Rejected"
		resp, body := post(t, srv.URL+"/v1/apps", bad, admin)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create with %v: want 400, got %d (%v)", bad, resp.StatusCode, body)
			continue
		}
		if errObj, ok := body["error"].(map[string]any); !ok || errObj["code"] != "validation_failed" {
			t.Errorf("create with %v: want validation_failed, got %v", bad, body)
		}

		delete(bad, "name")
		resp, body = patch(t, srv.URL+"/v1/apps/"+appID, bad, admin)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("patch with %v: want 400, got %d (%v)", bad, resp.StatusCode, body)
		}
	}

	// Nothing above changed the anchor app.
	var source, id string
	if err := pool.QueryRow(ctx,
		`SELECT external_source, external_id FROM apps WHERE id::text = $1`, appID).
		Scan(&source, &id); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if source != "" || id != "" {
		t.Fatalf("a rejected patch still wrote: source=%q id=%q", source, id)
	}
}

// THE BACKSTOP ITSELF (spec §10, point 3): the DB CHECK is the only one of the
// four validation points that survives an admin editing the value by some other
// route later, so it is asserted directly rather than only through the handler.
func TestExternalRefDatabaseCheckRejectsBadAppIDs(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	for _, bad := range []string{"0", "007", "99999999999", "1; rm -rf /", "620 -applaunch 480", "-620", " 620", "abc"} {
		_, err := pool.Exec(ctx,
			`INSERT INTO apps (name, description, runtime_spec, enabled, external_source, external_id)
			 VALUES ('bad', '', '{}'::jsonb, true, 'steam', $1)`, bad)
		if err == nil {
			t.Errorf("external_id %q was accepted by the database", bad)
			continue
		}
		if !strings.Contains(err.Error(), "apps_external_id_ck") {
			t.Errorf("external_id %q: want apps_external_id_ck, got %v", bad, err)
		}
	}

	// And the source enum.
	if _, err := pool.Exec(ctx,
		`INSERT INTO apps (name, description, runtime_spec, enabled, external_source)
		 VALUES ('bad', '', '{}'::jsonb, true, 'epic')`); err == nil {
		t.Error("external_source 'epic' was accepted by the database")
	} else if !strings.Contains(err.Error(), "apps_external_source_ck") {
		t.Errorf("want apps_external_source_ck, got %v", err)
	}

	// A well-formed appid at both ends of the grammar is accepted.
	for _, good := range []string{"1", "620", "9999999999"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO apps (name, description, runtime_spec, enabled, external_source, external_id)
			 VALUES ('good', '', '{}'::jsonb, true, 'steam', $1)`, good); err != nil {
			t.Errorf("external_id %q should be accepted: %v", good, err)
		}
	}
}
