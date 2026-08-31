package crud

// UI-P3 runtime-preset admin CRUD tests. Require Postgres (TEST_DATABASE_URL);
// all tests share one DB and truncate in setup (-p 1 is mandatory).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// adminBearer registers an account, promotes it to admin, and returns its token.
func adminBearer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, authSvc *auth.Service, email, username string) string {
	t.Helper()
	if _, err := authSvc.Register(ctx, email, username, "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email=$1`, email); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, email, "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	return tok.Plaintext
}

// --- happy paths ---

func TestRuntimePresetCRUD(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@preset.test", "presetadmin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email=$1`, "admin@preset.test"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@preset.test", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	bearer := tok.Plaintext

	// CREATE — only name is required; every other field falls through to the
	// schema default (never a zero value written over it).
	resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name": "Quasar Bench",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	p := body["runtime_preset"].(map[string]any)
	presetID := p["id"].(string)
	if p["image"] != "" || p["managed_home"] != false || p["home_container_path"] != "/home/quasar" {
		t.Fatalf("create defaults not applied: %v", p)
	}
	if usedBy, ok := p["used_by"].([]any); !ok || len(usedBy) != 0 {
		t.Fatalf("a fresh preset must report used_by: [], got %v", p["used_by"])
	}

	// CREATE with a full payload.
	resp, body = post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name":                "Steam (Proton)",
		"description":         "Steam with Proton",
		"image":               "ghcr.io/quasar/steam:latest",
		"args":                []string{"-silent"},
		"env":                 map[string]string{"PROTON_VERSION": "9.0"},
		"mounts":              []string{"/data/steam-cache:/cache"},
		"managed_home":        true,
		"home_container_path": "/home/steam",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create full preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	steamID := body["runtime_preset"].(map[string]any)["id"].(string)

	// LIST.
	resp, body = getReq(t, srv.URL+"/v1/admin/runtime-presets", bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list presets: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if items := body["items"].([]any); len(items) != 2 {
		t.Fatalf("list: want 2 presets, got %d", len(items))
	}

	// GET.
	resp, body = getReq(t, srv.URL+"/v1/admin/runtime-presets/"+steamID, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get preset: want 200, got %d (%v)", resp.StatusCode, body)
	}
	got := body["runtime_preset"].(map[string]any)
	if got["image"] != "ghcr.io/quasar/steam:latest" || got["home_container_path"] != "/home/steam" {
		t.Fatalf("get preset fields: %v", got)
	}

	// PATCH — an absent field must be left alone, not zeroed (the cb97bfb trap).
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+steamID, map[string]any{
		"image": "ghcr.io/quasar/steam:next",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch preset: want 200, got %d (%v)", resp.StatusCode, body)
	}
	got = body["runtime_preset"].(map[string]any)
	if got["image"] != "ghcr.io/quasar/steam:next" {
		t.Fatalf("patch did not apply: %v", got)
	}
	if got["managed_home"] != true || got["home_container_path"] != "/home/steam" || got["description"] != "Steam with Proton" {
		t.Fatalf("patch zeroed an absent field: %v", got)
	}

	// DELETE (not in use) → 204, then 404.
	if resp := deleteReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete unused preset: want 204, got %d", resp.StatusCode)
	}
	if resp, _ := getReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted preset: want 404, got %d", resp.StatusCode)
	}
}

// TestDeleteRuntimePresetInUseConflicts is the enforcement of the rule the admin
// UI only *hints* at by disabling its Delete button. The 409 is the gate.
func TestDeleteRuntimePresetInUseConflicts(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@inuse.test", "inuseadmin")

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "in-use"}, bearer)
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":              "app-with-preset",
		"runtime_preset_id": presetID,
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app with preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)
	if body["app"].(map[string]any)["runtime_preset_id"] != presetID {
		t.Fatalf("runtime_preset_id not persisted: %v", body["app"])
	}

	// DELETE while referenced → 409, and the preset is still there.
	resp = deleteReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete in-use preset: want 409, got %d", resp.StatusCode)
	}
	if resp, _ := getReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("preset must survive a refused delete, got %d", resp.StatusCode)
	}

	// used_by names the referencing app.
	_, body = getReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer)
	usedBy := body["runtime_preset"].(map[string]any)["used_by"].([]any)
	if len(usedBy) != 1 || usedBy[0].(map[string]any)["name"] != "app-with-preset" {
		t.Fatalf("used_by: want the referencing app, got %v", usedBy)
	}

	// A DISABLED app still holds the reference, so the delete stays refused.
	if resp, b := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"enabled": false}, bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable app: %d (%v)", resp.StatusCode, b)
	}
	if resp := deleteReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete preset used by a DISABLED app: want 409, got %d", resp.StatusCode)
	}

	// Point the app away (explicit null clears it) → delete now succeeds.
	if resp, b := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"runtime_preset_id": nil}, bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear preset: %d (%v)", resp.StatusCode, b)
	}
	if resp := deleteReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after clearing the reference: want 204, got %d", resp.StatusCode)
	}
}

// Admin gating is server-enforced on every route, never UI-gated (invariant #6).
func TestRuntimePresetsAdminGated(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	adminTok := adminBearer(t, ctx, pool, authSvc, "admin@gate.test", "gateadmin")

	if _, err := authSvc.Register(ctx, "user@gate.test", "gateuser", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@gate.test", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "gated"}, adminTok)
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	for _, tc := range []struct {
		name   string
		do     func(bearer string) int
		wantNo int
	}{
		{"list", func(b string) int { r, _ := getReq(t, srv.URL+"/v1/admin/runtime-presets", b); return r.StatusCode }, http.StatusForbidden},
		{"get", func(b string) int {
			r, _ := getReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, b)
			return r.StatusCode
		}, http.StatusForbidden},
		{"create", func(b string) int {
			r, _ := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "nope"}, b)
			return r.StatusCode
		}, http.StatusForbidden},
		{"patch", func(b string) int {
			r, _ := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{"image": "x"}, b)
			return r.StatusCode
		}, http.StatusForbidden},
		{"delete", func(b string) int { return deleteReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, b).StatusCode }, http.StatusForbidden},
	} {
		t.Run(tc.name+" rejects a non-admin token", func(t *testing.T) {
			if got := tc.do(userTok.Plaintext); got != tc.wantNo {
				t.Fatalf("non-admin %s: want %d, got %d", tc.name, tc.wantNo, got)
			}
		})
		t.Run(tc.name+" rejects an anonymous caller", func(t *testing.T) {
			if got := tc.do(""); got != http.StatusUnauthorized {
				t.Fatalf("anonymous %s: want 401, got %d", tc.name, got)
			}
		})
	}
}

// Validation: the JSONB payloads must be the shapes the agent expects, and a
// dangling runtime_preset_id on an app write is a 400, not an FK error at launch.
func TestRuntimePresetValidation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@valid.test", "validadmin")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"missing name", map[string]any{"image": "x"}},
		{"blank name", map[string]any{"name": "   "}},
		{"args not a string array", map[string]any{"name": "a", "args": map[string]any{"a": "b"}}},
		{"mounts not a string array", map[string]any{"name": "b", "mounts": "nope"}},
		{"env not a string map", map[string]any{"name": "c", "env": []string{"A=1"}}},
		{"relative home path", map[string]any{"name": "d", "home_container_path": "home/quasar"}},
		// Explicit JSON null must be REJECTED, not silently treated as absent or
		// as a real value. See presets.go validate()'s isExplicitJSONNull doc.
		{"args explicit null on create", map[string]any{"name": "e", "args": nil}},
		{"env explicit null on create", map[string]any{"name": "f", "env": nil}},
		{"mounts explicit null on create", map[string]any{"name": "g", "mounts": nil}},
		// The admin door must not be the laxer way into the same column
		// (internal/mountpolicy is shared with the catalog door).
		{"docker socket mount", map[string]any{"name": "h", "mounts": []string{"/var/run/docker.sock:/var/run/docker.sock"}}},
		{"docker socket parent mount", map[string]any{"name": "i", "mounts": []string{"/var/run:/hostrun"}}},
		{"docker state dir mount", map[string]any{"name": "j", "mounts": []string{"/var/lib/docker:/hostdocker"}}},
		{"pid-1 root mount", map[string]any{"name": "k", "mounts": []string{"/proc/1/root:/host"}}},
		{"host root mount", map[string]any{"name": "l", "mounts": []string{"/:/host"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", tc.body, bearer)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%v)", resp.StatusCode, body)
			}
		})
	}

	// Duplicate name → 409 (UNIQUE), not a 500.
	if resp, b := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "dup"}, bearer); resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed dup preset: %d (%v)", resp.StatusCode, b)
	}
	if resp, _ := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "dup"}, bearer); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate preset name: want 409, got %d", resp.StatusCode)
	}

	// An app pointing at a preset that does not exist is a 400 at write time.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":              "dangling",
		"runtime_preset_id": "00000000-0000-0000-0000-000000000000",
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dangling runtime_preset_id: want 400, got %d (%v)", resp.StatusCode, body)
	}

	// PATCH /v1/apps: absent runtime_preset_id must leave the column alone.
	_, body = post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "keepme"}, bearer)
	keepID := body["runtime_preset"].(map[string]any)["id"].(string)
	_, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "keeper", "runtime_preset_id": keepID}, bearer)
	appID := body["app"].(map[string]any)["id"].(string)
	_, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"description": "touched"}, bearer)
	if body["app"].(map[string]any)["runtime_preset_id"] != keepID {
		t.Fatalf("an absent runtime_preset_id on patch cleared the column: %v", body["app"])
	}

	// An app created with no runtime_preset_id has none — today's behaviour.
	_, body = post(t, srv.URL+"/v1/apps", map[string]any{"name": "no-preset"}, bearer)
	if v := body["app"].(map[string]any)["runtime_preset_id"]; v != nil {
		t.Fatalf("an app created without a preset must have runtime_preset_id null, got %v", v)
	}
}

// TestRuntimePresetNullFieldsOnUpdate is the update-path half of the explicit-
// null finding: `{"args": null}` (and env/mounts) must be rejected with 400,
// not silently written as a literal jsonb null — for a json.RawMessage field
// Go decodes `null` to the 4 bytes "null" rather than a nil/absent value, and
// isStringArray/isStringMap both treat `null -> slice/map` as a no-op success,
// so without the explicit-null guard the update guard `len(w.Args) > 0` (true
// for 4 bytes) would clobber the column. This also proves the existing
// tri-state behaviour (absent = unchanged) survives the fix, and that `[]`/`{}`
// still work as the documented way to clear a field.
func TestRuntimePresetNullFieldsOnUpdate(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@nullfields.test", "nullfieldsadmin")

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name":   "null-fields",
		"args":   []string{"-foo"},
		"env":    map[string]string{"A": "1"},
		"mounts": []string{"/data:/data"},
	}, bearer)
	if body["runtime_preset"] == nil {
		t.Fatalf("seed preset: %v", body)
	}
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	// Explicit null on PATCH for each of the three fields → 400, and the
	// column must be left untouched by the rejected request.
	for _, tc := range []struct {
		field string
		body  map[string]any
	}{
		{"args", map[string]any{"args": nil}},
		{"env", map[string]any{"env": nil}},
		{"mounts", map[string]any{"mounts": nil}},
	} {
		t.Run(tc.field+" explicit null on update is rejected", func(t *testing.T) {
			resp, body := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, tc.body, bearer)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("PATCH {%q: null}: want 400, got %d (%v)", tc.field, resp.StatusCode, body)
			}
		})
	}

	// The preset must be entirely unchanged after the three rejected PATCHes.
	_, body = getReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer)
	got := body["runtime_preset"].(map[string]any)
	if args, ok := got["args"].([]any); !ok || len(args) != 1 || args[0] != "-foo" {
		t.Fatalf("a rejected null PATCH must not touch args, got %v", got["args"])
	}
	if env, ok := got["env"].(map[string]any); !ok || env["A"] != "1" {
		t.Fatalf("a rejected null PATCH must not touch env, got %v", got["env"])
	}
	if mounts, ok := got["mounts"].([]any); !ok || len(mounts) != 1 || mounts[0] != "/data:/data" {
		t.Fatalf("a rejected null PATCH must not touch mounts, got %v", got["mounts"])
	}

	// An ABSENT field on update must still leave the stored value untouched —
	// the pre-existing tri-state behaviour must not regress. Patch an unrelated
	// field only.
	resp, body := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"description": "touched",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch unrelated field: want 200, got %d (%v)", resp.StatusCode, body)
	}
	got = body["runtime_preset"].(map[string]any)
	if args, ok := got["args"].([]any); !ok || len(args) != 1 || args[0] != "-foo" {
		t.Fatalf("an absent args on patch must leave the column alone, got %v", got["args"])
	}
	if env, ok := got["env"].(map[string]any); !ok || env["A"] != "1" {
		t.Fatalf("an absent env on patch must leave the column alone, got %v", got["env"])
	}
	if mounts, ok := got["mounts"].([]any); !ok || len(mounts) != 1 || mounts[0] != "/data:/data" {
		t.Fatalf("an absent mounts on patch must leave the column alone, got %v", got["mounts"])
	}

	// `[]`/`{}` still succeed and CLEAR the field — the documented way to empty
	// it, as opposed to the rejected null.
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"args": []string{},
		"env":  map[string]string{},
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch with [] / {}: want 200, got %d (%v)", resp.StatusCode, body)
	}
	got = body["runtime_preset"].(map[string]any)
	if args, ok := got["args"].([]any); !ok || len(args) != 0 {
		t.Fatalf("args: [] must clear the field, got %v", got["args"])
	}
	if env, ok := got["env"].(map[string]any); !ok || len(env) != 0 {
		t.Fatalf("env: {} must clear the field, got %v", got["env"])
	}
	// mounts untouched by that PATCH (absent) — still the original value.
	if mounts, ok := got["mounts"].([]any); !ok || len(mounts) != 1 || mounts[0] != "/data:/data" {
		t.Fatalf("mounts should be untouched by an unrelated PATCH, got %v", got["mounts"])
	}
}

// TestRuntimePresetNetworkField covers the §S2 `network` column end to end on the
// admin surface: it defaults to "" (inherit the host default), accepts exactly
// the three docker modes, survives a partial PATCH untouched, and 400s on
// anything else.
//
// The 400 is the point. This value becomes `docker run --network <value>` on a
// host, so an arbitrary string (e.g. `container:quasar-control`, which joins
// another container's network namespace) must never be storable. The CHECK
// constraint (migration 0061) and the agent's own parse are the other two layers.
func TestRuntimePresetNetworkField(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@netpreset.test", "netpresetadmin")

	// Absent on create ⇒ the schema default "" (inherit).
	resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name": "Inherit Net",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	inheritID := body["runtime_preset"].(map[string]any)["id"].(string)
	if got := body["runtime_preset"].(map[string]any)["network"]; got != "" {
		t.Fatalf(`network default: want "" (inherit), got %#v`, got)
	}

	// Stated on create — the Steam case (#463: its first boot downloads steamui.so).
	resp, body = post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name":    "Steam Net",
		"network": "bridge",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridged preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	steamID := body["runtime_preset"].(map[string]any)["id"].(string)
	if got := body["runtime_preset"].(map[string]any)["network"]; got != "bridge" {
		t.Fatalf("network on create: want bridge, got %#v", got)
	}

	// It round-trips through GET and LIST (this is what the admin UI reads).
	_, body = getReq(t, srv.URL+"/v1/admin/runtime-presets/"+steamID, bearer)
	if got := body["runtime_preset"].(map[string]any)["network"]; got != "bridge" {
		t.Fatalf("network on get: want bridge, got %#v", got)
	}
	_, body = getReq(t, srv.URL+"/v1/admin/runtime-presets", bearer)
	for _, it := range body["items"].([]any) {
		item := it.(map[string]any)
		if _, ok := item["network"]; !ok {
			t.Fatalf("list item is missing the network field: %v", item)
		}
	}

	// PATCH sets it, and a PATCH that does not mention it leaves it alone.
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+inheritID, map[string]any{
		"network": "none",
	}, bearer)
	if resp.StatusCode != http.StatusOK || body["runtime_preset"].(map[string]any)["network"] != "none" {
		t.Fatalf("patch network: got %d (%v)", resp.StatusCode, body)
	}
	_, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+inheritID, map[string]any{
		"description": "unrelated edit",
	}, bearer)
	if got := body["runtime_preset"].(map[string]any)["network"]; got != "none" {
		t.Fatalf("an unrelated patch clobbered network: %#v", got)
	}

	// Clearing back to inherit is an explicit "".
	_, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+inheritID, map[string]any{
		"network": "",
	}, bearer)
	if got := body["runtime_preset"].(map[string]any)["network"]; got != "" {
		t.Fatalf(`patch network to "": got %#v`, got)
	}

	// Every other value is a 400, on BOTH create and patch.
	//
	// `host` is in this list ON PURPOSE (review, Alice round 2 on PR #464): it is
	// a real docker network mode and the agent's own operator knob accepts it,
	// but it removes the container's network namespace — exposing everything on
	// host loopback and letting the app bind host ports — and a preset is
	// portable (P5 materializes one from a catalog manifest authored elsewhere).
	// An app-authored value must never be able to dissolve that boundary.
	for _, bad := range []string{"host", "container:quasar-control", "my-net", "Bridge", "none;rm -rf /"} {
		resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
			"name": "bad " + bad, "network": bad,
		}, bearer)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("create with network=%q: want 400, got %d (%v)", bad, resp.StatusCode, body)
		}
		resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+steamID,
			map[string]any{"network": bad}, bearer)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("patch with network=%q: want 400, got %d (%v)", bad, resp.StatusCode, body)
		}
	}
	// …and the rejected patch left the stored value untouched.
	_, body = getReq(t, srv.URL+"/v1/admin/runtime-presets/"+steamID, bearer)
	if got := body["runtime_preset"].(map[string]any)["network"]; got != "bridge" {
		t.Fatalf("a rejected patch changed network: %#v", got)
	}

	// The `host` rejection must EXPLAIN itself and name the supported door.
	// An operator who is refused with a bare "invalid value" reasonably concludes
	// they hit a bug and goes looking for a way around the check; one who is told
	// where host networking actually lives does the supported thing instead.
	resp, body = post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name": "host net", "network": "host",
	}, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with network=host: want 400, got %d", resp.StatusCode)
	}
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "QUASAR_CONTAINER_NETWORK") {
		t.Errorf("the host rejection must point at the operator knob, got: %q", msg)
	}
	if !strings.Contains(msg, "isolation") {
		t.Errorf("the host rejection must say why, got: %q", msg)
	}
}
