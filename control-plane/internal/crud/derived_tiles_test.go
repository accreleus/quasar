package crud

// steam-library-discovery Phase 3 — the ADMIN WRITE PATH for derived tiles, the
// storage-layer CHECK that keeps a tile identity-only, and the delete
// confirmation.
//
// Requires Postgres (TEST_DATABASE_URL); shares the DB with every other crud
// test (-p 1 mandatory).
//
// The launch-path behaviour these rows feed (§2's seven sites, the two 409s,
// admission from the parent) lives in internal/session and internal/storage —
// see derived_tiles_db_test.go and derived_home_db_test.go there.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// deleteJSON issues a DELETE and parses the body. The package already has a
// deleteReq that returns the response alone; the delete-confirmation 409 carries
// a list of tiles inside its error object, so this one keeps the body too.
func deleteJSON(t *testing.T, url, bearer string) (*http.Response, map[string]any) {
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
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

// createProviderApp POSTs a managed-home Steam provider app and returns its id.
//
// It turns library discovery ON first (#534): the create path now refuses a
// library-provider app while the instance-wide switch is off, because the
// provider reconciler would suspend the row seconds later and every later read
// would be a bare 404. These tests are about DERIVED TILES, and a provider app
// is their fixture — so they state the level the fixture needs rather than
// depending on whatever the shared test database was left at.
func createProviderApp(t *testing.T, pool *pgxpool.Pool, srv, admin, name string) string {
	t.Helper()
	setLibraryDiscovery(t, pool, true)
	resp, body := post(t, srv+"/v1/apps", map[string]any{
		"name":             name,
		"kind":             "launcher",
		"library_provider": "steam",
		"managed_home":     true,
		"runtime_spec":     map[string]any{"image": "steam:1"},
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create provider app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	return body["app"].(map[string]any)["id"].(string)
}

// TestDerivedTileRoundTripsThroughTheAdminEditor is the §13 Phase 3 requirement
// that an admin can hand-create a tile through the existing app editor API —
// "the validated Tower experiment made shippable", with no background job.
func TestDerivedTileRoundTripsThroughTheAdminEditor(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@tile.test", "tileadmin")
	if _, err := authSvc.Register(ctx, "user@tile.test", "tileuser", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@tile.test", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":            "Hades",
		"parent_app_id":   parent,
		"external_source": "steam",
		"external_id":     "1145360",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tile: want 201, got %d (%v)", resp.StatusCode, body)
	}
	tile := body["app"].(map[string]any)
	tileID := tile["id"].(string)
	if tile["parent_app_id"] != parent {
		t.Errorf("parent_app_id = %v, want %s", tile["parent_app_id"], parent)
	}
	// origin defaults to 'manual' — an admin create is manual by construction.
	if tile["origin"] != "manual" {
		t.Errorf("origin = %v, want manual (the column default for an admin create)", tile["origin"])
	}
	if tile["library_provider"] != "" {
		t.Errorf("library_provider = %v, want \"\" — a tile cannot be a provider", tile["library_provider"])
	}

	// parent_app_id is PUBLIC (§2.2 requirement 2: the client marks sibling tiles
	// blocked while a family session is live and has nothing else to group by).
	resp, body = getReq(t, srv.URL+"/v1/apps/"+tileID, userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user get tile: want 200, got %d (%v)", resp.StatusCode, body)
	}
	pub := body["app"].(map[string]any)
	if pub["parent_app_id"] != parent {
		t.Errorf("public parent_app_id = %v, want %s", pub["parent_app_id"], parent)
	}
	// …but origin and library_provider are NOT. They are provenance and operator
	// configuration; widening a read shape later is additive, narrowing is not.
	if _, present := pub["origin"]; present {
		t.Error("origin leaked onto the PUBLIC app shape; it is admin-only")
	}
	if _, present := pub["library_provider"]; present {
		t.Error("library_provider leaked onto the PUBLIC app shape; it is admin-only")
	}

	// An ordinary app serializes parent_app_id as null, never absent — a client
	// must be able to tell "not a tile" from "field missing".
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "Ordinary"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ordinary: %d (%v)", resp.StatusCode, body)
	}
	ord := body["app"].(map[string]any)
	if v, present := ord["parent_app_id"]; !present || v != nil {
		t.Errorf("ordinary app parent_app_id = %v (present=%v), want an explicit null", v, present)
	}

	// The tri-state patch: an explicit null promotes the tile back to an ordinary
	// app. (Its runtime_spec is still '{}', so it is inert rather than useful —
	// but the shape CHECK no longer binds it, which is the thing being asserted.)
	resp, body = patch(t, srv.URL+"/v1/apps/"+tileID, map[string]any{"parent_app_id": nil}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear parent: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["app"].(map[string]any)["parent_app_id"]; got != nil {
		t.Errorf("after an explicit-null patch parent_app_id = %v, want null", got)
	}
}

// TestDerivedTileInheritsTheParentsLaunchDefaults is §1.2's "copied once at
// creation, then owned by the admin" (review finding 5).
//
// These are the one field group §1.2 places in BOTH columns, and the split is
// deliberate: GetLaunchApp must NOT coalesce them from the parent at launch —
// doing so would silently undo every per-tile edit an admin makes — so if they
// are not copied at creation they are never copied at all, and a parent with a
// pinned launch profile does not pass it on.
//
// Phase 4's reconciler (§7.7) needs exactly this, so it lands on the create path
// where the reconciler inherits it rather than reimplementing it.
func TestDerivedTileInheritsTheParentsLaunchDefaults(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@inherit.test", "inheritadmin")

	// A parent pinned to a specific launch profile. Discovery on, for the same
	// fixture reason as createProviderApp (#534).
	setLibraryDiscovery(t, pool, true)
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Steam", "kind": "launcher", "library_provider": "steam",
		"managed_home": true, "runtime_spec": map[string]any{"image": "steam:1"},
		"default_profile_id": "1080p60", "profile_policy": "prefer",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create parent: %d (%v)", resp.StatusCode, body)
	}
	parent := body["app"].(map[string]any)["id"].(string)

	// A tile that says nothing about profiles inherits both.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Hades", "parent_app_id": parent,
		"external_source": "steam", "external_id": "1145360",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tile: %d (%v)", resp.StatusCode, body)
	}
	tile := body["app"].(map[string]any)
	if tile["default_profile_id"] != "1080p60" {
		t.Errorf("default_profile_id = %v, want 1080p60 copied from the parent", tile["default_profile_id"])
	}
	if tile["profile_policy"] != "prefer" {
		t.Errorf("profile_policy = %v, want prefer copied from the parent", tile["profile_policy"])
	}

	// It is a COPY, not a link: editing the parent afterwards does NOT reach the
	// tile. That is the "owned by the admin thereafter" half, and it is why
	// GetLaunchApp reads these off the tile.
	tileID := tile["id"].(string)
	resp, body = patch(t, srv.URL+"/v1/apps/"+parent, map[string]any{"default_profile_id": "720p60"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch parent: %d (%v)", resp.StatusCode, body)
	}
	resp, body = getReq(t, srv.URL+"/v1/apps/"+tileID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get tile: %d (%v)", resp.StatusCode, body)
	}
	if got := body["app"].(map[string]any)["default_profile_id"]; got != "1080p60" {
		t.Errorf("tile default_profile_id = %v after editing the parent, want 1080p60 — "+
			"it is copied ONCE and then owned by the admin", got)
	}

	// An EXPLICIT value on the tile wins over the inherited one.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Celeste", "parent_app_id": parent,
		"external_source": "steam", "external_id": "504230",
		"default_profile_id": "720p60", "profile_policy": "force",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create explicit tile: %d (%v)", resp.StatusCode, body)
	}
	explicit := body["app"].(map[string]any)
	if explicit["default_profile_id"] != "720p60" || explicit["profile_policy"] != "force" {
		t.Errorf("explicit tile = %v/%v, want 720p60/force — the caller's values must win",
			explicit["default_profile_id"], explicit["profile_policy"])
	}

	// An ORDINARY app is untouched by any of this.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "Ordinary"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ordinary: %d (%v)", resp.StatusCode, body)
	}
	ord := body["app"].(map[string]any)
	if ord["profile_policy"] != "inherit" || ord["default_profile_id"] != nil {
		t.Errorf("ordinary app = %v/%v, want inherit/null (the schema defaults)",
			ord["default_profile_id"], ord["profile_policy"])
	}
	_ = ctx
}

// TestOriginIsReadOnly is the operator ruling of 2026-07-29 made testable.
//
// `origin` is PROVENANCE, not configuration. The decisive argument is
// apps_parent_external_uk: a tile relabelled 'manual' still occupies its
// (parent, source, appid) slot, so a Phase-4 reconciler reading it as "not mine,
// therefore missing" cannot re-create it either — a resurrection loop with no
// visible cause. Hand-creating a tile pre-marked 'discovered' is the mirror image
// with the same root cause. Unwritability removes both.
//
// The enforcement is decodeJSON's DisallowUnknownFields rather than a bespoke
// branch, so this test is what proves the enforcement exists at all.
func TestOriginIsReadOnly(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@origin.test", "originadmin")

	// Create: refused outright rather than silently ignored.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Forged", "origin": "discovered",
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with origin: want 400, got %d (%v)", resp.StatusCode, body)
	}
	// And nothing was created — the refusal is before any write.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps WHERE name = 'Forged'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("the refused create still made %d row(s)", n)
	}

	// Patch: same, and the stored value is untouched.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "Legit"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"origin": "discovered"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch with origin: want 400, got %d (%v)", resp.StatusCode, body)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT origin FROM apps WHERE id::text = $1`, appID).Scan(&stored); err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if stored != "manual" {
		t.Errorf("stored origin = %q after a refused patch, want manual (untouched)", stored)
	}

	// validOrigin remains the single statement of the legal value set — the DB
	// CHECK's Go-side counterpart, for whatever writes 'discovered' in Phase 4.
	for _, c := range []struct {
		val  string
		want bool
	}{{"manual", true}, {"discovered", true}, {"", false}, {"Discovered", false}, {"junk", false}} {
		v := c.val
		if got := validOrigin(&v); got != c.want {
			t.Errorf("validOrigin(%q) = %v, want %v", c.val, got, c.want)
		}
	}
	if !validOrigin(nil) {
		t.Error("validOrigin(nil) = false, want true (absent is always fine)")
	}
}

// TestDerivedShapeCheckRejectsARuntimeOfItsOwn is the load-bearing CHECK.
//
// A validated Tower experiment hardcoded a host path into one tile's
// runtime_spec.mounts. That is explicitly not the shipping mechanism — it freezes
// a host path into a fleet-wide catalogue row and stops the tile tracking its
// parent — and apps_derived_shape_ck exists so nobody can reproduce it. There is
// no schema validation anywhere on the runtime_spec write path, so the database
// is the only place this can live.
//
// Each case is asserted through the HTTP API, because a CHECK violation reaching
// a client as a 500 would be a lie: the request is malformed, not the server.
func TestDerivedShapeCheckRejectsARuntimeOfItsOwn(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@shape.test", "shapeadmin")
	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	// A preset to point at, for the runtime_preset_id case.
	resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name": "preset-for-shape-test", "image": "img:1",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset: %d (%v)", resp.StatusCode, body)
	}
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	base := func() map[string]any {
		return map[string]any{
			"name":            "Bad Tile",
			"parent_app_id":   parent,
			"external_source": "steam",
			"external_id":     "1145360",
		}
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{
			// THE ONE THAT MATTERS: the experiment's hardcoded mount.
			name: "a runtime_spec of its own",
			mutate: func(m map[string]any) {
				m["runtime_spec"] = map[string]any{"mounts": []string{"/mnt/user/steam:/home/quasar/.steam:rw"}}
			},
			want: http.StatusBadRequest,
		},
		{
			name:   "managed_home true",
			mutate: func(m map[string]any) { m["managed_home"] = true },
			want:   http.StatusBadRequest,
		},
		{
			name:   "a runtime preset of its own",
			mutate: func(m map[string]any) { m["runtime_preset_id"] = presetID },
			want:   http.StatusBadRequest,
		},
		{
			// Refused by the HANDLER with a field-specific message (§11.3), not by
			// the CHECK — a raw constraint violation would not say which of the two
			// fields to change.
			name:   "library_provider set",
			mutate: func(m map[string]any) { m["library_provider"] = "steam" },
			want:   http.StatusBadRequest,
		},
		{
			name:   "no external ref",
			mutate: func(m map[string]any) { delete(m, "external_source"); delete(m, "external_id") },
			want:   http.StatusBadRequest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := base()
			c.mutate(req)
			resp, body := post(t, srv.URL+"/v1/apps", req, admin)
			if resp.StatusCode != c.want {
				t.Fatalf("want %d, got %d (%v)", c.want, resp.StatusCode, body)
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps WHERE name = 'Bad Tile'`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("a refused tile create left %d row(s) behind", n)
			}
		})
	}

	// The same shape is enforced on PATCH: an admin cannot pour a runtime into an
	// existing, legal tile after the fact. This is the case the create-path checks
	// alone would miss, and it is why the constraint is in the database.
	resp, body = post(t, srv.URL+"/v1/apps", base(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create a LEGAL tile: %d (%v)", resp.StatusCode, body)
	}
	tileID := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+tileID, map[string]any{
		"runtime_spec": map[string]any{"mounts": []string{"/mnt/user/steam:/home/quasar/.steam:rw"}},
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patching a runtime_spec onto a tile: want 400, got %d (%v)", resp.StatusCode, body)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT runtime_spec::text FROM apps WHERE id::text = $1`, tileID).Scan(&stored); err != nil {
		t.Fatalf("read runtime_spec: %v", err)
	}
	if stored != "{}" {
		t.Errorf("tile runtime_spec = %s after a refused patch, want {}", stored)
	}
}

// TestExternalIDIsValidatedAtTheWritePath is §10 point 2 (the control-plane
// ingest gate) with point 3 (the DB CHECK) behind it.
//
// The appid is destined for STEAM_STARTUP_FLAGS, which the quasar-steam
// entrypoint word-splits with `read -r -a`, so a stored "480 -foo" arrives at the
// Steam client as two extra ARGUMENTS. That makes this an injection grammar, not
// a format preference.
func TestExternalIDIsValidatedAtTheWritePath(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@appid.test", "appidadmin")
	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	for _, bad := range []string{
		"1; rm -rf /", // shell-shaped
		"0",           // not a positive integer
		"99999999999", // 11 digits, over the 10-digit bound
		"",            // empty: legal on an ordinary app, never on a tile
		"480 -foo",    // the argument-injection case the grammar exists for
		"007",         // leading zero
		" 620",        // leading whitespace
		"620\n",       // trailing newline
	} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
				"name": "Injected", "parent_app_id": parent,
				"external_source": "steam", "external_id": bad,
			}, admin)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("external_id %q: want 400, got %d (%v)", bad, resp.StatusCode, body)
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps WHERE name = 'Injected'`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("external_id %q created %d row(s)", bad, n)
			}
		})
	}
}

// TestOneTilePerParentAndAppid pins apps_parent_external_uk: the catalogue is
// bounded by the union of installed appids, not by users × games. A second user's
// scan of the same game must find the existing tile, never create a duplicate.
func TestOneTilePerParentAndAppid(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@uniq.test", "uniqadmin")
	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	body := map[string]any{
		"name": "Hades", "parent_app_id": parent,
		"external_source": "steam", "external_id": "1145360",
	}
	if resp, b := post(t, srv.URL+"/v1/apps", body, admin); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first tile: %d (%v)", resp.StatusCode, b)
	}
	body["name"] = "Hades (again)"
	resp, b := post(t, srv.URL+"/v1/apps", body, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate tile: want 409, got %d (%v)", resp.StatusCode, b)
	}

	// A DIFFERENT parent with the same appid is fine — the key is the pair, so two
	// Steam accounts (two provider apps) each get their own tile for one game.
	other := createProviderApp(t, pool, srv.URL, admin, "Steam (second account)")
	body["parent_app_id"] = other
	if resp, b := post(t, srv.URL+"/v1/apps", body, admin); resp.StatusCode != http.StatusCreated {
		t.Fatalf("same appid under a different parent: want 201, got %d (%v)", resp.StatusCode, b)
	}
	_ = ctx
}

// TestParentAppIDMustNameANonDerivedApp covers the rule the database cannot
// express: homeAppID (spec §2) substitutes the parent EXACTLY ONCE, so a
// grandchild tile would resolve its home to a parent TILE — which owns no home
// and whose runtime_spec is '{}' by CHECK. Every one of §2's seven sites would
// then agree with each other about the wrong answer.
func TestParentAppIDMustNameANonDerivedApp(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@depth.test", "depthadmin")
	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Hades", "parent_app_id": parent,
		"external_source": "steam", "external_id": "1145360",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tile: %d (%v)", resp.StatusCode, body)
	}
	tile := body["app"].(map[string]any)["id"].(string)

	// A tile of a tile.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Grandchild", "parent_app_id": tile,
		"external_source": "steam", "external_id": "504230",
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("grandchild tile: want 400, got %d (%v)", resp.StatusCode, body)
	}

	// A parent that does not exist at all.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Orphan", "parent_app_id": "00000000-0000-0000-0000-000000000000",
		"external_source": "steam", "external_id": "504230",
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown parent: want 400, got %d (%v)", resp.StatusCode, body)
	}

	// THE OTHER HALF OF THE GATE (review finding 3): an app that ALREADY HAS TILES
	// cannot be given a parent. validParentApp checks the prospective parent is not
	// derived; without this, create A, create tile T under A, then PATCH A to point
	// at P → 200, and T is a grandchild whose home resolves to a row that owns none.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "P"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create P: %d (%v)", resp.StatusCode, body)
	}
	p := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+parent, map[string]any{"parent_app_id": p}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("giving a parent to an app that has tiles: want 400, got %d (%v).\n"+
			"apps_derived_shape_ck narrows this window but does not close it, and Phase 4's "+
			"reconciler walks parent_app_id — a chain is the resurrection-loop shape.",
			resp.StatusCode, body)
	}
	// Untouched.
	var storedParent *string
	if err := pool.QueryRow(ctx, `SELECT parent_app_id::text FROM apps WHERE id::text = $1`, parent).Scan(&storedParent); err != nil {
		t.Fatalf("read parent_app_id: %v", err)
	}
	if storedParent != nil {
		t.Errorf("parent_app_id = %v after a refused patch, want null", *storedParent)
	}

	// CLEARING a parent is still allowed on an app with tiles: an explicit null can
	// never deepen a chain, and refusing it would strand the operator trying to
	// undo exactly this mistake.
	resp, body = patch(t, srv.URL+"/v1/apps/"+tile, map[string]any{"parent_app_id": nil}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing a parent: want 200, got %d (%v)", resp.StatusCode, body)
	}

	// An app cannot be its own parent.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "Selfish"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d (%v)", resp.StatusCode, body)
	}
	selfish := body["app"].(map[string]any)["id"].(string)
	resp, body = patch(t, srv.URL+"/v1/apps/"+selfish, map[string]any{"parent_app_id": selfish}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-parent: want 400, got %d (%v)", resp.StatusCode, body)
	}
	_ = ctx
}

// TestDeletingAProviderAppNeedsConfirmation covers §4.1's application-layer gate.
//
// The FK cascades, which is the INTEGRITY backstop — but a cascade is not a user
// experience. Deleting a Steam app silently takes every game tile with it, and
// those cascade further into every user's favourites and the tiles' artwork,
// irreversibly. §17's risk register already names the tile-level version of this
// ("an admin deletes a junk tile instead of ignoring it"); the parent is the same
// hazard multiplied by the size of the library.
func TestDeletingAProviderAppNeedsConfirmation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@del.test", "deladmin")
	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	for _, tile := range []struct{ name, appid string }{
		{"Hades", "1145360"}, {"Celeste", "504230"},
	} {
		resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
			"name": tile.name, "parent_app_id": parent,
			"external_source": "steam", "external_id": tile.appid,
		}, admin)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create tile %s: %d (%v)", tile.name, resp.StatusCode, body)
		}
	}

	// Unconfirmed: refused, and the body LISTS the tiles rather than counting
	// them — the point of the confirmation is that the admin sees what they are
	// about to destroy, and "2 tiles" is not that.
	resp, body := deleteJSON(t, srv.URL+"/v1/apps/"+parent, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unconfirmed delete: want 409, got %d (%v)", resp.StatusCode, body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("409 body has no error object: %v", body)
	}
	tiles, ok := errObj["derived_tiles"].([]any)
	if !ok || len(tiles) != 2 {
		t.Fatalf("error.derived_tiles = %v, want a 2-element list nested inside the error object", errObj["derived_tiles"])
	}
	names := map[string]bool{}
	for _, raw := range tiles {
		entry := raw.(map[string]any)
		if entry["id"] == nil || entry["name"] == nil {
			t.Errorf("derived_tiles entry missing id/name: %v", entry)
		}
		names[fmt.Sprint(entry["name"])] = true
	}
	if !names["Hades"] || !names["Celeste"] {
		t.Errorf("derived_tiles names = %v, want Hades and Celeste", names)
	}

	// Nothing was deleted.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("apps = %d after a refused delete, want 3", n)
	}

	// A truthy-ish value is NOT an opt-in: only the exact string. A typo in a
	// script must refuse, not cascade.
	resp, _ = deleteJSON(t, srv.URL+"/v1/apps/"+parent+"?delete_derived=1", admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete_derived=1: want 409 (only the exact string \"true\" opts in), got %d", resp.StatusCode)
	}

	// Confirmed: the parent and both tiles go.
	resp, body = deleteJSON(t, srv.URL+"/v1/apps/"+parent+"?delete_derived=true", admin)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("confirmed delete: want 204, got %d (%v)", resp.StatusCode, body)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("apps = %d after the confirmed delete, want 0 (the FK cascade takes the tiles)", n)
	}
}

// TestDeletingAProviderAppRefusesWhileATileIsLive is the guard the confirmation
// must not be able to bypass: deleting a parent with delete_derived=true would
// otherwise cascade a tile out from under a RUNNING session, and the session's
// app row would simply vanish mid-stream.
func TestDeletingAProviderAppRefusesWhileATileIsLive(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "admin@dellive.test", "delliveadmin")
	parent := createProviderApp(t, pool, srv.URL, admin, "Steam")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "Hades", "parent_app_id": parent,
		"external_source": "steam", "external_id": "1145360",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tile: %d (%v)", resp.StatusCode, body)
	}
	tileID := body["app"].(map[string]any)["id"].(string)

	// A running session on the TILE.
	var userID, hostID, gpuID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('live@del.test','livedel','x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status)
		VALUES ('h','online') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1::uuid, 0, 8192, 4) RETURNING id::text`, hostID).Scan(&gpuID); err != nil {
		t.Fatalf("seed gpu: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions
		(user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps,
		 h264_profile, reserved_vram_mb, reserved_encode_slots)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1280, 720, 30, 2000,
		        'constrained-baseline', 0, 1)`, userID, tileID, hostID, gpuID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Even WITH the confirmation, the live-session guard wins.
	resp, body = deleteJSON(t, srv.URL+"/v1/apps/"+parent+"?delete_derived=true", admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete with a live TILE session: want 409, got %d (%v)", resp.StatusCode, body)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("apps = %d, want 2 — nothing may be deleted while a tile session is live", n)
	}
}
